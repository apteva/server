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
	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings, permissions_json)
		 VALUES (?, ?, 'running', 1, ?, ?)`,
		appID, "proj-1", string(bj), string(permsJSON),
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	return id
}

// --- /callback/projects ---------------------------------------------

// Project-scoped install — singleton listing of the install's own
// project.
func TestCallback_Projects_ProjectScopedSingleton(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-1', 1, 'p1', 'Project one description')`)
	installID := seedInstall(t, s, "media", "proj-1")
	s.store.db.Exec(`UPDATE app_installs SET installed_by=1 WHERE id=?`, installID)

	req := httptest.NewRequest("GET", "/apps/callback/projects", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected singleton list, got %d", len(out))
	}
	if out[0]["id"] != "proj-1" {
		t.Errorf("got id %v, want proj-1", out[0]["id"])
	}
	if out[0]["description"] != "Project one description" {
		t.Errorf("got description %v, want project description", out[0]["description"])
	}
}

// Global install — every project the install's owner has.
//
// Locks in the column-name fix: the handler must read installed_by,
// not user_id (which doesn't exist on app_installs). Before the fix
// this returned 404 "install not found" in prod because SELECT user_id
// errored on the missing column.
func TestCallback_Projects_GlobalInstallListsOwnerProjects(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-A', 1, 'a', 'alpha context')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-B', 1, 'b', 'beta context')`)
	// Different user's project — must NOT leak.
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (2, 'c@d.e', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name) VALUES ('not-mine', 2, 'theirs')`)

	installID := seedInstall(t, s, "media", "") // global
	s.store.db.Exec(`UPDATE app_installs SET installed_by=1 WHERE id=?`, installID)

	req := httptest.NewRequest("GET", "/apps/callback/projects", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotIDs := map[string]bool{}
	gotDescriptions := map[string]string{}
	for _, p := range out {
		id := p["id"].(string)
		gotIDs[id] = true
		gotDescriptions[id], _ = p["description"].(string)
	}
	if !gotIDs["proj-A"] || !gotIDs["proj-B"] {
		t.Errorf("missing owner projects: got %v", gotIDs)
	}
	if gotDescriptions["proj-A"] != "alpha context" || gotDescriptions["proj-B"] != "beta context" {
		t.Errorf("missing project descriptions: got %v", gotDescriptions)
	}
	if gotIDs["not-mine"] {
		t.Errorf("leaked foreign project")
	}
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

func TestCallback_IntegrationExecute_StripsProjectRoutingInput(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_test"}`))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug:    "anthropic",
		BaseURL: upstream.URL,
		Auth: AppAuthConfig{
			Types:   []string{"api_key"},
			Headers: map[string]string{"x-api-key": "{{api_key}}"},
		},
		Tools: []AppToolDef{{Name: "messages_create", Method: "POST", Path: "/v1/messages"}},
	})

	manifest := integrationOfficialManifest("functions", true)
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermConnectionsExecute}
	installID := seedRunningInstall(t, s, "functions", "", manifest, nil)

	plain, _ := json.Marshal(map[string]string{"api_key": "test-key"})
	encrypted, err := Encrypt(s.secret, string(plain))
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "anthropic", AppName: "Anthropic", Name: "Anthropic",
		AuthType: "api_key", EncryptedCreds: encrypted, ProjectID: "proj-1",
		CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"messages_create","input":{"_project_id":"proj-1","model":"claude-test","messages":[{"role":"user","content":"hello"}]}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamBody["model"] != "claude-test" {
		t.Fatalf("upstream body missing provider input: %#v", upstreamBody)
	}
	if _, leaked := upstreamBody["_project_id"]; leaked {
		t.Fatalf("routing metadata leaked upstream: %#v", upstreamBody)
	}
}

func TestSanitizeIntegrationCallbackInputDoesNotMutateCaller(t *testing.T) {
	input := map[string]any{"_project_id": " proj-1 ", "model": "claude-test"}
	projectID, clean := sanitizeIntegrationCallbackInput(input)
	if projectID != "proj-1" {
		t.Fatalf("projectID=%q", projectID)
	}
	if _, present := clean["_project_id"]; present {
		t.Fatalf("clean input contains routing field: %#v", clean)
	}
	if input["_project_id"] != " proj-1 " {
		t.Fatalf("caller input mutated: %#v", input)
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

// /whoami carries the platform's public_url so apps mint shareable
// URLs without re-reading APTEVA_PUBLIC_URL env (which is frozen at
// sidecar spawn time). Setting changes propagate via the SDK's
// sub-second WhoAmI cache.
func TestCallback_Whoami_ReturnsPublicURL(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.SetSetting("public_url", "https://agents.example.com"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, nil)
	req := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out sdk.InstallIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PublicURL != "https://agents.example.com" {
		t.Errorf("public_url = %q, want https://agents.example.com", out.PublicURL)
	}

	// Live-fresh: change the setting, next whoami call reflects it.
	if err := s.store.SetSetting("public_url", "https://updated.example.com"); err != nil {
		t.Fatalf("update: %v", err)
	}
	req2 := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req2.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req2.Header.Set("X-User-ID", "1")
	rec2 := httptest.NewRecorder()
	s.handleAppCallback(rec2, req2)
	var out2 sdk.InstallIdentity
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if out2.PublicURL != "https://updated.example.com" {
		t.Errorf("after setting change: public_url = %q, want updated", out2.PublicURL)
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

// --- /connections/:id/credentials auth + happy-path -----------------

// seedCredsConnection creates a connection with encrypted credentials
// and returns its id. The slug + creds shape are configurable so
// tests can exercise R2 / S3 / generic.
func seedCredsConnection(t *testing.T, s *Server, slug string, creds map[string]string) int64 {
	t.Helper()
	ensureTestAdmin(t, s)
	if len(s.secret) == 0 {
		// Encrypt requires a 32-byte AES key. newTestServer doesn't
		// populate s.secret, so seed a deterministic test key here
		// rather than threading a secret through every callsite.
		s.secret = []byte("0123456789abcdef0123456789abcdef")
	}
	credsJSON, _ := json.Marshal(creds)
	enc, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: slug, AppName: slug, Name: "test",
		AuthType: "aws_sigv4", EncryptedCreds: enc,
		ProjectID: "proj-1", CreatedVia: "integration",
	})
	if err != nil {
		t.Fatalf("create conn: %v", err)
	}
	return conn.ID
}

func TestCallback_GetCredentials_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{"access_key_id": "AKIA"})
	// Manifest declares the role + binding but NOT the permission.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "platform.connections.read_credentials") {
		t.Errorf("expected error to name missing permission, got: %s", rec.Body.String())
	}
}

