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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// versionDirRE matches a strict semver-shaped directory name. Used by
// pruneOldAppVersions to gate destructive RemoveAll calls — anything
// not matching this is reserved for the platform (per-install
// `data/` lives next to version dirs since the v0.9 layout) and MUST
// NOT be reaped. Pre-v0.14.3 the reclaimer only filtered names
// starting with `.` and the comment LIED ("only touch dirs that
// look like a version"); the tracking ticket from prod showed
// `data` getting added to the reap set and silently destroying every
// per-install SQLite DB on a sidecar respawn.
//
// Accepts: "0.1.0", "0.1.15", "1.0.0-rc1", "0.2.0+build7".
// Rejects: "data", "tmp", "nightly", ".gobuild", "" .
var versionDirRE = regexp.MustCompile(`^\d+\.\d+\.\d+([.+-].*)?$`)

// reservedAppSiblings is an explicit allowlist denylist for non-
// version directory names that the platform creates next to version
// dirs. Belt-and-suspenders alongside versionDirRE: even if a future
// contributor weakens or replaces the regex, named entries here stay
// protected. Add to this set when introducing any new sibling layout.
var reservedAppSiblings = map[string]bool{
	"data":     true, // per-install DB + APTEVA_DATA_DIR
	"releases": true, // reserved for future tarball cache
	"tmp":      true, // future scratch
	".gobuild": true, // shared GOCACHE/GOMODCACHE (also caught by dot-prefix)
}

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
	// Share GOCACHE + GOMODCACHE across every app build instead of
	// giving each (app × version) its own ~900 MB private cache. The
	// shared dir lives under a hidden .gobuild/ sibling so it can't
	// collide with an app called "gocache" or "gomodcache".
	gobuildDir := filepath.Join(sup.cacheDir, ".gobuild")
	if err := os.MkdirAll(gobuildDir, 0755); err != nil {
		return 0, "", err
	}

	progress(fmt.Sprintf("Cloning %s@%s…", src.Repo, ref))
	if err := cloneOrUpdate(srcDir, src.Repo, ref); err != nil {
		return 0, "", fmt.Errorf("clone %s@%s: %w", src.Repo, ref, err)
	}
	port, err = sup.buildAndSpawn(installID, m, srcDir, entry, binPath, gobuildDir, nil, env, progress)
	if err != nil {
		return 0, "", err
	}
	return port, binPath, nil
}

