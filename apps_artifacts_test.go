package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const artifactFixtureManifest = `schema: apteva-app/v1
name: artifact-test
version: 1.2.3
runtime:
  kind: source
  port: 8080
  source:
    repo: https://example.invalid/never-clone
    ref: main
    entry: mcp/artifact-test
`

func artifactFixture(t *testing.T, bin []byte, extra map[string][]byte) (*sdk.Manifest, []byte) {
	t.Helper()
	m, err := sdk.ParseManifest([]byte(artifactFixtureManifest))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"bin": bin, "src/mcp/artifact-test/apteva.yaml": []byte(artifactFixtureManifest), "src/mcp/artifact-test/ui/panel.mjs": []byte("export default 42")}
	for k, v := range extra {
		files[k] = v
	}
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for path, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return m, out.Bytes()
}
func attachArtifact(m *sdk.Manifest, data []byte, url string) {
	sum := sha256.Sum256(data)
	m.Runtime.Artifacts = map[string]sdk.BundleSpec{localPlatform(): {URL: url, SHA256: hex.EncodeToString(sum[:])}}
}
func localArtifact(t *testing.T, m *sdk.Manifest, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.tar.gz")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	attachArtifact(m, data, path)
}

func TestAppArtifactDownloadCacheAndCorruption(t *testing.T) {
	m, data := artifactFixture(t, []byte("executable"), nil)
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write(data) }))
	defer remote.Close()
	attachArtifact(m, data, remote.URL)
	sup := NewLocalSupervisor(t.TempDir())
	t.Setenv("PATH", t.TempDir()) // neither git nor a compiler exists at install time
	start := time.Now()
	bin, err := sup.BuildFromSourceBinary(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.Close()
	cached, err := sup.BuildFromSourceBinary(m, nil)
	if err != nil || cached != bin || calls != 1 {
		t.Fatalf("offline reuse=%s calls=%d err=%v", cached, calls, err)
	}
	t.Logf("cold + offline reuse: %s", time.Since(start))
	if err := os.WriteFile(filepath.Join(artifactAppRoot(m, bin), "ui/panel.mjs"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.BuildFromSourceBinary(m, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("cache corruption accepted: %v", err)
	}
}

func TestAppArtifactFailuresNeverBuildSource(t *testing.T) {
	for _, kind := range []string{"checksum", "http", "traversal", "manifest", "missing-ui", "symlink", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			extra := map[string][]byte{}
			switch kind {
			case "traversal":
				extra["../../escape"] = []byte("bad")
			case "manifest":
				extra["src/mcp/artifact-test/apteva.yaml"] = []byte(strings.Replace(artifactFixtureManifest, "1.2.3", "9.9.9", 1))
			case "missing-ui":
				extra["src/mcp/artifact-test/apteva.yaml"] = []byte(artifactFixtureManifest + "icon: /ui/missing.svg\n")
			}
			m, data := artifactFixture(t, []byte("bin"), extra)
			if kind == "missing-ui" {
				m.Icon = "/ui/missing.svg"
			}
			if kind == "symlink" || kind == "duplicate" {
				var out bytes.Buffer
				gz := gzip.NewWriter(&out)
				tw := tar.NewWriter(gz)
				h := &tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "/tmp/escape"}
				if kind == "duplicate" {
					h = &tar.Header{Name: "bin", Typeflag: tar.TypeReg, Size: 0}
					tw.WriteHeader(h)
				}
				tw.WriteHeader(h)
				tw.Close()
				gz.Close()
				data = out.Bytes()
			}
			localArtifact(t, m, data)
			if kind == "checksum" {
				a := m.Runtime.Artifacts[localPlatform()]
				a.SHA256 = strings.Repeat("0", 64)
				m.Runtime.Artifacts[localPlatform()] = a
			}
			if kind == "http" {
				remote := httptest.NewServer(http.NotFoundHandler())
				defer remote.Close()
				attachArtifact(m, data, remote.URL)
			}
			sup := NewLocalSupervisor(t.TempDir())
			t.Setenv("PATH", t.TempDir())
			var progress []string
			if _, err := sup.BuildFromSourceBinary(m, func(s string) { progress = append(progress, s) }); err == nil {
				t.Fatal("invalid artifact accepted")
			}
			if strings.Contains(strings.Join(progress, " "), "Cloning") {
				t.Fatal("silently fell back to source")
			}
			matches, _ := filepath.Glob(filepath.Join(sup.cacheDir, m.Name, m.Version, "artifacts", "*"))
			if len(matches) != 0 {
				t.Fatalf("failed artifact committed: %v", matches)
			}
		})
	}
}

