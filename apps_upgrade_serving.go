package main

import (
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Deployment progress and serving availability are separate. The database
// keeps the committed instance running; pending_manifest_json tracks the
// replacement. The list API still presents pending to existing polling UIs.
func (s *Server) stageAppUpgrade(id int64, next *sdk.Manifest, declarations ...appComponentDeclarations) error {
	if err := validateAppRuntimeCompatibility(next, Version); err != nil {
		return err
	}
	status := "pending"
	if s.runtimeManaged(id) {
		if active, e := s.appRuntimeManager().active(id); e == nil && active.Status == "active" {
			status = "running"
		}
	}
	if s.localApps != nil {
		old := s.localApps.currentProc(id)
		_, err := fixedRuntimePorts(next)
		if err != nil {
			return err
		}
		// Even exclusive updates can serve OLD throughout download/build.
		// beforeExclusiveStop persists unavailability at the actual cutover.
		if old != nil && s.localApps.verifyCurrentProc(id, 5*time.Second) {
			status = "running"
		}
	}
	pending, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var components appComponentDeclarations
	if len(declarations) > 0 {
		components = declarations[0]
	}
	if err = saveAppComponentDeclarations(tx, id, next.Version, components); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE app_installs SET status=?,status_message='Upgrading…',error_message='',pending_manifest_json=? WHERE id=?`, status, string(pending), id)
	if err != nil {
		return fmt.Errorf("stage app upgrade: %w", err)
	}
	return tx.Commit()
}

func (s *Server) markExclusiveActivationUnavailable(id int64) error {
	result, err := s.store.db.Exec(`UPDATE app_installs SET status='pending',status_message='Updating — temporarily unavailable' WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("record exclusive cutover: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("record exclusive cutover: installation %d no longer exists", id)
	}
	// Remove the stopped route from live routing and dependency resolution.
	if s.installedApps != nil {
		s.installedApps.Remove(id)
	}
	return nil
}

func (s *Server) recordExclusiveActivationPID(id int64, pid int) error {
	// Keep the committed binary, port and manifest until readiness succeeds.
	// Recovery kills this PID, then reconstructs OLD with that committed state.
	if pid <= 0 {
		return fmt.Errorf("exclusive candidate for installation %d has exited", id)
	}
	result, err := s.store.db.Exec(`UPDATE app_installs SET local_pid=? WHERE id=?`, pid, id)
	if err != nil {
		return fmt.Errorf("record exclusive candidate pid: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("record exclusive candidate pid: installation %d no longer exists", id)
	}
	return nil
}
