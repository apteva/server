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
	"strings"
	"sync/atomic"
	"time"
)

// HostRouter wraps the server's main mux with a Host-header dispatch.
// On a Host match, the request is reverse-proxied to the loopback
// port the deploy app reports for that release. On a miss the request
// falls through to the wrapped handler unchanged — existing path-based
// routing (/api, /apps/<name>/) keeps working.
//
// Routes are pulled from the deploy app's deploy_list_routes tool on
// a 5-second tick. The pull is best-effort: a transient deploy outage
// just means the table goes stale, not that the server stops serving.

type routeEntry struct {
	Slug   string
	Port   int
	Domain string
	Status string
}

type HostRouter struct {
	server *Server
	next   http.Handler

	current atomic.Pointer[map[string]routeEntry] // domain → entry

	stopCh chan struct{}
}

func NewHostRouter(s *Server, next http.Handler) *HostRouter {
	hr := &HostRouter{server: s, next: next, stopCh: make(chan struct{})}
	empty := map[string]routeEntry{}
	hr.current.Store(&empty)
	return hr
}

func (hr *HostRouter) Start(refresh time.Duration) {
	go hr.refreshLoop(refresh)
}

func (hr *HostRouter) Stop() { close(hr.stopCh) }

func (hr *HostRouter) refreshLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	hr.refreshOnce()
	for {
		select {
		case <-hr.stopCh:
			return
		case <-t.C:
			hr.refreshOnce()
		}
	}
}

func (hr *HostRouter) refreshOnce() {
	if hr.server == nil || hr.server.installedApps == nil {
		return
	}
	if hr.server.installedApps.GetByName("deploy") == nil {
		// Deploy isn't installed — keep the table empty.
		empty := map[string]routeEntry{}
		hr.current.Store(&empty)
		return
	}
	raw, err := callInstalledAppTool(hr.server, "deploy", "deploy_list_routes", map[string]any{})
	if err != nil {
		log.Printf("[host-router] refresh failed: %v", err)
		return
	}
	var payload struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("[host-router] decode envelope: %v", err)
		return
	}
	if payload.Error != nil {
		log.Printf("[host-router] deploy_list_routes: %s", payload.Error.Message)
		return
	}
	if len(payload.Result.Content) == 0 {
		return
	}
	var inner struct {
		Routes []struct {
			Slug   string `json:"slug"`
			Port   int    `json:"port"`
			Domain string `json:"domain"`
			Status string `json:"status"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(payload.Result.Content[0].Text), &inner); err != nil {
		log.Printf("[host-router] decode routes: %v", err)
		return
	}
	tab := make(map[string]routeEntry, len(inner.Routes))
	for _, r := range inner.Routes {
		if r.Domain == "" || r.Port == 0 || r.Status != "live" {
			continue
		}
		tab[strings.ToLower(r.Domain)] = routeEntry{
			Slug: r.Slug, Port: r.Port, Domain: r.Domain, Status: r.Status,
		}
	}
	hr.current.Store(&tab)
}

func (hr *HostRouter) lookup(host string) (routeEntry, bool) {
	host = strings.ToLower(host)
	// Strip port if any (e.g. "app.acme.com:8080").
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	tab := hr.current.Load()
	if tab == nil {
		return routeEntry{}, false
	}
	e, ok := (*tab)[host]
	return e, ok
}

func (hr *HostRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entry, ok := hr.lookup(r.Host)
	if !ok {
		hr.next.ServeHTTP(w, r)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", entry.Port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Keep the original Host so the upstream app sees the public name.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = r.Host
		req.Header.Set("X-Forwarded-Host", r.Host)
		if r.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		// Most likely cause: the supervised process died between the
		// last route refresh and this request. Map to 502 so the
		// browser can retry.
		http.Error(rw, "deployment unreachable: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
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

