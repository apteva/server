package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeAgentCore serves a running agent's GET/PUT /config. A PUT replaces the
// MCP set unless reject is set, in which case it answers with that status and
// body, as Core does when a connection cannot be made.
type fakeAgentCore struct {
	mu      sync.Mutex
	servers []any
	puts    int
	reject  int
	body    string
}

func (c *fakeAgentCore) serve(t *testing.T, s *Server, agentID int64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"mcp_servers": c.servers})
		case http.MethodPut:
			c.puts++
			if c.reject != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.reject)
				_, _ = w.Write([]byte(c.body))
				return
			}
			var update map[string]any
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.servers, _ = update["mcp_servers"].([]any)
			writeJSON(w, map[string]string{"status": "updated"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	s.agents.processes[agentID] = &runningAgent{port: srv.Listener.Addr().(*net.TCPAddr).Port, coreAPIKey: "core-key", reattached: true}
}

func (c *fakeAgentCore) current() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return mcpMaps(c.servers)
}

func (c *fakeAgentCore) putCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

func appBridgeEntry(t *testing.T, s *Server, installID int64, projectID string) map[string]any {
	t.Helper()
	row := readMCPRow(t, s, installID)
	record, err := s.store.GetMCPServerByIDUnscoped(row["id"].(int64))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := gatewayMCPConfigFromRecord(*record, projectID, localServerPort(), "")
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func writeSavedAgentMCP(t *testing.T, s *Server, agentID int64, servers ...map[string]any) {
	t.Helper()
	dir := s.agents.instanceDir(agentID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"mcp_servers": mcpMapsAsAny(servers)})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readSavedAgentMCP(t *testing.T, s *Server, agentID int64) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agentID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return mcpMaps(cfg["mcp_servers"])
}

func hasAppInstallEntry(servers []map[string]any, installID int64) bool {
	for _, entry := range servers {
		if appInstallIDFromMCPConfig(entry) == installID {
			return true
		}
	}
	return false
}

func bindingCount(t *testing.T, s *Server, installID int64) int {
	t.Helper()
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND enabled=1`, installID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func bindAgent(t *testing.T, s *Server, installID, agentID int64) {
	t.Helper()
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES (?,?,1)`, installID, agentID); err != nil {
		t.Fatal(err)
	}
}

func setInstallStatus(t *testing.T, s *Server, installID int64, status string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/apps/installs/%d/status", installID), bytes.NewBufferString(`{"status":"`+status+`"}`))
	rec := httptest.NewRecorder()
	s.handleSetInstallStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set status %s: %d %s", status, rec.Code, rec.Body.String())
	}
}

var unrelatedMCP = map[string]any{"name": "unrelated", "transport": "http", "url": "https://example.test/mcp"}

