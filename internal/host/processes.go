package host

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	// UID/GID optionally run the process as a dedicated unprivileged account.
	// Both must be >= APTEVA_APP_UID_BASE (default 200000) and the host must
	// run as root on Linux. GID defaults to UID. Unset keeps the host's user.
	UID *uint32 `json:"uid,omitempty"`
	GID *uint32 `json:"gid,omitempty"`
}

// DefaultAppUIDBase is the lowest UID/GID a hosted app process may run as.
const DefaultAppUIDBase = 200000

// AppUIDBase returns APTEVA_APP_UID_BASE, falling back to the default for
// unset, malformed or system-account (< 1000) values.
func AppUIDBase() uint32 {
	if v := os.Getenv("APTEVA_APP_UID_BASE"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil && n >= 1000 {
			return uint32(n)
		}
	}
	return DefaultAppUIDBase
}

// processCredential validates an optional per-app identity.
func processCredential(uid, gid *uint32) (*syscall.Credential, error) {
	if uid == nil && gid == nil {
		return nil, nil
	}
	if uid == nil {
		return nil, errors.New("gid requires uid")
	}
	g := *uid
	if gid != nil {
		g = *gid
	}
	base := AppUIDBase()
	if *uid == 0 || g == 0 {
		return nil, errors.New("hosted processes must not run as uid or gid 0")
	}
	// MaxUint32 is the "unchanged" (-1) sentinel of setresuid/setresgid.
	if *uid < base || g < base || *uid == math.MaxUint32 || g == math.MaxUint32 {
		return nil, fmt.Errorf("uid and gid must be >= APTEVA_APP_UID_BASE (%d)", base)
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errors.New("per-app uid requires apteva-host running as root on Linux")
	}
	// Empty Groups clears supplementary groups inherited from the host.
	return &syscall.Credential{Uid: *uid, Gid: g, Groups: []uint32{}}, nil
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
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "APTEVA_HOST_") || key == "SERVER_SECRET" {
			return ProcessStatus{}, errors.New("host and server credentials must not be passed to app processes")
		}
	}
	credential, err := processCredential(request.UID, request.GID)
	if err != nil {
		return ProcessStatus{}, err
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}
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
