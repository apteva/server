package main

// Hostname-based routing for the gateway.
//
// Owns an in-memory map (hostname → target URL) hydrated from the
// `routes` app's REST surface on boot. When the routes app emits
// routes.changed events, the cache refreshes. The matcher middleware
// is slotted into the mux ahead of path-based fallthrough — a hit
// reverse-proxies to the registered target; a miss lets the existing
// /apps/* + dashboard routing run.
//
// Failure modes:
//   - routes app not installed: cache stays empty; everything falls
//     through to the existing routing (i.e. behaviour identical to
//     pre-routes installs).
//   - routes app slow to start: 3s timeout on hydration; subscribe
//     loop will catch up once it comes online.
//   - routes app crashes mid-flight: cache stays as last-known-good;
//     hits keep working; new register/unregister fail until it's back.

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Route mirrors the on-the-wire shape from apps/mcp/routes' Route
// struct. We re-declare it here rather than depending on the app's
// Go module so the server keeps zero compile-time coupling to the
// routes app — the routes app can be uninstalled, refactored, or
// versioned independently.
type Route struct {
	ID             int64  `json:"id"`
	Hostname       string `json:"hostname"`
	Target         string `json:"target"`
	OwnerInstallID int64  `json:"owner_install_id"`
	OwnerKind      string `json:"owner_kind"`
	CertFQDN       string `json:"cert_fqdn,omitempty"`
	AllowHTTP      bool   `json:"allow_http,omitempty"`
}

// RouteCache holds the parsed, ready-to-proxy view of the routes
// table. Reads (one per public request) take an RLock; writes (one
// per routes.changed event) take Lock. The cache is authoritative —
// the matcher never falls back to a live DB query.
type RouteCache struct {
	mu     sync.RWMutex
	byHost map[string]parsedRoute
}

type parsedRoute struct {
	hostname  string
	target    *url.URL
	certFQDN  string
	allowHTTP bool
	owner     int64
	kind      string
	// originApp is set (to the bare app name) when target is an
	// "app://<name>" reference. In that case `target` is NOT a usable
	// backend URL — HostRouter resolves the app's LIVE sidecar address
	// per request so the route survives sidecar restarts that reassign
	// the local port. Empty for ordinary http(s):// targets.
	originApp string
	// originProject disambiguates project-scoped apps (e.g. storage has
	// one install per project). From the app:// target's ?project_id=.
	// Empty resolves by app name alone (global apps).
	originProject string
}

func NewRouteCache() *RouteCache {
	return &RouteCache{byHost: map[string]parsedRoute{}}
}

// Lookup returns the parsed route for a hostname, or zero-value if
// no route matches. Hostname must already have its port stripped.
func (c *RouteCache) Lookup(hostname string) (parsedRoute, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.byHost[strings.ToLower(hostname)]
	return r, ok
}

// Replace swaps in a fresh route set — used at boot or after a
// reconnect to the event stream when we can't trust incremental
// updates anymore. Atomic from a reader's perspective.
func (c *RouteCache) Replace(routes []Route) {
	next := make(map[string]parsedRoute, len(routes))
	for _, r := range routes {
		p, ok := parseRoute(r)
		if !ok {
			continue
		}
		next[strings.ToLower(r.Hostname)] = p
	}
	c.mu.Lock()
	c.byHost = next
	c.mu.Unlock()
}

// Apply handles an incremental routes.changed event. action ∈
// {"created", "updated", "removed"}.
func (c *RouteCache) Apply(action, hostname string, target, certFQDN string, allowHTTP bool, owner int64, kind string) {
	host := strings.ToLower(strings.TrimSpace(hostname))
	if host == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if action == "removed" {
		delete(c.byHost, host)
		return
	}
	// Reuse parseRoute so app:// targets are recognised identically on
	// the incremental-event path and the full-replace path.
	p, ok := parseRoute(Route{
		Hostname:       host,
		Target:         target,
		CertFQDN:       certFQDN,
		AllowHTTP:      allowHTTP,
		OwnerInstallID: owner,
		OwnerKind:      kind,
	})
	if !ok {
		return
	}
	c.byHost[host] = p
}

// Size is for the platform status admin surface.
func (c *RouteCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byHost)
}

func parseRoute(r Route) (parsedRoute, bool) {
	u, err := url.Parse(r.Target)
	if err != nil || u.Host == "" {
		return parsedRoute{}, false
	}
	p := parsedRoute{
		hostname:  strings.ToLower(r.Hostname),
		target:    u,
		certFQDN:  r.CertFQDN,
		allowHTTP: r.AllowHTTP,
		owner:     r.OwnerInstallID,
		kind:      r.OwnerKind,
	}
	// app://<name>?project_id=<pid> — record the app name (+ project
	// for project-scoped apps); the live sidecar address is resolved
	// per request in HostRouter (u itself isn't proxyable).
	if u.Scheme == "app" {
		p.originApp = strings.ToLower(u.Host)
		p.originProject = u.Query().Get("project_id")
	}
	return p, true
}

// ─── Server hooks ─────────────────────────────────────────────────

