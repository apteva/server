package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// handleCallbackPlatformBackup exposes only the two explicitly permissioned
// backup operations to app-install tokens. Ordinary /api/platform routes stay
// outside appTokenRouteAllowed and remain user/admin-only.
func (s *Server) handleCallbackPlatformBackup(w http.ResponseWriter, r *http.Request, parts []string) {
	operation := "unknown"
	if len(parts) == 1 {
		operation = strings.TrimSpace(parts[0])
	}
	installID, _ := requireInstallID(r)
	audit := &platformBackupAuditWriter{ResponseWriter: w}
	started := time.Now()
	defer func() {
		outcome := "completed"
		if audit.statusCode() >= 400 {
			outcome = "denied_or_failed"
		}
		log.Printf("[PLATFORM-BACKUP] install_id=%d operation=%s outcome=%s http_status=%d elapsed=%s",
			installID, operation, outcome, audit.statusCode(), time.Since(started).Round(time.Millisecond))
	}()

	if len(parts) != 1 || (operation != "snapshot" && operation != "restore") {
		http.Error(audit, "unknown platform backup callback", http.StatusNotFound)
		return
	}
	permission := sdk.PermPlatformBackupRead
	wantMethod := http.MethodGet
	if operation == "restore" {
		permission = sdk.PermPlatformBackupRestore
		wantMethod = http.MethodPost
	}
	if r.Method != wantMethod {
		http.Error(audit, wantMethod+" only", http.StatusMethodNotAllowed)
		return
	}
	if err := s.authorizePlatformBackupInstall(installID, permission); err != nil {
		http.Error(audit, err.Error(), http.StatusForbidden)
		return
	}
	if operation == "restore" && r.Header.Get("X-Confirm-Restore") != "yes" {
		http.Error(audit, "missing X-Confirm-Restore: yes — restore is destructive, confirmation required", http.StatusBadRequest)
		return
	}
	if operation == "snapshot" {
		s.writePlatformSnapshot(audit, r)
		return
	}
	s.restorePlatformSnapshot(audit, r)
}

func (s *Server) authorizePlatformBackupInstall(installID int64, permission sdk.Permission) error {
	if installID <= 0 {
		return fmt.Errorf("valid app installation required")
	}
	var projectID, status string
	var installedBy int64
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(project_id,''), COALESCE(installed_by,0), status FROM app_installs WHERE id=?`,
		installID,
	).Scan(&projectID, &installedBy, &status); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("app installation not found")
		}
		return fmt.Errorf("read app installation: %w", err)
	}
	if status != "running" {
		return fmt.Errorf("app installation must be active")
	}
	if projectID != "" {
		return fmt.Errorf("platform backups require a global app installation")
	}
	if installedBy <= 0 || !s.isAdmin(installedBy) {
		return fmt.Errorf("platform backups require an installation owned by an administrator")
	}
	if !installHasPermission(s, installID, permission) {
		return fmt.Errorf("app installation lacks approved permission %s", permission)
	}
	return nil
}

type platformBackupAuditWriter struct {
	http.ResponseWriter
	status int
}

func (w *platformBackupAuditWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *platformBackupAuditWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (w *platformBackupAuditWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
