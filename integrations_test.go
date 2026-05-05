package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestRemoteMcpAppLoad verifies the catalog parses kind=remote_mcp +
// the embedded mcp{} block. Without these the new HubSpot hosted-MCP
// entry (and any future Notion / Linear hosted MCPs) would silently
// load as legacy REST apps with empty Tools.
func TestRemoteMcpAppLoad(t *testing.T) {
	dir := t.TempDir()

	hubspotMcp := `{
		"slug": "hubspot-mcp",
		"name": "HubSpot (hosted MCP)",
		"description": "Vendor-hosted MCP",
		"categories": ["crm", "mcp"],
		"kind": "remote_mcp",
		"base_url": "https://mcp-eu1.hubspot.com",
		"mcp": {
			"transport": "http",
			"url": "https://mcp-eu1.hubspot.com/mcp",
			"auth_header": {
				"name": "Authorization",
				"value": "Bearer {{token}}"
			}
		},
		"auth": {
			"types": ["oauth2"],
			"oauth2": {
				"authorize_url": "https://mcp-eu1.hubspot.com/oauth/authorize/user",
				"token_url": "https://mcp-eu1.hubspot.com/oauth/token",
				"scopes": [],
				"client_id_required": true,
				"pkce": true
			}
		},
		"tools": []
	}`

	if err := os.WriteFile(filepath.Join(dir, "hubspot-mcp.json"), []byte(hubspotMcp), 0644); err != nil {
		t.Fatalf("write hubspot-mcp.json: %v", err)
	}

	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	app := catalog.Get("hubspot-mcp")
	if app == nil {
		t.Fatal("expected hubspot-mcp in catalog")
	}
	if app.Kind != "remote_mcp" {
		t.Errorf("kind: expected remote_mcp, got %q", app.Kind)
	}
	if app.MCP == nil {
		t.Fatal("MCP config missing")
	}
	if app.MCP.Transport != "http" {
		t.Errorf("transport: expected http, got %q", app.MCP.Transport)
	}
	if app.MCP.URL != "https://mcp-eu1.hubspot.com/mcp" {
		t.Errorf("url: %q", app.MCP.URL)
	}
	if app.MCP.AuthHeader == nil || app.MCP.AuthHeader.Name != "Authorization" {
		t.Errorf("auth_header missing or wrong: %+v", app.MCP.AuthHeader)
	}
	if app.MCP.AuthHeader.Value != "Bearer {{token}}" {
		t.Errorf("auth_header value: %q", app.MCP.AuthHeader.Value)
	}
	// remote_mcp templates legitimately have empty tools — the upstream
	// is the source of truth via tools/list.
	if len(app.Tools) != 0 {
		t.Errorf("remote_mcp tools should be empty, got %d", len(app.Tools))
	}

	// AppSummary must surface kind so the catalog UI can render the
	// "hosted MCP" badge alongside the REST entry with the same brand.
	list := catalog.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(list))
	}
	if list[0].Kind != "remote_mcp" {
		t.Errorf("summary.Kind: expected remote_mcp, got %q", list[0].Kind)
	}
}

