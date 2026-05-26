package main

// Local-spawn supervisor for the Apteva Apps system.
//
// When apteva-server runs in single-host mode (no orchestrator
// configured, or APTEVA_APP_LOCAL=1), this file owns the lifecycle of
// installed apps as native subprocesses:
//
//	install → download manifest.runtime.binaries[<os>-<arch>] → cache →
//	          pick free port → spawn → poll /health → mark running →
//	          register sidecar URL in InstalledAppsRegistry
//
//	uninstall → SIGTERM the pid → wait → reap
//
//	restart server → re-spawn every install whose status='running' but
//	                 whose pid is dead (process didn't survive the restart)
//
// No Docker. The model matches WordPress's "drop a plugin file in,
// activate it" feel except the unit is a Go binary, not a PHP file,
// and lives in its own process.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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

	sdk "github.com/apteva/app-sdk"
)

// terminateProc kills a captured proc handle without touching any
// supervisor map. Used by both Stop() (after deleting from procs) and
// RetireOld() (after deleting from pending). Group-signal — see Stop()
// for the rationale on -pgid vs single-pid.
func terminateProc(p *localProc, grace time.Duration) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	signalLocalProc(pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		signalLocalProc(pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			log.Printf("[APPS-LOCAL] pid=%d did not report exit after SIGKILL", pid)
		}
	}
}

func signalLocalProc(pid int, sig syscall.Signal) {
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid > 0 {
		if err := syscall.Kill(-pgid, sig); err == nil {
			return
		}
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(sig)
	}
}

// LocalSupervisor tracks running app subprocesses keyed by install id.
// One instance per Server, created in startup before LoadInstalledApps.
//
// Three concurrency primitives govern the install pipeline:
//
//   - buildMu — per-(app,version) lock. Held across clone + build +
//     spawn so two installs of the SAME app+version (e.g. one project
//     install + one global install of the same app) serialize cleanly
//     instead of racing on shared cache-dir paths (src/, bin, the
//     two gocache/gomodcache dirs).
//   - installSem — global concurrency cap. Buffered channel sized by
//     APTEVA_INSTALL_CONCURRENCY (default 2 on a typical laptop).
//     Acquired AFTER the per-app lock so the FIFO queue is fair
//     across apps. Different apps still serialize through this layer
//     — that's the point: each `go build` peaks ~1.0–1.5 GB RSS, so
//     uncapped fan-out OOMs the host.
//   - inflight — per-install_id "already running" guard. Stops a
//     second goroutine for the same install row from racing with the
//     first (retry-on-error, dep recursion, accidental double-call).
type LocalSupervisor struct {
	cacheDir string

	mu    sync.Mutex
	procs map[int64]*localProc
	// stopping is set by StopAll during process shutdown. It prevents a
	// concurrent install/update goroutine from spawning a fresh sidecar
	// after StopAll has already drained the maps.
	stopping bool

	buildMuG sync.Mutex
	buildMu  map[string]*sync.Mutex

	inflightG sync.Mutex
	inflight  map[int64]struct{}

	installSem chan struct{}

	// pending — old proc handles captured during a blue-green spawn,
	// awaiting RetireOld() from the caller after it has updated the
	// DB + refreshed the in-memory registry. Old keeps serving until
	// retire fires; this is what gives us zero-downtime upgrades.
	pendingMu sync.Mutex
	pending   map[int64]*localProc
}

type localProc struct {
	cmd       *exec.Cmd
	port      int
	logfile   io.Closer
	done      chan struct{}
	stoppedAt time.Time
}

const appSidecarLogMaxBytes int64 = 16 << 20

// NewLocalSupervisor returns a supervisor whose binary cache lives at
// cacheDir (typically ~/.apteva/apps). Concurrency cap reads from
// APTEVA_INSTALL_CONCURRENCY (default 2).
func NewLocalSupervisor(cacheDir string) *LocalSupervisor {
	_ = os.MkdirAll(cacheDir, 0755)
	n := envInstallConcurrency()
	return &LocalSupervisor{
		cacheDir:   cacheDir,
		procs:      map[int64]*localProc{},
		buildMu:    map[string]*sync.Mutex{},
		inflight:   map[int64]struct{}{},
		installSem: make(chan struct{}, n),
		pending:    map[int64]*localProc{},
	}
}

// envInstallConcurrency returns APTEVA_INSTALL_CONCURRENCY parsed as
// an int, clamped to [1, 16]. Default 2 — a Mac laptop comfortably
// handles two concurrent `go build`s; Pi/low-RAM users can drop it
// to 1 if a single build is already pegging memory.
func envInstallConcurrency() int {
	const dflt = 2
	v := os.Getenv("APTEVA_INSTALL_CONCURRENCY")
	if v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Printf("[APPS-LOCAL] APTEVA_INSTALL_CONCURRENCY=%q invalid; using default %d", v, dflt)
		return dflt
	}
	if n > 16 {
		n = 16
	}
	return n
}

// lockApp returns a release function for the per-(app,version) mutex.
// Held across the install pipeline (clone → build → spawn). Two
// installs of the same app+version serialize; different apps don't
// share keys so they don't block each other here.
func (sup *LocalSupervisor) lockApp(name, version string) func() {
	key := name + "@" + version
	sup.buildMuG.Lock()
	m, ok := sup.buildMu[key]
	if !ok {
		m = &sync.Mutex{}
		sup.buildMu[key] = m
	}
	sup.buildMuG.Unlock()
	m.Lock()
	return m.Unlock
}

