package main

// Static-app serving: when an install's manifest declares
// `runtime.kind: static`, the app contributes a directory of files
// (typically a built SPA) instead of a sidecar process. apteva-server
// mounts that directory directly on its HTTP mux at a configurable
// prefix, so the bundle is reachable at http://<server>/<mount>/...
// without a separate container, port, or orchestrator entry.
//
// The mux we use is rebuilt on every (un)install — Go's stdlib
// ServeMux can't deregister patterns, so we keep a "static frame" mux
// that wraps the main one and re-derives its routes from the current
// installed-apps registry. main.go installs a single handler on the
// outer mux that delegates to this frame; calling RemountStaticApps()
// after any install/uninstall change is enough.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sdk "github.com/apteva/app-sdk"
)

// staticAppMounts is the live set of registered (mountPath, handler)
// pairs derived from the InstalledAppsRegistry. Replaced atomically on
// every RemountStaticApps() call; reads on the request path are
// lock-free under the RWMutex.
type staticAppMounts struct {
	mu     sync.RWMutex
	routes map[string]http.Handler // mountPath (with trailing slash) → handler
}

func newStaticAppMounts() *staticAppMounts {
	return &staticAppMounts{routes: map[string]http.Handler{}}
}

// match returns the handler whose mount path is the longest prefix of
// the request path. Empty mountPath ("") never matches; mountPath "/"
// (i.e. an app installed at the server root) is intentionally rejected
// because it'd shadow the dashboard and the entire /api/* surface —
// the install API rejects that case before persisting.
func (m *staticAppMounts) match(reqPath string) (string, http.Handler) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var bestPrefix string
	var bestHandler http.Handler
	for prefix, h := range m.routes {
		if !strings.HasPrefix(reqPath, prefix) {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			bestHandler = h
		}
	}
	return bestPrefix, bestHandler
}

func (m *staticAppMounts) replace(routes map[string]http.Handler) {
	m.mu.Lock()
	m.routes = routes
	m.mu.Unlock()
}

// staticAppHandler is the single http.Handler the main mux delegates
// to for every request that didn't match a known prefix. It runs the
// longest-prefix lookup against the current static-app routes, and
// falls through to next when nothing matches — letting the normal
// dashboard SPA take over.
func (s *Server) staticAppHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, h := s.staticMounts.match(r.URL.Path)
		if h == nil {
			next.ServeHTTP(w, r)
			return
		}
		// StripPrefix so the inner handler sees a clean root-relative
		// path (e.g. "/" or "/foo/bar.js" instead of "/client/" or
		// "/client/foo/bar.js"). Trailing-slash handling: a bare
		// `/client` redirects to `/client/` so SPA routes resolve
		// against the bundle root.
		if r.URL.Path == strings.TrimRight(prefix, "/") {
			http.Redirect(w, r, prefix, http.StatusMovedPermanently)
			return
		}
		http.StripPrefix(strings.TrimRight(prefix, "/"), h).ServeHTTP(w, r)
	})
}

// RemountStaticApps rebuilds the static-mounts table from the live
// registry. Idempotent: same set of installs in → same routes out.
// Call after every install / uninstall and once at boot after
// LoadInstalledApps.
func (s *Server) RemountStaticApps() {
	if s.staticMounts == nil {
		s.staticMounts = newStaticAppMounts()
	}
	routes := map[string]http.Handler{}
	for _, e := range s.installedApps.List() {
		if e.StaticDir == "" {
			continue
		}
		mount := e.MountPath
		if mount == "" || mount == "/" {
			log.Printf("[APPS-STATIC] skip %s — empty or root mount path", e.AppName)
			continue
		}
		if !strings.HasPrefix(mount, "/") {
			mount = "/" + mount
		}
		// Trailing slash so longest-prefix match is unambiguous
		// across e.g. "/client" and "/client-secondary".
		if !strings.HasSuffix(mount, "/") {
			mount = mount + "/"
		}
		routes[mount] = newStaticAppFileHandler(e)
	}
	s.staticMounts.replace(routes)
	log.Printf("[APPS-STATIC] remounted %d static app(s)", len(routes))
}

