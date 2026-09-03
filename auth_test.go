package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// Reset rate limiters for clean test state
	loginLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}
	registerLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}
	mfaLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}
	publicClientRateMu.Lock()
	publicClientRateBuckets = map[int64]publicClientRateBucket{}
	publicClientRateMu.Unlock()

	return &Server{
		store:          store,
		agents:         NewAgentManager(t.TempDir(), "echo"),
		broadcaster:    NewTelemetryBroadcaster(),
		regMode:        "open",
		instanceSecret: "test-secret",
		secret:         bytes.Repeat([]byte{0x42}, 32),
	}
}

func postJSON(t *testing.T, handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	return result
}

// getSessionCookie extracts the session cookie from a response.
func getSessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// --- Register ---

func TestRegister_Success(t *testing.T) {
	s := newTestServer(t)
	w := postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	if body["email"] != "alice@test.com" {
		t.Errorf("expected alice@test.com, got %v", body["email"])
	}
}

func TestRegister_Duplicate(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	w := postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "different123",
	})
	if w.Code != 409 {
		t.Errorf("expected 409 for duplicate, got %d", w.Code)
	}
}

func TestRegister_ShortPassword(t *testing.T) {
	s := newTestServer(t)
	w := postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "short",
	})
	if w.Code != 400 {
		t.Errorf("expected 400 for short password, got %d", w.Code)
	}
}

