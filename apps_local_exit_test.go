package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestLocalSidecarExitUsesPublishedRouteOwner(t *testing.T) {
	bin := buildLocalSidecarFixture(t)
	for _, scenario := range []struct {
		name      string
		published bool
	}{
		{"old owner exits while replacement warms", false},
		{"retiring child exits after replacement publication", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newTestServer(t)
			s.installedApps = NewInstalledAppsRegistry()
			sup := NewLocalSupervisor(t.TempDir())
			s.localApps = sup
			sup.onProcessExit = s.localProcessExited
			t.Cleanup(func() { sup.StopAll(time.Second) })

			const name = "sidecar-exit-fixture"
			id := seedRunningInstall(t, s, name, "", sdk.Manifest{Name: name, Version: "1"}, nil)
			publish := func(spec activationSpec) {
				t.Helper()
				if err := sup.persistReadyProcess(id, spec.httpPort, func(pid int) error {
					return s.updateLocalInstallRuntime(id, pid, int64(spec.httpPort))
				}); err != nil {
					t.Fatal(err)
				}
				s.LoadInstalledApps()
			}
			old := localSidecarSpec(t, id, name, bin, 0, nil)
			if err := sup.activate(old, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			oldProc := sup.currentProc(id)
			publish(old)

			next := localSidecarSpec(t, id, name, bin, 0, nil)
			if err := sup.spawn(next); err != nil {
				t.Fatal(err)
			}
			publishNext := func() {
				t.Helper()
				if err := sup.waitReadyToPublish(next, 5*time.Second); err != nil {
					t.Fatal(err)
				}
				publish(next)
			}
			if scenario.published {
				publishNext()
			} else {
				if err := sup.persistReadyProcess(id, next.httpPort, func(int) error {
					t.Fatal("unchecked candidate reached publication")
					return nil
				}); err == nil {
					t.Fatal("unchecked candidate was accepted")
				}
			}

			stopSidecarFixture(t, oldProc)
			if !scenario.published {
				assertLocalSidecarState(t, s, id, "error", 0, "")
				s.LoadInstalledApps()
				if s.installedApps.GetByName(name) != nil {
					t.Fatal("registry reload restored dead route")
				}
				publishNext()
			}
			nextProc := sup.currentProc(id)
			nextURL := localSidecarURL(int64(next.httpPort))
			assertLocalSidecarState(t, s, id, "running", nextProc.cmd.Process.Pid, nextURL)
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(s.installedApps.Get(id).SidecarURL + "/health")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("replacement health HTTP %d", resp.StatusCode)
			}
			stopSidecarFixture(t, nextProc)
			assertLocalSidecarState(t, s, id, "error", 0, "")
			if !scenario.published {
				// Recover through the ordinary status API. Merely flipping
				// SQLite back to running would publish the dead endpoint.
				if _, err := s.store.db.Exec("UPDATE app_installs SET local_bin_path=? WHERE id=?", bin, id); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPut, "/apps/installs/"+strconv.FormatInt(id, 10)+"/status", strings.NewReader(`{"status":"running"}`))
				response := httptest.NewRecorder()
				s.handleSetInstallStatus(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("restart after exit: HTTP %d %s", response.Code, response.Body.String())
				}
				newPID := sup.PID(id)
				if newPID == 0 || newPID == nextProc.cmd.Process.Pid {
					t.Fatal("status API did not start a replacement process")
				}
				assertLocalSidecarState(t, s, id, "running", newPID, nextURL)
			}
		})
	}
}

func stopSidecarFixture(t *testing.T, proc *localProc) {
	t.Helper()
	if err := proc.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar was not reaped")
	}
}

func assertLocalSidecarState(t *testing.T, s *Server, id int64, wantStatus string, wantPID int, wantURL string) {
	t.Helper()
	var status, url string
	var pid int
	if err := s.store.db.QueryRow("SELECT status, local_pid, sidecar_url_override FROM app_installs WHERE id=?", id).Scan(&status, &pid, &url); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || pid != wantPID || url != wantURL {
		t.Fatalf("persisted state = %q, %d, %q; want %q, %d, %q", status, pid, url, wantStatus, wantPID, wantURL)
	}
	app := s.installedApps.Get(id)
	if wantURL == "" {
		if app != nil {
			t.Fatal("dead sidecar still in the routing registry")
		}
	} else if app == nil || app.localPID != wantPID || app.SidecarURL != wantURL {
		t.Fatal("published route does not match the ready replacement")
	}
}

func TestLocalSidecarExitSurvivesUnavailableSQLite(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	const name = "exit-persistence-fixture"
	id := seedRunningInstall(t, s, name, "", sdk.Manifest{Name: name, Version: "1"}, nil)
	const pid, port = 1234, 5678
	if err := s.updateLocalInstallRuntime(id, pid, port); err != nil {
		t.Fatal(err)
	}
	s.LoadInstalledApps()
	if _, err := s.store.db.Exec(`CREATE TRIGGER reject_exit BEFORE UPDATE OF local_pid ON app_installs
		WHEN NEW.local_pid=0 BEGIN SELECT RAISE(FAIL, 'injected disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	s.localProcessExited(id, pid, port, nil)
	s.LoadInstalledApps()
	if s.installedApps.Get(id) != nil {
		t.Fatal("stale SQLite row restored the dead route")
	}
	if _, err := s.store.db.Exec("DROP TRIGGER reject_exit"); err != nil {
		t.Fatal(err)
	}
	s.LoadInstalledApps()
	assertLocalSidecarState(t, s, id, "error", 0, "")
}
