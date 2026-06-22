package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

// seedBoundApp inserts an app + install + a binding to the agent, returning
// the install id. Used to set up "this agent uses app X".
func seedBoundApp(t *testing.T, s *Server, appName, projectID string, agentID int64) int64 {
	t.Helper()
	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'local','','','{}')`, appName)
	if err != nil {
		t.Fatalf("seed app %s: %v", appName, err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, ?, 'running')`, appID, projectID)
	if err != nil {
		t.Fatalf("seed install %s: %v", appName, err)
	}
	installID, _ := res.LastInsertId()
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, agentID); err != nil {
		t.Fatalf("seed binding %s: %v", appName, err)
	}
	return installID
}

// TestDeriveEnvironmentSpecForAgent: an agent's bindings become the environment's apps.
func TestDeriveEnvironmentSpecForAgent(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	s := &Server{store: store, port: "5280", environments: NewEnvironmentManager(environmentDataRoot(dataDir))}
	s.environments.server = s
	// Mock the source resolver so the test doesn't depend on the filesystem.
	s.environments.ResolveSource = func(name string) (string, error) { return "/src/" + name, nil }

	agent, err := store.CreateAgent(1, "files-agent", "handle files", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	seedBoundApp(t, s, "storage", "proj-1", agent.ID)
	seedBoundApp(t, s, "crm", "proj-1", agent.ID)

	spec, err := s.DeriveEnvironmentSpecForAgent(agent, "w1")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if spec.ID != "w1" || spec.ProjectID != "proj-1" {
		t.Fatalf("spec scope wrong: %+v", spec)
	}
	if len(spec.AppSrcDirs) != 2 ||
		spec.AppSrcDirs["storage"] != "/src/storage" ||
		spec.AppSrcDirs["crm"] != "/src/crm" {
		t.Fatalf("derived AppSrcDirs wrong: %+v", spec.AppSrcDirs)
	}

	// An unresolvable bound app surfaces as an error (you can't build it).
	s.environments.ResolveSource = defaultSourceResolver
	seedBoundApp(t, s, "definitely-not-real-app-xyz", "proj-1", agent.ID)
	if _, err := s.DeriveEnvironmentSpecForAgent(agent, "w2"); err == nil {
		t.Fatalf("expected error for unresolvable bound app")
	}
}

// TestEnvironment_DerivedFromAgentBindings (gated) is the full bridge: bind the real
// storage app to an agent → CreateEnvironmentForAgent → the environment stands up storage
// from local source. This is the "create eval for this agent → environment built
// from its bindings" path, end to end.
func TestEnvironment_DerivedFromAgentBindings(t *testing.T) {
	requireRealAppEnvironmentTests(t)
	_ = findAppSource(t, "storage") // skip early if storage source is absent
	s := newEnvironmentTestServer(t)

	agent, err := s.store.CreateAgent(1, "files-agent", "handle files via storage", "autonomous", "{}", "proj-files")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	// The agent is bound to storage (as the wizard would write).
	seedBoundApp(t, s, "storage", "proj-files", agent.ID)

	// Environment is derived from the binding — no manual app list.
	environment, err := s.CreateEnvironmentForAgent(agent, "w-files")
	if err != nil {
		t.Fatalf("create environment from agent bindings: %v", err)
	}
	defer environment.Stop()

	inst, ok := environment.Install("storage")
	if !ok {
		t.Fatal("storage not installed in the derived environment")
	}
	token := fmt.Sprintf("dev-%d", inst.InstallID)
	res := callMCP(t, inst.SidecarURL+"/mcp", token, "tools/list", map[string]any{})
	if len(res) == 0 {
		t.Fatal("storage tools/list returned nothing")
	}
	t.Logf("✓ environment derived from agent bindings: storage installed (id=%d) + serving MCP", inst.InstallID)
}