// acquireInstall reserves the install_id slot. Returns false if the
// id is already in flight — caller should bail without touching the
// row, since the in-flight goroutine will finish and update the
// install state.
func (sup *LocalSupervisor) acquireInstall(id int64) bool {
	sup.inflightG.Lock()
	defer sup.inflightG.Unlock()
	if _, busy := sup.inflight[id]; busy {
		return false
	}
	sup.inflight[id] = struct{}{}
	return true
}

func (sup *LocalSupervisor) releaseInstall(id int64) {
	sup.inflightG.Lock()
	delete(sup.inflight, id)
	sup.inflightG.Unlock()
}

// acquireBuildSlot blocks until a global build slot is available,
// returning a release function. Used to cap concurrent go-build
// processes (memory governor).
func (sup *LocalSupervisor) acquireBuildSlot() func() {
	sup.installSem <- struct{}{}
	return func() { <-sup.installSem }
}

// localPlatform returns the manifest binaries[] key for this host:
// "<goos>-<goarch>" e.g. "linux-amd64", "darwin-arm64". Matches the
// schema apps publish in their manifest.
func localPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Install resolves the binary URL from the manifest, downloads + caches
// it, picks a free port, spawns the child with the platform's env, and
// polls /health until the sidecar is ready or a 30s deadline elapses.
//
// Returns the spawned port + bin path so the caller can persist them
// in app_installs. On any failure, leaves no orphan child.
func (sup *LocalSupervisor) Install(installID int64, m *sdk.Manifest, env map[string]string) (port int, binPath string, err error) {
	bin, ok := m.Runtime.Binaries[localPlatform()]
	if !ok {
		return 0, "", fmt.Errorf("manifest has no binary for %s — author needs to publish a release for this platform", localPlatform())
	}
	binPath, err = sup.fetchBinary(m.Name, m.Version, bin)
	if err != nil {
		return 0, "", err
	}
	port, err = freePort()
	if err != nil {
		return 0, "", err
	}
	if err := sup.spawn(installID, m.Name, binPath, port, env); err != nil {
		return 0, "", err
	}
	healthPath := m.Runtime.HealthCheck
	if healthPath == "" {
		healthPath = "/health"
	}
	if err := sup.waitHealthy(installID, port, healthPath, 30*time.Second); err != nil {
		// NEW failed health → kill it and (if there was a prior
		// version parked by spawn's blue-green capture) restore OLD
		// to the procs map. After rollback, sup.PID(installID)
		// reports OLD's pid, which the caller checks to decide
		// whether to leave the install row 'running' on its
		// previous version or flip it to 'error' (fresh installs).
		_ = sup.Stop(installID)
		sup.rollbackToOld(installID)
		return 0, "", err
	}
	return port, binPath, nil
}

// Restart re-spawns an install that's already in the DB — used at boot
// to re-attach to apps the supervisor no longer holds in memory (server
// restart, child died across restart).
func (sup *LocalSupervisor) Restart(installID int64, m *sdk.Manifest, port int, binPath string, env map[string]string) error {
	if binPath == "" || port == 0 {
		return fmt.Errorf("no cached bin/port — re-install needed")
	}
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("cached binary missing at %s: %w", binPath, err)
	}
	if err := sup.spawn(installID, m.Name, binPath, port, env); err != nil {
		return err
	}
	healthPath := m.Runtime.HealthCheck
	if healthPath == "" {
		healthPath = "/health"
	}
	return sup.waitHealthy(installID, port, healthPath, 30*time.Second)
}

// Stop sends SIGTERM, waits up to 5s, then SIGKILL.
func (sup *LocalSupervisor) Stop(installID int64) error {
	sup.mu.Lock()
	p := sup.procs[installID]
	delete(sup.procs, installID)
	sup.mu.Unlock()
	terminateProc(p, 5*time.Second)
	return nil
}

// RetireOld terminates the previous-version sidecar that spawn()
// captured during a blue-green upgrade. Caller invokes this AFTER
// updating the DB + refreshing the in-memory registry, so by the
// time old dies no traffic is going to it. Safe no-op when there's
// no pending retire (every fresh-install path).
func (sup *LocalSupervisor) RetireOld(installID int64, grace time.Duration) {
	sup.pendingMu.Lock()
	p := sup.pending[installID]
	delete(sup.pending, installID)
	sup.pendingMu.Unlock()
	if p != nil {
		log.Printf("[APPS-LOCAL] retiring old sidecar install=%d pid=%d (blue-green handoff complete)",
			installID, p.cmd.Process.Pid)
	}
	terminateProc(p, grace)
}

// rollbackToOld is the failure-path counterpart to a successful
// blue-green spawn: when waitHealthy on the new sidecar fails, the
// caller has already Stop()'d the new (it was in procs[installID]).
// We then move the captured old back into procs so it remains the
// supervisor's tracked process for this install — old is still
// alive (we never killed it) and the DB still points at its port,
// so traffic keeps flowing as if the upgrade had never been attempted.
func (sup *LocalSupervisor) rollbackToOld(installID int64) {
	sup.pendingMu.Lock()
	p := sup.pending[installID]
	delete(sup.pending, installID)
	sup.pendingMu.Unlock()
	if p == nil {
		return
	}
	sup.mu.Lock()
	sup.procs[installID] = p
	sup.mu.Unlock()
	log.Printf("[APPS-LOCAL] rolled back to previous sidecar install=%d pid=%d (new failed health)",
		installID, p.cmd.Process.Pid)
}

