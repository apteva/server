package main

import "net/http"

func authorizeTestMCPRequest(s *Server, r *http.Request) {
	if s.instanceSecret == "" {
		s.instanceSecret = "review-test-mcp-secret"
	}
	r.Header.Set("X-Apteva-MCP-Capability", internalMCPCapability(s.instanceSecret, r.URL.Path))
}
