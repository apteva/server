package main

// environment_app_gateway.go — the bridge that lets an in-environment agent core reach
// the environment's token-protected app sidecars.
//
// In-environment apps are real installs spawned with a per-install app token, so
// their /mcp requires that bearer. The agent core isn't app-sdk and can't add
// a per-server auth header (and we don't change core). So the agent's
// mcp_servers point here — a loopback reverse-proxy that looks up the
// in-environment install and injects its dev token before forwarding to the real
// sidecar. The server brokers the token; the agent never sees it.
//
// Path: /api/environment-app-gateway/<environmentID>/<app>/<tail...>
// (e.g. .../mcp). Loopback only; the (unguessable) environment id in the path is the credential,
// It is not behind authMiddleware so the
// spawned core can reach it.

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func (s *Server) handleEnvironmentAppGateway(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/environment-app-gateway/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "environment-app-gateway: need /<environmentID>/<app>/<path>", http.StatusBadRequest)
		return
	}
	environmentID, app := parts[0], parts[1]
	tail := "/"
	if len(parts) == 3 {
		tail = "/" + parts[2]
	}
	environment, ok := s.environments.Get(environmentID)
	if !ok {
		http.Error(w, "environment not found: "+environmentID, http.StatusNotFound)
		return
	}
	inst, ok := environment.Install(app)
	if !ok {
		http.Error(w, "app not in environment: "+app, http.StatusNotFound)
		return
	}
	target, err := url.Parse(inst.SidecarURL)
	if err != nil {
		http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
		return
	}
	token, err := s.appInstallToken(inst.InstallID)
	if err != nil {
		http.Error(w, "app credential unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.URL.Path = tail
		req.Header.Set("Authorization", "Bearer "+token)
	}
	proxy.ServeHTTP(w, r)
}

// environmentAppMCPURL is the mcp_servers URL an in-environment agent uses to reach a
// real (install-backed) app through the token-brokering gateway.
func (s *Server) environmentAppMCPURL(environmentID, app string) string {
	return fmt.Sprintf("http://127.0.0.1:%s/api/environment-app-gateway/%s/%s/mcp", s.port, environmentID, app)
}
