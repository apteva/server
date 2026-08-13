package main

import (
	"net/http"
	"strings"
)

// corsConfig is built once at startup from the CORS_ORIGIN env var.
//
//	unset / ""    → disabled. The bundled dashboard is same-origin.
//	"off" / "none"→ disabled: no Access-Control-* headers at all.
//	"*"           → wildcard: Allow-Origin: *, NO credentials (browsers
//	                reject "*" with Allow-Credentials). Use only for
//	                API-key clients.
//	"a.com,b.com" → strict allowlist: only those origins get headers.
type corsConfig struct {
	mode    string // "permissive" | "wildcard" | "allowlist"
	origins map[string]bool
}

func newCORSConfig(env string) *corsConfig {
	env = strings.TrimSpace(env)
	lower := strings.ToLower(env)
	switch {
	case lower == "off" || lower == "none" || lower == "disabled":
		return nil
	case env == "":
		return nil
	case env == "*":
		return &corsConfig{mode: "wildcard"}
	default:
		c := &corsConfig{mode: "allowlist", origins: map[string]bool{}}
		for _, o := range strings.Split(env, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				c.origins[o] = true
			}
		}
		return c
	}
}

// middleware adds Access-Control-* headers and short-circuits OPTIONS
// preflights. Must wrap the /api mux only — webhook / oauth / mcp muxes
// have their own semantics.
//
// Credentialed requests (cookie or X-API-Key) cannot be paired with
// `*`, so permissive mode echoes the exact request Origin back.
func (c *corsConfig) middleware(next http.Handler) http.Handler {
	return c.middlewareWithDynamicOrigin(next, nil)
}

// middlewareWithDynamicOrigin permits a narrowly registered API-key origin in
// addition to the operator's static CORS configuration. Browser preflights do
// not carry Authorization, so the callback checks the server's active
// delegated-key records by origin; the subsequent request still passes full
// authentication and authorization.
func (c *corsConfig) middlewareWithDynamicOrigin(next http.Handler, dynamic func(*http.Request, string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := ""
		credentials := false
		if c != nil {
			switch c.mode {
			case "permissive":
				if origin != "" {
					allow = origin
					credentials = true
				}
			case "wildcard":
				allow = "*"
			case "allowlist":
				if origin != "" && c.origins[origin] {
					allow = origin
					credentials = true
				}
			}
		}
		if allow == "" && origin != "" && dynamic != nil && dynamic(r, origin) {
			allow = origin
		}

		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			if credentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-Invite-Token, X-Setup-Token")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// needsCrossOriginCookies reports whether the session cookie should be
// emitted as SameSite=None; Secure so browsers send it on cross-origin
// credentialed requests. Any mode that sends credentials across
// origins requires it; wildcard and "off" modes don't.
func (c *corsConfig) needsCrossOriginCookies() bool {
	if c == nil {
		return false
	}
	return c.mode == "permissive" || c.mode == "allowlist"
}