// startRouteCache is called from main() after the apps registry is
// wired up. Hydrates the cache from the routes app (if installed)
// and starts the event-subscription loop. Best-effort — failures
// degrade to "no hostname routing" silently.
func (s *Server) startRouteCache() {
	if s.routeCache == nil {
		s.routeCache = NewRouteCache()
	}
	if s.edgeCache == nil {
		s.edgeCache = NewEdgeCache()
	}
	go s.hydrateRouteCache(3 * time.Second)
	go s.subscribeRouteEvents()
}

func (s *Server) hydrateRouteCache(timeout time.Duration) {
	entry := s.installedApps.GetByName("routes")
	if entry == nil {
		// Routes app isn't installed; cache stays empty. Subscribe
		// loop will hydrate later if it gets installed.
		return
	}
	url := entry.SidecarURL + "/api/routes"
	client := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest("GET", url, nil)
	if entry.Token != "" {
		req.Header.Set("Authorization", "Bearer "+entry.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[ROUTES-CACHE] hydrate: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[ROUTES-CACHE] hydrate status=%d body=%s", resp.StatusCode, string(body))
		return
	}
	var body struct {
		Routes []Route `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("[ROUTES-CACHE] hydrate decode: %v", err)
		return
	}
	s.routeCache.Replace(body.Routes)
	log.Printf("[ROUTES-CACHE] hydrated %d route(s)", len(body.Routes))
}

// subscribeRouteEvents opens an SSE connection to the platform's
// app-events stream filtered to topic=routes.changed and applies
// each event to the cache. Reconnects with backoff on disconnect.
// Re-hydrates fully on every (re)connect to recover from missed
// events.
func (s *Server) subscribeRouteEvents() {
	for {
		entry := s.installedApps.GetByName("routes")
		if entry == nil {
			// No routes app installed yet — wait and retry. New
			// installs trigger LoadInstalledApps, but we don't have a
			// signal here, so poll.
			time.Sleep(10 * time.Second)
			continue
		}
		s.consumeRouteEvents(entry)
		// Disconnect — re-hydrate then reconnect after a short delay.
		s.hydrateRouteCache(3 * time.Second)
		time.Sleep(2 * time.Second)
	}
}

func (s *Server) consumeRouteEvents(entry *InstalledApp) {
	// app-events stream lives on the platform's own /api/app-events/
	// surface, not the routes sidecar. The platform broadcasts every
	// emit() call from any app to subscribers. We filter to
	// topic=routes.changed in-process.
	url := s.localGatewayURL() + "/api/app-events/routes"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("[ROUTES-CACHE] sub req: %v", err)
		return
	}
	// The events bus accepts a sidecar's APTEVA_APP_TOKEN with
	// platform-wide read access; routes app's token works.
	if entry.Token != "" {
		req.Header.Set("Authorization", "Bearer "+entry.Token)
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[ROUTES-CACHE] sub dial: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[ROUTES-CACHE] sub status=%d body=%s", resp.StatusCode, string(body))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var env struct {
			Topic string         `json:"topic"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			continue
		}
		if env.Topic != "routes.changed" {
			continue
		}
		applyRouteEvent(s.routeCache, env.Data)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[ROUTES-CACHE] sub read: %v", err)
	}
}

func applyRouteEvent(cache *RouteCache, data map[string]any) {
	action, _ := data["action"].(string)
	hostname, _ := data["hostname"].(string)
	if action == "" || hostname == "" {
		return
	}
	target, _ := data["target"].(string)
	certFQDN, _ := data["cert_fqdn"].(string)
	allowHTTP, _ := data["allow_http"].(bool)
	var owner int64
	if v, ok := data["owner_install_id"].(float64); ok {
		owner = int64(v)
	}
	kind, _ := data["owner_kind"].(string)
	cache.Apply(action, hostname, target, certFQDN, allowHTTP, owner, kind)
}

// ─── HostRouter integration ───────────────────────────────────────
//
// HostRouter (host_routing.go) is the existing reverse-proxy
// dispatcher in front of the main mux. It used to source routes by
// polling deploy_list_routes every 5s. With the routes app shipping,
// HostRouter now consumes the RouteCache instead — the cache is the
// single source of truth, sourced from any app that registers via
// routes_register (deploy, code, manual panel entries, future apps).
//
// LookupForRouter is the bridge: HostRouter calls it on every
// request, gets back the parsed target + route flags, and
// reverse-proxies. Returning a copy (not a pointer) lets the caller
// release the cache lock immediately.

type RouteHit struct {
	Hostname  string
	Target    *url.URL
	CertFQDN  string
	AllowHTTP bool
	// OriginApp, when non-empty, means the route fronts an installed
	// app by name and Target is a placeholder. HostRouter resolves the
	// app's live sidecar URL per request before proxying. OriginProject
	// scopes the resolution for project-scoped apps.
	OriginApp     string
	OriginProject string
}

func (c *RouteCache) LookupForRouter(host string) (RouteHit, bool) {
	r, ok := c.Lookup(host)
	if !ok {
		return RouteHit{}, false
	}
	return RouteHit{
		Hostname:      r.hostname,
		Target:        r.target,
		CertFQDN:      r.certFQDN,
		AllowHTTP:     r.allowHTTP,
		OriginApp:     r.originApp,
		OriginProject: r.originProject,
	}, true
}

// silence "imported and not used" for httputil — the actual proxying
// lives in host_routing.go; this file only owns the data model.
var _ = httputil.NewSingleHostReverseProxy
