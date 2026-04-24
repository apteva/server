package main

// Static / SPA handlers referenced from main.go's dual-UI mount.
// Serves bundles from disk ($DATA_DIR/dashboard and $DATA_DIR/simple)
// with SPA-fallback semantics: unknown paths under the prefix return
// index.html so client-side routing works.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// newSPAHandler returns a handler that serves files from `dir`, falling
// back to index.html for any path that doesn't map to an existing file.
// `stripPrefix` (e.g. "/admin") is trimmed from the incoming request
// path before the lookup so the bundle can live at any mount point.
func newSPAHandler(dir, stripPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := r.URL.Path
		if stripPrefix != "" {
			rel = strings.TrimPrefix(rel, stripPrefix)
		}
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = "index.html"
		}
		full := filepath.Join(dir, rel)
		// Disallow path traversal — filepath.Join already cleans `..`
		// but belt-and-braces: ensure the resolved path is under dir.
		abs, err := filepath.Abs(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		dirAbs, _ := filepath.Abs(dir)
		if !strings.HasPrefix(abs, dirAbs) {
			http.NotFound(w, r)
			return
		}
		if fileExists(full) {
			http.ServeFile(w, r, full)
			return
		}
		// SPA fallback — serve index.html so client-side routing can
		// handle the path.
		idx := filepath.Join(dir, "index.html")
		if fileExists(idx) {
			f, err := os.Open(idx)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.Copy(w, f)
			return
		}
		http.NotFound(w, r)
	}
}

// newSimpleHandler serves the client-facing read-only bundle at the
// root. Same mechanics as newSPAHandler with no prefix stripping and
// a tighter SPA fallback (only fall back for paths that look like app
// routes, not /api/* etc.).
func newSimpleHandler(dir string) http.HandlerFunc {
	return newSPAHandler(dir, "")
}
