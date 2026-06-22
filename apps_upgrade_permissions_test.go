package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestUpgradeApp_BlocksWhenManifestAddsRequiredPermission(t *testing.T) {
	s, installID := seedBuiltinUpgradePermissionFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/1/upgrade", nil)
	req.URL.Path = "/apps/installs/" + itoa(installID) + "/upgrade"
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new permissions required") ||
		!strings.Contains(rec.Body.String(), string(sdk.PermIngressWrite)) {
		t.Fatalf("response should name missing permission, got: %s", rec.Body.String())
	}
	var version string
	if err := s.store.db.QueryRow(`SELECT version FROM app_installs WHERE id=?`, installID).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != "0.1.0" {
		t.Fatalf("upgrade should not bump version after permission conflict, got %q", version)
	}
}

func TestUpgradeApp_ApprovesNewPermissionsAndUpgrades(t *testing.T) {
	s, installID := seedBuiltinUpgradePermissionFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/1/upgrade", strings.NewReader(`{"approve_new_permissions":true}`))
	req.URL.Path = "/apps/installs/" + itoa(installID) + "/upgrade"
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var version, rawPermissions string
	if err := s.store.db.QueryRow(`SELECT version, permissions_json FROM app_installs WHERE id=?`, installID).Scan(&version, &rawPermissions); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if version != "0.2.0" {
		t.Fatalf("upgrade should bump version after approval, got %q", version)
	}
	if !strings.Contains(rawPermissions, string(sdk.PermDBWriteApp)) ||
		!strings.Contains(rawPermissions, string(sdk.PermIngressWrite)) {
		t.Fatalf("approved permissions should include old and new permissions, got %s", rawPermissions)
	}
}

func seedBuiltinUpgradePermissionFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema:      sdk.SchemaCurrent,
		Name:        "deploy",
		DisplayName: "Deploy",
		Version:     "0.2.0",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{
				sdk.PermDBWriteApp,
				sdk.PermIngressWrite,
			},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('deploy', 'builtin', '', '', ?)`,
		string(manifestJSON),
	); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name='deploy'`).Scan(&appID); err != nil {
		t.Fatalf("select app: %v", err)
	}
	approved, _ := json.Marshal([]sdk.Permission{sdk.PermDBWriteApp})
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version, permissions_json)
		 VALUES (?, '', 'running', '0.1.0', ?)`,
		appID, string(approved),
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	return s, installID
}