// PID returns the OS pid of the running child for this install, or 0.
func (sup *LocalSupervisor) PID(installID int64) int {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	p := sup.procs[installID]
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// --- internals -------------------------------------------------------------

func (sup *LocalSupervisor) fetchBinary(name, version, url string) (string, error) {
	dir := filepath.Join(sup.cacheDir, name, version)
	bin := filepath.Join(dir, "bin")
	if _, err := os.Stat(bin); err == nil {
		// Already cached. Trust the cache — version-pinned URLs are
		// expected to be immutable; user can clear ~/.apteva/apps to
		// force re-download.
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	log.Printf("[APPS-LOCAL] fetching %s@%s from %s", name, version, url)
	// file:// URLs are honoured for local-only testing — useful before
	// a release exists. http(s):// is the production path.
	var src io.ReadCloser
	if filepath.IsAbs(url) || (len(url) > 7 && url[:7] == "file://") {
		path := url
		if len(url) > 7 && url[:7] == "file://" {
			path = url[7:]
		}
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open file %q: %w", path, err)
		}
		src = f
	} else {
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Get(url)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", url, err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return "", fmt.Errorf("fetch %s: http %d", url, resp.StatusCode)
		}
		src = resp.Body
	}
	defer src.Close()
	tmp := bin + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("download body: %w", err)
	}
	out.Close()
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	if err := os.Chmod(bin, 0755); err != nil {
		return "", err
	}
	return bin, nil
}

func (sup *LocalSupervisor) spawn(installID int64, appName, bin string, port int, env map[string]string) error {
	// Blue-green: if there's a live process already tracked for this
	// install_id (always true on the upgrade path — BuildFromSource
	// re-enters spawn with a fresh bin), DON'T stop it here. We
	// capture it into `pending` AFTER cmd.Start() succeeds (below),
	// so a Start failure doesn't pollute pending and we don't lose
	// OLD on a doomed exec.
	dir := filepath.Dir(bin)
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(dataDir, 0755)
	uiDir := filepath.Join(dir, "ui") // empty unless the app populated it; SDK no-ops if missing
	_ = os.MkdirAll(uiDir, 0755)
	// Per-install persistent DB dir, kept OUTSIDE the version dir so
	// an upgrade rebuild lands in a fresh <version>/ folder without
	// nuking the app's data. Layout:
	//   <appsRoot>/<name>/data/<install-id>/app.db
	// The double-parent walk turns <appsRoot>/<name>/<version>/ into
	// <appsRoot>/<name>/.
	persistentRoot := filepath.Join(filepath.Dir(dir), "data", strconv.FormatInt(installID, 10))
	_ = os.MkdirAll(persistentRoot, 0755)
	dbPath := filepath.Join(persistentRoot, "app.db")
	logPath := filepath.Join(dir, "stderr.log")
	logf, err := newRotatingLogFile(logPath, appSidecarLogMaxBytes)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(context.Background(), bin)
	cmd.Dir = dataDir
	// Strip the parent's DB_PATH and APTEVA_DATA_DIR so the sidecar
	// doesn't accidentally inherit apteva-server's own paths (the SDK
	// respects these env vars over manifest defaults, and os.Environ()
	// would otherwise leak them through).
	envv := envWithout(os.Environ(), "DB_PATH", "APTEVA_DATA_DIR")
	envv = append(envv, fmt.Sprintf("APTEVA_APP_PORT=%d", port))
	envv = append(envv, "DB_PATH="+dbPath)
	// Writable per-install dir for any persistent file the app needs
	// outside its DB (blobs, cloned repos, generated artifacts, …) —
	// same dir AppDB lives in. The SDK surfaces this via ctx.DataDir()
	// so apps stop hardcoding paths like "/data/foo" that only exist
	// inside container deployments.
	envv = append(envv, "APTEVA_DATA_DIR="+persistentRoot)
	for k, v := range env {
		envv = append(envv, k+"="+v)
	}
	cmd.Env = envv
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Setpgid puts the child into a new process group with itself as
	// the leader. Two reasons:
	//   1. Lets us kill the entire subtree (sidecar + any chromedp
	//      Chrome / external helpers it spawns) with one signal —
	//      `syscall.Kill(-pgid, SIGTERM)` — at shutdown time.
	//   2. Detaches the child's pgid from apteva-server's, so a
	//      Ctrl+C in the parent's terminal doesn't get delivered to
	//      every sidecar before our own shutdown handler runs (we
	//      want to stop apps cleanly, not have them die mid-flush).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("spawn %s: %w", appName, err)
	}
	log.Printf("[APPS-LOCAL] spawned %s install=%d pid=%d port=%d", appName, installID, cmd.Process.Pid, port)
	// Atomic swap: install NEW into procs, capture the prior occupant
	// (if any, alive) for blue-green retirement. Doing this under one
	// lock means a concurrent Stop / lookup never sees an empty slot
	// during the upgrade. Anything we displace is the caller's OLD —
	// the caller will fire RetireOld(installID) after the DB +
	// in-memory registry have been swung over to NEW, or
	// rollbackToOld(installID) if NEW fails health.
	done := make(chan struct{})
	proc := &localProc{cmd: cmd, port: port, logfile: logf, done: done}
	reap := func() {
		defer close(done)
		err := cmd.Wait()
		log.Printf("[APPS-LOCAL] child exited install=%d pid=%d err=%v", installID, cmd.Process.Pid, err)
		sup.mu.Lock()
		if p, ok := sup.procs[installID]; ok && p.cmd == cmd {
			p.stoppedAt = time.Now()
			delete(sup.procs, installID)
		}
		sup.mu.Unlock()
		_ = logf.Close()
	}
	sup.mu.Lock()
	if sup.stopping {
		sup.mu.Unlock()
		go reap()
		terminateProc(proc, 100*time.Millisecond)
		return fmt.Errorf("local supervisor is stopping")
	}
	prev := sup.procs[installID]
	sup.procs[installID] = proc
	sup.mu.Unlock()
	if prev != nil && prev.cmd != nil && prev.cmd.Process != nil &&
		processAlive(prev.cmd.Process.Pid) {
		sup.pendingMu.Lock()
		// If there's already a pending retire (an earlier upgrade
		// whose RetireOld never fired — e.g. installFromSource
		// crashed mid-flight), terminate it now. We can't carry
		// two pending olds; the older-old has been superseded by
		// the just-displaced prev anyway.
		if older := sup.pending[installID]; older != nil {
			delete(sup.pending, installID)
			go terminateProc(older, 5*time.Second)
		}
		sup.pending[installID] = prev
		sup.pendingMu.Unlock()
		log.Printf("[APPS-LOCAL] blue-green: parked previous pid=%d for install=%d (will retire after handoff)",
			prev.cmd.Process.Pid, installID)
	}
	// Reaper goroutine — log when the child exits unexpectedly.
	go reap()
	return nil
}

