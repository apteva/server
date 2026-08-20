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

const appVersionRetainPrevious = 1

var (
	gitRetryDelays    = []time.Duration{1 * time.Second, 3 * time.Second}
	gitCommandTimeout = 5 * time.Minute
	gitCloneTimeout   = 15 * time.Minute
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
	binPath, err = sup.BuildFromSourceBinary(m, progress)
	if err != nil {
		return 0, "", err
	}
	port, err = sup.startBuiltSource(installID, m, binPath, env, progress)
	if err != nil {
		cleanupFailedSourceVersionDir(filepath.Dir(binPath), binPath)
		return 0, "", err
	}
	return port, binPath, nil
}

// BuildFromSourceBinary reconstructs an exact source runtime without starting
// it. Clone quarantine uses this to make copied installs portable while keeping
// app workers and external side effects stopped.
func (sup *LocalSupervisor) BuildFromSourceBinary(m *sdk.Manifest, progress func(string)) (binPath string, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	src := m.Runtime.Source
	if src == nil || src.Repo == "" {
		return "", fmt.Errorf("manifest has no source.repo")
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
		return "", err
	}
	// Share GOCACHE + GOMODCACHE across every app build instead of
	// giving each (app × version) its own ~900 MB private cache. The
	// shared dir lives under a hidden .gobuild/ sibling so it can't
	// collide with an app called "gocache" or "gomodcache".
	gobuildDir := filepath.Join(sup.cacheDir, ".gobuild")
	if err := os.MkdirAll(gobuildDir, 0755); err != nil {
		return "", err
	}

	progress(fmt.Sprintf("Cloning %s@%s…", src.Repo, ref))
	if err := cloneOrUpdate(srcDir, src.Repo, ref, entry); err != nil {
		cleanupFailedSourceVersionDir(dir, binPath)
		return "", fmt.Errorf("clone %s@%s: %w", src.Repo, ref, err)
	}
	progress("Compiling…")
	if err := goBuild(srcDir, entry, binPath, gobuildDir, nil, progress); err != nil {
		cleanupFailedSourceVersionDir(dir, binPath)
		return "", fmt.Errorf("go build: %w", err)
	}
	if err := stripGitMetadata(srcDir); err != nil {
		log.Printf("[APPS-SOURCE] strip git metadata %s: %v", srcDir, err)
	}
	return binPath, nil
}

func (sup *LocalSupervisor) startBuiltSource(installID int64, m *sdk.Manifest, binPath string, env map[string]string, progress func(string)) (int, error) {
	srcDir := filepath.Join(filepath.Dir(binPath), "src")
	entry := "."
	if m.Runtime.Source != nil && m.Runtime.Source.Entry != "" {
		entry = m.Runtime.Source.Entry
	}
	return sup.spawnBuiltSource(installID, m, srcDir, entry, binPath, env, progress)
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
	return sup.spawnBuiltSource(installID, m, srcDir, entry, binPath, env, progress)
}

func (sup *LocalSupervisor) spawnBuiltSource(installID int64, m *sdk.Manifest, srcDir, entry, binPath string, env map[string]string, progress func(string)) (port int, err error) {
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
	spec, err := newActivationSpec(installID, m, binPath, port, env)
	if err != nil {
		return 0, err
	}
	progress("Waiting for health check…")
	if err := sup.activate(spec, 60*time.Second); err != nil {
		return 0, err
	}
	return port, nil
}

