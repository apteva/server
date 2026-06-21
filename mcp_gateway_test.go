package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func newGatewayAgentAPITestServer(s *Server) *httptest.Server {
	apiMux := http.NewServeMux()
	instancesCollectionHandler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/agents", instancesCollectionHandler)
	apiMux.HandleFunc("/agents/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/agents/") {
			r.URL.Path = "/instances/" + strings.TrimPrefix(r.URL.Path, "/agents/")
		}
		path := strings.TrimPrefix(r.URL.Path, "/instances/")
		switch {
		case strings.HasSuffix(path, "/config"):
			s.handleUpdateConfig(w, r)
		case strings.HasSuffix(path, "/start"):
			s.handleStartInstance(w, r)
		case strings.HasSuffix(path, "/stop"):
			s.handleStopInstance(w, r)
		default:
			s.handleInstance(w, r)
		}
	}))
	root := http.NewServeMux()
	root.Handle("/api/", http.StripPrefix("/api", apiMux))
	return httptest.NewServer(root)
}

func TestGatewayAgentCreateToolUsesAgentsAPI(t *testing.T) {
	s := newTestServer(t)
	ts := newGatewayAgentAPITestServer(s)
	defer ts.Close()

	client := gatewayAPIClient{
		baseURL:        ts.URL + "/api",
		userID:         1,
		instanceSecret: "test-secret",
	}
	result, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name":      "CRM Helper",
		"directive": "Help the CRM team triage and update account work.",
		"mode":      "cautious",
		"start":     "false",
	}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agents_create returned error: %v", err)
	}
	row, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", result)
	}
	if row["name"] != "CRM Helper" {
		t.Fatalf("expected created agent name from API response, got %#v", row["name"])
	}
	idFloat, ok := row["id"].(float64)
	if !ok || idFloat <= 0 {
		t.Fatalf("expected numeric created agent id, got %#v", row["id"])
	}

	created, err := s.store.GetAgent(1, int64(idFloat))
	if err != nil {
		t.Fatalf("created agent not persisted by server handler: %v", err)
	}
	if created.Mode != "cautious" {
		t.Fatalf("expected mode cautious, got %q", created.Mode)
	}
	if created.Status != "stopped" {
		t.Fatalf("expected start=false to leave agent stopped, got %q", created.Status)
	}
	if !helperHasRequiredSystemMCPs(created) {
		t.Fatalf("expected created agent to preserve default system MCP flags, config=%s", created.Config)
	}

	listResult, err := handleGatewayAgentTool("agents_list", map[string]any{}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agents_list returned error: %v", err)
	}
	rows, ok := listResult.([]any)
	if !ok {
		t.Fatalf("expected list result array, got %T", listResult)
	}
	found := false
	for _, item := range rows {
		obj, ok := item.(map[string]any)
		if ok && obj["name"] == "CRM Helper" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("agents_list did not include created agent: %#v", listResult)
	}
}

func TestGatewayAgentUpdateAndStopToolsUseAgentsAPI(t *testing.T) {
	s := newTestServer(t)
	ts := newGatewayAgentAPITestServer(s)
	defer ts.Close()

	client := gatewayAPIClient{
		baseURL:        ts.URL + "/api",
		userID:         1,
		instanceSecret: "test-secret",
	}
	created, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name":      "CRM Helper",
		"directive": "Help the CRM team triage and update account work.",
		"start":     "false",
	}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agents_create returned error: %v", err)
	}
	row, ok := created.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", created)
	}
	idFloat, ok := row["id"].(float64)
	if !ok || idFloat <= 0 {
		t.Fatalf("expected numeric created agent id, got %#v", row["id"])
	}
	id := int64(idFloat)

	if _, err := handleGatewayAgentTool("agents_update", map[string]any{
		"id":        id,
		"name":      "Renamed CRM Helper",
		"directive": "Updated directive from MCP.",
		"mode":      "learn",
	}, "", client, s.store, "/tmp/apteva-server"); err != nil {
		t.Fatalf("agents_update returned error: %v", err)
	}
	updated, err := s.store.GetAgent(1, id)
	if err != nil {
		t.Fatalf("updated agent not found: %v", err)
	}
	if updated.Name != "Renamed CRM Helper" {
		t.Fatalf("expected renamed agent, got %q", updated.Name)
	}
	if updated.Directive != "Updated directive from MCP." {
		t.Fatalf("expected directive update, got %q", updated.Directive)
	}
	if updated.Mode != "learn" {
		t.Fatalf("expected mode learn, got %q", updated.Mode)
	}

	if _, err := handleGatewayAgentTool("agents_stop", map[string]any{"id": id}, "", client, s.store, "/tmp/apteva-server"); err != nil {
		t.Fatalf("agents_stop returned error: %v", err)
	}
	stopped, err := s.store.GetAgent(1, id)
	if err != nil {
		t.Fatalf("stopped agent not found: %v", err)
	}
	if stopped.Status != "stopped" || stopped.Port != 0 || stopped.Pid != 0 {
		t.Fatalf("expected stopped agent status/port/pid reset, got status=%q port=%d pid=%d", stopped.Status, stopped.Port, stopped.Pid)
	}

	if _, err := handleGatewayAgentTool("agents_delete", map[string]any{"id": id}, "", client, s.store, "/tmp/apteva-server"); err != nil {
		t.Fatalf("agents_delete returned error: %v", err)
	}
	if _, err := s.store.GetAgent(1, id); err != sql.ErrNoRows {
		t.Fatalf("expected deleted agent to be gone, got err=%v", err)
	}
}

