package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Execution grants authorize private app admission, never public routes. A
// revoked row is retained so restart/migration cannot resurrect an old owner.
// Multiple grants are allowed only for an explicitly nonexclusive cutover or
// eligible replicas; each lease still names one exact deployment/generation.
func runtimeRollingBack(op runtimeOperation) bool {
	return op.RollbackRequested || op.Phase == "rolling_back" || op.RollbackID != ""
}

func runtimeOwnershipExclusive(a, b runtimeDeployment) bool {
	return runtimeExclusive(a, b) || a.Component != nil || b.Component != nil ||
		runtimeNeedsAdmission(a) && !a.AppCaps.ReplicaSafe || runtimeNeedsAdmission(b) && !b.AppCaps.ReplicaSafe
}

func (m *appRuntimeManager) runtimeOperationTx(tx *sql.Tx, op runtimeOperation) (runtimeOperation, error) {
	// Backend work for a rollback targets its recovery execution; retain the
	// original candidate identity in the durable lifecycle operation.
	if op.RollbackID != "" && op.DeploymentID == op.RollbackID {
		op.DeploymentID = op.FailedID
	}
	var raw, owner, status string
	var lease int64
	var cancelled int
	err := tx.QueryRow(`SELECT record_json,owner,lease_until,status,cancel_requested FROM app_runtime_operations WHERE id=? AND install_id=?`, op.ID, op.InstallID).Scan(&raw, &owner, &lease, &status, &cancelled)
	var stored runtimeStoredOperation
	if err != nil {
		return op, err
	}
	if json.Unmarshal([]byte(raw), &stored) != nil || owner != m.owner || lease < time.Now().Unix() || status != "running" || m.ctx.Err() != nil {
		return op, fmt.Errorf("deployment operation ownership lost")
	}
	actual := stored.runtimeOperation
	if cancelled != 0 && !runtimeRollingBack(actual) {
		return op, errRuntimeCancelled
	}
	if actual.PreviousID != op.PreviousID || actual.DeploymentID != op.DeploymentID || actual.RollbackID != op.RollbackID {
		return op, fmt.Errorf("deployment operation identity changed")
	}
	return actual, nil
}

// Validate the transition against the journal inside the transaction that
// changes ownership or publication. checkOwner followed by an unguarded write
// is insufficient: the lifecycle lease may change between those steps.
func (m *appRuntimeManager) runtimeTransitionTx(tx *sql.Tx, d runtimeDeployment, op runtimeOperation) error {
	actual, err := m.runtimeOperationTx(tx, op)
	if err != nil {
		return err
	}
	rollback := runtimeRollingBack(actual)
	allowed := d.ID == actual.DeploymentID
	if rollback {
		allowed = d.ID == actual.PreviousID || d.ID == actual.RollbackID
	}
	if !allowed || d.InstallID != actual.InstallID {
		return fmt.Errorf("candidate is not owned by this operation")
	}
	var active, pending string
	var generation int64
	if err = tx.QueryRow(`SELECT runtime_active_deployment,runtime_pending_deployment,runtime_generation FROM app_installs WHERE id=? AND runtime_managed=1`, d.InstallID).Scan(&active, &pending, &generation); err != nil {
		return err
	}
	if active != actual.PreviousID && active != d.ID {
		return fmt.Errorf("published predecessor changed")
	}
	switch {
	case actual.Kind == "readmit":
		if active != d.ID || pending != "" || generation != d.Generation {
			return fmt.Errorf("readmission owner changed")
		}
	case actual.Kind == "scale":
		if pending != "" || generation != d.Generation {
			return fmt.Errorf("replica generation changed")
		}
	default:
		expected := actual.DeploymentID
		if actual.RollbackID != "" {
			expected = actual.RollbackID
		}
		if pending != expected && !(pending == "" && active == d.ID) {
			return fmt.Errorf("pending candidate changed")
		}
		if !rollback && generation != d.Generation {
			return fmt.Errorf("candidate generation changed")
		}
	}
	var raw string
	if err = tx.QueryRow(`SELECT record_json FROM app_runtime_deployments WHERE id=? AND install_id=? AND generation=? AND status IN ('ready','active')`, d.ID, d.InstallID, d.Generation).Scan(&raw); err != nil {
		return fmt.Errorf("candidate readiness no longer verified: %w", err)
	}
	var saved runtimeStoredDeployment
	if json.Unmarshal([]byte(raw), &saved) != nil || saved.AppCaps != d.AppCaps || saved.AppCapsKnown != d.AppCapsKnown || runtimeFingerprint(saved.Manifest) != runtimeFingerprint(d.Manifest) || saved.Execution.Handle != d.Execution.Handle {
		return fmt.Errorf("verified candidate changed")
	}
	return nil
}

