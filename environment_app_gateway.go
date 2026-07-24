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
	"strconv"
	"strings"

	sdk "github.com/apteva/app-sdk"
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

// handleEnvironmentAppPublicGateway exposes a runtime app's regular HTTP and
// WebSocket routes to local protocol fixtures. The runtime id is an unguessable
// capability and the route is loopback-only; app credentials stay server-side.
//
// Shape: /api/environment-app-public/<runtime>/api/apps/<app>[/_install/<id>]/...
func (s *Server) handleEnvironmentAppPublicGateway(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/environment-app-public/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	environment, ok := s.environments.Get(parts[0])
	if !ok {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	appPath := strings.TrimPrefix(parts[1], "api/apps/")
	appParts := strings.SplitN(appPath, "/", 2)
	if len(appParts) == 0 || appParts[0] == "" {
		http.NotFound(w, r)
		return
	}
	appName, err := url.PathUnescape(appParts[0])
	if err != nil {
		http.Error(w, "invalid app name", http.StatusBadRequest)
		return
	}
	inst, ok := environment.Install(appName)
	if !ok {
		http.Error(w, "app not in runtime", http.StatusNotFound)
		return
	}
	tail := "/"
	if len(appParts) == 2 {
		tail = "/" + appParts[1]
	}
	if strings.HasPrefix(tail, "/_install/") {
		installParts := strings.SplitN(strings.TrimPrefix(tail, "/_install/"), "/", 2)
		if len(installParts) == 0 || installParts[0] != strconv.FormatInt(inst.InstallID, 10) {
			http.Error(w, "runtime install mismatch", http.StatusNotFound)
			return
		}
		tail = "/"
		if len(installParts) == 2 {
			tail += installParts[1]
		}
	}
	s.proxyEnvironmentInstall(w, r, inst, tail)
}

func (s *Server) proxyEnvironmentInstall(w http.ResponseWriter, r *http.Request, inst *localInstall, tail string) {
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
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = tail
		req.URL.RawPath = ""
		req.Header.Set("Authorization", "Bearer "+token)
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) runtimePlatformURL(runtimeID string) string {
	return fmt.Sprintf("https://runtime.apteva.invalid/%s", url.PathEscape(runtimeID))
}

func (s *Server) runtimeGatewayURL(runtimeID string) string {
	port := s.port
	if port == "" {
		port = "5280"
	}
	return fmt.Sprintf("http://127.0.0.1:%s/api/environment-app-public/%s", port, url.PathEscape(runtimeID))
}

func (s *Server) runtimeAppEndpoint(runtime *Environment, appName string) (*sdk.RuntimeAppEndpoint, error) {
	inst, ok := runtime.Install(appName)
	if !ok {
		return nil, fmt.Errorf("app %q is not install-backed", appName)
	}
	platformURL := s.runtimePlatformURL(runtime.ID)
	gatewayURL := s.runtimeGatewayURL(runtime.ID)
	return &sdk.RuntimeAppEndpoint{
		PlatformURL: platformURL,
		GatewayURL:  gatewayURL,
		AppURL: fmt.Sprintf(
			"%s/api/apps/%s/_install/%d",
			gatewayURL,
			url.PathEscape(appName),
			inst.InstallID,
		),
	}, nil
}

func (s *Server) runtimePlatformURLForInstall(installID int64) string {
	if s.environments == nil || installID <= 0 {
		return ""
	}
	for _, runtime := range s.environments.List() {
		for _, name := range runtime.InstallNames() {
			if inst, ok := runtime.Install(name); ok && inst.InstallID == installID {
				return s.runtimePlatformURL(runtime.ID)
			}
		}
	}
	return ""
}
