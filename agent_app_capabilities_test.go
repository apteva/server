package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type appCapabilityFixture struct {
	s            *Server
	agent        *Agent
	client       gatewayAPIClient
	install, mcp int64
}

func newAppCapabilityFixture(t *testing.T) appCapabilityFixture {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for _, project := range []string{"cap-project", "other-project"} {
		if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name,description) VALUES(?,1,?,'')`, project, project); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := s.store.CreateAgent(1, "Stopped executor", "original directive", "cautious", `{}`, "cap-project")
	if err != nil {
		t.Fatal(err)
	}
	ts := newGatewayAgentAPITestServer(s)
	t.Cleanup(ts.Close)
	parsed, _ := url.Parse(ts.URL)
	s.port = parsed.Port()
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = []any{map[string]any{"name": "existing", "url": "https://existing.test/mcp?secret=never-expose", "headers": map[string]any{"Authorization": "secret-header"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Keep the app installation ID different from its MCP registry ID.
	if _, err := s.store.CreateMCPServer(1, "unused-registry-row", "echo", "[]", "", "", "cap-project"); err != nil {
		t.Fatal(err)
	}
	install := seedAppWithTools(t, s, "processes", "cap-project", []string{"processes_get"})
	if err := s.registerAppMCP(install); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO skills(slug,name,description,body,source,install_id,project_id,enabled) VALUES('processes:coordination','coordination','Coordinate procedures','Use processes tools.','app',?,'cap-project',1)`, install); err != nil {
		t.Fatal(err)
	}
	mcp := readMCPRow(t, s, install)["id"].(int64)
	return appCapabilityFixture{s, agent, gatewayAPIClient{baseURL: ts.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}, install, mcp}
}

func (f appCapabilityFixture) update(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	args["id"] = f.agent.ID
	out, err := handleGatewayAgentTool("agents_update", args, f.agent.ProjectID, f.client, f.s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func (f appCapabilityFixture) assertState(t *testing.T, attached bool) {
	t.Helper()
	saved, err := f.s.store.GetAgentByID(f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "stopped" || f.s.agents.GetPort(saved.ID) != 0 || saved.Mode != "cautious" {
		t.Fatalf("attachment changed agent lifecycle: %#v", saved)
	}
	cfg, err := f.s.currentAgentMCPServers(saved, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasMCPName(cfg, "processes") != attached {
		t.Fatalf("actual attachment mismatch: %#v", cfg)
	}
	var count int
	if err := f.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, f.install, saved.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if (count == 1) != attached {
		t.Fatalf("binding mismatch: %d", count)
	}
	skills, err := journalActiveSkillRecords(filepath.Join(f.s.agents.instanceDir(saved.ID), "memory.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, hasSkill := skills["processes:coordination"]
	if hasSkill != attached {
		t.Fatalf("skill mismatch: %#v", skills)
	}
}

func TestGatewayAppCapabilityAddRemoveSetAndVerification(t *testing.T) {
	f := newAppCapabilityFixture(t)
	for i := 0; i < 2; i++ {
		result := f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install)}})
		capabilities := result["capabilities"].(map[string]any)
		apps := capabilities["apps"].([]map[string]any)
		if len(apps) != 1 || apps[0]["install_id"] != f.install || apps[0]["mcp_server_id"] != f.mcp || apps[0]["name"] != "processes" {
			t.Fatalf("receipt does not verify real app: %#v", capabilities)
		}
		if capabilities["count"] != 2 {
			t.Fatalf("add replaced unrelated tools or duplicated an attachment: %#v", capabilities)
		}
		raw, _ := json.Marshal(result)
		for _, secret := range []string{"never-expose", "secret-header", "api_key", "mcp_token", "http://", "https://"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("receipt leaked %s: %s", secret, raw)
			}
		}
		f.assertState(t, true)
	}
	// Legacy selections saved in generic config cannot fabricate actual attachments.
	saved, _ := f.s.store.GetAgentByID(f.agent.ID)
	saved.Config = `{"bound_app_install_ids":[999999]}`
	if err := f.s.store.UpdateAgent(saved); err != nil {
		t.Fatal(err)
	}
	get, err := handleGatewayAgentTool("agents_get", map[string]any{"id": f.agent.ID}, f.agent.ProjectID, f.client, f.s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	apps := get.(map[string]any)["capabilities"].(map[string]any)["apps"].([]map[string]any)
	if len(apps) != 1 || apps[0]["install_id"] != f.install {
		t.Fatalf("agents_get trusted stale DB config: %#v", get)
	}
	f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install)}, "mcp_action": "remove"})
	f.assertState(t, false)
	cfg, _ := f.s.currentAgentMCPServers(f.agent, 0)
	if !hasMCPName(cfg, "existing") {
		t.Fatal("remove deleted unrelated tools")
	}
	f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install)}, "mcp_action": "set"})
	cfg, _ = f.s.currentAgentMCPServers(f.agent, 0)
	if hasMCPName(cfg, "existing") {
		t.Fatal("explicit set did not replace non-system capabilities")
	}
	f.assertState(t, true)
}

