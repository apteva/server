package main

import (
	"database/sql"
	"fmt"
	"time"
)

// Recovery requires authenticated, identity-bound app status: an explicit
// fenced restart request or standby of the already committed owner. Failed
// HTTP/host probes and ordinary draining never authorize a replacement.
func (m *appRuntimeManager) maybeRecoverRuntime(d runtimeDeployment, health runtimeHealthObservation) {
	if !runtimeRecoveryRequested(health) || !m.runtimeMonitorCurrent(d) {
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
	if !runtimeRecoveryRequested(fresh) || fresh.Recovery != health.Recovery {
		return
	}
	kind := "restart"
	if fresh.Recovery == "activate" {
		kind = "readmit"
	}
	_, _ = m.beginExpected(d.InstallID, kind, fmt.Sprintf("recover-%s-%d", d.ID, time.Now().Unix()/30), d.Placement, nil, d.ID)
}

func runtimeRecoveryRequested(health runtimeHealthObservation) bool {
	return health.Recovery == "restart" && health.Admission == "fenced" || health.Recovery == "activate" && health.Admission == "standby"
}

// An already-owned standby execution needs activation, not another process.
// The same durable operation ownership, generation CAS and persisted recovery
// budget apply; neither stale health nor a failed inspect authorizes admission.
func (m *appRuntimeManager) readmitRuntime(op *runtimeOperation) error {
	if err := m.phase(op, "verifying_admission"); err != nil {
		return err
	}
	d, err := m.active(op.InstallID)
	if err != nil || d.ID != op.DeploymentID || d.ID != op.PreviousID {
		return fmt.Errorf("standby recovery owner changed")
	}
	current := func() error {
		if err := m.checkOwner(*op); err != nil {
			return err
		}
		var n int
		err := m.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs i WHERE i.id=? AND i.runtime_active_deployment=? AND i.runtime_generation=? AND i.runtime_pending_deployment='' AND i.runtime_managed=1 AND i.status='running' AND EXISTS(SELECT 1 FROM app_runtime_deployments d WHERE d.id=i.runtime_active_deployment AND d.install_id=i.id AND d.generation=i.runtime_generation AND d.status='active') AND (SELECT COUNT(*) FROM app_runtime_deployments WHERE install_id=i.id AND status='active')=1`, d.InstallID, d.ID, d.Generation).Scan(&n)
		if err != nil || n != 1 {
			return fmt.Errorf("standby recovery owner or generation changed")
		}
		return nil
	}
	if err = current(); err != nil {
		return err
	}
	b, err := m.backend(d)
	if err != nil {
		return err
	}
	req, err := m.request(d, *op, "inspect")
	if err != nil {
		return err
	}
	out, err := b.Inspect(req)
	if err != nil {
		return err
	}
	out, err = runtimeVerifiedObservation(d, out)
	if err != nil || out.State != "running" {
		return fmt.Errorf("standby execution ownership could not be verified")
	}
	health := m.runtimeAppHealth(d, out)
	if !health.IdentityVerified || !(health.Recovery == "activate" && health.Admission == "standby" || health.Healthy && health.Admitting) {
		return fmt.Errorf("owned execution is no longer in recoverable standby")
	}
	d.Execution = out
	token, err := m.token(d)
	if err != nil {
		return err
	}
	if err = m.verifyRuntimeCapabilities(&d, out.Endpoint, token); err != nil {
		return err
	}
	if !runtimeNeedsAdmission(d) {
		return fmt.Errorf("standby app has no verified activation capability")
	}
	if err = current(); err != nil {
		return err
	}
	if err = m.saveDeployment(d); err != nil {
		return err
	}
	if err = m.commitExecutionOwnership(d, *op); err != nil {
		return err
	}
	if !health.Admitting {
		if err = m.phase(op, "activating_owned_execution"); err != nil {
			return err
		}
		if err = m.activateAndConfirm(d, *op); err != nil {
			return err
		}
	} else {
		// A resumed operation may find activation already completed. The
		// descriptor request above must not turn a stale active status into
		// success if the app lost admission while capabilities were checked.
		fresh := m.runtimeAppHealth(d, out)
		if !fresh.Healthy || !fresh.IdentityVerified || !fresh.Admitting {
			return fmt.Errorf("owned execution admission is no longer confirmed")
		}
		if err = m.confirmExecutionAdmission(d, *op); err != nil {
			return err
		}
	}
	if err = current(); err != nil {
		return err
	}
	if err = m.saveDeployment(d); err != nil {
		return err
	}
	_, err = m.s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=?,status_message='',error_message='' WHERE id=? AND runtime_active_deployment=? AND runtime_generation=? AND status='running' AND runtime_pending_deployment=''`, out.Endpoint, d.InstallID, d.ID, d.Generation)
	if err == nil && m.s.installedApps != nil {
		m.s.LoadInstalledApps()
	}
	return err
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
	// Automatic recovery must not disrupt another active replica.
	if count != 1 {
		return fmt.Errorf("automatic recovery requires a single active execution")
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
