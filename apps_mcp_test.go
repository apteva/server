package main

// apps_mcp.go covers the bridge between app_installs and mcp_servers.
// Each test seeds a fresh in-memory store, inserts an app row + an
// install row, then exercises the bridge.

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// seedAppWithManifest creates an apps row carrying a manifest with
// the given tools, plus an app_installs row in 'running' state, and
// returns the install id. Wraps seedInstall (already in
// appbus_handlers_test.go) — that helper writes a minimal manifest;
// here we want to control mcp_tools.
func seedAppWithTools(t *testing.T, s *Server, appName, projectID string, toolNames []string) int64 {
	t.Helper()
	tools := make([]sdk.MCPToolSpec, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, sdk.MCPToolSpec{Name: n, Description: "tool: " + n})
	}
	manifest := sdk.Manifest{
		Schema:      sdk.SchemaCurrent,
		Name:        appName,
		DisplayName: appName,
		Version:     "0.1.0",
		Description: "Test app for " + appName,
		Provides:    sdk.Provides{MCPTools: tools},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		appName, string(manifestJSON),
	); err != nil {
		t.Fatalf("insert apps: %v", err)
	}
	var appID int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, appName).Scan(&appID); err != nil {
		t.Fatalf("select app id: %v", err)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, ?, 'running', 1)`,
		appID, projectID,
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	id, _ := res.LastInsertId()
	// users(id=1) is needed for the FK on mcp_servers.user_id once we
	// register. Idempotent insert.
	s.store.db.Exec(
		`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (?, ?, ?)`,
		1, "test@test.local", "x",
	)
	return id
}

func TestHandleListAppsClosesInstallRowsBeforeIntegrationRows(t *testing.T) {
	store := newTestStore(t)
	s := &Server{store: store, catalog: NewAppCatalog()}
	ensureTestAdmin(t, s)

	manifest := sdk.Manifest{
		Schema:      sdk.SchemaCurrent,
		Name:        "storage",
		DisplayName: "Storage",
		Version:     "0.1.0",
		Provides: sdk.Provides{UISurfaces: []sdk.UISurface{{
			ID: "files", Label: "Files", Icon: "folder", Schema: sdk.NativeSurfaceSchemaCurrent,
			Entry: "/ui/surfaces/files.json", Slots: []string{sdk.UISurfaceSlotMobileProjectApp},
		}}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		"storage", string(manifestJSON),
	); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, version, permissions_json)
		 SELECT id, 'proj-1', 'running', '0.1.0', '[]' FROM apps WHERE name='storage'`,
	); err != nil {
		t.Fatalf("insert install: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO connections (user_id, app_slug, app_name, name, auth_type, encrypted_credentials, status, project_id)
		 VALUES (1, 'notion', 'Notion', 'Notion', 'api_key', '{}', 'active', 'proj-1')`,
	); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	s.catalog.Register(&AppTemplate{
		Slug:        "notion",
		Name:        "Notion",
		Description: "Notion integration",
		UIComponents: []IntegrationUIComponent{{
			Name:  "PageCard",
			Entry: "PageCard.mjs",
		}},
	})

	req := httptest.NewRequest("GET", "/api/apps?project_id=proj-1", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.handleListApps(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleListApps deadlocked while appending integration UI rows")
	}
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []AppRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want app row plus integration row: %s", len(rows), rec.Body.String())
	}
	var storage *AppRow
	for i := range rows {
		if rows[i].Name == "storage" {
			storage = &rows[i]
		}
	}
	if storage == nil || len(storage.UISurfaces) != 1 || storage.UISurfaces[0].ID != "files" {
		t.Fatalf("storage ui_surfaces missing from response: %#v", storage)
	}
}

// readMCPRow returns one mcp_servers row by upstream_id, or nil if
// not present. Tests assert on its contents.
func readMCPRow(t *testing.T, s *Server, installID int64) map[string]any {
	t.Helper()
	row := map[string]any{}
	var (
		id, userID, toolCount                                                    int64
		name, desc, source, transport, url, projectID, allowed, upstream, status string
	)
	err := s.store.db.QueryRow(
		`SELECT id, user_id, name, description, source, transport, url, project_id,
				allowed_tools, upstream_id, tool_count, status
		 FROM mcp_servers WHERE upstream_id = ?`,
		appMCPUpstreamID(installID),
	).Scan(&id, &userID, &name, &desc, &source, &transport, &url, &projectID,
		&allowed, &upstream, &toolCount, &status)
	if err != nil {
		return nil
	}
	row["id"] = id
	row["user_id"] = userID
	row["name"] = name
	row["description"] = desc
	row["source"] = source
	row["transport"] = transport
	row["url"] = url
	row["project_id"] = projectID
	row["allowed_tools"] = allowed
	row["upstream_id"] = upstream
	row["tool_count"] = toolCount
	row["status"] = status
	return row
}

// --- registerAppMCP --------------------------------------------------

func TestRegisterAppMCP_InsertsRow(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "storage", "proj-1",
		[]string{"files_upload", "files_get", "files_delete"})

	if err := s.registerAppMCP(installID); err != nil {
		t.Fatalf("registerAppMCP: %v", err)
	}
	row := readMCPRow(t, s, installID)
	if row == nil {
		t.Fatal("no mcp_servers row created")
	}
	if row["name"] != "storage" {
		t.Errorf("name = %v, want storage", row["name"])
	}
	if row["source"] != "app" {
		t.Errorf("source = %v, want app", row["source"])
	}
	if row["transport"] != "http" {
		t.Errorf("transport = %v, want http", row["transport"])
	}
	if row["project_id"] != "proj-1" {
		t.Errorf("project_id = %v, want proj-1", row["project_id"])
	}
	if row["tool_count"] != int64(3) {
		t.Errorf("tool_count = %v, want 3", row["tool_count"])
	}
	if row["status"] != "running" {
		t.Errorf("status = %v, want running", row["status"])
	}

	url := row["url"].(string)
	if !strings.Contains(url, "/api/apps/storage/mcp") {
		t.Errorf("url = %q, expected /api/apps/storage/mcp path", url)
	}
	if !strings.Contains(url, "api_key=app_") {
		t.Errorf("url = %q, expected random per-install app token", url)
	}
	if !strings.Contains(url, "install_id=") {
		t.Errorf("url = %q, expected install_id query param", url)
	}

	var tools []string
	if err := json.Unmarshal([]byte(row["allowed_tools"].(string)), &tools); err != nil {
		t.Fatalf("allowed_tools not JSON: %v", err)
	}
	if len(tools) != 3 || tools[0] != "files_upload" {
		t.Errorf("allowed_tools = %v", tools)
	}
}

func TestInjectProjectIntoMCPRequestAddsProjectID(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/apps/social/mcp?project_id=proj-1", strings.NewReader(`{
		"jsonrpc":"2.0",
		"id":"abc",
		"method":"tools/call",
		"params":{"name":"posts_create","arguments":{"body":"hello"}}
	}`))
	if err := injectProjectIntoMCPRequest(req, "proj-1"); err != nil {
		t.Fatalf("injectProjectIntoMCPRequest: %v", err)
	}
	body, _ := io.ReadAll(req.Body)
	var rpc map[string]any
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v\n%s", err, body)
	}
	params := rpc["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if got := args["_project_id"]; got != "proj-1" {
		t.Fatalf("_project_id=%v, want proj-1", got)
	}
	if got := rpc["id"]; got != "abc" {
		t.Fatalf("id=%v, want abc", got)
	}
}

func TestInjectProjectIntoMCPRequestOverridesSpoofedProjectID(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/apps/social/mcp?project_id=proj-1", strings.NewReader(`{
		"jsonrpc":"2.0",
		"id":"abc",
		"method":"tools/call",
		"params":{"name":"account_list","arguments":{"_project_id":"victim-project"}}
	}`))
	if err := injectProjectIntoMCPRequest(req, "proj-1"); err != nil {
		t.Fatalf("injectProjectIntoMCPRequest: %v", err)
	}
	body, _ := io.ReadAll(req.Body)
	var rpc map[string]any
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	params := rpc["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if got := args["_project_id"]; got != "proj-1" {
		t.Fatalf("_project_id=%v, want trusted project proj-1", got)
	}
}

func TestInstallIDFromDevAPIKey(t *testing.T) {
	if got := installIDFromDevAPIKey("dev-42"); got != 42 {
		t.Fatalf("installIDFromDevAPIKey(dev-42)=%d, want 42", got)
	}
	if got := installIDFromDevAPIKey("real-user-key"); got != 0 {
		t.Fatalf("installIDFromDevAPIKey(non-dev)=%d, want 0", got)
	}
}

func TestRegisterAppMCP_NoToolsSkips(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "panel-only", "proj-1", nil)

	if err := s.registerAppMCP(installID); err != nil {
		t.Fatalf("registerAppMCP: %v", err)
	}
	if row := readMCPRow(t, s, installID); row != nil {
		t.Fatalf("expected no row for app with no mcp_tools, got %+v", row)
	}
}

func TestRegisterAppMCP_IsIdempotentAndRefreshes(t *testing.T) {
	// Calling register twice should produce one row, not two — and the
	// second call should pick up an updated allowed_tools list (the
	// upgrade-adds-new-tool case).
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "storage", "proj-1",
		[]string{"files_upload", "files_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	firstRow := readMCPRow(t, s, installID)

	// Simulate an app upgrade that added a new tool: rewrite the apps
	// row's manifest_json with three tools and a new display name.
	updatedManifest := sdk.Manifest{
		Schema:      sdk.SchemaCurrent,
		Name:        "storage",
		DisplayName: "Storage v2",
		Version:     "0.2.0",
		Description: "After upgrade",
		Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{
			{Name: "files_upload"},
			{Name: "files_get"},
			{Name: "files_delete"},
		}},
	}
	updatedJSON, _ := json.Marshal(updatedManifest)
	if _, err := s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE name = ?`, string(updatedJSON), "storage"); err != nil {
		t.Fatal(err)
	}

	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	secondRow := readMCPRow(t, s, installID)

	if firstRow["id"] != secondRow["id"] {
		t.Fatalf("primary key changed across re-register: %v → %v",
			firstRow["id"], secondRow["id"])
	}
	if secondRow["tool_count"] != int64(3) {
		t.Errorf("tool_count after refresh = %v, want 3", secondRow["tool_count"])
	}
	if !strings.Contains(secondRow["allowed_tools"].(string), "files_delete") {
		t.Errorf("allowed_tools didn't pick up new tool: %v", secondRow["allowed_tools"])
	}
	// Bridge row's `description` column is the dashboard's primary
	// label — must be short. We map to DisplayName, not Description.
	if secondRow["description"] != "Storage v2" {
		t.Errorf("description (display label) didn't refresh: %v", secondRow["description"])
	}

	// Exactly one row should exist for this install.
	var count int
	s.store.db.QueryRow(
		`SELECT COUNT(*) FROM mcp_servers WHERE upstream_id = ?`,
		appMCPUpstreamID(installID),
	).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestRegisterAppMCP_UsesDisplayNameNotDescription(t *testing.T) {
	// Description is multi-paragraph prose; the dashboard would render
	// it as the row's primary label. Make sure we map to the manifest's
	// short DisplayName instead, leaving the long blurb in apps.manifest_json
	// for detail views to surface separately.
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema:      sdk.SchemaCurrent,
		Name:        "storage",
		DisplayName: "Storage",
		Description: "File storage. Long. Multi-sentence. " + strings.Repeat("blah ", 50),
		Provides:    sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "files_upload"}}},
	}
	mj, _ := json.Marshal(manifest)
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		"storage", string(mj),
	); err != nil {
		t.Fatal(err)
	}
	var appID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name='storage'`).Scan(&appID)
	res, _ := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, '', 'running', 1)`,
		appID,
	)
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)

	if err := s.registerAppMCP(id); err != nil {
		t.Fatal(err)
	}
	row := readMCPRow(t, s, id)
	if row["description"] != "Storage" {
		t.Fatalf("expected DisplayName 'Storage' as description; got %q", row["description"])
	}
	if strings.Contains(row["description"].(string), "blah") {
		t.Fatalf("the long manifest description leaked into mcp_servers.description")
	}
}

