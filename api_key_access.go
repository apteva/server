package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
)

const (
	APIKeyReadOnly  = "read_only"
	APIKeyReadWrite = "read_write"
)

type readOnlyAPIKeyContext struct{}

func withReadOnlyAPIKey(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), readOnlyAPIKeyContext{}, true))
}
func isReadOnlyAPIKey(r *http.Request) bool {
	restricted, _ := r.Context().Value(readOnlyAPIKeyContext{}).(bool)
	return restricted
}

func privateRequestToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); a != "" {
		return strings.TrimPrefix(a, "Bearer ")
	}
	if token := r.Header.Get("X-API-Key"); token != "" {
		return token
	}
	if apiKeyQueryAllowed(r) {
		return r.URL.Query().Get("api_key")
	}
	if appTokenQueryAllowed(r) {
		token := r.URL.Query().Get("api_key")
		if strings.HasPrefix(token, "app_") || strings.HasPrefix(token, "dev-") {
			return token
		}
	}
	return ""
}

// This is an allowlist, not a GET-method blanket permission. New endpoints,
// credential exports, arbitrary app/core proxies and protocol upgrades must not
// silently gain access. Existing handler-level user/project checks still apply.
func readOnlyAPIRequestAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		return false
	}
	p := r.URL.Path // authMiddleware runs after /api is stripped
	if path.Clean(p) != p || strings.ContainsAny(p, "\\%") {
		return false
	}
	switch p {
	case "/auth/me", "/auth/keys", "/auth/onboarding/status",
		"/projects", "/agents", "/instances",
		"/telemetry", "/telemetry/timeline", "/telemetry/stats",
		"/telemetry/project-activity", "/telemetry/project-stats",
		"/telemetry/project-timeline", "/telemetry/project-tools", "/telemetry/stream",
		"/settings/server", "/settings/new-agent-provider":
		return true
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) == 2 && parts[0] == "projects" && parts[1] != "" {
		return true
	}
	if len(parts) < 2 || (parts[0] != "agents" && parts[0] != "instances") {
		return false
	}
	if id, err := strconv.ParseInt(parts[1], 10, 64); err != nil || id <= 0 {
		return false
	}
	if len(parts) == 2 {
		return true
	}
	if len(parts) != 3 {
		return false
	}
	switch parts[2] {
	case "status", "threads", "events", "chat-history", "config":
		return true
	}
	return false
}

// Configuration can contain executable commands, environment variables and
// provider/MCP credentials. Expose only reviewed scalar settings to read-only
// callers; arbitrary nested config is intentionally not an inspection surface.
func readOnlyAgentConfig(config map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{
		"directive", "mode", "proactivity", "model", "provider", "default_provider",
		"rate", "temperature", "max_tokens", "context_size", "reasoning_effort",
		"background_memory_enabled", "realtime_enabled",
	} {
		switch value := config[key].(type) {
		case string, float64, int, bool:
			out[key] = value
		}
	}
	return out
}

func restrictAgentConfig(r *http.Request, agent *Agent) {
	if !isReadOnlyAPIKey(r) {
		return
	}
	var config map[string]any
	_ = json.Unmarshal([]byte(agent.Config), &config)
	raw, _ := json.Marshal(readOnlyAgentConfig(config))
	agent.Config = string(raw)
}
