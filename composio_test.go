package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// mockComposio spins up an httptest.Server whose handler is dispatched by the
// first matching (method, path-prefix) tuple. Each route can assert on the
// request (headers, query, body) and return a canned JSON response.
//
// Its Count() reports how many requests each route served so tests can verify
// pagination terminated, search bypassed the loop, reconcile looped, etc.
type mockComposio struct {
	t         *testing.T
	server    *httptest.Server
	routes    []*mockRoute
	gotAPIKey atomic.Value // string of last seen x-api-key
}

type mockRoute struct {
	method  string
	path    string // exact or prefix match ending with "/"
	calls   atomic.Int64
	handler func(w http.ResponseWriter, r *http.Request)
}

func newMockComposio(t *testing.T) *mockComposio {
	t.Helper()
	m := &mockComposio{t: t}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.Header.Get("x-api-key"); k != "" {
			m.gotAPIKey.Store(k)
		}
		for _, rt := range m.routes {
			if rt.method != "" && rt.method != r.Method {
				continue
			}
			match := rt.path == r.URL.Path ||
				(strings.HasSuffix(rt.path, "/") && strings.HasPrefix(r.URL.Path, rt.path))
			if !match {
				continue
			}
			rt.calls.Add(1)
			rt.handler(w, r)
			return
		}
		t.Errorf("unexpected composio request: %s %s (query=%s)", r.Method, r.URL.Path, r.URL.RawQuery)
		http.Error(w, "no mock route", http.StatusNotFound)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockComposio) on(method, path string, h func(http.ResponseWriter, *http.Request)) *mockRoute {
	rt := &mockRoute{method: method, path: path, handler: h}
	m.routes = append(m.routes, rt)
	return rt
}

func (m *mockComposio) client() *ComposioClient {
	c := NewComposioClient("test-api-key")
	c.BaseURL = m.server.URL
	return c
}

func writeMockJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- Client tests ---

func TestComposio_ListApps_SinglePage(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/toolkits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("expected limit=1000, got %s", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("cursor") != "" {
			t.Errorf("expected no cursor on first page, got %s", r.URL.Query().Get("cursor"))
		}
		writeMockJSON(w, 200, map[string]any{
			"items": []map[string]any{
				{"slug": "github", "name": "GitHub", "composio_managed_auth_schemes": []string{"oauth2"}},
				{"slug": "pushover", "name": "Pushover", "auth_schemes": []string{"api_key"}},
				{"slug": "deepwiki_mcp", "name": "DeepWiki MCP", "no_auth": true},
			},
		})
	})

	apps, err := m.client().ListApps("")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 3 {
		t.Fatalf("got %d apps, want 3", len(apps))
	}
	if apps[0].Slug != "github" || !apps[0].ComposioManaged {
		t.Errorf("github: slug=%q managed=%v", apps[0].Slug, apps[0].ComposioManaged)
	}
	if apps[1].Slug != "pushover" || apps[1].ComposioManaged {
		t.Errorf("pushover should not be composio_managed: %+v", apps[1])
	}
	if !apps[2].NoAuth {
		t.Errorf("deepwiki should be no_auth")
	}
	if got := m.gotAPIKey.Load().(string); got != "test-api-key" {
		t.Errorf("x-api-key header missing: %q", got)
	}
}

func TestComposio_ListApps_Pagination(t *testing.T) {
	m := newMockComposio(t)
	callIdx := atomic.Int64{}
	rt := m.on("GET", "/api/v3/toolkits", func(w http.ResponseWriter, r *http.Request) {
		n := callIdx.Add(1)
		switch n {
		case 1:
			writeMockJSON(w, 200, map[string]any{
				"items": []map[string]any{
					{"slug": "a", "name": "A"},
					{"slug": "b", "name": "B"},
				},
				"next_cursor": "cur-2",
			})
		case 2:
			if r.URL.Query().Get("cursor") != "cur-2" {
				t.Errorf("page 2 missing cursor: %s", r.URL.RawQuery)
			}
			writeMockJSON(w, 200, map[string]any{
				"items": []map[string]any{
					{"slug": "c", "name": "C"},
				},
				"next_cursor": "cur-3",
			})
		case 3:
			if r.URL.Query().Get("cursor") != "cur-3" {
				t.Errorf("page 3 missing cursor: %s", r.URL.RawQuery)
			}
			// Terminate with empty items
			writeMockJSON(w, 200, map[string]any{"items": []any{}})
		}
	})

	apps, err := m.client().ListApps("")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 3 {
		t.Fatalf("expected 3 aggregated apps, got %d", len(apps))
	}
	if apps[0].Slug != "a" || apps[2].Slug != "c" {
		t.Errorf("pagination order wrong: %v", apps)
	}
	if rt.calls.Load() != 3 {
		t.Errorf("expected 3 page fetches, got %d", rt.calls.Load())
	}
}

