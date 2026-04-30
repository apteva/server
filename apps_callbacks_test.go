package main

// Tests for the integration-binding + app-call authorization checks.
// The actual integration-execute downstream (decrypt + HTTP call to
// upstream) is covered by the existing /connections/:id/execute
// tests; here we only exercise the new auth surface.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// seedInstallWithBindings inserts an apps row + an app_installs row
// with the given manifest and bindings. Returns the install id.
func seedInstallWithBindings(t *testing.T, s *Server, appName string, manifest sdk.Manifest, bindings map[string]any) int64 {
	t.Helper()
	mj, _ := json.Marshal(manifest)
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		appName, string(mj),
	); err != nil {
		t.Fatalf("insert apps: %v", err)
	}
	var appID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name=?`, appName).Scan(&appID)
	bj, _ := json.Marshal(bindings)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings)
		 VALUES (?, ?, 'running', 1, ?)`,
		appID, "proj-1", string(bj),
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	return id
}

// --- /integrations/:connID/execute auth checks ----------------------

func TestCallback_IntegrationExecute_RequiresInstallToken(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/apps/callback/integrations/42/execute",
		strings.NewReader(`{"tool":"x","input":{}}`))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCallback_IntegrationExecute_RejectsUnboundConnection(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "x",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsExecute},
			Integrations: []sdk.IntegrationDep{
				{Role: "provider", Kind: "integration", CompatibleSlugs: []string{"openai-api"}},
			},
		},
	}
	// Bound to connection 99, NOT 42.
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"provider": 99})

	req := httptest.NewRequest("POST", "/apps/callback/integrations/42/execute",
		strings.NewReader(`{"tool":"generate_image","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unbound conn, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCallback_IntegrationExecute_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	// Manifest declares the dep but NOT the permission.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "x",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "provider", Kind: "integration", CompatibleSlugs: []string{"openai-api"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"provider": 42})
	req := httptest.NewRequest("POST", "/apps/callback/integrations/42/execute",
		strings.NewReader(`{"tool":"generate_image","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing permission, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "platform.connections.execute") {
		t.Errorf("error message should name the missing permission: %s", rec.Body.String())
	}
}

// --- /apps/:appName/call auth checks --------------------------------

func TestCallback_AppCall_RejectsUnboundApp(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "image-studio",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Integrations: []sdk.IntegrationDep{
				{Role: "storage", Kind: "app", CompatibleAppNames: []string{"storage"}},
			},
		},
	}
	// No binding for "storage" — operator declined.
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{})

	req := httptest.NewRequest("POST", "/apps/callback/apps/storage/call",
		strings.NewReader(`{"tool":"files_upload","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unbound app, got %d", rec.Code)
	}
}

func TestCallback_AppCall_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "image-studio",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "storage", Kind: "app", CompatibleAppNames: []string{"storage"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"storage": 17})
	req := httptest.NewRequest("POST", "/apps/callback/apps/storage/call",
		strings.NewReader(`{"tool":"files_upload","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 missing permission, got %d", rec.Code)
	}
}

// --- /whoami includes bindings -------------------------------------

func TestCallback_Whoami_ReturnsBindings(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, map[string]any{
		"provider": float64(42),
		"storage":  float64(17),
	})
	req := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out sdk.InstallIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.InstallID != installID {
		t.Errorf("install_id = %d, want %d", out.InstallID, installID)
	}
	if got := out.Bindings["provider"]; got == nil {
		t.Errorf("bindings.provider missing")
	}
}

// --- helpers --------------------------------------------------------

// installBoundConnection / installBoundApp / etc. are exercised
// indirectly by the auth-failure tests above. A direct helper test:

func TestInstallBoundConnection_Match(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, map[string]any{"provider": float64(42)})
	role, ok := installBoundConnection(s, installID, 42)
	if !ok || role != "provider" {
		t.Fatalf("expected role=provider, got role=%q ok=%v", role, ok)
	}
	_, ok = installBoundConnection(s, installID, 999)
	if ok {
		t.Fatal("expected miss for unbound connection id")
	}
}
