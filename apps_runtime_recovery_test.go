package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (*Server, *appRuntimeManager, *runtimeBackendFixture, runtimeDeployment) {
	t.Helper()
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	return s, m, f, d
}
func fencedFixture(f *runtimeBackendFixture, d runtimeDeployment, state, identity, action string, healthCode int) {
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/exec/"+d.ID+"/") {
			if strings.HasSuffix(r.URL.Path, "/health") {
				w.WriteHeader(healthCode)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/app/status") {
				writeJSON(w, map[string]any{"deployment_id": identity, "generation": d.Generation, "admitting": state == "active", "state": state, "recovery": map[string]string{"action": action}})
				return
			}
		}
		original.ServeHTTP(w, r)
	})
}
func TestManagedRuntimeAutomaticFencedRecovery(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	fencedFixture(f, d, "fenced", d.ID, "restart", 503)
	f.mu.Lock()
	out := f.executions[d.ID]
	out.Ready = false
	f.executions[d.ID] = out
	f.mu.Unlock()
	collectRuntimeMonitor(t, m, d.InstallID)
	if current, _ := m.active(d.InstallID); current.ID != d.ID {
		t.Fatal("one probe restarted app")
	}
	nextRuntimeHealth(t, m, d.ID)
	var opID string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, err := m.operation(opID)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatalf("recovery failed: %s", op.Error)
	}
	current, _ := m.active(d.InstallID)
	old, _ := m.loadDeployment(d.ID)
	if current.ID == d.ID || current.Generation <= d.Generation || old.Status != "stopped" {
		t.Fatalf("replacement unsafe current=%s old=%s", current.ID, old.Status)
	}
	var attempts int
	if err := s.store.db.QueryRow(`SELECT attempts FROM app_runtime_recovery WHERE install_id=?`, d.InstallID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts %d %v", attempts, err)
	}
	// Stale callbacks/observations cannot restart the new owner.
	m.maybeRecoverRuntime(d, runtimeHealthObservation{Admission: "fenced", Recovery: "restart"})
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate recovery %d", n)
	}
}
func TestManagedRuntimeRecoveryRequiresExplicitCurrentIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, state, identity, action string
		code                          int
	}{
		{"draining", "draining", "current", "restart", 503}, {"no request", "fenced", "current", "", 503}, {"wrong identity", "fenced", "other", "restart", 503}, {"healthy", "active", "current", "restart", 200}, {"failed auth", "fenced", "current", "restart", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, d := recoveryFixture(t)
			identity := tc.identity
			if identity == "current" {
				identity = d.ID
			}
			fencedFixture(f, d, tc.state, identity, tc.action, tc.code)
			collectRuntimeMonitor(t, m, d.InstallID)
			nextRuntimeHealth(t, m, d.ID)
			nextRuntimeHealth(t, m, d.ID)
			var n int
			s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&n)
			if n != 0 {
				t.Fatalf("unauthorized restart %d", n)
			}
		})
	}
}
func TestManagedRuntimeRecoveryCooldownAndOwnerCAS(t *testing.T) {
	s, m, _, d := recoveryFixture(t)
	now := time.Now()
	claim := func(expected string, at time.Time, commit bool) error {
		tx, e := s.store.db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		e = m.claimRuntimeRecovery(tx, d.InstallID, expected, at)
		if e == nil && commit {
			e = tx.Commit()
		}
		return e
	}
	if err := claim("obsolete", now, true); err == nil {
		t.Fatal("wrong owner accepted")
	}
	for i, delay := range []int64{30, 60, 120, 240, 300, 300} {
		if err := claim(d.ID, now, true); err != nil {
			t.Fatal(err)
		}
		var next int64
		s.store.db.QueryRow(`SELECT next_attempt FROM app_runtime_recovery WHERE install_id=?`, d.InstallID).Scan(&next)
		if next-now.Unix() != delay {
			t.Fatalf("attempt %d delay %d", i, next-now.Unix())
		}
		if err := claim(d.ID, now.Add(time.Second), true); err == nil {
			t.Fatal("cooldown bypass")
		}
		now = time.Unix(next, 0)
	}
	// Transaction rollback does not consume a retry slot.
	before := now
	if err := claim(d.ID, before, false); err != nil {
		t.Fatal(err)
	}
	if err := claim(d.ID, before, true); err != nil {
		t.Fatal(err)
	}
	s.store.db.Exec(`UPDATE app_installs SET runtime_pending_deployment='pending' WHERE id=?`, d.InstallID)
	if err := claim(d.ID, now.Add(time.Hour), true); err == nil {
		t.Fatal("pending migration restarted")
	}
	s.store.db.Exec(`UPDATE app_installs SET runtime_pending_deployment='',status='disabled' WHERE id=?`, d.InstallID)
	if err := claim(d.ID, now.Add(time.Hour), true); err == nil {
		t.Fatal("disabled installation restarted")
	}
}

