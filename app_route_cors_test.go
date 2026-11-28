package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type routeCORSFixture struct {
	s       *Server
	handler http.Handler
	install int64
	// What the sidecar saw on its last request.
	calls   int
	method  string
	cookie  string
	userID  string
	session string
}

// newRouteCORSFixture installs an app with one route that owns its CORS
// (/p/) and one ordinary public route (/open/). The sidecar answers CORS the
// way a careless app might, including a credentials header it must not get
// through.
func newRouteCORSFixture(t *testing.T, name, projectID string) *routeCORSFixture {
	t.Helper()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	f := &routeCORSFixture{s: s}
	if projectID != "" {
		ensureTestAdmin(t, s)
		if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name,description) VALUES(?,1,?,'')`, projectID, projectID); err != nil {
			t.Fatal(err)
		}
	}
	f.install = seedCORSAppInstall(t, s, name, projectID)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.method = r.Method
		f.cookie = r.Header.Get("Cookie")
		f.userID = r.Header.Get("X-User-ID")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sidecar.Close)
	s.installedApps.Add(&InstalledApp{
		InstallID: f.install, AppName: name, ProjectID: projectID, SidecarURL: sidecar.URL, Token: "install-token",
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
			{Prefix: "/p/", NoAuth: true, CORS: sdk.RouteCORSApp},
			{Prefix: "/open/", NoAuth: true},
		}}},
	})
	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	f.handler = (*corsConfig)(nil).middlewareWithDynamicPolicy(apiMux, s.dynamicAppCORSPolicy)

	f.session = generateToken(32)
	if err := s.store.CreateSession(f.session, ensureTestAdmin(t, s), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *routeCORSFixture) do(method, path, origin string, withSession bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "content-type")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	if withSession {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: f.session})
		req.AddCookie(&http.Cookie{Name: "app_pref", Value: "dark"})
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// A preflight from any origin, the "null" of a sandboxed page included,
// reaches the app, and the app cannot make the route credentialed.
func TestRouteCORSAppDelegatesPreflightForAnyOrigin(t *testing.T) {
	f := newRouteCORSFixture(t, "previews", "")
	for _, origin := range []string{"null", "https://anywhere.example"} {
		before := f.calls
		rec := f.do(http.MethodOptions, "/apps/previews/p/ws1/token/api", origin, false)
		if rec.Code != http.StatusNoContent || f.calls != before+1 || f.method != http.MethodOptions {
			t.Fatalf("origin %s: status=%d sidecar calls=%d method=%q, want the preflight at the sidecar", origin, rec.Code, f.calls-before, f.method)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("origin %s: allow-origin=%q, want the sidecar's", origin, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type" {
			t.Fatalf("origin %s: allow-headers=%q, want the sidecar's", origin, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Fatalf("origin %s: credentials header %q got through", origin, got)
		}
	}
}

// A public route that does not declare cors: app keeps the platform's answer.
func TestRouteCORSUndeclaredRouteKeepsPlatformPreflight(t *testing.T) {
	f := newRouteCORSFixture(t, "previews", "")
	rec := f.do(http.MethodOptions, "/apps/previews/open/thing", "null", false)
	if rec.Code != http.StatusNoContent || f.calls != 0 {
		t.Fatalf("status=%d sidecar calls=%d, want the platform's bare 204", rec.Code, f.calls)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin=%q on an undeclared route", got)
	}
}

// The viewer's platform session never authenticates a request to such a
// route: no CSRF refusal, no identity and no session cookie at the sidecar.
func TestRouteCORSAppIgnoresPlatformSession(t *testing.T) {
	f := newRouteCORSFixture(t, "previews", "")
	rec := f.do(http.MethodPost, "/apps/previews/p/ws1/token/api", "null", true)
	if rec.Code != http.StatusNoContent || f.calls != 1 {
		t.Fatalf("status=%d body=%q sidecar calls=%d, want the request at the sidecar", rec.Code, rec.Body.String(), f.calls)
	}
	if f.userID != "" {
		t.Fatalf("sidecar saw X-User-ID %q", f.userID)
	}
	if strings.Contains(f.cookie, cookieName+"=") || !strings.Contains(f.cookie, "app_pref=dark") {
		t.Fatalf("sidecar cookie header = %q, want the session cookie gone and others kept", f.cookie)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credentials header %q got through", got)
	}

	// The same request to an ordinary public route, and to the platform's
	// own API, is still refused.
	if rec := f.do(http.MethodPost, "/apps/previews/open/thing", "null", true); rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared public route: status=%d, want 403", rec.Code)
	}
	reached := false
	api := f.s.authMiddleware(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest(http.MethodPost, "/agents", strings.NewReader(`{}`))
	req.Header.Set("Origin", "null")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: f.session})
	rec = httptest.NewRecorder()
	api(rec, req)
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("platform API: status=%d reached=%v, want 403", rec.Code, reached)
	}
}

// A registered origin with credentials cannot make the route credentialed.
func TestRouteCORSAppStaysUncredentialedForRegisteredOrigin(t *testing.T) {
	f := newRouteCORSFixture(t, "previews", "")
	const origin = "https://app.example"
	if rec := callCORSRegistration(t, f.s, f.install, http.MethodPut, "site", `{"origins":["`+origin+`"],"credentials":true}`); rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	if policy := f.s.dynamicAppCORSPolicy(httptest.NewRequest(http.MethodPost, "/apps/previews/open/thing", nil), origin); !policy.Credentials {
		t.Fatal("precondition: the registered origin is credentialed on an ordinary route")
	}
	rec := f.do(http.MethodPost, "/apps/previews/p/ws1/token/api", origin, false)
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" || f.calls != 1 {
		t.Fatalf("credentials header %q, sidecar calls=%d", got, f.calls)
	}
}

// Both ways of naming the install resolve the route.
func TestRouteCORSAppResolvesInstallSelectorAndProject(t *testing.T) {
	f := newRouteCORSFixture(t, "previews", "proj-x")
	for _, path := range []string{
		"/apps/previews/_install/" + itoa64(f.install) + "/p/ws1/token/api",
		"/apps/previews/p/ws1/token/api?project_id=proj-x",
	} {
		before := f.calls
		rec := f.do(http.MethodOptions, path, "null", false)
		if rec.Code != http.StatusNoContent || f.calls != before+1 {
			t.Fatalf("%s: status=%d body=%q sidecar calls=%d", path, rec.Code, rec.Body.String(), f.calls-before)
		}
	}
	// Another install's id, or another project, does not match.
	for _, path := range []string{
		"/apps/previews/_install/999999/p/ws1/token/api",
		"/apps/previews/p/ws1/token/api?project_id=other",
	} {
		before := f.calls
		if rec := f.do(http.MethodOptions, path, "null", false); f.calls != before || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s: reached the sidecar or got CORS headers", path)
		}
	}
}
