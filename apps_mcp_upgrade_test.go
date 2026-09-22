package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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
	metadataChanged := appMCPSurfaceSnapshot{Available: true, Tools: append([]installMCPToolInfo(nil), base.Tools...)}
	metadataChanged.Tools[0].Meta = map[string]any{"io.apteva/wakeOnResult": true}
	if !appMCPSurfaceChanged(base, metadataChanged) {
		t.Fatal("protocol metadata change was not detected")
	}
	if appMCPSurfaceChanged(base, appMCPSurfaceSnapshot{}) {
		t.Fatal("an unavailable post-upgrade snapshot must not trigger speculative restarts")
	}
}

func TestAppMCPCapabilityRevisionTracksContractNotImplementationVersion(t *testing.T) {
	base := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{
		{Name: "lookup", Description: "Look up a record", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}},
		{Name: "create", Description: "Create a record", InputSchema: map[string]any{"type": "object"}},
	}}
	baseRevision := appMCPCapabilityRevision(base, nil)
	reordered := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{base.Tools[1], base.Tools[0]}}
	if got := appMCPCapabilityRevision(reordered, nil); got != baseRevision {
		t.Fatalf("tool ordering changed capability revision: %s != %s", got, baseRevision)
	}
	changed := appMCPSurfaceSnapshot{Available: true, Tools: append([]installMCPToolInfo(nil), base.Tools...)}
	changed.Tools[0].InputSchema = map[string]any{"type": "object", "required": []any{"id"}}
	if got := appMCPCapabilityRevision(changed, nil); got == baseRevision {
		t.Fatal("schema change did not change capability revision")
	}
	metadataChanged := appMCPSurfaceSnapshot{Available: true, Tools: append([]installMCPToolInfo(nil), base.Tools...)}
	metadataChanged.Tools[0].Meta = map[string]any{"io.apteva/wakeOnResult": true}
	if got := appMCPCapabilityRevision(metadataChanged, nil); got == baseRevision {
		t.Fatal("protocol metadata change did not change capability revision")
	}
}

func TestAppMCPCapabilityChangedUsesBridgeRevisionWhenSnapshotsUnavailable(t *testing.T) {
	unavailable := appMCPSurfaceSnapshot{}
	if appMCPCapabilityChanged(unavailable, unavailable, "same", "same", true, true) {
		t.Fatal("unchanged stored revision triggered a capability refresh")
	}
	if !appMCPCapabilityChanged(unavailable, unavailable, "old", "new", true, true) {
		t.Fatal("stored revision change was missed")
	}
	if !appMCPCapabilityChanged(unavailable, unavailable, "old", "", true, false) {
		t.Fatal("removed MCP bridge was missed")
	}
	if !appMCPCapabilityChanged(unavailable, unavailable, "", "new", false, true) {
		t.Fatal("new MCP bridge was missed")
	}
}

func TestReconcileBoundRunningAgentsForAppMCPChangeIsSelective(t *testing.T) {
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

	var reconciled, restarted []int64
	err := s.reconcileBoundRunningAgentsForAppMCPChange(installID, func(_ context.Context, agentID int64) error {
		reconciled = append(reconciled, agentID)
		return nil
	}, func(_ context.Context, agentID int64) error {
		restarted = append(restarted, agentID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{boundRunning}; !reflect.DeepEqual(reconciled, want) {
		t.Fatalf("reconciled=%v want=%v", reconciled, want)
	}
	if len(restarted) != 0 {
		t.Fatalf("successful live reconcile unexpectedly restarted=%v", restarted)
	}
}

func TestReconcileBoundRunningAgentsFallsBackToRestart(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "surface-fallback", "proj-1", []string{"lookup"})
	agent, err := s.store.CreateAgent(1, "bound-running", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES (?,?,1)`,
		installID, agent.ID,
	); err != nil {
		t.Fatal(err)
	}
	s.agents.processes[agent.ID] = &runningAgent{port: 4100, reattached: true}

	var restarted []int64
	err = s.reconcileBoundRunningAgentsForAppMCPChange(installID, func(context.Context, int64) error {
		return errors.New("live reconcile failed")
	}, func(_ context.Context, agentID int64) error {
		restarted = append(restarted, agentID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{agent.ID}; !reflect.DeepEqual(restarted, want) {
		t.Fatalf("restarted=%v want=%v", restarted, want)
	}
}

func TestReconcileRunningAgentAppMCPUpdatesOnlyLiveAttachment(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "live-refresh", "proj-1", []string{"lookup"})
	oldSurface := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{{
		Name: "lookup", Description: "old description", InputSchema: map[string]any{"type": "object"},
	}}}
	if err := s.registerAppMCPWithSurface(installID, oldSurface); err != nil {
		t.Fatal(err)
	}
	row := readMCPRow(t, s, installID)
	record, err := s.store.GetMCPServerByIDUnscoped(row["id"].(int64))
	if err != nil {
		t.Fatal(err)
	}
	oldConfig, err := gatewayMCPConfigFromRecord(*record, "proj-1", localServerPort(), "")
	if err != nil {
		t.Fatal(err)
	}
	oldURL := oldConfig["url"].(string)

	agent, err := s.store.CreateAgent(1, "live-refresh-agent", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	current := map[string]any{
		"mcp_servers": []any{
			oldConfig,
			map[string]any{"name": "unrelated", "transport": "http", "url": "https://example.test/mcp"},
		},
	}
	var applied map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&applied); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			current["mcp_servers"] = applied["mcp_servers"]
			writeJSON(w, map[string]string{"status": "updated"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer core.Close()
	port := core.Listener.Addr().(*net.TCPAddr).Port
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}

	newSurface := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{{
		Name: "lookup", Description: "new description", InputSchema: map[string]any{"type": "object"},
	}}}
	if err := s.registerAppMCPWithSurface(installID, newSurface); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileRunningAgentAppMCP(context.Background(), agent.ID, installID); err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("running Core did not receive a live MCP reconciliation")
	}
	servers := mcpMaps(applied["mcp_servers"])
	if len(servers) != 2 {
		t.Fatalf("MCP count=%d want 2: %#v", len(servers), servers)
	}
	var newURL string
	for _, server := range servers {
		if server["name"] == "live-refresh" {
			newURL, _ = server["url"].(string)
		}
	}
	if newURL == "" || newURL == oldURL {
		t.Fatalf("app capability URL was not revised: old=%q new=%q", oldURL, newURL)
	}
	if !hasMCPName(servers, "unrelated") {
		t.Fatalf("unrelated MCP was removed: %#v", servers)
	}
	if !s.agents.IsRunning(agent.ID) {
		t.Fatal("agent process was stopped during live attachment reconciliation")
	}
}

func TestRemoveAppInstallMCPConfigPreservesOtherServers(t *testing.T) {
	current := []map[string]any{
		{"name": "target", "url": "http://127.0.0.1:5280/api/apps/target/mcp?install_id=42&cap_rev=old"},
		{"name": "other-app", "url": "http://127.0.0.1:5280/api/apps/other/mcp?install_id=43"},
		{"name": "remote", "url": "https://example.test/mcp"},
	}
	got := removeAppInstallMCPConfig(current, 42)
	if len(got) != 2 || hasMCPName(got, "target") || !hasMCPName(got, "other-app") || !hasMCPName(got, "remote") {
		t.Fatalf("unexpected MCP set after app surface removal: %#v", got)
	}
}
