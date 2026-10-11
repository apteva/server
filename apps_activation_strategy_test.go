package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestExclusiveConfiguredListenerAndPolicyTransitions(t *testing.T) {
	bin := buildLocalSidecarFixture(t)
	for _, tc := range []struct{ old, next string }{
		{"", sdk.ActivationExclusive},
		{sdk.ActivationAuto, sdk.ActivationExclusive},
		{sdk.ActivationExclusive, sdk.ActivationAuto},
		{sdk.ActivationExclusive, ""},
		{sdk.ActivationExclusive, sdk.ActivationExclusive},
	} {
		t.Run(tc.old+"_to_"+tc.next, func(t *testing.T) {
			sup := NewLocalSupervisor(t.TempDir())
			t.Cleanup(func() { sup.StopAll(time.Second) })
			events := filepath.Join(t.TempDir(), "events")
			port := unusedTCPPort(t)
			spec := func(version string) activationSpec {
				return localSidecarSpec(t, 71, "sip-service", copyFixtureBinary(t, bin, version), 0, map[string]string{
					"TEST_SIDECAR_ID": version, "TEST_SIDECAR_EVENTS": events, "TEST_SIDECAR_FIXED_PORT": fmt.Sprint(port),
				})
			}
			old := spec("old")
			old.activationStrategy = tc.old
			if err := sup.activate(old, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			previous := sup.currentProc(71)
			next := spec("new")
			next.activationStrategy = tc.next
			if len(next.fixedPorts) != 0 || !requiresExclusiveActivation(previous, next) {
				t.Fatal("configured listener must use the declared policy")
			}
			if err := sup.activate(next, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			waitLocalProcDone(t, previous)
			assertOrderedEvents(t, readEventLines(t, events), "start old", "stop old", "start new")
		})
	}
}

func TestExclusiveRollbackUsesPreviousStartupBudget(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { sup.StopAll(time.Second) })
	bin := buildLocalSidecarFixture(t)
	events := filepath.Join(t.TempDir(), "events")
	old := localSidecarSpec(t, 72, "sip-service", copyFixtureBinary(t, bin, "old"), 0, map[string]string{
		"TEST_SIDECAR_ID": "old", "TEST_SIDECAR_EVENTS": events, "TEST_STARTUP_DELAY": "600ms",
	})
	old.activationStrategy = sdk.ActivationExclusive
	old.startupTimeout = 3 * time.Second
	if err := sup.activate(old, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	next := localSidecarSpec(t, 72, "sip-service", copyFixtureBinary(t, bin, "new"), 0, map[string]string{
		"TEST_SIDECAR_ID": "new", "TEST_SIDECAR_EVENTS": events, "TEST_SIDECAR_FAIL_HEALTH": "1",
	})
	next.startupTimeout = 300 * time.Millisecond
	err := sup.activate(next, 3*time.Second)
	if err == nil || !activationRollbackVerified(err) {
		t.Fatalf("rollback: %v", err)
	}
	if got := sup.currentProc(72); got == nil || got.spec.binPath != old.binPath || got.spec.env["TEST_STARTUP_DELAY"] != "600ms" {
		t.Fatal("original specification was not restored")
	}
	assertOrderedEvents(t, readEventLines(t, events), "start old", "stop old", "start new", "stop new", "start old")
}

func TestExclusiveStartupAndRollbackFailures(t *testing.T) {
	bin := buildLocalSidecarFixture(t)
	for _, rollbackFails := range []bool{false, true} {
		t.Run(fmt.Sprint(rollbackFails), func(t *testing.T) {
			sup := NewLocalSupervisor(t.TempDir())
			t.Cleanup(func() { sup.StopAll(time.Second) })
			old := localSidecarSpec(t, 73, "sip-service", copyFixtureBinary(t, bin, "old"), 0, nil)
			old.activationStrategy = sdk.ActivationExclusive
			old.startupTimeout = 3 * time.Second
			if err := sup.activate(old, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			if rollbackFails {
				sup.currentProc(73).spec.binPath = filepath.Join(t.TempDir(), "missing-old")
			}
			next := localSidecarSpec(t, 73, "sip-service", filepath.Join(t.TempDir(), "missing-new"), 0, nil)
			err := sup.activate(next, time.Second)
			if err == nil || activationRollbackVerified(err) == rollbackFails {
				t.Fatalf("rollbackFails=%v error=%v", rollbackFails, err)
			}
			if rollbackFails && sup.currentProc(73) != nil {
				t.Fatal("failed rollback reported a running process")
			}
		})
	}
}

func TestExclusiveRefusesReplacementWithoutConfirmedStop(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	spec := drainFixtureSpec(t, health.URL)
	spec.installID = 74
	spec.activationStrategy = sdk.ActivationExclusive
	old := &localProc{spec: spec, done: make(chan struct{})} // Missing reap/process handle.
	sup.procs[74] = old
	next := spec
	next.binPath = filepath.Join(t.TempDir(), "must-not-start")
	err := sup.activate(next, time.Second)
	if err == nil || !strings.Contains(err.Error(), "cannot confirm process exit") {
		t.Fatalf("error=%v", err)
	}
	if sup.currentProc(74) != old {
		t.Fatal("unconfirmed old handle was dropped")
	}
	close(old.done)
	if err := sup.Stop(74); err != nil {
		t.Fatal(err)
	}
}

func TestActivationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		min, server string
		fail        bool
	}{
		{"", "0.1.0", false}, {"0.88.0", "0.87.0", true}, {"0.88.0", "0.88.0", false},
		{"0.88.0", "0.89.0", false}, {"0.88.0", "0.88.0-rc.1", true},
		{"invalid", "dev", true}, {"0.88.0", "unknown", true}, {"0.88.0", "dev", false},
	} {
		t.Run(tc.min+"_"+tc.server, func(t *testing.T) {
			m := &sdk.Manifest{Name: "sip-service", MinAptevaVersion: tc.min, Runtime: sdk.Runtime{ActivationStrategy: sdk.ActivationExclusive}}
			if err := validateAppRuntimeCompatibility(m, tc.server); (err != nil) != tc.fail {
				t.Fatalf("error=%v", err)
			}
		})
	}
	_, err := newActivationSpec(1, &sdk.Manifest{Runtime: sdk.Runtime{ActivationStrategy: "rolling"}}, "", 0, nil)
	if err == nil {
		t.Fatal("restored JSON policy was not validated")
	}
}

func TestExclusivePreparationAndCutoverAvailability(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	s.localApps.beforeExclusiveStop = s.markExclusiveActivationUnavailable
	s.localApps.afterExclusiveSpawn = s.recordExclusiveActivationPID
	bin := buildLocalSidecarFixture(t)
	m := sdk.Manifest{Name: "sip-service", Version: "1.0.0", Runtime: sdk.Runtime{ActivationStrategy: sdk.ActivationExclusive}}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	old := localSidecarSpec(t, id, m.Name, copyFixtureBinary(t, bin, "old"), 0, nil)
	old.activationStrategy = sdk.ActivationExclusive
	if err := s.localApps.activate(old, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	previous := s.localApps.currentProc(id)
	s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=?,local_pid=?,local_port=?,local_bin_path=? WHERE id=?`, localSidecarURL(int64(old.httpPort)), s.localApps.PID(id), old.httpPort, old.binPath, id)
	s.LoadInstalledApps()
	nextManifest := m
	nextManifest.Version = "2.0.0"
	nextManifest.Runtime.ActivationStrategy = ""
	if err := s.stageAppUpgrade(id, &nextManifest); err != nil {
		t.Fatal(err)
	}
	var status string
	s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status)
	if status != "running" || s.installedApps.Get(id) == nil || !processAlive(previous.cmd.Process.Pid) {
		t.Fatal("preparation interrupted old service")
	}
	observed := false
	s.localApps.afterExclusiveSpawn = func(id int64, pid int) error {
		if err := s.recordExclusiveActivationPID(id, pid); err != nil {
			return err
		}
		var savedPID int
		var message, committed string
		s.store.db.QueryRow(`SELECT status,status_message,manifest_json,local_pid FROM app_installs WHERE id=?`, id).Scan(&status, &message, &committed, &savedPID)
		var stored sdk.Manifest
		json.Unmarshal([]byte(committed), &stored)
		if status != "pending" || !strings.Contains(message, "temporarily unavailable") || savedPID != pid || stored.Runtime.ActivationStrategy != sdk.ActivationExclusive || s.installedApps.Get(id) != nil {
			return errors.New("cutover state not persisted")
		}
		observed = true
		return nil
	}
	next := localSidecarSpec(t, id, m.Name, copyFixtureBinary(t, bin, "new"), 0, nil)
	if err := s.localApps.activate(next, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("exclusive transition bypassed cutover callback")
	}
}

func TestActivationFailureDoesNotMistakeCandidateForRollback(t *testing.T) {
	sup := NewLocalSupervisor(t.TempDir())
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer health.Close()
	spec := drainFixtureSpec(t, health.URL)
	spec.installID = 75
	sup.procs[75] = &localProc{spec: spec, done: make(chan struct{})}
	if sup.previousVersionStillReady(75, &activationError{cause: errors.New("candidate failure")}) {
		t.Fatal("candidate readiness incorrectly verified the previous version")
	}
}

func TestExclusivePolicySurvivesRestartAndPendingRecovery(t *testing.T) {
	bin := buildLocalSidecarFixture(t)
	for _, phase := range []string{"preparing", "old_stopped", "candidate_starting"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestServer(t)
			s.installedApps = NewInstalledAppsRegistry()
			cache := t.TempDir()
			s.localApps = NewLocalSupervisor(cache)
			t.Cleanup(func() { s.localApps.StopAll(time.Second) })
			s.localApps.beforeExclusiveStop = s.markExclusiveActivationUnavailable
			s.localApps.afterExclusiveSpawn = s.recordExclusiveActivationPID
			events := filepath.Join(t.TempDir(), "events")
			listenerPort := unusedTCPPort(t)
			config := map[string]string{"TEST_SIDECAR_ID": "configured", "TEST_SIDECAR_EVENTS": events, "TEST_SIDECAR_FIXED_PORT": fmt.Sprint(listenerPort)}
			raw, _ := json.Marshal(config)
			encrypted, err := Encrypt(s.secret, string(raw))
			if err != nil {
				t.Fatal(err)
			}
			oldManifest := sdk.Manifest{Name: "sip-service", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service", ActivationStrategy: sdk.ActivationExclusive, Binaries: map[string]string{localPlatform(): "https://example.invalid/not-fetched"}}}
			nextManifest := oldManifest
			nextManifest.Version = "2.0.0"
			nextManifest.Runtime.ActivationStrategy = ""
			id := seedRunningInstall(t, s, oldManifest.Name, "", oldManifest, nil)
			for _, version := range []string{"1.0.0", "2.0.0"} {
				path := filepath.Join(cache, oldManifest.Name, version, "bin")
				os.MkdirAll(filepath.Dir(path), 0755)
				data, _ := os.ReadFile(bin)
				if err := os.WriteFile(path, data, 0755); err != nil {
					t.Fatal(err)
				}
			}
			oldBin := filepath.Join(cache, oldManifest.Name, "1.0.0", "bin")
			httpPort := unusedTCPPort(t)
			// Model boot from committed state: even with no tracked process, the
			// stored exclusive policy must become the recovered process's policy.
			oldJSON, _ := json.Marshal(oldManifest)
			s.store.db.Exec(`UPDATE app_installs SET config_encrypted=?,local_pid=1,local_port=?,local_bin_path=? WHERE id=?`, encrypted, httpPort, oldBin, id)
			s.resumeOneLocalInstall(id, 0, int64(httpPort), oldBin, oldManifest.Name, "", encrypted, oldManifest.Version, string(oldJSON))
			old := s.localApps.currentProc(id)
			if old == nil || old.spec.activationStrategy != sdk.ActivationExclusive {
				t.Fatal("policy/configuration lost on restart")
			}
			if err := s.stageAppUpgrade(id, &nextManifest); err != nil {
				t.Fatal(err)
			}
			if phase != "preparing" {
				if err := s.localApps.Stop(id); err != nil {
					t.Fatal(err)
				}
				if err := s.markExclusiveActivationUnavailable(id); err != nil {
					t.Fatal(err)
				}
				if phase == "candidate_starting" {
					// A surviving candidate is an orphan to the recovering supervisor.
					orphanSup := NewLocalSupervisor(t.TempDir())
					t.Cleanup(func() { orphanSup.StopAll(time.Second) })
					candidate := localSidecarSpec(t, id, oldManifest.Name, filepath.Join(cache, oldManifest.Name, "2.0.0", "bin"), 0, map[string]string{"APTEVA_APP_CONFIG": string(raw)})
					if err := orphanSup.spawn(candidate); err != nil {
						t.Fatal(err)
					}
					if err := orphanSup.waitReady(candidate, 3*time.Second); err != nil {
						t.Fatal(err)
					}
					if err := s.recordExclusiveActivationPID(id, orphanSup.PID(id)); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.ResumePendingLocalInstalls()
			end := time.Now().Add(8 * time.Second)
			for {
				var status, version, pending, message string
				err := s.store.db.QueryRow(`SELECT status,version,pending_manifest_json,error_message FROM app_installs WHERE id=?`, id).Scan(&status, &version, &pending, &message)
				if err != nil {
					t.Fatal(err)
				}
				if status == "running" && version == "2.0.0" && pending == "" {
					s.localApps.inflightG.Lock()
					_, busy := s.localApps.inflight[id]
					s.localApps.inflightG.Unlock()
					if !busy {
						break
					}
				}
				if status == "error" || time.Now().After(end) {
					t.Fatalf("recovery status=%s version=%s pending=%s error=%s", status, version, pending, message)
				}
				time.Sleep(25 * time.Millisecond)
			}
			current := s.localApps.currentProc(id)
			if current == nil || current.spec.activationStrategy != "" {
				t.Fatal("target policy not committed after recovery")
			}
			assertOrderedEvents(t, readEventLines(t, events), "start configured", "stop configured", "start configured")
		})
	}
}

func TestExclusiveVerifiedRollbackRestoresServingAndActivationRevision(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	s.localApps.beforeExclusiveStop = s.markExclusiveActivationUnavailable
	s.localApps.afterExclusiveSpawn = s.recordExclusiveActivationPID
	bin := buildLocalSidecarFixture(t)
	m := sdk.Manifest{Name: "sip-service", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service", ActivationStrategy: sdk.ActivationExclusive, Binaries: map[string]string{localPlatform(): "https://example.invalid/not-fetched"}}}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	old := localSidecarSpec(t, id, m.Name, copyFixtureBinary(t, bin, "old"), 0, nil)
	old.activationStrategy = sdk.ActivationExclusive
	if err := s.localApps.activate(old, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	oldPID := s.localApps.PID(id)
	s.store.db.Exec(`UPDATE app_installs SET local_pid=?,local_bin_path=?,local_port=?,sidecar_url_override=? WHERE id=?`, oldPID, old.binPath, old.httpPort, localSidecarURL(int64(old.httpPort)), id)
	s.LoadInstalledApps()
	next := m
	next.Version = "2.0.0"
	next.Runtime.ActivationStrategy = ""
	badBin := filepath.Join(cache, m.Name, next.Version, "bin")
	os.MkdirAll(filepath.Dir(badBin), 0755)
	if err := os.WriteFile(badBin, []byte("not executable"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.stageAppUpgrade(id, &next); err != nil {
		t.Fatal(err)
	}
	err := s.installLocally(id, &next, "", nil)
	if err == nil || !activationRollbackVerified(err) {
		t.Fatalf("rollback: %v", err)
	}
	var status, version, pending, message string
	var revision int64
	var pid int
	if err := s.store.db.QueryRow(`SELECT status,version,pending_manifest_json,status_message,mcp_activation_revision,local_pid FROM app_installs WHERE id=?`, id).Scan(&status, &version, &pending, &message, &revision, &pid); err != nil {
		t.Fatal(err)
	}
	if status != "running" || version != m.Version || pending != "" || !strings.Contains(message, "previous version still running") || revision != 1 || pid == oldPID || s.installedApps.Get(id) == nil {
		t.Fatalf("rollback state=%s version=%s pending=%s message=%s revision=%d pid=%d oldpid=%d", status, version, pending, message, revision, pid, oldPID)
	}
}
