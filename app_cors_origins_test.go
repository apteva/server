package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedCORSAppInstall(t *testing.T, s *Server, name, projectID string) int64 {
	t.Helper()
	ensureTestAdmin(t, s)
	res, err := s.store.db.Exec(
		`INSERT INTO apps(name,source,manifest_json) VALUES(?, 'test', '{}')`, name,
	)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs(app_id,project_id,status,installed_by) VALUES(?,?,'running',1)`,
		appID, projectID,
	)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := res.LastInsertId()
	return installID
}

func callCORSRegistration(t *testing.T, s *Server, installID int64, method, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/apps/callback/cors-origins/"+key, strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa64(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func serveDynamicPreflight(s *Server, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	(*corsConfig)(nil).middlewareWithDynamicPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), s.dynamicAppCORSPolicy).ServeHTTP(rec, req)
	return rec
}

func TestAppCanRegisterAndReplaceLiveCORSOrigins(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "auth-cors-test", "")

	put := callCORSRegistration(t, s, installID, http.MethodPut, "oauth-client-1",
		`{"origins":["https://SaaS.Example/","http://localhost:3000","https://saas.example"]}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	var saved appCORSOriginPolicyRegistration
	if err := json.NewDecoder(put.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Key != "oauth-client-1" || len(saved.Origins) != 2 || saved.Origins[0] != "https://saas.example" {
		t.Fatalf("saved registration=%+v", saved)
	}
	if saved.Preflight != appCORSPreflightPlatform || !saved.Credentials {
		t.Fatalf("origin-only request did not preserve legacy policy defaults: %+v", saved)
	}

	allowed := serveDynamicPreflight(s, "/apps/auth-cors-test/login", "https://saas.example")
	if allowed.Code != http.StatusNoContent || allowed.Header().Get("Access-Control-Allow-Origin") != "https://saas.example" {
		t.Fatalf("allowed preflight status=%d headers=%v", allowed.Code, allowed.Header())
	}
	if allowed.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("dynamic exact origin must allow credentials: %v", allowed.Header())
	}
	wrongApp := serveDynamicPreflight(s, "/apps/some-other-app/login", "https://saas.example")
	if got := wrongApp.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("origin leaked to another app: %q", got)
	}
	platformRoute := serveDynamicPreflight(s, "/auth/login", "https://saas.example")
	if got := platformRoute.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("origin leaked to platform route: %q", got)
	}

	replace := callCORSRegistration(t, s, installID, http.MethodPut, "oauth-client-1",
		`{"origins":["https://new.example"]}`)
	if replace.Code != http.StatusOK {
		t.Fatalf("replace status=%d body=%s", replace.Code, replace.Body.String())
	}
	if got := serveDynamicPreflight(s, "/apps/auth-cors-test/login", "https://saas.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("replaced origin still allowed: %q", got)
	}
	if got := serveDynamicPreflight(s, "/apps/auth-cors-test/login", "https://new.example").Header().Get("Access-Control-Allow-Origin"); got != "https://new.example" {
		t.Fatalf("new origin not active immediately: %q", got)
	}

	deleted := callCORSRegistration(t, s, installID, http.MethodDelete, "oauth-client-1", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if got := serveDynamicPreflight(s, "/apps/auth-cors-test/login", "https://new.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("deleted origin still allowed: %q", got)
	}
}

