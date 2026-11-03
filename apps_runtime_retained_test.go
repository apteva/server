package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func runtimeObservation(t *testing.T, s *Server, id string) (state, admission, message string) {
	t.Helper()
	if err := s.store.db.QueryRow(`SELECT state,admission_state,message FROM app_runtime_observations WHERE deployment_id=?`, id).Scan(&state, &admission, &message); err != nil {
		t.Fatal(err)
	}
	return state, admission, message
}
func runtimeInstallGeneration(t *testing.T, s *Server, id int64) (int64, string) {
	t.Helper()
	var generation int64
	var pending string
	if err := s.store.db.QueryRow(`SELECT runtime_generation,runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&generation, &pending); err != nil {
		t.Fatal(err)
	}
	return generation, pending
}
func discardRuntimeCandidate(t *testing.T, m *appRuntimeManager, id int64, key string) {
	t.Helper()
	op, err := m.begin(id, "discard", key, runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op = waitRuntimeOperation(t, m, op); op.Status != "succeeded" {
		t.Fatalf("discard: %s", op.Error)
	}
}

// A pending or failed candidate advances the installation's generation. The
// published execution it keeps is still observed, and recovered once the
// candidate is discarded.
func TestManagedRuntimeRetainedExecutionMonitoredWhileCandidatePending(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	candidate := prepareRuntimeFixture(t, m, d.InstallID, d.Placement, "prepare-next")
	if generation, pending := runtimeInstallGeneration(t, s, d.InstallID); generation == d.Generation || pending != candidate.ID {
		t.Fatal("fixture did not leave a pending candidate", generation, pending)
	}
	f.mu.Lock()
	f.admitting[d.ID] = false
	f.mu.Unlock()
	collectRuntimeMonitor(t, m, d.InstallID)
	nextRuntimeHealth(t, m, d.ID)
	state, admission, message := runtimeObservation(t, s, d.ID)
	if state != "degraded" || admission != "standby" || !strings.Contains(message, runtimeRecoveryWaits) {
		t.Fatal("retained execution was not observed", state, admission, message)
	}
	var n int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE install_id=? AND kind='readmit'`, d.InstallID).Scan(&n)
	if n != 0 {
		t.Fatal("recovery ran over a pending candidate")
	}
	discardRuntimeCandidate(t, m, d.InstallID, "discard-next")
	if generation, pending := runtimeInstallGeneration(t, s, d.InstallID); generation != d.Generation || pending != "" {
		t.Fatal("discard left the candidate generation current", generation, pending)
	}
	collectRuntimeMonitor(t, m, d.InstallID)
	nextRuntimeHealth(t, m, d.ID)
	var opID string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND kind='readmit'`, d.InstallID).Scan(&opID); err != nil {
		t.Fatal("readmission did not resume after discard: ", err)
	}
	op, err := m.operation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if op = waitRuntimeOperation(t, m, op); op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.admitting[d.ID] {
		t.Fatal("retained execution remains in standby")
	}
}

func TestManagedRuntimeGenerationRepairAfterDiscardedCandidate(t *testing.T) {
	s, m, _, d := recoveryFixture(t)
	// An older discard cleared the candidate but kept its generation.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET runtime_generation=? WHERE id=?`, d.Generation+1, d.InstallID); err != nil {
		t.Fatal(err)
	}
	if err := s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	if generation, _ := runtimeInstallGeneration(t, s, d.InstallID); generation != d.Generation {
		t.Fatal("published generation not restored", generation)
	}
	// A pending candidate's generation stays current until it is resolved.
	candidate := prepareRuntimeFixture(t, m, d.InstallID, d.Placement, "prepare-next")
	if err := s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	if generation, pending := runtimeInstallGeneration(t, s, d.InstallID); generation != candidate.Generation || pending != candidate.ID {
		t.Fatal("repair touched a pending candidate", generation, pending)
	}
}

