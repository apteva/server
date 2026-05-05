package main

// Source-mode supervisor for the Apteva Apps system.
//
// Counterpart to apps_local.go. Where local mode downloads a pre-built
// binary, source mode clones the app's repo at a pinned ref, runs
// `go build`, and reuses the same spawn/healthcheck/process-tracking
// machinery from LocalSupervisor.
//
// Authors push source — no per-platform release pipeline. The trade-off
// is a Go toolchain on the host running apteva-server. Resume across
// restarts re-uses the cached binary; only changes to ref force a
// rebuild.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// humaniseBuildLine turns a stray go build output line into something
// short enough for the status pill in the dashboard. We ignore noise
// like "verifying" / module-version chatter and fall back to a short
// truncation for anything we don't recognise — keeps the UI honest
// without spamming detail nobody can read in a 30-character pill.
func humaniseBuildLine(line string) string {
	if len(line) > 80 {
		line = line[:77] + "…"
	}
	return "Building: " + line
}

// BuildFromSource clones the app repo at the requested ref, runs
// `go build`, then hands off to the existing spawn + healthcheck flow.
// Returns the spawned port + binary path so the caller can persist
// them in app_installs. progress is invoked at each phase so the
// caller can persist a human-readable status message — passing nil
// is fine.
func (sup *LocalSupervisor) BuildFromSource(installID int64, m *sdk.Manifest, env map[string]string, progress func(string)) (port int, binPath string, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	src := m.Runtime.Source
	if src == nil || src.Repo == "" {
		return 0, "", fmt.Errorf("manifest has no source.repo")
	}
	ref := src.Ref
	if ref == "" {
		ref = "main"
	}
	entry := src.Entry
	if entry == "" {
		entry = "."
	}

	dir := filepath.Join(sup.cacheDir, m.Name, m.Version)
	srcDir := filepath.Join(dir, "src")
	binPath = filepath.Join(dir, "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, "", err
	}

	progress(fmt.Sprintf("Cloning %s@%s…", src.Repo, ref))
	if err := cloneOrUpdate(srcDir, src.Repo, ref); err != nil {
		return 0, "", fmt.Errorf("clone %s@%s: %w", src.Repo, ref, err)
	}
	progress("Compiling…")
	// Pass the progress callback through so goBuild can update the
	// status as toolchain output arrives — "Downloading X dependencies",
	// "Extracting…", "Linking binary…" instead of one stale phrase.
	if err := goBuild(srcDir, entry, binPath, dir, progress); err != nil {
		return 0, "", fmt.Errorf("go build: %w", err)
	}

	port, err = freePort()
	if err != nil {
		return 0, "", err
	}
	progress("Starting sidecar…")
	// Tell the SDK where to find the panel + iframe UI bundle.
	// The spawned binary's cwd is <cacheDir>/data, but the panel
	// bundle the source-tree has is at <srcDir>/<entry>/ui — point
	// APTEVA_UI_DIR there so the SDK's static handler serves the
	// real .mjs files instead of an empty data/ui/ dir.
	entryDir := srcDir
	if entry != "" && entry != "." {
		entryDir = filepath.Join(srcDir, entry)
	}
	if env == nil {
		env = map[string]string{}
	}
	env["APTEVA_UI_DIR"] = filepath.Join(entryDir, "ui")
	// Resolve the manifest's relative migrations path to an absolute
	// directory inside the cloned source tree. The SDK respects
	// APTEVA_MIGRATIONS_DIR over the manifest field — without this,
	// a sidecar spawned with cmd.Dir = <bin>/data would look up
	// "migrations/" in the wrong place and apps would start with no
	// schema (the "no such table: files" failure).
	if m.DB != nil && m.DB.Migrations != "" {
		migrations := m.DB.Migrations
		if !filepath.IsAbs(migrations) {
			migrations = filepath.Join(entryDir, migrations)
		}
		env["APTEVA_MIGRATIONS_DIR"] = migrations
	}
	if err := sup.spawn(installID, m.Name, binPath, port, env); err != nil {
		return 0, "", err
	}
	healthPath := m.Runtime.HealthCheck
	if healthPath == "" {
		healthPath = "/health"
	}
	progress("Waiting for health check…")
	if err := sup.waitHealthy(installID, port, healthPath, 60*time.Second); err != nil {
		_ = sup.Stop(installID)
		return 0, "", err
	}
	return port, binPath, nil
}

