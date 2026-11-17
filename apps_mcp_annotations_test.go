package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestAppMCPAnnotationsChangeCapability(t *testing.T) {
	snapshot := func(annotations map[string]any) appMCPSurfaceSnapshot {
		return appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{{
			Name: "lookup", InputSchema: map[string]any{"type": "object"}, Annotations: annotations,
		}}}
	}
	readOnly := map[string]any{"readOnlyHint": true}
	for _, tc := range []struct {
		name          string
		before, after map[string]any
		changed       bool
	}{
		{"added", nil, readOnly, true},
		{"modified", readOnly, map[string]any{"readOnlyHint": false}, true},
		{"removed", readOnly, nil, true},
		{"other hint", readOnly, map[string]any{"readOnlyHint": true, "idempotentHint": true}, true},
		{"extension", readOnly, map[string]any{"readOnlyHint": true, "custom": map[string]any{"mode": "inspect"}}, true},
		{"unchanged", readOnly, map[string]any{"readOnlyHint": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := snapshot(tc.before), snapshot(tc.after)
			if got := appMCPSurfaceChanged(before, after); got != tc.changed {
				t.Fatalf("surface changed=%v want %v", got, tc.changed)
			}
			oldRevision, newRevision := appMCPCapabilityRevision(before, nil), appMCPCapabilityRevision(after, nil)
			if got := oldRevision != newRevision; got != tc.changed {
				t.Fatalf("revision changed=%v want %v", got, tc.changed)
			}
			// The stored revision must also detect upgrades when the old sidecar is unavailable.
			if got := appMCPCapabilityChanged(appMCPSurfaceSnapshot{}, after, oldRevision, newRevision, true, true); got != tc.changed {
				t.Fatalf("capability changed=%v want %v", got, tc.changed)
			}
		})
	}
}

func TestAppMCPAnnotationOnlyActivationRefreshesRunningAgent(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "annotation-refresh", "proj-1", []string{"lookup"})
	var annotationJSON atomic.Value
	annotationJSON.Store(`{}`)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"tools": []any{map[string]any{
				"name": "lookup", "description": "Look up a record", "inputSchema": map[string]any{"type": "object"},
				"annotations": json.RawMessage(annotationJSON.Load().(string)),
			}},
		}})
	}))
	defer sidecar.Close()
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	s.installedApps.Add(&InstalledApp{InstallID: installID, AppName: "annotation-refresh", ProjectID: "proj-1", SidecarURL: sidecar.URL})
	before := s.snapshotAppMCPSurface(installID)
	if !before.Available || len(before.Tools) != 1 {
		t.Fatalf("missing initial snapshot: %#v", before)
	}
	if err := s.registerAppMCPWithSurface(installID, before); err != nil {
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
	agent, err := s.store.CreateAgent(1, "annotation-agent", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES (?,?,1)`, installID, agent.ID); err != nil {
		t.Fatal(err)
	}
	unrelated := map[string]any{"name": "unrelated", "transport": "http", "url": "https://example.test/mcp"}
	current := map[string]any{"mcp_servers": []any{oldConfig, unrelated}}
	applied := make(chan map[string]any, 4)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, current)
		case http.MethodPut:
			var update map[string]any
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			applied <- update
			writeJSON(w, map[string]string{"status": "updated"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer core.Close()
	s.agents.processes[agent.ID] = &runningAgent{port: core.Listener.Addr().(*net.TCPAddr).Port, coreAPIKey: "core-key", reattached: true}

	annotationJSON.Store(`{"readOnlyHint":true,"destructiveHint":false,"custom":{"mode":"inspect"}}`)
	after := s.snapshotAppMCPSurface(installID)
	want := map[string]any{"readOnlyHint": true, "destructiveHint": false, "custom": map[string]any{"mode": "inspect"}}
	if !after.Available || len(after.Tools) != 1 || !reflect.DeepEqual(after.Tools[0].Annotations, want) {
		t.Fatalf("tools/list annotations were not preserved: %#v", after)
	}
	if err := s.registerAppMCPAfterActivation(installID, before); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-applied:
		servers := mcpMaps(update["mcp_servers"])
		if len(servers) != 2 || !hasMCPName(servers, "unrelated") {
			t.Fatalf("unrelated attachment changed: %#v", servers)
		}
		var newURL string
		for _, server := range servers {
			if server["name"] == "annotation-refresh" {
				newURL, _ = server["url"].(string)
			}
		}
		if newURL == "" || newURL == oldConfig["url"] {
			t.Fatal("annotation-only upgrade did not revise the live MCP URL")
		}
	default:
		t.Fatal("annotation-only upgrade did not reconcile the running agent")
	}
	if !s.agents.IsRunning(agent.ID) {
		t.Fatal("agent stopped during live reconciliation")
	}
	if err := s.registerAppMCPAfterActivation(installID, after); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
		t.Fatal("unchanged annotations triggered another reconciliation")
	default:
	}
}