func TestDisablingAppDetachesItFromBoundAgentsUntilEnabled(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	installID := seedAppWithTools(t, s, "tasks", "proj-1", []string{"list"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	entry := appBridgeEntry(t, s, installID, "proj-1")

	running, err := s.store.CreateAgent(1, "running", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := s.store.CreateAgent(1, "stopped", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	core := &fakeAgentCore{servers: mcpMapsAsAny([]map[string]any{entry, unrelatedMCP})}
	core.serve(t, s, running.ID)
	writeSavedAgentMCP(t, s, stopped.ID, entry, unrelatedMCP)
	bindAgent(t, s, installID, running.ID)
	bindAgent(t, s, installID, stopped.ID)

	setInstallStatus(t, s, installID, "disabled")
	if live := core.current(); hasAppInstallEntry(live, installID) || !hasMCPName(live, "unrelated") {
		t.Fatalf("running agent still carries the disabled app: %#v", live)
	}
	if saved := readSavedAgentMCP(t, s, stopped.ID); hasAppInstallEntry(saved, installID) || !hasMCPName(saved, "unrelated") {
		t.Fatalf("stopped agent still carries the disabled app: %#v", saved)
	}
	if n := bindingCount(t, s, installID); n != 2 {
		t.Fatalf("disable dropped the bindings that re-attach the app: count=%d", n)
	}

	setInstallStatus(t, s, installID, "running")
	if live := core.current(); !hasAppInstallEntry(live, installID) || !hasMCPName(live, "unrelated") {
		t.Fatalf("enable did not re-attach the app to the running agent: %#v", live)
	}
	if saved := readSavedAgentMCP(t, s, stopped.ID); !hasAppInstallEntry(saved, installID) || !hasMCPName(saved, "unrelated") {
		t.Fatalf("enable did not re-attach the app to the stopped agent: %#v", saved)
	}
	if n := bindingCount(t, s, installID); n != 2 {
		t.Fatalf("bindings after enable: count=%d want 2", n)
	}
}

func TestAgentStartDropsDisabledAppEntryAndKeepsBinding(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "tasks", "proj-1", []string{"list"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	entry := appBridgeEntry(t, s, installID, "proj-1")
	// State left by a disable that did not detach the app.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='disabled' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	if err := s.unregisterAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "restarting", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	writeSavedAgentMCP(t, s, agent.ID, entry, unrelatedMCP)
	bindAgent(t, s, installID, agent.ID)

	if err := s.refreshAgentAppMCPConfigs(agent); err != nil {
		t.Fatal(err)
	}
	if saved := readSavedAgentMCP(t, s, agent.ID); hasAppInstallEntry(saved, installID) || !hasMCPName(saved, "unrelated") {
		t.Fatalf("agent starts with the disabled app's dead entry: %#v", saved)
	}
	if n := bindingCount(t, s, installID); n != 1 {
		t.Fatalf("start dropped the binding that re-attaches the app: count=%d", n)
	}
}

func TestRemovingAppWithoutBridgeDetachesByInstallID(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "tasks", "proj-1", []string{"list"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	entry := appBridgeEntry(t, s, installID, "proj-1")
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='disabled' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	if err := s.unregisterAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "detach", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	writeSavedAgentMCP(t, s, agent.ID, entry, unrelatedMCP)
	bindAgent(t, s, installID, agent.ID)

	req := authedRequest(t, http.MethodPost, "/instances/"+itoa64(agent.ID)+"/mcp-servers", "", map[string]any{
		"action": "remove", "bound_app_install_ids": []int64{installID},
	})
	rec := httptest.NewRecorder()
	s.handleAgentMCPServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove disabled app: %d %s", rec.Code, rec.Body.String())
	}
	if saved := readSavedAgentMCP(t, s, agent.ID); hasAppInstallEntry(saved, installID) || !hasMCPName(saved, "unrelated") {
		t.Fatalf("remove left the disabled app attached: %#v", saved)
	}
	if n := bindingCount(t, s, installID); n != 0 {
		t.Fatalf("remove kept the binding, so enabling the app would re-attach it: count=%d", n)
	}
}

func TestRepeatedRegistrationOfOneActivationReconcilesOnce(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "conversations", "proj-1", []string{"send"})
	var description sync.Map
	description.Store("d", "old")
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := description.Load("d")
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"tools": []any{map[string]any{"name": "send", "description": d, "inputSchema": map[string]any{"type": "object"}}},
		}})
	}))
	defer sidecar.Close()
	if s.installedApps == nil {
		s.installedApps = NewInstalledAppsRegistry()
	}
	s.installedApps.Add(&InstalledApp{InstallID: installID, AppName: "conversations", ProjectID: "proj-1", SidecarURL: sidecar.URL})
	before := s.snapshotAppMCPSurface(installID)
	if !before.Available {
		t.Fatal("missing initial snapshot")
	}
	if err := s.registerAppMCPWithSurface(installID, before); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "bound", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	bindAgent(t, s, installID, agent.ID)
	// Core keeps answering with the configuration it had before the upgrade,
	// as a Core that is still applying it or restarting does.
	core := &fakeAgentCore{servers: mcpMapsAsAny([]map[string]any{appBridgeEntry(t, s, installID, "proj-1"), unrelatedMCP})}
	core.serve(t, s, agent.ID)
	stale := core.servers

	description.Store("d", "new")
	if err := s.registerAppMCPAfterActivation(installID, before); err != nil {
		t.Fatal(err)
	}
	if n := core.putCount(); n != 1 {
		t.Fatalf("first registration reconciled %d times, want 1", n)
	}
	core.mu.Lock()
	core.servers = stale
	core.mu.Unlock()
	// The upgrade path registers the same activation again with the snapshot
	// it took before the upgrade.
	if err := s.registerAppMCPAfterActivation(installID, before); err != nil {
		t.Fatal(err)
	}
	if n := core.putCount(); n != 1 {
		t.Fatalf("registering the same activation again reconciled the agent again: puts=%d", n)
	}
}

func TestLiveReconcileRestartsOnlyWhenTheAppConnectionFailed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failed      string
		wantRestart bool
	}{
		{"another connection failed", "tasks", false},
		{"the app connection failed", "conversations", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ensureTestAdmin(t, s)
			installID := seedAppWithTools(t, s, "conversations", "proj-1", []string{"send"})
			old := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{{Name: "send", Description: "old", InputSchema: map[string]any{"type": "object"}}}}
			if err := s.registerAppMCPWithSurface(installID, old); err != nil {
				t.Fatal(err)
			}
			agent, err := s.store.CreateAgent(1, "bound", "directive", "autonomous", "{}", "proj-1")
			if err != nil {
				t.Fatal(err)
			}
			bindAgent(t, s, installID, agent.ID)
			tasks := map[string]any{"name": "tasks", "transport": "http", "url": "http://127.0.0.1:5280/api/apps/tasks/mcp?install_id=999999"}
			body, _ := json.Marshal(map[string]any{
				"error":              "apply configuration: MCP replacement failed",
				"failed_mcp_servers": []string{tc.failed},
			})
			core := &fakeAgentCore{
				servers: mcpMapsAsAny([]map[string]any{appBridgeEntry(t, s, installID, "proj-1"), tasks}),
				reject:  http.StatusInternalServerError,
				body:    string(body),
			}
			core.serve(t, s, agent.ID)
			updated := appMCPSurfaceSnapshot{Available: true, Tools: []installMCPToolInfo{{Name: "send", Description: "new", InputSchema: map[string]any{"type": "object"}}}}
			if err := s.registerAppMCPWithSurface(installID, updated); err != nil {
				t.Fatal(err)
			}

			var restarted []int64
			err = s.reconcileBoundRunningAgentsForAppMCPChange(installID, func(ctx context.Context, agentID int64) error {
				return s.reconcileRunningAgentAppMCP(ctx, agentID, installID)
			}, func(_ context.Context, agentID int64) error {
				restarted = append(restarted, agentID)
				return nil
			})
			if core.putCount() != 1 {
				t.Fatalf("live reconcile was not attempted: puts=%d", core.putCount())
			}
			if got := len(restarted) == 1; got != tc.wantRestart {
				t.Fatalf("restarted=%v, want restart=%v (err=%v)", restarted, tc.wantRestart, err)
			}
			if !tc.wantRestart && err == nil {
				t.Fatal("a reconcile blocked by another connection was not reported")
			}
		})
	}
}
