package main

import (
	"net/http"
	"strings"
)

// registerAppRuntimeRoutes mounts the app data-plane routes that sidecars use
// at runtime. Production and Environment/eval test servers both need this
// surface; keeping it here prevents tests from drifting into a smaller route
// set than real apteva-server.
func (s *Server) registerAppRuntimeRoutes(apiMux *http.ServeMux) {
	// App event bus — generic SDK-level pub/sub for app→dashboard live UI.
	// Sidecars POST emits via APTEVA_APP_TOKEN; browsers SSE-subscribe via
	// cookie/API-key auth. Mounted under /api/app-events/ to sidestep the
	// catch-all /api/apps/<name>/... proxy further down.
	apiMux.HandleFunc("/app-events/internal/emit", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleAppEventEmit(w, r)
			return
		}
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/app-events/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleAppEventStream(w, r)
			return
		}
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
	}))

	// Platform callbacks from sidecars: grants, app-to-app calls,
	// connection execution, project listing, whoami, credentials.
	apiMux.HandleFunc("/apps/callback/", s.authMiddleware(s.handleAppCallback))

	// Reverse-proxy: any non-management /apps/<name>/... goes to the
	// installed app's sidecar. Specific /apps management routes should be
	// registered separately; ServeMux picks their longer patterns.
	apiMux.HandleFunc("/apps/", func(w http.ResponseWriter, r *http.Request) {
		if s.tryHandlePublicClientAppMCP(w, r) {
			return
		}
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/apps/")
			first := path
			if i := strings.Index(path, "/"); i >= 0 {
				first = path[:i]
			}
			switch first {
			case "preview", "install", "installs", "marketplace", "callback":
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleAppProxy(w, r)
		})(w, r)
	})
}
