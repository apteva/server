package main

// apps_source_test.go — regression coverage for the version-dir
// reclaimer. The bug fixed in v0.14.3 (silent destruction of
// per-install SQLite DBs at sidecar respawn) was traced to the
// reclaimer treating the sibling `data/` directory as a stale
// version dir and rm -rf-ing it. The test below pins the fix.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// TestPruneOldAppVersions_PreservesDataDir is the load-bearing
// regression test for the v0.14.3 fix. We materialise the exact
// layout the prod sidecar had on the box that hit the bug:
//
//	apps/bills/0.1.11/
//	apps/bills/0.1.14/   (older fallback)
//	apps/bills/0.1.15/   (active)
//	apps/bills/data/18/app.db   (per-install DB)
//
// then call pruneOldAppVersions and assert the DB bytes survive.
// Any future change that destroys per-install state will fail this
// test. Pre-v0.14.3 the reclaimer would happily delete `data/` here
// and the test would lose `app.db`.
func TestPruneOldAppVersions_PreservesDataDir(t *testing.T) {
	cache := t.TempDir()
	appDir := filepath.Join(cache, "bills")
	for _, v := range []string{"0.1.11", "0.1.14", "0.1.15"} {
		if err := os.MkdirAll(filepath.Join(appDir, v), 0o755); err != nil {
			t.Fatalf("mkdir version %s: %v", v, err)
		}
	}
	// Per-install DB. Bytes are arbitrary but distinctive so we
	// can prove the file is the SAME file (not just a same-named
	// fresh empty one) after the call.
	dbDir := filepath.Join(appDir, "data", "18")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	dbPath := filepath.Join(dbDir, "app.db")
	const content = "PINNED-DB-BYTES"
	if err := os.WriteFile(dbPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write app.db: %v", err)
	}

	// Stagger mtimes so the 0.1.14 fallback is the most-recent
	// non-active version (older than 0.1.15 but newer than 0.1.11).
	// Without explicit mtimes, all three would share filesystem
	// granularity and the keepRecent=1 selection would be
	// non-deterministic.
	now := time.Now()
	mustChtime(t, filepath.Join(appDir, "0.1.11"), now.Add(-3*time.Hour))
	mustChtime(t, filepath.Join(appDir, "0.1.14"), now.Add(-1*time.Hour))
	mustChtime(t, filepath.Join(appDir, "0.1.15"), now)
	// Touch data/ so it has a recent mtime — under the pre-v0.14.3
	// shape this would have made `data` even MORE attractive to
	// the reaper (sorted by mtime descending, keepRecent=1 would
	// have picked it as the "fallback"). The fixed reclaimer must
	// reject `data` regardless of how recent its mtime is.
	mustChtime(t, filepath.Join(appDir, "data"), now)

	removed := pruneOldAppVersions(cache, "bills", "0.1.15", 1)

	// 1. data/18/app.db must still exist with the same bytes.
	got, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("data/18/app.db destroyed by reclaimer: %v", err)
	}
	if !bytes.Equal(got, []byte(content)) {
		t.Fatalf("data/18/app.db bytes mismatch: got %q want %q", got, content)
	}

	// 2. data/ must NOT appear in the removed list.
	for _, r := range removed {
		if r == "data" {
			t.Fatalf("'data' appeared in removed list: %v", removed)
		}
	}

	// 3. 0.1.11 (oldest non-active) should be gone.
	if _, err := os.Stat(filepath.Join(appDir, "0.1.11")); !os.IsNotExist(err) {
		t.Fatalf("expected 0.1.11 reaped, got err=%v", err)
	}

	// 4. 0.1.14 (most-recent non-active) should be kept as fallback.
	if _, err := os.Stat(filepath.Join(appDir, "0.1.14")); err != nil {
		t.Fatalf("0.1.14 should be kept as fallback: %v", err)
	}

	// 5. Active version untouched.
	if _, err := os.Stat(filepath.Join(appDir, "0.1.15")); err != nil {
		t.Fatalf("active version 0.1.15 must not be touched: %v", err)
	}
}

