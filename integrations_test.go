package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
		"logo": "https://example.com/pushover.png",
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

func TestEmbeddedGitHubCatalogAgentCoverage(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/github.json")
	if err != nil {
		t.Fatalf("read embedded GitHub catalog: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode GitHub catalog: %v", err)
	}
	if got, want := len(app.Tools), 103; got != want {
		t.Fatalf("GitHub tool count=%d want=%d", got, want)
	}
	if got := app.Auth.Headers["Accept"]; got != "application/vnd.github+json" {
		t.Fatalf("GitHub Accept header=%q", got)
	}
	if got := app.Auth.Headers["X-GitHub-Api-Version"]; got != "2022-11-28" {
		t.Fatalf("GitHub API version=%q", got)
	}

	wantTools := map[string]bool{
		"list_root_contents":      false,
		"create_git_commit":       false,
		"delete_file":             false,
		"upload_release_asset":    false,
		"list_webhook_deliveries": false,
	}
	for i := range app.Tools {
		tool := &app.Tools[i]
		if _, wanted := wantTools[tool.Name]; wanted {
			wantTools[tool.Name] = true
		}
		if tool.Name == "delete_file" {
			if tool.RequestTransform == nil || tool.RequestTransform.Type != "json_wrap" {
				t.Fatalf("delete_file missing JSON DELETE transform: %#v", tool.RequestTransform)
			}
		}
		if tool.Name == "upload_release_asset" {
			if tool.BaseURL != "https://uploads.github.com" || tool.BodyBinaryParam != "file" {
				t.Fatalf("upload_release_asset config=%+v", tool)
			}
		}
	}
	for name, found := range wantTools {
		if !found {
			t.Fatalf("embedded GitHub catalog missing %s", name)
		}
	}
}

func TestEmbedded3DPrintingCatalogProductionContracts(t *testing.T) {
	readApp := func(slug string) AppTemplate {
		t.Helper()
		raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
		if err != nil {
			t.Fatalf("read embedded %s catalog: %v", slug, err)
		}
		var app AppTemplate
		if err := json.Unmarshal(raw, &app); err != nil {
			t.Fatalf("decode embedded %s catalog: %v", slug, err)
		}
		return app
	}
	findTool := func(app AppTemplate, name string) AppToolDef {
		t.Helper()
		for _, tool := range app.Tools {
			if tool.Name == name {
				return tool
			}
		}
		t.Fatalf("%s catalog missing %s", app.Slug, name)
		return AppToolDef{}
	}

	treatstock := readApp("treatstock")
	if got := treatstock.Auth.QueryParams["private-key"]; got != "{{private_key}}" {
		t.Fatalf("Treatstock private-key auth=%q", got)
	}
	if upload := findTool(treatstock, "create_printable_pack"); upload.MultipartForm == nil || upload.MultipartForm.FileFields["files"] != "files[]" {
		t.Fatalf("Treatstock upload multipart config=%+v", upload.MultipartForm)
	}

	slant := readApp("slant3d")
	presigned := findTool(slant, "upload_presigned_file")
	if presigned.Path != "{uploadUrl}" || presigned.BodyBinaryParam != "file" || len(presigned.OmitAuthHeaders) != 1 || presigned.OmitAuthHeaders[0] != "Authorization" {
		t.Fatalf("Slant presigned upload config=%+v", presigned)
	}
	if estimate := findTool(slant, "estimate_file_price"); estimate.Path != "/api/files/{publicFileServiceId}/estimate" {
		t.Fatalf("Slant estimate path=%q", estimate.Path)
	}

	shapeways := readApp("shapeways")
	modelUpload := findTool(shapeways, "upload_model")
	if modelUpload.MultipartForm != nil {
		t.Fatal("Shapeways model upload must be JSON/base64, not multipart")
	}
	required, _ := modelUpload.InputSchema["required"].([]any)
	requiredSet := map[string]bool{}
	for _, value := range required {
		if name, ok := value.(string); ok {
			requiredSet[name] = true
		}
	}
	for _, name := range []string{"file", "fileName", "hasRightsToModel", "acceptTermsAndConditions"} {
		if !requiredSet[name] {
			t.Fatalf("Shapeways upload missing required field %s", name)
		}
	}

	sculpteo := readApp("sculpteo")
	if upload := findTool(sculpteo, "upload_design"); upload.MultipartForm == nil || upload.MultipartForm.FileFields["file"] != "file" {
		t.Fatalf("Sculpteo upload multipart config=%+v", upload.MultipartForm)
	}
}

func TestOpenAICodexCatalogDoesNotPinLegacyDefaultModel(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/openai-codex.json")
	if err != nil {
		t.Fatalf("read embedded OpenAI Codex catalog: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode OpenAI Codex catalog: %v", err)
	}
	if len(app.Tools) == 0 {
		t.Fatal("OpenAI Codex catalog has no tools")
	}
	for _, tool := range app.Tools {
		properties, _ := tool.InputSchema["properties"].(map[string]any)
		model, _ := properties["model"].(map[string]any)
		if _, pinned := model["default"]; pinned {
			t.Fatalf("tool %s still pins a model default: %#v", tool.Name, model)
		}
		required, _ := tool.InputSchema["required"].([]any)
		for _, rawRequired := range required {
			if requiredName, _ := rawRequired.(string); requiredName == "model" {
				t.Fatalf("tool %s still requires model", tool.Name)
			}
		}
	}
}

