package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedMCPNames(t *testing.T, s *Server, agentID int64) []string {
	t.Helper()
	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(inst.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	return mcpNamesIn(cfg)
}

func mcpNamesIn(cfg map[string]any) []string {
	names := []string{}
	for _, entry := range mcpMaps(cfg["mcp_servers"]) {
		name, _ := entry["name"].(string)
		names = append(names, name)
	}
	return names
}

func fakeCoreForStart(t *testing.T, s *Server) {
	t.Helper()
	core := filepath.Join(t.TempDir(), "fake-core")
	script := "#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1 & wait $!; done\n"
	if err := os.WriteFile(core, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.agents.coreCmd = core
	s.agents.AuthorizeMCPConfig = s.authorizeAgentMCPConfig
}

// A saved entry for another project's MCP server used to fail every start.
func TestAgentStartsAfterDroppingSavedMCPFromAnotherProject(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	ensureTestAdmin(t, s)
	fakeCoreForStart(t, s)
	mine, err := s.store.CreateMCPServer(1, "mine", "mine-mcp", `[]`, "", "Mine", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateMCPServer(1, "other", "other-mcp", `[]`, "", "Other", "project-b")
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(map[string]any{"mcp_servers": []any{
		map[string]any{"name": "other", "transport": "http", "url": "http://127.0.0.1:5280/mcp/" + itoa64(other.ID)},
		map[string]any{"name": "mine", "transport": "http", "url": "http://127.0.0.1:5280/mcp/" + itoa64(mine.ID)},
		map[string]any{"name": "gone", "transport": "http", "url": "http://127.0.0.1:5280/mcp/999999"},
	}})
	inst, err := s.store.CreateAgent(1, "project-a-agent", "directive", "autonomous", string(saved), "project-a")
	if err != nil {
		t.Fatal(err)
	}

	err = s.agents.Start(inst, nil, s.port, nil, s.instanceSecret)
	if err == nil || !strings.Contains(err.Error(), "belongs to project") {
		s.agents.Stop(inst.ID)
		t.Fatalf("start with another project's saved entry: err=%v, want project mismatch", err)
	}

	if err := s.dropUnusableSavedMCPs(inst); err != nil {
		t.Fatal(err)
	}
	if got := savedMCPNames(t, s, inst.ID); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("saved entries after cleanup = %v, want [mine]", got)
	}
	if err := s.agents.Start(inst, nil, s.port, nil, s.instanceSecret); err != nil {
		t.Fatalf("start after cleanup: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(inst.ID) })

	raw, err := os.ReadFile(filepath.Join(s.agents.InstanceDir(inst.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	started := strings.Join(mcpNamesIn(cfg), ",")
	if !strings.Contains(started, "mine") || strings.Contains(started, "other") || strings.Contains(started, "gone") {
		t.Fatalf("started with MCP servers %q, want mine without other or gone", started)
	}
}

// A saved configuration with nothing wrong is not rewritten.
func TestDropUnusableSavedMCPsKeepsUsableEntries(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	ensureTestAdmin(t, s)
	mine, err := s.store.CreateMCPServer(1, "mine", "mine-mcp", `[]`, "", "Mine", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	saved := `{"mcp_servers":[{"name":"mine","transport":"http","url":"http://127.0.0.1:5280/mcp/` + itoa64(mine.ID) + `"},{"name":"remote","transport":"http","url":"https://example.com/mcp"}],"include_channels":true}`
	inst, err := s.store.CreateAgent(1, "project-a-agent", "directive", "autonomous", saved, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	before := inst.Config
	if err := s.dropUnusableSavedMCPs(inst); err != nil {
		t.Fatal(err)
	}
	if inst.Config != before {
		t.Fatalf("usable saved config was rewritten:\n before %s\n after  %s", before, inst.Config)
	}
}

// A detached entry that was still in the saved configuration used to come
// back on the next start.
func TestDetachedSavedMCPStaysDetachedAfterStart(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	ensureTestAdmin(t, s)
	fakeCoreForStart(t, s)
	keep, err := s.store.CreateMCPServer(1, "keep", "keep-mcp", `[]`, "", "Keep", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	detach, err := s.store.CreateMCPServer(1, "detach", "detach-mcp", `[]`, "", "Detach", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(map[string]any{"mcp_servers": []any{
		map[string]any{"name": "keep", "transport": "http", "url": "http://127.0.0.1:5280/mcp/" + itoa64(keep.ID)},
		map[string]any{"name": "detach", "transport": "http", "url": "http://127.0.0.1:5280/mcp/" + itoa64(detach.ID)},
	}})
	inst, err := s.store.CreateAgent(1, "project-a-agent", "directive", "autonomous", string(saved), "project-a")
	if err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPost, "/instances/"+itoa64(inst.ID)+"/mcp-servers", "", map[string]any{
		"action": "remove", "mcp_server_ids": []int64{detach.ID},
	})
	rec := httptest.NewRecorder()
	s.handleAgentMCPServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(savedMCPNames(t, s, inst.ID), ","); got != "keep" {
		t.Fatalf("saved entries after detach = %q, want keep", got)
	}

	latest, err := s.store.GetAgentByID(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Start(latest, nil, s.port, nil, s.instanceSecret); err != nil {
		t.Fatalf("start after detach: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(latest.ID) })
	raw, err := os.ReadFile(filepath.Join(s.agents.InstanceDir(latest.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	started := strings.Join(mcpNamesIn(cfg), ",")
	if !strings.Contains(started, "keep") || strings.Contains(started, "detach") {
		t.Fatalf("started with MCP servers %q, want keep without detach", started)
	}
}