func TestAppManagedCORSDelegatesRegisteredPreflight(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "api-cors-test", "")
	put := callCORSRegistration(t, s, installID, http.MethodPut, "api:42",
		`{"origins":["https://console.example"],"preflight":"app","credentials":false}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	var saved appCORSOriginPolicyRegistration
	if err := json.NewDecoder(put.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if saved.Preflight != appCORSPreflightApp || saved.Credentials {
		t.Fatalf("saved registration=%+v", saved)
	}

	called := false
	handler := (*corsConfig)(nil).middlewareWithDynamicPolicy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Headers", "content-type, x-customer-header")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusAccepted)
	}), s.dynamicAppCORSPolicy)

	req := httptest.NewRequest(http.MethodOptions, "/apps/api-cors-test/gw/orders", nil)
	req.Header.Set("Origin", "https://console.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-customer-header")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusAccepted {
		t.Fatalf("registered app preflight was not delegated: called=%v status=%d body=%s", called, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-customer-header" {
		t.Fatalf("sidecar CORS headers were not preserved: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credential-free registration did not constrain sidecar response: %q", got)
	}

	called = false
	req = httptest.NewRequest(http.MethodOptions, "/apps/api-cors-test/gw/orders", nil)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unregistered origin reached sidecar: called=%v status=%d headers=%v", called, rec.Code, rec.Header())
	}
}

func TestAppCORSPolicyPrecedesGlobalGrants(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "api-precedence-test", "")
	const origin = "https://console.example"
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "api:42",
		`{"origins":["https://console.example"],"preflight":"app","credentials":false}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := s.replacePlatformCORSOrigins(ensureTestAdmin(t, s), "global-console", []string{origin}); err != nil {
		t.Fatal(err)
	}

	called := false
	// The origin is allowed both by the static operator config and the live
	// platform-admin registry. The install policy must still delegate and
	// enforce credentials=false.
	handler := newCORSConfig(origin).middlewareWithDynamicPolicy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "x-gateway-token")
		w.WriteHeader(http.StatusAccepted)
	}), s.dynamicAppCORSPolicy)
	req := httptest.NewRequest(http.MethodOptions, "/apps/api-precedence-test/gw/orders", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != http.StatusAccepted {
		t.Fatalf("restrictive app policy lost to global grant: called=%v status=%d", called, rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "x-gateway-token" {
		t.Fatalf("delegated response headers lost: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("global grant broadened credentials: %q", got)
	}
}

func TestAppCORSPolicyUsesRestrictiveIntersectionAcrossKeys(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "api-intersection-test", "")
	const origin = "https://shared.example"
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "client-a",
		`{"origins":["https://shared.example"],"preflight":"platform","credentials":true}`); rec.Code != http.StatusOK {
		t.Fatalf("register client-a status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "client-b",
		`{"origins":["https://shared.example"],"preflight":"app","credentials":false}`); rec.Code != http.StatusOK {
		t.Fatalf("register client-b status=%d body=%s", rec.Code, rec.Body.String())
	}

	policy := s.registeredAppCORSInstallPolicy(installID, origin)
	if !policy.Allowed || !policy.DelegatePreflight || policy.Credentials {
		t.Fatalf("shared-origin policy was broadened: %+v", policy)
	}
}

func TestHostRouterAppOriginAppliesOwnerCORSAndPreservesPrefix(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "api-host-cors-test", "")
	const origin = "https://console.example"
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "api:custom-host",
		`{"origins":["https://console.example"],"preflight":"app","credentials":false}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	called := false
	var gotPath string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotPath = r.URL.Path
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Headers", "content-type, x-gateway-token")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer sidecar.Close()

	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: installID, AppName: "api-host-cors-test", SidecarURL: sidecar.URL,
	})
	s.routeCache = NewRouteCache()
	s.routeCache.Replace([]Route{{
		Hostname:       "gateway.example.com",
		Target:         "app://api-host-cors-test/gw?ingress_auth=none",
		AllowHTTP:      true,
		OwnerInstallID: installID,
	}})
	// Also grant the same origin globally to prove that the install's
	// delegated, credential-free policy remains authoritative.
	s.corsConfig = newCORSConfig(origin)
	router := NewHostRouter(s, http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodOptions, "http://gateway.example.com/orders", nil)
	req.Host = "gateway.example.com"
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-gateway-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if !called || rec.Code != http.StatusAccepted {
		t.Fatalf("custom-host preflight not delegated: called=%v status=%d body=%s", called, rec.Code, rec.Body.String())
	}
	if gotPath != "/gw/orders" {
		t.Fatalf("sidecar path=%q want /gw/orders", gotPath)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-gateway-token" {
		t.Fatalf("sidecar CORS headers were not preserved: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credential-free custom-host policy was broadened: %q", got)
	}

	called = false
	req = httptest.NewRequest(http.MethodOptions, "http://gateway.example.com/orders", nil)
	req.Host = "gateway.example.com"
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unregistered custom-host origin reached sidecar: called=%v status=%d headers=%v", called, rec.Code, rec.Header())
	}
}

func TestAppManagedCORSPreflightReachesMethodSpecificNoAuthSidecar(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	installID := seedCORSAppInstall(t, s, "method-cors-test", "")
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "route:submit",
		`{"origins":["https://app.example"],"preflight":"app","credentials":false}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	var seenMethod, seenRequestedMethod string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenRequestedMethod = r.Header.Get("Access-Control-Request-Method")
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Headers", "content-type, x-widget-token")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID: installID, AppName: "method-cors-test", SidecarURL: sidecar.URL, Token: "install-token",
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
			{Method: http.MethodPost, Prefix: "/submit", NoAuth: true},
		}}},
	})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	handler := (*corsConfig)(nil).middlewareWithDynamicPolicy(apiMux, s.dynamicAppCORSPolicy)
	req := httptest.NewRequest(http.MethodOptions, "/apps/method-cors-test/submit", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-widget-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenMethod != http.MethodOptions || seenRequestedMethod != http.MethodPost {
		t.Fatalf("sidecar saw method=%q requested=%q", seenMethod, seenRequestedMethod)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-widget-token" {
		t.Fatalf("sidecar allow-headers lost through proxy: %q", got)
	}
}