func TestPruneOldAppVersions_ReclaimsEmptyLegacyVersionDataDir(t *testing.T) {
	cache := t.TempDir()
	appDir := filepath.Join(cache, "media")
	for _, v := range []string{"0.13.21", "0.13.22", "0.13.23"} {
		if err := os.MkdirAll(filepath.Join(appDir, v, "src", ".git", "objects", "pack"), 0o755); err != nil {
			t.Fatalf("mkdir version %s: %v", v, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(appDir, "0.13.21", "data"), 0o755); err != nil {
		t.Fatalf("mkdir empty legacy data: %v", err)
	}

	removed := pruneOldAppVersions(cache, "media", "0.13.23", 0)

	if !containsPrunedVersion(removed, "0.13.21") {
		t.Fatalf("expected empty legacy data version to be removed, got %v", removed)
	}
	if _, err := os.Stat(filepath.Join(appDir, "0.13.21")); !os.IsNotExist(err) {
		t.Fatalf("expected 0.13.21 removed, got err=%v", err)
	}
}

func TestPruneOldAppVersions_PreservesNonEmptyLegacyVersionDataDir(t *testing.T) {
	cache := t.TempDir()
	appDir := filepath.Join(cache, "media")
	for _, v := range []string{"0.13.21", "0.13.22", "0.13.23"} {
		if err := os.MkdirAll(filepath.Join(appDir, v), 0o755); err != nil {
			t.Fatalf("mkdir version %s: %v", v, err)
		}
	}
	legacyDB := filepath.Join(appDir, "0.13.21", "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(legacyDB), 0o755); err != nil {
		t.Fatalf("mkdir legacy data: %v", err)
	}
	if err := os.WriteFile(legacyDB, []byte("legacy-db"), 0o644); err != nil {
		t.Fatalf("write legacy db: %v", err)
	}

	removed := pruneOldAppVersions(cache, "media", "0.13.23", 0)

	if containsPrunedVersion(removed, "0.13.21") {
		t.Fatalf("non-empty legacy data version should not be removed: %v", removed)
	}
	if got, err := os.ReadFile(legacyDB); err != nil || string(got) != "legacy-db" {
		t.Fatalf("legacy data changed: got=%q err=%v", got, err)
	}
}

func TestPruneOldAppVersionsKeeping_PreservesAllReferencedVersions(t *testing.T) {
	cache := t.TempDir()
	appDir := filepath.Join(cache, "social")
	for _, v := range []string{"0.14.11", "0.14.12", "0.14.19"} {
		if err := os.MkdirAll(filepath.Join(appDir, v), 0o755); err != nil {
			t.Fatalf("mkdir version %s: %v", v, err)
		}
	}

	removed := pruneOldAppVersionsKeeping(cache, "social", map[string]bool{
		"0.14.12": true,
		"0.14.19": true,
	}, 0)

	if containsPrunedVersion(removed, "0.14.12") || containsPrunedVersion(removed, "0.14.19") {
		t.Fatalf("referenced versions should be preserved, got %v", removed)
	}
	if _, err := os.Stat(filepath.Join(appDir, "0.14.11")); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced version removed, got err=%v", err)
	}
}

func TestStripGitMetadata_RemovesOnlyGitDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, ".git", "objects", "pack"), 0o755); err != nil {
		t.Fatalf("mkdir git metadata: %v", err)
	}
	mainFile := filepath.Join(src, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := stripGitMetadata(src); err != nil {
		t.Fatalf("stripGitMetadata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git should be removed, got err=%v", err)
	}
	if got, err := os.ReadFile(mainFile); err != nil || string(got) != "package main\n" {
		t.Fatalf("source file changed: got=%q err=%v", got, err)
	}
}