func TestGatewayRejectsMisplacedAttachmentSettingsBeforeMutations(t *testing.T) {
	f := newAppCapabilityFixture(t)
	for _, key := range []string{"bound_app_install_ids", "bound_connection_ids", "mcp_server_ids", "mcp_action", "mcp_servers"} {
		for _, asString := range []bool{false, true} {
			var config any = map[string]any{key: []any{float64(f.install)}}
			if asString {
				raw, _ := json.Marshal(config)
				config = string(raw)
			}
			_, err := handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "name": "Must not change", "directive": "Must not change", "config": config}, f.agent.ProjectID, f.client, f.s.store, "")
			if err == nil || !strings.Contains(err.Error(), "config."+key) {
				t.Fatalf("misplaced %s accepted or unclear error: %v", key, err)
			}
			saved, _ := f.s.store.GetAgentByID(f.agent.ID)
			if saved.Name != f.agent.Name || saved.Directive != f.agent.Directive || saved.Config != f.agent.Config {
				t.Fatalf("invalid call caused partial metadata change: %#v", saved)
			}
		}
	}
	req := authedRequest(t, "PUT", "/instances/"+itoa64(f.agent.ID)+"/config", "", map[string]any{"directive": "Do not change", "config": `{"bound_app_install_ids":[22,91]}`})
	rec := httptest.NewRecorder()
	f.s.handleUpdateConfig(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "config.bound_app_install_ids") {
		t.Fatalf("HTTP config accepted misplaced attachment: %d %s", rec.Code, rec.Body.String())
	}
	f.assertState(t, false)
}

func TestAppCapabilityInvalidSelectionIsAtomic(t *testing.T) {
	f := newAppCapabilityFixture(t)
	other := seedAppWithTools(t, f.s, "other-app", "other-project", []string{"other_get"})
	if err := f.s.registerAppMCP(other); err != nil {
		t.Fatal(err)
	}
	noTools := seedAppWithTools(t, f.s, "ui-only", "cap-project", nil)
	for _, ids := range [][]any{{float64(f.install), float64(other)}, {float64(f.install), float64(999999)}, {float64(f.install), float64(noTools)}, {float64(f.install), 1.5}, {float64(f.install), -1.0}} {
		_, err := handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "bound_app_install_ids": ids}, f.agent.ProjectID, f.client, f.s.store, "")
		if err == nil {
			t.Fatalf("invalid selection accepted: %v", ids)
		}
		f.assertState(t, false)
		cfg, _ := f.s.currentAgentMCPServers(f.agent, 0)
		if len(cfg) != 1 || !hasMCPName(cfg, "existing") {
			t.Fatalf("failed attachment changed current config: %#v", cfg)
		}
	}
	// Both selector namespaces participate in one atomic mutation.
	_, err := handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "bound_app_install_ids": []any{float64(f.install)}, "mcp_server_ids": []any{float64(999999)}, "mcp_action": "add"}, f.agent.ProjectID, f.client, f.s.store, "")
	if err == nil {
		t.Fatal("combined invalid selection succeeded")
	}
	f.assertState(t, false)
	f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install)}, "mcp_server_ids": []any{float64(f.mcp)}, "mcp_action": "add"})
	f.assertState(t, true)
	cfg, _ := f.s.currentAgentMCPServers(f.agent, 0)
	if len(cfg) != 2 {
		t.Fatalf("combined selection duplicated app: %#v", cfg)
	}
}

func TestGatewayAgentCapabilitiesReadLiveCoreAndRedact(t *testing.T) {
	f := newAppCapabilityFixture(t)
	record, _ := f.s.store.GetMCPServerByIDUnscoped(f.mcp)
	actual, err := gatewayMCPConfigFromRecord(*record, f.agent.ProjectID, f.s.port, "")
	if err != nil {
		t.Fatal(err)
	}
	actual["headers"] = map[string]any{"Authorization": "live-secret"}
	var ignoreMutation atomic.Bool
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer core-key" {
			t.Error("missing Core authentication")
		}
		if ignoreMutation.Load() {
			if r.URL.Path == "/threads" {
				writeJSON(w, []any{})
				return
			}
			writeJSON(w, map[string]any{"mcp_servers": []any{}})
			return
		}
		writeJSON(w, map[string]any{"mcp_servers": []any{actual}, "providers": []any{map[string]any{"api_key": "provider-secret"}}})
	}))
	defer core.Close()
	u, _ := url.Parse(core.URL)
	_, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	f.s.agents.processes[f.agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	capabilities, err := gatewayAgentCapabilities(f.agent.ID, f.client, f.s.store)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities["count"] != 1 || len(capabilities["apps"].([]map[string]any)) != 1 {
		t.Fatalf("live snapshot read disk instead: %#v", capabilities)
	}
	raw, _ := json.Marshal(capabilities)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "headers") || strings.Contains(string(raw), "url") {
		t.Fatalf("live snapshot leaked runtime config: %s", raw)
	}
	// A Core that acknowledges the request but does not change config must
	// never produce a successful attachment claim from the management tool.
	if _, err := f.s.store.db.Exec(`UPDATE skills SET enabled=0 WHERE install_id=?`, f.install); err != nil {
		t.Fatal(err)
	}
	ignoreMutation.Store(true)
	_, err = handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "bound_app_install_ids": []any{float64(f.install)}}, f.agent.ProjectID, f.client, f.s.store, "")
	if err == nil || !strings.Contains(err.Error(), "does not verify app installation") {
		t.Fatalf("unverified attachment reported as success: %v", err)
	}
}