// envWithout returns env with any KEY=… entries for the given keys
// stripped. Used to scrub inherited APTEVA-server env vars (DB_PATH,
// APTEVA_DATA_DIR, …) so the spawned sidecar doesn't accidentally
// open the platform's sqlite file or write into the platform's data
// dir.
func envWithout(env []string, keys ...string) []string {
	out := env[:0]
outer:
	for _, e := range env {
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				continue outer
			}
		}
		out = append(out, e)
	}
	return out
}

func (sup *LocalSupervisor) waitHealthy(installID int64, port int, healthPath string, deadline time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)
	end := time.Now().Add(deadline)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(end) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("health check %s never returned 200 within %s", url, deadline)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// --- DB-side adapter the install handler + boot loader use ------------------

// installLocally runs the full local-mode install pipeline + persists
// the result into app_installs. Returns nil on success; on failure the
// row is left in 'error' status with the message stored.
//
// Concurrency: gated by three primitives (LocalSupervisor.lockApp,
// acquireInstall, acquireBuildSlot — see the type's docstring).
// inflight + per-app lock are taken UNCONDITIONALLY. The build slot
// is taken only on the heavy-pipeline path (download binary +
// spawn); static apps and dependency-resolution recursion don't pay
// its cost.
func (s *Server) installLocally(installID int64, m *sdk.Manifest, projectID string, decryptedConfig map[string]string) error {
	// Drop a duplicate goroutine for the same install_id on the
	// floor. Returning nil (not error) means: the in-flight call
	// will write the final state to the DB; we don't need to.
	if !s.localApps.acquireInstall(installID) {
		log.Printf("[APPS-LOCAL] install %d already in flight — skipping duplicate goroutine", installID)
		return nil
	}
	defer s.localApps.releaseInstall(installID)

	// Per-(app,version) lock: serialise two installs of the same
	// app+version (e.g. one global + one project install) so they
	// don't race on the shared cache-dir paths (src/, bin, gocache).
	// Different apps have different keys → no contention here.
	releaseAppLock := s.localApps.lockApp(m.Name, m.Version)
	defer releaseAppLock()

	// Static apps: no sidecar, no port, no spawn. Persist the asset
	// directory under sidecar_url_override using a `static://<abs>`
	// scheme so LoadInstalledApps can recognise the install at boot
	// and mount it on the HTTP mux. Reuses the existing column to
	// avoid a schema migration; the URL never gets dialled, only
	// parsed. See apps_static.go for the serving side.
	//
	// Resolution rules for static_dir (in priority order):
	//   1. Absolute path on disk → used as-is (built-in apps shipped
	//      inside the apteva-server image with the bundle pre-baked).
	//   2. Relative path + runtime.source set → clone the source repo
	//      into the apps cache, resolve static_dir relative to the
	//      clone. Same flow kind:source uses, minus the `go build`.
	//      Lets remote-installable static apps ship dist/ in their
	//      git repo and have apteva-server pick it up at install time.
	//   3. Relative path + no source → error (not enough info to find
	//      the bundle).
	if m.Runtime.Kind == "static" {
		dir, err := s.resolveStaticInstallDir(m)
		if err != nil {
			s.store.db.Exec(`UPDATE app_installs SET status='error', error_message=? WHERE id=?`, err.Error(), installID)
			return err
		}
		s.store.db.Exec(
			`UPDATE app_installs SET
				status='running',
				version=?,
				local_pid=0,
				local_bin_path='',
				local_port=0,
				sidecar_url_override=?,
				status_message='',
				error_message=''
			 WHERE id=?`,
			m.Version, "static://"+dir, installID)
		s.LoadInstalledApps()
		s.reconcileAllAppDepBindings()
		s.RemountStaticApps()
		if !filepath.IsAbs(strings.TrimSpace(m.Runtime.StaticDir)) {
			if removed := pruneOldAppVersions(s.localApps.cacheDir, m.Name, m.Version, appVersionRetainPrevious); len(removed) > 0 {
				log.Printf("[APPS-LOCAL] reclaimed %d stale static version dir(s) for %s: %v", len(removed), m.Name, removed)
			}
		}
		return nil
	}

	cfgJSON, _ := json.Marshal(decryptedConfig)
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_PUBLIC_URL":  s.publicBaseURL(),
		"APTEVA_APP_TOKEN":   "dev-" + strconv.FormatInt(installID, 10), // TODO: real per-install token
		"APTEVA_INSTALL_ID":  strconv.FormatInt(installID, 10),
		"APTEVA_PROJECT_ID":  projectID,
		"APTEVA_APP_CONFIG":  string(cfgJSON),
	}

	// Note: the global build-slot semaphore is acquired by the
	// outermost goroutine in handleInstallApp / handleUpgradeApp, NOT
	// here. Dep-resolution recursion (apps_dependencies.go calls into
	// installFromSource/installLocally synchronously while the parent
	// already holds a slot) would deadlock if we tried to re-acquire.
	// Outermost-only acquisition keeps the cap honest without nesting.

	port, binPath, err := s.localApps.Install(installID, m, env)
	if err != nil {
		// Same blue-green failure shape as installFromSource: if
		// the supervisor rolled back to a previous version, keep
		// status='running' so the install keeps serving on OLD
		// and agents' MCP URLs (via the loopback proxy) still
		// resolve. Only fresh-install failures flip to 'error'.
		if s.localApps.PID(installID) > 0 {
			s.markInstallRunningOnPreviousVersion(installID, err)
		} else {
			s.store.db.Exec(
				`UPDATE app_installs SET status='error', error_message=? WHERE id=?`,
				err.Error(), installID)
		}
		return err
	}
	pid := s.localApps.PID(installID)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	s.store.db.Exec(
		`UPDATE app_installs SET
			status='running',
			version=?,
			local_pid=?,
			local_bin_path=?,
			local_port=?,
			sidecar_url_override=?,
			status_message='',
			error_message=''
		 WHERE id=?`,
		m.Version, pid, binPath, port, url, installID)
	s.LoadInstalledApps()
	s.reconcileAllAppDepBindings()
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS] register MCP install=%d: %v", installID, err)
	}
	// Blue-green handoff complete: DB + registry + mcp_servers all
	// point at NEW. Terminate whatever spawn parked. No-op on fresh
	// installs.
	s.localApps.RetireOld(installID, 5*time.Second)
	if removed := pruneOldAppVersions(s.localApps.cacheDir, m.Name, m.Version, appVersionRetainPrevious); len(removed) > 0 {
		log.Printf("[APPS-LOCAL] reclaimed %d stale binary version dir(s) for %s: %v", len(removed), m.Name, removed)
	}
	return nil
}