func TestCloneOrUpdateRetriesFreshCloneAndPromotesOnlyOnSuccess(t *testing.T) {
	countFile := installFakeGit(t, 3)
	withFastGitRetries(t)
	srcDir := filepath.Join(t.TempDir(), "trading", "0.4.16", "src")

	if err := cloneOrUpdate(srcDir, "github.com/apteva/apps", "main"); err != nil {
		t.Fatalf("cloneOrUpdate: %v", err)
	}

	if got := strings.TrimSpace(readFile(t, countFile)); got != "3" {
		t.Fatalf("clone attempts=%q, want 3", got)
	}
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err != nil {
		t.Fatalf("successful clone was not promoted to srcDir: %v", err)
	}
	assertNoCloneTemps(t, filepath.Dir(srcDir))
}

func TestCloneArgsUsePartialShallowCheckoutForBranchRefs(t *testing.T) {
	args := cloneArgs("https://github.com/apteva/apps.git", "/tmp/clone", "main")
	joined := strings.Join(args, " ")
	for _, want := range []string{"clone", "--filter=blob:none", "--no-checkout", "--depth=1", "--single-branch", "--branch main"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("clone args %q missing %q", joined, want)
		}
	}
}

func TestRefExprRecognizesNamespacedReleaseTags(t *testing.T) {
	tests := map[string]string{
		"":                     "origin/main",
		"main":                 "origin/main",
		"feature/source-cache": "origin/feature/source-cache",
		"v1.2.3":               "v1.2.3",
		"instances/v0.4.19":    "instances/v0.4.19",
		"0123456789abcdef":     "0123456789abcdef",
	}
	for input, want := range tests {
		if got := refExpr(input); got != want {
			t.Errorf("refExpr(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestUpdateManifestURLPrefersRegistryReleaseChannel(t *testing.T) {
	manifestURL := "https://example.test/apps/instances/apteva.yaml"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":"apteva-registry/v1","apps":[{"name":"instances","manifest_url":"` + manifestURL + `"}]}`))
	}))
	defer registry.Close()
	t.Setenv("APTEVA_APP_REGISTRY_URL", registry.URL)

	registryCacheMu.Lock()
	previousCache := registryCache
	registryCache = registryCacheEntry{}
	registryCacheMu.Unlock()
	t.Cleanup(func() {
		registryCacheMu.Lock()
		registryCache = previousCache
		registryCacheMu.Unlock()
	})

	s := newTestServer(t)
	installed := &sdk.Manifest{
		Name: "instances",
		Runtime: sdk.Runtime{Source: &sdk.SourceSpec{
			Repo:  "github.com/apteva/apps",
			Ref:   "instances/v0.4.18",
			Entry: "mcp/instances",
		}},
	}
	if got := s.updateManifestURL("instances", installed); got != manifestURL {
		t.Fatalf("updateManifestURL()=%q, want registry URL %q", got, manifestURL)
	}

	wantFallback := "https://raw.githubusercontent.com/apteva/apps/instances/v0.4.18/mcp/instances/apteva.yaml"
	if got := s.updateManifestURL("private-app", installed); got != wantFallback {
		t.Fatalf("fallback updateManifestURL()=%q, want %q", got, wantFallback)
	}
}

func TestNormalizeSparsePaths(t *testing.T) {
	got := normalizeSparsePaths("mcp/code/", "/ui/dist", ".", "", "mcp/code")
	want := []string{"mcp/code", "ui/dist"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sparse paths=%v want %v", got, want)
	}
}