func TestComposio_ListApps_Search_SinglePageOnly(t *testing.T) {
	m := newMockComposio(t)
	rt := m.on("GET", "/api/v3/toolkits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("search") != "pushover" {
			t.Errorf("expected search=pushover, got %q", r.URL.Query().Get("search"))
		}
		writeMockJSON(w, 200, map[string]any{
			"items":       []map[string]any{{"slug": "pushover", "name": "Pushover"}},
			"next_cursor": "cur-2", // even if upstream offers more pages, search path should stop
		})
	})

	apps, err := m.client().ListApps("pushover")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Slug != "pushover" {
		t.Fatalf("unexpected search result: %+v", apps)
	}
	if rt.calls.Load() != 1 {
		t.Errorf("search should make exactly 1 request, got %d", rt.calls.Load())
	}
}

func TestComposio_EnsureAuthConfig_Reuses(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("toolkit_slug") != "github" {
			t.Errorf("expected toolkit_slug=github, got %s", r.URL.RawQuery)
		}
		writeMockJSON(w, 200, map[string]any{
			"items": []map[string]any{
				{
					"id":                  "ac_existing",
					"auth_scheme":         "OAUTH2",
					"is_composio_managed": true,
					"toolkit":             map[string]string{"slug": "github"},
				},
			},
		})
	})
	// POST should not be hit. If it is, the test server will 404 it.

	cfg, err := m.client().ensureAuthConfig("github", "", nil)
	if err != nil {
		t.Fatalf("ensureAuthConfig: %v", err)
	}
	if cfg.ID != "ac_existing" {
		t.Errorf("expected reuse of ac_existing, got %s", cfg.ID)
	}
}

func TestComposio_EnsureAuthConfig_CreatesWhenMissing(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"items": []any{}})
	})
	postCalled := false
	m.on("POST", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		postCalled = true
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		tk, _ := body["toolkit"].(map[string]any)
		if slug, _ := tk["slug"].(string); slug != "pushover" {
			t.Errorf("unexpected create body: %+v", body)
		}
		ac, _ := body["auth_config"].(map[string]any)
		if ty, _ := ac["type"].(string); ty != "use_composio_managed_auth" {
			t.Errorf("expected managed auth type, got %v", ac)
		}
		writeMockJSON(w, 201, map[string]any{
			"toolkit": map[string]string{"slug": "pushover"},
			"auth_config": map[string]any{
				"id":                  "ac_new",
				"auth_scheme":         "API_KEY",
				"is_composio_managed": true,
			},
		})
	})

	cfg, err := m.client().ensureAuthConfig("pushover", "", nil)
	if err != nil {
		t.Fatalf("ensureAuthConfig: %v", err)
	}
	if !postCalled {
		t.Errorf("expected POST /auth_configs to be called")
	}
	if cfg.ID != "ac_new" {
		t.Errorf("expected ac_new, got %s", cfg.ID)
	}
}

func TestComposio_EnsureAuthConfig_CustomAuthWithCreds(t *testing.T) {
	m := newMockComposio(t)
	// List returns empty so we go straight to POST
	m.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"items": []any{}})
	})
	m.on("POST", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ac, _ := body["auth_config"].(map[string]any)
		if ac["type"] != "use_custom_auth" {
			t.Errorf("expected use_custom_auth, got %v", ac["type"])
		}
		if ac["authScheme"] != "API_KEY" {
			t.Errorf("expected API_KEY scheme, got %v", ac["authScheme"])
		}
		creds, _ := ac["credentials"].(map[string]any)
		if creds["app_token"] != "atok" || creds["user_key"] != "ukey" {
			t.Errorf("expected creds app_token/user_key, got %v", creds)
		}
		writeMockJSON(w, 201, map[string]any{
			"auth_config": map[string]any{"id": "ac_custom", "auth_scheme": "API_KEY"},
		})
	})

	cfg, err := m.client().ensureAuthConfig("pushover", "API_KEY", map[string]string{
		"app_token": "atok",
		"user_key":  "ukey",
	})
	if err != nil {
		t.Fatalf("ensureAuthConfig: %v", err)
	}
	if cfg.ID != "ac_custom" {
		t.Errorf("expected ac_custom, got %s", cfg.ID)
	}
}

