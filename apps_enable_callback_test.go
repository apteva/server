package main

import (
	"bytes"
	"fmt"
	"net"
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

// This sidecar probes telemetry once, before exposing its health endpoint,
// just like SubscribeTelemetry during Conversations OnMount.
func TestEnableLocalAppCallbackHelper(t *testing.T) {
	if os.Getenv("APTEVA_ENABLE_CALLBACK_HELPER") != "1" {
		return
	}
	req, err := http.NewRequest(http.MethodGet, os.Getenv("APTEVA_GATEWAY_URL")+"/api/apps/callback/telemetry?events=tool.call&thread_prefix=chat-", nil)
	if err != nil {
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("APTEVA_APP_TOKEN"))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		os.Exit(2)
	}
	resp.Body.Close()
	if err := os.WriteFile(os.Getenv("APTEVA_ENABLE_CALLBACK_RESULT"), []byte(strconv.Itoa(resp.StatusCode)), 0600); err != nil {
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("APTEVA_APP_PORT"), mux); err != nil {
		os.Exit(4)
	}
}

func TestEnableLocalAppAuthenticatesTelemetryBeforeHealth(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := sdk.Manifest{Name: "enable-callback", Version: "1.0.0", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermTelemetryRead}}}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	api := httptest.NewServer(http.StripPrefix("/api", http.HandlerFunc(s.authMiddleware(s.handleAppCallback))))
	defer api.Close()
	s.port = strings.TrimPrefix(api.URL, "http://127.0.0.1:")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sidecar")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quotedExe := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec "+quotedExe+" -test.run='^TestEnableLocalAppCallbackHelper$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, "callback-status")
	t.Setenv("APTEVA_ENABLE_CALLBACK_HELPER", "1")
	t.Setenv("APTEVA_ENABLE_CALLBACK_RESULT", result)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='disabled',local_bin_path=?,local_port=? WHERE id=?`, bin, port, id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/apps/installs/%d/status", id), bytes.NewBufferString(`{"status":"running"}`))
	rec := httptest.NewRecorder()
	s.handleSetInstallStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	status, err := os.ReadFile(result)
	if err != nil || string(status) != "200" {
		t.Fatalf("startup telemetry callback: status=%s err=%v, want 200 before health", status, err)
	}
	var persisted string
	s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&persisted)
	if persisted != "running" {
		t.Fatalf("healthy install status=%q", persisted)
	}
}

func TestEnableLocalAppFailureRestoresDisabledAndRejectsCallbacks(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	id := seedRunningInstall(t, s, "enable-failure", "", sdk.Manifest{Name: "enable-failure", Version: "1.0.0"}, nil)
	token, err := s.appInstallToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='disabled',local_bin_path=?,local_port=60027 WHERE id=?`, filepath.Join(t.TempDir(), "missing"), id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/apps/installs/%d/status", id), bytes.NewBufferString(`{"status":"running"}`))
	rec := httptest.NewRecorder()
	s.handleSetInstallStatus(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed enable: %d %s", rec.Code, rec.Body.String())
	}
	var status, message string
	s.store.db.QueryRow(`SELECT status,status_message FROM app_installs WHERE id=?`, id).Scan(&status, &message)
	if status != "disabled" || message != "" {
		t.Fatalf("failed enable left status=%q message=%q", status, message)
	}
	callback := httptest.NewRequest(http.MethodGet, "/apps/callback/whoami", nil)
	callback.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(response, callback)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled callback status=%d, want 401", response.Code)
	}
}
