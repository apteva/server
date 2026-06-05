package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
