package main

import (
	"net/http"
	"path/filepath"
	"strings"
)

// handleIntegrationStatic serves UI bundles for a given integration:
//   GET /api/integrations/<slug>/ui/<file>
//
// Resolves to <uiDir>/<slug>/<file>. The uiDir is set at boot from
// either APTEVA_INTEGRATIONS_UI_DIR (preferred), the dev path
// alongside appsDir (../../integrations/dist/ui from the data dir),
// or the prod path (<dataDir>/integrations-ui) — see main.go's
// integrationsUIDir helper.
//
// We restrict to the /ui/<file> sub-path so other catalog routes
// registered at /integrations/* (catalog, groups, …) keep working.
// File names are sanitised — no traversal, ESM only.
func (s *Server) handleIntegrationStatic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/integrations/")
	parts := strings.SplitN(rest, "/", 3)
	// Expected shape: <slug>/ui/<file>. Anything else falls through
	// to a 404 — this handler must NOT catch /catalog, /groups,
	// /connections, etc. (those have more-specific registrations
	// and the http.ServeMux longest-prefix rule already protects
	// them, but the explicit guard makes it impossible to mis-route
	// after a future refactor that loses a registration).
	if len(parts) != 3 || parts[1] != "ui" {
		http.NotFound(w, r)
		return
	}
	slug := parts[0]
	file := parts[2]
	if slug == "" || file == "" || strings.Contains(file, "..") {
		http.NotFound(w, r)
		return
	}
	if s.integrationsUIDir == "" {
		http.Error(w, "integrations UI bundles not configured", http.StatusNotFound)
		return
	}
	full := filepath.Join(s.integrationsUIDir, slug, file)
	// filepath.Join already canonicalises and would happily resolve
	// "../" sequences; guard explicitly that the resolved path
	// stays under the configured root.
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(s.integrationsUIDir)+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(file, ".mjs") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(file, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(file, ".map") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	// Long-cache + cache-bust via the ?v=<integration version> the
	// dashboard adds to the import URL — same pattern as apps.
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFile(w, r, full)
}
