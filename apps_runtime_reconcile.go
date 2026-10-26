package main

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"time"
)

func (m *appRuntimeManager) reconcile() {
	m.s.store.db.Exec(`DELETE FROM app_runtime_browser_revocations WHERE expires_at<?`, time.Now().Unix())
	rows, err := m.s.store.db.Query(`SELECT id FROM app_runtime_operations WHERE status IN ('queued','running') AND (owner='' OR lease_until<?)`, time.Now().Unix())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		m.launch(id)
	}
	m.queueRuntimeObservations(0, false)
}

func (s *Server) managedRuntimeRestart(id int64) error {
	m := s.appRuntimeManager()
	d, err := m.active(id)
	if err != nil {
		return err
	}
	_, err = m.begin(id, "restart", "restart-"+generateToken(12), d.Placement, nil)
	return err
}
func (m *appRuntimeManager) restartOperation(op *runtimeOperation) error {
	if runtimeRollingBack(*op) {
		return m.activate(op)
	}
	if err := m.prepare(op); err != nil {
		old, e := m.loadDeployment(op.PreviousID)
		candidate, ce := m.loadDeployment(op.DeploymentID)
		if e == nil && ce == nil && old.Status == "stopped" && !op.PreviousStopped {
			return m.rollback(op, &old, &candidate, err)
		}
		return err
	}
	return m.activate(op)
}

func (m *appRuntimeManager) stopSync(id int64, remove bool) error {
	kind := "stop"
	if remove {
		kind = "delete"
	}
	op, err := m.begin(id, kind, kind+"-"+generateToken(12), runtimePlacement{}, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(m.ctx, 6*time.Minute)
	defer cancel()
	for {
		state, e := m.operation(op.ID)
		if e != nil {
			return e
		}
		if state.Status == "succeeded" {
			return nil
		}
		if state.Status == "failed" {
			return fmt.Errorf("managed execution stop failed: %s", state.Error)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("managed stop is still pending; installation retained")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s *Server) runtimeProviderInUse(id int64) bool {
	rows, err := s.store.db.Query(`SELECT record_json FROM app_runtime_deployments WHERE status != 'deleted'`)
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var d runtimeStoredDeployment
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &d) != nil {
			return true
		}
		if !d.Placement.local() && (d.Placement.Backend.InstallID == id || d.Placement.Target.InstallID == id) {
			return true
		}
	}
	return false
}

func (s *Server) managedRuntimeUpgrade(id int64, manifest *sdk.Manifest) error {
	m := s.appRuntimeManager()
	d, err := m.active(id)
	if err != nil {
		return err
	}
	op, err := m.begin(id, "upgrade", "upgrade-"+generateToken(12), d.Placement, manifest)
	if err != nil {
		return err
	}
	return m.waitOperation(op.ID)
}
func (m *appRuntimeManager) waitOperation(ref string) error {
	ctx, cancel := context.WithTimeout(m.ctx, 35*time.Minute)
	defer cancel()
	for {
		op, err := m.operation(ref)
		if err != nil {
			return err
		}
		if op.Status == "succeeded" {
			return nil
		}
		if op.Status == "failed" {
			return fmt.Errorf("%s", op.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *appRuntimeManager) retryOperation(id string, installID int64) error {
	finish, err := m.s.beginHostedWork()
	if err != nil {
		return err
	}
	defer finish()
	op, err := m.operation(id)
	if err != nil || op.InstallID != installID {
		return fmt.Errorf("operation not found")
	}
	if op.Status != "failed" && op.Status != "cancelled" {
		return fmt.Errorf("operation is not retryable")
	}
	var pending, active string
	if err = m.s.store.db.QueryRow(`SELECT runtime_pending_deployment,runtime_active_deployment FROM app_installs WHERE id=?`, installID).Scan(&pending, &active); err != nil {
		return err
	}
	if op.DeploymentID != "" && op.DeploymentID != pending && op.DeploymentID != active && !runtimeRollingBack(op) && op.Kind != "scale" {
		return fmt.Errorf("operation no longer owns a candidate")
	}
	op.Status = "queued"
	op.RetryCount++
	op.LastFailure = op.Error
	op.Error = ""
	op.UpdatedAt = runtimeTimestamp()
	if _, err = m.s.store.db.Exec(`UPDATE app_runtime_operations SET status='queued',owner='',lease_until=0,cancel_requested=0,record_json=? WHERE id=? AND status IN ('failed','cancelled')`, runtimeJSON(runtimeStoredOperation{op, op.SnapshotPath}), id); err != nil {
		return err
	}
	m.launch(id)
	return nil
}
