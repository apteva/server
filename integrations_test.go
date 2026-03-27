package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func createTestCatalog(t *testing.T) *AppCatalog {
	t.Helper()

	// Create a temp dir with test app JSONs
	dir := t.TempDir()

	pushover := `{
		"slug": "pushover",
		"name": "Pushover",
		"description": "Push notifications",
		"categories": ["notifications"],
		"base_url": "https://api.pushover.net/1",
		"auth": {
			"types": ["api_key"],
			"query_params": {"token": "{{app_token}}"},
			"credential_fields": [
				{"name": "app_token", "label": "App Token"},
				{"name": "user_key", "label": "User Key"}
			]
		},
		"tools": [
			{
				"name": "send_notification",
				"description": "Send a push notification",
				"method": "POST",
				"path": "/messages.json",
				"input_schema": {
					"type": "object",
					"properties": {
						"message": {"type": "string"},
						"user": {"type": "string"}
					},
					"required": ["message"]
				}
			}
		]
	}`

	github := `{
		"slug": "github",
		"name": "GitHub",
		"description": "GitHub API",
		"categories": ["development"],
		"base_url": "https://api.github.com",
		"auth": {
			"types": ["bearer"],
			"headers": {"Authorization": "Bearer {{token}}"},
			"credential_fields": [
				{"name": "token", "label": "Access Token"}
			]
		},
		"tools": [
			{
				"name": "get_user",
				"description": "Get user profile",
				"method": "GET",
				"path": "/users/{username}",
				"input_schema": {
					"type": "object",
					"properties": {
						"username": {"type": "string"}
					},
					"required": ["username"]
				}
			},
			{
				"name": "list_repos",
				"description": "List repos",
				"method": "GET",
				"path": "/users/{username}/repos",
				"input_schema": {
					"type": "object",
					"properties": {
						"username": {"type": "string"}
					}
				}
			}
		]
	}`

	os.WriteFile(filepath.Join(dir, "pushover.json"), []byte(pushover), 0644)
	os.WriteFile(filepath.Join(dir, "github.json"), []byte(github), 0644)

	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	return catalog
}

func TestCatalogLoad(t *testing.T) {
	catalog := createTestCatalog(t)

	if catalog.Count() != 2 {
		t.Fatalf("expected 2 apps, got %d", catalog.Count())
	}
}

func TestCatalogGet(t *testing.T) {
	catalog := createTestCatalog(t)

	app := catalog.Get("pushover")
	if app == nil {
		t.Fatal("expected pushover app")
	}
	if app.Name != "Pushover" {
		t.Errorf("expected Pushover, got %s", app.Name)
	}
	if len(app.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(app.Tools))
	}
	if len(app.Auth.CredentialFields) != 2 {
		t.Errorf("expected 2 credential fields, got %d", len(app.Auth.CredentialFields))
	}

	missing := catalog.Get("nonexistent")
	if missing != nil {
		t.Error("expected nil for nonexistent app")
	}
}

func TestCatalogList(t *testing.T) {
	catalog := createTestCatalog(t)

	list := catalog.List()
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
	// Should be sorted by name
	if list[0].Name != "GitHub" {
		t.Errorf("expected GitHub first (sorted), got %s", list[0].Name)
	}
}

func TestCatalogSearch(t *testing.T) {
	catalog := createTestCatalog(t)

	results := catalog.Search("push")
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'push', got %d", len(results))
	}
	if results[0].Slug != "pushover" {
		t.Errorf("expected pushover, got %s", results[0].Slug)
	}

	results = catalog.Search("development")
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'development', got %d", len(results))
	}

	results = catalog.Search("")
	if len(results) != 2 {
		t.Fatalf("expected all 2 for empty search, got %d", len(results))
	}
}

func TestResolveTemplate(t *testing.T) {
	creds := map[string]string{
		"app_token": "tok123",
		"user_key":  "usr456",
	}

	result := resolveTemplate("{{app_token}}", creds)
	if result != "tok123" {
		t.Errorf("expected tok123, got %s", result)
	}

	result = resolveTemplate("Bearer {{app_token}}", creds)
	if result != "Bearer tok123" {
		t.Errorf("expected Bearer tok123, got %s", result)
	}
}

func TestResolveTemplateFallback(t *testing.T) {
	creds := map[string]string{
		"bearer_token": "bt789",
	}

	result := resolveTemplate("Bearer {{token}}", creds)
	if result != "Bearer bt789" {
		t.Errorf("expected Bearer bt789, got %s", result)
	}
}

func TestBuildURL(t *testing.T) {
	url := buildURL("https://api.github.com", "/users/{username}/repos", map[string]any{
		"username": "octocat",
	})
	if url != "https://api.github.com/users/octocat/repos" {
		t.Errorf("got %s", url)
	}
}