func TestGatewayAgentUpdateCanUpdateMCPServers(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-a', 1, 'Project A', '')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO project_members (project_id, user_id, role, added_by) VALUES ('proj-a', 1, 'owner', 1)`)
	ts := newGatewayAgentAPITestServer(s)
	defer ts.Close()

	client := gatewayAPIClient{
		baseURL:        ts.URL + "/api",
		userID:         1,
		instanceSecret: "test-secret",
	}
	created, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name":       "MCP Worker",
		"directive":  "Use selected tools.",
		"start":      "false",
		"project_id": "proj-a",
	}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agents_create returned error: %v", err)
	}
	row, ok := created.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", created)
	}
	agentID := int64(row["id"].(float64))

	conn, err := s.store.CreateConnection(1, "github", "GitHub", "github-tools", "api_key", "enc", "proj-a")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	mcpID, err := s.store.CreateMCPServerFromConnection(1, conn, 3)
	if err != nil {
		t.Fatalf("CreateMCPServerFromConnection: %v", err)
	}

	result, err := handleGatewayAgentTool("agents_update", map[string]any{
		"id":             agentID,
		"mcp_server_ids": strconv.FormatInt(mcpID, 10),
		"mcp_action":     "set",
	}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("agents_update returned error: %v", err)
	}
	resultObj, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", result)
	}
	if resultObj["mcp_servers"] == nil {
		t.Fatalf("expected agents_update result to include mcp_servers update, got %#v", resultObj)
	}

	var cfg struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := client.do(http.MethodGet, "/agents/"+strconv.FormatInt(agentID, 10)+"/config", nil, &cfg); err != nil {
		t.Fatalf("GET config: %v", err)
	}
	found := false
	for _, entry := range cfg.MCPServers {
		if entry["name"] == "github-tools" {
			found = true
			if entry["transport"] != "http" {
				t.Fatalf("expected http transport, got %#v", entry)
			}
			if url, _ := entry["url"].(string); !strings.Contains(url, "/mcp/"+strconv.FormatInt(mcpID, 10)) {
				t.Fatalf("expected per-row mcp URL, got %#v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("agent config missing attached MCP row: %#v", cfg.MCPServers)
	}
}

func TestGatewayListMCPServersClassifiesAndFiltersKinds(t *testing.T) {
	s := newTestServer(t)

	operatorConn, err := s.store.CreateConnection(1, "github", "GitHub", "github", "api_key", "enc", "proj-a")
	if err != nil {
		t.Fatalf("CreateConnection operator: %v", err)
	}
	operatorMCPID, err := s.store.CreateMCPServerFromConnection(1, operatorConn, 3)
	if err != nil {
		t.Fatalf("CreateMCPServerFromConnection operator: %v", err)
	}
	globalOperatorConn, err := s.store.CreateConnection(1, "github", "GitHub", "github", "api_key", "enc", "")
	if err != nil {
		t.Fatalf("CreateConnection global operator: %v", err)
	}
	globalOperatorMCPID, err := s.store.CreateMCPServerFromConnection(1, globalOperatorConn, 3)
	if err != nil {
		t.Fatalf("CreateMCPServerFromConnection global operator: %v", err)
	}

	appConn, err := s.store.CreateConnection(1, "linear", "Linear", "linear-app-owned", "api_key", "enc", "proj-a")
	if err != nil {
		t.Fatalf("CreateConnection app-owned: %v", err)
	}
	if _, err := s.store.db.Exec(`UPDATE connections SET created_via='app_install', owner_app_install_id=77 WHERE id=?`, appConn.ID); err != nil {
		t.Fatalf("mark app-owned connection: %v", err)
	}
	appOwnedMCPID, err := s.store.CreateMCPServerFromConnection(1, appConn, 5)
	if err != nil {
		t.Fatalf("CreateMCPServerFromConnection app-owned: %v", err)
	}

	directApp, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: 1, Name: "media-studio", Description: "Media Studio app bridge",
		Source: "app", Transport: "http", URL: "http://127.0.0.1:9901/mcp", ProjectID: "proj-a", ToolCount: 4,
	})
	if err != nil {
		t.Fatalf("CreateMCPServerExt app: %v", err)
	}
	remote, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: 1, Name: "composio-gmail", Description: "Hosted Gmail MCP",
		Source: "remote", Transport: "http", URL: "https://mcp.example.test/gmail", ProjectID: "proj-a", ToolCount: 9,
	})
	if err != nil {
		t.Fatalf("CreateMCPServerExt remote: %v", err)
	}
	custom, err := s.store.CreateMCPServer(1, "manual-server", "manual-mcp", `["--stdio"]`, "", "Manual server", "proj-a")
	if err != nil {
		t.Fatalf("CreateMCPServer custom: %v", err)
	}

	all, err := listGatewayMCPServers(s.store, 1, "proj-a", map[string]any{}, "5280", "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected all 5 MCP servers, got %d: %#v", len(all), all)
	}
	byID := map[int64]gatewayMCPServer{}
	for _, row := range all {
		byID[row.ID] = row
	}
	if _, ok := byID[globalOperatorMCPID]; ok {
		t.Fatalf("project-scoped MCP should shadow global MCP with same name, got global row %#v", byID[globalOperatorMCPID])
	}
	if got := byID[operatorMCPID].Kind; got != "integration" {
		t.Fatalf("operator local MCP kind = %q, want integration", got)
	}
	if got := byID[appOwnedMCPID].Kind; got != "app" {
		t.Fatalf("app-owned local MCP kind = %q, want app", got)
	}
	if got := byID[directApp.ID].Kind; got != "app" {
		t.Fatalf("direct app MCP kind = %q, want app", got)
	}
	if got := byID[remote.ID].Kind; got != "remote" {
		t.Fatalf("remote MCP kind = %q, want remote", got)
	}
	if got := byID[custom.ID].Kind; got != "custom" {
		t.Fatalf("custom MCP kind = %q, want custom", got)
	}
	if byID[appOwnedMCPID].CreatedVia != "app_install" || byID[appOwnedMCPID].OwnerAppInstallID != 77 {
		t.Fatalf("expected app-owned metadata, got created_via=%q owner=%d", byID[appOwnedMCPID].CreatedVia, byID[appOwnedMCPID].OwnerAppInstallID)
	}
	if byID[operatorMCPID].MCPURL != "http://127.0.0.1:5280/mcp/"+strconv.FormatInt(operatorMCPID, 10) {
		t.Fatalf("unexpected operator mcp_url: %q", byID[operatorMCPID].MCPURL)
	}
	if got := byID[directApp.ID].MCPURL; !strings.Contains(got, "project_id=proj-a") {
		t.Fatalf("expected direct app mcp_url to include project id, got %q", got)
	}

	appOnly, err := listGatewayMCPServers(s.store, 1, "proj-a", map[string]any{"kind": "app"}, "5280", "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("list app only: %v", err)
	}
	if len(appOnly) != 2 {
		t.Fatalf("expected 2 app MCP servers, got %d: %#v", len(appOnly), appOnly)
	}
	for _, row := range appOnly {
		if row.Kind != "app" {
			t.Fatalf("kind=app returned non-app row: %#v", row)
		}
	}

	integrationOnly, err := listGatewayMCPServers(s.store, 1, "proj-a", map[string]any{"kind": "integration"}, "5280", "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("list integration only: %v", err)
	}
	if len(integrationOnly) != 1 || integrationOnly[0].ID != operatorMCPID {
		t.Fatalf("expected only operator integration MCP, got %#v", integrationOnly)
	}

	noApps, err := listGatewayMCPServers(s.store, 1, "proj-a", map[string]any{"include_app_owned": "false"}, "5280", "/tmp/apteva-server")
	if err != nil {
		t.Fatalf("list without app-owned: %v", err)
	}
	for _, row := range noApps {
		if row.Kind == "app" {
			t.Fatalf("include_app_owned=false returned app row: %#v", row)
		}
	}

	if _, err := listGatewayMCPServers(s.store, 1, "proj-a", map[string]any{"kind": "bogus"}, "5280", "/tmp/apteva-server"); err == nil {
		t.Fatalf("expected invalid kind to return an error")
	}
}
