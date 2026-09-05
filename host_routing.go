package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// HostRouter wraps the server's main mux with a Host-header dispatch.
// On a Host match, the request is reverse-proxied to the registered
// target. On a miss the request falls through to the wrapped handler
// unchanged — existing path-based routing (/api, /apps/<name>/) keeps
// working.
//
// Routes come from the RouteCache (routes_cache.go), which hydrates
// from the `routes` app at boot and refreshes via SSE on
// routes.changed events. Any app that wants a public hostname calls
// routes_register on the routes app — deploy via attach_domain, code
// via repos_dev_start with expose=true, manual entries from the
// Routes panel.
//
// Pre-routes-app installs used a polling refresh against
// deploy_list_routes; that data path was removed when the routes
// app shipped (the deploy integration migrated to routes_register).
// HostRouter now keeps its existing public surface (NewHostRouter,
// Start, Stop) for compatibility with main.go's wiring, but the
// refresh loop is gone — the cache pushes updates instead of HostRouter
// pulling them.

type HostRouter struct {
	server *Server
	next   http.Handler
	stopCh chan struct{}
}

func NewHostRouter(s *Server, next http.Handler) *HostRouter {
	return &HostRouter{server: s, next: next, stopCh: make(chan struct{})}
}

// Start kicks off the route cache's hydration + event subscription.
// Refresh interval is unused now (the cache is event-driven) but kept
// in the signature to avoid touching every caller in main.go.
func (hr *HostRouter) Start(_ time.Duration) {
	if hr.server != nil {
		hr.server.startRouteCache()
	}
}

func (hr *HostRouter) Stop() { close(hr.stopCh) }

func (hr *HostRouter) lookup(host string) (RouteHit, bool) {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if hr.server == nil || hr.server.routeCache == nil {
		return RouteHit{}, false
	}
	return hr.server.routeCache.LookupForRouter(host)
}

func (hr *HostRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if hr.server != nil && hr.server.ingressCerts != nil && hr.server.ingressCerts.ServeHTTPChallenge(w, r) {
		return
	}
	host := r.Host
	if hr.server != nil && hr.server.primaryHost != "" {
		stripped := host
		if i := strings.IndexByte(stripped, ':'); i >= 0 {
			stripped = stripped[:i]
		}
		if strings.EqualFold(stripped, hr.server.primaryHost) {
			if envTruthy(os.Getenv("APTEVA_INGRESS_ENABLED")) && r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
				httpsHost := stripHostPort(r.Host)
				http.Redirect(w, r, "https://"+httpsHost+r.URL.RequestURI(), http.StatusMovedPermanently)
				return
			}
			hr.next.ServeHTTP(w, r)
			return
		}
	}
	hit, ok := hr.lookup(host)
	if !ok {
		hr.next.ServeHTTP(w, r)
		return
	}
	if !hit.AllowHTTP && r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
		return
	}
	// app:// ingress bypasses the /api mux, so enforce the owning
	// installation's live browser-origin policy here. The shared config is a
	// fallback only; a matching app registration remains authoritative.
	if hit.OriginApp != "" && hit.OwnerInstallID > 0 && hr.server != nil {
		next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			hr.serveRoute(rw, req, hit)
		})
		hr.server.corsConfig.middlewareWithDynamicPolicy(next, func(_ *http.Request, origin string) dynamicCORSPolicy {
			return hr.server.dynamicIngressCORSPolicy(hit.OwnerInstallID, origin)
		}).ServeHTTP(w, r)
		return
	}
	hr.serveRoute(w, r, hit)
}

func (hr *HostRouter) serveRoute(w http.ResponseWriter, r *http.Request, hit RouteHit) {
	isUpgrade := requestIsProtocolUpgrade(r)
	cacheHost := fmt.Sprintf("%s@%d", hit.Hostname, hit.Generation)
	// Edge cache: serve fresh public assets without touching the origin.
	// Protocol upgrades must retain the original ResponseWriter's Hijacker;
	// neither cache lookup nor response wrapping applies to a connection that
	// becomes a bidirectional stream.
	if !isUpgrade && hr.server != nil && hr.server.edgeCache != nil && hr.server.edgeCache.serve(w, r, cacheHost) {
		return
	}
	if !isUpgrade && hr.server != nil && hr.server.edgeCache != nil {
		handled, release := hr.server.edgeCache.coalesce(w, r, cacheHost)
		defer release()
		if handled {
			return
		}
	}
	// Resolve the effective backend. For app:// origins this looks up
	// the app's LIVE sidecar URL per request, so a sidecar restart
	// (which reassigns the local port) can't leave the route pointing
	// at a dead backend.
	target, ok := hr.resolveTarget(hit)
	if !ok {
		log.Printf("[host-router] %s: origin app %q not resolvable (not installed/running?)", hit.Hostname, hit.OriginApp)
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	appToken := ""
	if hit.OriginAppTokenAuth {
		appToken = hr.resolveAppToken(hit)
		if appToken == "" {
			log.Printf("[host-router] %s: app-token ingress auth requested but app token is unavailable", hit.Hostname)
			http.Error(w, "backend authentication unavailable", http.StatusBadGateway)
			return
		}
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = coreProxyClient.Transport
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		clearPrincipalHeaders(req)
		for name := range req.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-apteva-caller-") {
				req.Header.Del(name)
			}
		}
		req.Host = r.Host
		req.Header.Set("X-Forwarded-Host", r.Host)
		if !isUpgrade {
			hr.server.applyGeoCountryHeader(req.Header, r)
		} else {
			req.Header.Del(geoCountryHeader)
		}
		req.Header.Del("X-Apteva-Original-Authorization")
		req.Header.Del("X-Apteva-App-Token")
		if appToken != "" {
			if visitorAuthorization := req.Header.Get("Authorization"); visitorAuthorization != "" {
				req.Header.Set("X-Apteva-Original-Authorization", visitorAuthorization)
			}
			req.Header.Set("Authorization", "Bearer "+appToken)
		}
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("[host-router] proxy %s → %s: %v", hit.Hostname, target, err)
		http.Error(rw, "backend unreachable: "+err.Error(), http.StatusBadGateway)
	}
	// On a cache miss, tee the origin response so eligible public assets
	// get stored for next time.
	if !isUpgrade && hr.server != nil && hr.server.edgeCache != nil {
		cw := hr.server.edgeCache.wrap(w, r, cacheHost)
		defer cw.release()
		proxy.ServeHTTP(cw, r)
		cw.finalize()
		return
	}
	proxy.ServeHTTP(w, r)
}

