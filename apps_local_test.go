package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestPortableLocalBinPath(t *testing.T) {
	cache := t.TempDir()
	current := filepath.Join(cache, "deploy", "1.2.3", "bin")
	if err := os.MkdirAll(filepath.Dir(current), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if got, ok := portableLocalBinPath(current, cache, "deploy", "1.2.3"); !ok || got != current {
		t.Fatalf("existing path changed: got=%q ok=%v", got, ok)
	}
	old := filepath.Join(t.TempDir(), "deploy", "1.2.3", "bin")
	if got, ok := portableLocalBinPath(old, cache, "deploy", "1.2.3"); !ok || got != current {
		t.Fatalf("portable path not rebased: got=%q ok=%v", got, ok)
	}
	if got, ok := portableLocalBinPath(filepath.Join(t.TempDir(), "unrelated"), cache, "deploy", "1.2.3"); ok || got != "" {
		t.Fatalf("arbitrary missing path was rebased: got=%q ok=%v", got, ok)
	}
	if _, ok := portableLocalBinPath(old, cache, "../deploy", "1.2.3"); ok {
		t.Fatal("unsafe app name was accepted")
	}
}

func TestPrepareCloneLocalRuntimesRebasesWithoutStarting(t *testing.T) {
	s := newTestServer(t)
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	m := sdk.Manifest{Name: "deploy", Version: "1.2.3", Runtime: sdk.Runtime{Kind: "source", Source: &sdk.SourceSpec{Repo: "example.invalid/repo", Ref: "v1.2.3"}}}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	current := filepath.Join(cache, m.Name, m.Version, "bin")
	if err := os.MkdirAll(filepath.Dir(current), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(t.TempDir(), m.Name, m.Version, "bin")
	if err := os.MkdirAll(filepath.Dir(old), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("source binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET local_bin_path=?, local_pid=4242, local_port=7000 WHERE id=?`, old, id); err != nil {
		t.Fatal(err)
	}

	if err := s.PrepareCloneLocalRuntimes(); err != nil {
		t.Fatal(err)
	}
	var path, status string
	var pid, port int64
	if err := s.store.db.QueryRow(`SELECT local_bin_path, status, local_pid, local_port FROM app_installs WHERE id=?`, id).Scan(&path, &status, &pid, &port); err != nil {
		t.Fatal(err)
	}
	if path != current || status != "running" || pid != 4242 || port != 7000 {
		t.Fatalf("unexpected quarantine mutation: path=%q status=%q pid=%d port=%d", path, status, pid, port)
	}
	if s.localApps.PID(id) != 0 {
		t.Fatal("quarantine started the sidecar")
	}
}

func TestCloneQuarantineEnabledIsOptIn(t *testing.T) {
	t.Setenv("APTEVA_CLONE_QUARANTINE", "")
	if cloneQuarantineEnabled() {
		t.Fatal("quarantine enabled by default")
	}
	t.Setenv("APTEVA_CLONE_QUARANTINE", "1")
	if !cloneQuarantineEnabled() {
		t.Fatal("quarantine not enabled for 1")
	}
}

func TestExactSourceRuntimeManifestPinsInstalledVersion(t *testing.T) {
	raw := `{"name":"deploy","version":"9.9.9","runtime":{"kind":"source","source":{"repo":"github.com/apteva/apps","ref":"deploy/v1.2.3"}}}`
	m, err := exactSourceRuntimeManifest("deploy", "1.2.3", raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.3" || m.Runtime.Source.Ref != "deploy/v1.2.3" {
		t.Fatalf("runtime not pinned: version=%q ref=%q", m.Version, m.Runtime.Source.Ref)
	}
	mutable := strings.Replace(raw, "deploy/v1.2.3", "main", 1)
	if _, err := exactSourceRuntimeManifest("deploy", "1.2.3", mutable); err == nil {
		t.Fatal("mutable source ref accepted for exact reconstruction")
	}
}

func TestChildEnvWithOverridesReplacesInheritedProxyEnv(t *testing.T) {
	base := []string{
		"KEEP=1",
		"HTTP_PROXY=http://wrong-upper",
		"http_proxy=http://wrong-lower",
		"HTTPS_PROXY=http://wrong-upper",
		"https_proxy=http://wrong-lower",
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=localhost",
		"DB_PATH=/tmp/server.db",
	}
	got := childEnvWithOverrides(base, map[string]string{
		"HTTP_PROXY":      "http://127.0.0.1:61000",
		"HTTPS_PROXY":     "http://127.0.0.1:61000",
		"NO_PROXY":        "",
		"DB_PATH":         "/tmp/app.db",
		"APTEVA_DATA_DIR": "/tmp/app-data",
	})

	assertEnvValue(t, got, "KEEP", "1")
	assertEnvValue(t, got, "HTTP_PROXY", "http://127.0.0.1:61000")
	assertEnvValue(t, got, "HTTPS_PROXY", "http://127.0.0.1:61000")
	assertEnvValue(t, got, "NO_PROXY", "")
	assertEnvValue(t, got, "DB_PATH", "/tmp/app.db")
	assertEnvValue(t, got, "APTEVA_DATA_DIR", "/tmp/app-data")
	assertEnvMissing(t, got, "http_proxy")
	assertEnvMissing(t, got, "https_proxy")
	assertEnvMissing(t, got, "no_proxy")
}

func TestLocalSupervisorStopAllKillsRunningAndPendingSidecars(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	running := startStubbornLocalProc(t)
	pending := startStubbornLocalProc(t)

	sup.mu.Lock()
	sup.procs[1] = running
	sup.mu.Unlock()
	sup.pendingMu.Lock()
	sup.pending[1] = pending
	sup.pendingMu.Unlock()

	sup.StopAll(100 * time.Millisecond)

	waitLocalProcDone(t, running)
	waitLocalProcDone(t, pending)

	sup.mu.Lock()
	procCount := len(sup.procs)
	sup.mu.Unlock()
	sup.pendingMu.Lock()
	pendingCount := len(sup.pending)
	sup.pendingMu.Unlock()
	if procCount != 0 || pendingCount != 0 {
		t.Fatalf("StopAll left tracked sidecars: procs=%d pending=%d", procCount, pendingCount)
	}
}

func TestLocalResumeDependencyWavesStartBoundAppsBeforeCallers(t *testing.T) {
	fleetManifest := sdk.Manifest{
		Name: "fleet",
		Requires: sdk.Requires{Integrations: []sdk.IntegrationDep{{
			Role: "host_provider", Kind: "app", CompatibleAppNames: []string{"instances"},
		}}},
	}
	fleetJSON, err := json.Marshal(fleetManifest)
	if err != nil {
		t.Fatal(err)
	}
	deps := localResumeDependencyIDs(string(fleetJSON), `{"host_provider":40}`)
	if len(deps) != 1 || deps[0] != 40 {
		t.Fatalf("dependency IDs=%v, want [40]", deps)
	}

	waves := localResumeWaves([]localResumeRow{
		{id: 29, appName: "fleet", dependencyIDs: deps},
		{id: 40, appName: "instances"},
		{id: 33, appName: "routes"},
	})
	if len(waves) != 2 {
		t.Fatalf("waves=%v, want 2 waves", resumeWaveIDs(waves))
	}
	if got := resumeWaveIDs(waves); !equalInt64Slices(got[0], []int64{33, 40}) || !equalInt64Slices(got[1], []int64{29}) {
		t.Fatalf("waves=%v, want [[33 40] [29]]", got)
	}
}

func TestLocalResumeDependencyWavesDoNotDeadlockCycles(t *testing.T) {
	waves := localResumeWaves([]localResumeRow{
		{id: 1, dependencyIDs: []int64{2}},
		{id: 2, dependencyIDs: []int64{1}},
	})
	got := resumeWaveIDs(waves)
	if len(got) != 1 || !equalInt64Slices(got[0], []int64{1, 2}) {
		t.Fatalf("cycle waves=%v, want [[1 2]]", got)
	}
}

func TestUpdateLocalInstallRuntimeReplacesStaleSidecarOverride(t *testing.T) {
	s := newTestServer(t)
	installID := seedLocalInstallWithStaleOverride(t, s)

	s.updateLocalInstallRuntime(installID, 1234, 60027)

	var pid int
	var sidecarURL string
	if err := s.store.db.QueryRow(
		`SELECT local_pid, sidecar_url_override FROM app_installs WHERE id=?`,
		installID,
	).Scan(&pid, &sidecarURL); err != nil {
		t.Fatalf("query install: %v", err)
	}
	if pid != 1234 {
		t.Fatalf("local_pid=%d, want 1234", pid)
	}
	if sidecarURL != "http://127.0.0.1:60027" {
		t.Fatalf("sidecar_url_override=%q, want managed local port", sidecarURL)
	}
}

func TestSetInstallStatusUsesManagedLocalPort(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	installID := seedLocalInstallWithStaleOverride(t, s)

	req := httptest.NewRequest(
		http.MethodPut,
		"/apps/installs/"+strconv.FormatInt(installID, 10)+"/status",
		bytes.NewBufferString(`{"status":"running","sidecar_url":"http://127.0.0.1:8080"}`),
	)
	rec := httptest.NewRecorder()

	s.handleSetInstallStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sidecarURL string
	if err := s.store.db.QueryRow(
		`SELECT sidecar_url_override FROM app_installs WHERE id=?`,
		installID,
	).Scan(&sidecarURL); err != nil {
		t.Fatalf("query install: %v", err)
	}
	if sidecarURL != "http://127.0.0.1:60027" {
		t.Fatalf("sidecar_url_override=%q, want managed local port", sidecarURL)
	}
}

func TestSetInstallStatusRefreshesAppMCPRegistration(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	installID := seedAppWithTools(t, s, "code", "proj-1", []string{"repos_list"})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}

	setStatus := func(status string) {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPut,
			"/apps/installs/"+strconv.FormatInt(installID, 10)+"/status",
			bytes.NewBufferString(`{"status":"`+status+`","sidecar_url":"http://127.0.0.1:8080"}`),
		)
		rec := httptest.NewRecorder()
		s.handleSetInstallStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("set status %s: status=%d body=%s", status, rec.Code, rec.Body.String())
		}
	}

	setStatus("running")
	if row := readMCPRow(t, s, installID); row == nil {
		t.Fatal("running install did not register its agent-visible MCP tools")
	}

	setStatus("disabled")
	if row := readMCPRow(t, s, installID); row != nil {
		t.Fatalf("disabled install retained its MCP registration: %+v", row)
	}
}

