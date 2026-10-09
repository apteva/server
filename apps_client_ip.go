package main

import (
	"net/http"
	"net/http/httputil"

	sdk "github.com/apteva/app-sdk"
)

// configureAppClientIP resolves the address before ReverseProxy appends its
// internal hop to X-Forwarded-For. Install this after configuring the Director:
// the assertion must describe the final destination URL, not the ingress URL.
// Empty tokens scrub metadata for legacy sidecars and non-app backends without
// turning an unverified client assertion into trusted platform metadata.
// ReverseProxy uses the same Director for HTTP and WebSocket handshakes.
func configureAppClientIP(proxy *httputil.ReverseProxy, source *http.Request, token string) {
	ip := resolvedClientIP(source)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		sdk.SetClientIPHeaders(req, ip, token)
	}
}
