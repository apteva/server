package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestUpdateConfigPreservesRunningMCPServersOnDirectivePatch(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "preserve-tools", "old", "autonomous", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	current := map[string]any{
		"directive": "old",
		"mode":      "autonomous",
		"mcp_servers": []any{
			map[string]any{"name": "channels", "transport": "http", "url": "http://127.0.0.1/channels"},
			map[string]any{"name": "crm", "transport": "http", "url": "http://127.0.0.1/crm"},
		},
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPut:
			var update map[string]any
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for key, value := range update {
				current[key] = value
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer core.Close()
	parsed, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}

	req := authedRequest(t, http.MethodPut, "/instances/"+itoa64(agent.ID)+"/config", "", map[string]any{
		"directive": "new directive",
	})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("directive patch status=%d body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	servers := mcpMaps(current["mcp_servers"])
	mu.Unlock()
	names := map[string]bool{}
	for _, server := range servers {
		name, _ := server["name"].(string)
		names[name] = true
	}
	if !names["channels"] || !names["crm"] || len(names) != 2 {
		t.Fatalf("directive patch changed running MCP set: %#v", servers)
	}
}

func TestAgentMCPMutationIsAdditiveAndSynchronizesAppBinding(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO projects(id,user_id,name,description) VALUES('proj-1',1,'Project','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members(project_id,user_id,role,added_by) VALUES('proj-1',1,'owner',1)`); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "app-tools", "directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = []any{
			map[string]any{"name": "existing", "transport": "http", "url": "http://127.0.0.1/existing"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	installID := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	row := readMCPRow(t, s, installID)
	serverID := row["id"].(int64)

	mutate := func(action string) {
		t.Helper()
		req := authedRequest(t, http.MethodPost, "/instances/"+itoa64(agent.ID)+"/mcp-servers", "", map[string]any{
			"action": action, "mcp_server_ids": []int64{serverID},
		})
		rec := httptest.NewRecorder()
		s.handleAgentMCPServers(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s MCP status=%d body=%s", action, rec.Code, rec.Body.String())
		}
	}

	mutate("add")
	servers, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(servers, "existing") || !hasMCPName(servers, "crm") {
		t.Fatalf("add replaced an existing attachment: %#v", servers)
	}
	var bound int
	if err := s.store.db.QueryRow(`
		SELECT COUNT(*) FROM app_agent_bindings
		WHERE install_id=? AND agent_id=? AND enabled=1`, installID, agent.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("app binding count=%d, want 1 after attach", bound)
	}

	mutate("remove")
	servers, err = s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(servers, "existing") || hasMCPName(servers, "crm") {
		t.Fatalf("remove changed the wrong attachments: %#v", servers)
	}
	if err := s.store.db.QueryRow(`
		SELECT COUNT(*) FROM app_agent_bindings
		WHERE install_id=? AND agent_id=? AND enabled=1`, installID, agent.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 0 {
		t.Fatalf("app binding count=%d, want 0 after detach", bound)
	}
}

func TestAgentMCPMutationMatchesAppByStableInstallID(t *testing.T) {
	current := []map[string]any{
		{"name": "existing", "transport": "http", "url": "http://127.0.0.1/mcp/12"},
		{
			"name":      "legacy-crm-name",
			"transport": "http",
			"url":       "http://127.0.0.1:5280/api/apps/legacy-crm/mcp?api_key=stale&install_id=18",
		},
	}
	selected := []map[string]any{
		{
			"name":      "crm",
			"transport": "http",
			"url":       "http://127.0.0.1:5280/api/apps/crm/mcp?api_key=current&install_id=18&project_id=proj-1",
		},
	}

	added := mutateMCPServers(current, selected, "add")
	if len(added) != 2 || !hasMCPName(added, "existing") || !hasMCPName(added, "crm") ||
		hasMCPName(added, "legacy-crm-name") {
		t.Fatalf("add did not replace stale identity: %#v", added)
	}
	removed := mutateMCPServers(added, selected, "remove")
	if len(removed) != 1 || !hasMCPName(removed, "existing") || hasMCPName(removed, "crm") {
		t.Fatalf("remove did not match stable app identity: %#v", removed)
	}
}

func TestRefreshAgentAppMCPConfigsRepairsStaleURLAndBinding(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO projects(id,user_id,name,description) VALUES('proj-1',1,'Project','')`,
	); err != nil {
		t.Fatal(err)
	}
	installID := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	row := readMCPRow(t, s, installID)
	currentURL := row["url"].(string)
	parsed, err := url.Parse(currentURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("api_key", "stale-token")
	parsed.RawQuery = query.Encode()

	agent, err := s.store.CreateAgent(1, "stale-app-agent", "directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = []any{
			map[string]any{
				"name":      "old-crm-name",
				"transport": "http",
				"url":       parsed.String(),
			},
			map[string]any{
				"name":      "unrelated",
				"transport": "http",
				"url":       "https://mcp.example.test",
			},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.refreshAgentAppMCPConfigs(agent); err != nil {
		t.Fatal(err)
	}
	servers, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(servers, "crm") || !hasMCPName(servers, "unrelated") ||
		hasMCPName(servers, "old-crm-name") {
		t.Fatalf("refresh produced the wrong MCP set: %#v", servers)
	}
	var refreshedURL string
	for _, server := range servers {
		if server["name"] == "crm" {
			refreshedURL, _ = server["url"].(string)
		}
	}
	refreshedParsed, err := url.Parse(refreshedURL)
	if err != nil {
		t.Fatal(err)
	}
	if refreshedParsed.Query().Get("api_key") == "stale-token" ||
		refreshedParsed.Query().Get("install_id") != itoa64(installID) ||
		refreshedParsed.Query().Get("project_id") != "proj-1" {
		t.Fatalf("app URL was not refreshed from inventory: %s", refreshedURL)
	}
	var bound int
	if err := s.store.db.QueryRow(
		`SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=? AND install_id=? AND enabled=1`,
		agent.ID, installID,
	).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("refreshed config did not repair binding metadata: %d", bound)
	}
}

func TestConcurrentAgentMCPAddsDoNotOverwriteEachOther(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "parallel-tools", "directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.store.CreateMCPServer(1, "first", "first-mcp", `[]`, "", "First", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.store.CreateMCPServer(1, "second", "second-mcp", `[]`, "", "Second", "proj-1")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan string, 2)
	for _, serverID := range []int64{first.ID, second.ID} {
		go func(id int64) {
			<-start
			req := authedRequest(t, http.MethodPost, "/instances/"+itoa64(agent.ID)+"/mcp-servers", "", map[string]any{
				"action": "add", "mcp_server_ids": []int64{id},
			})
			rec := httptest.NewRecorder()
			s.handleAgentMCPServers(rec, req)
			if rec.Code != http.StatusOK {
				errs <- rec.Body.String()
				return
			}
			errs <- ""
		}(serverID)
	}
	close(start)
	for range 2 {
		if message := <-errs; message != "" {
			t.Fatalf("concurrent add failed: %s", message)
		}
	}
	servers, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(servers, "first") || !hasMCPName(servers, "second") {
		t.Fatalf("concurrent adds overwrote one another: %#v", servers)
	}
}

func TestAgentMCPMutationRejectsAnotherProject(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "project-a-agent", "directive", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateMCPServer(1, "other", "other-mcp", `[]`, "", "Other", "project-b")
	if err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, http.MethodPost, "/instances/"+itoa64(agent.ID)+"/mcp-servers", "", map[string]any{
		"action": "add", "mcp_server_ids": []int64{other.ID},
	})
	rec := httptest.NewRecorder()
	s.handleAgentMCPServers(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "belongs to project") {
		t.Fatalf("cross-project attach status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStartupReconciliationRepairsHistoricalAppBindingDrift(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	row := readMCPRow(t, s, installID)
	agentConfig, _ := json.Marshal(map[string]any{
		"mcp_servers": []any{
			map[string]any{"name": "crm", "transport": "http", "url": row["url"]},
		},
	})
	agent, err := s.store.CreateAgent(1, "historical-drift", "directive", "autonomous", string(agentConfig), "proj-1")
	if err != nil {
		t.Fatal(err)
	}

	s.reconcileAllAgentAppBindings()
	var bound int
	if err := s.store.db.QueryRow(`
		SELECT COUNT(*) FROM app_agent_bindings
		WHERE install_id=? AND agent_id=? AND enabled=1`, installID, agent.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("startup reconciliation did not add missing binding: count=%d", bound)
	}

	agent.Config = `{}`
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	s.reconcileAllAgentAppBindings()
	if err := s.store.db.QueryRow(`
		SELECT COUNT(*) FROM app_agent_bindings
		WHERE install_id=? AND agent_id=? AND enabled=1`, installID, agent.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 0 {
		t.Fatalf("startup reconciliation retained stale binding: count=%d", bound)
	}
}

func TestCreateAgentWithAppSelectionAttachesItsMCPConfig(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO projects(id,user_id,name,description) VALUES('proj-1',1,'Project','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members(project_id,user_id,role,added_by) VALUES('proj-1',1,'owner',1)`); err != nil {
		t.Fatal(err)
	}
	installID := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPost, "/instances", "", map[string]any{
		"name":                  "CRM agent",
		"directive":             "Use CRM.",
		"project_id":            "proj-1",
		"start":                 false,
		"bound_app_install_ids": []int64{installID},
		"bound_connection_ids":  []int64{},
	})
	rec := httptest.NewRecorder()
	s.handleCreateInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.store.GetAgentByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(fresh.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(mcpMaps(cfg["mcp_servers"]), "crm") {
		t.Fatalf("created agent config missing selected app MCP: %s", fresh.Config)
	}
}

func TestLegacyReplaceAllBindingEndpointRejectsMCPApps(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "crm", "proj-1", []string{"contacts_get"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, http.MethodPut, "/apps/installs/"+itoa64(installID)+"/instances", "", map[string]any{
		"instance_ids": []int64{1},
	})
	rec := httptest.NewRecorder()
	s.handleSetInstallBindings(rec, req)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "/api/agents/:id/mcp-servers") {
		t.Fatalf("legacy endpoint status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func hasMCPName(servers []map[string]any, name string) bool {
	for _, server := range servers {
		if server["name"] == name {
			return true
		}
	}
	return false
}
