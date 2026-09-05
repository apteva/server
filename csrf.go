package main

import (
	"net/http"
	"net/url"
	"strings"
)

// Cookie authentication requires browser-origin validation for mutations.
// Originless native clients retain compatibility; cross-site Fetch Metadata
// cannot bypass the check by omitting Origin.
func (s *Server) allowSessionMutation(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		if r.Header.Get("Sec-Fetch-Site") != "cross-site" {
			return true
		}
	} else if u, err := url.Parse(origin); err == nil && u.Host != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https") {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		if trustForwardedHeaders(r) {
			if v := r.Header.Get("X-Forwarded-Host"); v != "" {
				host = strings.TrimSpace(strings.Split(v, ",")[0])
			}
			if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
				scheme = strings.TrimSpace(strings.Split(v, ",")[0])
			}
		}
		if strings.EqualFold(u.Host, host) && u.Scheme == scheme {
			return true
		}
		if s.corsConfig != nil && s.corsConfig.origins[origin] {
			return true
		}
		if s.store != nil {
			policy := s.dynamicAppCORSPolicy(r, origin)
			if policy.Allowed && policy.Credentials {
				return true
			}
		}
	}
	http.Error(w, "untrusted request origin", http.StatusForbidden)
	return false
}