func TestCloneOrUpdateCleansPartialCloneOnFailure(t *testing.T) {
	countFile := installFakeGit(t, 99)
	withFastGitRetries(t)
	srcDir := filepath.Join(t.TempDir(), "trading", "0.4.16", "src")

	err := cloneOrUpdate(srcDir, "github.com/apteva/apps", "main")
	if err == nil {
		t.Fatal("expected clone failure")
	}

	if got := strings.TrimSpace(readFile(t, countFile)); got != "3" {
		t.Fatalf("clone attempts=%q, want 3", got)
	}
	if _, statErr := os.Stat(srcDir); !os.IsNotExist(statErr) {
		t.Fatalf("partial clone should not be promoted to srcDir, stat err=%v", statErr)
	}
	assertNoCloneTemps(t, filepath.Dir(srcDir))
}

func TestCloneOrUpdateReportsCloneTimeoutAndCleansTemps(t *testing.T) {
	installSlowGit(t)
	withFastGitRetries(t)
	prevTimeout := gitCloneTimeout
	gitCloneTimeout = 20 * time.Millisecond
	t.Cleanup(func() { gitCloneTimeout = prevTimeout })
	srcDir := filepath.Join(t.TempDir(), "code", "0.5.10", "src")

	err := cloneOrUpdate(srcDir, "github.com/apteva/apps", "main")
	if err == nil {
		t.Fatal("expected clone timeout")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error should include timeout detail, got %v", err)
	}
	if _, statErr := os.Stat(srcDir); !os.IsNotExist(statErr) {
		t.Fatalf("partial clone should not be promoted to srcDir, stat err=%v", statErr)
	}
	assertNoCloneTemps(t, filepath.Dir(srcDir))
}

func TestCleanupFailedSourceVersionDirRemovesOnlyWhenNoBinaryExists(t *testing.T) {
	root := t.TempDir()
	failedVersion := filepath.Join(root, "trading", "0.4.16")
	if err := os.MkdirAll(filepath.Join(failedVersion, "src"), 0o755); err != nil {
		t.Fatalf("mkdir failed version: %v", err)
	}
	cleanupFailedSourceVersionDir(failedVersion, filepath.Join(failedVersion, "bin"))
	if _, err := os.Stat(failedVersion); !os.IsNotExist(err) {
		t.Fatalf("failed version without binary should be removed, err=%v", err)
	}

	healthyVersion := filepath.Join(root, "trading", "0.4.15")
	binPath := filepath.Join(healthyVersion, "bin")
	if err := os.MkdirAll(healthyVersion, 0o755); err != nil {
		t.Fatalf("mkdir healthy version: %v", err)
	}
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	cleanupFailedSourceVersionDir(healthyVersion, binPath)
	if _, err := os.Stat(healthyVersion); err != nil {
		t.Fatalf("version with binary should be preserved: %v", err)
	}
}

func TestCleanupFailedSourceVersionDirRemovesCloneTempsBesideHealthyBinary(t *testing.T) {
	root := t.TempDir()
	healthyVersion := filepath.Join(root, "code", "0.5.9")
	binPath := filepath.Join(healthyVersion, "bin")
	cloneTemp := filepath.Join(healthyVersion, ".clone-deadbeef", ".git", "objects")
	if err := os.MkdirAll(cloneTemp, 0o755); err != nil {
		t.Fatalf("mkdir clone temp: %v", err)
	}
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	cleanupFailedSourceVersionDir(healthyVersion, binPath)

	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("healthy binary should be preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(healthyVersion, ".clone-deadbeef")); !os.IsNotExist(err) {
		t.Fatalf("clone temp should be removed, got err=%v", err)
	}
}

