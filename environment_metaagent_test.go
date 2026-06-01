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
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(helper.Config), &cfg); err != nil {
		t.Fatalf("config not valid JSON: %v", err)
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
