package main

import (
	"context"
	"reflect"
	"testing"
)

func TestAppMCPSurfaceChangedComparesNamesDescriptionsAndSchemas(t *testing.T) {
	base := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{
		{Name: "lookup", Description: "Look up a record", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}},
		{Name: "create", Description: "Create a record", InputSchema: map[string]any{"type": "object"}},
	}}
	reordered := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{base.Tools[1], base.Tools[0]}}
	if appMCPSurfaceChanged(base, reordered) {
		t.Fatal("tool ordering alone changed the surface")
	}

	descriptionChanged := appMCPSurfaceSnapshot{Available: true, Tools: append([]installMCPToolInfo(nil), base.Tools...)}
	descriptionChanged.Tools[0].Description = "Look up one record"
	if !appMCPSurfaceChanged(base, descriptionChanged) {
		t.Fatal("description change was not detected")
	}

	schemaChanged := appMCPSurfaceSnapshot{Available: true, Tools: append([]installMCPToolInfo(nil), base.Tools...)}
	schemaChanged.Tools[0].InputSchema = map[string]any{"type": "object", "required": []any{"id"}}
	if !appMCPSurfaceChanged(base, schemaChanged) {
		t.Fatal("input schema change was not detected")
	}
	if appMCPSurfaceChanged(base, appMCPSurfaceSnapshot{}) {
		t.Fatal("an unavailable post-upgrade snapshot must not trigger speculative restarts")
	}
}

func TestRestartBoundRunningAgentsForAppMCPChangeIsSelective(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "surface-upgrade", "proj-1", []string{"lookup"})

	create := func(name string) int64 {
		agent, err := s.store.CreateAgent(1, name, "directive", "autonomous", "{}", "proj-1")
		if err != nil {
			t.Fatal(err)
		}
		return agent.ID
	}
	boundRunning := create("bound-running")
	boundStopped := create("bound-stopped")
	disabledRunning := create("disabled-running")
	unboundRunning := create("unbound-running")

	if _, err := s.store.db.Exec(
		`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES (?,?,1),(?,?,1),(?,?,0)`,
		installID, boundRunning, installID, boundStopped, installID, disabledRunning,
	); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{boundRunning, disabledRunning, unboundRunning} {
		s.agents.processes[id] = &runningAgent{port: 4100 + i, reattached: true}
	}

	var restarted []int64
	err := s.restartBoundRunningAgentsForAppMCPChange(installID, func(_ context.Context, agentID int64) error {
		restarted = append(restarted, agentID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{boundRunning}; !reflect.DeepEqual(restarted, want) {
		t.Fatalf("restarted=%v want=%v", restarted, want)
	}
}
