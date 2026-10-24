package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Home, DBPath, DataDir, ListenAddr, ServerBinary string
	StartupTimeout, DrainTimeout                    time.Duration
}
type Candidate struct {
	ExistingSchema bool   `json:"existing_schema,omitempty"`
	SchemaHash     string `json:"schema_hash,omitempty"`
	SecretHash     string `json:"secret_hash,omitempty"`
	AgentPolicy    string `json:"agent_policy,omitempty"`
	Binary         string `json:"binary"`
	Version        string `json:"version"`
}
type slot struct {
	candidate Candidate
	backend   *backend
	socket    string
	cmd       *exec.Cmd
	done      chan struct{}
	err       error
	log       *os.File
}
type Host struct {
	logSinks     map[string]*logSink
	logsMu       sync.Mutex
	logs         []*processLog
	processes    processes
	deploymentWG sync.WaitGroup
	slotsMu      sync.Mutex
	slots        []*slot
	ingress      *ingress
	cfg          Config
	mu           sync.Mutex
	state        State
	router       *Router
	active       *slot
	ctx          context.Context
	cancel       context.CancelFunc
	deploying    bool
}

func New(cfg Config) (*Host, error) {
	var err error
	cfg.Home, err = filepath.Abs(cfg.Home)
	if err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = os.Getenv("DATA_DIR")
		if cfg.DataDir == "" {
			cfg.DataDir = cfg.Home
		}
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DBPath, err = filepath.Abs(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	cfg.ServerBinary, err = filepath.EvalSymlinks(cfg.ServerBinary)
	if err != nil {
		return nil, err
	}
	cfg.ServerBinary, err = filepath.Abs(cfg.ServerBinary)
	if err != nil {
		return nil, err
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 3 * time.Minute
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 5 * time.Minute
	}
	if cfg.Home == "/" || cfg.ListenAddr == "" {
		return nil, errors.New("explicit installation home and listener required")
	}
	state := State{Protocol: Protocol, Home: cfg.Home, DBPath: cfg.DBPath, DataDir: cfg.DataDir, ListenAddr: cfg.ListenAddr, PID: os.Getpid(), Binary: cfg.ServerBinary}
	state.HostBinary, err = os.Executable()
	if err != nil {
		return nil, err
	}
	state.HostBinary, err = filepath.EvalSymlinks(state.HostBinary)
	if err != nil {
		return nil, err
	}
	if prior, e := ReadState(cfg.Home); e == nil {
		if (prior.DataDir != "" && prior.DataDir != cfg.DataDir) || prior.DBPath != cfg.DBPath || prior.ListenAddr != cfg.ListenAddr {
			return nil, errors.New("host identity changed; refusing to open a different database or port")
		}
		// A durable committed active slot takes precedence over a stale CLI symlink.
		if prior.Binary != "" {
			state.Binary = prior.Binary
			state.CleanStop = prior.CleanStop
			state.Version = prior.Version
			state.Listeners = prior.Listeners
			state.ActiveSlot = prior.ActiveSlot
			state.PendingSlot = prior.PendingSlot
			state.HTTPAddr = prior.HTTPAddr
			state.HTTPSAddr = prior.HTTPSAddr
		}
		if prior.Job != nil && !Terminal(prior.Job.Phase) {
			prior.Job.Phase = "recovering"
			prior.Job.Message = "Recovering the last committed version without restoring database files"
			state.Job = prior.Job
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	return &Host{cfg: cfg, state: state, router: NewRouter(), ingress: newIngress(), processes: processes{entries: map[string]*hostedProcess{}}}, nil
}
func Terminal(phase string) bool {
	return phase == "succeeded" || phase == "failed" || phase == "rolled_back" || phase == "recovery_required"
}
func (h *Host) Snapshot() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := json.Marshal(h.state)
	var s State
	_ = json.Unmarshal(b, &s)
	return s
}
func (h *Host) saveLocked() error { return WriteState(StatePath(h.cfg.Home), h.state) }
func (h *Host) phase(phase, msg string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.Job != nil {
		h.state.Job.Phase = phase
		h.state.Job.Message = msg
		h.state.Job.UpdatedAt = time.Now().UTC()
	}
	return h.saveLocked()
}

func (h *Host) Run(ctx context.Context) error {
	dir := filepath.Join(h.cfg.Home, "host")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	release, err := Lock(filepath.Join(dir, "host.lock"), false)
	if err != nil {
		return err
	}
	defer release()
	h.ctx, h.cancel = context.WithCancel(ctx)
	var ctl, listener net.Listener
	var control, frontend *http.Server
	defer func() {
		cleanStop := h.ctx.Err() != nil
		h.cancel()
		h.deploymentWG.Wait()
		h.slotsMu.Lock()
		slots := append([]*slot(nil), h.slots...)
		h.slotsMu.Unlock()
		for _, s := range slots {
			h.stopSlot(s, false)
		}
		h.stopProcesses()
		h.closeLogs()
		if frontend != nil {
			_ = frontend.Close()
		}
		if control != nil {
			_ = control.Close()
		}
		if listener != nil {
			_ = listener.Close()
		}
		if ctl != nil {
			_ = ctl.Close()
		}
		_ = os.Remove(SocketPath(h.cfg.Home))
		h.closeListeners()
		if cleanStop {
			h.mu.Lock()
			h.state.CleanStop = true
			h.state.ActiveSlot = nil
			h.state.PendingSlot = nil
			_ = h.saveLocked()
			h.mu.Unlock()
		}
	}()
	// Lock ownership, not a stale PID, authorizes removing a leftover socket.
	_ = os.Remove(SocketPath(h.cfg.Home))
	ctl, err = net.Listen("unix", SocketPath(h.cfg.Home))
	if err != nil {
		return err
	}
	if err = os.Chmod(SocketPath(h.cfg.Home), 0600); err != nil {
		return err
	}
	control = &http.Server{Handler: h.control(), ReadHeaderTimeout: 5 * time.Second}
	go control.Serve(ctl)
	listener, err = net.Listen("tcp", h.cfg.ListenAddr)
	if err != nil {
		return err
	}
	frontend = &http.Server{Handler: h.router, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go frontend.Serve(listener)
	savedListeners := h.Snapshot().Listeners
	if len(savedListeners) == 0 && (h.state.HTTPAddr != "" || h.state.HTTPSAddr != "") {
		savedListeners = map[string]ListenerRequest{"manual": {HTTP: h.state.HTTPAddr, HTTPS: h.state.HTTPSAddr}}
	}
	for owner, request := range savedListeners {
		request.Owner = owner
		if err = h.ensureListeners(request); err != nil {
			return err
		}
	}
	s, _, err := h.recoverSlot(h.ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.active = s
	h.state.Binary = s.candidate.Binary
	h.state.Version = s.candidate.Version
	h.state.ActiveSlot = recordSlot(s)
	h.state.CleanStop = false
	h.state.PendingSlot = nil
	err = h.saveLocked()
	h.mu.Unlock()
	if err != nil {
		h.stopSlot(s, false)
		return err
	}
	h.router.Publish(s.backend)
	if job := h.Snapshot().Job; job != nil {
		phase := "rolled_back"
		if job.Version == s.candidate.Version {
			phase = "succeeded"
		}
		_ = h.phase(phase, "Recovered the serving version without restoring or deleting live data")
	}
	select {
	case <-h.ctx.Done():
		// Normal stop/restart means stop the whole installation, as before.
		h.router.Pause()
		return nil
	case <-s.done:
		// Once a deployment replaces s, its exit is expected. Monitor the current
		// slot independently instead of treating retirement as a host failure.
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-h.ctx.Done():
				return nil
			case <-ticker.C:
				h.mu.Lock()
				current := h.active
				busy := h.deploying
				h.mu.Unlock()
				if !busy && current != nil {
					select {
					case <-current.done:
						if err := h.recoverController(); err != nil {
							return err
						}
					default:
					}
				}
			}
		}
	}
}
func (h *Host) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /validate", func(w http.ResponseWriter, r *http.Request) {
		var c Candidate
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&c) != nil {
			http.Error(w, "invalid candidate", 400)
			return
		}
		if err := h.validateCandidate(c); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.StartupTimeout)
		defer cancel()
		if err := h.preflight(ctx, &c); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"validated": true})
	})
	mux.HandleFunc("POST /listeners", h.listenersHandler)
	mux.HandleFunc("POST /process/start", h.processStartHandler)
	mux.HandleFunc("GET /process/status", h.processStatusHandler)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		state := h.Snapshot()
		if state.ActiveSlot != nil {
			state.ActiveSlot.ProxyKey = ""
		}
		if state.PendingSlot != nil {
			state.PendingSlot.ProxyKey = ""
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		var c Candidate
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&c); err != nil {
			http.Error(w, "invalid candidate", 400)
			return
		}
		if err := h.begin(c); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		state := h.Snapshot()
		if state.ActiveSlot != nil {
			state.ActiveSlot.ProxyKey = ""
		}
		if state.PendingSlot != nil {
			state.PendingSlot.ProxyKey = ""
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	return mux
}
func (h *Host) begin(c Candidate) error {
	if err := h.validateCandidate(c); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deploying {
		return errors.New("a deployment is already running")
	}
	if h.active == nil || (h.state.Job != nil && h.state.Job.Phase == "recovery_required") {
		return errors.New("host is not ready")
	}
	select {
	case <-h.active.done:
		return errors.New("active controller is not serving")
	default:
	}
	h.router.mu.Lock()
	serving := h.router.gate == nil && h.router.active == h.active.backend
	h.router.mu.Unlock()
	if !serving {
		return errors.New("host ingress is not ready")
	}
	h.state.Job = &Deployment{ID: strconv.FormatInt(time.Now().UnixNano(), 36), Phase: "preflight", Version: c.Version, Previous: h.state.Version, UpdatedAt: time.Now().UTC()}
	if err := h.saveLocked(); err != nil {
		return err
	}
	h.deploying = true
	h.deploymentWG.Add(1)
	go func() {
		defer h.deploymentWG.Done()
		defer func() { h.mu.Lock(); h.deploying = false; h.mu.Unlock() }()
		h.deploy(c)
	}()
	return nil
}
func (h *Host) validateCandidate(c Candidate) error {
	if c.AgentPolicy != "" && c.AgentPolicy != "preserve" && c.AgentPolicy != "rolling" && c.AgentPolicy != "restart" {
		return errors.New("invalid agent policy")
	}
	if c.Version == "" || c.Version == "." || c.Version == ".." || strings.ContainsAny(c.Version, "/\\\x00") {
		return errors.New("invalid version")
	}
	expected := filepath.Join(h.cfg.Home, "versions", c.Version, "apteva-server")
	actual, err := filepath.EvalSymlinks(c.Binary)
	if err != nil {
		return err
	}
	if actual != expected || c.Binary != expected {
		return errors.New("candidate must be the immutable server in this installation's version directory")
	}
	return nil
}
func (h *Host) preflight(ctx context.Context, c *Candidate) error {
	if err := h.probeProtocol(ctx, *c); err != nil {
		return err
	}
	dir := filepath.Join(h.cfg.Home, "host", "preflight", strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := preflightSpace(h.cfg.DBPath, filepath.Join(dir, "apteva.db")); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Binary, "--preflight")
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": h.cfg.Home, "DATA_DIR": h.cfg.DataDir, "DB_PATH": h.cfg.DBPath, "APTEVA_UPDATE_PREFLIGHT": filepath.Join(dir, "apteva.db"), "APTEVA_HOST_SOCKET": "", "APTEVA_HOST_SLOT_SOCKET": ""})
	logf, err := os.OpenFile(filepath.Join(dir, "preflight.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("candidate preflight failed; see %s: %w", filepath.Join(dir, "preflight.log"), err)
	}
	var report struct {
		Version      string `json:"version"`
		SchemaHash   string `json:"schema_hash"`
		RollbackSafe bool   `json:"rollback_safe"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "apteva.db.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &report); err != nil {
		return err
	}
	if report.Version != c.Version || !report.RollbackSafe || report.SchemaHash == "" {
		return errors.New("release changes stored schema or data; a restart migration is required, and the current server remains active")
	}
	c.SchemaHash = report.SchemaHash
	c.ExistingSchema = true
	return nil
}
func (h *Host) deploy(c Candidate) {
	ctx, cancel := context.WithTimeout(h.ctx, 2*h.cfg.StartupTimeout+h.cfg.DrainTimeout)
	defer cancel()
	if err := h.preflight(ctx, &c); err != nil {
		_ = h.phase("failed", err.Error())
		return
	}
	h.mu.Lock()
	source := h.active
	h.mu.Unlock()
	var identity DatabaseIdentity
	identityClient := UnixClient(source.socket)
	err := Call(ctx, identityClient, "GET", "/identity", nil, &identity)
	identityClient.CloseIdleConnections()
	if err != nil || identity.SchemaHash != c.SchemaHash || identity.SecretHash == "" {
		_ = h.phase("failed", "Cannot confirm the active database and encryption key identity")
		return
	}
	c.SecretHash = identity.SecretHash
	// Recovery of this controller must also skip migrations and adopt live work.
	source.candidate.ExistingSchema = true
	source.candidate.SchemaHash = identity.SchemaHash
	source.candidate.SecretHash = identity.SecretHash
	_ = h.phase("warming", "Starting the candidate without opening the live database or starting runtimes")
	candidate, err := h.startSlot(ctx, c)
	if err != nil {
		_ = h.phase("failed", err.Error())
		return
	}
	if err = h.prepare(ctx, candidate); err != nil {
		h.stopSlot(candidate, false)
		_ = h.phase("failed", "Candidate preparation failed: "+err.Error())
		return
	}
	h.mu.Lock()
	h.state.PendingSlot = recordSlot(candidate)
	h.state.ActiveSlot = recordSlot(source)
	persistErr := h.saveLocked()
	h.mu.Unlock()
	if persistErr != nil {
		h.stopSlot(candidate, false)
		_ = h.phase("failed", persistErr.Error())
		return
	}
	h.mu.Lock()
	old := h.active
	h.mu.Unlock()
	h.router.Pause()
	_ = h.phase("draining", "Waiting for active requests and calls; new requests are queued without replay")
	drainCtx, stopDrain := context.WithTimeout(ctx, h.cfg.DrainTimeout)
	err = h.router.Drain(drainCtx, old.backend)
	stopDrain()
	if err != nil {
		h.stopSlot(candidate, false)
		h.router.Resume()
		_ = h.phase("failed", "Existing work did not finish; the active version was retained")
		return
	}
	// Verify again after draining: data written during candidate warming must
	// never be overwritten by a preflight backup or silently transformed.
	currentIdentity, identityErr := h.diskIdentity(ctx, c)
	if identityErr != nil || currentIdentity.SchemaHash != c.SchemaHash || currentIdentity.SecretHash != c.SecretHash {
		err = fmt.Errorf("live database or encryption key identity changed after preparation: %v", identityErr)
		h.stopSlot(candidate, false)
		h.router.Resume()
		_ = h.phase("failed", err.Error())
		return
	}
	if err = h.phase("transferring", "Transferring exclusive ownership; running agent and app processes are preserved"); err != nil {
		h.stopSlot(candidate, false)
		h.router.Resume()
		return
	}
	policy := c.AgentPolicy
	if policy == "" {
		var v struct {
			Policy string `json:"agent_policy"`
		}
		client := UnixClient(old.socket)
		_ = Call(ctx, client, "GET", "/policy", nil, &v)
		client.CloseIdleConnections()
		policy = v.Policy
		if policy == "" {
			policy = "preserve"
		}
	}
	expected := map[string]int{}
	oldClient := UnixClient(old.socket)
	err = Call(ctx, oldClient, "POST", "/freeze", nil, &expected)
	oldClient.CloseIdleConnections()
	if err != nil {
		h.stopSlot(candidate, false)
		h.router.Resume()
		_ = h.phase("failed", "Cannot verify runtime ownership before transfer: "+err.Error())
		return
	}
	defer func() {
		unfreezeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		client := UnixClient(old.socket)
		defer client.CloseIdleConnections()
		_ = Call(unfreezeCtx, client, "POST", "/unfreeze", nil, nil)
	}()
	transferCtx, finishTransfer := context.WithTimeout(context.Background(), 2*h.cfg.StartupTimeout)
	defer finishTransfer()
	if err = h.retire(transferCtx, old); err != nil {
		h.stopSlot(candidate, false)
		_ = h.phase("recovery_required", "Ownership release could not be confirmed; candidate was not activated: "+err.Error())
		return
	}
	h.mu.Lock()
	h.active = candidate
	h.mu.Unlock()
	h.router.SetStarting(candidate.backend)
	if err = h.activate(transferCtx, candidate); err == nil {
		actual := map[string]int{}
		client := UnixClient(candidate.socket)
		err = Call(ctx, client, "GET", "/runtimes", nil, &actual)
		client.CloseIdleConnections()
		if err == nil {
			for key, pid := range expected {
				if actual[key] != pid {
					err = fmt.Errorf("runtime %s was not adopted with its original PID", key)
					break
				}
			}
		}
	}
	if err != nil {
		h.stopSlot(candidate, true)
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), h.cfg.StartupTimeout)
		defer recoveryCancel()
		_ = h.phase("rolling_back", "Starting the previous binary against the same current database")
		previous, recoveryErr := h.startSlot(recoveryCtx, old.candidate)
		if recoveryErr == nil {
			h.mu.Lock()
			h.active = previous
			h.mu.Unlock()
			h.router.SetStarting(previous.backend)
			recoveryErr = h.prepare(recoveryCtx, previous)
			if recoveryErr == nil {
				h.mu.Lock()
				h.state.PendingSlot = recordSlot(previous)
				recoveryErr = h.saveLocked()
				h.mu.Unlock()
			}
			if recoveryErr == nil {
				recoveryErr = h.activate(recoveryCtx, previous)
			}
		}
		if recoveryErr != nil {
			h.stopSlot(previous, true)
			_ = h.phase("recovery_required", fmt.Sprintf("candidate failed: %v; recovery failed: %v; database and runtime data were not restored or deleted", err, recoveryErr))
			return
		}
		h.mu.Lock()
		h.active = previous
		h.state.ActiveSlot = recordSlot(previous)
		h.state.PendingSlot = nil
		h.mu.Unlock()
		h.router.Publish(previous.backend)
		_ = h.phase("rolled_back", fmt.Sprintf("Previous version restored; all live data retained: %v", err))
		return
	}
	h.mu.Lock()
	h.active = candidate
	h.state.Binary = c.Binary
	h.state.Version = c.Version
	h.state.ActiveSlot = recordSlot(candidate)
	h.state.PendingSlot = nil
	err = h.saveLocked()
	h.mu.Unlock()
	if err != nil {
		_ = h.phase("recovery_required", "Candidate is serving but the committed state could not be saved: "+err.Error())
	}
	h.router.Publish(candidate.backend)
	if err == nil {
		message := "New server is serving; application and database data were retained"
		if policy != "preserve" {
			client := UnixClient(candidate.socket)
			rolloutErr := Call(transferCtx, client, "POST", "/rollout", map[string]string{"agent_policy": policy}, nil)
			client.CloseIdleConnections()
			if rolloutErr != nil {
				message += ". Core rollout needs attention: " + rolloutErr.Error()
			} else {
				message += ". Core rollout requested: " + policy
			}
		}
		_ = h.phase("succeeded", message)
	}
}
func overrides(base []string, values map[string]string) []string {
	out := make([]string, 0, len(base)+len(values))
	for _, v := range base {
		k, _, _ := strings.Cut(v, "=")
		if _, ok := values[k]; !ok {
			out = append(out, v)
		}
	}
	for k, v := range values {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
func (h *Host) startSlot(ctx context.Context, c Candidate) (*slot, error) {
	if err := h.probeProtocol(ctx, c); err != nil {
		return nil, err
	}

	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := reserve.Addr().String()
	reserve.Close()
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	socket := filepath.Join(h.cfg.Home, "host", "slot-"+id+".sock")
	b, err := newBackend("http://" + addr)
	if err != nil {
		return nil, err
	}
	pipe, err := h.logPipe(filepath.Join(h.cfg.Home, "host", "server-"+id+".log"), 16<<20)
	if err != nil {
		return nil, err
	}
	logfile := pipe.writer
	s := &slot{candidate: c, backend: b, socket: socket, done: make(chan struct{}), log: logfile}
	cmd := exec.Command(c.Binary)
	s.cmd = cmd
	_, publicPort, _ := net.SplitHostPort(h.cfg.ListenAddr)
	core := os.Getenv("CORE_CMD")
	if core == "" || core == filepath.Join(h.cfg.Home, "bin", "apteva-core") {
		core = filepath.Join(filepath.Dir(c.Binary), "apteva-core")
	}
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": h.cfg.Home, "DATA_DIR": h.cfg.DataDir, "DB_PATH": h.cfg.DBPath, "PORT": publicPort, "APTEVA_HOST_SLOT_SOCKET": socket, "APTEVA_HOST_SOCKET": SocketPath(h.cfg.Home), "APTEVA_HOST_BACKEND_ADDR": addr, "APTEVA_HOST_PROXY_SECRET": b.secret, "APTEVA_HOST_EXISTING_SCHEMA": map[bool]string{true: "1", false: ""}[c.ExistingSchema], "APTEVA_HOST_SCHEMA_HASH": c.SchemaHash, "CORE_CMD": core})
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	if err = cmd.Start(); err != nil {
		logfile.Close()
		return nil, err
	}
	h.slotsMu.Lock()
	h.slots = append(h.slots, s)
	h.slotsMu.Unlock()
	go func() { s.err = cmd.Wait(); logfile.Close(); _ = os.Remove(socket); close(s.done) }()
	waitCtx, cancel := context.WithTimeout(ctx, h.cfg.StartupTimeout)
	defer cancel()
	client := UnixClient(socket)
	defer client.CloseIdleConnections()
	var status struct {
		Protocol int    `json:"protocol"`
		Version  string `json:"version"`
	}
	for {
		if err = Call(waitCtx, client, "GET", "/status", nil, &status); err == nil {
			if status.Protocol != Protocol || (c.Version != "" && status.Version != c.Version) {
				h.stopSlot(s, false)
				return nil, errors.New("candidate protocol or release version does not match")
			}
			s.candidate.Version = status.Version
			return s, nil
		}
		select {
		case <-s.done:
			return nil, fmt.Errorf("candidate exited before standby: %v", s.err)
		case <-waitCtx.Done():
			h.stopSlot(s, false)
			return nil, fmt.Errorf("candidate did not reach standby: %w", waitCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (h *Host) activate(ctx context.Context, s *slot) error {
	client := UnixClient(s.socket)
	defer client.CloseIdleConnections()
	if err := Call(ctx, client, "POST", "/activate", nil, nil); err != nil {
		return err
	}
	return h.waitReady(ctx, s)
}
func (h *Host) waitReady(ctx context.Context, s *slot) error {
	waitCtx, cancel := context.WithTimeout(ctx, h.cfg.StartupTimeout)
	defer cancel()
	healthClient := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(waitCtx, "GET", s.backend.url.String()+"/health", nil)
		req.Header.Set("X-Apteva-Host-Token", s.backend.secret)
		resp, err := healthClient.Do(req)
		if err == nil {
			var status struct {
				OK      bool   `json:"ok"`
				Version string `json:"cli"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&status)
			resp.Body.Close()
			if decodeErr == nil && resp.StatusCode == 200 && status.OK && status.Version == s.candidate.Version {
				return nil
			}
		}
		select {
		case <-s.done:
			return fmt.Errorf("candidate exited during activation: %v", s.err)
		case <-waitCtx.Done():
			return fmt.Errorf("candidate readiness failed: %w", waitCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (h *Host) retire(ctx context.Context, s *slot) error {
	client := UnixClient(s.socket)
	defer client.CloseIdleConnections()
	if err := Call(ctx, client, "POST", "/retire", nil, nil); err != nil {
		return fmt.Errorf("could not prepare runtime preservation: %w", err)
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return errors.New("previous controller did not release ownership; candidate was not activated")
	}
}
func (h *Host) stopSlot(s *slot, preserve bool) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	if preserve {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client := UnixClient(s.socket)
		_ = Call(ctx, client, "POST", "/retire", nil, nil)
		client.CloseIdleConnections()
		cancel()
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
		return
	case <-time.After(30 * time.Second):
		log.Printf("[HOST] server did not exit; terminating server PID only")
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// Never launch an older, unaware server against live data in "standby" mode.
// Capability probing itself gets disposable paths and no public listeners.
func (h *Host) probeProtocol(ctx context.Context, c Candidate) error {
	dir, err := os.MkdirTemp(filepath.Join(h.cfg.Home, "host"), "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, c.Binary, "--host-protocol")
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": dir, "DATA_DIR": dir, "DB_PATH": filepath.Join(dir, "probe.db"), "PORT": "0", "APTEVA_BIND": "127.0.0.1", "APTEVA_CLONE_QUARANTINE": "1", "APTEVA_HOST_SLOT_SOCKET": "", "APTEVA_HOST_SOCKET": "", "APTEVA_INGRESS_ENABLED": "", "APTEVA_HTTP_LISTEN_ADDR": "", "APTEVA_HTTPS_LISTEN_ADDR": "", "APTEVA_TLS_LISTEN_ADDR": ""})
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("server lacks the hosted standby protocol; an explicit restart migration is required: %w", err)
	}
	var response struct {
		ExistingSchema bool   `json:"existing_schema"`
		Protocol       int    `json:"protocol"`
		Version        string `json:"version"`
	}
	if json.Unmarshal(output, &response) != nil || response.Protocol != Protocol || !response.ExistingSchema || (c.Version != "" && response.Version != c.Version) {
		return errors.New("candidate does not support this host protocol or release identity")
	}
	return nil
}