func TestRegisterAppMCP_UnknownInstallReturnsError(t *testing.T) {
	s := newTestServer(t)
	if err := s.registerAppMCP(99999); err == nil {
		t.Fatal("expected error for missing install, got nil")
	}
}

// --- unregisterAppMCP ------------------------------------------------

func TestUnregisterAppMCP_DeletesRow(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "storage", "proj-1", []string{"files_upload"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	if readMCPRow(t, s, installID) == nil {
		t.Fatal("setup: row should exist")
	}
	if err := s.unregisterAppMCP(installID); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if row := readMCPRow(t, s, installID); row != nil {
		t.Fatalf("expected row deleted, got %+v", row)
	}
}

func TestUnregisterAppMCP_NoRowIsNoOp(t *testing.T) {
	s := newTestServer(t)
	// Never registered — unregister should not error.
	if err := s.unregisterAppMCP(42); err != nil {
		t.Fatalf("unregister with no row: %v", err)
	}
}

func TestRegisterAppMCP_UpgradeWithToolRemoval(t *testing.T) {
	// App that drops all its MCP tools across an upgrade should have
	// its bridge row deleted (unregister-on-empty path).
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "storage", "proj-1",
		[]string{"files_upload", "files_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	if readMCPRow(t, s, installID) == nil {
		t.Fatal("setup: row should exist")
	}
	// Manifest now declares zero tools.
	updatedManifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "storage",
		Version:  "0.3.0",
		Provides: sdk.Provides{},
	}
	updatedJSON, _ := json.Marshal(updatedManifest)
	s.store.db.Exec(`UPDATE apps SET manifest_json = ? WHERE name = ?`, string(updatedJSON), "storage")
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	if row := readMCPRow(t, s, installID); row != nil {
		t.Fatalf("expected row removed when manifest has no tools, got %+v", row)
	}
}