func TestAppArtifactMissingPlatformUsesSource(t *testing.T) {
	m, _ := artifactFixture(t, []byte("bin"), nil)
	m.Runtime.Artifacts = map[string]sdk.BundleSpec{"other-platform": {URL: "unusable", SHA256: strings.Repeat("0", 64)}}
	// Use a missing local repository so the existing source path fails immediately.
	m.Runtime.Source.Repo = filepath.Join(t.TempDir(), "missing.git")
	t.Setenv("PATH", t.TempDir())
	sup := NewLocalSupervisor(t.TempDir())
	var messages []string
	_, err := sup.BuildFromSourceBinary(m, func(s string) { messages = append(messages, s) })
	if err == nil || !strings.Contains(strings.Join(messages, " "), "Cloning") {
		t.Fatalf("no source fallback: %v %v", messages, err)
	}
}

func TestAppArtifactUpgradeAndRestartPreserveData(t *testing.T) {
	fixture := buildLocalSidecarFixture(t)
	binData, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	m, data := artifactFixture(t, binData, nil)
	localArtifact(t, m, data)
	sup := NewLocalSupervisor(t.TempDir())
	defer sup.StopAll(time.Second)
	old := filepath.Join(sup.cacheDir, m.Name, "1.2.2", "bin")
	os.MkdirAll(filepath.Dir(old), 0755)
	os.WriteFile(old, binData, 0755)
	env := map[string]string{"APTEVA_APP_CONFIG": `{"preserve":"yes"}`, "APTEVA_INSTALL_ID": "42", "APTEVA_PROJECT_ID": "my-project"}
	oldPort, err := sup.startBuiltSource(42, m, old, env, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(sup.cacheDir, m.Name, "data", "42")
	sentinel := filepath.Join(root, "attachment.txt")
	os.WriteFile(sentinel, []byte("keep this"), 0644)
	oldPID := sup.PID(42)
	t.Setenv("PATH", t.TempDir())
	// A bad release must leave the running source process alone.
	a := m.Runtime.Artifacts[localPlatform()]
	broken := *m
	broken.Runtime.Artifacts = map[string]sdk.BundleSpec{localPlatform(): {URL: a.URL, SHA256: strings.Repeat("0", 64)}}
	if _, _, err := sup.BuildFromSource(42, &broken, env, nil); err == nil {
		t.Fatal("bad release accepted")
	}
	if sup.PID(42) != oldPID || !sup.verifyCurrentProc(42, time.Second) {
		t.Fatal("failed download stopped previous runtime")
	}
	port, bin, err := sup.BuildFromSource(42, m, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	sup.RetireOld(42, time.Second)
	if port == oldPort || sup.PID(42) == oldPID {
		t.Fatal("new runtime was not activated")
	}
	assertProcess := func() {
		t.Helper()
		sup.mu.Lock()
		proc := sup.procs[42]
		childEnv := append([]string{}, proc.cmd.Env...)
		sup.mu.Unlock()
		assertEnvValue(t, childEnv, "APTEVA_DATA_DIR", root)
		assertEnvValue(t, childEnv, "DB_PATH", filepath.Join(root, "app.db"))
		assertEnvValue(t, childEnv, "APTEVA_APP_CONFIG", `{"preserve":"yes"}`)
		assertEnvValue(t, childEnv, "APTEVA_PROJECT_ID", "my-project")
		assertEnvValue(t, childEnv, "APTEVA_UI_DIR", filepath.Join(artifactAppRoot(m, bin), "ui"))
		got, _ := os.ReadFile(sentinel)
		if string(got) != "keep this" {
			t.Fatal("attachment changed")
		}
	}
	assertProcess()
	if err := sup.Stop(42); err != nil {
		t.Fatal(err)
	}
	if err := sup.Restart(42, m, port, bin, env); err != nil {
		t.Fatal(err)
	}
	assertProcess()
	relocated := filepath.Join("/old-host", m.Name, m.Version, "artifacts", a.SHA256, "bin")
	if got, ok := portableLocalBinPath(relocated, sup.cacheDir, m.Name, m.Version); !ok || got != bin {
		t.Fatalf("rebase failed: %s", got)
	}
	raw, _ := json.Marshal(m)
	if _, err := exactSourceRuntimeManifest(m.Name, m.Version, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// Optional real release smoke: no source tools in the child/install PATH.
// APTEVA_TEST_ARTIFACT_MANIFEST=/path/to/release/apteva.yaml go test -run TestConversationsArtifactSmoke -v
func TestConversationsArtifactSmoke(t *testing.T) {
	path := os.Getenv("APTEVA_TEST_ARTIFACT_MANIFEST")
	if path == "" {
		t.Skip("set APTEVA_TEST_ARTIFACT_MANIFEST")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	sup := NewLocalSupervisor(t.TempDir())
	defer sup.StopAll(time.Second)
	t.Setenv("PATH", t.TempDir())
	env := map[string]string{"APTEVA_APP_TOKEN": "artifact-smoke-token", "APTEVA_INSTALL_ID": "7", "APTEVA_APP_CONFIG": "{}"}
	start := time.Now()
	port, bin, err := sup.BuildFromSource(7, m, env, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Conversations %s cold artifact ready in %s", m.Version, time.Since(start))
	for _, path := range []string{"/health", "/ui/ConversationsPanel.mjs"} {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		req.Header.Set("Authorization", "Bearer artifact-smoke-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	if _, err := os.Stat(filepath.Join(sup.cacheDir, m.Name, "data", "7", "app.db")); err != nil {
		t.Fatal(err)
	}
	if err := verifyAppArtifact(filepath.Dir(bin), m.Runtime.Artifacts[localPlatform()].SHA256, m); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(sup.cacheDir, m.Name, "data", "7", "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil || tables < 10 {
		t.Fatalf("missing migrations: %d %v", tables, err)
	}
	if _, err := db.Exec("CREATE TABLE artifact_preservation_test (value TEXT); INSERT INTO artifact_preservation_test VALUES ('keep me')"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer artifact-smoke-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"tools"`)) {
		t.Fatalf("MCP tools: %d %s", resp.StatusCode, body)
	}
	if err := sup.Stop(7); err != nil {
		t.Fatal(err)
	}
	if err := sup.Restart(7, m, port, bin, env); err != nil {
		t.Fatal(err)
	}
	var preserved string
	if err := db.QueryRow("SELECT value FROM artifact_preservation_test").Scan(&preserved); err != nil || preserved != "keep me" {
		t.Fatalf("DB lost on restart: %v", err)
	}
	t.Logf("verified %d database tables, MCP tools, UI, and data after restart", tables)
	start = time.Now()
	if _, err := sup.BuildFromSourceBinary(m, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified cache reuse %s", time.Since(start))
}

func TestAppArtifactCloneIgnoresStaleSourceBinary(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	m, data := artifactFixture(t, []byte("prebuilt"), nil)
	localArtifact(t, m, data)
	id := seedRunningInstall(t, s, m.Name, "", *m, nil)
	old := filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "bin")
	os.MkdirAll(filepath.Dir(old), 0755)
	os.WriteFile(old, []byte("stale source"), 0755)
	s.store.db.Exec("UPDATE app_installs SET local_bin_path=? WHERE id=?", old, id)
	t.Setenv("PATH", t.TempDir())
	if err := s.PrepareCloneLocalRuntimes(); err != nil {
		t.Fatal(err)
	}
	var got string
	s.store.db.QueryRow("SELECT local_bin_path FROM app_installs WHERE id=?", id).Scan(&got)
	if !strings.Contains(got, "/artifacts/") {
		t.Fatalf("selected stale source: %s", got)
	}
	if s.localApps.PID(id) != 0 {
		t.Fatal("quarantine spawned app")
	}
}

func TestConversationsArtifactInstallThroughServer(t *testing.T) {
	path := os.Getenv("APTEVA_TEST_ARTIFACT_MANIFEST")
	if path == "" {
		t.Skip("set APTEVA_TEST_ARTIFACT_MANIFEST")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	artifact := m.Runtime.Artifacts[localPlatform()]
	packagePath := strings.TrimPrefix(artifact.URL, "file://")
	// Real HTTP artifact delivery, independent of source-host availability.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, packagePath) }))
	defer remote.Close()
	artifact.URL = remote.URL + "/package.tar.gz"
	m.Runtime.Artifacts[localPlatform()] = artifact
	s := newTestServer(t)
	admin := createTestAdmin(t, s)
	s.localApps = NewLocalSupervisor(t.TempDir())
	defer s.localApps.StopAll(time.Second)
	s.installedApps = NewInstalledAppsRegistry()
	s.staticMounts = newStaticAppMounts()
	t.Setenv("PATH", t.TempDir())
	started := time.Now()
	id, err := s.installAppFromManifest(admin.ID, m, "")
	if err != nil {
		t.Fatal(err)
	}
	var status, bin, config, bindings string
	if err := s.store.db.QueryRow("SELECT status,local_bin_path,COALESCE(config_encrypted,''),COALESCE(integration_bindings,'') FROM app_installs WHERE id=?", id).Scan(&status, &bin, &config, &bindings); err != nil {
		t.Fatal(err)
	}
	if status != "running" || !strings.Contains(bin, "/artifacts/") {
		t.Fatalf("not prebuilt: %s %s", status, bin)
	}
	remote.Close()
	if err := s.ensureInterfaceConversations(admin.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.store.db.QueryRow("SELECT COUNT(*) FROM mcp_servers WHERE upstream_id=?", appMCPUpstreamID(id)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing MCP bridge: %d %v", count, err)
	}
	// Re-enter the same install path as an upgrade/retry and verify metadata.
	if err := s.installFromSource(id, m, "", nil); err != nil {
		t.Fatal(err)
	}
	var configAfter, bindingsAfter string
	s.store.db.QueryRow("SELECT COALESCE(config_encrypted,''),COALESCE(integration_bindings,'') FROM app_installs WHERE id=?", id).Scan(&configAfter, &bindingsAfter)
	if configAfter != config || bindingsAfter != bindings {
		t.Fatal("install metadata changed")
	}
	t.Logf("HTTP install, onboarding reuse, MCP bridge and offline upgrade completed in %s", time.Since(started))
}
