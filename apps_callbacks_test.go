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
	// Bound to connection 99, NOT the conn we'll request.
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"provider": 99})
	// A different conn that's app_install-owned by a different install
	// (so neither bound, nor owned by us, nor operator-installed).
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "openai-api", AppName: "OpenAI", Name: "x",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"generate_image","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unbound conn, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not reachable") {
		t.Errorf("expected 'not reachable' message, got: %s", rec.Body.String())
	}
}

// Operator-installed connections (created_via='integration') are
// reachable by ANY install with platform.connections.execute
// permission. This is the path Social uses to call list_pages on a
// Facebook integration the operator installed in Settings →
// Integrations — without it, the page picker would 403 and disappear.
func TestCallback_IntegrationExecute_AllowsOperatorInstalledConnection(t *testing.T) {
	s := newTestServer(t)
	// Stub the catalog so the handler can find the upstream tool.
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "facebook-api", Name: "Facebook",
		Tools: []AppToolDef{{Name: "list_pages"}},
	})
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	// No bindings — social doesn't pre-declare facebook-api.
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	// Operator-installed integration connection.
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", AppName: "Facebook", Name: "Facebook Pages",
		ProjectID: "proj-1", CreatedVia: "integration",
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	// We don't have a real Facebook to call out to — the auth check
	// should pass and we'll fail later in resolveConnectionContext or
	// the actual upstream HTTP. Anything other than 403/404 means the
	// auth gate let us through, which is what this test asserts.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("operator connection rejected by auth: %s", rec.Body.String())
	}
}

// App-owned connections (owner_app_install_id == calling install) are
// reachable by their owner. Mirrors social's "I created this via
// platform.oauth.start" flow.
func TestCallback_IntegrationExecute_AllowsOwnedConnection(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "facebook-api", Tools: []AppToolDef{{Name: "list_pages"}},
	})
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", Name: "fb",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: installID,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("owned connection rejected: %s", rec.Body.String())
	}
}

// App-owned connection but owner is a DIFFERENT install — must be
// rejected (otherwise apps could read each other's private OAuth
// tokens just by knowing the connection id).
func TestCallback_IntegrationExecute_RejectsCrossAppOwnedConnection(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	// Owned by a DIFFERENT install (id 999).
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", Name: "fb",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-app owned conn, got %d: %s", rec.Code, rec.Body.String())
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
