package main

import (
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"time"
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
	setIntegrationContentType(w, file)
	// Long-cache + cache-bust via the ?v=<integration version> the
	// dashboard adds to the import URL — same pattern as apps.
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	// On-disk first (dev workflow + ops-pushed updates), embed second
	// (the npx-apteva install path with no source tree). Either layer
	// can be missing in any given install — embed is empty when the
	// build skipped the integrations-ui sync, and integrationsUIDir
	// is empty when neither the env var nor the dev / prod fallback
	// resolved to a real directory.
	if s.integrationsUIDir != "" {
		full := filepath.Join(s.integrationsUIDir, slug, file)
		// Guard: filepath.Join collapses "..", so make sure the cleaned
		// path stays under the configured root.
		if strings.HasPrefix(filepath.Clean(full), filepath.Clean(s.integrationsUIDir)+string(filepath.Separator)) {
			if _, err := http.Dir(s.integrationsUIDir).Open(slug + "/" + file); err == nil {
				http.ServeFile(w, r, full)
				return
			}
		}
	}
	if serveIntegrationFromEmbed(w, r, slug, file) {
		return
	}
	http.NotFound(w, r)
}

// setIntegrationContentType maps file extension → MIME header so
// the browser executes .mjs as JavaScript, etc. Shared between the
// disk and embed paths.
func setIntegrationContentType(w http.ResponseWriter, file string) {
	switch {
	case strings.HasSuffix(file, ".mjs"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(file, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(file, ".map"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
}

// serveIntegrationFromEmbed pulls the bundle out of the binary's
// integrations-ui embed and writes it to the response. Returns true
// when it served (or attempted to and failed mid-stream); false when
// the file simply doesn't exist in the embed so the caller can fall
// through to a 404.
func serveIntegrationFromEmbed(w http.ResponseWriter, r *http.Request, slug, file string) bool {
	f, err := integrationsUIEmbeddedFS.Open(slug + "/" + file)
	if err != nil {
		return false
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Embedded fs.File implementations are seekable in modern Go,
		// but guard anyway — falls back to copying the body without
		// Range support.
		_, _ = io.Copy(w, f)
		return true
	}
	// Use ServeContent so Range / If-Modified-Since headers behave.
	// modTime=zero is fine — the embed has no real mtime; clients
	// cache via the ?v= cache-bust the dashboard adds anyway.
	modTime := time.Time{}
	http.ServeContent(w, r, file, modTime, rs)
	return true
}

// silence unused import warning when the FS is empty — fs.WalkDir
// is not used here but keeping the import slot reserved for future
// "list available integrations" debug endpoints. (No-op in current
// build.)
var _ = fs.WalkDir
