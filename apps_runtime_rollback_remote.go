package main

import "fmt"

// A confirmed stopped execution is terminal on some backends. Restore a pinned
// release as a fresh execution/credential generation instead of resurrecting a
// retired process or depending on a provider-specific resume operation.
func (m *appRuntimeManager) rollbackRemoteDeployment(op *runtimeOperation, old, candidate runtimeDeployment, cause error) error {
	var recovery runtimeDeployment
	var err error
	if op.RollbackID == "" {
		recovery, err = m.makeDeployment(old.InstallID, old.Placement, &old.Manifest, old.Replica)
		if err != nil {
			return fmt.Errorf("previous deployment recovery could not be prepared")
		}
		recovery.Package = old.Package
		recovery.PackagePreparation = old.PackagePreparation
		recovery.TargetInfo = old.TargetInfo
		recovery.AppCaps = old.AppCaps
		recovery.Execution.StateRef = old.Execution.StateRef
		if err = m.checkOwner(*op); err != nil {
			return err
		}
		tx, err := m.s.store.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		raw := runtimeJSON(runtimeStoredDeployment{runtimeDeployment: recovery, Credential: recovery.CredentialEncrypted, Environment: recovery.EnvironmentEncrypted})
		if _, err = tx.Exec(`INSERT INTO app_runtime_deployments(id,install_id,generation,replica,status,record_json) VALUES(?,?,?,?,?,?)`, recovery.ID, recovery.InstallID, recovery.Generation, recovery.Replica, recovery.Status, raw); err != nil {
			return err
		}
		op.RollbackID = recovery.ID
		op.FailedID = candidate.ID
		if _, err = tx.Exec(`UPDATE app_runtime_operations SET record_json=? WHERE id=?`, runtimeJSON(runtimeStoredOperation{*op, op.SnapshotPath}), op.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE app_installs SET runtime_pending_deployment=?,runtime_generation=? WHERE id=?`, recovery.ID, recovery.Generation, recovery.InstallID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if err = m.saveDeployment(recovery); err != nil {
			return err
		} // Index the new credential after the journal transaction.
	} else {
		recovery, err = m.loadDeployment(op.RollbackID)
		if err != nil {
			return err
		}
	}
	if active, err := m.active(old.InstallID); err == nil && active.ID == recovery.ID && recovery.Status == "active" {
		return fmt.Errorf("%w; previous deployment restored and verified; database changes are not automatically undone", cause)
	}
	work := *op
	work.DeploymentID = recovery.ID
	if recovery.Status == "preparing" || recovery.Status == "prepared" {
		if err = m.prepare(&work); err != nil {
			return fmt.Errorf("activation failed; previous deployment recovery preparation failed: %w", err)
		}
		recovery, err = m.loadDeployment(recovery.ID)
		if err != nil {
			return err
		}
	}
	if op.SnapshotPath != "" {
		if err = m.restoreSnapshot(&recovery, *op); err != nil {
			return fmt.Errorf("rollback state restoration failed")
		}
	}
	if recovery.Status == "starting" || recovery.Status == "ready" {
		b, e := m.backend(recovery)
		if e != nil {
			return e
		}
		req, e := m.request(recovery, work, "inspect")
		if e != nil {
			return e
		}
		observed, e := b.Inspect(req)
		if e != nil {
			return fmt.Errorf("rollback start outcome remains unknown; another start refused: %w", e)
		}
		if _, e = runtimeMergeExecution(recovery.Execution, observed); e != nil {
			return e
		}
		switch {
		case observed.State == "running":
			err = m.waitReady(&recovery, work)
		case observed.State == "prepared":
			// The backend confirms the failed start never produced a writer.
			err = m.startDeployment(&recovery, work)
		case observed.State == "stopped" && observed.StoppedConfirmed:
			// Terminal executions need a fresh generation, with their latest state.
			recovery.Status = "stopped"
			recovery.Execution = observed
			if err = m.saveDeployment(recovery); err != nil {
				return err
			}
			if err = m.captureStateSnapshot(recovery, op); err != nil {
				return fmt.Errorf("rollback recovery state could not be retained: %w", err)
			}
			op.RollbackID = ""
			op.DeploymentID = candidate.ID
			if err = m.saveOperation(*op); err != nil {
				return err
			}
			return m.rollbackRemoteDeployment(op, old, candidate, cause)
		default:
			return fmt.Errorf("rollback start outcome remains unconfirmed; another start refused")
		}
		if err != nil {
			return fmt.Errorf("activation failed; previous deployment readiness failed: %w", err)
		}
	} else if err = m.startDeployment(&recovery, work); err != nil {
		return fmt.Errorf("activation failed; previous deployment readiness failed: %w", err)
	}
	if err = m.activateAndConfirm(recovery, *op); err != nil {
		return fmt.Errorf("previous deployment could not resume admission")
	}
	if err = m.publishIngress(recovery, work); err != nil {
		return fmt.Errorf("previous deployment ingress could not resume: %w", err)
	}
	if err = m.commitActive(recovery); err != nil {
		return err
	}
	if err = m.phase(op, "rolling_back"); err != nil {
		return err
	}
	m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, candidate.ID)
	return fmt.Errorf("%w; previous deployment restored and verified; database changes are not automatically undone", cause)
}