// newStaticAppFileHandler returns the per-install handler that serves
// files from the install's StaticDir. SPA fallback: any path that
// doesn't resolve to a file on disk gets index.html (so client-side
// router URLs work on hard refresh). The handler also injects a
// `<script>window.__APTEVA_APP__={…}</script>` block before
// `</head>` on every served HTML response — that's how the bundle
// receives its mount path, kiosk key, default project, and branding
// without a build-time substitution.
func newStaticAppFileHandler(e *InstalledApp) http.Handler {
	root := e.StaticDir
	indexPath := filepath.Join(root, "index.html")
	mountPath := e.MountPath
	if mountPath == "" {
		mountPath = "/"
	}
	cfg := e.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	branding := map[string]string{}
	if e.Manifest.Provides.UIApp != nil {
		branding["title_template"] = e.Manifest.Provides.UIApp.Branding.TitleTemplate
		branding["logo"] = e.Manifest.Provides.UIApp.Branding.Logo
		branding["theme_css"] = e.Manifest.Provides.UIApp.Branding.ThemeCSS
	}
	injectionPayload := map[string]any{
		"base":             mountPath,
		"api_base":         "/api",
		"app_name":         e.AppName,
		"install_id":       e.InstallID,
		"default_project":  cfg["default_project"],
		"kiosk_api_key":    cfg["kiosk_api_key"],
		"branding":         branding,
	}
	injectionJSON, _ := json.Marshal(injectionPayload)
	// We also export the legacy globals __API_BASE__ and
	// __DEFAULT_PROJECT__ so bundles that haven't migrated to the
	// __APTEVA_APP__ object still pick up runtime config. JSON-encode
	// the strings to keep escaping honest.
	apiBase, _ := json.Marshal("/api")
	defProj, _ := json.Marshal(cfg["default_project"])
	injection := []byte(fmt.Sprintf(
		"<script>window.__APTEVA_APP__=%s;window.__API_BASE__=%s;window.__DEFAULT_PROJECT__=%s;</script>",
		string(injectionJSON), string(apiBase), string(defProj),
	))

	fileServer := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the requested file. Empty / trailing-slash paths
		// serve index.html with branding injection. Existing files
		// stream straight through fileServer; missing paths fall back
		// to index.html (SPA fallback). Directory traversal is
		// blocked by http.FileServer's standard Clean+filepath.Join
		// safeties.
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			serveInjectedHTML(w, indexPath, injection)
			return
		}
		full := filepath.Join(root, clean)
		fi, err := os.Stat(full)
		if err == nil && !fi.IsDir() {
			if strings.HasSuffix(strings.ToLower(clean), ".html") {
				serveInjectedHTML(w, full, injection)
				return
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback — serve the index with injection.
		serveInjectedHTML(w, indexPath, injection)
	})
}

// serveInjectedHTML reads htmlPath, injects the install's branding
// payload before `</head>` (or as a prefix if no head tag), and writes
// the result. Cache-Control: no-store so the injected payload always
// reflects the current install config (cheap — index.html is tiny and
// the assets it references are content-hashed and cacheable).
func serveInjectedHTML(w http.ResponseWriter, htmlPath string, injection []byte) {
	f, err := os.Open(htmlPath)
	if err != nil {
		http.Error(w, "index.html not found in app bundle", http.StatusNotFound)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read index.html: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := injectInto(body, injection)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// injectInto inserts injection just before the closing </head> tag in
// body. If no </head> is present it falls back to placing the script
// at the very start so React still picks it up before main.js runs.
func injectInto(body, injection []byte) []byte {
	closeHead := []byte("</head>")
	if i := indexOfFold(body, closeHead); i >= 0 {
		out := make([]byte, 0, len(body)+len(injection))
		out = append(out, body[:i]...)
		out = append(out, injection...)
		out = append(out, body[i:]...)
		return out
	}
	out := make([]byte, 0, len(body)+len(injection))
	out = append(out, injection...)
	out = append(out, body...)
	return out
}

// indexOfFold is bytes.Index with ASCII-case folding so the injection
// finds </head> regardless of whether the source uses </HEAD>, etc.
func indexOfFold(haystack, needle []byte) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			h := haystack[i+j]
			n := needle[j]
			if 'A' <= h && h <= 'Z' {
				h = h + ('a' - 'A')
			}
			if 'A' <= n && n <= 'Z' {
				n = n + ('a' - 'A')
			}
			if h != n {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// resolveMountPath picks the URL prefix for a static-app install. The
// install's per-tenant config (`config.mount_path`) wins over the
// manifest's default (`provides.ui_app.mount_path`) wins over a
// derived "/<app-name>". Always normalises to "/foo" form (no
// trailing slash; that's added by the mount loop).
func resolveMountPath(m *sdk.Manifest, cfg map[string]string) string {
	if v := strings.TrimSpace(cfg["mount_path"]); v != "" {
		return normaliseMount(v)
	}
	if m.Provides.UIApp != nil {
		if v := strings.TrimSpace(m.Provides.UIApp.MountPath); v != "" {
			return normaliseMount(v)
		}
	}
	return "/" + m.Name
}

func normaliseMount(s string) string {
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		s = "/"
	}
	return s
}
