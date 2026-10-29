package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// createRemoteMcpForTest registers a remote_mcp app, stores a connection
// with creds and runs createRemoteMcpFromConnection against it.
func createRemoteMcpForTest(t *testing.T, s *Server, app *AppTemplate, creds map[string]string) (*Connection, int64, error) {
	t.Helper()
	app.Kind = "remote_mcp"
	if app.Name == "" {
		app.Name = app.Slug
	}
	s.catalog.Register(app)
	credsJSON, _ := json.Marshal(creds)
	encCreds, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: app.Slug, AppName: app.Name, Name: "Site", AuthType: "basic",
		EncryptedCreds: encCreds, ProjectID: "demo", Source: "local", Status: "active",
	})
	if err != nil {
		t.Fatalf("create conn: %v", err)
	}
	id, err := s.createRemoteMcpFromConnection(1, conn, s.catalog.Get(app.Slug), encCreds)
	return conn, id, err
}

func newRemoteMcpTestServer(t *testing.T) *Server {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()
	return s
}

func wordpressMcpTestApp() *AppTemplate {
	return &AppTemplate{
		Slug: "wordpress-mcp",
		MCP: &RemoteMcpConfig{
			Transport:  "http",
			URL:        "{{site_url}}/wp-json/mcp/mcp-adapter-default-server",
			AuthHeader: &McpAuthHeaderTmpl{Name: "Authorization", Value: "Basic {{basic_auth}}"},
		},
		Auth: AppAuthConfig{Types: []string{"basic"}},
	}
}

func TestRemoteMcpTemplatedURLAndBasicAuth(t *testing.T) {
	s := newRemoteMcpTestServer(t)
	// IP literals resolve without DNS; 93.184.216.34 is public.
	_, mcpID, err := createRemoteMcpForTest(t, s, wordpressMcpTestApp(), map[string]string{
		"site_url": "https://93.184.216.34/",
		"username": "editor",
		"password": "abcd efgh ijkl",
	})
	if err != nil {
		t.Fatalf("createRemoteMcpFromConnection: %v", err)
	}
	rec, encEnv, err := s.store.GetMCPServer(1, mcpID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if want := "https://93.184.216.34/wp-json/mcp/mcp-adapter-default-server"; rec.URL != want {
		t.Errorf("url: got %q, want %q", rec.URL, want)
	}
	plain, err := Decrypt(s.secret, encEnv)
	if err != nil {
		t.Fatalf("decrypt env: %v", err)
	}
	var env map[string]string
	json.Unmarshal([]byte(plain), &env)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("editor:abcd efgh ijkl"))
	if env["AUTHORIZATION"] != want {
		t.Errorf("env[AUTHORIZATION]: got %q, want %q", env["AUTHORIZATION"], want)
	}
}

func TestRemoteMcpTemplatedURLRejected(t *testing.T) {
	for name, siteURL := range map[string]string{
		"unresolved placeholder": "",
		"plain http":             "http://93.184.216.34",
		"loopback":               "https://127.0.0.1:5280",
		"private network":        "https://10.0.0.5",
		"link-local metadata":    "https://169.254.169.254",
	} {
		t.Run(name, func(t *testing.T) {
			s := newRemoteMcpTestServer(t)
			creds := map[string]string{"username": "editor", "password": "pw"}
			if siteURL != "" {
				creds["site_url"] = siteURL
			}
			conn, _, err := createRemoteMcpForTest(t, s, wordpressMcpTestApp(), creds)
			if err == nil {
				t.Fatal("expected an error")
			}
			rows, _ := s.store.ListMCPServers(1, "demo")
			for _, r := range rows {
				if r.ConnectionID == conn.ID {
					t.Fatalf("mcp_servers row %d inserted despite error %v", r.ID, err)
				}
			}
		})
	}
}

// The three shipped remote_mcp entries must resolve exactly as they did
// before templates switched to the normalized credential view.
func TestRemoteMcpAuthHeaderUnchangedForShippedEntries(t *testing.T) {
	cases := []struct {
		name    string
		header  *McpAuthHeaderTmpl
		creds   map[string]string
		wantKey string
		want    string
	}{
		{"hubspot token", &McpAuthHeaderTmpl{Name: "Authorization", Value: "Bearer {{token}}"}, map[string]string{"token": "t1", "refresh_token": "r1"}, "AUTHORIZATION", "Bearer t1"},
		{"hubspot access_token", &McpAuthHeaderTmpl{Name: "Authorization", Value: "Bearer {{token}}"}, map[string]string{"access_token": "t2"}, "AUTHORIZATION", "Bearer t2"},
		{"altfins api key", &McpAuthHeaderTmpl{Name: "X-API-Key", Value: "{{api_key}}"}, map[string]string{"api_key": "k1"}, "API_KEY", "k1"},
		{"default header", nil, map[string]string{"token": "t3"}, "AUTHORIZATION", "Bearer t3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &AppTemplate{MCP: &RemoteMcpConfig{URL: "https://mcp.example.com/mcp", AuthHeader: tc.header}}
			rawKey, rawValue, rawErr := resolveRemoteMcpAuthHeader(app, tc.creds)
			key, value, err := resolveRemoteMcpAuthHeader(app, normalizeCredentials(tc.creds))
			if err != nil || rawErr != nil {
				t.Fatalf("resolve: %v / raw %v", err, rawErr)
			}
			if key != tc.wantKey || value != tc.want || key != rawKey || value != rawValue {
				t.Errorf("got %s=%q (raw %s=%q), want %s=%q", key, value, rawKey, rawValue, tc.wantKey, tc.want)
			}
			url, err := resolveRemoteMcpURL(t.Context(), app, normalizeCredentials(tc.creds))
			if err != nil || url != app.MCP.URL {
				t.Errorf("fixed url changed: %q, %v", url, err)
			}
		})
	}
}

func TestRefreshRemoteMcpAuthDoesNotPersistDerivedCredentials(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fresh","refresh_token":"r2"}`))
	}))
	defer tokens.Close()
	s := newRemoteMcpTestServer(t)
	conn, mcpID, err := createRemoteMcpForTest(t, s, &AppTemplate{
		Slug: "oauth-mcp",
		MCP:  &RemoteMcpConfig{Transport: "http", URL: "https://mcp.example.com/mcp"},
		Auth: AppAuthConfig{Types: []string{"oauth2"}, OAuth2: &OAuthConfig{TokenURL: tokens.URL}},
	}, map[string]string{
		"access_token":  "stale",
		"refresh_token": "r1",
		"client_id":     "cid",
		"username":      "editor",
		"password":      "pw",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env := map[string]string{}
	if err := s.refreshRemoteMcpAuth(mcpID, conn.ID, env); err != nil {
		t.Fatalf("refreshRemoteMcpAuth: %v", err)
	}
	if env["AUTHORIZATION"] != "Bearer fresh" {
		t.Errorf("env after refresh: %v", env)
	}
	_, encCreds, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	plain, _ := Decrypt(s.secret, encCreds)
	var stored map[string]string
	json.Unmarshal([]byte(plain), &stored)
	if stored["access_token"] != "fresh" {
		t.Errorf("refreshed token not persisted: %v", stored["access_token"])
	}
	// refreshOAuthAccessToken itself stores token/accessToken/refreshToken;
	// these keys only normalizeCredentials would add.
	for _, derived := range []string{"basic_auth", "bearer_token", "auth_token", "authToken"} {
		if _, ok := stored[derived]; ok {
			t.Errorf("derived key %q persisted into connection credentials", derived)
		}
	}
}