func TestAppCapabilitySelectionHonorsAgentAccess(t *testing.T) {
	f := newAppCapabilityFixture(t)
	user, err := f.s.store.CreateUser("viewer@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.db.Exec(`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES(?,?,'viewer',1)`, f.agent.ProjectID, user.ID); err != nil {
		t.Fatal(err)
	}
	client := f.client
	client.userID = user.ID
	_, err = handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "bound_app_install_ids": []any{float64(f.install)}}, f.agent.ProjectID, client, f.s.store, "")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("viewer attached capability: %v", err)
	}
	f.assertState(t, false)
	if _, err := f.s.store.db.Exec(`UPDATE project_members SET role='editor' WHERE project_id=? AND user_id=?`, f.agent.ProjectID, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := handleGatewayAgentTool("agents_update", map[string]any{"id": f.agent.ID, "bound_app_install_ids": []any{float64(f.install)}}, f.agent.ProjectID, client, f.s.store, ""); err != nil {
		t.Fatalf("project editor cannot attach platform-owned app: %v", err)
	}
	f.assertState(t, true)
}

func TestAppCapabilityGlobalAndExplicitSetPreservesSystem(t *testing.T) {
	f := newAppCapabilityFixture(t)
	global := seedAppWithTools(t, f.s, "global-tools", "", []string{"global_get"})
	if err := f.s.registerAppMCP(global); err != nil {
		t.Fatal(err)
	}
	if err := f.s.writeStoppedConfigAtomic(f.agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = append(mcpMapsAsAny(mcpMaps(cfg["mcp_servers"])), map[string]any{"name": "channels", "url": "http://127.0.0.1/channels"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install), float64(global)}, "mcp_action": "set"})
	cfg, _ := f.s.currentAgentMCPServers(f.agent, 0)
	if len(cfg) != 3 || !hasMCPName(cfg, "channels") || !hasMCPName(cfg, "global-tools") || hasMCPName(cfg, "existing") {
		t.Fatalf("set lost system or global attachment: %#v", cfg)
	}
	f.assertState(t, true)
	f.update(t, map[string]any{"bound_app_install_ids": []any{}, "mcp_action": "set"})
	cfg, _ = f.s.currentAgentMCPServers(f.agent, 0)
	if len(cfg) != 1 || !hasMCPName(cfg, "channels") {
		t.Fatalf("empty set lost system attachment: %#v", cfg)
	}
	f.assertState(t, false)
}

func TestAppCapabilityUpdatesRunningCoreThroughSharedPath(t *testing.T) {
	f := newAppCapabilityFixture(t)
	// Keep this hot-config fixture focused on tool reconciliation, without
	// invoking the separate memory HTTP API for the app's skill.
	if _, err := f.s.store.db.Exec(`UPDATE skills SET enabled=0 WHERE install_id=?`, f.install); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := map[string]any{"mcp_servers": []any{map[string]any{"name": "live-existing", "url": "https://live-existing.test/mcp"}}}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer core-key" {
			t.Error("missing Core authentication")
		}
		if r.Method == "GET" && r.URL.Path == "/threads" {
			writeJSON(w, []any{})
			return
		}
		if r.Method == "PUT" && r.URL.Path == "/config" {
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
				return
			}
			for key, value := range patch {
				current[key] = value
			}
			writeJSON(w, map[string]any{"status": "updated"})
			return
		}
		writeJSON(w, current)
	}))
	defer core.Close()
	u, _ := url.Parse(core.URL)
	port, _ := strconv.Atoi(u.Port())
	f.s.agents.processes[f.agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	result := f.update(t, map[string]any{"bound_app_install_ids": []any{float64(f.install)}})
	if result["capabilities"].(map[string]any)["count"] != 2 {
		t.Fatalf("receipt not based on live Core: %#v", result)
	}
	mu.Lock()
	servers := mcpMaps(current["mcp_servers"])
	mu.Unlock()
	if len(servers) != 2 || !hasMCPName(servers, "live-existing") || !hasMCPName(servers, "processes") {
		t.Fatalf("hot attachment overwrote live MCP set: %#v", servers)
	}
	var bound int
	if err := f.s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, f.install, f.agent.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatal("hot attachment did not synchronize app binding")
	}
}