func TestPlatformManagedAppCORSCanDisableCredentials(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "public-api-cors-test", "")
	put := callCORSRegistration(t, s, installID, http.MethodPut, "api:public",
		`{"origins":["https://docs.example"],"credentials":false}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	preflight := serveDynamicPreflight(s, "/apps/public-api-cors-test/gw/docs", "https://docs.example")
	if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Origin") != "https://docs.example" {
		t.Fatalf("preflight status=%d headers=%v", preflight.Code, preflight.Header())
	}
	if got := preflight.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credential-free policy emitted Allow-Credentials: %q", got)
	}
}

func TestAppCORSRegistrationRejectsUnknownPreflightMode(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "bad-policy-cors-test", "")
	rec := callCORSRegistration(t, s, installID, http.MethodPut, "client",
		`{"origins":["https://example.com"],"preflight":"magic"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "platform") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAppCORSRegistryMigratesOriginOnlySchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cors-upgrade.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE app_cors_origins`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		CREATE TABLE app_cors_origins (
			install_id INTEGER NOT NULL,
			registration_key TEXT NOT NULL,
			origin TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (install_id, registration_key, origin)
		)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if !columnExists(upgraded.db, "app_cors_origins", "preflight_mode") ||
		!columnExists(upgraded.db, "app_cors_origins", "credentials") {
		t.Fatal("origin-only CORS registry was not upgraded with policy columns")
	}
}

func TestAppCORSRegistrationRejectsWildcardsAndNonOrigins(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "strict-cors-test", "")
	for _, body := range []string{
		`{"origins":["*"]}`,
		`{"origins":["https://example.com/path"]}`,
		`{"origins":["javascript:alert(1)"]}`,
	} {
		rec := callCORSRegistration(t, s, installID, http.MethodPut, "client", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestRegisteredCORSOriginRespectsProjectAndInstallSelection(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "project-cors-test", "project-a")
	if rec := callCORSRegistration(t, s, installID, http.MethodPut, "client", `{"origins":["https://a.example"]}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := serveDynamicPreflight(s, "/apps/project-cors-test/login", "https://a.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("project install selected without project context: %q", got)
	}
	if got := serveDynamicPreflight(s, "/apps/project-cors-test/login?project_id=project-b", "https://a.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("origin allowed for wrong project: %q", got)
	}
	if got := serveDynamicPreflight(s, "/apps/project-cors-test/login?project_id=project-a", "https://a.example").Header().Get("Access-Control-Allow-Origin"); got != "https://a.example" {
		t.Fatalf("origin denied for matching project: %q", got)
	}
	path := "/apps/project-cors-test/_install/" + itoa64(installID) + "/login"
	if got := serveDynamicPreflight(s, path, "https://a.example").Header().Get("Access-Control-Allow-Origin"); got != "https://a.example" {
		t.Fatalf("origin denied for explicit install: %q", got)
	}
}

func TestPublicClientOriginsAuthorizeMCPPreflight(t *testing.T) {
	s := newTestServer(t)
	_, _, _ = seedPublicClientKey(t, s, "catalog", []string{"catalog_prices_list"}, []string{"https://shop.example"}, 60)

	allowed := serveDynamicPreflight(s, "/apps/catalog/mcp", "https://shop.example")
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://shop.example" {
		t.Fatalf("public-client origin denied: %q", got)
	}
	wrongRoute := serveDynamicPreflight(s, "/apps/catalog/admin", "https://shop.example")
	if got := wrongRoute.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("public-client origin broadened beyond MCP: %q", got)
	}
}

func TestCORSOriginCallbackRequiresAuthenticatedInstallToken(t *testing.T) {
	s := newTestServer(t)
	installID := seedCORSAppInstall(t, s, "callback-auth-cors-test", "")
	handler := s.authMiddleware(s.handleAppCallback)

	req := httptest.NewRequest(http.MethodPut, "/apps/callback/cors-origins/client", strings.NewReader(`{"origins":["https://example.com"]}`))
	// A browser/client cannot forge the trusted identity header; authMiddleware
	// removes it before checking credentials.
	req.Header.Set("X-Apteva-App-Install-ID", itoa64(installID))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPut, "/apps/callback/cors-origins/client", strings.NewReader(`{"origins":["https://example.com"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", rec.Code, rec.Body.String())
	}
}
