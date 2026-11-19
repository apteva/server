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

	"github.com/apteva/server/internal/sshwatch"
)

type Config struct {
	Home, DBPath, DataDir, ListenAddr, ServerBinary string
	StartupTimeout, DrainTimeout                    time.Duration
	// CheckTimeout bounds a candidate's database update check; the check is
	// also stopped when it stops reporting progress.
	CheckTimeout time.Duration
	// Security configures the in-process SSH collector; nil derives it from
	// the APTEVA_SECURITY_* environment.
	Security *sshwatch.Config
	// SecurityCollector is "on" (default), "reader" (never collect, serve the
	// shared store) or "off"; empty reads APTEVA_SECURITY_COLLECTOR.
	SecurityCollector string
}
type Candidate struct {
	ExistingSchema bool   `json:"existing_schema,omitempty"`
	SchemaHash     string `json:"schema_hash,omitempty"`
	SecretHash     string `json:"secret_hash,omitempty"`
	AgentPolicy    string `json:"agent_policy,omitempty"`
	Binary         string `json:"binary"`
	Version        string `json:"version"`
	// AllowRestart lets a release that changes stored schema deploy by
	// stopping the current server and booting this one, which migrates.
	AllowRestart bool `json:"allow_restart,omitempty"`
	// From the candidate's database check: the previous server can run on
	// the migrated database, and the backup written for this deployment.
	compatible bool
	backup     string
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
	controlToken string
	// nextServer is the NextServerPath request this start follows; it is
	// removed once that server serves.
	nextServer *Candidate
	security   *sshwatch.Collector
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
	if cfg.CheckTimeout == 0 {
		cfg.CheckTimeout = 30 * time.Minute
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
		// The port is the identity; the bind address may change (e.g. the
		// management listener moving from 0.0.0.0 to loopback).
		_, priorPort, _ := net.SplitHostPort(prior.ListenAddr)
		_, port, _ := net.SplitHostPort(cfg.ListenAddr)
		if (prior.DataDir != "" && prior.DataDir != cfg.DataDir) || prior.DBPath != cfg.DBPath || priorPort != port {
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
	h := &Host{cfg: cfg, state: state, router: NewRouter(), ingress: newIngress(), processes: processes{entries: map[string]*hostedProcess{}}}
	h.applyNextServer()
	return h, nil
}

// applyNextServer boots a requested release fresh, as a restart migration
// started by the CLI through an older host. Only a cleanly stopped host with
// no slot to adopt follows it; the request names a server in this
// installation's version directory.
func (h *Host) applyNextServer() {
	path := NextServerPath(h.cfg.Home)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var next Candidate
	if err = json.Unmarshal(raw, &next); err == nil {
		err = h.validateCandidate(next)
	}
	if err == nil && (!h.state.CleanStop || h.state.ActiveSlot != nil || h.state.PendingSlot != nil || (h.state.Job != nil && !Terminal(h.state.Job.Phase))) {
		err = errors.New("the previous host did not stop cleanly")
	}
	if err != nil {
		// A stale request must not take effect at some later start.
		log.Printf("[HOST] discarding %s: %v", path, err)
		_ = os.Remove(path)
		return
	}
	h.nextServer = &next
	if next.Version == h.state.Version && next.Binary == h.state.Binary {
		return
	}
	h.state.Job = &Deployment{ID: strconv.FormatInt(time.Now().UnixNano(), 36), Phase: "recovering", Message: "Starting the requested version; it migrates the database as it boots", Version: next.Version, Previous: h.state.Version, UpdatedAt: time.Now().UTC()}
	h.state.Binary, h.state.Version = next.Binary, next.Version
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
	if h.controlToken, err = h.prepareControlToken(); err != nil {
		return fmt.Errorf("host control token: %w", err)
	}
	h.ctx, h.cancel = context.WithCancel(ctx)
	securityDone := h.startSecurity(h.ctx)
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
		if securityDone != nil {
			<-securityDone
		}
		if cleanStop {
			// No slot outlives a clean stop, so the next start mints a new token.
			_ = os.Remove(ControlTokenPath(h.cfg.Home))
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
	control = &http.Server{Handler: h.authorizeControl(h.control()), ConnContext: controlConnContext, ReadHeaderTimeout: 5 * time.Second}
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
	message := "Recovered the serving version without restoring or deleting live data"
	if next := h.nextServer; next != nil && s.candidate.Binary == next.Binary {
		_ = os.Remove(NextServerPath(h.cfg.Home))
		message = "New server is serving after a restart migration; the database was migrated in place"
	}
	if job := h.Snapshot().Job; job != nil {
		phase := "rolled_back"
		if job.Version == s.candidate.Version {
			phase = "succeeded"
		}
		_ = h.phase(phase, message)
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
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.CheckTimeout)
		defer cancel()
		check, err := h.preflight(ctx, &c, false, nil)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"validated": true, "restart_required": check.restart, "check": check.summary})
	})
	mux.HandleFunc("POST /listeners", h.listenersHandler)
	mux.HandleFunc("POST /process/start", h.processStartHandler)
	mux.HandleFunc("GET /process/status", h.processStatusHandler)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		state := h.Snapshot()
		state.Capabilities = HostCapabilities
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
		state.Capabilities = HostCapabilities
		if state.ActiveSlot != nil {
			state.ActiveSlot.ProxyKey = ""
		}
		if state.PendingSlot != nil {
			state.PendingSlot.ProxyKey = ""
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	h.securityRoutes(mux)
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

// restartMigrationRequired is matched by older CLIs; keep the wording.
const restartMigrationRequired = "release changes stored schema or data; a restart migration is required, and the current server remains active"

func (h *Host) deploy(c Candidate) {
	checkCtx, cancelCheck := context.WithTimeout(h.ctx, h.cfg.CheckTimeout)
	check, err := h.preflight(checkCtx, &c, true, func(message string) { _ = h.phase("preflight", message) })
	cancelCheck()
	if err != nil {
		_ = h.phase("failed", err.Error())
		return
	}
	h.recordCheck(check.summary)
	if check.restart {
		h.restartMigration(c)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 2*h.cfg.StartupTimeout+h.cfg.DrainTimeout)
	defer cancel()
	h.mu.Lock()
	source := h.active
	h.mu.Unlock()
	var identity DatabaseIdentity
	identityClient := UnixClient(source.socket)
	err = Call(ctx, identityClient, "GET", "/identity", nil, &identity)
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
	policy := h.agentPolicy(ctx, c, old)
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
		pruneSucceededBackups(c)
		_ = h.phase("succeeded", h.rollout(transferCtx, candidate, policy, "New server is serving; application and database data were retained"))
	}
}

