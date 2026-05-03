package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dashboard/*
var dashboardFS embed.FS

// Integration UI bundles baked into the binary so `npx apteva` users
// get GitHub IssueCard / Slack MessageCard / etc. without a separate
// download step. build-local.sh syncs integrations/dist/ui/ →
// server/integrations-ui/ before each go build, so this captures
// whatever component bundles were latest at compile time.
// Empty by default — the embed directive doesn't fail when the dir
// doesn't exist; serveIntegrationFromEmbed handles "no files" gracefully.
//
//go:embed all:integrations-ui
var integrationsUIFS embed.FS

// integrationsUIEmbeddedFS exposes the embed root rooted at
// `integrations-ui/<slug>/<file>` so callers can fs.Sub through it
// to get a per-slug filesystem if they want. Today only
// handleIntegrationStatic uses it as a fallback after the on-disk
// path misses.
var integrationsUIEmbeddedFS fs.FS = func() fs.FS {
	sub, err := fs.Sub(integrationsUIFS, "integrations-ui")
	if err != nil {
		return integrationsUIFS
	}
	return sub
}()

// dashboardHandler serves the embedded dashboard with SPA fallback.
func dashboardHandler() http.Handler {
	sub, _ := fs.Sub(dashboardFS, "dashboard")
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the exact file
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}

		// Check if file exists in embedded FS
		f, err := sub.Open(strings.TrimPrefix(path, "/"))
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback — serve index.html for all unmatched routes
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