func (s *Server) markInstallRunningOnPreviousVersion(installID int64, cause error) {
	errMsg := ""
	if cause != nil {
		errMsg = cause.Error()
	}
	s.store.db.Exec(
		`UPDATE app_installs
		    SET status='running',
		        status_message='upgrade failed; previous version still running',
		        error_message=?
		  WHERE id=?`,
		errMsg, installID,
	)
	s.LoadInstalledApps()
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS] register MCP after upgrade rollback install=%d: %v", installID, err)
	}
}

// resolveStaticInstallDir derives the absolute on-disk path for a
// static app's asset directory at install time. Lookup rules, in
// priority order:
//
//  1. Absolute static_dir → built-in apps the Dockerfile pre-baked.
//     Stat and return.
//  2. runtime.bundle set → prebuilt tarball delivery. Download,
//     verify sha256, extract under <cacheDir>/<name>/<version>/dist.
//     Browser code is platform-agnostic, so one tarball works on
//     every install host (no <os>-<arch> matrix). This is the
//     preferred path: install hosts don't need a JS toolchain.
//  3. runtime.source set → clone-on-install (no build). Useful for
//     authoring / dev loops where the repo already commits dist/.
//     Will *not* run `bun run build` for you — if dist/ isn't in
//     the cloned tree, install fails with the same error the bundle
//     path would have caught.
//  4. None of the above → error.
//
// Side effects: cache layout under <cacheDir>/<name>/<version>/ —
// "src" for clones, "dist" for bundles. Repeat installs of the same
// version short-circuit (cache hit).
func (s *Server) resolveStaticInstallDir(m *sdk.Manifest) (string, error) {
	d := strings.TrimSpace(m.Runtime.StaticDir)
	if d == "" {
		return "", fmt.Errorf("kind=static requires runtime.static_dir")
	}
	// Rule 1 — absolute path. Built-in apps the Dockerfile pre-baked
	// land here; we just stat it and return.
	if filepath.IsAbs(d) {
		if _, err := os.Stat(d); err != nil {
			return "", fmt.Errorf("static_dir does not exist or is unreadable: %s (%v)", d, err)
		}
		return d, nil
	}
	// Rule 2 — bundle declared. Download the prebuilt tarball, verify
	// its sha256, extract under the apps cache. Preferred path.
	if m.Runtime.Bundle != nil && m.Runtime.Bundle.URL != "" {
		extractDir := filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "dist")
		if err := fetchAndExtractBundle(m.Runtime.Bundle.URL, m.Runtime.Bundle.SHA256, extractDir); err != nil {
			return "", fmt.Errorf("install bundle %s@%s: %w", m.Name, m.Version, err)
		}
		// static_dir is the path *inside* the extracted tree. Common
		// values: "." (tarball was packed with `tar -C dist .`) or
		// "dist" (tarball preserved the dist/ prefix).
		full := filepath.Join(extractDir, d)
		if _, err := os.Stat(full); err != nil {
			return "", fmt.Errorf("static_dir %q not found inside extracted bundle at %s (%v)", d, extractDir, err)
		}
		return full, nil
	}
	// Rule 3 — relative + source set. Clone the repo (cache hit on
	// repeat installs of the same version), then join. No build step
	// — the cloned tree is expected to already contain static_dir.
	if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
		dir := filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "src")
		if err := cloneOrUpdate(dir, m.Runtime.Source.Repo, m.Runtime.Source.Ref); err != nil {
			return "", fmt.Errorf("clone %s@%s: %w", m.Runtime.Source.Repo, m.Runtime.Source.Ref, err)
		}
		if err := stripGitMetadata(dir); err != nil {
			log.Printf("[APPS-STATIC] strip git metadata %s: %v", dir, err)
		}
		full := filepath.Join(dir, d)
		if _, err := os.Stat(full); err != nil {
			return "", fmt.Errorf("static_dir %q not found inside cloned repo at %s (%v) — repo doesn't ship a prebuilt bundle; switch the manifest to runtime.bundle (prebuilt tarball) or commit %s/ to the source tree", d, dir, err, d)
		}
		return full, nil
	}
	// Rule 4 — no way to find the bundle.
	return "", fmt.Errorf(
		"kind=static with relative static_dir %q requires runtime.bundle (prebuilt tarball) or runtime.source.repo so the bundle can be fetched",
		d,
	)
}