// BuildFromLocalSource compiles + spawns an app from a local working-copy
// directory (no git clone) — used by Environment test installs so the developer's
// CURRENT code runs, not a published ref. entry defaults to the manifest's
// source.entry (or "."). extraGoEnv lets the caller inject build env (e.g.
// GOWORK pointing at a temp workspace so the local app-sdk overlay applies).
func (sup *LocalSupervisor) BuildFromLocalSource(installID int64, m *sdk.Manifest, localSrcDir string, goEnv []string, env map[string]string, progress func(string)) (port int, binPath string, err error) {
	// localSrcDir already points at the app's module dir (where go.mod
	// lives), so entry is always "." — unlike a git source whose entry is
	// relative to the repo root (e.g. "mcp/social").
	entry := "."
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
func cloneOrUpdate(srcDir, repo, ref string, sparsePaths ...string) error {
	repoURL := normalizeRepoURL(repo)
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err == nil {
		if err := runGitWithRetry(srcDir, "fetch", "--tags", "--force", "origin"); err != nil {
			// Cache poisoned (different remote, etc.) — wipe and reclone.
			os.RemoveAll(srcDir)
		} else {
			if err := runGit(srcDir, "checkout", "--detach", refExpr(ref)); err != nil {
				return err
			}
			return nil
		}
	}
	if _, err := os.Stat(srcDir); err == nil {
		// Successful installs strip src/.git after build so runtime app
		// dirs don't retain hundreds of MB of object packs. A future
		// rebuild of the same version therefore starts from a clean clone.
		if err := os.RemoveAll(srcDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(srcDir), 0755); err != nil {
		return err
	}
	parent := filepath.Dir(srcDir)
	cleanupCloneTemps(parent)
	tmpDir, err := cloneFreshWithRetry(parent, repoURL, ref, normalizeSparsePaths(sparsePaths...))
	if err != nil {
		cleanupCloneTemps(parent)
		return err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := runGit(tmpDir, "checkout", "--detach", refExpr(ref)); err != nil {
		return err
	}
	if err := os.RemoveAll(srcDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, srcDir); err != nil {
		return err
	}
	cleanupTmp = false
	return nil
}

func cleanupFailedSourceVersionDir(versionDir, binPath string) {
	cleanupCloneTemps(versionDir)
	if _, err := os.Stat(binPath); err == nil {
		return
	}
	if err := os.RemoveAll(versionDir); err != nil {
		log.Printf("[APPS-SOURCE] cleanup failed source dir %s: %v", versionDir, err)
	}
}

func cloneFreshWithRetry(parent, repoURL, ref string, sparsePaths []string) (string, error) {
	var lastErr error
	attempts := len(gitRetryDelays) + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(gitRetryDelays[attempt-2])
		}
		tmpDir, err := os.MkdirTemp(parent, ".clone-")
		if err != nil {
			return "", err
		}
		start := time.Now()
		log.Printf("[APPS-SOURCE] git clone start repo=%s dest=%s attempt=%d/%d timeout=%s",
			repoURL, tmpDir, attempt, attempts, gitCloneTimeout)
		err = runGitWithTimeout(gitCloneTimeout, "", cloneArgs(repoURL, tmpDir, ref)...)
		if err == nil && len(sparsePaths) > 0 {
			if sparseErr := configureSparseCheckout(tmpDir, sparsePaths); sparseErr != nil {
				err = sparseErr
			}
		}
		if err == nil {
			log.Printf("[APPS-SOURCE] git clone complete repo=%s dest=%s attempt=%d/%d duration=%s",
				repoURL, tmpDir, attempt, attempts, time.Since(start).Round(time.Millisecond))
			return tmpDir, nil
		}
		lastErr = err
		_ = os.RemoveAll(tmpDir)
		if attempt < attempts {
			log.Printf("[APPS-SOURCE] git clone failed repo=%s dest=%s attempt=%d/%d duration=%s: %v; retrying",
				repoURL, tmpDir, attempt, attempts, time.Since(start).Round(time.Millisecond), err)
		}
	}
	return "", lastErr
}

func cloneArgs(repoURL, tmpDir, ref string) []string {
	args := []string{"clone", "--filter=blob:none", "--no-checkout"}
	trimmedRef := strings.TrimSpace(ref)
	if trimmedRef == "" {
		trimmedRef = "main"
	}
	if !isLikelySHA(trimmedRef) {
		args = append(args, "--depth=1", "--single-branch", "--branch", trimmedRef)
	}
	args = append(args, repoURL, tmpDir)
	return args
}

func configureSparseCheckout(repoDir string, sparsePaths []string) error {
	if len(sparsePaths) == 0 {
		return nil
	}
	if err := runGit(repoDir, "sparse-checkout", "init", "--cone"); err != nil {
		return err
	}
	args := append([]string{"sparse-checkout", "set"}, sparsePaths...)
	return runGit(repoDir, args...)
}

func normalizeSparsePaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		p := strings.TrimSpace(filepath.ToSlash(path))
		p = strings.TrimPrefix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func cleanupCloneTemps(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".clone-") {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			log.Printf("[APPS-SOURCE] cleanup clone temp %s: %v", path, err)
		}
	}
}