func TestComposio_InitiateConnection_AlwaysUsesLinkPath(t *testing.T) {
	// Every auth scheme now goes through the Connect Link flow. API_KEY /
	// BASIC / OAUTH all call POST /connected_accounts/link and return a
	// redirect URL the user must open. The direct POST /connected_accounts
	// endpoint is never used from our code — tools auto-inject credentials
	// only when they're stored via Composio's hosted credential form.
	for _, scheme := range []string{"API_KEY", "OAUTH2", "BASIC"} {
		t.Run(scheme, func(t *testing.T) {
			m := newMockComposio(t)
			m.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
				writeMockJSON(w, 200, map[string]any{"items": []any{}})
			})
			m.on("POST", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
				writeMockJSON(w, 201, map[string]any{
					"auth_config": map[string]any{"id": "ac_" + scheme, "auth_scheme": scheme},
				})
			})
			m.on("POST", "/api/v3/connected_accounts", func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected direct POST — must always use /link")
				writeMockJSON(w, 400, map[string]any{"error": "should not be called"})
			})
			m.on("POST", "/api/v3/connected_accounts/link", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["auth_config_id"] != "ac_"+scheme {
					t.Errorf("wrong auth_config_id: %v", body["auth_config_id"])
				}
				if body["user_id"] != "proj:x" {
					t.Errorf("wrong user_id: %v", body["user_id"])
				}
				writeMockJSON(w, 201, map[string]any{
					"redirect_url":         "https://composio.example/connect/" + scheme,
					"connected_account_id": "ca_" + scheme,
				})
			})

			acct, redirect, err := m.client().InitiateConnection("testtoolkit", scheme, "proj:x", nil, nil)
			if err != nil {
				t.Fatalf("InitiateConnection: %v", err)
			}
			if redirect != "https://composio.example/connect/"+scheme {
				t.Errorf("expected /link redirect URL, got %q", redirect)
			}
			if acct.Status != "INITIATED" {
				t.Errorf("expected INITIATED status, got %s", acct.Status)
			}
		})
	}
}

func TestComposio_GetToolkitDetails_PicksApiKeyMode(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/toolkits/pushover", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"slug":                          "pushover",
			"name":                          "Pushover",
			"composio_managed_auth_schemes": []string{}, // not managed
			"auth_guide_url":                "https://pushover.net/api",
			"auth_config_details": []map[string]any{
				{
					"mode": "api_key",
					"name": "API Key",
					"fields": map[string]any{
						"auth_config_creation": map[string]any{
							"required": []map[string]any{
								{"name": "app_token", "displayName": "App Token", "type": "string", "description": "", "required": true},
							},
							"optional": []any{},
						},
						"connected_account_initiation": map[string]any{
							"required": []map[string]any{
								{"name": "user_key", "displayName": "User Key", "type": "string", "description": "Your Pushover user key", "required": true},
							},
							"optional": []any{},
						},
					},
				},
			},
		})
	})

	d, err := m.client().GetToolkitDetails("pushover")
	if err != nil {
		t.Fatalf("GetToolkitDetails: %v", err)
	}
	if d.AuthMode != "api_key" || d.IsComposioManaged {
		t.Errorf("unexpected mode: %+v", d)
	}
	if len(d.ConfigFields) != 1 || d.ConfigFields[0].Name != "app_token" || !d.ConfigFields[0].Required {
		t.Errorf("config fields wrong: %+v", d.ConfigFields)
	}
	if len(d.InitFields) != 1 || d.InitFields[0].Name != "user_key" {
		t.Errorf("init fields wrong: %+v", d.InitFields)
	}
}

