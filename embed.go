package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dashboard/*
var dashboardFS embed.FS

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