func installFakeGit(t *testing.T, succeedOnAttempt int) string {
	t.Helper()
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	script := filepath.Join(dir, "git")
	body := `#!/bin/sh
set -eu
cmd="${1:-}"
case "$cmd" in
  clone)
    count=0
    if [ -f "$APTEVA_FAKE_GIT_COUNT" ]; then count=$(cat "$APTEVA_FAKE_GIT_COUNT"); fi
    count=$((count + 1))
    echo "$count" > "$APTEVA_FAKE_GIT_COUNT"
    for last do :; done
    dest="$last"
    mkdir -p "$dest/partial"
    if [ "$count" -lt "$APTEVA_FAKE_GIT_SUCCEED_ON" ]; then
      echo "LibreSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443" >&2
      exit 56
    fi
    mkdir -p "$dest/.git"
    echo "package main" > "$dest/main.go"
    exit 0
    ;;
  checkout|fetch|sparse-checkout)
    exit 0
    ;;
esac
echo "unexpected git command: $*" >&2
exit 2
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("APTEVA_FAKE_GIT_COUNT", countFile)
	t.Setenv("APTEVA_FAKE_GIT_SUCCEED_ON", strconv.Itoa(succeedOnAttempt))
	return countFile
}

func installSlowGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	body := `#!/bin/sh
set -eu
cmd="${1:-}"
case "$cmd" in
  clone)
    for last do :; done
    dest="$last"
    mkdir -p "$dest/partial"
    sleep 5
    exit 0
    ;;
  checkout|fetch|sparse-checkout)
    exit 0
    ;;
esac
echo "unexpected git command: $*" >&2
exit 2
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write slow fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func withFastGitRetries(t *testing.T) {
	t.Helper()
	prev := gitRetryDelays
	gitRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { gitRetryDelays = prev })
}

func assertNoCloneTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read clone parent: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clone-") {
			t.Fatalf("temporary clone dir was not removed: %s", entry.Name())
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestVersionDirRE_AcceptsRejects pins the regex behaviour. Without
// this, a future "let me make the regex more permissive" tweak
// could silently reintroduce the data-dir destruction bug.
func TestVersionDirRE_AcceptsRejects(t *testing.T) {
	for _, ok := range []string{
		"0.1.0",
		"0.1.15",
		"1.0.0",
		"1.0.0-rc1",
		"1.0.0-rc.2",
		"0.2.0+build7",
		"0.2.0.alpha", // four-segment is allowed via the optional tail
	} {
		if !versionDirRE.MatchString(ok) {
			t.Errorf("expected %q to match versionDirRE", ok)
		}
	}
	for _, bad := range []string{
		"",
		"data",
		"tmp",
		"nightly",
		".gobuild",
		"v0.1.0", // leading 'v' not allowed; install_source.go writes bare semver
		"0.1",    // two-segment isn't a valid app version
		"latest",
		"current",
	} {
		if versionDirRE.MatchString(bad) {
			t.Errorf("expected %q to be REJECTED by versionDirRE", bad)
		}
	}
}

// TestPruneOldAppVersions_ReservedNamesAlwaysSafe makes sure the
// allowlist guard (the second layer of defence) catches anything
// the regex would otherwise accept. Belt-and-suspenders — even if
// a contributor decides "let's accept short names like `latest`",
// reserved entries stay protected.
func TestPruneOldAppVersions_ReservedNamesAlwaysSafe(t *testing.T) {
	cache := t.TempDir()
	appDir := filepath.Join(cache, "demo")
	for _, name := range []string{"data", "releases", "tmp", "0.1.0", "0.2.0"} {
		if err := os.MkdirAll(filepath.Join(appDir, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	// Drop a sentinel file inside each reserved dir so we can
	// confirm survival with byte equality, not just stat.
	for _, name := range []string{"data", "releases", "tmp"} {
		f := filepath.Join(appDir, name, "sentinel")
		if err := os.WriteFile(f, []byte(name), 0o644); err != nil {
			t.Fatalf("write sentinel %s: %v", name, err)
		}
	}

	pruneOldAppVersions(cache, "demo", "0.2.0", 0)

	for _, name := range []string{"data", "releases", "tmp"} {
		f := filepath.Join(appDir, name, "sentinel")
		got, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("reserved sibling %s destroyed: %v", name, err)
			continue
		}
		if string(got) != name {
			t.Errorf("reserved sibling %s sentinel mutated: got %q want %q", name, got, name)
		}
	}
}

func mustChtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func containsPrunedVersion(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
