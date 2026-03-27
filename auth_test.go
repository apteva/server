package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &Server{
		store:       store,
		instances:   NewInstanceManager(t.TempDir(), "echo", 4000),
		broadcaster: NewTelemetryBroadcaster(),
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
