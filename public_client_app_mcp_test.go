package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicClientAppMCP_AllowsScopedActionForProjectInstall(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	user, project, rawKey := seedPublicClientKey(t, s, "catalog", []string{"catalog_prices_list"}, []string{"https://shop.example"}, 60)

	var seenAuth string
	var seenOriginalAuth string
	var seenProjectID string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenOriginalAuth = r.Header.Get("X-Apteva-Original-Authorization")
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Errorf("sidecar decode request: %v", err)
		}
		seenProjectID, _ = rpc.Params.Arguments["_project_id"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer sidecar.Close()

	s.installedApps.Add(&InstalledApp{InstallID: 101, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "install-token-project"})
	s.installedApps.Add(&InstalledApp{InstallID: 102, AppName: "catalog", ProjectID: "other-project", SidecarURL: sidecar.URL, Token: "install-token-other"})
	s.installedApps.Add(&InstalledApp{InstallID: 103, AppName: "catalog", ProjectID: "", SidecarURL: sidecar.URL, Token: "install-token-global"})

	w := servePublicClientMCP(t, s, rawKey, "https://shop.example", "catalog_prices_list")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if seenAuth != "Bearer install-token-project" {
		t.Fatalf("expected project install token, got %q", seenAuth)
	}
	if strings.Contains(seenOriginalAuth, rawKey) {
		t.Fatalf("public key leaked to sidecar through original auth header")
	}
	if seenProjectID != project.ID {
		t.Fatalf("expected injected project_id %q, got %q", project.ID, seenProjectID)
	}

	var storedLastUsed string
	if err := s.store.db.QueryRow("SELECT COALESCE(last_used, '') FROM api_keys WHERE user_id = ?", user.ID).Scan(&storedLastUsed); err != nil {
		t.Fatalf("query key last_used: %v", err)
	}
	if storedLastUsed == "" {
		t.Fatalf("expected public client key last_used to be updated")
	}
}

func TestPublicClientAppMCP_BlocksUnscopedAction(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	_, project, rawKey := seedPublicClientKey(t, s, "catalog", []string{"catalog_prices_list"}, []string{"https://shop.example"}, 60)
	var calls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 101, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "install-token"})

	w := servePublicClientMCP(t, s, rawKey, "https://shop.example", "catalog_admin_delete")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("sidecar should not be called for unscoped action")
	}
}

func TestPublicClientAppMCP_BlocksWrongOrigin(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	_, project, rawKey := seedPublicClientKey(t, s, "catalog", []string{"catalog_prices_list"}, []string{"https://shop.example"}, 60)
	var calls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 101, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "install-token"})

	w := servePublicClientMCP(t, s, rawKey, "https://evil.example", "catalog_prices_list")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("sidecar should not be called for wrong origin")
	}
}

func TestPublicClientAppMCP_RateLimitsPerKey(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	_, project, rawKey := seedPublicClientKey(t, s, "catalog", []string{"catalog_prices_list"}, []string{"https://shop.example"}, 1)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 101, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "install-token"})

	first := servePublicClientMCP(t, s, rawKey, "https://shop.example", "catalog_prices_list")
	if first.Code != http.StatusOK {
		t.Fatalf("expected first call 200, got %d: %s", first.Code, first.Body.String())
	}
	second := servePublicClientMCP(t, s, rawKey, "https://shop.example", "catalog_prices_list")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second call 429, got %d: %s", second.Code, second.Body.String())
	}
}

func seedPublicClientKey(t *testing.T, s *Server, appName string, actions, origins []string, rateLimit int) (*User, *Project, string) {
	t.Helper()
	user, err := s.store.CreateUser("public-client@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	project, err := s.store.CreateProject(user.ID, "Website", "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	scopes, _ := json.Marshal([]publicClientScope{{
		Type:    "app_action",
		App:     appName,
		Actions: actions,
	}})
	allowedOrigins, _ := json.Marshal(origins)
	rawKey := "pk_test_public_client_key"
	if _, err := s.store.CreateAPIKey(user.ID, "Website public client", HashAPIKey(rawKey), "pk_test", APIKeyCreateOptions{
		Kind:               "public_client",
		ProjectID:          project.ID,
		Scopes:             string(scopes),
		AllowedOrigins:     string(allowedOrigins),
		RateLimitPerMinute: rateLimit,
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return user, project, rawKey
}

func servePublicClientMCP(t *testing.T, s *Server, rawKey, origin, toolName string) *httptest.ResponseRecorder {
	t.Helper()
	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + toolName + `","arguments":{"q":"x"}}}`
	req := httptest.NewRequest(http.MethodPost, "/apps/catalog/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Origin", origin)
	w := httptest.NewRecorder()
	apiMux.ServeHTTP(w, req)
	return w
}