// localGatewayURL is what the spawned sidecar uses to call back into
// apteva-server. Default is best-effort: "http://127.0.0.1:<our-port>".
func (s *Server) localGatewayURL() string {
	port := s.port
	if port == "" {
		port = "5280"
	}
	return "http://127.0.0.1:" + port
}

// ResumeLocalInstalls re-spawns every install whose status='running'
// but whose pid is no longer alive. Called from LoadInstalledApps at
// boot. Failures are logged + the install flips to 'error'.
func (s *Server) ResumeLocalInstalls() {
	// We pull `i.version` (what was actually installed) separately from
	// `a.manifest_json` (the upstream-refreshed snapshot) — they
	// diverge as soon as marketplace polling sees a newer published
	// version. The cached binary + source live under the INSTALLED
	// version's directory, so respawn paths must derive from
	// installedVersion, not from m.Version. Otherwise APTEVA_UI_DIR
	// points at a not-yet-cached version and every dashboard request
	// for /api/apps/<name>/ui/<Panel>.mjs returns 404.
	rows, err := s.store.db.Query(
		`SELECT i.id, i.local_pid, i.local_bin_path, i.local_port,
			COALESCE(i.project_id,''), i.config_encrypted, i.version, a.manifest_json
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status='running' AND i.local_bin_path != ''`)
	if err != nil {
		return
	}
	type resumeRow struct {
		id, pid, port                                              int64
		binPath, projectID, cfgEnc, installedVersion, manifestJSON string
	}
	var todo []resumeRow
	for rows.Next() {
		var r resumeRow
		if err := rows.Scan(&r.id, &r.pid, &r.binPath, &r.port,
			&r.projectID, &r.cfgEnc, &r.installedVersion, &r.manifestJSON); err == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}

	// Resume in parallel. Each install is independent (different
	// port, different binary, different cache dir) — sequential resume
	// adds up to ~300ms × N where the dominant cost is killOrphan's
	// SIGTERM-grace + waitHealthy on /health. With N=20+ that's a
	// painful 6-10s of stop-the-world boot. Parallelizing drops it to
	// the slowest single sidecar's wall time.
	//
	// Concurrency cap protects hosts with hundreds of installs from
	// flooding the process table all at once. Reuses
	// APTEVA_INSTALL_CONCURRENCY but raised to a less stingy default
	// (8) since resume doesn't run `go build` (no 1+ GB RSS spikes
	// per worker — that's why install is capped at 2).
	parallel := envResumeConcurrency()
	if parallel > len(todo) {
		parallel = len(todo)
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, r := range todo {
		r := r
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.resumeOneLocalInstall(r.id, r.pid, r.port, r.binPath, r.projectID, r.cfgEnc, r.installedVersion, r.manifestJSON)
		}()
	}
	wg.Wait()
}

type pendingLocalInstall struct {
	id, localPID, localPort                         int64
	appName, projectID, configEnc, version, binPath string
	manifestJSON                                    string
}

// ResumePendingLocalInstalls reclaims install/upgrade jobs that were left
// in status='pending' when the server process exited. The queue itself is
// in-memory, so a persisted "Queued — waiting for a build slot" row must
// be explicitly put back onto a fresh boot's semaphore.
func (s *Server) ResumePendingLocalInstalls() {
	if s.localApps == nil {
		return
	}
	rows, err := s.store.db.Query(
		`SELECT i.id, i.local_pid, i.local_port, i.local_bin_path,
			COALESCE(i.project_id,''), COALESCE(i.config_encrypted,''),
			i.version, a.name, a.manifest_json
		   FROM app_installs i JOIN apps a ON a.id = i.app_id
		  WHERE i.status='pending'`)
	if err != nil {
		log.Printf("[APPS-LOCAL] load pending installs: %v", err)
		return
	}
	var pending []pendingLocalInstall
	for rows.Next() {
		var r pendingLocalInstall
		if err := rows.Scan(&r.id, &r.localPID, &r.localPort, &r.binPath,
			&r.projectID, &r.configEnc, &r.version, &r.appName, &r.manifestJSON); err != nil {
			log.Printf("[APPS-LOCAL] scan pending install: %v", err)
			continue
		}
		pending = append(pending, r)
	}
	rows.Close()

	for _, r := range pending {
		var m sdk.Manifest
		if err := json.Unmarshal([]byte(r.manifestJSON), &m); err != nil {
			log.Printf("[APPS-LOCAL] pending install=%d app=%s has bad manifest: %v", r.id, r.appName, err)
			continue
		}
		if !isRecoverableLocalPending(&m) {
			continue
		}
		cfg := map[string]string(nil)
		if r.configEnc != "" {
			if plain, err := Decrypt(s.secret, r.configEnc); err == nil {
				_ = json.Unmarshal([]byte(plain), &cfg)
			}
		}
		if r.localPID > 0 && r.localPort > 0 && r.binPath != "" {
			s.resumeOneLocalInstall(r.id, r.localPID, r.localPort, r.binPath, r.projectID, r.configEnc, r.version, r.manifestJSON)
		}
		log.Printf("[APPS-LOCAL] resuming pending install=%d app=%s version=%s", r.id, m.Name, m.Version)
		s.store.db.Exec(
			`UPDATE app_installs SET status_message='Queued — waiting for a build slot' WHERE id=?`,
			r.id,
		)
		go func(r pendingLocalInstall, m sdk.Manifest, cfg map[string]string) {
			release := s.localApps.acquireBuildSlot()
			defer release()
			var err error
			if m.Runtime.Kind == "source" || m.Runtime.Source != nil {
				err = s.installFromSource(r.id, &m, r.projectID, cfg)
			} else {
				err = s.installLocally(r.id, &m, r.projectID, cfg)
			}
			if err != nil {
				log.Printf("[APPS-LOCAL] resumed pending install=%d app=%s failed: %v", r.id, m.Name, err)
				return
			}
			s.store.db.Exec(`UPDATE app_installs SET version = ? WHERE id = ?`, m.Version, r.id)
		}(r, m, cfg)
	}
}

