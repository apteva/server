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
func (s *Server) stageAppUpgrade(id int64, next *sdk.Manifest) error {
	status := "pending"
	if s.localApps != nil {
		old := s.localApps.currentProc(id)
		ports, err := fixedRuntimePorts(next)
		if err != nil {
			return err
		}
		// Fixed-port apps cannot overlap. Never advertise them as continuously
		// available when activation will have to stop the old listener first.
		if old != nil && len(old.spec.fixedPorts) == 0 && len(ports) == 0 && s.localApps.verifyCurrentProc(id, 5*time.Second) {
			status = "running"
		}
	}
	pending, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = s.store.db.Exec(`UPDATE app_installs SET status=?,status_message='Upgrading…',error_message='',pending_manifest_json=? WHERE id=?`, status, string(pending), id)
	if err != nil {
		return fmt.Errorf("stage app upgrade: %w", err)
	}
	return nil
}