func TestComposio_GetToolkitDetails_PrefersManagedOAuth(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/toolkits/github", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"slug":                          "github",
			"name":                          "GitHub",
			"composio_managed_auth_schemes": []string{"OAUTH2"},
			"auth_config_details": []map[string]any{
				{"mode": "api_key", "name": "API Key", "fields": map[string]any{"auth_config_creation": map[string]any{"required": []any{}, "optional": []any{}}, "connected_account_initiation": map[string]any{"required": []any{}, "optional": []any{}}}},
				{"mode": "oauth2", "name": "OAuth 2.0", "fields": map[string]any{"auth_config_creation": map[string]any{"required": []any{}, "optional": []any{}}, "connected_account_initiation": map[string]any{"required": []any{}, "optional": []any{}}}},
			},
		})
	})

	d, err := m.client().GetToolkitDetails("github")
	if err != nil {
		t.Fatalf("GetToolkitDetails: %v", err)
	}
	if d.AuthMode != "oauth2" {
		t.Errorf("expected oauth2 to be picked, got %s", d.AuthMode)
	}
	if !d.IsComposioManaged {
		t.Errorf("expected managed=true")
	}
}

func TestComposio_InitiateConnection_FullFlow(t *testing.T) {
	m := newMockComposio(t)
	// Step 1: lookup returns empty
	m.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"items": []any{}})
	})
	// Step 2: create auth config
	m.on("POST", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 201, map[string]any{
			"auth_config": map[string]any{"id": "ac_x", "is_composio_managed": true},
		})
	})
	// Step 3: create link session
	m.on("POST", "/api/v3/connected_accounts/link", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["auth_config_id"] != "ac_x" {
			t.Errorf("expected auth_config_id=ac_x, got %v", body["auth_config_id"])
		}
		if body["user_id"] != "proj:sheets" {
			t.Errorf("expected user_id=proj:sheets, got %v", body["user_id"])
		}
		writeMockJSON(w, 201, map[string]any{
			"link_token":           "lnk_abc",
			"redirect_url":         "https://composio.example/authorize/abc",
			"expires_at":           "2099-01-01T00:00:00Z",
			"connected_account_id": "ca_abc",
		})
	})

	acct, redirect, err := m.client().InitiateConnection("pushover", "", "proj:sheets", nil, nil)
	if err != nil {
		t.Fatalf("InitiateConnection: %v", err)
	}
	if acct.ID != "ca_abc" || acct.Status != "INITIATED" || acct.AuthConfigID != "ac_x" {
		t.Errorf("unexpected account: %+v", acct)
	}
	if redirect != "https://composio.example/authorize/abc" {
		t.Errorf("wrong redirect: %s", redirect)
	}
}

func TestComposio_GetConnectedAccount(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/connected_accounts/ca_abc", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"id":     "ca_abc",
			"status": "ACTIVE",
		})
	})
	acct, err := m.client().GetConnectedAccount("ca_abc")
	if err != nil {
		t.Fatalf("GetConnectedAccount: %v", err)
	}
	if acct.Status != "ACTIVE" {
		t.Errorf("expected ACTIVE, got %s", acct.Status)
	}
}

func TestComposio_RevokeConnection(t *testing.T) {
	m := newMockComposio(t)
	called := false
	m.on("DELETE", "/api/v3/connected_accounts/ca_abc", func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeMockJSON(w, 200, map[string]any{"success": true})
	})
	if err := m.client().RevokeConnection("ca_abc"); err != nil {
		t.Fatalf("RevokeConnection: %v", err)
	}
	if !called {
		t.Errorf("expected DELETE /connected_accounts/ca_abc")
	}
}

func TestComposio_CreateMCPServer_UsesCustomEndpoint(t *testing.T) {
	m := newMockComposio(t)
	m.on("POST", "/api/v3/mcp/servers/custom", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "apteva-proj-sheets" {
			t.Errorf("unexpected name: %v", body["name"])
		}
		tks, _ := body["toolkits"].([]any)
		if len(tks) != 2 || tks[0] != "github" || tks[1] != "pushover" {
			t.Errorf("unexpected toolkits: %v", tks)
		}
		if body["managed_auth_via_composio"] != true {
			t.Errorf("managed_auth_via_composio flag missing")
		}
		writeMockJSON(w, 201, map[string]any{
			"id":      "srv_1",
			"name":    body["name"],
			"mcp_url": "https://mcp.composio.example/u/abc",
		})
	})

	srv, err := m.client().CreateMCPServer("apteva-proj-sheets", []string{"github", "pushover"}, nil, nil)
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if srv.URL != "https://mcp.composio.example/u/abc" || srv.ID != "srv_1" {
		t.Errorf("unexpected server: %+v", srv)
	}
}

