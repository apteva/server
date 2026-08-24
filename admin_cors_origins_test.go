package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callAdminCORSAPI(s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleAdminCORSOrigins)(rec, req)
	return rec
}

func TestAdminCORSOriginsAreLiveAndServerWide(t *testing.T) {
	s := newTestServer(t)
	adminKey := testPrivateAPIKey(t, s)

	put := callAdminCORSAPI(s, http.MethodPut, "/admin/cors-origins/saas-dashboard", adminKey,
		`{"origins":["https://Dashboard.Example/","http://localhost:5173"]}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	var saved appCORSOriginRegistration
	if err := json.NewDecoder(put.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Key != "saas-dashboard" || len(saved.Origins) != 2 || saved.Origins[0] != "https://dashboard.example" {
		t.Fatalf("saved=%+v", saved)
	}

	// A platform registration is intentionally broader than an app callback
	// registration: it permits the exact origin on ordinary authenticated API
	// routes as well as app routes. The real request still passes normal auth.
	for _, path := range []string{"/auth/login", "/projects", "/apps/auth/login"} {
		preflight := serveDynamicPreflight(s, path, "https://dashboard.example")
		if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != "https://dashboard.example" {
			t.Fatalf("path=%s allow-origin=%q headers=%v", path, got, preflight.Header())
		}
		if got := preflight.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("path=%s credentials=%q", path, got)
		}
	}
	if got := serveDynamicPreflight(s, "/projects", "https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unregistered origin allowed: %q", got)
	}

	list := callAdminCORSAPI(s, http.MethodGet, "/admin/cors-origins", adminKey, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"saas-dashboard"`) || !strings.Contains(list.Body.String(), `"legacy_environment"`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}

	replace := callAdminCORSAPI(s, http.MethodPut, "/admin/cors-origins/saas-dashboard", adminKey,
		`{"origins":["https://new-dashboard.example"]}`)
	if replace.Code != http.StatusOK {
		t.Fatalf("replace status=%d body=%s", replace.Code, replace.Body.String())
	}
	if got := serveDynamicPreflight(s, "/projects", "https://dashboard.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("replaced origin still allowed: %q", got)
	}
	if got := serveDynamicPreflight(s, "/projects", "https://new-dashboard.example").Header().Get("Access-Control-Allow-Origin"); got != "https://new-dashboard.example" {
		t.Fatalf("replacement not immediately active: %q", got)
	}

	deleted := callAdminCORSAPI(s, http.MethodDelete, "/admin/cors-origins/saas-dashboard", adminKey, "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if got := serveDynamicPreflight(s, "/projects", "https://new-dashboard.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("deleted origin still allowed: %q", got)
	}
}

func TestAdminCORSOriginsRequirePlatformAdmin(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	userID := mkUser(t, s, "cors-user@test.local")
	userKey := "sk_non_admin_cors"
	if _, err := s.store.CreateAPIKey(userID, "test", HashAPIKey(userKey), "sk_non_adm"); err != nil {
		t.Fatal(err)
	}

	forged := httptest.NewRequest(http.MethodPut, "/admin/cors-origins/client", strings.NewReader(`{"origins":["https://example.com"]}`))
	forged.Header.Set("X-User-ID", "1")
	forgedRec := httptest.NewRecorder()
	s.authMiddleware(s.handleAdminCORSOrigins)(forgedRec, forged)
	if forgedRec.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity status=%d body=%s", forgedRec.Code, forgedRec.Body.String())
	}

	denied := callAdminCORSAPI(s, http.MethodPut, "/admin/cors-origins/client", userKey,
		`{"origins":["https://example.com"]}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", denied.Code, denied.Body.String())
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM platform_cors_origins`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied request mutated registry: count=%d err=%v", count, err)
	}
}

func TestDynamicCORSOriginEnablesCrossOriginSessionCookieWithoutRestart(t *testing.T) {
	s := newTestServer(t)
	adminKey := testPrivateAPIKey(t, s)
	if rec := callAdminCORSAPI(s, http.MethodPut, "/admin/cors-origins/dashboard", adminKey,
		`{"origins":["https://dashboard.example"]}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	previous := crossOriginCookies
	crossOriginCookies = false
	t.Cleanup(func() { crossOriginCookies = previous })
	handler := (*corsConfig)(nil).middlewareWithDynamicOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSessionCookie(w, r, "session-token")
		w.WriteHeader(http.StatusNoContent)
	}), s.dynamicAppCORSOriginAllowed)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.Header.Set("Origin", "https://dashboard.example")
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].SameSite != http.SameSiteNoneMode || !cookies[0].Secure {
		t.Fatalf("cookie=%+v headers=%v", cookies, rec.Header())
	}
}

func TestAdminCORSOriginsExposeLegacyEnvironmentAsReadOnly(t *testing.T) {
	t.Setenv("CORS_ORIGIN", "https://legacy-b.example, https://legacy-a.example")
	state := currentLegacyCORSEnvironmentState()
	if !state.Configured || state.Mode != "allowlist" || !state.ReadOnly || !state.RestartRequired {
		t.Fatalf("state=%+v", state)
	}
	want := []string{"https://legacy-a.example", "https://legacy-b.example"}
	if len(state.Origins) != len(want) || state.Origins[0] != want[0] || state.Origins[1] != want[1] {
		t.Fatalf("origins=%v want=%v", state.Origins, want)
	}
}