func isRecoverableLocalPending(m *sdk.Manifest) bool {
	if m == nil {
		return false
	}
	if m.Runtime.Kind == "source" || m.Runtime.Source != nil {
		return true
	}
	_, ok := m.Runtime.Binaries[localPlatform()]
	return ok
}

// resumeOneLocalInstall is the per-install body of ResumeLocalInstalls,
// extracted so the parallel fanout can call it from a worker goroutine.
// Logging keeps the install id in every line so interleaved output
// stays attributable.
func (s *Server) resumeOneLocalInstall(id, pid, port int64, binPath, projectID, cfgEnc, installedVersion, manifestJSON string) {
	// If the recorded pid is still alive, it's an orphan from a
	// previous apteva-server (we just started — nothing we spawned
	// has had time to exist yet). The orphan still holds its port
	// + log file and would race with our respawn for the cached
	// `bin`, so kill it cleanly before proceeding. SIGTERM first,
	// SIGKILL after a short grace.
	if pid > 0 && processAlive(int(pid)) {
		log.Printf("[APPS-LOCAL] orphan install=%d pid=%d from previous server — killing before respawn", id, pid)
		killOrphan(int(pid))
	}
	var m sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		return
	}
	var cfg map[string]string
	if cfgEnc != "" {
		if plain, err := Decrypt(s.secret, cfgEnc); err == nil {
			_ = json.Unmarshal([]byte(plain), &cfg)
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_PUBLIC_URL":  s.publicBaseURL(),
		"APTEVA_APP_TOKEN":   "dev-" + strconv.FormatInt(id, 10),
		"APTEVA_INSTALL_ID":  strconv.FormatInt(id, 10),
		"APTEVA_PROJECT_ID":  projectID,
		"APTEVA_APP_CONFIG":  string(cfgJSON),
	}
	// Re-derive APTEVA_UI_DIR + APTEVA_MIGRATIONS_DIR from the
	// cached clone so the resumed sidecar can serve /ui/*Panel.mjs
	// and re-run any pending migrations. Without these, dashboard
	// requests for /api/apps/<name>/ui/<Panel>.mjs come back 404
	// after every server restart and the operator has to fully
	// reinstall — see installFromSource for the install-time
	// counterpart that sets the same env.
	//
	// Use the INSTALLED version, not m.Version. The two diverge
	// once upstream publishes a newer release: the marketplace
	// poller refreshes a.manifest_json (so m.Version becomes
	// upstream's latest), but the cached binary + source still
	// live under the installed version. m.Version-driven paths
	// would point at a not-yet-cached version dir.
	cacheVersion := installedVersion
	if cacheVersion == "" {
		cacheVersion = m.Version
	}
	if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
		srcDir := filepath.Join(s.localApps.cacheDir, m.Name, cacheVersion, "src")
		entryDir := srcDir
		if e := strings.TrimSpace(m.Runtime.Source.Entry); e != "" && e != "." {
			entryDir = filepath.Join(srcDir, e)
		}
		env["APTEVA_UI_DIR"] = filepath.Join(entryDir, "ui")
		if m.DB != nil && m.DB.Migrations != "" {
			migrations := m.DB.Migrations
			if !filepath.IsAbs(migrations) {
				migrations = filepath.Join(entryDir, migrations)
			}
			env["APTEVA_MIGRATIONS_DIR"] = migrations
		}
	}
	log.Printf("[APPS-LOCAL] resuming install=%d (pid=%d was dead) ui=%s",
		id, pid, env["APTEVA_UI_DIR"])
	if err := s.localApps.Restart(id, &m, int(port), binPath, env); err != nil {
		log.Printf("[APPS-LOCAL] resume failed install=%d: %v", id, err)
		s.store.db.Exec(`UPDATE app_installs SET status='error', error_message=? WHERE id=?`, err.Error(), id)
		return
	}
	newPID := s.localApps.PID(id)
	s.store.db.Exec(`UPDATE app_installs SET local_pid=? WHERE id=?`, newPID, id)
}

// envResumeConcurrency returns APTEVA_RESUME_CONCURRENCY parsed as
// an int, clamped to [1, 64]. Default 8. Resume is cheap per worker
// (no `go build`) so the cap is mostly defensive against pathological
// hosts with hundreds of installs spawning all at once.
func envResumeConcurrency() int {
	const dflt = 8
	v := os.Getenv("APTEVA_RESUME_CONCURRENCY")
	if v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return dflt
	}
	if n > 64 {
		n = 64
	}
	return n
}