func TestComposio_ErrorResponse_NonJSON404(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/toolkits", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, "<!DOCTYPE html><html>404 page</html>")
	})
	_, err := m.client().ListApps("")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "http 404") {
		t.Errorf("expected http 404 in error, got %q", err.Error())
	}
}

func TestComposio_McpServerNameFor_HashBased(t *testing.T) {
	// Every output should be within Composio's 4-30 char window and
	// deterministic for a given (user, toolkit-set) tuple.
	cases := []struct {
		user     string
		toolkits []string
	}{
		{"proj:abcd1234", []string{"github"}},
		{"user:1", []string{"pushover"}},
		{"proj:" + strings.Repeat("x", 80), []string{"github", "slack"}},
		{"proj:1775978777182-f067dd7b59b0d45f", []string{"pushover", "googlesheets"}},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got := mcpServerNameFor(tc.user, tc.toolkits)
		if len(got) < 4 || len(got) > 30 {
			t.Errorf("name %q out of bounds [4,30] (len=%d)", got, len(got))
		}
		if !strings.HasPrefix(got, "apteva-") {
			t.Errorf("name %q missing apteva- prefix", got)
		}
		key := tc.user + "|" + strings.Join(tc.toolkits, ",")
		if prev, ok := seen[got]; ok {
			t.Errorf("collision: %q and %q both produced %q", prev, key, got)
		}
		seen[got] = key

		// Determinism: same input twice → same output
		if got2 := mcpServerNameFor(tc.user, tc.toolkits); got2 != got {
			t.Errorf("non-deterministic: %q → %q then %q", key, got, got2)
		}
	}

	// Order-independence: same toolkits in different order → same name
	a := mcpServerNameFor("proj:x", []string{"github", "slack"})
	b := mcpServerNameFor("proj:x", []string{"slack", "github"})
	if a != b {
		t.Errorf("toolkit order should not matter: %q vs %q", a, b)
	}

	// Adding a toolkit → different name (so a new upstream server gets created)
	c := mcpServerNameFor("proj:x", []string{"github"})
	d := mcpServerNameFor("proj:x", []string{"github", "slack"})
	if c == d {
		t.Errorf("adding toolkits should change the name: both produced %q", c)
	}
}