func TestGoogleAdsCatalogSeparatesOAuthAndUserCredentials(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/google-ads.json")
	if err != nil {
		t.Fatalf("read embedded Google Ads catalog: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatalf("decode embedded Google Ads catalog: %v", err)
	}
	fields := make(map[string]CredentialField, len(app.Auth.CredentialFields))
	for _, field := range app.Auth.CredentialFields {
		fields[field.Name] = field
	}
	developer := fields["developer_token"]
	if developer.Source != "user" || developer.Hidden || developer.Required == nil || !*developer.Required {
		t.Fatalf("developer token metadata=%#v", developer)
	}
	manager := fields["manager_customer_id"]
	if manager.Source != "user" || manager.Hidden || manager.Required == nil || *manager.Required {
		t.Fatalf("manager customer id metadata=%#v", manager)
	}
	for _, name := range []string{"token", "refresh_token", "expires_in", "token_type"} {
		field := fields[name]
		if field.Source != "oauth" || !field.Hidden {
			t.Fatalf("%s metadata=%#v", name, field)
		}
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

func TestBuildHeadersOmitsUnresolvedOptionalCredential(t *testing.T) {
	headers := buildHeaders(
		map[string]string{
			"Authorization":     "Bearer {{token}}",
			"developer-token":   "{{developer_token}}",
			"login-customer-id": "{{manager_customer_id}}",
		},
		map[string]string{
			"access_token":    "oauth-access",
			"developer_token": "developer-secret",
		},
	)
	if headers["Authorization"] != "Bearer oauth-access" {
		t.Fatalf("Authorization=%q", headers["Authorization"])
	}
	if headers["developer-token"] != "developer-secret" {
		t.Fatalf("developer-token=%q", headers["developer-token"])
	}
	if _, exists := headers["login-customer-id"]; exists {
		t.Fatalf("optional unresolved header was retained: %q", headers["login-customer-id"])
	}
}

func TestCollectOAuthSupplementalCredentials(t *testing.T) {
	required := true
	optional := false
	app := &AppTemplate{Auth: AppAuthConfig{CredentialFields: []CredentialField{
		{Name: "developer_token", Label: "Developer token", Required: &required, Source: "user"},
		{Name: "manager_customer_id", Label: "Manager customer ID", Required: &optional, Source: "user"},
		{Name: "access_token", Label: "Access token", Source: "oauth", Hidden: true},
	}}}
	if _, err := collectOAuthSupplementalCredentials(app, map[string]string{}); err == nil {
		t.Fatal("missing required user credential was accepted")
	}
	got, err := collectOAuthSupplementalCredentials(app, map[string]string{
		"developer_token":     " secret ",
		"manager_customer_id": "",
		"access_token":        "must-not-be-stored",
		"unknown":             "must-not-be-stored",
	})
	if err != nil {
		t.Fatalf("valid supplemental credentials rejected: %v", err)
	}
	if got["developer_token"] != "secret" {
		t.Fatalf("developer_token=%q", got["developer_token"])
	}
	if len(got) != 1 {
		t.Fatalf("unexpected supplemental credentials: %#v", got)
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

func TestBuildURL_EscapesPathParams(t *testing.T) {
	url := buildURL("https://sheets.googleapis.com/v4", "/spreadsheets/{spreadsheetId}/values/{range}", map[string]any{
		"spreadsheetId": "sheet123",
		"range":         "'Form Responses 1'!1:30",
	})
	want := "https://sheets.googleapis.com/v4/spreadsheets/sheet123/values/%27Form%20Responses%201%27%211:30"
	if url != want {
		t.Errorf("got %s, want %s", url, want)
	}
}

func TestBuildURL_PreservesAbsoluteURLHostAndFullURLParams(t *testing.T) {
	url := buildURL("https://api.pinecone.io", "https://{index_host}/query", map[string]any{
		"index_host": "idx-123.svc.us-east-1.pinecone.io",
	})
	want := "https://idx-123.svc.us-east-1.pinecone.io/query"
	if url != want {
		t.Errorf("got %s, want %s", url, want)
	}

	url = buildURL("https://generativelanguage.googleapis.com/v1beta", "{videoUrl}", map[string]any{
		"videoUrl": "https://download.example.com/video file.mp4?token=a/b",
	})
	want = "https://download.example.com/video file.mp4?token=a/b"
	if url != want {
		t.Errorf("got %s, want %s", url, want)
	}
}

func TestBuildAuthQuery(t *testing.T) {
	q := buildAuthQuery(map[string]string{"token": "{{app_token}}"}, map[string]string{"app_token": "abc123"})
	if q != "token=abc123" {
		t.Errorf("expected token=abc123, got %s", q)
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
	if conns[0]["logo"] != "https://example.com/pushover.png" {
		t.Errorf("expected catalog logo, got %v", conns[0]["logo"])
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

func TestDashboardListsHideAppOwnedConnectionsByDefault(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.catalog = createTestCatalog(t)

	creds, _ := json.Marshal(map[string]string{"token": "test"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	if _, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         1,
		AppSlug:        "github",
		AppName:        "GitHub",
		Name:           "GitHub operator",
		AuthType:       "bearer",
		EncryptedCreds: encrypted,
		ProjectID:      "proj",
		CreatedVia:     "integration",
	}); err != nil {
		t.Fatalf("create operator connection: %v", err)
	}
	if _, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:            1,
		AppSlug:           "github",
		AppName:           "GitHub",
		Name:              "social:github:1",
		AuthType:          "bearer",
		EncryptedCreds:    encrypted,
		ProjectID:         "proj",
		CreatedVia:        "app_install",
		OwnerAppInstallID: 52,
	}); err != nil {
		t.Fatalf("create app-owned connection: %v", err)
	}

	req := httptest.NewRequest("GET", "/connections?project_id=proj", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListConnections(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list default: status %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode default list: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "GitHub operator" {
		t.Fatalf("default list should only include operator connection, got %#v", rows)
	}

	req = httptest.NewRequest("GET", "/connections?project_id=proj&include_app_owned=1", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleListConnections(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list include_app_owned: status %d: %s", rec.Code, rec.Body.String())
	}
	rows = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode include list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("include_app_owned should include both connections, got %#v", rows)
	}
}

func TestDashboardMCPListHidesAppOwnedConnectionRowsByDefault(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.catalog = createTestCatalog(t)

	creds, _ := json.Marshal(map[string]string{"token": "test"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	operatorConn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         1,
		AppSlug:        "github",
		AppName:        "GitHub",
		Name:           "GitHub operator",
		AuthType:       "bearer",
		EncryptedCreds: encrypted,
		ProjectID:      "proj",
		CreatedVia:     "integration",
	})
	if err != nil {
		t.Fatalf("create operator connection: %v", err)
	}
	appConn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:            1,
		AppSlug:           "github",
		AppName:           "GitHub",
		Name:              "social:github:1",
		AuthType:          "bearer",
		EncryptedCreds:    encrypted,
		ProjectID:         "proj",
		CreatedVia:        "app_install",
		OwnerAppInstallID: 52,
	})
	if err != nil {
		t.Fatalf("create app-owned connection: %v", err)
	}
	if _, err := s.store.CreateMCPServerFromConnection(1, operatorConn, 2); err != nil {
		t.Fatalf("create operator mcp: %v", err)
	}
	if _, err := s.store.CreateMCPServerFromConnection(1, appConn, 2); err != nil {
		t.Fatalf("create app-owned mcp: %v", err)
	}

	req := httptest.NewRequest("GET", "/mcp-servers?project_id=proj", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListMCPServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp list default: status %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode default mcp list: %v", err)
	}
	if len(rows) != 1 || rows[0]["connection_id"].(float64) != float64(operatorConn.ID) {
		t.Fatalf("default mcp list should only include operator row, got %#v", rows)
	}

	req = httptest.NewRequest("GET", "/mcp-servers?project_id=proj&include_app_owned=1", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleListMCPServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp list include_app_owned: status %d: %s", rec.Code, rec.Body.String())
	}
	rows = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode include mcp list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("include_app_owned should include both mcp rows, got %#v", rows)
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
	if servers[0].Name != "my-github" {
		t.Errorf("expected my-github, got %s", servers[0].Name)
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
	ensureTestAdmin(t, s)
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
		UserID:         1,
		AppSlug:        "hubspot-mcp",
		AppName:        "HubSpot (hosted MCP)",
		Name:           "Demo Portal",
		AuthType:       "oauth2",
		EncryptedCreds: encCreds,
		ProjectID:      "demo",
		Source:         "local",
		Status:         "active",
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
	ensureTestAdmin(t, s)
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
// Regression: integrations like Namecheap put the command in tool.path as
// "/?Command=...". Auth credentials must be appended with "&", not a second
// "?", or downstream parsers swallow the first auth param into the preceding
// value. See namecheap error 1010101 ("Parameter APIUser is missing").
func TestExecuteIntegrationTool_AuthQueryWithPathQuery(t *testing.T) {
	var capturedURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURI = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-namecheap",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{
			Types: []string{"api_key"},
			QueryParams: map[string]string{
				"ApiUser": "{{api_user}}",
				"ApiKey":  "{{api_key}}",
			},
		},
	}
	tool := &AppToolDef{
		Name:   "get_hosts",
		Method: "GET",
		Path:   "/?Command=namecheap.domains.dns.getHosts",
	}
	creds := map[string]string{"api_user": "alice", "api_key": "secret"}
	if _, err := executeIntegrationTool(app, tool, creds, map[string]any{}, ""); err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if strings.Count(capturedURI, "?") != 1 {
		t.Errorf("expected exactly one '?' in URI, got %q", capturedURI)
	}
	// Parse and assert each param landed as its own key.
	q, err := url.ParseQuery(strings.TrimPrefix(capturedURI[strings.Index(capturedURI, "?")+1:], ""))
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if got := q.Get("Command"); got != "namecheap.domains.dns.getHosts" {
		t.Errorf("Command=%q, want namecheap.domains.dns.getHosts", got)
	}
	if got := q.Get("ApiUser"); got != "alice" {
		t.Errorf("ApiUser=%q, want alice", got)
	}
	if got := q.Get("ApiKey"); got != "secret" {
		t.Errorf("ApiKey=%q, want secret", got)
	}
}

func TestExecuteIntegrationTool_UsesIntegrationSpecificProxy(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		http.Error(w, "target should not be hit directly", http.StatusTeapot)
	}))
	defer target.Close()

	var proxiedURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxiedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"via":"proxy"}`))
	}))
	defer proxy.Close()

	t.Setenv("APTEVA_INTEGRATION_PROXY_RINGOVER", proxy.URL)

	app := &AppTemplate{
		Slug:    "ringover",
		BaseURL: target.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:   "teams",
		Method: http.MethodGet,
		Path:   "/v2/teams",
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "secret"}, map[string]any{}, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if targetHit {
		t.Fatal("request hit target directly; expected proxy")
	}
	if !strings.HasPrefix(proxiedURL, target.URL+"/v2/teams") {
		t.Fatalf("proxied URL=%q, want target absolute URL", proxiedURL)
	}
	if !res.Success || res.Status != http.StatusOK {
		t.Fatalf("status=%d success=%v data=%v", res.Status, res.Success, res.Data)
	}
}

func TestIntegrationProxyEnvName_NormalizesSlug(t *testing.T) {
	got := integrationProxyEnvName("google-sheets.v2")
	want := "APTEVA_INTEGRATION_PROXY_GOOGLE_SHEETS_V2"
	if got != want {
		t.Fatalf("env name=%q, want %q", got, want)
	}
}

func TestExecuteIntegrationTool_QueryParamArraysRepeatValues(t *testing.T) {
	var declaredRanges []string
	var genericRanges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/declared/"):
			declaredRanges = r.URL.Query()["ranges"]
		case strings.HasPrefix(r.URL.Path, "/generic/"):
			genericRanges = r.URL.Query()["ranges"]
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-google-sheets",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"oauth2"}},
	}
	declaredTool := &AppToolDef{
		Name:        "batch_get",
		Method:      "GET",
		Path:        "/declared/{spreadsheetId}",
		QueryParams: []string{"ranges"},
	}
	genericTool := &AppToolDef{
		Name:   "batch_get_generic",
		Method: "GET",
		Path:   "/generic/{spreadsheetId}",
	}
	inputAny := map[string]any{
		"spreadsheetId": "abc",
		"ranges":        []any{"Sheet1!A1:B2", "Sheet2!C1:D2"},
	}
	if _, err := executeIntegrationTool(app, declaredTool, map[string]string{"access_token": "tok"}, inputAny, ""); err != nil {
		t.Fatalf("execute declared: %v", err)
	}
	want := []string{"Sheet1!A1:B2", "Sheet2!C1:D2"}
	if !reflect.DeepEqual(declaredRanges, want) {
		t.Fatalf("declared ranges=%v, want %v", declaredRanges, want)
	}

	inputString := map[string]any{
		"spreadsheetId": "abc",
		"ranges":        []string{"Sheet1!A1:B2", "Sheet2!C1:D2"},
	}
	if _, err := executeIntegrationTool(app, genericTool, map[string]string{"access_token": "tok"}, inputString, ""); err != nil {
		t.Fatalf("execute generic: %v", err)
	}
	if !reflect.DeepEqual(genericRanges, want) {
		t.Fatalf("generic ranges=%v, want %v", genericRanges, want)
	}
}

func TestExecuteIntegrationTool_QueryParamAliases(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-bunny-stream",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:              "list_videos",
		Method:            "GET",
		Path:              "/library/123/videos",
		QueryParamAliases: map[string]string{"collectionId": "collection"},
	}
	input := map[string]any{
		"collectionId": "collection-123",
		"page":         2,
	}
	if _, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "tok"}, input, ""); err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if got := gotQuery.Get("collection"); got != "collection-123" {
		t.Fatalf("collection query=%q, want collection-123 (all=%v)", got, gotQuery)
	}
	if got := gotQuery.Get("collectionId"); got != "" {
		t.Fatalf("collectionId leaked into query as %q (all=%v)", got, gotQuery)
	}
	if got := gotQuery.Get("page"); got != "2" {
		t.Fatalf("page query=%q, want 2 (all=%v)", got, gotQuery)
	}
}

func TestExecuteIntegrationTool_PostQueryParamsExcludedFromBody(t *testing.T) {
	var gotQuery url.Values
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-bunny-stream",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:        "fetch_video",
		Method:      "POST",
		Path:        "/library/123/videos/fetch",
		QueryParams: []string{"collectionId", "thumbnailTime"},
	}
	input := map[string]any{
		"url":           "https://example.com/video.mp4",
		"title":         "video.mp4",
		"collectionId":  "collection-123",
		"thumbnailTime": 1000,
	}
	if _, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "tok"}, input, ""); err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if got := gotQuery.Get("collectionId"); got != "collection-123" {
		t.Fatalf("collectionId query=%q, want collection-123 (all=%v)", got, gotQuery)
	}
	if got := gotQuery.Get("thumbnailTime"); got != "1000" {
		t.Fatalf("thumbnailTime query=%q, want 1000 (all=%v)", got, gotQuery)
	}
	if _, leaked := gotBody["collectionId"]; leaked {
		t.Fatalf("collectionId leaked into body: %#v", gotBody)
	}
	if _, leaked := gotBody["thumbnailTime"]; leaked {
		t.Fatalf("thumbnailTime leaked into body: %#v", gotBody)
	}
	if gotBody["url"] != "https://example.com/video.mp4" || gotBody["title"] != "video.mp4" {
		t.Fatalf("body lost fetch fields: %#v", gotBody)
	}
}

func TestExecuteIntegrationTool_HeaderParamsExcludedFromBody(t *testing.T) {
	var gotHeader string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("model")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{Slug: "fake-fish", BaseURL: srv.URL}
	tool := &AppToolDef{
		Name:         "text_to_speech",
		Method:       "POST",
		Path:         "/v1/tts",
		HeaderParams: map[string]string{"model": "model"},
	}
	input := map[string]any{"model": "s2-pro", "text": "Hello"}
	if _, err := executeIntegrationTool(app, tool, nil, input, ""); err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if gotHeader != "s2-pro" {
		t.Fatalf("model header=%q, want s2-pro", gotHeader)
	}
	if _, leaked := gotBody["model"]; leaked {
		t.Fatalf("model leaked into body: %#v", gotBody)
	}
	if gotBody["text"] != "Hello" {
		t.Fatalf("body lost text: %#v", gotBody)
	}
}

func TestExecuteIntegrationTool_ByteRangeHeaderTransform(t *testing.T) {
	var gotRange string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-4194303/125829120")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{1, 2, 3, 4})
	}))
	defer srv.Close()

	app := &AppTemplate{Slug: "fake-google-drive", BaseURL: srv.URL}
	tool := &AppToolDef{
		Name:   "download_file",
		Method: "GET",
		Path:   "/drive/v3/files/{fileId}?alt=media",
		HeaderTransforms: []HeaderTransformDef{{
			Type:       "byte_range",
			Header:     "Range",
			StartParam: "start_byte",
			EndParam:   "end_byte",
		}},
	}
	res, err := executeIntegrationTool(app, tool, nil, map[string]any{
		"fileId":     "file-123",
		"start_byte": float64(0),
		"end_byte":   float64(4194303),
	}, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("execute result=%+v err=%v", res, err)
	}
	if gotRange != "bytes=0-4194303" {
		t.Fatalf("Range header=%q, want bytes=0-4194303", gotRange)
	}
	if got := gotQuery.Get("alt"); got != "media" {
		t.Fatalf("alt query=%q, want media", got)
	}
	if gotQuery.Has("start_byte") || gotQuery.Has("end_byte") {
		t.Fatalf("range inputs leaked into query: %v", gotQuery)
	}
	if got := res.Headers["Content-Range"]; got != "bytes 0-4194303/125829120" {
		t.Fatalf("Content-Range response header=%q", got)
	}
}

func TestApplyHeaderTransformsRejectsInvalidByteRange(t *testing.T) {
	headers := map[string]string{}
	transforms := []HeaderTransformDef{{
		Type: "byte_range", StartParam: "start_byte", EndParam: "end_byte",
	}}
	if _, err := applyHeaderTransforms(headers, transforms, map[string]any{
		"end_byte": float64(10),
	}); err == nil || !strings.Contains(err.Error(), "end_byte requires start_byte") {
		t.Fatalf("end-only range error=%v", err)
	}
	if _, err := applyHeaderTransforms(headers, transforms, map[string]any{
		"start_byte": float64(10), "end_byte": float64(9),
	}); err == nil || !strings.Contains(err.Error(), "greater than or equal") {
		t.Fatalf("reversed range error=%v", err)
	}
}

func TestExecuteIntegrationTool_DeleteVideoPathParamNoBody(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotQuery url.Values
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"success":true,"message":"OK","statusCode":200}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-bunny-stream",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:   "delete_video",
		Method: "DELETE",
		Path:   "/library/374587/videos/{videoId}",
	}
	input := map[string]any{
		"videoId": "94a1872d-7296-4ec7-8cb1-d814b1eddafe",
	}
	if _, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "tok"}, input, ""); err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method=%q, want DELETE", gotMethod)
	}
	if gotPath != "/library/374587/videos/94a1872d-7296-4ec7-8cb1-d814b1eddafe" {
		t.Fatalf("path=%q", gotPath)
	}
	if len(gotQuery) != 0 {
		t.Fatalf("unexpected query: %v", gotQuery)
	}
	if len(gotBody) != 0 {
		t.Fatalf("DELETE sent body: %s", gotBody)
	}
}

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
	res, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "tok"}, map[string]any{}, "")
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

// TestExecuteIntegrationTool_BodyRoot asserts that when a tool declares
// body_root_param, the named input field's value becomes the entire JSON
// request body verbatim — including a top-level array, which the default
// "marshal all inputs into an object" path cannot produce. IONOS DNS's
// create-records endpoint (POST /zones/{zoneId}/records) needs this.
func TestExecuteIntegrationTool_BodyRoot(t *testing.T) {
	var capturedBody []byte
	var capturedPath string
	var capturedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-ionos",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:     "create_records",
		Method:   "POST",
		Path:     "/zones/{zoneId}/records",
		BodyRoot: "records",
	}
	input := map[string]any{
		"zoneId": "zone-123",
		"records": []any{
			map[string]any{"name": "mail.acme.com", "type": "A", "content": "1.2.3.4", "ttl": 3600},
		},
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "pref.secret"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/zones/zone-123/records" {
		t.Errorf("path=%q, want /zones/zone-123/records", capturedPath)
	}
	if capturedCT != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", capturedCT)
	}
	// The body must be a bare JSON array, not an object wrapping "records",
	// and must NOT contain the path param zoneId.
	var arr []map[string]any
	if err := json.Unmarshal(capturedBody, &arr); err != nil {
		t.Fatalf("body is not a top-level JSON array: %v (body=%s)", err, capturedBody)
	}
	if len(arr) != 1 {
		t.Fatalf("array len=%d, want 1 (body=%s)", len(arr), capturedBody)
	}
	if arr[0]["name"] != "mail.acme.com" || arr[0]["type"] != "A" {
		t.Errorf("record mismatch: %v", arr[0])
	}
	if strings.Contains(string(capturedBody), "zoneId") {
		t.Errorf("body leaked path param zoneId: %s", capturedBody)
	}
}

func TestAppToolDef_BodyBinaryParamUnmarshal(t *testing.T) {
	var tool AppToolDef
	if err := json.Unmarshal([]byte(`{
		"name": "set_thumbnail",
		"method": "POST",
		"path": "/upload",
		"body_binary_param": "image",
		"query_params": ["videoId"]
	}`), &tool); err != nil {
		t.Fatalf("unmarshal AppToolDef: %v", err)
	}
	if tool.BodyBinaryParam != "image" {
		t.Fatalf("BodyBinaryParam=%q, want image", tool.BodyBinaryParam)
	}
}

func TestExecuteIntegrationTool_BodyBinaryParam(t *testing.T) {
	rawPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	var capturedQuery string
	var capturedCT string
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		capturedCT = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-youtube",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"oauth2"}},
	}
	tool := &AppToolDef{
		Name:            "set_thumbnail",
		Method:          "POST",
		Path:            "/upload",
		QueryParams:     []string{"videoId"},
		BodyBinaryParam: "image",
	}
	input := map[string]any{
		"videoId": "abc123",
		"image": map[string]any{
			"_binary":  true,
			"base64":   base64.StdEncoding.EncodeToString(rawPNG),
			"mimeType": "image/png",
		},
	}

	res, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "tok"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success || res.Status != http.StatusOK {
		t.Fatalf("success=%v status=%d data=%v", res.Success, res.Status, res.Data)
	}
	values, err := url.ParseQuery(capturedQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", capturedQuery, err)
	}
	if got := values.Get("videoId"); got != "abc123" {
		t.Fatalf("videoId query=%q, want abc123", got)
	}
	if capturedCT != "image/png" {
		t.Fatalf("Content-Type=%q, want image/png", capturedCT)
	}
	if !bytes.Equal(capturedBody, rawPNG) {
		t.Fatalf("body bytes mismatch: got %x want %x", capturedBody, rawPNG)
	}
	bodyText := string(capturedBody)
	for _, forbidden := range []string{"_binary", "base64", "mimeType", "videoId", "abc123", "{"} {
		if strings.Contains(bodyText, forbidden) {
			t.Fatalf("body leaked JSON/query field %q: %q", forbidden, bodyText)
		}
	}
}

func TestExecuteIntegrationTool_BodyBinaryParamRejectsInvalidEnvelope(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-youtube",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"oauth2"}},
	}
	tool := &AppToolDef{
		Name:            "set_thumbnail",
		Method:          "POST",
		Path:            "/upload",
		BodyBinaryParam: "image",
	}
	_, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "tok"}, map[string]any{
		"image": map[string]any{"_binary": true, "base64": "not-valid-base64"},
	}, "")
	if err == nil {
		t.Fatal("expected invalid base64 error")
	}
	if called {
		t.Fatal("upstream server was called for invalid binary envelope")
	}
}

func TestExecuteIntegrationTool_OptionalBodyBinaryParamFallsBackToJSON(t *testing.T) {
	var capturedCT string
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{Slug: "deepgram", BaseURL: srv.URL}
	tool := &AppToolDef{
		Name:            "listen",
		Method:          "POST",
		Path:            "/v1/listen",
		BodyBinaryParam: "audio",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":   map[string]any{"type": "string"},
				"audio": map[string]any{"type": "string"},
			},
		},
	}
	input := map[string]any{"url": "https://cdn.example.test/audio.mp3", "model": "nova-3"}
	result, err := executeIntegrationTool(app, tool, nil, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !result.Success {
		t.Fatalf("result=%+v", result)
	}
	if capturedCT != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", capturedCT)
	}
	if capturedBody["url"] != input["url"] || capturedBody["model"] != "nova-3" {
		t.Fatalf("upstream body=%#v", capturedBody)
	}
	if _, leaked := capturedBody["audio"]; leaked {
		t.Fatalf("absent binary field leaked into JSON: %#v", capturedBody)
	}
}

func TestExecuteIntegrationTool_RequiredBodyBinaryParamStillRejectsMissing(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	app := &AppTemplate{Slug: "youtube-api", BaseURL: srv.URL}
	tool := &AppToolDef{
		Name:            "set_thumbnail",
		Method:          "POST",
		Path:            "/upload",
		BodyBinaryParam: "image",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"videoId", "image"},
		},
	}
	_, err := executeIntegrationTool(app, tool, nil, map[string]any{"videoId": "abc123"}, "")
	if err == nil || !strings.Contains(err.Error(), `body_binary_param "image" is required`) {
		t.Fatalf("error=%v, want required binary error", err)
	}
	if called {
		t.Fatal("upstream called despite missing required binary input")
	}
}

func TestExecuteIntegrationTool_MultipartForm(t *testing.T) {
	var capturedPath string
	var capturedCT string
	var capturedName string
	var capturedLogin string
	var capturedFilename string
	var capturedFile []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		capturedName = r.MultipartForm.Value["name"][0]
		capturedLogin = r.MultipartForm.Value["login"][0]
		file, header, err := r.FormFile("model")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		capturedFilename = header.Filename
		capturedFile, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-upload",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{
			Types:      []string{"api_key"},
			BodyParams: map[string]string{"login": "{{login}}"},
		},
	}
	tool := &AppToolDef{
		Name:   "upload",
		Method: "POST",
		Path:   "/v1/files/{folderId}",
		MultipartForm: &MultipartFormDef{
			FileFields: map[string]string{"file": "model"},
			FieldNames: []string{"name"},
		},
	}
	input := map[string]any{
		"folderId": "abc",
		"name":     "gear",
		"filename": "gear.stl",
		"file":     base64.StdEncoding.EncodeToString([]byte("solid gear")),
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"login": "user@example.com"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/v1/files/abc" {
		t.Fatalf("path=%q", capturedPath)
	}
	if !strings.HasPrefix(capturedCT, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type=%q", capturedCT)
	}
	if capturedName != "gear" {
		t.Fatalf("name=%q", capturedName)
	}
	if capturedLogin != "user@example.com" {
		t.Fatalf("login=%q", capturedLogin)
	}
	if capturedFilename != "gear.stl" {
		t.Fatalf("filename=%q", capturedFilename)
	}
	if string(capturedFile) != "solid gear" {
		t.Fatalf("file=%q", string(capturedFile))
	}
}

func TestExecuteIntegrationTool_ApifyRunActorBodyRootAndQuery(t *testing.T) {
	var capturedPath string
	var capturedQuery url.Values
	var capturedBody []byte
	var capturedAuth string
	var capturedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedAuth = r.Header.Get("Authorization")
		capturedCT = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"data":{"id":"run-123","defaultDatasetId":"ds-123","status":"READY"}}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "apify",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{
			Types:   []string{"bearer"},
			Headers: map[string]string{"Authorization": "Bearer {{token}}"},
		},
	}
	tool := &AppToolDef{
		Name:        "run_actor",
		Method:      "POST",
		Path:        "/actors/{actorId}/runs",
		QueryParams: []string{"waitForFinish", "maxItems"},
		BodyRoot:    "input",
	}
	input := map[string]any{
		"actorId":       "compass~crawler-google-places",
		"input":         map[string]any{"searchStringsArray": []any{"homes for sale Lisbon"}, "maxCrawledPlacesPerSearch": 5},
		"waitForFinish": 60,
		"maxItems":      25,
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"token": "apify-secret"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/actors/compass~crawler-google-places/runs" {
		t.Fatalf("path=%q, want /actors/compass~crawler-google-places/runs", capturedPath)
	}
	if capturedQuery.Get("waitForFinish") != "60" || capturedQuery.Get("maxItems") != "25" {
		t.Fatalf("query=%v, want waitForFinish=60&maxItems=25", capturedQuery)
	}
	if capturedAuth != "Bearer apify-secret" {
		t.Fatalf("Authorization=%q, want Bearer apify-secret", capturedAuth)
	}
	if capturedCT != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", capturedCT)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body is not JSON object: %v (body=%s)", err, capturedBody)
	}
	if _, ok := body["input"]; ok {
		t.Fatalf("body wrapped actor input under input: %s", capturedBody)
	}
	if _, ok := body["actorId"]; ok {
		t.Fatalf("body leaked actorId: %s", capturedBody)
	}
	if _, ok := body["waitForFinish"]; ok {
		t.Fatalf("body leaked query param waitForFinish: %s", capturedBody)
	}
	if body["maxCrawledPlacesPerSearch"].(float64) != 5 {
		t.Fatalf("actor input body mismatch: %v", body)
	}
}

func TestExecuteIntegrationTool_ApifySyncDatasetItemsBodyRoot(t *testing.T) {
	var capturedPath string
	var capturedQuery url.Values
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`[{"url":"https://example.com/listing/1"}]`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "apify",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"bearer"}},
	}
	tool := &AppToolDef{
		Name:        "run_actor_sync_get_dataset_items",
		Method:      "POST",
		Path:        "/actors/{actorId}/run-sync-get-dataset-items",
		QueryParams: []string{"format", "clean", "limit"},
		BodyRoot:    "input",
	}
	input := map[string]any{
		"actorId": "apify~web-scraper",
		"input": map[string]any{
			"startUrls": []any{map[string]any{"url": "https://example.com"}},
		},
		"format": "json",
		"clean":  true,
		"limit":  10,
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"token": "apify-secret"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/actors/apify~web-scraper/run-sync-get-dataset-items" {
		t.Fatalf("path=%q, want sync endpoint", capturedPath)
	}
	if capturedQuery.Get("format") != "json" || capturedQuery.Get("clean") != "true" || capturedQuery.Get("limit") != "10" {
		t.Fatalf("query=%v, want format=json&clean=true&limit=10", capturedQuery)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body is not JSON object: %v (body=%s)", err, capturedBody)
	}
	if _, ok := body["input"]; ok {
		t.Fatalf("body wrapped actor input under input: %s", capturedBody)
	}
	if _, ok := body["format"]; ok {
		t.Fatalf("body leaked dataset query params: %s", capturedBody)
	}
	if _, ok := body["startUrls"]; !ok {
		t.Fatalf("body missing actor input startUrls: %v", body)
	}
}

func TestExecuteIntegrationTool_BrightDataDatasetTriggerBodyRoot(t *testing.T) {
	var capturedPath string
	var capturedQuery url.Values
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"snapshot_id":"snap-123"}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "brightdata",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{
			Types:   []string{"bearer"},
			Headers: map[string]string{"Authorization": "Bearer {{api_key}}"},
		},
	}
	tool := &AppToolDef{
		Name:        "dataset_trigger",
		Method:      "POST",
		Path:        "/datasets/v3/trigger",
		QueryParams: []string{"dataset_id", "include_errors"},
		BodyRoot:    "input",
	}
	input := map[string]any{
		"dataset_id":     "gd_lk5ns7kz21pck8jpis",
		"include_errors": "true",
		"input": []any{
			map[string]any{"url": "https://example.com/place/1"},
			map[string]any{"url": "https://example.com/place/2"},
		},
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "bright-secret"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/datasets/v3/trigger" {
		t.Fatalf("path=%q, want /datasets/v3/trigger", capturedPath)
	}
	if capturedQuery.Get("dataset_id") != "gd_lk5ns7kz21pck8jpis" || capturedQuery.Get("include_errors") != "true" {
		t.Fatalf("query=%v, want dataset_id + include_errors", capturedQuery)
	}
	var rows []map[string]any
	if err := json.Unmarshal(capturedBody, &rows); err != nil {
		t.Fatalf("body is not top-level JSON array: %v (body=%s)", err, capturedBody)
	}
	if len(rows) != 2 || rows[0]["url"] != "https://example.com/place/1" {
		t.Fatalf("rows mismatch: %v", rows)
	}
	if strings.Contains(string(capturedBody), "dataset_id") || strings.Contains(string(capturedBody), "input") {
		t.Fatalf("body leaked wrapper/query params: %s", capturedBody)
	}
}

func TestExecuteIntegrationTool_BrowseAIPathParams(t *testing.T) {
	var capturedPath string
	var capturedQuery url.Values
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"statusCode":200,"messageCode":"success"}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "browse-ai",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{
			Types:   []string{"bearer"},
			Headers: map[string]string{"Authorization": "Bearer {{token}}"},
		},
	}
	tool := &AppToolDef{
		Name:   "run_robot",
		Method: "POST",
		Path:   "/robots/{robot_id}/tasks",
	}
	input := map[string]any{
		"robot_id": "robot-123",
		"inputParameters": map[string]any{
			"originUrl": "https://example.com/products",
		},
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"token": "browse-secret"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/robots/robot-123/tasks" {
		t.Fatalf("path=%q, want /robots/robot-123/tasks", capturedPath)
	}
	if got := capturedQuery.Get("robot_id"); got != "" {
		t.Fatalf("robot_id leaked into query as %q", got)
	}
	if _, ok := capturedBody["robot_id"]; ok {
		t.Fatalf("robot_id leaked into body: %v", capturedBody)
	}
	params, ok := capturedBody["inputParameters"].(map[string]any)
	if !ok || params["originUrl"] != "https://example.com/products" {
		t.Fatalf("inputParameters mismatch: %v", capturedBody)
	}
}

func TestExecuteIntegrationTool_ToolHeadersOverrideAppHeaders(t *testing.T) {
	var capturedCT string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCT = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-twilio",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{Headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		}},
	}
	tool := &AppToolDef{
		Name:    "update_whatsapp_sender",
		Method:  "POST",
		Path:    "/v2/Channels/Senders/{SenderSid}",
		Headers: map[string]string{"Content-Type": "application/json"},
	}
	input := map[string]any{
		"SenderSid": "XE123",
		"webhook": map[string]any{
			"callback_url":    "https://example.test/webhooks/twilio-inbound",
			"callback_method": "POST",
		},
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedCT != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", capturedCT)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body is not JSON: %v body=%s", err, capturedBody)
	}
	if _, leaked := body["SenderSid"]; leaked {
		t.Fatalf("path param leaked into body: %s", capturedBody)
	}
	if _, ok := body["webhook"].(map[string]any); !ok {
		t.Fatalf("webhook object missing from body: %s", capturedBody)
	}
}

func TestExecuteIntegrationTool_StripeCheckoutSessionFormEncoding(t *testing.T) {
	var capturedCT string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCT = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"cs_test_123","url":"https://checkout.stripe.com/c/pay/cs_test_123"}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "stripe",
		BaseURL: srv.URL,
		Auth: AppAuthConfig{Headers: map[string]string{
			"Authorization": "Bearer {{token}}",
			"Content-Type":  "application/x-www-form-urlencoded",
		}},
	}
	tool := &AppToolDef{
		Name:   "create_checkout_session",
		Method: "POST",
		Path:   "/checkout/sessions",
	}
	input := map[string]any{
		"mode":        "payment",
		"success_url": "https://example.test/success?session_id={CHECKOUT_SESSION_ID}",
		"cancel_url":  "https://example.test/cancel",
		"line_items": []any{
			map[string]any{
				"quantity": float64(1),
				"price_data": map[string]any{
					"currency":    "usd",
					"unit_amount": float64(2000),
					"product_data": map[string]any{
						"name": "Starter",
					},
				},
			},
		},
		"metadata": map[string]any{
			"apteva_invoice_id": "123",
		},
	}
	res, err := executeIntegrationTool(app, tool, map[string]string{"token": "sk_test_123"}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d data=%v", res.Success, res.Status, res.Data)
	}
	if capturedCT != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type=%q, want application/x-www-form-urlencoded", capturedCT)
	}
	if strings.Contains(string(capturedBody), `"line_items"`) {
		t.Fatalf("body appears JSON/object-string encoded: %s", capturedBody)
	}
	values, err := url.ParseQuery(string(capturedBody))
	if err != nil {
		t.Fatalf("ParseQuery: %v body=%s", err, capturedBody)
	}
	assertFormValue := func(key, want string) {
		t.Helper()
		if got := values.Get(key); got != want {
			t.Fatalf("%s=%q, want %q; body=%s", key, got, want, capturedBody)
		}
	}
	assertFormValue("mode", "payment")
	assertFormValue("success_url", "https://example.test/success?session_id={CHECKOUT_SESSION_ID}")
	assertFormValue("line_items[0][price_data][currency]", "usd")
	assertFormValue("line_items[0][price_data][product_data][name]", "Starter")
	assertFormValue("line_items[0][quantity]", "1")
	assertFormValue("metadata[apteva_invoice_id]", "123")
	if values.Get("line_items") != "" {
		t.Fatalf("line_items encoded as scalar: %s", capturedBody)
	}
}

func TestExecuteIntegrationTool_NumericPathParamAvoidsScientificNotation(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := &AppTemplate{
		Slug:    "fake-ringover",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}},
	}
	tool := &AppToolDef{
		Name:   "start_ivr_callback",
		Method: "POST",
		Path:   "/ivrs/{ivr_id}/callback",
	}
	input := map[string]any{
		// json.Unmarshal decodes numbers into float64 in the HTTP executor path.
		"ivr_id":      float64(17560906),
		"to_number":   float64(34648257793),
		"from_number": float64(34930494946),
		"timeout":     float64(20),
	}

	res, err := executeIntegrationTool(app, tool, map[string]string{}, input, "")
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	if !res.Success {
		t.Fatalf("success=%v status=%d", res.Success, res.Status)
	}
	if capturedPath != "/ivrs/17560906/callback" {
		t.Fatalf("path=%q, want /ivrs/17560906/callback", capturedPath)
	}
	if strings.Contains(string(capturedBody), "ivr_id") {
		t.Fatalf("path param leaked into body: %s", capturedBody)
	}
}