func stripGitMetadata(srcDir string) error {
	gitDir := filepath.Join(srcDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(gitDir)
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
	// App release tags are namespaced by slug (for example
	// "instances/v0.4.19"). Treat a version-shaped final path segment
	// as a tag so checkout uses the local tag ref instead of inventing
	// an origin/<tag> remote-tracking branch.
	if strings.HasPrefix(filepath.Base(ref), "v") {
		return true
	}
	return isLikelySHA(ref)
}

func isLikelySHA(ref string) bool {
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
	return runGitWithTimeout(gitCommandTimeout, dir, args...)
}

func runGitWithTimeout(timeout time.Duration, dir string, args ...string) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git %s timed out after %s: %w: %s", strings.Join(args, " "), timeout, err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	log.Printf("[APPS-SOURCE] git %s completed in %s", strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
	return nil
}

func runGitWithRetry(dir string, args ...string) error {
	var lastErr error
	attempts := len(gitRetryDelays) + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(gitRetryDelays[attempt-2])
		}
		err := runGit(dir, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < attempts {
			log.Printf("[APPS-SOURCE] git %s failed attempt %d/%d: %v; retrying",
				strings.Join(args, " "), attempt, attempts, err)
		}
	}
	return lastErr
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
		tail       []string // last N lines for error output
		lastUpdate = time.Now()
		downloads  = 0 // count distinct downloads
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
	oldMCPSurface := s.snapshotAppMCPSurface(installID)

	releaseAppLock := s.localApps.lockApp(m.Name, m.Version)
	defer releaseAppLock()

	cfgJSON, _ := json.Marshal(decryptedConfig)
	appToken, err := s.appInstallToken(installID)
	if err != nil {
		return fmt.Errorf("create app credential: %w", err)
	}
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_PUBLIC_URL":  s.publicBaseURL(),
		"APTEVA_APP_TOKEN":   appToken,
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
		s.localApps.DiscardPendingFixedPorts(installID)
		// A source failure may occur during clone/build while OLD is still
		// serving, or during activation after a verified rollback. Preserve
		// status='running' only after checking OLD's HTTP and fixed TCP
		// readiness; process existence alone is not sufficient evidence.
		if activationRollbackVerified(err) || s.localApps.verifyCurrentProc(installID, 5*time.Second) {
			s.markInstallRunningOnPreviousVersion(installID, err)
		} else {
			s.store.db.Exec(
				`UPDATE app_installs SET status='error', status_message='', error_message=?, pending_manifest_json='' WHERE id=?`,
				err.Error(), installID)
		}
		return err
	}
	// Agent Plugins compatibility is additive to the native app install. The
	// source checkout is now complete, so discover fixed skills/*/SKILL.md
	// paths and merge valid portable skills with the existing manifest list.
	// A malformed plugin never takes the healthy native sidecar down; the
	// manifest-declared skills registered earlier remain in place.
	pluginRoot := filepath.Join(filepath.Dir(binPath), "src")
	if m.Runtime.Source != nil && m.Runtime.Source.Entry != "" && m.Runtime.Source.Entry != "." {
		pluginRoot = filepath.Join(pluginRoot, filepath.FromSlash(m.Runtime.Source.Entry))
	}
	if err := s.syncAgentPluginSkillsForInstall(installID, m, projectID, pluginRoot); err != nil {
		log.Printf("[AGENT-PLUGIN] app=%s install=%d compatibility sync failed: %v", m.Name, installID, err)
	}
	pid := s.localApps.PID(installID)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	manifestJSON, _ := json.Marshal(m)
	s.store.db.Exec(
		`UPDATE app_installs SET
			status='running',
			version=?,
			manifest_json=?,
			pending_manifest_json='',
			local_pid=?,
			local_bin_path=?,
			local_port=?,
			sidecar_url_override=?,
			status_message='',
			error_message=''
		 WHERE id=?`,
		m.Version, string(manifestJSON), pid, binPath, port, url, installID)
	s.LoadInstalledApps()
	// A new install becoming running may unblock requires.apps deps
	// on parent installs that were waiting for it. Walk every running
	// install and backfill any missing app-dep bindings — idempotent
	// and only writes when a key is genuinely missing.
	s.reconcileAllAppDepBindings()
	// Bridge the app's manifest tools into the platform's mcp_servers
	// table so [[list_mcp_servers]] surfaces them and agents can connect.
	if err := s.registerAppMCPAfterActivation(installID, oldMCPSurface); err != nil {
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
	if removed := s.pruneUnreferencedAppVersions(m.Name, m.Version); len(removed) > 0 {
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
	keep := map[string]bool{}
	if keepCurrent != "" {
		keep[keepCurrent] = true
	}
	return pruneOldAppVersionsKeeping(cacheDir, appName, keep, keepRecent)
}

// pruneUnreferencedAppVersions retains every version referenced by a running
// or pending install across all projects, plus the version that just became
// healthy. This is the install-aware cleanup path; the lower-level helper is
// intentionally DB-agnostic for tests and maintenance tools.
func (s *Server) pruneUnreferencedAppVersions(appName, currentVersion string) []string {
	if s == nil || s.localApps == nil {
		return nil
	}
	keep := map[string]bool{}
	if currentVersion != "" {
		keep[currentVersion] = true
	}
	rows, err := s.store.db.Query(
		`SELECT i.version
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE a.name = ? AND i.status IN ('running', 'pending') AND i.version != ''`,
		appName,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		if rows.Scan(&version) == nil && version != "" {
			keep[version] = true
		}
	}
	return pruneOldAppVersionsKeeping(s.localApps.cacheDir, appName, keep, appVersionRetainPrevious)
}

func pruneOldAppVersionsKeeping(cacheDir, appName string, keepVersions map[string]bool, keepRecent int) []string {
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
		if !e.IsDir() || keepVersions[e.Name()] {
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
	retainCount := keepRecent
	if retainCount > len(others) {
		retainCount = len(others)
	}
	for _, o := range others[:retainCount] {
		if err := stripGitMetadata(filepath.Join(appDir, o.name, "src")); err != nil {
			log.Printf("[APPS-SOURCE] strip git metadata %s/%s: %v", appName, o.name, err)
		}
	}
	if retainCount >= len(others) {
		return nil
	}
	var removed []string
	for _, o := range others[retainCount:] {
		path := filepath.Join(appDir, o.name)
		// Older app builds briefly created <version>/data. An empty
		// directory is not durable state and must not pin a full source
		// checkout forever; only preserve the version if that legacy data
		// tree contains actual files.
		if hasFiles(filepath.Join(path, "data")) {
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

func hasFiles(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if !d.IsDir() {
			found = true
		}
		return nil
	})
	return found
}

func (s *Server) PruneInstalledAppVersions() {
	if s.localApps == nil {
		return
	}
	rows, err := s.store.db.Query(
		`SELECT a.name, i.version
		   FROM app_installs i JOIN apps a ON a.id = i.app_id
		  WHERE i.status IN ('running', 'pending') AND i.version != ''`)
	if err != nil {
		log.Printf("[APPS-SOURCE] load installed versions for prune: %v", err)
		return
	}
	keepByApp := map[string]map[string]bool{}
	for rows.Next() {
		var appName, version string
		if err := rows.Scan(&appName, &version); err != nil {
			continue
		}
		if keepByApp[appName] == nil {
			keepByApp[appName] = map[string]bool{}
		}
		keepByApp[appName][version] = true
	}
	rows.Close()
	for appName, keep := range keepByApp {
		for version := range keep {
			cleanupCloneTemps(filepath.Join(s.localApps.cacheDir, appName, version))
			srcDir := filepath.Join(s.localApps.cacheDir, appName, version, "src")
			if err := stripGitMetadata(srcDir); err != nil {
				log.Printf("[APPS-SOURCE] strip git metadata %s: %v", srcDir, err)
			}
		}
		if removed := pruneOldAppVersionsKeeping(s.localApps.cacheDir, appName, keep, appVersionRetainPrevious); len(removed) > 0 {
			log.Printf("[APPS-SOURCE] boot reclaimed %d stale version dir(s) for %s: %v", len(removed), appName, removed)
		}
	}
}
