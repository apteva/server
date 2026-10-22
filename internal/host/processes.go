package host

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProcessRequest struct {
	ID        string   `json:"id"`
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	Directory string   `json:"directory"`
	LogPath   string   `json:"log_path"`
}
type ProcessStatus struct {
	PID     int  `json:"pid"`
	Running bool `json:"running"`
}
type hostedProcess struct {
	cmd         *exec.Cmd
	done        chan struct{}
	fingerprint [32]byte
}
type processes struct {
	mu      sync.Mutex
	entries map[string]*hostedProcess
}

func (h *Host) startProcess(request ProcessRequest) (ProcessStatus, error) {
	if request.ID == "" || len(request.ID) > 128 || len(request.Args) == 0 || !filepath.IsAbs(request.Args[0]) || !filepath.IsAbs(request.Directory) || !filepath.IsAbs(request.LogPath) {
		return ProcessStatus{}, errors.New("invalid hosted process identity or paths")
	}
	for _, item := range request.Env {
		if strings.HasPrefix(item, "APTEVA_HOST_") {
			return ProcessStatus{}, errors.New("host credentials must not be passed to app processes")
		}
	}
	b, _ := json.Marshal(request)
	fingerprint := sha256.Sum256(b)
	h.processes.mu.Lock()
	defer h.processes.mu.Unlock()
	if existing := h.processes.entries[request.ID]; existing != nil {
		if existing.fingerprint != fingerprint {
			return ProcessStatus{}, errors.New("process request identity reused with different input")
		}
		running := true
		select {
		case <-existing.done:
			running = false
		default:
		}
		return ProcessStatus{PID: existing.cmd.Process.Pid, Running: running}, nil
	}
	pipe, err := h.logPipe(request.LogPath, 16<<20)
	if err != nil {
		return ProcessStatus{}, err
	}
	cmd := exec.Command(request.Args[0], request.Args[1:]...)
	cmd.Dir = request.Directory
	cmd.Env = request.Env
	cmd.Stdout = pipe.writer
	cmd.Stderr = pipe.writer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		pipe.writer.Close()
		return ProcessStatus{}, err
	}
	pipe.writer.Close()
	cmd.Env = nil
	p := &hostedProcess{cmd: cmd, done: make(chan struct{}), fingerprint: fingerprint}
	h.processes.entries[request.ID] = p
	go func() { _ = cmd.Wait(); close(p.done) }()
	return ProcessStatus{PID: cmd.Process.Pid, Running: true}, nil
}
func (h *Host) processStartHandler(w http.ResponseWriter, r *http.Request) {
	var request ProcessRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid process request", 400)
		return
	}
	state, err := h.startProcess(request)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}
func (h *Host) stopProcesses() {
	h.processes.mu.Lock()
	procs := make([]*hostedProcess, 0, len(h.processes.entries))
	for _, p := range h.processes.entries {
		procs = append(procs, p)
	}
	h.processes.mu.Unlock()
	for _, p := range procs {
		select {
		case <-p.done:
			continue
		default:
		}
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, p := range procs {
		select {
		case <-p.done:
		case <-deadline.C:
			for _, p := range procs {
				select {
				case <-p.done:
				default:
					_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
				}
			}
			return
		}
	}
}
func (h *Host) processStatusHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	h.processes.mu.Lock()
	p := h.processes.entries[id]
	h.processes.mu.Unlock()
	if p == nil {
		http.Error(w, "process is not tracked", 404)
		return
	}
	running := true
	select {
	case <-p.done:
		running = false
	default:
	}
	_ = json.NewEncoder(w).Encode(ProcessStatus{PID: p.cmd.Process.Pid, Running: running})
}