func TestRegister_MissingFields(t *testing.T) {
	s := newTestServer(t)
	w := postJSON(t, s.handleRegister, map[string]string{"email": ""})
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRegister_WrongMethod(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/auth/register", nil)
	w := httptest.NewRecorder()
	s.handleRegister(w, req)
	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// --- Login ---

func TestLogin_Success(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	w := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cookie := getSessionCookie(w)
	if cookie == "" {
		t.Error("expected session cookie to be set")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	w := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "wrongpassword",
	})
	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestLogin_NoUser(t *testing.T) {
	s := newTestServer(t)
	w := postJSON(t, s.handleLogin, map[string]string{
		"email": "nobody@test.com", "password": "password123",
	})
	if w.Code != 401 {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// --- Auth Middleware ---

func TestAuthMiddleware_NoToken(t *testing.T) {
	s := newTestServer(t)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401 without token, got %d", w.Code)
	}
}

func TestAuthMiddleware_CoreKeyIsScopedToLoopbackRuntimeToken(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("core-runtime-token@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "Codex agent", "directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	const coreKey = "core_runtime_token_test"
	if _, err := s.store.db.Exec(`UPDATE agents SET status='running', core_api_key=?, pid=42, port=4242 WHERE id=?`, coreKey, agent.ID); err != nil {
		t.Fatal(err)
	}

	called := false
	gotUserID := ""
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotUserID = r.Header.Get("X-User-ID")
		w.WriteHeader(http.StatusNoContent)
	})
	request := func(method, path, remoteAddr string) *httptest.ResponseRecorder {
		t.Helper()
		called = false
		gotUserID = ""
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("Authorization", "Bearer "+coreKey)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	for _, path := range []string{
		"/providers/7/auth/runtime-token",
		"/api/providers/7/auth/runtime-token",
	} {
		rec := request(http.MethodPost, path, "127.0.0.1:43123")
		if rec.Code != http.StatusNoContent || !called || gotUserID != itoa(user.ID) {
			t.Fatalf("core callback %s status=%d called=%v user=%q", path, rec.Code, called, gotUserID)
		}
	}

	for _, tc := range []struct {
		method string
		path   string
		remote string
	}{
		{http.MethodPost, "/connections/1/reauth", "127.0.0.1:43123"},
		{http.MethodPost, "/apiproviders/7/auth/runtime-token", "127.0.0.1:43123"},
		{http.MethodPost, "/providers/7/auth/runtime-token/extra", "127.0.0.1:43123"},
		{http.MethodPost, "/providers/not-a-number/auth/runtime-token", "127.0.0.1:43123"},
		{http.MethodDelete, "/providers/7/auth/runtime-token", "127.0.0.1:43123"},
		{http.MethodPost, "/providers/7/auth/runtime-token", "203.0.113.9:43123"},
	} {
		rec := request(tc.method, tc.path, tc.remote)
		if rec.Code != http.StatusUnauthorized || called {
			t.Errorf("core key escaped scope: %s %s from %s status=%d called=%v", tc.method, tc.path, tc.remote, rec.Code, called)
		}
	}

	if _, err := s.store.db.Exec(`UPDATE agents SET status='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	rec := request(http.MethodPost, "/providers/7/auth/runtime-token", "127.0.0.1:43123")
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("stopped core key remained valid: status=%d called=%v", rec.Code, called)
	}
}

func TestAuthMiddleware_AnonymousAppGETRequiresManifestDeclaration(t *testing.T) {
	s := newTestServer(t)
	called := false
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/apps/storage/files/6/content/x.mp4", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if called || w.Code != http.StatusUnauthorized {
		t.Fatalf("undeclared anonymous app route got called=%v status=%d", called, w.Code)
	}
}

func TestAuthMiddleware_SignedURLDoesNotBypassManifestAuth(t *testing.T) {
	s := newTestServer(t)
	called := false
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/apps/storage/files/6/content?sig=forged&exp=9999999999", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if called || w.Code != http.StatusUnauthorized {
		t.Fatalf("signed URL parameters bypassed manifest auth: called=%v status=%d", called, w.Code)
	}
}

// Management surfaces always require auth, never fall through —
// even on GET. Without this an unauthed user could enumerate the
// install list, the marketplace, etc.
func TestAuthMiddleware_AnonymousManagementGET_StillRefused(t *testing.T) {
	s := newTestServer(t)
	for _, sub := range []string{"installs", "callback", "preview", "install", "marketplace"} {
		req := httptest.NewRequest("GET", "/apps/"+sub+"/anything", nil)
		w := httptest.NewRecorder()
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})(w, req)
		if w.Code != 401 {
			t.Errorf("management route %q let an anonymous GET through (status %d)", sub, w.Code)
		}
	}
}

// POST/PUT/DELETE on an app route does NOT fall through — even
// public files are read-only via the anonymous path. Mutations
// always require auth.
func TestAuthMiddleware_AnonymousAppMutation_Refused(t *testing.T) {
	s := newTestServer(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req := httptest.NewRequest(method, "/apps/storage/files/6", nil)
		w := httptest.NewRecorder()
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})(w, req)
		if w.Code != 401 {
			t.Errorf("%s on app route should require auth (status %d)", method, w.Code)
		}
	}
}

func TestAuthMiddleware_AnonymousNoAuthAppRoute_AllowsMutation(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 1,
		AppName:   "functions",
		ProjectID: "proj-1",
		Manifest: sdk.Manifest{
			Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
				{Prefix: "/url/", NoAuth: true},
			}},
		},
	})

	called := false
	var sawUserID string
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		sawUserID = r.Header.Get("X-User-ID")
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("POST", "/apps/functions/url/ingest/token?project_id=proj-1", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if !called {
		t.Fatalf("anonymous POST to no_auth app route should fall through; got %d", w.Code)
	}
	if sawUserID != "" {
		t.Errorf("anonymous no_auth request leaked X-User-ID = %q", sawUserID)
	}
}

func TestAuthMiddleware_AnonymousNoAuthAppRoute_MethodSpecific(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 1,
		AppName:   "hooks",
		Manifest: sdk.Manifest{
			Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
				{Method: "POST", Prefix: "/public", NoAuth: true},
			}},
		},
	})

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	post := httptest.NewRequest("POST", "/apps/hooks/public", nil)
	postW := httptest.NewRecorder()
	handler(postW, post)
	if postW.Code != 200 {
		t.Fatalf("POST to matching no_auth route got %d", postW.Code)
	}

	patch := httptest.NewRequest("PATCH", "/apps/hooks/public", nil)
	patchW := httptest.NewRecorder()
	handler(patchW, patch)
	if patchW.Code != 401 {
		t.Fatalf("PATCH to POST-only no_auth route got %d, want 401", patchW.Code)
	}
}

func TestAuthMiddleware_AnonymousNoAuthAppRoute_ServeMuxParameter(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 1,
		AppName:   "push",
		Manifest: sdk.Manifest{
			Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
				{Method: http.MethodPost, Prefix: "/v1/deliveries", NoAuth: true},
				{Method: http.MethodPost, Prefix: "/v1/devices/{id}/test", NoAuth: true},
			}},
		},
	})

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer push_relay_grant" {
			t.Errorf("Authorization = %q, want relay grant preserved", got)
		}
		w.WriteHeader(http.StatusCreated)
	})
	for _, path := range []string{
		"/apps/push/v1/deliveries",
		"/apps/push/v1/devices/device-123/test",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer push_relay_grant")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("POST %s status=%d, want 201", path, rec.Code)
		}
	}

	for _, path := range []string{
		"/apps/push/v1/devices/device-123",
		"/apps/push/v1/devices/device-123/test/extra",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer push_relay_grant")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s status=%d, want 401", path, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/push/v1/devices/device-123/test", nil)
	req.Header.Set("Authorization", "Bearer push_relay_grant")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET method on POST-only parameter route status=%d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_AnonymousNoAuthAppRoute_DoesNotExposeOtherRoutes(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 1,
		AppName:   "functions",
		Manifest: sdk.Manifest{
			Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
				{Prefix: "/url/", NoAuth: true},
			}},
		},
	})

	req := httptest.NewRequest("POST", "/apps/functions/fn/old-private-path", nil)
	w := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})(w, req)
	if w.Code != 401 {
		t.Fatalf("anonymous POST outside no_auth route got %d, want 401", w.Code)
	}
}

func TestAuthMiddleware_SessionCookie(t *testing.T) {
	s := newTestServer(t)

	// Register + login to get cookie
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)
	if cookie == "" {
		t.Fatal("no session cookie from login")
	}

	// Use cookie in middleware
	var gotUserID string
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = r.Header.Get("X-User-ID")
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 with valid cookie, got %d", w.Code)
	}
	if gotUserID == "" {
		t.Error("expected X-User-ID to be set")
	}
}

func TestAuthMiddleware_APIKey(t *testing.T) {
	s := newTestServer(t)

	// Register + create API key directly via store
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	req := httptest.NewRequest("POST", "/auth/keys", bytes.NewReader([]byte(`{"name":"test"}`)))
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateKey(w, req)

	keyBody := decodeJSON(t, w)
	apiKey := keyBody["key"].(string)

	// Use API key in middleware
	var gotUserID string
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = r.Header.Get("X-User-ID")
		w.WriteHeader(200)
	})
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "Bearer "+apiKey)
	w2 := httptest.NewRecorder()
	handler(w2, req2)

	if w2.Code != 200 {
		t.Errorf("expected 200 with API key, got %d", w2.Code)
	}
	if gotUserID == "" {
		t.Error("expected X-User-ID to be set via API key")
	}
}

func TestAuthMiddleware_PublicClientKeyDoesNotGrantFullAccess(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	project, err := s.store.CreateProject(1, "Website", "", "")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"name":            "website",
		"kind":            "public_client",
		"project_id":      project.ID,
		"scopes":          []map[string]any{{"type": "app_action", "app": "example", "actions": []string{"action.name"}}},
		"allowed_origins": []string{"https://example.com"},
	})
	req := httptest.NewRequest("POST", "/auth/keys", bytes.NewReader(body))
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateKey(w, req)
	if w.Code != 200 {
		t.Fatalf("create scoped key: %d %s", w.Code, w.Body.String())
	}
	keyBody := decodeJSON(t, w)
	apiKey := keyBody["key"].(string)
	if !strings.HasPrefix(apiKey, "pk_") {
		t.Fatalf("public client key prefix = %q", apiKey)
	}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req2 := httptest.NewRequest("GET", "/api/agents", nil)
	req2.Header.Set("Authorization", "Bearer "+apiKey)
	w2 := httptest.NewRecorder()
	handler(w2, req2)

	if w2.Code != 401 {
		t.Fatalf("public client key should not grant full API access, got %d", w2.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	s := newTestServer(t)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "invalid-token-here"})
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != 401 {
		t.Errorf("expected 401 with invalid token, got %d", w.Code)
	}
}

func TestAuthMiddleware_InternalGatewaySecretLoopback(t *testing.T) {
	s := newTestServer(t)
	var gotUserID string
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = r.Header.Get("X-User-ID")
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.RemoteAddr = "127.0.0.1:49152"
	req.Header.Set("X-Agent-Secret", "test-secret")
	req.Header.Set("X-Apteva-MCP-User-ID", "42")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 with internal gateway auth, got %d", w.Code)
	}
	if gotUserID != "42" {
		t.Fatalf("expected X-User-ID 42, got %q", gotUserID)
	}
}

func TestAuthMiddleware_InternalGatewaySecretRejectsNonLoopback(t *testing.T) {
	s := newTestServer(t)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.RemoteAddr = "203.0.113.10:49152"
	req.Header.Set("X-Agent-Secret", "test-secret")
	req.Header.Set("X-Apteva-MCP-User-ID", "42")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401 from non-loopback internal gateway auth, got %d", w.Code)
	}
}

// --- API Keys endpoints ---

func TestCreateKey(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	req := httptest.NewRequest("POST", "/auth/keys", bytes.NewReader([]byte(`{"name":"prod"}`)))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleCreateKey(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	key := body["key"].(string)
	if len(key) < 10 {
		t.Errorf("expected long key, got %s", key)
	}
	if body["prefix"] == nil {
		t.Error("expected prefix")
	}
	if body["kind"] != "private" {
		t.Fatalf("kind = %v, want private", body["kind"])
	}
}

func TestCreateScopedClientKey(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	project, err := s.store.CreateProject(1, "Website", "", "")
	if err != nil {
		t.Fatal(err)
	}

	bodyBytes, _ := json.Marshal(map[string]any{
		"name":                  "website",
		"kind":                  "public_client",
		"project_id":            project.ID,
		"scopes":                []map[string]any{{"type": "app_action", "app": "example", "actions": []string{"action.name"}}},
		"allowed_origins":       []string{"https://example.com"},
		"rate_limit_per_minute": 30,
	})
	req := httptest.NewRequest("POST", "/auth/keys", bytes.NewReader(bodyBytes))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleCreateKey(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	if key := body["key"].(string); !strings.HasPrefix(key, "pk_") {
		t.Fatalf("key = %q, want pk_ prefix", key)
	}

	listReq := httptest.NewRequest("GET", "/auth/keys", nil)
	listReq.Header.Set("X-User-ID", "1")
	listW := httptest.NewRecorder()
	s.handleListKeys(listW, listReq)
	var keys []map[string]any
	json.Unmarshal(listW.Body.Bytes(), &keys)
	if len(keys) != 1 {
		t.Fatalf("keys len = %d, want 1", len(keys))
	}
	if keys[0]["kind"] != "public_client" || keys[0]["project_id"] != project.ID {
		t.Fatalf("listed key metadata = %#v", keys[0])
	}
	if keys[0]["rate_limit_per_minute"].(float64) != 30 {
		t.Fatalf("rate limit = %#v", keys[0]["rate_limit_per_minute"])
	}
}

func TestListKeys(t *testing.T) {
	s := newTestServer(t)
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	for _, name := range []string{"key1", "key2"} {
		req := httptest.NewRequest("POST", "/auth/keys", bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
		req.Header.Set("X-User-ID", "1")
		w := httptest.NewRecorder()
		s.handleCreateKey(w, req)
	}

	req := httptest.NewRequest("GET", "/auth/keys", nil)
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleListKeys(w, req)

	var keys []map[string]any
	json.Unmarshal(w.Body.Bytes(), &keys)
	if len(keys) != 2 {
		t.Fatalf("expected 2, got %d", len(keys))
	}
}

// --- Full flow via HTTP server ---

func TestFullServer_CookieAuthFlow(t *testing.T) {
	s := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/me", s.handleMe)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/instances", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleListInstances(w, r)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Use a cookie jar so cookies persist across requests
	jar := &testCookieJar{}
	client := &http.Client{Jar: jar}

	// Register
	regBody, _ := json.Marshal(map[string]string{
		"email": "test@test.com", "password": "testtest123",
	})
	resp, _ := client.Post(srv.URL+"/auth/register", "application/json", bytes.NewReader(regBody))
	if resp.StatusCode != 200 {
		t.Fatalf("register: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Login — should set cookie
	resp, _ = client.Post(srv.URL+"/auth/login", "application/json", bytes.NewReader(regBody))
	if resp.StatusCode != 200 {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /auth/me — should work with cookie
	resp, _ = client.Get(srv.URL + "/auth/me")
	if resp.StatusCode != 200 {
		t.Fatalf("me: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List instances — should work with cookie
	resp, _ = client.Get(srv.URL + "/instances")
	if resp.StatusCode != 200 {
		t.Fatalf("instances: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Without cookie should fail
	resp, _ = http.Get(srv.URL + "/instances")
	if resp.StatusCode != 401 {
		t.Errorf("unauth: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAuthPreferencesLanguagePersistsIntoMe(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("language@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"language": "fr-FR"})
	req := httptest.NewRequest(http.MethodPut, "/auth/preferences", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preferences status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	token := "language-session"
	if err := s.store.CreateSession(token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w = httptest.NewRecorder()
	s.handleMe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["language"] != "fr" {
		t.Fatalf("language=%v, want fr", resp["language"])
	}
}

func TestAuthPreferencesInterfaceLevelPersistsIntoMe(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("interface@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.store.GetUserInterfaceLevel(user.ID); got != "business" {
		t.Fatalf("new user interface level=%q, want business", got)
	}

	body := []byte(`{"interface_level":"personal"}`)
	req := httptest.NewRequest(http.MethodPut, "/auth/preferences", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preferences status=%d body=%s", w.Code, w.Body.String())
	}

	token := "interface-session"
	if err := s.store.CreateSession(token, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w = httptest.NewRecorder()
	s.handleMe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		InterfaceLevel *string `json:"interface_level"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.InterfaceLevel == nil || *response.InterfaceLevel != "personal" {
		t.Fatalf("interface_level=%v, want personal", response.InterfaceLevel)
	}
}

func TestAuthPreferencesRejectsInvalidInterfaceLevel(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("invalid-interface@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPut,
		"/auth/preferences",
		strings.NewReader(`{"interface_level":"admin"}`),
	)
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid interface level status=%d body=%s", w.Code, w.Body.String())
	}
	if got := s.store.GetUserInterfaceLevel(user.ID); got != "business" {
		t.Fatalf("invalid update changed interface level to %q", got)
	}
}

func TestLegacyUserInterfaceLevelIsUnset(t *testing.T) {
	s := newTestServer(t)
	result, err := s.store.db.Exec(
		"INSERT INTO users (email, password_hash) VALUES (?, ?)",
		"legacy-interface@test.com", "hash",
	)
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := result.LastInsertId()
	if got := s.store.GetUserInterfaceLevel(userID); got != "" {
		t.Fatalf("legacy interface level=%q, want unset", got)
	}
	token := "legacy-interface-session"
	if err := s.store.CreateSession(token, userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w := httptest.NewRecorder()
	s.handleMe(w, req)
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if value, exists := response["interface_level"]; !exists || value != nil {
		t.Fatalf("legacy interface_level=%v (exists=%v), want explicit null", value, exists)
	}
}

func TestAuthPreferencesUILayoutPersistsAndPreservesLanguage(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("layout@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetUserLanguage(user.ID, "fr"); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ui_layout":{"projects":{"project-a":{"slots":{"dashboard.home":["work:overview"]},"sidebar":["work"]}}}}`)
	req := httptest.NewRequest(http.MethodPut, "/auth/preferences", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preferences status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Language string         `json:"language"`
		UILayout map[string]any `json:"ui_layout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Language != "fr" {
		t.Fatalf("layout-only update reset language to %q", response.Language)
	}
	projects, ok := response.UILayout["projects"].(map[string]any)
	if !ok || projects["project-a"] == nil {
		t.Fatalf("ui_layout did not round-trip as an object: %#v", response.UILayout)
	}

	bad := httptest.NewRequest(http.MethodPut, "/auth/preferences", strings.NewReader(`{"ui_layout":[]}`))
	bad.Header.Set("X-User-ID", itoa(user.ID))
	badResponse := httptest.NewRecorder()
	s.handleAuthPreferences(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("array layout status=%d body=%s", badResponse.Code, badResponse.Body.String())
	}
}

// Simple cookie jar for testing
type testCookieJar struct {
	cookies []*http.Cookie
}

func (j *testCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.cookies = append(j.cookies, cookies...)
}

func (j *testCookieJar) Cookies(u *url.URL) []*http.Cookie {
	return j.cookies
}
