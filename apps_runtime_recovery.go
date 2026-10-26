package main

import (
	"database/sql"
	"fmt"
	"time"
)

// Recovery is opt-in through an authenticated, identity-bound app status. Failed
// HTTP/host probes and ordinary draining never authorize a replacement.
func (m *appRuntimeManager) maybeRecoverRuntime(d runtimeDeployment, health runtimeHealthObservation) {
	if health.Recovery != "restart" || health.Admission != "fenced" || !m.runtimeMonitorCurrent(d) {
		return
	}
	var next int64
	err := m.s.store.db.QueryRow(`SELECT next_attempt FROM app_runtime_recovery WHERE install_id=?`, d.InstallID).Scan(&next)
	if err != nil && err != sql.ErrNoRows || time.Now().Unix() < next {
		return
	}
	// A prior cached inspection is insufficient authority for a restart.
	backend, err := m.backend(d)
	if err != nil {
		return
	}
	req, err := m.request(d, runtimeOperation{ID: "recovery-check"}, "inspect")
	if err != nil {
		return
	}
	out, err := backend.Inspect(req)
	if err != nil {
		return
	}
	out, err = runtimeVerifiedObservation(d, out)
	if err != nil || out.State != "running" {
		return
	}
	// Recheck status after inspection: an old failure cannot restart a recovered app.
	fresh := m.runtimeAppHealth(d, out)
	if fresh.Recovery != "restart" || fresh.Admission != "fenced" {
		return
	}
	_, _ = m.beginExpected(d.InstallID, "restart", fmt.Sprintf("recover-%s-%d", d.ID, time.Now().Unix()/30), d.Placement, nil, d.ID)
}

// The owner guard, cooldown and lifecycle operation are committed atomically.
// Persisting the cooldown prevents Server restarts or new app generations from
// turning repeated failures into a tight replacement loop.
func (m *appRuntimeManager) claimRuntimeRecovery(tx *sql.Tx, id int64, expected string, now time.Time) error {
	var active, pending, status string
	var count int
	if err := tx.QueryRow(`SELECT runtime_active_deployment,runtime_pending_deployment,status FROM app_installs WHERE id=? AND runtime_managed=1`, id).Scan(&active, &pending, &status); err != nil {
		return err
	}
	if active != expected || pending != "" || status != "running" {
		return fmt.Errorf("recovery owner changed or installation is not idle")
	}
	if err := tx.QueryRow(`SELECT count(*) FROM app_runtime_deployments WHERE install_id=? AND status='active'`, id).Scan(&count); err != nil {
		return err
	}
	// Whole-installation restart must not disrupt another active replica.
	if count != 1 {
		return fmt.Errorf("automatic replacement requires a single active execution")
	}
	if err := tx.QueryRow(`SELECT count(*) FROM app_runtime_deployments WHERE id=? AND install_id=? AND status='active'`, expected, id).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("recovery deployment is no longer active")
	}
	var attempts int
	var last, next int64
	err := tx.QueryRow(`SELECT attempts,last_attempt,next_attempt FROM app_runtime_recovery WHERE install_id=?`, id).Scan(&attempts, &last, &next)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if now.Unix() < next {
		return fmt.Errorf("automatic recovery is backing off")
	}
	if now.Unix()-last >= 900 {
		attempts = 0
	}
	attempts++
	if attempts > 5 {
		attempts = 5
	}
	delay := 30 * time.Second * time.Duration(1<<uint(attempts-1))
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	_, err = tx.Exec(`INSERT INTO app_runtime_recovery(install_id,attempts,last_attempt,next_attempt) VALUES(?,?,?,?) ON CONFLICT(install_id) DO UPDATE SET attempts=excluded.attempts,last_attempt=excluded.last_attempt,next_attempt=excluded.next_attempt`, id, attempts, now.Unix(), now.Add(delay).Unix())
	return err
}
