package main

import (
	"errors"
	"fmt"
)

var errRuntimeCancelled = errors.New("deployment operation cancelled")

func (m *appRuntimeManager) cancelOperation(id string, installID int64) error {
	op, err := m.operation(id)
	if err != nil || op.InstallID != installID {
		return fmt.Errorf("operation not found")
	}
	if op.Status != "queued" && op.Status != "running" {
		return fmt.Errorf("operation already finished")
	}
	if op.Phase == "committing" || op.Phase == "ownership_committed" || op.Phase == "publishing" || op.Phase == "retiring_previous" || runtimeRollingBack(op) {
		return fmt.Errorf("cutover already requires completion; cancellation refused")
	}
	_, err = m.s.store.db.Exec(`UPDATE app_runtime_operations SET cancel_requested=1 WHERE id=?`, id)
	return err
}
func (m *appRuntimeManager) discardOperation(op *runtimeOperation) error {
	d, err := m.loadDeployment(op.DeploymentID)
	if err != nil {
		return err
	}
	if d.Status == "starting" || d.Status == "ready" {
		if err = m.stopDeployment(&d, *op); err != nil {
			return err
		}
	}
	if d.Status == "active" {
		return fmt.Errorf("active deployment cannot be discarded")
	}
	if err = m.deleteBackendDeployment(d, *op); err != nil {
		return err
	}
	d.Status = "deleted"
	if err = m.saveDeployment(d); err != nil {
		return err
	}
	if _, err = m.s.store.db.Exec(`DELETE FROM app_runtime_ports WHERE deployment_id=?`, d.ID); err != nil {
		return err
	}
	active, _ := m.active(op.InstallID)
	// The discarded candidate's generation was never published. Return the
	// installation to its published generation so ownership checks, readmission
	// and recovery of the retained execution apply again; new generations are
	// still allocated above every recorded deployment.
	_, err = m.s.store.db.Exec(`UPDATE app_installs SET runtime_pending_deployment='',runtime_placement_json=?,pending_manifest_json='',runtime_generation=COALESCE((SELECT generation FROM app_runtime_deployments WHERE id=app_installs.runtime_active_deployment AND install_id=app_installs.id),runtime_generation) WHERE id=? AND runtime_pending_deployment=?`, runtimeJSON(active.Placement), op.InstallID, d.ID)
	if d.Placement.local() {
		m.s.localApps.DiscardPendingFixedPorts(d.InstallID)
	}
	return err
}