func seedLocalInstallWithStaleOverride(t *testing.T, s *Server) int64 {
	t.Helper()
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('media-studio', 'local', '', '', '{}')`,
	)
	if err != nil {
		t.Fatalf("insert app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs
			(app_id, status, local_pid, local_bin_path, local_port, sidecar_url_override)
		 VALUES (?, 'running', 99, '/tmp/media-studio', 60027, 'http://127.0.0.1:8080')`,
		appID,
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	return installID
}

func assertEnvValue(t *testing.T, env []string, key, want string) {
	t.Helper()
	found := 0
	var value string
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			found++
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	if found != 1 {
		t.Fatalf("%s appears %d times in env, want exactly 1: %+v", key, found, env)
	}
	if value != want {
		t.Fatalf("%s=%q, want %q", key, value, want)
	}
}

func assertEnvMissing(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			t.Fatalf("%s should be absent from env: %+v", key, env)
		}
	}
}

func startStubbornLocalProc(t *testing.T) *localProc {
	t.Helper()
	cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 60; done")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stubborn proc: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	p := &localProc{cmd: cmd, done: done}
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			signalLocalProc(cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})
	return p
}

func waitLocalProcDone(t *testing.T, p *localProc) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("process pid=%d did not exit", p.cmd.Process.Pid)
	}
}

func resumeWaveIDs(waves [][]localResumeRow) [][]int64 {
	out := make([][]int64, 0, len(waves))
	for _, wave := range waves {
		ids := make([]int64, 0, len(wave))
		for _, row := range wave {
			ids = append(ids, row.id)
		}
		out = append(out, ids)
	}
	return out
}

func equalInt64Slices(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
