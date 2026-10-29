package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"strings"
)

// resolveRemoteMcpAuthHeader resolves a remote_mcp entry's auth header
// against a connection's credentials. Pass normalizeCredentials output so
// templates see the same view as REST integrations (aliases, derived
// basic_auth). Returns the env key probeRemoteMCP / MCPManager.Start read
// the header from, and the resolved value.
func resolveRemoteMcpAuthHeader(app *AppTemplate, creds map[string]string) (envKey, value string, err error) {
	// Default header pattern matches the TS generator: Authorization:
	// Bearer {{token}}. Templates can override per-app for vendors that
	// expect a different header (e.g. X-Auth-Token) or value layout.
	headerName := "Authorization"
	headerValue := "Bearer {{token}}"
	if app.MCP.AuthHeader != nil {
		if app.MCP.AuthHeader.Name != "" {
			headerName = app.MCP.AuthHeader.Name
		}
		if app.MCP.AuthHeader.Value != "" {
			headerValue = app.MCP.AuthHeader.Value
		}
	}
	resolved, ok := resolveRemoteMcpCredTemplate(headerValue, creds)
	if !ok {
		// At least one {{name}} placeholder did not resolve to a real
		// credential value. Fail loud — the upstream MCP would reject
		// the call anyway, and a silent "this connection is broken"
		// row is worse than a startup error operators can see.
		return "", "", fmt.Errorf("could not resolve auth header %q from connection credentials", headerValue)
	}
	// Standard headers map to the keys the proxy reads case-sensitively;
	// any other header passes through under its upper-cased name.
	envKey = strings.ToUpper(headerName)
	if envKey == "X-API-KEY" {
		envKey = "API_KEY"
	}
	return envKey, resolved, nil
}

// resolveRemoteMcpURL resolves {{credential}} placeholders in a remote_mcp
// entry's URL, for servers whose address differs per connection (e.g. a
// customer's own WordPress site). Fixed vendor URLs are returned unchanged.
// A templated URL points the server at a host the user typed, so it must be
// https and resolve to public addresses only.
func resolveRemoteMcpURL(ctx context.Context, app *AppTemplate, creds map[string]string) (string, error) {
	if !strings.Contains(app.MCP.URL, "{{") {
		return app.MCP.URL, nil
	}
	// Trim trailing slashes so "https://example.com/" + "/wp-json/..."
	// does not produce "//".
	trimmed := make(map[string]string, len(creds))
	for k, v := range creds {
		trimmed[k] = strings.TrimRight(v, "/")
	}
	resolved, ok := resolveRemoteMcpCredTemplate(app.MCP.URL, trimmed)
	if !ok {
		return "", fmt.Errorf("could not resolve mcp url %q from connection credentials", app.MCP.URL)
	}
	if err := validatePublicHTTPSURL(ctx, resolved); err != nil {
		return "", fmt.Errorf("mcp url %s: %w", resolved, err)
	}
	return resolved, nil
}

// validatePublicHTTPSURL rejects user-supplied URLs that are not https or
// whose host resolves to a loopback, private, link-local or otherwise
// non-public address. It checks at resolve time only; it does not pin the
// addresses later connections dial.
func validatePublicHTTPSURL(ctx context.Context, raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return errors.New("must be an https URL")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return errors.New("host cannot be resolved")
	}
	for _, addr := range addrs {
		ip := addr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("host resolves to a private address")
		}
	}
	return nil
}
