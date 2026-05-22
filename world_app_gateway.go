package main

// world_app_gateway.go — the bridge that lets an in-world agent core reach
// the world's token-protected app sidecars.
//
// In-world apps are real installs spawned with APTEVA_APP_TOKEN=dev-<id>, so
// their /mcp requires that bearer. The agent core isn't app-sdk and can't add
// a per-server auth header (and we don't change core). So the agent's
// mcp_servers point here — a loopback reverse-proxy that looks up the
// in-world install and injects its dev token before forwarding to the real
// sidecar. The server brokers the token; the agent never sees it.
//
// Path: /api/world-app-gateway/<worldID>/<app>/<tail...>  (e.g. .../mcp)
// Loopback only; the (unguessable) world id in the path is the credential,
// same model as the eval-mock-gateway. Not behind authMiddleware so the
// spawned core can reach it.

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func (s *Server) handleWorldAppGateway(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/world-app-gateway/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "world-app-gateway: need /<worldID>/<app>/<path>", http.StatusBadRequest)
		return
	}
	worldID, app := parts[0], parts[1]
	tail := "/"
	if len(parts) == 3 {
		tail = "/" + parts[2]
	}
	world, ok := s.worlds.Get(worldID)
	if !ok {
		http.Error(w, "world not found: "+worldID, http.StatusNotFound)
		return
	}
	inst, ok := world.Install(app)
	if !ok {
		http.Error(w, "app not in world: "+app, http.StatusNotFound)
		return
	}
	target, err := url.Parse(inst.SidecarURL)
	if err != nil {
		http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
		return
	}
	token := fmt.Sprintf("dev-%d", inst.InstallID)
	proxy := httputil.NewSingleHostReverseProxy(target)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.URL.Path = tail
		req.Header.Set("Authorization", "Bearer "+token)
	}
	proxy.ServeHTTP(w, r)
}

// worldAppMCPURL is the mcp_servers URL an in-world agent uses to reach a
// real (install-backed) app through the token-brokering gateway.
func (s *Server) worldAppMCPURL(worldID, app string) string {
	return fmt.Sprintf("http://127.0.0.1:%s/api/world-app-gateway/%s/%s/mcp", s.port, worldID, app)
}
