package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAptevaServerMCP_RealLLM_Codex_BuildsBusiness exercises the actual
// apteva-server stdio MCP gateway through a real core and Codex provider. The
// marketplace is local, but every management call follows the same gateway ->
// authenticated server API -> store path used in production.
func TestAptevaServerMCP_RealLLM_Codex_BuildsBusiness(t *testing.T) {
	registry := newBusinessSetupMarketplace(t)
	t.Setenv("APTEVA_APP_REGISTRY_URL", registry.URL+"/registry.json")

	providerState := loadOpenAICodexProviderState(t)
	var projectID string
	directive := strings.Join([]string{
		"# Role",
		"You are an Apteva business setup operator. Build small teams with installed apps and well-scoped specialist agents.",
		"# Goals",
		"Turn an operator's business brief into a verified project configuration.",
		"# Methods",
		"Use platform state and tool results as evidence. Keep reusable setup methods here.",
		"# Safety",
		"Keep newly created specialist agents stopped until their configuration is complete.",
	}, "\n")
	h := setupRealChannelChatHarnessWithProviderPrepared(t,
		"Apteva Business Builder", directive,
		`{"include_apteva_server":true,"include_channels":true}`,
		15, "llm", "OpenAI Codex", providerState,
		func(s *Server, userID int64, agent *Agent) {
			if err := s.store.SetPlatformRole(userID, PlatformAdmin); err != nil {
				t.Fatalf("make test operator admin: %v", err)
			}
			project, err := s.store.CreateProject(userID, "Northstar Studio", "Real MCP business setup fixture", "#ff6b00")
			if err != nil {
				t.Fatalf("create business project: %v", err)
			}
			projectID = project.ID
			agent.ProjectID = project.ID
		},
	)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	var baselineMessageID int64
	if err := h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID); err != nil {
		t.Fatalf("baseline chat id: %v", err)
	}

	h.post(t, strings.Join([]string{
		"Set up the Northstar Studio business completely in the current Apteva project.",
		"Use the real apteva-server management tools and finish the platform changes, not just a plan.",
		"1. Call apps_marketplace and inspect the available fixtures.",
		"2. Install exactly northstar-crm and northstar-campaigns in the current project using their manifest_url values.",
		"3. Create exactly two specialist agents, both stopped while being configured:",
		"   - Northstar Sales, bound only to the northstar-crm install.",
		"   - Northstar Growth, bound only to the northstar-campaigns install.",
		"4. Give both agents structured Markdown directives with Role, Goals, Operating Rules, Tools and Integrations, and Learning sections.",
		"5. After creation, call agents_update on each agent. Add a reusable Learning rule: Sales must qualify leads before handoff; Growth must coordinate campaigns with CRM segments.",
		"6. Verify the installed apps with apps_list and the configured team with agents_list.",
		"A lasting policy for every future business setup is: discover and install required apps before creating specialist agents. Retain that as a reusable method, not as Northstar run history.",
		"Only after all state is verified, send one final chat message containing BUSINESS SETUP COMPLETE and a concise summary.",
	}, "\n"))

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if businessSetupFinalMessageExists(t, h, baselineMessageID) {
			break
		}
		time.Sleep(time.Second)
	}
	if !businessSetupFinalMessageExists(t, h, baselineMessageID) {
		calls := newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)
		t.Fatalf("business setup did not finish; calls=%v", toolNames(calls))
	}
	// Allow the final channels_send wake and any immediately dependent evolve
	// result to reach telemetry/DB before asserting the complete state.
	time.Sleep(3 * time.Second)

	assertBusinessSetupToolCoverage(t, h, baselineCalls)
	assertBusinessSetupState(t, h, projectID)
}

func newBusinessSetupMarketplace(t *testing.T) *httptest.Server {
	t.Helper()
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/registry.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema": "apteva-app-registry/v1",
				"apps": []map[string]any{
					{
						"name": "northstar-crm", "display_name": "Northstar CRM", "version": "1.0.0",
						"description": "Customer records and sales pipeline for a small business.",
						"author":      "Apteva Test", "manifest_url": baseURL + "/northstar-crm.yaml", "official": true,
					},
					{
						"name": "northstar-campaigns", "display_name": "Northstar Campaigns", "version": "1.0.0",
						"description": "Campaign planning and audience execution for a growth team.",
						"author":      "Apteva Test", "manifest_url": baseURL + "/northstar-campaigns.yaml", "official": true,
					},
				},
			})
		case "/northstar-crm.yaml":
			w.Header().Set("Content-Type", "application/yaml")
			fmt.Fprint(w, businessAppManifest("northstar-crm", "Northstar CRM", "Customer records and sales pipeline.", "contacts_list"))
		case "/northstar-campaigns.yaml":
			w.Header().Set("Content-Type", "application/yaml")
			fmt.Fprint(w, businessAppManifest("northstar-campaigns", "Northstar Campaigns", "Campaign planning and audience execution.", "campaigns_list"))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)
	return server
}

func businessAppManifest(name, displayName, description, toolName string) string {
	return fmt.Sprintf(`schema: apteva-app/v1
name: %s
display_name: %s
version: 1.0.0
description: %s
scopes: [project]
requires:
  permissions: []
provides:
  mcp_tools:
    - name: %s
      description: List fixture records.
`, name, displayName, description, toolName)
}

