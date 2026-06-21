package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEnsureEnvironmentMCPOnHelper: the meta-agent's config gains the environment-mcp
// server, and re-applying is a no-op (no duplicate).
func TestEnsureEnvironmentMCPOnHelper(t *testing.T) {
	s := &Server{port: "5280"}
	helper := &Agent{Config: "{}"}

	s.ensureEnvironmentMCPOnHelper(helper)
	if !strings.Contains(helper.Config, "/api/environment-mcp") || !strings.Contains(helper.Config, `"environments"`) {
		t.Fatalf("environment-mcp not added: %s", helper.Config)
	}

	// Idempotent: second call must not add a duplicate.
	s.ensureEnvironmentMCPOnHelper(helper)
	var cfg struct {
		IncludeAptevaServer bool             `json:"include_apteva_server"`
		IncludeChannels     bool             `json:"include_channels"`
		MCPServers          []map[string]any `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(helper.Config), &cfg); err != nil {
		t.Fatalf("config not valid JSON: %v", err)
	}
	if !cfg.IncludeAptevaServer {
		t.Fatalf("expected platform helper to force include_apteva_server=true: %s", helper.Config)
	}
	if !cfg.IncludeChannels {
		t.Fatalf("expected platform helper to force include_channels=true: %s", helper.Config)
	}
	n := 0
	for _, m := range cfg.MCPServers {
		if m["name"] == "environments" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 'environments' mcp_server, got %d: %s", n, helper.Config)
	}
}

func TestEnsureEnvironmentMCPOnHelperRemovesLegacyWorlds(t *testing.T) {
	s := &Server{port: "5280"}
	helper := &Agent{Config: `{"mcp_servers":[{"name":"worlds","no_spawn":true,"transport":"http","url":"http://127.0.0.1:5280/api/world-mcp"},{"name":"environments","no_spawn":true,"transport":"http","url":"http://127.0.0.1:5280/api/environment-mcp"}]}`}

	s.ensureEnvironmentMCPOnHelper(helper)

	var cfg struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(helper.Config), &cfg); err != nil {
		t.Fatalf("config not valid JSON: %v", err)
	}
	for _, m := range cfg.MCPServers {
		url, _ := m["url"].(string)
		if m["name"] == "worlds" || strings.Contains(url, "/api/world-mcp") {
			t.Fatalf("legacy worlds mcp_server was not removed: %s", helper.Config)
		}
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0]["name"] != "environments" {
		t.Fatalf("expected only environments mcp_server, got: %s", helper.Config)
	}
}
