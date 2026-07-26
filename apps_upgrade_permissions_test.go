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
	req.Header.Set("X-User-ID", "1")
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
	var version, installManifestJSON string
	if err := s.store.db.QueryRow(`SELECT version, manifest_json FROM app_installs WHERE id=?`, installID).Scan(&version, &installManifestJSON); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != "0.1.0" {
		t.Fatalf("upgrade should not bump version after permission conflict, got %q", version)
	}
	var installManifest sdk.Manifest
	_ = json.Unmarshal([]byte(installManifestJSON), &installManifest)
	if installManifest.Version != "0.1.0" {
		t.Fatalf("permission conflict changed running manifest to %q", installManifest.Version)
	}
}

func TestUpgradeApp_ApprovesNewPermissionsAndUpgrades(t *testing.T) {
	s, installID := seedBuiltinUpgradePermissionFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/1/upgrade", strings.NewReader(`{"approve_new_permissions":true}`))
	req.Header.Set("X-User-ID", "1")
	req.URL.Path = "/apps/installs/" + itoa(installID) + "/upgrade"
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var version, rawPermissions, installManifestJSON string
	if err := s.store.db.QueryRow(`SELECT version, permissions_json, manifest_json FROM app_installs WHERE id=?`, installID).Scan(&version, &rawPermissions, &installManifestJSON); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if version != "0.2.0" {
		t.Fatalf("upgrade should bump version after approval, got %q", version)
	}
	var installManifest sdk.Manifest
	_ = json.Unmarshal([]byte(installManifestJSON), &installManifest)
	if installManifest.Version != "0.2.0" {
		t.Fatalf("approved upgrade manifest = %q, want 0.2.0", installManifest.Version)
	}
	if !strings.Contains(rawPermissions, string(sdk.PermDBWriteApp)) ||
		!strings.Contains(rawPermissions, string(sdk.PermIngressWrite)) {
		t.Fatalf("approved permissions should include old and new permissions, got %s", rawPermissions)
	}
}

func TestUpgradeApp_PrunesPermissionsRemovedFromManifest(t *testing.T) {
	s, installID := seedBuiltinUpgradePermissionFixture(t)
	var appID int64
	if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, installID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	available := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "deploy", DisplayName: "Deploy", Version: "0.2.0",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermDBWriteApp}},
	}
	availableJSON, _ := json.Marshal(available)
	old := available
	old.Version = "0.1.0"
	old.Requires.Permissions = []sdk.Permission{sdk.PermDBWriteApp, sdk.PermConnectionsReadCredentials}
	oldJSON, _ := json.Marshal(old)
	oldPermissions, _ := json.Marshal(old.Requires.Permissions)
	if _, err := s.store.db.Exec(`UPDATE apps SET manifest_json=? WHERE id=?`, string(availableJSON), appID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET manifest_json=?, permissions_json=? WHERE id=?`,
		string(oldJSON), string(oldPermissions), installID,
	); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/"+itoa(installID)+"/upgrade", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rawPermissions string
	if err := s.store.db.QueryRow(`SELECT permissions_json FROM app_installs WHERE id=?`, installID).Scan(&rawPermissions); err != nil {
		t.Fatal(err)
	}
	var got []sdk.Permission
	if err := json.Unmarshal([]byte(rawPermissions), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != sdk.PermDBWriteApp {
		t.Fatalf("permissions after upgrade=%v, want only %s", got, sdk.PermDBWriteApp)
	}
}

func TestUpgradeApp_PrunesBindingsRemovedFromManifest(t *testing.T) {
	s, installID := seedBuiltinUpgradePermissionFixture(t)
	var appID int64
	if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, installID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	available := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "deploy", DisplayName: "Deploy", Version: "0.2.0",
		Requires: sdk.Requires{
			Permissions:  []sdk.Permission{sdk.PermDBWriteApp},
			Integrations: []sdk.IntegrationDep{{Role: "domains"}},
		},
	}
	old := available
	old.Version = "0.1.0"
	old.Requires.Integrations = []sdk.IntegrationDep{
		{Role: "domains"},
		{Role: "certs"},
		{Role: "routes"},
	}
	availableJSON, _ := json.Marshal(available)
	oldJSON, _ := json.Marshal(old)
	if _, err := s.store.db.Exec(`UPDATE apps SET manifest_json=? WHERE id=?`, string(availableJSON), appID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs
		    SET manifest_json=?, permissions_json='["db.write.app"]',
		        integration_bindings='{"domains":31,"certs":32,"routes":33}'
		  WHERE id=?`,
		string(oldJSON), installID,
	); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/"+itoa(installID)+"/upgrade", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := readBindings(t, s, installID)
	if len(got) != 1 {
		t.Fatalf("obsolete bindings survived upgrade: %v", got)
	}
	if value, _ := asInt64(got["domains"]); value != 31 {
		t.Fatalf("valid domains binding changed: %v", got)
	}
}

func seedBuiltinUpgradePermissionFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
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
	oldManifest := manifest
	oldManifest.Version = "0.1.0"
	oldManifest.Requires.Permissions = []sdk.Permission{sdk.PermDBWriteApp}
	oldManifestJSON, _ := json.Marshal(oldManifest)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version, manifest_json, source, permissions_json)
		 VALUES (?, '', 'running', '0.1.0', ?, 'builtin', ?)`,
		appID, string(oldManifestJSON), string(approved),
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	return s, installID
}
