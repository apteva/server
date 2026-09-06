package main

import (
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMigrationStartupDeadlineAndBinaryFallback(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(time.Second) })
	bin := buildLocalSidecarFixture(t)
	old := localSidecarSpec(t, 1, "tables", bin, 0, nil)
	if err := sup.activate(old, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	oldProc := sup.currentProc(1)
	next := localSidecarSpec(t, 1, "tables", bin, 0, map[string]string{"TEST_STARTUP_DELAY": "700ms"})
	next.startupTimeout = 2 * time.Second
	if err := sup.activate(next, 100*time.Millisecond); err != nil {
		t.Fatal("manifest budget ignored", err)
	}
	if !processAlive(oldProc.cmd.Process.Pid) {
		t.Fatal("old process retired before handoff")
	}
	sup.RetireOld(1, time.Second)
	active := sup.currentProc(1)
	failed := localSidecarSpec(t, 1, "tables", bin, 0, map[string]string{"TEST_STARTUP_DELAY": "10s"})
	failed.startupTimeout = 400 * time.Millisecond
	start := time.Now()
	err := sup.activate(failed, 3*time.Second)
	if err == nil || !activationRollbackVerified(err) {
		t.Fatalf("fallback not verified: %v", err)
	}
	if !strings.Contains(err.Error(), "does not restore committed database changes") {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("initialization progress extended the absolute deadline")
	}
	if sup.currentProc(1) != active || !processAlive(active.cmd.Process.Pid) {
		t.Fatal("old process not retained")
	}
	failed.databaseUpgrade = "requires_restore"
	if err := sup.activate(failed, time.Second); err == nil {
		t.Fatal("unsafe upgrade allowed")
	}
	if sup.currentProc(1) != active {
		t.Fatal("blocked activation disturbed old process")
	}
}
func TestInitializingNeverReadyAndFailedExitsEarly(t *testing.T) {
	for _, state := range []string{"initializing", "failed"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(503)
				fmt.Fprintf(w, `{"status":%q,"completed":1,"total":55}`, state)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			port, _ := strconv.Atoi(u.Port())
			start := time.Now()
			err := NewLocalSupervisor(t.TempDir()).waitHealthy(1, port, "/health", 300*time.Millisecond)
			if err == nil {
				t.Fatal("unready app accepted")
			}
			if state == "failed" && time.Since(start) > 250*time.Millisecond {
				t.Fatal("failed startup waited for deadline")
			}
		})
	}
	for _, seconds := range []int{-1, 3601} {
		if _, err := newActivationSpec(1, &sdk.Manifest{Runtime: sdk.Runtime{StartupTimeoutSeconds: seconds}}, "", 1, nil); err == nil {
			t.Fatal("unbounded manifest accepted")
		}
	}
}

func TestStartupManifestAndEarlyExit(t *testing.T) {
	spec, err := newActivationSpec(9, &sdk.Manifest{Runtime: sdk.Runtime{StartupTimeoutSeconds: 600, DatabaseUpgrade: "backward_compatible"}}, "", 43210, nil)
	if err != nil {
		t.Fatal(err)
	}
	copied := spec.clone()
	if copied.startupTimeout != 10*time.Minute || copied.databaseUpgrade != "backward_compatible" {
		t.Fatal("startup contract lost from activation spec", copied)
	}
	sup := NewLocalSupervisor(t.TempDir())
	done := make(chan struct{})
	close(done)
	sup.procs[9] = &localProc{port: 43210, done: done}
	start := time.Now()
	err = sup.waitHealthy(9, 43210, "/health", 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("dead process consumed startup budget")
	}
}
