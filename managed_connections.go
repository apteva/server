package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

const managedConnectionBodyLimit = 1 << 20

func (s *Server) handleCallbackManagedConnectionEnsure(w http.ResponseWriter, r *http.Request) {
	installID, userID, installProject, ok := s.authorizeManagedConnectionMutation(w, r)
	if !ok {
		return
	}
	var body sdk.ManagedConnectionRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedConnectionBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Key = strings.TrimSpace(body.Key)
	body.AppSlug = strings.TrimSpace(body.AppSlug)
	body.Name = strings.TrimSpace(body.Name)
	body.AuthType = strings.TrimSpace(body.AuthType)
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	if body.Key == "" || len(body.Key) > 200 {
		http.Error(w, "idempotency_key is required and must be at most 200 characters", http.StatusBadRequest)
		return
	}
	if body.AppSlug == "" || body.Name == "" {
		http.Error(w, "app_slug and name are required", http.StatusBadRequest)
		return
	}
	if len(body.Fields) == 0 || len(body.Fields) > 128 {
		http.Error(w, "fields must contain between 1 and 128 entries", http.StatusBadRequest)
		return
	}

	projectID, err := s.managedConnectionProject(userID, installProject, body.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if s.catalog == nil {
		http.Error(w, "integration catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	app := s.catalog.Get(body.AppSlug)
	if app == nil {
		http.Error(w, "integration app not found in catalog", http.StatusNotFound)
		return
	}
	if body.AuthType == "" {
		if len(app.Auth.Types) > 0 {
			body.AuthType = app.Auth.Types[0]
		} else {
			body.AuthType = "api_key"
		}
	}
	fields, err := materializeGeneratedConnectionCredentials(app, body.Fields)
	if err != nil {
		http.Error(w, "credential generation failed", http.StatusInternalServerError)
		return
	}
	fields = applyCredentialFieldDefaults(app, fields)
	encrypted, err := encryptManagedConnectionFields(s.secret, fields)
	if err != nil {
		http.Error(w, "credential encryption failed", http.StatusInternalServerError)
		return
	}

	conn, created, err := s.ensureManagedConnection(ConnectionInput{
		UserID:                 userID,
		AppSlug:                body.AppSlug,
		AppName:                app.Name,
		Name:                   body.Name,
		AuthType:               body.AuthType,
		EncryptedCreds:         encrypted,
		ProjectID:              projectID,
		Source:                 "local",
		Status:                 "active",
		CreatedVia:             "app_install",
		OwnerAppInstallID:      installID,
		CredentialManagement:   "app",
		CredentialExportPolicy: string(sdk.ExportNever),
		ManagedKey:             body.Key,
	})
	if err != nil {
		if errors.Is(err, errManagedConnectionConflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "ensure managed connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	action := "ensured"
	if created {
		action = "created"
	}
	s.recordManagedConnectionEvent(conn, installID, action)
	writeJSON(w, managedPlatformConnection(conn))
}

func (s *Server) handleCallbackManagedConnectionRotate(w http.ResponseWriter, r *http.Request, idStr string) {
	installID, userID, _, ok := s.authorizeManagedConnectionMutation(w, r)
	if !ok {
		return
	}
	connID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if !isOwnedManagedConnection(conn, installID) {
		http.Error(w, "managed connection not owned by this app", http.StatusForbidden)
		return
	}
	var body sdk.ManagedConnectionRotation
	r.Body = http.MaxBytesReader(w, r.Body, managedConnectionBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Fields) == 0 || len(body.Fields) > 128 {
		http.Error(w, "fields must contain between 1 and 128 entries", http.StatusBadRequest)
		return
	}
	if s.catalog == nil {
		http.Error(w, "integration catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "integration app not found in catalog", http.StatusNotFound)
		return
	}
	fields, err := materializeGeneratedConnectionCredentials(app, body.Fields)
	if err != nil {
		http.Error(w, "credential generation failed", http.StatusInternalServerError)
		return
	}
	fields = applyCredentialFieldDefaults(app, fields)
	encrypted, err := encryptManagedConnectionFields(s.secret, fields)
	if err != nil {
		http.Error(w, "credential encryption failed", http.StatusInternalServerError)
		return
	}
	result, err := s.store.db.Exec(`UPDATE connections
		SET encrypted_credentials=?, status='active'
		WHERE id=? AND user_id=? AND owner_app_install_id=?
		  AND credential_management='app' AND credential_export_policy='never'`,
		encrypted, connID, userID, installID)
	if err != nil {
		http.Error(w, "rotate managed connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		http.Error(w, "managed connection not owned by this app", http.StatusForbidden)
		return
	}
	conn.Status = "active"
	s.recordManagedConnectionEvent(conn, installID, "rotated")
	writeJSON(w, managedPlatformConnection(conn))
}

func (s *Server) handleCallbackManagedConnectionRevoke(w http.ResponseWriter, r *http.Request, idStr string) {
	installID, userID, _, ok := s.authorizeManagedConnectionMutation(w, r)
	if !ok {
		return
	}
	connID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || connID <= 0 {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if !isOwnedManagedConnection(conn, installID) {
		http.Error(w, "managed connection not owned by this app", http.StatusForbidden)
		return
	}
	result, err := s.store.db.Exec(`DELETE FROM connections
		WHERE id=? AND user_id=? AND owner_app_install_id=?
		  AND credential_management='app' AND credential_export_policy='never'`,
		connID, userID, installID)
	if err != nil {
		http.Error(w, "revoke managed connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		http.Error(w, "managed connection not owned by this app", http.StatusForbidden)
		return
	}
	s.recordManagedConnectionEvent(conn, installID, "revoked")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizeManagedConnectionMutation(w http.ResponseWriter, r *http.Request) (installID, userID int64, installProject string, ok bool) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return 0, 0, "", false
	}
	if !installHasPermission(s, installID, sdk.PermConnectionsManageOwnedCredentials) {
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsManageOwnedCredentials), http.StatusForbidden)
		return 0, 0, "", false
	}
	if err := s.store.db.QueryRow(`SELECT COALESCE(installed_by,0), COALESCE(project_id,'')
		FROM app_installs WHERE id=? AND status='running'`, installID).Scan(&userID, &installProject); err != nil || userID <= 0 {
		http.Error(w, "running app installation not found", http.StatusForbidden)
		return 0, 0, "", false
	}
	if requestUserID := getUserID(r); requestUserID != 0 && requestUserID != userID {
		http.Error(w, "app installation owner mismatch", http.StatusForbidden)
		return 0, 0, "", false
	}
	return installID, userID, installProject, true
}

func (s *Server) managedConnectionProject(userID int64, installProject, requested string) (string, error) {
	if installProject != "" {
		if requested != "" && requested != installProject {
			return "", errors.New("managed connection project must match the app installation project")
		}
		return installProject, nil
	}
	if requested == "" {
		return "", nil
	}
	role, err := s.store.GetProjectRole(requested, userID)
	if err != nil || role.Rank() < ProjectEditor.Rank() {
		return "", errors.New("app owner cannot edit the requested project")
	}
	return requested, nil
}

var errManagedConnectionConflict = errors.New("managed connection idempotency key conflicts with an existing connection")

func (s *Server) ensureManagedConnection(in ConnectionInput) (*Connection, bool, error) {
	existing, _, err := s.store.getManagedConnection(in.OwnerAppInstallID, in.ManagedKey)
	if err == nil {
		if existing.UserID != in.UserID || existing.AppSlug != in.AppSlug || existing.ProjectID != in.ProjectID || existing.CredentialManagement != "app" || existing.CredentialExportPolicy != "never" {
			return nil, false, errManagedConnectionConflict
		}
		_, err = s.store.db.Exec(`UPDATE connections SET name=?, auth_type=?, encrypted_credentials=?, status='active', app_name=?
			WHERE id=? AND owner_app_install_id=? AND managed_key=?`,
			in.Name, in.AuthType, in.EncryptedCreds, in.AppName, existing.ID, in.OwnerAppInstallID, in.ManagedKey)
		if err != nil {
			return nil, false, err
		}
		return s.reloadManagedConnection(in.OwnerAppInstallID, in.ManagedKey, false)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	conn, err := s.store.CreateConnectionExt(in)
	if err == nil {
		return conn, true, nil
	}
	// A concurrent retry may have won the unique managed-key insert. Resolve
	// the durable identity once before returning the database error.
	if raced, _, lookupErr := s.store.getManagedConnection(in.OwnerAppInstallID, in.ManagedKey); lookupErr == nil {
		if raced.UserID != in.UserID || raced.AppSlug != in.AppSlug || raced.ProjectID != in.ProjectID {
			return nil, false, errManagedConnectionConflict
		}
		return raced, false, nil
	}
	return nil, false, err
}

func (s *Server) reloadManagedConnection(ownerID int64, key string, created bool) (*Connection, bool, error) {
	conn, _, err := s.store.getManagedConnection(ownerID, key)
	return conn, created, err
}

func (s *Store) getManagedConnection(ownerID int64, key string) (*Connection, string, error) {
	var userID, connID int64
	err := s.db.QueryRow(`SELECT user_id, id FROM connections WHERE owner_app_install_id=? AND managed_key=?`, ownerID, key).Scan(&userID, &connID)
	if err != nil {
		return nil, "", err
	}
	return s.GetConnection(userID, connID)
}

func encryptManagedConnectionFields(secret []byte, fields map[string]string) (string, error) {
	serialized, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return Encrypt(secret, string(serialized))
}

func isOwnedManagedConnection(conn *Connection, installID int64) bool {
	return conn != nil && conn.OwnerAppInstallID == installID && conn.CredentialManagement == "app" && conn.CredentialExportPolicy == "never" && conn.ManagedKey != ""
}

func managedPlatformConnection(conn *Connection) sdk.PlatformConnection {
	out := sdk.PlatformConnection{
		ID: conn.ID, AppSlug: conn.AppSlug, Name: conn.Name,
		Status: conn.Status, ProjectID: conn.ProjectID,
	}
	if conn.CredentialManagement == "app" {
		out.CredentialManagement = conn.CredentialManagement
		out.ExportPolicy = sdk.ConnectionExportPolicy(conn.CredentialExportPolicy)
	}
	return out
}

func (s *Server) recordManagedConnectionEvent(conn *Connection, installID int64, action string) {
	if conn == nil {
		return
	}
	_, _ = s.store.db.Exec(`INSERT INTO managed_connection_events
		(connection_id, owner_app_install_id, action, app_slug, project_id, managed_key)
		VALUES (?, ?, ?, ?, ?, ?)`, conn.ID, installID, action, conn.AppSlug, conn.ProjectID, conn.ManagedKey)
}

func denyNonExportableConnection(w http.ResponseWriter, conn *Connection) bool {
	if conn != nil && conn.CredentialExportPolicy == string(sdk.ExportNever) {
		http.Error(w, fmt.Sprintf("connection %d credentials are non-exportable", conn.ID), http.StatusForbidden)
		return true
	}
	return false
}