func TestComposio_CreateMCPServer_DuplicateNameFallsBackToLookup(t *testing.T) {
	m := newMockComposio(t)
	// POST /custom returns a duplicate-name error
	m.on("POST", "/api/v3/mcp/servers/custom", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":1151,"slug":"MCP_DuplicateServerName","message":"An MCP server with name \"apteva-abc\" already exists"}}`)
	})
	// GET /mcp/servers returns the existing row
	m.on("GET", "/api/v3/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "apteva-abc" {
			t.Errorf("expected name filter apteva-abc, got %q", r.URL.Query().Get("name"))
		}
		writeMockJSON(w, 200, map[string]any{
			"items": []map[string]any{
				{"id": "srv_existing", "name": "apteva-abc", "mcp_url": "https://mcp.example/existing"},
			},
		})
	})

	srv, err := m.client().CreateMCPServer("apteva-abc", []string{"pushover"}, nil, nil)
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if srv.ID != "srv_existing" || srv.URL != "https://mcp.example/existing" {
		t.Errorf("expected existing server returned on dup, got %+v", srv)
	}
}

func TestComposio_FindMCPServerByName_ExactMatch(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"items": []map[string]any{
				// Partial match — should NOT be picked
				{"id": "srv_1", "name": "apteva-abc-longer", "mcp_url": "https://wrong.example"},
				// Exact match — should be picked
				{"id": "srv_2", "name": "apteva-abc", "mcp_url": "https://correct.example"},
			},
		})
	})
	srv, err := m.client().FindMCPServerByName("apteva-abc")
	if err != nil {
		t.Fatalf("FindMCPServerByName: %v", err)
	}
	if srv == nil || srv.ID != "srv_2" {
		t.Errorf("expected srv_2, got %+v", srv)
	}
}

func TestComposio_FindMCPServerByName_NoMatchReturnsNil(t *testing.T) {
	m := newMockComposio(t)
	m.on("GET", "/api/v3/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"items": []any{}})
	})
	srv, err := m.client().FindMCPServerByName("nope")
	if err != nil {
		t.Fatalf("FindMCPServerByName: %v", err)
	}
	if srv != nil {
		t.Errorf("expected nil, got %+v", srv)
	}
}

// --- Server-level integration tests with a mock Composio + real Store ---

// newComposioTestServer wires a *Server backed by a real SQLite store, a live
// secret, and a ComposioClient pointed at the given mock. It seeds a Composio
// provider row and returns its providerID.
func newComposioTestServer(t *testing.T, mock *mockComposio) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	s.secret = testSecret()
	s.mcpManager = NewMCPManager()
	s.catalog = NewAppCatalog()
	s.port = "0"

	// Create a user
	user, err := s.store.CreateUser("test@example.com", "hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Seed a Composio provider row with the mock's URL embedded in its data.
	// Our client lookups rely on composioClientFor reading the encrypted API
	// key; to inject the mock base URL we monkey-patch by overriding the
	// composioClientFor via a test hook defined below.
	data, _ := json.Marshal(map[string]string{"COMPOSIO_API_KEY": "test-key"})
	enc, err := Encrypt(s.secret, string(data))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	p, err := s.store.CreateProvider(user.ID, 9, "integrations", "Composio", enc, "")
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	// Install a per-test hook so composioClientFor returns a client pointing
	// at the mock server.
	testComposioBaseURL = mock.server.URL
	t.Cleanup(func() { testComposioBaseURL = "" })

	return s, p.ID
}

// stubComposioMCPReconcile wires up the three routes the per-toolkit
// reconciler needs for a happy-path create: custom server create (one call
// per toolkit), instance create (one per server id), and URL generate (one
// per server). All three handlers are registered with prefix/catch-all
// behavior so any toolkit the reconciler sends works.
//
// Upstream server IDs are derived from the request body's "name" so each
// toolkit maps to a distinct srv_* id, and /generate returns a URL that
// embeds the name for easy assertion in tests.
func stubComposioMCPReconcile(m *mockComposio) {
	// Register exact-match routes first so they take precedence over the
	// prefix catch-all below (mockComposio uses first-match-wins ordering).
	m.on("POST", "/api/v3/mcp/servers/custom", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		writeMockJSON(w, 201, map[string]any{
			"id":      "srv_" + name,
			"name":    name,
			"mcp_url": "https://mcp.example/base/" + name,
		})
	})
	m.on("POST", "/api/v3/mcp/servers/generate", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		srvID, _ := body["mcp_server_id"].(string)
		writeMockJSON(w, 200, map[string]any{
			"mcp_url":      "https://mcp.example/base",
			"user_ids_url": []string{"https://mcp.example/u/" + srvID},
		})
	})
	// Prefix match on .../instances — registered LAST so exact routes win.
	m.on("POST", "/api/v3/mcp/servers/", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 201, map[string]any{
			"id":            "inst_1",
			"instance_id":   "proj:sheets",
			"mcp_server_id": "unknown",
		})
	})
}

// listRemoteMCPs returns the remote mcp_servers rows for a given project as
// a map keyed by name, so tests can assert per-toolkit row shape.
func listRemoteMCPs(t *testing.T, s *Server, userID int64, projectID string) map[string]MCPServerRecord {
	t.Helper()
	rows, _ := s.store.ListMCPServers(userID, projectID)
	out := map[string]MCPServerRecord{}
	for _, r := range rows {
		if r.Source == "remote" {
			out[r.Name] = r
		}
	}
	return out
}

func TestReconcileComposioMCPServer_CreatesOneRowPerToolkit(t *testing.T) {
	mock := newMockComposio(t)
	stubComposioMCPReconcile(mock)

	s, providerID := newComposioTestServer(t, mock)

	// Seed two active Composio connections in the "sheets" project
	for _, slug := range []string{"github", "pushover"} {
		_, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID: 1, AppSlug: slug, AppName: slug, Name: slug,
			AuthType: "composio", ProjectID: "sheets", Source: "composio",
			Status: "active", ProviderID: providerID, ExternalID: "ca_" + slug,
		})
		if err != nil {
			t.Fatalf("CreateConnectionExt: %v", err)
		}
	}

	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rows := listRemoteMCPs(t, s, 1, "sheets")
	if len(rows) != 2 {
		t.Fatalf("expected 2 remote rows (one per toolkit), got %d: %v", len(rows), rows)
	}
	for _, slug := range []string{"github", "pushover"} {
		r, ok := rows[slug]
		if !ok {
			t.Errorf("expected remote row named %q, got rows %v", slug, rows)
			continue
		}
		if r.Source != "remote" || r.Transport != "http" {
			t.Errorf("row %s: wrong source/transport: %+v", slug, r)
		}
		if r.URL == "" {
			t.Errorf("row %s: empty URL", slug)
		}
	}
}

func TestReconcileComposioMCPServer_IdempotentAndAddsIncrementally(t *testing.T) {
	mock := newMockComposio(t)
	stubComposioMCPReconcile(mock)

	s, providerID := newComposioTestServer(t, mock)

	// First connection — reconcile creates one row.
	s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "github", Name: "github",
		AuthType: "composio", ProjectID: "sheets", Source: "composio",
		Status: "active", ProviderID: providerID, ExternalID: "ca1",
	})
	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := len(listRemoteMCPs(t, s, 1, "sheets")); got != 1 {
		t.Errorf("after 1st reconcile: expected 1 row, got %d", got)
	}

	// Second connection for a DIFFERENT toolkit — reconcile adds a row
	// without replacing the first.
	s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "pushover", Name: "pushover",
		AuthType: "composio", ProjectID: "sheets", Source: "composio",
		Status: "active", ProviderID: providerID, ExternalID: "ca2",
	})
	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	rows := listRemoteMCPs(t, s, 1, "sheets")
	if len(rows) != 2 {
		t.Errorf("after 2nd reconcile: expected 2 rows, got %d: %v", len(rows), rows)
	}
	if _, ok := rows["github"]; !ok {
		t.Errorf("github row should still be present after adding pushover")
	}
	if _, ok := rows["pushover"]; !ok {
		t.Errorf("pushover row should be present")
	}

	// Third reconcile with no connection changes — still 2 rows, no drift.
	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if got := len(listRemoteMCPs(t, s, 1, "sheets")); got != 2 {
		t.Errorf("after idempotent 3rd reconcile: expected 2 rows, got %d", got)
	}
}

func TestReconcileComposioMCPServer_RemovesRowOnConnectionDelete(t *testing.T) {
	mock := newMockComposio(t)
	stubComposioMCPReconcile(mock)

	s, providerID := newComposioTestServer(t, mock)

	s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "github", Name: "github",
		AuthType: "composio", ProjectID: "sheets", Source: "composio",
		Status: "active", ProviderID: providerID, ExternalID: "ca1",
	})
	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if got := len(listRemoteMCPs(t, s, 1, "sheets")); got != 1 {
		t.Fatalf("expected 1 row after create, got %d", got)
	}

	// Delete the connection and reconcile — the row for its toolkit should
	// be reaped.
	conns, _ := s.store.ListConnections(1, "sheets")
	s.store.DeleteConnection(1, conns[0].ID)
	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile teardown: %v", err)
	}
	if got := len(listRemoteMCPs(t, s, 1, "sheets")); got != 0 {
		t.Errorf("expected 0 rows after connection deleted, got %d", got)
	}
}

func TestReconcileComposioMCPServer_ReapsLegacyAggregateRow(t *testing.T) {
	// Simulate a DB with the pre-refactor aggregate "composio" row still
	// present. The reconciler should drop it on first run since no active
	// connection has the slug "composio".
	mock := newMockComposio(t)
	stubComposioMCPReconcile(mock)

	s, providerID := newComposioTestServer(t, mock)

	// Seed a legacy aggregate row directly.
	_, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID:      1,
		Name:        "composio", // legacy aggregate name
		Description: "Composio hosted MCP — 1 toolkits",
		Source:      "remote",
		Transport:   "http",
		URL:         "https://mcp.example/legacy",
		ProviderID:  providerID,
		ProjectID:   "sheets",
	})
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Seed a real connection so the reconciler has something to do.
	s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "pushover", Name: "pushover",
		AuthType: "composio", ProjectID: "sheets", Source: "composio",
		Status: "active", ProviderID: providerID, ExternalID: "ca1",
	})

	if err := s.reconcileComposioMCPServer(1, providerID, "sheets"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rows := listRemoteMCPs(t, s, 1, "sheets")
	if _, legacyStill := rows["composio"]; legacyStill {
		t.Errorf("legacy aggregate row should have been reaped, still present")
	}
	if _, ok := rows["pushover"]; !ok {
		t.Errorf("pushover row should be present after reconcile")
	}
}

func TestHandleListComposioApps_EndToEnd(t *testing.T) {
	mock := newMockComposio(t)
	mock.on("GET", "/api/v3/toolkits", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"items": []map[string]any{{"slug": "pushover", "name": "Pushover"}},
		})
	})
	s, providerID := newComposioTestServer(t, mock)

	req := httptest.NewRequest("GET", fmt.Sprintf("/composio/apps?provider_id=%d&search=pushover", providerID), nil)
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleListComposioApps(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var apps []ComposioApp
	_ = json.Unmarshal(w.Body.Bytes(), &apps)
	if len(apps) != 1 || apps[0].Slug != "pushover" {
		t.Errorf("unexpected response: %+v", apps)
	}
}

func TestHandleCreateConnection_Composio_InitiatesAndStoresPending(t *testing.T) {
	mock := newMockComposio(t)
	mock.on("GET", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"items": []any{}})
	})
	mock.on("POST", "/api/v3/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 201, map[string]any{
			"auth_config": map[string]any{"id": "ac_x", "is_composio_managed": true},
		})
	})
	mock.on("POST", "/api/v3/connected_accounts/link", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 201, map[string]any{
			"link_token":           "lnk",
			"redirect_url":         "https://composio.example/authorize/xyz",
			"connected_account_id": "ca_xyz",
			"expires_at":           "2099-01-01T00:00:00Z",
		})
	})
	s, providerID := newComposioTestServer(t, mock)

	body := map[string]any{
		"source":      "composio",
		"provider_id": providerID,
		"app_slug":    "pushover",
		"name":        "my pushover",
		"project_id":  "sheets",
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/connections", strings.NewReader(string(data)))
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateConnection(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Connection  Connection `json:"connection"`
		RedirectURL string     `json:"redirect_url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RedirectURL != "https://composio.example/authorize/xyz" {
		t.Errorf("wrong redirect: %s", resp.RedirectURL)
	}
	if resp.Connection.Status != "pending" {
		t.Errorf("expected pending, got %s", resp.Connection.Status)
	}
	if resp.Connection.Source != "composio" || resp.Connection.ExternalID != "ca_xyz" {
		t.Errorf("row fields wrong: %+v", resp.Connection)
	}
}