func TestCallback_GetCredentials_RejectsUnboundConnection(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{"access_key_id": "AKIA"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	// Bind to a DIFFERENT connection id than the one we'll request.
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(99)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCallback_GetCredentials_RejectsIncompatibleSlug(t *testing.T) {
	s := newTestServer(t)
	// Slug stored on the connection is openai-api, manifest only allows cloudflare-r2.
	connID := seedCredsConnection(t, s, "openai-api", map[string]string{"api_key": "sk-1"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "compatible_slugs") {
		t.Errorf("expected error mentioning compatible_slugs, got: %s", rec.Body.String())
	}
}

func TestCallback_GetCredentials_HappyPath(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{
		"account_id":        "abc123",
		"access_key_id":     "AKIATEST",
		"secret_access_key": "shhh",
		"region":            "auto",
	})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out sdk.ConnectionCredentials
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ConnectionID != connID {
		t.Errorf("ConnectionID = %d, want %d", out.ConnectionID, connID)
	}
	if out.Slug != "cloudflare-r2" {
		t.Errorf("Slug = %q, want cloudflare-r2", out.Slug)
	}
	if out.Fields["account_id"] != "abc123" || out.Fields["access_key_id"] != "AKIATEST" {
		t.Errorf("Fields missing expected values: %+v", out.Fields)
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

// --- Project-aware app routing -------------------------------------

func TestRegistry_GetByNameAndProject_PrefersProjectMatch(t *testing.T) {
	r := NewInstalledAppsRegistry()
	r.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "alpha"})
	r.Add(&InstalledApp{InstallID: 2, AppName: "storage", ProjectID: "beta"})
	r.Add(&InstalledApp{InstallID: 3, AppName: "storage", ProjectID: ""})

	if got := r.GetByNameAndProject("storage", "alpha"); got == nil || got.InstallID != 1 {
		t.Errorf("alpha → install_id=1, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", "beta"); got == nil || got.InstallID != 2 {
		t.Errorf("beta → install_id=2, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", "gamma"); got == nil || got.InstallID != 3 {
		t.Errorf("gamma (no match) → global install_id=3, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", ""); got == nil || got.InstallID != 3 {
		t.Errorf("empty project → global install_id=3, got %+v", got)
	}
	if got := r.GetByNameAndProject("missing", "alpha"); got != nil {
		t.Errorf("unknown app → nil, got %+v", got)
	}
}

func TestRegistry_GetByNameAndProject_NoGlobalFallback(t *testing.T) {
	r := NewInstalledAppsRegistry()
	r.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "alpha"})
	// No global install. A request scoped to "beta" must NOT silently
	// route to alpha — that's the bug we're fixing.
	if got := r.GetByNameAndProject("storage", "beta"); got != nil {
		t.Errorf("beta with no global → nil, got install_id=%d", got.InstallID)
	}
}

func TestInstallBoundAppID_ResolvesBoundTarget(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	// Seed a "storage" app + two installs under different projects.
	s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('storage','git','','','{}')`)
	var storageAppID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name='storage'`).Scan(&storageAppID)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1,'a@b.c','x')`)
	res1, _ := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-alpha', 'running', 1)`, storageAppID)
	storageInstall1, _ := res1.LastInsertId()
	res2, _ := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-beta', 'running', 1)`, storageAppID)
	storageInstall2, _ := res2.LastInsertId()
	s.installedApps.Add(&InstalledApp{InstallID: storageInstall1, AppName: "storage", ProjectID: "proj-alpha"})
	s.installedApps.Add(&InstalledApp{InstallID: storageInstall2, AppName: "storage", ProjectID: "proj-beta"})

	// Caller (media) bound to storageInstall2 specifically.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "media",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "storage"}},
		},
	}
	mediaInstallID := seedInstallWithBindings(t, s, "media", manifest, map[string]any{
		"storage": float64(storageInstall2),
	})

	got := installBoundAppID(s, mediaInstallID, "storage")
	if got != storageInstall2 {
		t.Errorf("expected bound install_id=%d, got %d", storageInstall2, got)
	}
}
