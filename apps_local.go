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

// LocalSupervisor tracks running app subprocesses keyed by install id.
// One instance per Server, created in startup before LoadInstalledApps.
type LocalSupervisor struct {
	cacheDir string
	mu       sync.Mutex
	procs    map[int64]*localProc
}

type localProc struct {
	cmd        *exec.Cmd
	port       int
	logfile    *os.File
	stoppedAt  time.Time
}

// NewLocalSupervisor returns a supervisor whose binary cache lives at
// cacheDir (typically ~/.apteva/apps).
func NewLocalSupervisor(cacheDir string) *LocalSupervisor {
	_ = os.MkdirAll(cacheDir, 0755)
	return &LocalSupervisor{cacheDir: cacheDir, procs: map[int64]*localProc{}}
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
		_ = sup.Stop(installID)
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
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	if p.logfile != nil {
		_ = p.logfile.Close()
	}
	return nil
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
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
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
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("spawn %s: %w", appName, err)
	}
	log.Printf("[APPS-LOCAL] spawned %s install=%d pid=%d port=%d", appName, installID, cmd.Process.Pid, port)
	sup.mu.Lock()
	sup.procs[installID] = &localProc{cmd: cmd, port: port, logfile: logf}
	sup.mu.Unlock()
	// Reaper goroutine — log when the child exits unexpectedly.
	go func() {
		err := cmd.Wait()
		log.Printf("[APPS-LOCAL] child exited install=%d pid=%d err=%v", installID, cmd.Process.Pid, err)
		sup.mu.Lock()
		if p, ok := sup.procs[installID]; ok && p.cmd == cmd {
			p.stoppedAt = time.Now()
			delete(sup.procs, installID)
		}
		sup.mu.Unlock()
		_ = logf.Close()
	}()
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
func (s *Server) installLocally(installID int64, m *sdk.Manifest, projectID string, decryptedConfig map[string]string) error {
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
				local_pid=0,
				local_bin_path='',
				local_port=0,
				sidecar_url_override=?,
				error_message=''
			 WHERE id=?`,
			"static://"+dir, installID)
		s.LoadInstalledApps()
		s.RemountStaticApps()
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
	port, binPath, err := s.localApps.Install(installID, m, env)
	if err != nil {
		s.store.db.Exec(
			`UPDATE app_installs SET status='error', error_message=? WHERE id=?`,
			err.Error(), installID)
		return err
	}
	pid := s.localApps.PID(installID)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	s.store.db.Exec(
		`UPDATE app_installs SET
			status='running',
			local_pid=?,
			local_bin_path=?,
			local_port=?,
			sidecar_url_override=?,
			error_message=''
		 WHERE id=?`,
		pid, binPath, port, url, installID)
	s.LoadInstalledApps()
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS] register MCP install=%d: %v", installID, err)
	}
	return nil
}

// resolveStaticInstallDir derives the absolute on-disk path for a
// static app's asset directory at install time. It implements the
// three-rule lookup described at the call site: absolute path → use
// as-is; relative + source → clone and join; relative + no source →
// error.
//
// Side effects: when cloning, the repo lands under
// $cacheDir/<name>/<version>/src — same convention installFromSource
// uses, so subsequent installs / restarts of the same version are a
// no-op fetch.
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
	// Rule 2 — relative + source set. Clone the repo (cache hit on
	// repeat installs of the same version), then join.
	if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
		dir := filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "src")
		if err := cloneOrUpdate(dir, m.Runtime.Source.Repo, m.Runtime.Source.Ref); err != nil {
			return "", fmt.Errorf("clone %s@%s: %w", m.Runtime.Source.Repo, m.Runtime.Source.Ref, err)
		}
		full := filepath.Join(dir, d)
		if _, err := os.Stat(full); err != nil {
			return "", fmt.Errorf("static_dir %q not found inside cloned repo at %s (%v)", d, dir, err)
		}
		return full, nil
	}
	// Rule 3 — relative + no source. We don't know where to look.
	return "", fmt.Errorf(
		"kind=static with relative static_dir %q requires runtime.source.repo so the bundle can be fetched",
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
	rows, err := s.store.db.Query(
		`SELECT i.id, i.local_pid, i.local_bin_path, i.local_port,
			COALESCE(i.project_id,''), i.config_encrypted, a.manifest_json
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.status='running' AND i.local_bin_path != ''`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, pid, port int64
		var binPath, projectID, cfgEnc, manifestJSON string
		if err := rows.Scan(&id, &pid, &binPath, &port, &projectID, &cfgEnc, &manifestJSON); err != nil {
			continue
		}
		// Already alive? Nothing to do.
		if pid > 0 && processAlive(int(pid)) {
			continue
		}
		var m sdk.Manifest
		if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
			continue
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
		if m.Runtime.Source != nil && m.Runtime.Source.Repo != "" {
			srcDir := filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "src")
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
			continue
		}
		newPID := s.localApps.PID(id)
		s.store.db.Exec(`UPDATE app_installs SET local_pid=? WHERE id=?`, newPID, id)
	}
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