// resolveTarget returns the backend URL to proxy to. For ordinary
// http(s):// routes that's the cached Target. For app:// origins it
// resolves the named app's LIVE sidecar URL from the installed-apps
// registry (refreshed on every sidecar (re)spawn via
// LoadInstalledApps), preferring the project-scoped install when the
// route carries a project. Returns ok=false when the app isn't
// installed/running so the caller can 502 instead of nil-proxying.
func (hr *HostRouter) resolveTarget(hit RouteHit) (*url.URL, bool) {
	if hit.OriginApp == "" {
		return hit.Target, hit.Target != nil
	}
	if hr.server == nil || hr.server.installedApps == nil {
		return nil, false
	}
	var entry *InstalledApp
	if hit.OriginProject != "" {
		entry = hr.server.installedApps.GetByNameAndProject(hit.OriginApp, hit.OriginProject)
	}
	if hit.OriginProject == "" {
		entry = hr.server.installedApps.GetByNameAndProject(hit.OriginApp, "")
	}
	if entry == nil || entry.SidecarURL == "" {
		return nil, false
	}
	u, err := url.Parse(entry.SidecarURL)
	if err != nil || u.Host == "" {
		return nil, false
	}
	if hit.Target != nil {
		u.Path, u.RawPath = joinRouteURLPath(u, hit.Target)
	}
	return u, true
}

// joinRouteURLPath mirrors ReverseProxy's path joining semantics. app:// URI
// query parameters are server routing controls and intentionally are not
// copied; only the declared path prefix is mounted ahead of the visitor path.
func joinRouteURLPath(base, appTarget *url.URL) (path, rawPath string) {
	if base.RawPath == "" && appTarget.RawPath == "" {
		return singleJoiningSlash(base.Path, appTarget.Path), ""
	}
	basePath := base.EscapedPath()
	targetPath := appTarget.EscapedPath()
	baseSlash := strings.HasSuffix(basePath, "/")
	targetSlash := strings.HasPrefix(targetPath, "/")
	switch {
	case baseSlash && targetSlash:
		return base.Path + appTarget.Path[1:], basePath + targetPath[1:]
	case !baseSlash && !targetSlash:
		return base.Path + "/" + appTarget.Path, basePath + "/" + targetPath
	}
	return base.Path + appTarget.Path, basePath + targetPath
}

func singleJoiningSlash(left, right string) string {
	leftSlash := strings.HasSuffix(left, "/")
	rightSlash := strings.HasPrefix(right, "/")
	switch {
	case leftSlash && rightSlash:
		return left + right[1:]
	case !leftSlash && !rightSlash:
		return left + "/" + right
	}
	return left + right
}

func (hr *HostRouter) resolveAppToken(hit RouteHit) string {
	if hit.OriginApp == "" || hit.OwnerInstallID <= 0 || hr.server == nil || hr.server.installedApps == nil {
		return ""
	}
	var entry *InstalledApp
	if hit.OriginProject != "" {
		entry = hr.server.installedApps.GetByNameAndProject(hit.OriginApp, hit.OriginProject)
	}
	if hit.OriginProject == "" {
		entry = hr.server.installedApps.GetByNameAndProject(hit.OriginApp, "")
	}
	if entry == nil || entry.InstallID != hit.OwnerInstallID {
		return ""
	}
	return strings.TrimSpace(entry.Token)
}

// ─── cross-app tool call helper ───────────────────────────────────
//
// Same shape as apps_callbacks.go's "call target app" flow, but
// callable from server-internal code (route refresh, cert pull) with
// no incoming request to plumb.

func callInstalledAppTool(s *Server, appName, tool string, args map[string]any) ([]byte, error) {
	target := s.installedApps.GetByName(appName)
	if target == nil {
		return nil, fmt.Errorf("app not running: %s", appName)
	}
	if target.SidecarURL == "" {
		return nil, fmt.Errorf("app %q has no sidecar URL", appName)
	}
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	body, _ := json.Marshal(rpc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", target.SidecarURL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s.%s: HTTP %d", appName, tool, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