func TestBuildAuthQuery(t *testing.T) {
	q := buildAuthQuery(map[string]string{"token": "{{app_token}}"}, map[string]string{"app_token": "abc123"})
	if q != "?token=abc123" {
		t.Errorf("expected ?token=abc123, got %s", q)
	}

	q = buildAuthQuery(nil, map[string]string{})
	if q != "" {
		t.Errorf("expected empty, got %s", q)
	}
}

func TestBuildHeaders(t *testing.T) {
	h := buildHeaders(map[string]string{"Authorization": "Bearer {{token}}"}, map[string]string{"token": "ghp_123"})
	if h["Authorization"] != "Bearer ghp_123" {
		t.Errorf("got %s", h["Authorization"])
	}
}

func TestExtractPath(t *testing.T) {
	data := map[string]any{
		"data": map[string]any{
			"items": []any{"a", "b"},
		},
	}
	result := extractPath(data, "data.items")
	items, ok := result.([]any)
	if !ok || len(items) != 2 {
		t.Errorf("expected [a, b], got %v", result)
	}
}

// --- Connection tests ---

func TestConnectionCRUD(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = createTestCatalog(t)

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create connection
	creds, _ := json.Marshal(map[string]string{"app_token": "tok123", "user_key": "usr456"})
	encrypted, _ := Encrypt(s.secret, string(creds))

	conn, err := s.store.CreateConnection(1, "pushover", "Pushover", "My Pushover", "api_key", encrypted, "")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if conn.AppSlug != "pushover" {
		t.Errorf("expected pushover, got %s", conn.AppSlug)
	}

	// List
	list, _ := s.store.ListConnections(1)
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}

	// Get with encrypted creds
	got, encCreds, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Name != "My Pushover" {
		t.Errorf("expected My Pushover, got %s", got.Name)
	}

	// Decrypt and verify
	plain, _ := Decrypt(s.secret, encCreds)
	var decrypted map[string]string
	json.Unmarshal([]byte(plain), &decrypted)
	if decrypted["app_token"] != "tok123" {
		t.Errorf("expected tok123, got %s", decrypted["app_token"])
	}

	// Delete
	s.store.DeleteConnection(1, conn.ID)
	list2, _ := s.store.ListConnections(1)
	if len(list2) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(list2))
	}
}

func TestConnectionHTTPHandler(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = createTestCatalog(t)

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	// Create connection via HTTP
	body, _ := json.Marshal(map[string]any{
		"app_slug":    "pushover",
		"name":        "My Pushover",
		"credentials": map[string]string{"app_token": "tok123", "user_key": "usr456"},
	})
	req := httptest.NewRequest("POST", "/connections", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateConnection(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var conn Connection
	json.Unmarshal(rec.Body.Bytes(), &conn)
	if conn.AppSlug != "pushover" {
		t.Errorf("expected pushover, got %s", conn.AppSlug)
	}
	if conn.AppName != "Pushover" {
		t.Errorf("expected Pushover, got %s", conn.AppName)
	}

	// List connections
	req = httptest.NewRequest("GET", "/connections", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleListConnections(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}

	var conns []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &conns)
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	if conns[0]["tool_count"].(float64) != 1 {
		t.Errorf("expected 1 tool, got %v", conns[0]["tool_count"])
	}

	// Get tools
	req = httptest.NewRequest("GET", "/connections/1/tools", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleConnectionTools(rec, req)

	if rec.Code != 200 {
		t.Fatalf("tools: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var tools []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &tools)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0]["name"] != "pushover_send_notification" {
		t.Errorf("expected pushover_send_notification, got %v", tools[0]["name"])
	}
}

func TestMCPServerAutoCreatedFromConnection(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = createTestCatalog(t)

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	creds, _ := json.Marshal(map[string]string{"token": "ghp_test"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	conn, _ := s.store.CreateConnection(1, "github", "GitHub", "My GitHub", "bearer", encrypted, "")

	// Auto-create MCP server
	s.store.CreateMCPServerFromConnection(1, conn, 2)

	// Check MCP server exists
	servers, _ := s.store.ListMCPServers(1)
	if len(servers) != 1 {
		t.Fatalf("expected 1 MCP server, got %d", len(servers))
	}
	if servers[0].Name != "GitHub" {
		t.Errorf("expected GitHub, got %s", servers[0].Name)
	}

	// Delete connection should delete MCP server
	s.store.DeleteMCPServerByConnection(conn.ID)
	servers2, _ := s.store.ListMCPServers(1)
	if len(servers2) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(servers2))
	}
}
