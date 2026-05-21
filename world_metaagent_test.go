package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEnsureWorldMCPOnHelper: the meta-agent's config gains the world-mcp
// server, and re-applying is a no-op (no duplicate).
func TestEnsureWorldMCPOnHelper(t *testing.T) {
	s := &Server{port: "5280"}
	helper := &Agent{Config: "{}"}

	s.ensureWorldMCPOnHelper(helper)
	if !strings.Contains(helper.Config, "/api/world-mcp") || !strings.Contains(helper.Config, `"worlds"`) {
		t.Fatalf("world-mcp not added: %s", helper.Config)
	}

	// Idempotent: second call must not add a duplicate.
	s.ensureWorldMCPOnHelper(helper)
	var cfg struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(helper.Config), &cfg); err != nil {
		t.Fatalf("config not valid JSON: %v", err)
	}
	n := 0
	for _, m := range cfg.MCPServers {
		if m["name"] == "worlds" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 'worlds' mcp_server, got %d: %s", n, helper.Config)
	}
}