func TestHandleGetConnection_ComposioPending_FlipsActiveAndReconciles(t *testing.T) {
	mock := newMockComposio(t)
	mock.on("GET", "/api/v3/connected_accounts/ca_xyz", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{"id": "ca_xyz", "status": "ACTIVE"})
	})
	stubComposioMCPReconcile(mock)

	s, providerID := newComposioTestServer(t, mock)

	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "pushover", AppName: "pushover", Name: "my pushover",
		AuthType: "composio", ProjectID: "sheets", Source: "composio",
		Status: "pending", ProviderID: providerID, ExternalID: "ca_xyz",
	})
	if err != nil {
		t.Fatalf("CreateConnectionExt: %v", err)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/connections/%d", conn.ID), nil)
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleGetConnection(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got Connection
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Status != "active" {
		t.Errorf("expected status=active, got %s", got.Status)
	}

	// Reconcile should have created a remote MCP row named after the toolkit.
	rows := listRemoteMCPs(t, s, 1, "sheets")
	row, ok := rows["pushover"]
	if !ok {
		t.Fatalf("expected remote MCP row named 'pushover', got %v", rows)
	}
	if row.URL == "" {
		t.Errorf("expected non-empty MCP url, got empty")
	}
}

func TestComposioClientFor_RejectsWrongProvider(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	user, _ := s.store.CreateUser("x@y.z", "hash")
	data, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": "k"})
	enc, _ := Encrypt(s.secret, string(data))
	p, _ := s.store.CreateProvider(user.ID, 2, "llm", "OpenAI", enc, "")

	_, err := s.composioClientFor(user.ID, p.ID)
	if err == nil || !strings.Contains(err.Error(), "not Composio") {
		t.Errorf("expected 'not Composio' error, got %v", err)
	}
}