// buildAndSpawn compiles srcDir/<entry> → binPath, then spawns +
// health-checks the sidecar. It is the shared tail of every source-style
// install: BuildFromSource calls it after a git clone, BuildFromLocalSource
// calls it with a working-copy dir (no clone). Keeping a single tail means
// git-source and local-source installs run the identical build→spawn→health
// path — there is no second mechanism to drift from production.
func (sup *LocalSupervisor) buildAndSpawn(installID int64, m *sdk.Manifest, srcDir, entry, binPath, gobuildDir string, goEnv []string, env map[string]string, progress func(string)) (port int, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	if entry == "" {
		entry = "."
	}
	progress("Compiling…")
	// Pass the progress callback through so goBuild can update the
	// status as toolchain output arrives — "Downloading X dependencies",
	// "Extracting…", "Linking binary…" instead of one stale phrase.
	if err := goBuild(srcDir, entry, binPath, gobuildDir, goEnv, progress); err != nil {
		return 0, fmt.Errorf("go build: %w", err)
	}

	port, err = freePort()
	if err != nil {
		return 0, err
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
	// directory inside the source tree. The SDK respects
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
	// Materialize declared native-binary deps (ffmpeg / ffprobe / …)
	// and prepend their cache dirs to PATH so the spawned sidecar's
	// exec.LookPath / exec.Command calls resolve to the bundled
	// versions. No-op for apps that don't declare requires.binaries.
	if binPathPrefix, err := EnsureBinaries(m, progress); err != nil {
		return 0, fmt.Errorf("native binary dep: %w", err)
	} else if binPathPrefix != "" {
		existing := os.Getenv("PATH")
		env["PATH"] = binPathPrefix + string(os.PathListSeparator) + existing
	}
	if err := sup.spawn(installID, m.Name, binPath, port, env); err != nil {
		return 0, err
	}
	healthPath := m.Runtime.HealthCheck
	if healthPath == "" {
		healthPath = "/health"
	}
	progress("Waiting for health check…")
	if err := sup.waitHealthy(installID, port, healthPath, 60*time.Second); err != nil {
		// NEW failed health → kill it and (if there was a prior
		// version parked by spawn's blue-green capture) restore
		// OLD to the procs map. After rollback, sup.PID(installID)
		// reports OLD's pid, which installFromSource checks to
		// decide whether the install stays 'running' on its
		// previous version or flips to 'error' (fresh installs).
		_ = sup.Stop(installID)
		sup.rollbackToOld(installID)
		return 0, err
	}
	return port, nil
}

// BuildFromLocalSource compiles + spawns an app from a local working-copy
// directory (no git clone) — used by World test installs so the developer's
// CURRENT code runs, not a published ref. entry defaults to the manifest's
// source.entry (or "."). extraGoEnv lets the caller inject build env (e.g.
// GOWORK pointing at a temp workspace so the local app-sdk overlay applies).
func (sup *LocalSupervisor) BuildFromLocalSource(installID int64, m *sdk.Manifest, localSrcDir string, goEnv []string, env map[string]string, progress func(string)) (port int, binPath string, err error) {
	entry := "."
	if m.Runtime.Source != nil && m.Runtime.Source.Entry != "" {
		entry = m.Runtime.Source.Entry
	}
	dir := filepath.Join(sup.cacheDir, "_local", m.Name)
	binPath = filepath.Join(dir, "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, "", err
	}
	gobuildDir := filepath.Join(sup.cacheDir, ".gobuild")
	if err := os.MkdirAll(gobuildDir, 0755); err != nil {
		return 0, "", err
	}
	port, err = sup.buildAndSpawn(installID, m, localSrcDir, entry, binPath, gobuildDir, goEnv, env, progress)
	if err != nil {
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
func goBuild(srcDir, entry, binPath, cacheDir string, goEnv []string, progress func(string)) error {
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
	// GOCACHE / GOMODCACHE MUST be absolute — Go rejects relative
	// values with "GOMODCACHE entry is relative; must be absolute
	// path". The caller (apps_source.go's NewLocalSupervisor wiring)
	// already absolutises cacheBase, but a malformed env or a
	// future caller passing a relative path would shadow that. One
	// extra filepath.Abs call here costs nothing and keeps the
	// failure mode "fail loudly during go build" instead of "every
	// install error is the same opaque message."
	absCache := cacheDir
	if a, err := filepath.Abs(cacheDir); err == nil {
		absCache = a
	}
	envv := os.Environ()
	envv = append(envv,
		"CGO_ENABLED=0",
		"GOCACHE="+filepath.Join(absCache, "gocache"),
		"GOMODCACHE="+filepath.Join(absCache, "gomodcache"),
	)
	// Caller-supplied build env (e.g. GOWORK pointing at a temp workspace
	// so a local-source build resolves sibling modules like app-sdk from
	// the working copy). Appended last so it wins. Empty for git installs.
	envv = append(envv, goEnv...)
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
		// Blue-green failure handling. If BuildFromSource rolled
		// back to a previous version (PID > 0 means OLD is back in
		// procs), the install is still serving on its old binary
		// + old port — surface the upgrade failure as an
		// error_message but keep status='running' so the registry
		// doesn't evict the entry on the next LoadInstalledApps()
		// and agent MCP traffic keeps flowing through the proxy.
		// Fresh-install failures (no OLD to roll back to) flip the
		// row to 'error' as before.
		if s.localApps.PID(installID) > 0 {
			s.store.db.Exec(
				`UPDATE app_installs SET status_message='upgrade failed; previous version still running', error_message=? WHERE id=?`,
				err.Error(), installID)
		} else {
			s.store.db.Exec(
				`UPDATE app_installs SET status='error', status_message='', error_message=? WHERE id=?`,
				err.Error(), installID)
		}
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
	// A new install becoming running may unblock requires.apps deps
	// on parent installs that were waiting for it. Walk every running
	// install and backfill any missing app-dep bindings — idempotent
	// and only writes when a key is genuinely missing.
	s.reconcileAllAppDepBindings()
	// Bridge the app's manifest tools into the platform's mcp_servers
	// table so [[list_mcp_servers]] surfaces them and agents can connect.
	if err := s.registerAppMCP(installID); err != nil {
		log.Printf("[APPS] register MCP install=%d: %v", installID, err)
	}
	// Blue-green handoff complete: DB now points at NEW's port, the
	// in-memory registry is refreshed, and the mcp_servers row is
	// up to date. Anything still parked from the previous version
	// can be terminated. No-op on fresh installs (nothing pending).
	s.localApps.RetireOld(installID, 5*time.Second)
	// Reclaim disk: every prior version of this app under
	// <cacheDir>/<name>/<old-version>/ is dead weight now that NEW
	// is healthy. We keep the just-installed version + one previous
	// (most-recent by mtime) as a fallback if the user manually pins
	// back, and rm -rf the rest. Best-effort: errors are logged but
	// don't fail the install.
	if removed := pruneOldAppVersions(s.localApps.cacheDir, m.Name, m.Version, 1); len(removed) > 0 {
		log.Printf("[APPS-SOURCE] reclaimed %d stale version dir(s) for %s: %v", len(removed), m.Name, removed)
	}
	return nil
}

// pruneOldAppVersions deletes <cacheDir>/<appName>/<version>/ subdirs
// other than `keepCurrent` and the `keepRecent` most-recently-modified
// other versions. Returns the list of deleted version names. Safe to
// call concurrently across apps; per-app calls are serialised by the
// caller's lockApp lease.
func pruneOldAppVersions(cacheDir, appName, keepCurrent string, keepRecent int) []string {
	appDir := filepath.Join(cacheDir, appName)
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	type verEntry struct {
		name    string
		modTime time.Time
	}
	var others []verEntry
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keepCurrent {
			continue
		}
		n := e.Name()
		// Two layered guards. The pattern guard does the heavy
		// lifting — versionDirRE matches strict semver, so a
		// sibling like `data/` (per-install DB store since the
		// v0.9 layout) never enters the reap set. The reserved-
		// names allowlist is defense in depth so a future regex
		// weakening can't silently re-introduce the bug. Both must
		// pass before the entry's even a candidate.
		if reservedAppSiblings[n] {
			continue
		}
		if !versionDirRE.MatchString(n) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, verEntry{name: n, modTime: info.ModTime()})
	}
	// Sort descending by mtime; keep the first keepRecent.
	sort.Slice(others, func(i, j int) bool { return others[i].modTime.After(others[j].modTime) })
	if keepRecent < 0 {
		keepRecent = 0
	}
	if keepRecent >= len(others) {
		return nil
	}
	var removed []string
	for _, o := range others[keepRecent:] {
		path := filepath.Join(appDir, o.name)
		// Re-verify a data/ dir doesn't exist directly under the
		// version path — the SDK keeps user data at <version>/data/
		// for older app builds; we don't want to nuke that. Newer
		// installs put data under a per-install dir elsewhere.
		if _, err := os.Stat(filepath.Join(path, "data")); err == nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Printf("[APPS-SOURCE] prune %s/%s: %v", appName, o.name, err)
			continue
		}
		removed = append(removed, o.name)
	}
	return removed
}
