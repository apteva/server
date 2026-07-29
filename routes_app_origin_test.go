package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// app:// origins let a route front an installed app by name; the
// server resolves the app's LIVE sidecar per request so the route
// survives sidecar restarts that reassign the local port.

func TestParseRoute_AppOrigin(t *testing.T) {
	p, ok := parseRoute(Route{Hostname: "files.acme.com", Target: "app://storage?project_id=p1&ingress_auth=app_token"})
	if !ok {
		t.Fatal("parseRoute rejected a valid app:// target")
	}
	if p.originApp != "storage" {
		t.Errorf("originApp=%q, want storage", p.originApp)
	}
	if p.originProject != "p1" {
		t.Errorf("originProject=%q, want p1", p.originProject)
	}
	if !p.originAppTokenAuth {
		t.Error("app-token ingress auth opt-in was not parsed")
	}

	// Plain http target: no app-origin metadata.
	h, ok := parseRoute(Route{Hostname: "x.acme.com", Target: "http://127.0.0.1:8080"})
	if !ok {
		t.Fatal("parseRoute rejected a valid http target")
	}
	if h.originApp != "" || h.originProject != "" || h.originAppTokenAuth {
		t.Errorf("http target carried app-origin metadata: app=%q project=%q", h.originApp, h.originProject)
	}
}

func TestHostRouter_AppTokenIngressAuthPreservesVisitorAuthorization(t *testing.T) {
	var gotAuthorization, gotOriginal string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotOriginal = r.Header.Get("X-Apteva-Original-Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	registry := NewInstalledAppsRegistry()
	registry.Add(&InstalledApp{
		InstallID:  1,
		AppName:    "tunnel",
		SidecarURL: backend.URL,
		Token:      "sidecar-secret",
	})
	cache := NewRouteCache()
	cache.Replace([]Route{{
		Hostname:       "demo.tunnel.example.com",
		Target:         "app://tunnel?ingress_auth=app_token",
		AllowHTTP:      true,
		OwnerInstallID: 1,
	}})
	router := NewHostRouter(&Server{
		routeCache:    cache,
		edgeCache:     NewEdgeCache(),
		installedApps: registry,
	}, http.NotFoundHandler())

	request := httptest.NewRequest(http.MethodGet, "http://demo.tunnel.example.com/private", nil)
	request.Host = "demo.tunnel.example.com"
	request.Header.Set("Authorization", "Bearer visitor-token")
	request.Header.Set("X-Apteva-Original-Authorization", "spoofed")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if gotAuthorization != "Bearer sidecar-secret" {
		t.Fatalf("sidecar Authorization=%q", gotAuthorization)
	}
	if gotOriginal != "Bearer visitor-token" {
		t.Fatalf("preserved visitor Authorization=%q", gotOriginal)
	}
}

func TestHostRouter_AppTokenIngressAuthCannotTargetAnotherInstall(t *testing.T) {
	registry := NewInstalledAppsRegistry()
	registry.Add(&InstalledApp{
		InstallID:  2,
		AppName:    "sensitive",
		SidecarURL: "http://127.0.0.1:1",
		Token:      "must-not-be-used",
	})
	router := &HostRouter{server: &Server{installedApps: registry}}
	if token := router.resolveAppToken(RouteHit{
		OriginApp:          "sensitive",
		OriginAppTokenAuth: true,
		OwnerInstallID:     1,
	}); token != "" {
		t.Fatalf("cross-install route resolved target token %q", token)
	}
	if token := router.resolveAppToken(RouteHit{
		OriginApp:          "sensitive",
		OriginAppTokenAuth: true,
		OwnerInstallID:     0,
	}); token != "" {
		t.Fatalf("operator-owned route resolved app token %q", token)
	}
}

func TestHostRouter_ResolveTarget(t *testing.T) {
	reg := NewInstalledAppsRegistry()
	reg.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "p1", SidecarURL: "http://127.0.0.1:55001"})
	reg.Add(&InstalledApp{InstallID: 2, AppName: "storage", ProjectID: "p2", SidecarURL: "http://127.0.0.1:55002"})
	hr := &HostRouter{server: &Server{installedApps: reg}}

	// Project-scoped resolution picks the right install.
	if u, ok := hr.resolveTarget(RouteHit{OriginApp: "storage", OriginProject: "p1"}); !ok || u.String() != "http://127.0.0.1:55001" {
		t.Errorf("storage/p1 resolved to %v ok=%v, want http://127.0.0.1:55001", u, ok)
	}
	if u, ok := hr.resolveTarget(RouteHit{OriginApp: "storage", OriginProject: "p2"}); !ok || u.String() != "http://127.0.0.1:55002" {
		t.Errorf("storage/p2 resolved to %v ok=%v, want http://127.0.0.1:55002", u, ok)
	}

	// Unknown app → not resolvable (caller 502s instead of nil-proxying).
	if _, ok := hr.resolveTarget(RouteHit{OriginApp: "ghost", OriginProject: "p1"}); ok {
		t.Error("unknown app should not resolve")
	}

	// Plain http target passes through untouched.
	tu, _ := url.Parse("http://10.0.0.1:9000")
	if u, ok := hr.resolveTarget(RouteHit{Target: tu}); !ok || u != tu {
		t.Errorf("http target should pass through, got %v ok=%v", u, ok)
	}
}
