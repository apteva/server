package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func routeTestCatalog() *AppCatalog {
	catalog := NewAppCatalog()
	catalog.Register(&AppTemplate{
		Slug: "bunny-stream",
		Name: "Bunny Stream",
		Tools: []AppToolDef{
			{Name: "list_videos", Description: "List videos", InputSchema: map[string]any{"type": "object"}},
			{Name: "create_video", Description: "Create a video", InputSchema: map[string]any{"type": "object"}},
		},
	})
	catalog.Register(&AppTemplate{
		Slug: "codemagic",
		Name: "Codemagic",
		Tools: []AppToolDef{
			{Name: "builds_list", Description: "List builds", InputSchema: map[string]any{"type": "object"}},
		},
	})
	return catalog
}

func seedMCPRouteTestData(t *testing.T, store *Store, secret []byte) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES (1, 'route-test@example.test', 'x')`); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	encrypted, err := Encrypt(secret, `{}`)
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}
	// Deliberately make connection 71 a different integration from MCP row
	// 71. The two tables have independent numeric namespaces.
	if _, err := store.db.Exec(`
		INSERT INTO connections (id, user_id, app_slug, app_name, name, auth_type, encrypted_credentials, status)
		VALUES
			(42, 1, 'bunny-stream', 'Bunny Stream', 'Bunny Monika', 'api_key', ?, 'active'),
			(71, 1, 'codemagic', 'Codemagic', 'Codemagic Production', 'api_key', ?, 'active')
	`, encrypted, encrypted); err != nil {
		t.Fatalf("insert connections: %v", err)
	}
	// Leave nullable legacy columns null on purpose: an old row must still
	// scan successfully and remain authoritative after a reboot.
	if _, err := store.db.Exec(`
		INSERT INTO mcp_servers
			(id, user_id, name, command, args, encrypted_env, description, status, tool_count, pid,
			 source, transport, connection_id, project_id, allowed_tools)
		VALUES (71, 1, 'bunny-monika', '', NULL, NULL, 'Bunny Stream Monika', 'running', 2, NULL,
			'local', 'http', 42, '', '')
	`); err != nil {
		t.Fatalf("insert canonical MCP row: %v", err)
	}
}

func callMCPToolsList(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	))
	req.RemoteAddr = "127.0.0.1:40123"
	rec := httptest.NewRecorder()
	s.handleMCPEndpoint(rec, req)
	return rec
}

func toolNamesFromMCPResponse(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list response: %v (body=%s)", err, rec.Body.String())
	}
	names := make([]string, 0, len(response.Result.Tools))
	for _, tool := range response.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestMCPRoutePersistedRowWinsCrossTableIDCollisionAfterReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mcp-routes.db")
	secret := bytes.Repeat([]byte{0x51}, 32)
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedMCPRouteTestData(t, store, secret)
	if err := store.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	// Reopen the database to model a full server reboot. No runtime route
	// registry is reconstructed; the persisted MCP row is authoritative.
	store, err = NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Server{store: store, secret: secret, catalog: routeTestCatalog()}

	names := toolNamesFromMCPResponse(t, callMCPToolsList(t, s, "/mcp/71"))
	if strings.Join(names, ",") != "list_videos,create_video" {
		t.Fatalf("/mcp/71 resolved wrong catalog: got %v, want Bunny Stream tools", names)
	}
}

func TestMCPRouteNewScopedRowIsImmediateAndAcceptsCanonicalToolPrefix(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = routeTestCatalog()
	encrypted, err := Encrypt(s.secret, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnection(1, "bunny-stream", "Bunny Stream", "Bunny Monika", "api_key", encrypted, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateMCPServerFromConnection(1, conn, 2); err != nil {
		t.Fatal(err)
	}
	scoped, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID:       1,
		Name:         "bunny-monika-hsites",
		Description:  "H Sites Bunny view",
		Source:       "local",
		Transport:    "http",
		ConnectionID: conn.ID,
		ProjectID:    "project-a",
		AllowedTools: []string{"bunny-monika_list_videos"},
		ToolCount:    1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No restart, cache refresh, or route-registration call is needed.
	names := toolNamesFromMCPResponse(t, callMCPToolsList(t, s, "/mcp/"+itoa64(scoped.ID)))
	if strings.Join(names, ",") != "list_videos" {
		t.Fatalf("new scoped route filtered wrong tools: got %v, want [list_videos]", names)
	}
}

func TestMCPRouteLookupFailureDoesNotFallBackToConnectionID(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = routeTestCatalog()
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	rec := callMCPToolsList(t, s, "/mcp/71")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("lookup failure status=%d body=%s; want 500 without numeric reinterpretation", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to resolve MCP server") {
		t.Fatalf("unexpected lookup failure body: %s", rec.Body.String())
	}
}