// TestRemoteMcpAutoCreatedFromConnection drives the full server seam:
// load a kind=remote_mcp template into the catalog, decrypt creds, and
// verify createRemoteMcpFromConnection writes a single mcp_servers row
// pointing at the vendor's hosted URL with the OAuth token resolved
// into encrypted_env. This is what handleCreateConnection +
// handleOAuthCallback both call after a successful connect.
func TestRemoteMcpAutoCreatedFromConnection(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()

	// Inject a remote_mcp app directly into the catalog.
	tok := "Bearer {{token}}"
	s.catalog.Register(&AppTemplate{
		Slug:        "hubspot-mcp",
		Name:        "HubSpot (hosted MCP)",
		Description: "test",
		BaseURL:     "https://mcp-eu1.hubspot.com",
		Kind:        "remote_mcp",
		MCP: &RemoteMcpConfig{
			Transport: "http",
			URL:       "https://mcp-eu1.hubspot.com/mcp",
			AuthHeader: &McpAuthHeaderTmpl{
				Name:  "Authorization",
				Value: tok,
			},
		},
		Auth: AppAuthConfig{Types: []string{"oauth2"}},
	})

	// Persist a connection as if OAuth had just completed.
	credsJSON, _ := json.Marshal(map[string]string{
		"access_token": "ya29.fakeTokenAbc",
	})
	encCreds, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:    1,
		AppSlug:   "hubspot-mcp",
		AppName:   "HubSpot (hosted MCP)",
		Name:      "Demo Portal",
		AuthType:  "oauth2",
		EncryptedCreds: encCreds,
		ProjectID: "demo",
		Source:    "local",
		Status:    "active",
	})
	if err != nil {
		t.Fatalf("create conn: %v", err)
	}

	mcpID, err := s.createRemoteMcpFromConnection(1, conn, s.catalog.Get("hubspot-mcp"), encCreds)
	if err != nil {
		t.Fatalf("createRemoteMcpFromConnection: %v", err)
	}
	if mcpID == 0 {
		t.Fatal("expected non-zero mcp id")
	}

	// Row should be source=remote, transport=http, url=upstream.
	rec, encEnv, err := s.store.GetMCPServer(1, mcpID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if rec.Source != "remote" {
		t.Errorf("source: expected remote, got %q", rec.Source)
	}
	if rec.Transport != "http" {
		t.Errorf("transport: expected http, got %q", rec.Transport)
	}
	if rec.URL != "https://mcp-eu1.hubspot.com/mcp" {
		t.Errorf("url: %q", rec.URL)
	}
	if rec.ConnectionID != conn.ID {
		t.Errorf("connection_id: expected %d, got %d", conn.ID, rec.ConnectionID)
	}

	// encrypted_env should decrypt into {"AUTHORIZATION": "Bearer ya29.fakeTokenAbc"}.
	plain, derr := Decrypt(s.secret, encEnv)
	if derr != nil {
		t.Fatalf("decrypt env: %v", derr)
	}
	var env map[string]string
	if uerr := json.Unmarshal([]byte(plain), &env); uerr != nil {
		t.Fatalf("unmarshal env: %v", uerr)
	}
	want := "Bearer ya29.fakeTokenAbc"
	if env["AUTHORIZATION"] != want {
		t.Errorf("env[AUTHORIZATION]: expected %q, got %q", want, env["AUTHORIZATION"])
	}

	// Re-running createRemoteMcpFromConnection should leave a SINGLE row,
	// not multiply (this is what re-OAuth after token expiry does).
	if _, err := s.createRemoteMcpFromConnection(1, conn, s.catalog.Get("hubspot-mcp"), encCreds); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	rows, _ := s.store.ListMCPServers(1, "demo")
	count := 0
	for _, r := range rows {
		if r.ConnectionID == conn.ID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 row after re-create, got %d", count)
	}
}

// TestRemoteMcpRejectsUnresolvedTemplate guards against a silent
// "broken connection" — if the template references a credential field
// that doesn't exist on the connection, we fail loud so operators can
// see the misconfiguration before the agent tries to call upstream.
func TestRemoteMcpRejectsUnresolvedTemplate(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()

	s.catalog.Register(&AppTemplate{
		Slug:    "broken-mcp",
		Name:    "Broken",
		BaseURL: "https://x.example.com",
		Kind:    "remote_mcp",
		MCP: &RemoteMcpConfig{
			Transport: "http",
			URL:       "https://x.example.com/mcp",
			AuthHeader: &McpAuthHeaderTmpl{
				Name:  "Authorization",
				Value: "Bearer {{nonexistent}}",
			},
		},
		Auth: AppAuthConfig{Types: []string{"oauth2"}},
	})

	credsJSON, _ := json.Marshal(map[string]string{"access_token": "abc"})
	encCreds, _ := Encrypt(s.secret, string(credsJSON))
	conn, _ := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "broken-mcp", Name: "x", AuthType: "oauth2",
		EncryptedCreds: encCreds, Source: "local", Status: "active",
	})

	_, err := s.createRemoteMcpFromConnection(1, conn, s.catalog.Get("broken-mcp"), encCreds)
	if err == nil {
		t.Fatal("expected error for unresolved {{nonexistent}}")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Errorf("expected 'could not resolve' error, got %v", err)
	}
}

