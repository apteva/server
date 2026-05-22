package main

import (
	"net/url"
	"testing"
)

// app:// origins let a route front an installed app by name; the
// server resolves the app's LIVE sidecar per request so the route
// survives sidecar restarts that reassign the local port.

func TestParseRoute_AppOrigin(t *testing.T) {
	p, ok := parseRoute(Route{Hostname: "files.acme.com", Target: "app://storage?project_id=p1"})
	if !ok {
		t.Fatal("parseRoute rejected a valid app:// target")
	}
	if p.originApp != "storage" {
		t.Errorf("originApp=%q, want storage", p.originApp)
	}
	if p.originProject != "p1" {
		t.Errorf("originProject=%q, want p1", p.originProject)
	}

	// Plain http target: no app-origin metadata.
	h, ok := parseRoute(Route{Hostname: "x.acme.com", Target: "http://127.0.0.1:8080"})
	if !ok {
		t.Fatal("parseRoute rejected a valid http target")
	}
	if h.originApp != "" || h.originProject != "" {
		t.Errorf("http target carried app-origin metadata: app=%q project=%q", h.originApp, h.originProject)
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
