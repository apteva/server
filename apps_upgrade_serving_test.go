package main

import (
	"context"
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func drainFixtureSpec(t *testing.T, raw string) activationSpec {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return activationSpec{appName: "functions", httpPort: n, probeHost: host, healthPath: "/health", env: map[string]string{"APTEVA_APP_TOKEN": "drain-test", "APTEVA_APP_DRAIN_TOKEN": "control-test"}}
}
func TestFunctionsUpgradeKeepsBindingAndOldRoute(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "old-serving") }))
	defer old.Close()
	manifest := sdk.Manifest{Name: "functions", Version: "1.11.1", Runtime: sdk.Runtime{Kind: "source"}}
	id := seedRunningInstall(t, s, "functions", "", manifest, nil)
	s.store.db.Exec("UPDATE app_installs SET sidecar_url_override=? WHERE id=?", old.URL, id)
	caller := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs", Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "functions"}}}}, map[string]any{"functions": id})
	spec := drainFixtureSpec(t, old.URL)
	spec.installID = id
	s.localApps.procs[id] = &localProc{port: spec.httpPort, spec: spec, done: make(chan struct{})}
	s.LoadInstalledApps()
	next := manifest
	next.Version = "next"
	if err := s.stageAppUpgrade(id, &next); err != nil {
		t.Fatal(err)
	}
	var status, version, pending string
	if err := s.store.db.QueryRow("SELECT status,version,pending_manifest_json FROM app_installs WHERE id=?", id).Scan(&status, &version, &pending); err != nil {
		t.Fatal(err)
	}
	if status != "running" || version != "1.11.1" || pending == "" {
		t.Fatalf("lost serving state: %s %s %s", status, version, pending)
	}
	for i := 0; i < 10; i++ {
		s.LoadInstalledApps()
		if got := installBoundAppID(s, caller, "functions"); got != id {
			t.Fatalf("binding unavailable during upgrade: %d", got)
		}
		target := s.installedApps.Get(id)
		if target == nil || target.SidecarURL != old.URL {
			t.Fatalf("old route lost: %+v", target)
		}
		resp, err := http.Get(target.SidecarURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "old-serving" {
			t.Fatal(string(body))
		}
	}
	// A failed replacement leaves the committed version available.
	s.markInstallRunningOnPreviousVersion(id, context.DeadlineExceeded)
	if installBoundAppID(s, caller, "functions") != id {
		t.Fatal("rollback lost binding")
	}
}
func TestUpgradeWithoutServingProcessRemainsPending(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	m := sdk.Manifest{Name: "functions", Version: "1.11.1"}
	id := seedRunningInstall(t, s, "functions", "", m, nil)
	if err := s.stageAppUpgrade(id, &m); err != nil {
		t.Fatal(err)
	}
	var status string
	s.store.db.QueryRow("SELECT status FROM app_installs WHERE id=?", id).Scan(&status)
	if status != "pending" {
		t.Fatal(status)
	}
}
func TestFunctionsDrainWaitsAndPreservesAuth(t *testing.T) {
	var active atomic.Int64
	active.Store(1)
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/runtime/drain" || r.Header.Get("Authorization") != "Bearer drain-test" || r.Header.Get("X-Apteva-Drain-Token") != "control-test" {
			t.Error("bad drain request")
			http.Error(w, "bad", 400)
			return
		}
		polls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"draining": true, "drained": active.Load() == 0, "active_work": active.Load()})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- drainFunctions(ctx, drainFixtureSpec(t, srv.URL), time.Millisecond) }()
	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("returned before drain: %v", err)
	default:
	}
	active.Store(0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if polls.Load() < 2 {
		t.Fatal("did not poll")
	}
}
func TestFunctionsDrainDeadlineAndLegacyFallback(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"draining":true,"drained":false,"active_work":1}`)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := drainFunctions(ctx, drainFixtureSpec(t, srv.URL), time.Millisecond); err == nil {
				t.Fatal("unbounded work reported drained")
			}
			if d := time.Since(start); d < 40*time.Millisecond || d > time.Second {
				t.Fatalf("incorrect bounded wait: %v", d)
			}
		})
	}
}
func TestFunctionsDrainCancellationClosesDownstream(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- drainFunctions(ctx, drainFixtureSpec(t, srv.URL), time.Millisecond) }()
	<-entered
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request leaked")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatal(err)
	}
}

func TestFunctionsUpgradeListSeparatesServingFromProgress(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.manifestRefreshInFlight.Store(true) // Do not fetch upstream manifests in this test.
	id := seedRunningInstall(t, s, "functions", "", sdk.Manifest{Name: "functions", Version: "1.11.1"}, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET pending_manifest_json='{"version":"next"}',status_message='Waiting for health check…' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleListApps(w, httptest.NewRequest("GET", "/api/apps", nil))
	var rows []AppRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	for _, row := range rows {
		if row.InstallID == id {
			if !row.Serving || !row.UpgradeInProgress || row.Status != "pending" || row.Version != "1.11.1" {
				t.Fatalf("progress hid active version: %+v", row)
			}
			return
		}
	}
	t.Fatal("install missing from list")
}

func TestFunctionsDrainControlTokenIsPerProcess(t *testing.T) {
	m := &sdk.Manifest{Name: "functions"}
	env := map[string]string{"APTEVA_APP_TOKEN": "ordinary-app-token"}
	a, err := newActivationSpec(1, m, "/tmp/app", 1234, env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newActivationSpec(1, m, "/tmp/app", 1235, env)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := newActivationSpec(2, m, "/tmp/app", 1236, nil)
	if err != nil || empty.env["APTEVA_APP_DRAIN_TOKEN"] == "" {
		t.Fatal("missing control credential with empty environment")
	}
	token := a.env["APTEVA_APP_DRAIN_TOKEN"]
	if token == "" || token == b.env["APTEVA_APP_DRAIN_TOKEN"] || env["APTEVA_APP_DRAIN_TOKEN"] != "" {
		t.Fatal("control credential not isolated per process")
	}
	if a.clone().env["APTEVA_APP_DRAIN_TOKEN"] != token {
		t.Fatal("rollback lost control credential")
	}
}
