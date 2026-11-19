package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestInstallDeprecatedAppIsBlocked(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)

	body := map[string]any{
		"project_id": "proj-1",
		"manifest_yaml": `
schema: apteva-app/v1
name: certs
display_name: Certs
version: 0.1.0
provides:
  http_routes: []
  mcp_tools: []
runtime:
  kind: source
  port: 8080
  source:
    repo: github.com/apteva/apps
    ref: main
    entry: mcp/certs
`,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/apps/install", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()

	s.handleInstallApp(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deprecated") {
		t.Fatalf("expected deprecated response, got %s", rec.Body.String())
	}
}

func TestUpgradeDeprecatedAppIsBlocked(t *testing.T) {
	// Existing Certs installs stay upgradeable while the legacy certificate
	// fallback is on; with it off, Certs is blocked like any retired app.
	t.Setenv("APTEVA_LEGACY_CERTS_FALLBACK", "false")
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	manifestJSON := `{
		"schema":"apteva-app/v1",
		"name":"certs",
		"display_name":"Certs",
		"version":"0.2.0"
	}`
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('certs', 'builtin', '', '', ?)`,
		manifestJSON,
	); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name='certs'`).Scan(&appID); err != nil {
		t.Fatalf("select app: %v", err)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version, permissions_json) VALUES (?, '', 'running', '0.1.0', '[]')`,
		appID,
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	req := httptest.NewRequest(http.MethodPost, "/apps/installs/1/upgrade", nil)
	req.Header.Set("X-User-ID", "1")
	req.URL.Path = "/apps/installs/" + itoa(installID) + "/upgrade"
	rec := httptest.NewRecorder()

	s.handleUpgradeApp(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deprecated") {
		t.Fatalf("expected deprecated response, got %s", rec.Body.String())
	}
}

func TestExistingCertsCanUpgradeDuringLegacyFallbackMigration(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	manifestJSON := `{
		"schema":"apteva-app/v1",
		"name":"certs",
		"display_name":"Certs",
		"version":"0.5.3"
	}`
	res, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('certs', 'builtin', '', '', ?)`,
		manifestJSON,
	)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version, source, permissions_json)
		 VALUES (?, '', 'running', '0.5.2', 'builtin', '[]')`,
		appID,
	)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := res.LastInsertId()

	req := httptest.NewRequest(http.MethodPost, "/apps/installs/"+itoa(installID)+"/upgrade", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("migration-window upgrade status=%d body=%s", rec.Code, rec.Body.String())
	}

	t.Setenv("APTEVA_LEGACY_CERTS_FALLBACK", "false")
	if _, err := s.store.db.Exec(`UPDATE app_installs SET version='0.5.2' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleUpgradeApp(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("disabled-fallback upgrade status=%d, want 410: %s", rec.Code, rec.Body.String())
	}
}

// Hosting was retired once and has since been rebuilt; installs and
// dependency installs must reach the steps after the retired-app check.
// Both manifests stop at the scope check that follows it, so nothing is
// fetched or started.
func TestHostingIsNotRetired(t *testing.T) {
	if _, retired := deprecatedApp("hosting"); retired {
		t.Fatal("hosting is still listed as retired")
	}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	raw, _ := json.Marshal(map[string]any{
		"project_id": "proj-1",
		"manifest_yaml": `
schema: apteva-app/v1
name: hosting
display_name: Hosting
version: 2.3.0
scopes: [global]
runtime:
  kind: source
  port: 8080
  source:
    repo: github.com/apteva/apps
    ref: main
    entry: mcp/hosting
`,
	})
	req := httptest.NewRequest(http.MethodPost, "/apps/install", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleInstallApp(rec, req)
	if rec.Code == http.StatusGone || strings.Contains(rec.Body.String(), "deprecated") {
		t.Fatalf("hosting install refused as retired: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not support scope") {
		t.Fatalf("install did not reach the scope check: status=%d body=%s", rec.Code, rec.Body.String())
	}

	_, err := s.installAppFromManifest(1, &sdk.Manifest{Name: "hosting", Version: "2.3.0"}, "proj-1")
	if err == nil || strings.Contains(err.Error(), "deprecated") || !strings.Contains(err.Error(), "dependency scope") {
		t.Fatalf("dependency install did not reach the scope check: %v", err)
	}
}