// cloneOrUpdate ensures srcDir contains the repo at ref. Reuses an
// existing clone when possible (cheap fetch+checkout); falls back to
// fresh clone when the on-disk state is unrecognizable. Branch refs
// always update to tip; tags + SHAs are immutable so the fast path
// is a no-op once cached.
func cloneOrUpdate(srcDir, repo, ref string) error {
	repoURL := normalizeRepoURL(repo)
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err == nil {
		if err := runGit(srcDir, "fetch", "--tags", "--force", "origin"); err != nil {
			// Cache poisoned (different remote, etc.) — wipe and reclone.
			os.RemoveAll(srcDir)
		} else {
			if err := runGit(srcDir, "checkout", "--detach", refExpr(ref)); err != nil {
				return err
			}
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(srcDir), 0755); err != nil {
		return err
	}
	if err := runGit("", "clone", repoURL, srcDir); err != nil {
		return err
	}
	return runGit(srcDir, "checkout", "--detach", refExpr(ref))
}

// refExpr — turn "main" into "origin/main" so fetch+checkout works
// after a non-fresh clone. Tags + SHAs are passed through unchanged.
// The supervisor never tracks local branches, so detached-HEAD is
// always the right state.
func refExpr(ref string) string {
	if ref == "" {
		return "origin/main"
	}
	// Heuristic: looks like a SHA (hex, 7+ chars) or a tag (starts with v).
	if isLikelySHAOrTag(ref) {
		return ref
	}
	return "origin/" + ref
}

func isLikelySHAOrTag(ref string) bool {
	if strings.HasPrefix(ref, "v") {
		return true
	}
	if len(ref) >= 7 && len(ref) <= 40 {
		hex := true
		for _, r := range ref {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				hex = false
				break
			}
		}
		if hex {
			return true
		}
	}
	return false
}

// normalizeRepoURL — turn "github.com/apteva/app-tasks" into a clone
// URL. Already-prefixed URLs (https://, git@, etc.) are passed through.
func normalizeRepoURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://" + repo + ".git"
}

