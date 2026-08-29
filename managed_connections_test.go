package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func managedConnectionRequest(t *testing.T, s *Server, installID int64, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/apps/callback/connections/"+path, bytes.NewReader(raw))
	req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprintf("%d", installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func managedConnectionTestInstall(t *testing.T, s *Server, name string, permissions ...sdk.Permission) int64 {
	t.Helper()
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   name,
		Requires: sdk.Requires{
			Permissions: permissions,
			Integrations: []sdk.IntegrationDep{{
				Role: "provider", Kind: "integration", CompatibleSlugs: []string{"pushover"}, Required: true,
			}},
		},
	}
	return seedInstallWithBindings(t, s, name, manifest, nil)
}

func decodePlatformConnection(t *testing.T, rec *httptest.ResponseRecorder) sdk.PlatformConnection {
	t.Helper()
	var conn sdk.PlatformConnection
	if err := json.Unmarshal(rec.Body.Bytes(), &conn); err != nil {
		t.Fatalf("decode connection: %v body=%s", err, rec.Body.String())
	}
	return conn
}

func TestManagedConnectionsLifecycleIsIdempotentAndNonExportable(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = createTestCatalog(t)
	installID := managedConnectionTestInstall(t, s, "managed-owner",
		sdk.PermConnectionsManageOwnedCredentials,
		sdk.PermConnectionsReadCredentials,
		sdk.PermConnectionsReadPublicConfig,
	)

	ensure := sdk.ManagedConnectionRequest{
		Key: "phone:account-123", AppSlug: "pushover", Name: "Managed provider",
		Fields: map[string]string{"app_token": "secret-one", "user_key": "public-one"},
	}
	rec := managedConnectionRequest(t, s, installID, http.MethodPost, "managed/ensure", ensure)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure status=%d body=%s", rec.Code, rec.Body.String())
	}
	conn := decodePlatformConnection(t, rec)
	if conn.ID <= 0 || conn.ProjectID != "proj-1" || conn.CredentialManagement != "app" || conn.ExportPolicy != sdk.ExportNever {
		t.Fatalf("unexpected managed connection: %+v", conn)
	}

	var ownerID int64
	var management, exportPolicy, managedKey string
	var autoMCP int
	if err := s.store.db.QueryRow(`SELECT owner_app_install_id, credential_management, credential_export_policy, managed_key, auto_mcp
		FROM connections WHERE id=?`, conn.ID).Scan(&ownerID, &management, &exportPolicy, &managedKey, &autoMCP); err != nil {
		t.Fatal(err)
	}
	if ownerID != installID || management != "app" || exportPolicy != "never" || managedKey != ensure.Key || autoMCP != 0 {
		t.Fatalf("stored policy owner=%d management=%q export=%q key=%q auto_mcp=%d", ownerID, management, exportPolicy, managedKey, autoMCP)
	}

	// Repeating the durable key updates the existing row and credential blob.
	ensure.Fields["app_token"] = "secret-two"
	rec = managedConnectionRequest(t, s, installID, http.MethodPost, "managed/ensure", ensure)
	if rec.Code != http.StatusOK {
		t.Fatalf("second ensure status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodePlatformConnection(t, rec); got.ID != conn.ID {
		t.Fatalf("idempotent ensure id=%d want=%d", got.ID, conn.ID)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE owner_app_install_id=? AND managed_key=?`, installID, ensure.Key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("managed rows=%d err=%v", count, err)
	}
	_, encrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil || !strings.Contains(plain, "secret-two") || strings.Contains(plain, "secret-one") {
		t.Fatalf("credential update failed err=%v plaintext=%s", err, plain)
	}

	// Bind the row and prove that even an app with the legacy raw-read
	// permission cannot export a managed secret.
	bindings, _ := json.Marshal(map[string]any{"provider": conn.ID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, bindings, installID); err != nil {
		t.Fatal(err)
	}
	rec = managedConnectionRequest(t, s, installID, http.MethodGet, fmt.Sprintf("%d/credentials", conn.ID), nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "non-exportable") {
		t.Fatalf("app reveal status=%d body=%s", rec.Code, rec.Body.String())
	}
	dashboardReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/connections/%d/credentials", conn.ID), nil)
	dashboardReq.Header.Set("X-User-ID", "1")
	dashboardRec := httptest.NewRecorder()
	s.handleGetConnectionCredentials(dashboardRec, dashboardReq)
	if dashboardRec.Code != http.StatusForbidden || !strings.Contains(dashboardRec.Body.String(), "non-exportable") {
		t.Fatalf("dashboard reveal status=%d body=%s", dashboardRec.Code, dashboardRec.Body.String())
	}

	rotation := sdk.ManagedConnectionRotation{Fields: map[string]string{"app_token": "secret-three", "user_key": "public-three"}}
	rec = managedConnectionRequest(t, s, installID, http.MethodPut, fmt.Sprintf("%d/managed/credentials", conn.ID), rotation)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rec.Code, rec.Body.String())
	}
	_, encrypted, _ = s.store.GetConnection(1, conn.ID)
	plain, _ = Decrypt(s.secret, encrypted)
	if !strings.Contains(plain, "secret-three") {
		t.Fatalf("rotated credential missing: %s", plain)
	}

	otherInstall := managedConnectionTestInstall(t, s, "managed-other", sdk.PermConnectionsManageOwnedCredentials)
	rec = managedConnectionRequest(t, s, otherInstall, http.MethodPut, fmt.Sprintf("%d/managed/credentials", conn.ID), rotation)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-owner rotate status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = managedConnectionRequest(t, s, installID, http.MethodDelete, fmt.Sprintf("%d/managed", conn.ID), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, err := s.store.GetConnection(1, conn.ID); !errorsIsSQLNoRows(err) {
		t.Fatalf("revoked connection still present: %v", err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM managed_connection_events WHERE connection_id=?`, conn.ID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("audit events=%d err=%v", count, err)
	}
}

func TestManagedConnectionsRequirePermissionAndPreserveLegacyDefaults(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = createTestCatalog(t)
	installID := managedConnectionTestInstall(t, s, "managed-denied")
	rec := managedConnectionRequest(t, s, installID, http.MethodPost, "managed/ensure", sdk.ManagedConnectionRequest{
		Key: "denied", AppSlug: "pushover", Name: "Denied", Fields: map[string]string{"app_token": "x"},
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(sdk.PermConnectionsManageOwnedCredentials)) {
		t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
	}

	encrypted, err := encryptManagedConnectionFields(s.secret, map[string]string{"app_token": "legacy-secret"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.store.CreateConnection(1, "pushover", "Pushover", "Legacy", "api_key", encrypted, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, err := s.store.GetConnection(1, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CredentialManagement != "user" || loaded.CredentialExportPolicy != "bound_app" || loaded.ManagedKey != "" {
		t.Fatalf("legacy defaults changed: %+v", loaded)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/connections/%d/credentials", legacy.ID), nil)
	req.Header.Set("X-User-ID", "1")
	dashboardRec := httptest.NewRecorder()
	s.handleGetConnectionCredentials(dashboardRec, req)
	if dashboardRec.Code != http.StatusOK || !strings.Contains(dashboardRec.Body.String(), "legacy-secret") {
		t.Fatalf("legacy reveal status=%d body=%s", dashboardRec.Code, dashboardRec.Body.String())
	}
}

func errorsIsSQLNoRows(err error) bool { return err == sql.ErrNoRows }
