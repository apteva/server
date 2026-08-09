package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReconcileAgentAppSkills proves the app→agent binding's skill half:
// binding an app attaches its skills to the agent's shared memory journal.
func TestReconcileAgentAppSkills(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	s := &Server{store: store, agents: NewAgentManager(filepath.Join(dataDir, "agents"), "")}
	ensureTestAdmin(t, s)

	// An installed app that ships a skill.
	res, err := store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('storage','local','','','{}')`)
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, '', 'running')`, appID)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	installID, _ := res.LastInsertId()
	if _, err := store.db.Exec(
		`INSERT INTO skills (slug, name, description, body, source, install_id, project_id, enabled)
		 VALUES ('storage:files-howto','Files how-to','how to use files','Use files_write to save a file.','app', ?, '', 1)`,
		installID); err != nil {
		t.Fatalf("skill: %v", err)
	}

	agent, err := store.CreateAgent(1, "files-agent", "handle files", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	// No bindings → nothing inherited.
	if stats, err := s.reconcileAgentAppSkills(agent); err != nil {
		t.Fatal(err)
	} else if stats.Added != 0 {
		t.Fatalf("expected 0 with no bindings, got %+v", stats)
	}

	// Bound → the app's skill is inherited.
	if _, err := store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, agent.ID); err != nil {
		t.Fatal(err)
	}
	if stats, err := s.reconcileAgentAppSkills(agent); err != nil {
		t.Fatal(err)
	} else if stats.Added != 1 {
		t.Fatalf("expected 1 inherited skill, got %+v", stats)
	}

	data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "memory.jsonl"))
	if err != nil {
		t.Fatalf("read agent journal: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(data)), "files") {
		t.Fatalf("inherited skill not in agent journal: %s", data)
	}
}
