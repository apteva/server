package main

import (
	"net/http"
	"net/url"
	"strconv"
)

const runtimeBrowserOriginsKey = "runtime-browser"

func (s *Server) runtimeBrowserOriginAllowed(id int64, origin string) bool {
	if origin == "" {
		return false
	}
	if origin == s.runtimeBrowserOrigin() {
		return true
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_cors_origins WHERE install_id=? AND registration_key=? AND origin=?`, id, runtimeBrowserOriginsKey, origin).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
func (s *Server) handleRuntimeBrowserOrigins(w http.ResponseWriter, r *http.Request, id int64) {
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectViewer); !ok {
		return
	}
	if r.Method == http.MethodGet {
		rows, e := s.listAppCORSOriginRegistrations(id, runtimeBrowserOriginsKey)
		if e != nil {
			http.Error(w, "origin policy unavailable", 503)
			return
		}
		writeJSON(w, map[string]any{"registrations": rows})
		return
	}
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "GET or PUT only", 405)
		return
	}
	var body struct {
		Origins []string `json:"origins"`
	}
	if runtimeDecode(r, &body) != nil {
		http.Error(w, "invalid origin policy", 400)
		return
	}
	origins, e := normalizeAppCORSOrigins(body.Origins)
	if e != nil || len(origins) > 32 {
		http.Error(w, "invalid origin policy", 400)
		return
	}
	for _, origin := range origins {
		u, e := url.Parse(origin)
		if e != nil || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
			http.Error(w, "origins require HTTPS or localhost", 400)
			return
		}
	}
	if e = s.replaceAppCORSOrigins(id, runtimeBrowserOriginsKey, origins, appCORSPreflightPlatform, false); e != nil {
		http.Error(w, "save origin policy", 503)
		return
	}
	writeJSON(w, map[string]any{"origins": origins})
}
func (s *Server) handleRuntimeBrowserOrigin(w http.ResponseWriter, r *http.Request) {
	id, e := strconv.ParseInt(r.Header.Get("X-Apteva-App-Install-ID"), 10, 64)
	if e != nil || id <= 0 {
		http.Error(w, "authenticated app required", 403)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var body struct {
		Origin string `json:"origin"`
	}
	if runtimeDecode(r, &body) != nil {
		http.Error(w, "invalid origin", 400)
		return
	}
	// Only the active execution credential may inspect its current policy.
	if s.runtimeProviderOriginActive(r, id) == false {
		http.Error(w, "active execution required", 403)
		return
	}
	writeJSON(w, map[string]bool{"allowed": s.runtimeBrowserOriginAllowed(id, body.Origin)})
}
func (s *Server) runtimeProviderOriginActive(r *http.Request, id int64) bool {
	d, e := s.appRuntimeManager().active(id)
	if e != nil {
		return false
	}
	var ref string
	token := privateRequestToken(r)
	e = s.store.db.QueryRow(`SELECT deployment_id FROM app_runtime_credentials WHERE token_hash=?`, HashAPIKey(token)).Scan(&ref)
	return e == nil && ref == d.ID && d.InstallID == id && d.Status == "active"
}