// RespawnLocalInstall stops the running sidecar (if any) for the
// given install_id and re-spawns it with fresh env (re-decrypted
// config, current bindings, current public_url). Used after the
// operator changes integration_bindings or config — the sidecar's
// OnMount picks up the new state on next boot but doesn't re-read
// it on its own, so we have to bounce the process.
//
// Returns nil on success, non-nil if the install isn't found, the
// cached binary is missing, or the new process fails its health
// check. Updates app_installs.local_pid + status on success/failure.
func (s *Server) RespawnLocalInstall(installID int64) error {
	row := s.store.db.QueryRow(
		`SELECT i.local_pid, i.local_bin_path, i.local_port,
			COALESCE(i.project_id,''), i.config_encrypted, i.version, a.manifest_json
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	)
	var pid, port int64
	var binPath, projectID, cfgEnc, installedVersion, manifestJSON string
	if err := row.Scan(&pid, &binPath, &port, &projectID, &cfgEnc, &installedVersion, &manifestJSON); err != nil {
		return fmt.Errorf("install %d not found: %w", installID, err)
	}
	if binPath == "" {
		return fmt.Errorf("install %d has no cached binary — re-install needed", installID)
	}
	var m sdk.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		return fmt.Errorf("install %d: parse manifest: %w", installID, err)
	}

	// Stop the currently-tracked process. Stop is a no-op when the
	// supervisor doesn't have a tracked pid, so we also kill the DB-
	// recorded pid as an orphan if the supervisor lost track of it
	// (server restart between spawn and respawn, etc).
	_ = s.localApps.Stop(installID)
	if pid > 0 && processAlive(int(pid)) {
		killOrphan(int(pid))
	}

	var cfg map[string]string
	if cfgEnc != "" {
		if plain, err := Decrypt(s.secret, cfgEnc); err == nil {
			_ = json.Unmarshal([]byte(plain), &cfg)
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_PUBLIC_URL":  s.publicBaseURL(),
		"APTEVA_APP_TOKEN":   "dev-" + strconv.FormatInt(installID, 10),
		"APTEVA_INSTALL_ID":  strconv.FormatInt(installID, 10),
		"APTEVA_PROJECT_ID":  projectID,
		"APTEVA_APP_CONFIG":  string(cfgJSON),
	}
	cacheVersion := installedVersion
	if cacheVersion == "" {
		cacheVersion = m.Version
	}
	if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
		srcDir := filepath.Join(s.localApps.cacheDir, m.Name, cacheVersion, "src")
		entryDir := srcDir
		if e := strings.TrimSpace(m.Runtime.Source.Entry); e != "" && e != "." {
			entryDir = filepath.Join(srcDir, e)
		}
		env["APTEVA_UI_DIR"] = filepath.Join(entryDir, "ui")
		if m.DB != nil && m.DB.Migrations != "" {
			migrations := m.DB.Migrations
			if !filepath.IsAbs(migrations) {
				migrations = filepath.Join(entryDir, migrations)
			}
			env["APTEVA_MIGRATIONS_DIR"] = migrations
		}
	}

	log.Printf("[APPS-LOCAL] respawning install=%d after bindings/config change", installID)
	if err := s.localApps.Restart(installID, &m, int(port), binPath, env); err != nil {
		s.store.db.Exec(`UPDATE app_installs SET status='error', error_message=? WHERE id=?`, err.Error(), installID)
		return err
	}
	newPID := s.localApps.PID(installID)
	s.store.db.Exec(`UPDATE app_installs SET local_pid=?, error_message='' WHERE id=?`, newPID, installID)
	return nil
}

// processAlive — best-effort: kill -0 returns nil if the pid exists and
// we have permission to signal it. False positives possible across
// reboots if pid was recycled, but the health check at boot will catch
// that and respawn.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// killOrphan terminates a sidecar pid this server didn't spawn.
// Tries to kill the entire process group (sidecars are spawned with
// Setpgid, so any chromedp Chrome / sub-helpers go too); falls back
// to a single-pid kill if the group call fails. SIGTERM with a
// short grace, then SIGKILL.
func killOrphan(pid int) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = 0
	}
	signal := func(target int, sig syscall.Signal) {
		if pgid > 0 && target == pid {
			_ = syscall.Kill(-pgid, sig)
		} else {
			_ = syscall.Kill(target, sig)
		}
	}
	signal(pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ { // ~2s grace
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	signal(pid, syscall.SIGKILL)
}

// StopAll terminates every sidecar this supervisor is currently
// tracking. Called by apteva-server's shutdown handler so a clean
// exit doesn't leave orphans that the next boot will have to mop
// up. Concurrent — sidecars stop in parallel rather than serially.
func (sup *LocalSupervisor) StopAll(grace time.Duration) {
	sup.mu.Lock()
	sup.stopping = true
	procs := make([]*localProc, 0, len(sup.procs))
	for id, p := range sup.procs {
		procs = append(procs, p)
		delete(sup.procs, id)
	}
	sup.mu.Unlock()

	sup.pendingMu.Lock()
	for id, p := range sup.pending {
		procs = append(procs, p)
		delete(sup.pending, id)
	}
	sup.pendingMu.Unlock()

	if len(procs) == 0 {
		return
	}
	log.Printf("[APPS-LOCAL] StopAll: terminating %d sidecar(s)", len(procs))
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(p *localProc) {
			defer wg.Done()
			terminateProc(p, grace)
		}(p)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		log.Printf("[APPS-LOCAL] StopAll: %s grace expired, leaving any stragglers to the OS", grace)
	}
}