func (m *appRuntimeManager) commitExecutionOwnership(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = m.runtimeTransitionTx(tx, d, op); err != nil {
		return err
	}
	// Evaluate every possible predecessor, not just the primary routing pointer.
	// A missing grant is not evidence that an old process has stopped writing.
	rows, err := tx.Query(`SELECT record_json FROM app_runtime_deployments WHERE install_id=? AND id!=? AND status NOT IN ('preparing','prepared','deleted')`, d.InstallID, d.ID)
	if err != nil {
		return err
	}
	var others []runtimeDeployment
	for rows.Next() {
		var raw string
		var v runtimeStoredDeployment
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			break
		}
		others = append(others, v.runtimeDeployment)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	rollback := runtimeRollingBack(op)
	for _, other := range others {
		if other.Status == "stopped" && other.Execution.StoppedConfirmed {
			continue
		}
		exclusive := runtimeOwnershipExclusive(other, d) || op.Recovery && op.Kind != "readmit" || other.Generation != d.Generation && !sameRuntimeTarget(other, d)
		if exclusive {
			return fmt.Errorf("execution ownership requires confirmed predecessor shutdown")
		}
		// Nonexclusive rolling upgrades can keep the previous generation draining.
		// Unrelated candidates cannot acquire permission through this exception.
		if other.Generation != d.Generation && other.ID != op.PreviousID {
			var previousGeneration int64
			if e := tx.QueryRow(`SELECT generation FROM app_runtime_deployments WHERE id=?`, op.PreviousID).Scan(&previousGeneration); e != nil || other.Generation != previousGeneration {
				return fmt.Errorf("another execution still owns this installation")
			}
		}
		if rollback && other.ID == op.DeploymentID {
			return fmt.Errorf("rollback requires confirmed candidate shutdown")
		}
	}
	// Revoke all confirmed-stopped executions atomically with the grant. Live
	// nonexclusive predecessors retain their leases until drained and stopped.
	if _, err = tx.Exec(`UPDATE app_runtime_execution_owners SET granted=0,admission_confirmed=0 WHERE install_id=? AND deployment_id IN (SELECT id FROM app_runtime_deployments WHERE install_id=? AND status IN ('stopped','deleted'))`, d.InstallID, d.InstallID); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO app_runtime_execution_owners(deployment_id,install_id,generation,operation_id,granted) VALUES(?,?,?,?,1) ON CONFLICT(deployment_id) DO UPDATE SET operation_id=excluded.operation_id,granted=1,admission_confirmed=0 WHERE install_id=excluded.install_id AND generation=excluded.generation`, d.ID, d.InstallID, d.Generation, op.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (m *appRuntimeManager) requireExecutionOwnershipTx(tx *sql.Tx, d runtimeDeployment, op runtimeOperation) error {
	if err := m.runtimeTransitionTx(tx, d, op); err != nil {
		return err
	}
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM app_runtime_execution_owners WHERE deployment_id=? AND install_id=? AND generation=? AND operation_id=? AND granted=1`, d.ID, d.InstallID, d.Generation, op.ID).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("execution ownership was not committed")
	}
	return nil
}
func (m *appRuntimeManager) requireExecutionOwnership(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return m.requireExecutionOwnershipTx(tx, d, op)
}

func (m *appRuntimeManager) revokeExecutionOwnership(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Cleanup also needs the operation lease, but not a ready candidate or a
	// matching route (failed startup and retired replicas need revocation too).
	if _, err = m.runtimeOperationTx(tx, op); err != nil {
		return err
	}
	if d.InstallID != op.InstallID {
		return fmt.Errorf("execution installation mismatch")
	}
	_, err = tx.Exec(`INSERT INTO app_runtime_execution_owners(deployment_id,install_id,generation,operation_id,granted) VALUES(?,?,?,?,0) ON CONFLICT(deployment_id) DO UPDATE SET granted=0,admission_confirmed=0`, d.ID, d.InstallID, d.Generation, op.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Persist admission separately from the execution grant so an interrupted or
// future controller cannot publish a merely-owned standby execution.
func (m *appRuntimeManager) confirmExecutionAdmission(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = m.requireExecutionOwnershipTx(tx, d, op); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE app_runtime_execution_owners SET admission_confirmed=1 WHERE deployment_id=?`, d.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (m *appRuntimeManager) requirePublicationTx(tx *sql.Tx, d runtimeDeployment, op runtimeOperation) error {
	if err := m.requireExecutionOwnershipTx(tx, d, op); err != nil {
		return err
	}
	var admitted int
	if err := tx.QueryRow(`SELECT admission_confirmed FROM app_runtime_execution_owners WHERE deployment_id=?`, d.ID).Scan(&admitted); err != nil {
		return err
	}
	if admitted != 1 {
		return fmt.Errorf("execution admission was not confirmed")
	}
	return nil
}
func (m *appRuntimeManager) requirePublication(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return m.requirePublicationTx(tx, d, op)
}
func runtimePublishExecutionTx(tx *sql.Tx, d runtimeDeployment) error {
	if d.Placement.Ingress != nil && d.Placement.Ingress.Provider != nil {
		runtimeIngressEndpoints(&d)
	}
	d.Status = "active"
	raw := runtimeJSON(runtimeStoredDeployment{runtimeDeployment: d, Credential: d.CredentialEncrypted, Environment: d.EnvironmentEncrypted, Bin: d.LocalBin, Port: d.LocalPort, PID: d.LocalPID})
	_, err := tx.Exec(`UPDATE app_runtime_deployments SET status='active',record_json=? WHERE id=?`, raw, d.ID)
	return err
}
func (m *appRuntimeManager) commitReplica(d runtimeDeployment, op runtimeOperation) error {
	tx, err := m.s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = m.requirePublicationTx(tx, d, op); err != nil {
		return err
	}
	if err = runtimePublishExecutionTx(tx, d); err != nil {
		return err
	}
	return tx.Commit()
}
