package main

// apps_mcp.go covers the bridge between app_installs and mcp_servers.
// Each test seeds a fresh in-memory store, inserts an app row + an
// install row, then exercises the bridge.

import (
	"encoding/json"
	"strings"
	"testing"

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

// readMCPRow returns one mcp_servers row by upstream_id, or nil if
// not present. Tests assert on its contents.
func readMCPRow(t *testing.T, s *Server, installID int64) map[string]any {
	t.Helper()
	row := map[string]any{}
	var (
		id, userID, toolCount        int64
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
	if !strings.Contains(url, "api_key=dev-") {
		t.Errorf("url = %q, expected api_key=dev-<install_id>", url)
	}

	var tools []string
	if err := json.Unmarshal([]byte(row["allowed_tools"].(string)), &tools); err != nil {
		t.Fatalf("allowed_tools not JSON: %v", err)
	}
	if len(tools) != 3 || tools[0] != "files_upload" {
		t.Errorf("allowed_tools = %v", tools)
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
	// row's manifest_json with three tools.
	updatedManifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "storage",
		Version:  "0.2.0",
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
	if secondRow["description"] != "After upgrade" {
		t.Errorf("description didn't refresh: %v", secondRow["description"])
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
