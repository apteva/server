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

// dynamicCORSPolicy is the live decision returned by server-managed origin
// registries. Platform preflight keeps the historical behavior: the outer
// middleware writes the standard Apteva CORS headers and terminates OPTIONS.
// App preflight only authorizes the origin at this layer; OPTIONS and the
// eventual response are delegated to the owning sidecar so route-specific
// methods and headers are preserved.
type dynamicCORSPolicy struct {
	Allowed           bool
	Credentials       bool
	DelegatePreflight bool
}

// corsCredentialGuardWriter treats Credentials=false as a hard ceiling for an
// app-managed response. The sidecar still owns every other CORS header, but it
// cannot accidentally turn a credential-free registration into a
// credentialed browser policy.
type corsCredentialGuardWriter struct {
	http.ResponseWriter
}

func (w *corsCredentialGuardWriter) sanitize() {
	w.Header().Del("Access-Control-Allow-Credentials")
}

func (w *corsCredentialGuardWriter) WriteHeader(status int) {
	w.sanitize()
	w.ResponseWriter.WriteHeader(status)
}

func (w *corsCredentialGuardWriter) Write(body []byte) (int, error) {
	w.sanitize()
	return w.ResponseWriter.Write(body)
}

func (w *corsCredentialGuardWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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
	var policy func(*http.Request, string) dynamicCORSPolicy
	if dynamic != nil {
		policy = func(r *http.Request, origin string) dynamicCORSPolicy {
			allowed := dynamic(r, origin)
			return dynamicCORSPolicy{Allowed: allowed, Credentials: allowed}
		}
	}
	return c.middlewareWithDynamicPolicy(next, policy)
}

// middlewareWithDynamicPolicy is the policy-aware form used by the server.
// The bool callback above remains as a compatibility wrapper for focused tests
// and callers that only need the original exact-origin behavior.
func (c *corsConfig) middlewareWithDynamicPolicy(next http.Handler, dynamic func(*http.Request, string) dynamicCORSPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := ""
		credentials := false
		delegatePreflight := false
		// A matching install-scoped policy is authoritative for its app
		// surface. Evaluate it before static/global CORS so an app can require
		// delegated preflight or forbid credentials even when the operator also
		// permits the same origin globally.
		dynamicMatched := false
		if origin != "" && dynamic != nil {
			decision := dynamic(r, origin)
			if decision.Allowed {
				dynamicMatched = true
				credentials = decision.Credentials
				delegatePreflight = decision.DelegatePreflight
				if !delegatePreflight {
					allow = origin
				}
			}
		}
		if !dynamicMatched && c != nil {
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

		if r.Method == http.MethodOptions && !delegatePreflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if delegatePreflight && !credentials {
			next.ServeHTTP(&corsCredentialGuardWriter{ResponseWriter: w}, r)
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