// agentPolicy is the candidate's requested agent handling, else the running
// server's configured policy, else preserve.
func (h *Host) agentPolicy(ctx context.Context, c Candidate, current *slot) string {
	if c.AgentPolicy != "" {
		return c.AgentPolicy
	}
	var v struct {
		Policy string `json:"agent_policy"`
	}
	client := UnixClient(current.socket)
	_ = Call(ctx, client, "GET", "/policy", nil, &v)
	client.CloseIdleConnections()
	if v.Policy == "" {
		return "preserve"
	}
	return v.Policy
}

// rollout asks the new server to update agent cores per policy and appends
// the outcome to message.
func (h *Host) rollout(ctx context.Context, s *slot, policy, message string) string {
	if policy == "preserve" {
		return message
	}
	client := UnixClient(s.socket)
	defer client.CloseIdleConnections()
	if err := Call(ctx, client, "POST", "/rollout", map[string]string{"agent_policy": policy}, nil); err != nil {
		return message + ". Core rollout needs attention: " + err.Error()
	}
	return message + ". Core rollout requested: " + policy
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
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": h.cfg.Home, "DATA_DIR": h.cfg.DataDir, "DB_PATH": h.cfg.DBPath, "PORT": publicPort, "APTEVA_HOST_SLOT_SOCKET": socket, "APTEVA_HOST_SOCKET": SocketPath(h.cfg.Home), "APTEVA_HOST_BACKEND_ADDR": addr, "APTEVA_HOST_PROXY_SECRET": b.secret, "APTEVA_HOST_EXISTING_SCHEMA": map[bool]string{true: "1", false: ""}[c.ExistingSchema], "APTEVA_HOST_SCHEMA_HASH": c.SchemaHash, ControlTokenEnv: h.controlToken, "CORE_CMD": core})
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
	return h.withStartupCallbacks(s, func() error {
		client := UnixClient(s.socket)
		defer client.CloseIdleConnections()
		if err := Call(ctx, client, "POST", "/activate", nil, nil); err != nil {
			return err
		}
		return h.waitReady(ctx, s)
	})
}

