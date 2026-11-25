package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func exitingCoreServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	registerAndLogin(t, s)
	script := filepath.Join(t.TempDir(), "fake-core")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'stopping here' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.agents.coreCmd = script
	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	return s
}

func appEntry(name string, installID int64) map[string]any {
	return map[string]any{"name": name, "transport": "http", "url": fmt.Sprintf("http://127.0.0.1:5280/api/apps/%s/mcp?install_id=%d", name, installID)}
}

func mcpNames(value any) []string {
	var names []string
	for _, entry := range mcpMaps(value) {
		if name, _ := entry["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// startOnce runs the start path up to Core, which exits at once; Start has
// written the config.json Core would have loaded by then.
func startOnce(t *testing.T, s *Server, agentID int64) (savedApps []string, coreApps []string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleStartInstance(w, authedRequest(t, http.MethodPost, fmt.Sprintf("/instances/%d/start", agentID), "", nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("start status=%d body=%s, want the fake core's exit", w.Code, w.Body.String())
	}
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(agent.Config), &saved); err != nil {
		t.Fatal(err)
	}
	core := readSavedConfig(t, filepath.Join(s.agents.instanceDir(agentID), "config.json"))
	return mcpNames(saved["mcp_servers"]), mcpNames(core["mcp_servers"])
}

// An app taken off an agent stays off at its next start, even though the
// agent's saved configuration still listed it from when the agent was made.
// Apps the agent is linked to, and entries that are not apps, are kept; a
// disabled app is left out until it is enabled again.
func TestStartDropsSavedAppEntriesNoLongerLinked(t *testing.T) {
	s := exitingCoreServer(t)
	tasks := seedInstall(t, s, "tasks", "")
	crm := seedInstall(t, s, "crm", "")
	files := seedInstall(t, s, "files", "")
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='disabled' WHERE id=?`, files); err != nil {
		t.Fatal(err)
	}
	custom := map[string]any{"name": "custom", "transport": "http", "url": "https://mcp.example.test/mcp"}
	config, _ := json.Marshal(map[string]any{
		"include_channels": false, "include_apteva_server": false,
		"mcp_servers": []any{appEntry("tasks", tasks), appEntry("crm", crm), appEntry("files", files), custom},
	})
	agent, err := s.store.CreateAgent(1, "ran-before", "directive", "autonomous", string(config), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, install := range []int64{crm, files} {
		if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, install, agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.store.db.Exec(`UPDATE agents SET core_version='0.1.0' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	// Core saved the agent without tasks when it was taken off.
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = []any{appEntry("crm", crm), custom}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	saved, core := startOnce(t, s, agent.ID)
	if want := []string{"crm", "custom"}; fmt.Sprint(saved) != fmt.Sprint(want) {
		t.Fatalf("saved configuration apps = %v, want %v", saved, want)
	}
	for _, name := range core {
		if name == "tasks" || name == "files" {
			t.Fatalf("Core was given %s: %v", name, core)
		}
	}
	if !contains(core, "crm") || !contains(core, "custom") {
		t.Fatalf("Core apps = %v, want crm and custom kept", core)
	}
}

// Before an agent's first start, its saved configuration is the only record of
// the apps it was made with, even when config.json already holds other
// settings. Those apps reach Core and are not dropped.
func TestFirstStartKeepsAppsTheAgentWasMadeWith(t *testing.T) {
	s := exitingCoreServer(t)
	tasks := seedInstall(t, s, "tasks", "")
	config, _ := json.Marshal(map[string]any{
		"include_channels": false, "include_apteva_server": false,
		"mcp_servers": []any{appEntry("tasks", tasks)},
	})
	agent, err := s.store.CreateAgent(1, "brand-new", "directive", "autonomous", string(config), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["directive"] = "edited before the first start"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	saved, core := startOnce(t, s, agent.ID)
	if !contains(saved, "tasks") || !contains(core, "tasks") {
		t.Fatalf("saved=%v core=%v, want tasks in both", saved, core)
	}
}