// TestLegacyAppKindEmpty makes sure existing REST templates still load
// with an empty Kind (so older entries don't need to be rewritten and
// the UI's empty-means-rest convention holds).
func TestLegacyAppKindEmpty(t *testing.T) {
	catalog := createTestCatalog(t)
	app := catalog.Get("pushover")
	if app == nil {
		t.Fatal("expected pushover")
	}
	if app.Kind != "" {
		t.Errorf("legacy app should have empty Kind, got %q", app.Kind)
	}
	if app.MCP != nil {
		t.Errorf("legacy app should have nil MCP, got %+v", app.MCP)
	}
}

// ─── binary response handling (path A: Code app's GitHub import) ──

// TestIsBinaryContentType pins the prefix list. Order doesn't matter
// (it's a HasPrefix-any check) but each MIME type a real catalog
// integration will return must classify as binary, otherwise the
// executor stringifies the bytes and breaks decoding on the app side.
func TestIsBinaryContentType(t *testing.T) {
	binary := []string{
		"application/x-gzip",
		"application/x-gzip; charset=binary",
		"application/gzip",
		"application/zip",
		"application/x-tar",
		"application/octet-stream",
		"application/pdf",
		"image/png",
		"image/jpeg",
		"audio/mpeg",
		"video/mp4",
		"font/woff2",
		"  APPLICATION/X-GZIP  ",
	}
	for _, ct := range binary {
		if !isBinaryContentType(ct) {
			t.Errorf("expected %q to be binary", ct)
		}
	}
	text := []string{
		"application/json",
		"application/json; charset=utf-8",
		"text/plain",
		"text/html",
		"application/xml",
		"",
	}
	for _, ct := range text {
		if isBinaryContentType(ct) {
			t.Errorf("expected %q NOT to be binary", ct)
		}
	}
}

// TestExecuteIntegrationTool_BinaryResponse spins up a tiny upstream
// server returning a gzip tarball, runs it through executeIntegrationTool,
// and asserts the response lands in the {_binary, base64, mimeType, size}
// envelope shape (matching integrations/src/http-executor.ts).
//
// Before path A, this test would fail: respBody got coerced to string
// and the bytes lost on JSON-encoding round-trips. The Code app's
// GitHub import flow depends on this envelope.
func TestExecuteIntegrationTool_BinaryResponse(t *testing.T) {
	payload := []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef, 'a', 'b', 'c'}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-gzip")
		w.WriteHeader(200)
		w.Write(payload)
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-gh",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"oauth2"}},
	}
	tool := &AppToolDef{
		Name:   "get_archive",
		Method: "GET",
		Path:   "/x",
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "tok"}, map[string]any{})
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success || res.Status != 200 {
		t.Fatalf("status=%d success=%v", res.Status, res.Success)
	}
	env, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data not a map: %T = %v", res.Data, res.Data)
	}
	if env["_binary"] != true {
		t.Errorf("_binary=%v, want true", env["_binary"])
	}
	if env["mimeType"] != "application/x-gzip" {
		t.Errorf("mimeType=%v, want application/x-gzip", env["mimeType"])
	}
	if got := env["size"]; got != len(payload) {
		t.Errorf("size=%v, want %d", got, len(payload))
	}
	b64, _ := env["base64"].(string)
	if b64 == "" {
		t.Fatal("base64 empty")
	}
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if !bytes.Equal(dec, payload) {
		t.Fatalf("decoded bytes mismatch: %x vs %x", dec, payload)
	}
}