// --- backfillAppMCPs -------------------------------------------------

func TestBackfillAppMCPs_RegistersMissing(t *testing.T) {
	s := newTestServer(t)
	id1 := seedAppWithTools(t, s, "storage", "proj-1", []string{"files_upload"})
	id2 := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	// Pre-register id1 so backfill should skip it (idempotent skip,
	// not double-register).
	if err := s.registerAppMCP(id1); err != nil {
		t.Fatal(err)
	}

	s.backfillAppMCPs()

	if readMCPRow(t, s, id1) == nil {
		t.Error("id1 row missing after backfill")
	}
	if readMCPRow(t, s, id2) == nil {
		t.Error("id2 row missing — backfill didn't register it")
	}

	// Exactly one row per install.
	var count int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM mcp_servers WHERE source='app'`).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 source=app rows after backfill, got %d", count)
	}
}

func TestBackfillAppMCPs_SkipsNonRunning(t *testing.T) {
	s := newTestServer(t)
	id := seedAppWithTools(t, s, "storage", "proj-1", []string{"files_upload"})
	// Mark non-running. Backfill should skip it.
	s.store.db.Exec(`UPDATE app_installs SET status = 'error' WHERE id = ?`, id)
	s.backfillAppMCPs()
	if readMCPRow(t, s, id) != nil {
		t.Error("non-running install should be skipped by backfill")
	}
}

// --- upstream id format ----------------------------------------------

func TestAppMCPUpstreamID_Format(t *testing.T) {
	if got, want := appMCPUpstreamID(42), "app:42"; got != want {
		t.Errorf("upstream id = %q, want %q", got, want)
	}
}