func TestManagedRuntimeRecoveryRejectsUnverifiedBackend(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	fencedFixture(f, d, "fenced", d.ID, "restart", 503)
	f.mu.Lock()
	out := f.executions[d.ID]
	out.ReleaseDigest = strings.Repeat("b", 64)
	f.executions[d.ID] = out
	f.mu.Unlock()
	m.maybeRecoverRuntime(d, runtimeHealthObservation{Admission: "fenced", Recovery: "restart"})
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&n)
	if n != 0 {
		t.Fatal("mismatched backend execution authorized replacement")
	}
}

func TestManagedRuntimeRecoveryRequiresConfirmedStop(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	fencedFixture(f, d, "fenced", d.ID, "restart", 503)
	f.mu.Lock()
	f.failStop = true
	f.mu.Unlock()
	collectRuntimeMonitor(t, m, d.InstallID)
	nextRuntimeHealth(t, m, d.ID)
	var opID string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, _ := m.operation(opID)
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "failed" {
		t.Fatalf("unconfirmed stop succeeded: %+v", op)
	}
	current, _ := m.active(d.InstallID)
	if current.ID != "" && current.ID != d.ID {
		t.Fatal("replacement became owner before stop confirmation")
	}
	candidate, err := m.loadDeployment(op.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status == "active" {
		t.Fatal("unconfirmed shutdown activated candidate")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if event == "start:"+op.DeploymentID {
			t.Fatal("replacement started before confirmed shutdown")
		}
	}
}

func TestManagedRuntimeRecoveryBudgetSurvivesControllerRestart(t *testing.T) {
	s, m, _, d := recoveryFixture(t)
	now := time.Now()
	tx, _ := s.store.db.Begin()
	if err := m.claimRuntimeRecovery(tx, d.InstallID, d.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restarted := &appRuntimeManager{s: s}
	tx, _ = s.store.db.Begin()
	err := restarted.claimRuntimeRecovery(tx, d.InstallID, d.ID, now.Add(time.Second))
	tx.Rollback()
	if err == nil {
		t.Fatal("controller restart bypassed recovery budget")
	}
	tx, _ = s.store.db.Begin()
	if err := restarted.claimRuntimeRecovery(tx, d.InstallID, d.ID, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	var attempts int
	s.store.db.QueryRow(`SELECT attempts FROM app_runtime_recovery WHERE install_id=?`, d.InstallID).Scan(&attempts)
	if attempts != 1 {
		t.Fatal("stable interval did not reset retry budget")
	}
	replica := d
	replica.ID = "other-active-replica"
	replica.CredentialEncrypted = ""
	replica.Replica = 1
	if err := m.saveDeployment(replica); err != nil {
		t.Fatal(err)
	}
	tx, _ = s.store.db.Begin()
	err = restarted.claimRuntimeRecovery(tx, d.InstallID, d.ID, now.Add(time.Hour))
	tx.Rollback()
	if err == nil {
		t.Fatal("whole installation recovery would disrupt another replica")
	}
}