func runGit(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// goBuild runs `go build -o <binPath> .` inside srcDir/<entry>. The
// entry directory must contain a go.mod (each app is its own Go
// module — apteva/apps is a monorepo of independent modules, not one
// shared module). Caches (GOCACHE, GOMODCACHE) live under cacheDir so
// app builds don't fight the host's $GOPATH or pollute system caches.
//
// progress is called with humanised status strings as the toolchain
// emits new output lines, throttled to roughly every 500ms so the
// dashboard's poll loop has new content but the DB doesn't get
// hammered. Pass nil to disable.
func goBuild(srcDir, entry, binPath, cacheDir string, progress func(string)) error {
	goBin, err := resolveGoBinary()
	if err != nil {
		return err
	}
	buildDir := srcDir
	if entry != "" && entry != "." {
		buildDir = filepath.Join(srcDir, entry)
	}
	if _, err := os.Stat(filepath.Join(buildDir, "go.mod")); err != nil {
		return fmt.Errorf("entry dir %q has no go.mod — each kind:source app must be its own Go module", entry)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", binPath, ".")
	cmd.Dir = buildDir
	envv := os.Environ()
	envv = append(envv,
		"CGO_ENABLED=0",
		"GOCACHE="+filepath.Join(cacheDir, "gocache"),
		"GOMODCACHE="+filepath.Join(cacheDir, "gomodcache"),
	)
	cmd.Env = envv

	// Capture stdout + stderr together — `go build` emits download +
	// progress lines on stderr, build errors on stderr too. Stream
	// line-by-line so we can surface live status; keep a tail buffer
	// for the error message if the build fails.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}

	var (
		tail        []string                  // last N lines for error output
		lastUpdate  = time.Now()
		downloads   = 0                       // count distinct downloads
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		tail = append(tail, line)
		if len(tail) > 100 {
			tail = tail[len(tail)-100:]
		}
		if progress != nil && time.Since(lastUpdate) > 500*time.Millisecond {
			if strings.HasPrefix(line, "go: downloading ") {
				downloads++
				progress(fmt.Sprintf("Downloading dependencies (%d so far)…", downloads))
			} else if strings.HasPrefix(line, "go: extracting ") {
				progress("Extracting dependencies…")
			} else if strings.HasPrefix(line, "go: finding ") {
				progress("Resolving dependencies…")
			} else {
				progress(humaniseBuildLine(line))
			}
			lastUpdate = time.Now()
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		out := strings.Join(tail, "\n")
		return fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(out))
	}
	if progress != nil {
		progress("Linking binary…")
	}
	if err := os.Chmod(binPath, 0755); err != nil {
		return err
	}
	log.Printf("[APPS-SOURCE] built %s → %s", srcDir, binPath)
	return nil
}

// resolveGoBinary returns the path to a usable `go` toolchain. Today
// it just trusts $PATH; a future version can self-bootstrap a Go
// toolchain into ~/.apteva/go for users who don't have Go installed.
func resolveGoBinary() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("go toolchain not found on PATH — apteva-server needs Go ≥ 1.22 to build kind:source apps")
}

// --- DB-side adapter ----------------------------------------------------------

// installFromSource is the kind:source counterpart of installLocally.
// On success the install row flips to status='running' with the cached
// bin path + port; on failure the row is left in 'error' status with
// the message stored.
//
// Concurrency mirrors installLocally — see LocalSupervisor's docstring
// for what the three primitives do. Source mode is the heavy path
// (clone + go build + spawn), so the build slot is what actually
// prevents the OOM-on-bulk-install crashes the operator hits.
func (s *Server) installFromSource(installID int64, m *sdk.Manifest, projectID string, decryptedConfig map[string]string) error {
	if !s.localApps.acquireInstall(installID) {
		log.Printf("[APPS-SOURCE] install %d already in flight — skipping duplicate goroutine", installID)
		return nil
	}
	defer s.localApps.releaseInstall(installID)

	releaseAppLock := s.localApps.lockApp(m.Name, m.Version)
	defer releaseAppLock()

	cfgJSON, _ := json.Marshal(decryptedConfig)
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_PUBLIC_URL":  s.publicBaseURL(),
		"APTEVA_APP_TOKEN":   "dev-" + strconv.FormatInt(installID, 10), // TODO: real per-install token
		"APTEVA_INSTALL_ID":  strconv.FormatInt(installID, 10),
		"APTEVA_PROJECT_ID":  projectID,
		"APTEVA_APP_CONFIG":  string(cfgJSON),
	}
	progress := func(msg string) {
		s.store.db.Exec(
			`UPDATE app_installs SET status_message=? WHERE id=?`,
			msg, installID)
	}

	// Note: the global build-slot semaphore is acquired by the
	// outermost goroutine in handleInstallApp / handleUpgradeApp, NOT
	// here. Dep-resolution recursion (apps_dependencies.go calls into
	// installFromSource/installLocally synchronously while the parent
	// already holds a slot) would deadlock if we tried to re-acquire.

	port, binPath, err := s.localApps.BuildFromSource(installID, m, env, progress)
	if err != nil {
		s.store.db.Exec(
			`UPDATE app_installs SET status='error', status_message='', error_message=? WHERE id=?`,
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
			status_message='',
			error_message=''
		 WHERE id=?`,
		pid, binPath, port, url, installID)
	s.LoadInstalledApps()
	// Bridge the app's manifest tools into the platform's mcp_servers
	// table so [[list_mcp_servers]] surfaces them and agents can connect.
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS] register MCP install=%d: %v", installID, err)
	}
	return nil
}
