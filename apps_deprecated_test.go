package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallDeprecatedAppIsBlocked(t *testing.T) {
	s := newTestServer(t)

	body := map[string]any{
		"project_id": "proj-1",
		"manifest_yaml": `
schema: apteva-app/v1
name: routes
display_name: Routes
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
    entry: mcp/routes
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
	s := newTestServer(t)
	manifestJSON := `{
		"schema":"apteva-app/v1",
		"name":"routes",
		"display_name":"Routes",
		"version":"0.2.0"
	}`
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('routes', 'builtin', '', '', ?)`,
		manifestJSON,
	); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name='routes'`).Scan(&appID); err != nil {
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