// Register before activation can mount apps, or before waiting for an adopted
// controller already mounting them. Only the router's existing loopback callback
// exception is admitted; ordinary traffic stays gated until Publish. Keep this
// registration through successful readiness and remove it on failure.
func (h *Host) withStartupCallbacks(s *slot, ready func() error) error {
	h.router.SetStarting(s.backend)
	if err := ready(); err != nil {
		h.router.clearStarting(s.backend)
		return err
	}
	return nil
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
	if s != nil {
		// Covers failures after readiness too, e.g. ownership verification or
		// durable commit. Never leave callbacks routed to a stopped candidate.
		h.router.clearStarting(s.backend)
	}
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
	_, err := h.probeCandidate(ctx, c)
	return err
}

// probeCandidate checks the candidate's host protocol and returns the
// capabilities it reports.
func (h *Host) probeCandidate(ctx context.Context, c Candidate) ([]string, error) {
	dir, err := os.MkdirTemp(filepath.Join(h.cfg.Home, "host"), "probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, c.Binary, "--host-protocol")
	cmd.Env = overrides(os.Environ(), map[string]string{"APTEVA_HOME": dir, "DATA_DIR": dir, "DB_PATH": filepath.Join(dir, "probe.db"), "PORT": "0", "APTEVA_BIND": "127.0.0.1", "APTEVA_CLONE_QUARANTINE": "1", "APTEVA_HOST_SLOT_SOCKET": "", "APTEVA_HOST_SOCKET": "", ControlTokenEnv: "", "APTEVA_INGRESS_ENABLED": "", "APTEVA_HTTP_LISTEN_ADDR": "", "APTEVA_HTTPS_LISTEN_ADDR": "", "APTEVA_TLS_LISTEN_ADDR": ""})
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("server lacks the hosted standby protocol; an explicit restart migration is required: %w", err)
	}
	var response struct {
		ExistingSchema bool     `json:"existing_schema"`
		Protocol       int      `json:"protocol"`
		Version        string   `json:"version"`
		Capabilities   []string `json:"capabilities"`
	}
	if json.Unmarshal(output, &response) != nil || response.Protocol != Protocol || !response.ExistingSchema || (c.Version != "" && response.Version != c.Version) {
		return nil, errors.New("candidate does not support this host protocol or release identity")
	}
	if !sendsControlToken(response.Version, response.Capabilities) {
		return nil, fmt.Errorf("server %s predates the host control token, so every app it starts would be refused; deploy a server from 0.100.14 or later", response.Version)
	}
	return response.Capabilities, nil
}

// firstControlTokenRelease is the first release whose server sends the control
// token. It predates the capability list, so it is recognised by version.
var firstControlTokenRelease = [3]int{0, 100, 14}

func sendsControlToken(version string, capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == CapabilityControlToken {
			return true
		}
	}
	parsed, ok := releaseVersion(version)
	if !ok {
		return false
	}
	for i := range parsed {
		if parsed[i] != firstControlTokenRelease[i] {
			return parsed[i] > firstControlTokenRelease[i]
		}
	}
	return true
}

func releaseVersion(version string) ([3]int, bool) {
	var parsed [3]int
	core, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsed, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return parsed, false
		}
		parsed[i] = n
	}
	return parsed, true
}