func businessSetupFinalMessageExists(t *testing.T, h *realChannelChatHarness, afterID int64) bool {
	t.Helper()
	var count int
	if err := h.server.store.db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>? AND content LIKE '%BUSINESS SETUP COMPLETE%'`,
		h.chatID, afterID).Scan(&count); err != nil {
		t.Fatalf("query final business message: %v", err)
	}
	return count > 0
}

func assertBusinessSetupToolCoverage(t *testing.T, h *realChannelChatHarness, baseline map[string]bool) {
	t.Helper()
	calls := newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baseline)
	names := toolNames(calls)
	t.Logf("real apteva-server MCP tool sequence: %v", names)
	for _, required := range []string{
		"apps_marketplace", "apps_install", "agents_create", "agents_update",
		"apps_list", "agents_list", "evolve", "channels_send",
	} {
		if !containsTool(names, required) {
			t.Errorf("missing %s tool call; calls=%v", required, names)
		}
	}
	if countTools(names, "apps_install") < 2 {
		t.Errorf("apps_install calls=%d, want at least 2; calls=%v", countTools(names, "apps_install"), names)
	}
	if countTools(names, "agents_create") != 2 {
		t.Errorf("agents_create calls=%d, want exactly 2; calls=%v", countTools(names, "agents_create"), names)
	}
	if countTools(names, "agents_update") < 2 {
		t.Errorf("agents_update calls=%d, want at least 2; calls=%v", countTools(names, "agents_update"), names)
	}
	if countTools(names, "evolve") != 1 {
		t.Errorf("evolve calls=%d, want exactly 1; calls=%v", countTools(names, "evolve"), names)
	}
}

func countTools(names []string, suffix string) int {
	count := 0
	for _, name := range names {
		if name == suffix || strings.HasSuffix(name, "_"+suffix) {
			count++
		}
	}
	return count
}

func assertBusinessSetupState(t *testing.T, h *realChannelChatHarness, projectID string) {
	t.Helper()
	installs := map[string]int64{}
	rows, err := h.server.store.db.Query(`
		SELECT a.name, i.id FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE i.project_id=? AND a.name IN ('northstar-crm','northstar-campaigns')`, projectID)
	if err != nil {
		t.Fatalf("query business installs: %v", err)
	}
	for rows.Next() {
		var name string
		var id int64
		if rows.Scan(&name, &id) == nil {
			installs[name] = id
		}
	}
	rows.Close()
	if len(installs) != 2 {
		t.Fatalf("installed business apps=%v, want both fixtures", installs)
	}
	t.Logf("installed business apps: %v", installs)

	agents, err := h.server.store.ListAgentsInProject(projectID)
	if err != nil {
		t.Fatalf("list business agents: %v", err)
	}
	workers := map[string]Agent{}
	for _, agent := range agents {
		if agent.ID != h.agent.ID {
			workers[agent.Name] = agent
		}
	}
	if len(workers) != 2 {
		t.Fatalf("worker agents=%v, want exactly Northstar Sales and Northstar Growth", mapKeys(workers))
	}
	t.Logf("configured worker agents: %v", mapKeys(workers))
	checks := []struct {
		name       string
		app        string
		learning   []string
		directives []string
	}{
		{"Northstar Sales", "northstar-crm", []string{"qualif", "lead", "handoff"}, []string{"# Role", "# Goals", "# Operating Rules", "# Tools and Integrations", "# Learning"}},
		{"Northstar Growth", "northstar-campaigns", []string{"campaign", "CRM", "segment"}, []string{"# Role", "# Goals", "# Operating Rules", "# Tools and Integrations", "# Learning"}},
	}
	for _, check := range checks {
		agent, ok := workers[check.name]
		if !ok {
			t.Errorf("missing worker %q; workers=%v", check.name, mapKeys(workers))
			continue
		}
		if agent.Status != "stopped" {
			t.Errorf("%s status=%q, want stopped", check.name, agent.Status)
		}
		for _, want := range append(check.directives, check.learning...) {
			if !strings.Contains(strings.ToLower(agent.Directive), strings.ToLower(want)) {
				t.Errorf("%s directive missing %q:\n%s", check.name, want, agent.Directive)
			}
		}
		var bound int
		if err := h.server.store.db.QueryRow(`
			SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=? AND install_id=? AND enabled=1`,
			agent.ID, installs[check.app]).Scan(&bound); err != nil {
			t.Fatalf("query %s app binding: %v", check.name, err)
		}
		if bound != 1 {
			t.Errorf("%s is not bound to %s install %d", check.name, check.app, installs[check.app])
		}
	}

	orchestrator, err := h.server.store.GetAgentByID(h.agent.ID)
	if err != nil {
		t.Fatalf("reload orchestrator: %v", err)
	}
	lower := strings.ToLower(orchestrator.Directive)
	for _, want := range []string{"discover", "install", "apps", "before", "specialist agents"} {
		if !strings.Contains(lower, want) {
			t.Errorf("evolved orchestrator directive missing %q:\n%s", want, orchestrator.Directive)
		}
	}
	if strings.Contains(lower, "northstar") {
		t.Errorf("evolved directive stored one-off Northstar run history:\n%s", orchestrator.Directive)
	}
	t.Logf("evolved business setup directive:\n%s", orchestrator.Directive)
}