// Abandoning an exclusive cutover after draining the previous execution must
// re-arm it, and report retention as admitting only with confirmed admission.
func TestManagedRuntimeAbandonedDrainRestoresAdmission(t *testing.T) {
	for _, tc := range []struct {
		name           string
		activate, ack  bool
		want           string
		admittingAfter bool
	}{
		{name: "restart", want: "previous execution retained and admitting", admittingAfter: true},
		{name: "activate", activate: true, want: "previous execution retained and admitting", admittingAfter: true},
		{name: "acknowledgement-only", ack: true, want: "previous execution retained but its admission was not confirmed (standby)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, d := recoveryFixture(t)
			d.Manifest.Runtime.StartupTimeoutSeconds = 1
			d.Manifest.Runtime.ActivationStrategy = "exclusive"
			if err := m.saveDeployment(d); err != nil {
				t.Fatal(err)
			}
			s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(d.Manifest), d.InstallID)
			drained := make(chan struct{})
			var once sync.Once
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/exec/"+d.ID+"/") {
					if strings.HasSuffix(r.URL.Path, "/app/drain") {
						f.mu.Lock()
						f.admitting[d.ID] = false
						f.mu.Unlock()
						once.Do(func() { close(drained) })
						writeJSON(w, map[string]any{"drained": false})
						return
					}
					if tc.ack && strings.HasSuffix(r.URL.Path, "/app/activate") {
						w.WriteHeader(200)
						return
					}
				}
				original.ServeHTTP(w, r)
			})
			kind, placement := "restart", d.Placement
			if tc.activate {
				prepareRuntimeFixture(t, m, d.InstallID, d.Placement, "prepare-next")
				kind, placement = "activate", runtimePlacement{}
			}
			op, err := m.begin(d.InstallID, kind, "abandon-"+tc.name, placement, nil)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("previous execution was not drained")
			}
			if err = m.cancelOperation(op.ID, d.InstallID); err != nil {
				t.Fatal(err)
			}
			op = waitRuntimeOperation(t, m, op)
			if op.Status != "cancelled" || !strings.Contains(op.Error, tc.want) {
				t.Fatalf("%s: %s", op.Status, op.Error)
			}
			if current, _ := m.active(d.InstallID); current.ID != d.ID {
				t.Fatal("abandoned cutover replaced the previous execution")
			}
			f.mu.Lock()
			admitting := f.admitting[d.ID]
			f.mu.Unlock()
			if admitting != tc.admittingAfter {
				t.Fatal("previous execution admission does not match the reported outcome")
			}
		})
	}
}

// A confirmed exit is recorded before the restart, and stays recorded with
// the reason when the restart cannot start or already failed.
func TestManagedRuntimeExitedProcessRecordedUnavailable(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	prepareRuntimeFixture(t, m, d.InstallID, d.Placement, "prepare-next")
	m.restartExitedRuntime(d)
	state, admission, message := runtimeObservation(t, s, d.ID)
	if state != "unavailable" || admission != "unknown" || message != "Process exited; "+runtimeRecoveryWaits {
		t.Fatal("exit over a pending candidate not recorded", state, admission, message)
	}
	var n int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE idempotency_key=?`, "reconcile-"+d.ID).Scan(&n)
	if n != 0 {
		t.Fatal("restart attempted over a pending candidate")
	}
	discardRuntimeCandidate(t, m, d.InstallID, "discard-next")
	f.mu.Lock()
	f.oldID = d.ID
	f.failStart = true
	f.mu.Unlock()
	m.restartExitedRuntime(d)
	if state, _, message = runtimeObservation(t, s, d.ID); state != "unavailable" || message != "Process exited; restarting" {
		t.Fatal("exit not recorded before restart", state, message)
	}
	var opID string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE idempotency_key=?`, "reconcile-"+d.ID).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, err := m.operation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if op = waitRuntimeOperation(t, m, op); op.Status != "failed" {
		t.Fatal("fixture restart did not fail", op.Status)
	}
	m.restartExitedRuntime(d)
	if state, _, message = runtimeObservation(t, s, d.ID); state != "unavailable" || !strings.HasPrefix(message, "Process exited; automatic restart failed: ") {
		t.Fatal("failed restart not recorded", state, message)
	}
}
