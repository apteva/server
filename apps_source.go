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

// BuildFromSource clones the app repo at the requested ref, runs
// `go build`, then hands off to the existing spawn + healthcheck flow.
// Returns the spawned port + binary path so the caller can persist
// them in app_installs.
func (sup *LocalSupervisor) BuildFromSource(installID int64, m *sdk.Manifest, env map[string]string) (port int, binPath string, err error) {
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

	if err := cloneOrUpdate(srcDir, src.Repo, ref); err != nil {
		return 0, "", fmt.Errorf("clone %s@%s: %w", src.Repo, ref, err)
	}
	if err := goBuild(srcDir, entry, binPath, dir); err != nil {
		return 0, "", fmt.Errorf("go build: %w", err)
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

// goBuild runs `go build -o <binPath> <entry>` inside srcDir. Caches
// (GOCACHE, GOMODCACHE) live under cacheDir so app builds don't fight
// the host's $GOPATH or pollute system caches.
func goBuild(srcDir, entry, binPath, cacheDir string) error {
	goBin, err := resolveGoBinary()
	if err != nil {
		return err
	}
	// `go build` treats a bare path like `mcp/crm` as an import path
	// rooted at GOROOT/std. For monorepos that put apps in subfolders
	// (the apteva/apps layout) we need the relative-package form
	// `./mcp/crm`. Pass through paths that already look relative or
	// absolute, or that are a single-segment Go package selector.
	buildTarget := entry
	if entry != "" && entry != "." &&
		!strings.HasPrefix(entry, "./") &&
		!strings.HasPrefix(entry, "../") &&
		!strings.HasPrefix(entry, "/") {
		buildTarget = "./" + entry
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", binPath, buildTarget)
	cmd.Dir = srcDir
	envv := os.Environ()
	envv = append(envv,
		"CGO_ENABLED=0",
		"GOCACHE="+filepath.Join(cacheDir, "gocache"),
		"GOMODCACHE="+filepath.Join(cacheDir, "gomodcache"),
	)
	cmd.Env = envv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
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
func (s *Server) installFromSource(installID int64, m *sdk.Manifest, projectID string, decryptedConfig map[string]string) error {
	cfgJSON, _ := json.Marshal(decryptedConfig)
	env := map[string]string{
		"APTEVA_GATEWAY_URL": s.localGatewayURL(),
		"APTEVA_APP_TOKEN":   "dev-" + strconv.FormatInt(installID, 10), // TODO: real per-install token
		"APTEVA_INSTALL_ID":  strconv.FormatInt(installID, 10),
		"APTEVA_PROJECT_ID":  projectID,
		"APTEVA_APP_CONFIG":  string(cfgJSON),
	}
	port, binPath, err := s.localApps.BuildFromSource(installID, m, env)
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
	return nil
}
