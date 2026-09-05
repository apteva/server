package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func internalMCPCapability(secret, path string) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("mcp-route-v1:" + path))
	return hex.EncodeToString(mac.Sum(nil))
}
func authorizeMCPURL(raw, secret string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Path, "/mcp/") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return raw
	}
	token := internalMCPCapability(secret, u.Path)
	if token == "" {
		return raw
	}
	q := u.Query()
	q.Set("mcp_token", token)
	u.RawQuery = q.Encode()
	return u.String()
}
func (s *Server) authorizedInternalMCPRequest(r *http.Request) bool {
	expected := internalMCPCapability(s.instanceSecret, r.URL.Path)
	provided := r.URL.Query().Get("mcp_token")
	if provided == "" {
		provided = r.Header.Get("X-Apteva-MCP-Capability")
	}
	return expected != "" && hmac.Equal([]byte(provided), []byte(expected))
}
func authorizeCoreMCPURLs(config map[string]any, port, secret string) {
	entries, _ := config["mcp_servers"].([]any)
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		raw, _ := m["url"].(string)
		u, err := url.Parse(raw)
		if err == nil && u.Port() == port {
			m["url"] = authorizeMCPURL(raw, secret)
		}
	}
}

// Authorize references before issuing a route capability, including raw config
// updates and persisted configs from before capabilities were introduced.
func (s *Server) authorizeAgentMCPConfig(inst *Agent, config map[string]any) error {
	entries, _ := config["mcp_servers"].([]any)
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		raw, _ := m["url"].(string)
		u, err := url.Parse(raw)
		if err != nil || u.Port() != s.port || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.HasPrefix(u.Path, "/mcp/") {
			continue
		}
		part := strings.TrimPrefix(u.Path, "/mcp/")
		connection := strings.HasPrefix(part, "connection/")
		part = strings.TrimPrefix(strings.TrimPrefix(part, "connection/"), "custom/")
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid internal MCP reference")
		}
		row, err := s.store.GetMCPServerByIDUnscoped(id)
		if connection || (err == nil && row == nil) {
			row, err = s.store.FindCanonicalMCPServerByConnection(id)
		}
		if err != nil || row == nil {
			return fmt.Errorf("internal MCP reference %d not found", id)
		}
		if _, err = s.resolveAgentMCPConfigs(inst.UserID, inst, []int64{row.ID}); err != nil {
			return err
		}
		m["url"] = authorizeMCPURL(raw, s.instanceSecret)
	}
	return nil
}
