package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apteva/server/internal/managedmcp"
)

func newManagedMCPTestServer(t *testing.T) (*Server, *User, *Project) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "apteva.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	user, err := store.CreateUser("managed-mcp@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(user.ID, "Managed MCP", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:         store,
		agents:        NewAgentManager(dataDir, "echo"),
		mcpManager:    NewMCPManager(),
		catalog:       NewAppCatalog(),
		secret:        bytes.Repeat([]byte{0x42}, 32),
		port:          "0",
		dataDir:       dataDir,
		installedApps: NewInstalledAppsRegistry(),
	}
	return s, user, project
}

func buildManagedMCPRunner(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apteva-mcp-runner")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/apteva-mcp-runner")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, output)
	}
	return path
}

func managedDefinition() managedmcp.Definition {
	return managedmcp.Definition{
		Version: managedmcp.DefinitionVersion,
		Tools: []managedmcp.Tool{
			{
				Name:        "echo",
				Description: "Echo the supplied message.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{"type": "string"},
					},
				},
				Handler: "tools/echo.js",
				Code:    `return {message: input.message, source: "managed", configured: apteva.env("CUSTOM_VALUE") || "", server_secret: apteva.env("SERVER_SECRET") || ""};`,
			},
			{
				Name:        "create_row",
				Description: "Create a row through the bound Tables app.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
				},
				Handler: "tools/create_row.js",
				Code:    `return apteva.app("tables", "rows_create", {name: input.name});`,
			},
		},
	}
}

func TestManagedMCPCreateBridgeCallAndInventory(t *testing.T) {
	s, user, project := newManagedMCPTestServer(t)
	runner := buildManagedMCPRunner(t)
	t.Setenv("APTEVA_MCP_RUNNER_BIN", runner)
	t.Setenv("SERVER_SECRET", "must-not-reach-custom-code")

	runtimeMux := http.NewServeMux()
	runtimeMux.Handle("/api/", http.StripPrefix("/api", http.HandlerFunc(s.handleManagedMCPRuntimeGateway)))
	runtimeServer := httptest.NewServer(runtimeMux)
	t.Cleanup(runtimeServer.Close)
	parsed, _ := url.Parse(runtimeServer.URL)
	s.port = parsed.Port()

	var appInput map[string]any
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		appInput = request.Params.Arguments
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": `{"row_id":12}`}},
			},
		})
	}))
	t.Cleanup(sidecar.Close)
	s.installedApps.Add(&InstalledApp{
		InstallID: 71, AppName: "tables", ProjectID: project.ID,
		SidecarURL: sidecar.URL, Token: "test-app-token",
	})

	body, _ := json.Marshal(managedMCPCreateRequest{
		Name:        "customer-tools",
		Description: "Customer tools",
		ProjectID:   project.ID,
		Definition:  managedDefinition(),
		Env:         map[string]string{"CUSTOM_VALUE": "visible"},
		Bindings:    managedMCPBindings{Apps: map[string]int64{"tables": 71}},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp-servers/managed", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa64(user.ID))
	rec := httptest.NewRecorder()
	s.handleCreateManagedMCPServer(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created managedMCPResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Server.Source != managedMCPSource || created.Server.Status != "running" {
		t.Fatalf("unexpected server: %#v", created.Server)
	}
	t.Cleanup(func() { s.mcpManager.Stop(created.Server.ID) })

	listReq := httptest.NewRequest(http.MethodPost, "/mcp/custom/"+itoa64(created.Server.ID), strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	))
	listReq.RemoteAddr = "127.0.0.1:40000"
	listRec := httptest.NewRecorder()
	authorizeTestMCPRequest(s, listReq)
	s.handleCustomMCPBridge(listRec, listReq)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), `"echo"`) {
		t.Fatalf("tools/list status=%d body=%s", listRec.Code, listRec.Body.String())
	}

	callReq := httptest.NewRequest(http.MethodPost, "/mcp/custom/"+itoa64(created.Server.ID), strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello"}}}`,
	))
	callReq.RemoteAddr = "127.0.0.1:40000"
	callRec := httptest.NewRecorder()
	authorizeTestMCPRequest(s, callReq)
	s.handleCustomMCPBridge(callRec, callReq)
	if callRec.Code != http.StatusOK {
		t.Fatalf("tools/call status=%d body=%s", callRec.Code, callRec.Body.String())
	}
	if !strings.Contains(callRec.Body.String(), `\"message\":\"hello\"`) ||
		!strings.Contains(callRec.Body.String(), `\"source\":\"managed\"`) ||
		!strings.Contains(callRec.Body.String(), `\"configured\":\"visible\"`) ||
		!strings.Contains(callRec.Body.String(), `\"server_secret\":\"\"`) {
		t.Fatalf("unexpected tools/call response: %s", callRec.Body.String())
	}

	appCallReq := httptest.NewRequest(http.MethodPost, "/mcp/custom/"+itoa64(created.Server.ID), strings.NewReader(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_row","arguments":{"name":"Ada"}}}`,
	))
	appCallReq.RemoteAddr = "127.0.0.1:40000"
	appCallRec := httptest.NewRecorder()
	authorizeTestMCPRequest(s, appCallReq)
	s.handleCustomMCPBridge(appCallRec, appCallReq)
	if appCallRec.Code != http.StatusOK || !strings.Contains(appCallRec.Body.String(), `\"row_id\":12`) {
		t.Fatalf("bound app call status=%d body=%s", appCallRec.Code, appCallRec.Body.String())
	}
	if appInput["name"] != "Ada" || appInput["_project_id"] != project.ID {
		t.Fatalf("bound app input=%#v", appInput)
	}

	inventoryReq := httptest.NewRequest(http.MethodGet, "/mcp-servers?project_id="+url.QueryEscape(project.ID), nil)
	inventoryReq.Header.Set("X-User-ID", itoa64(user.ID))
	inventoryRec := httptest.NewRecorder()
	s.handleListMCPServers(inventoryRec, inventoryReq)
	if inventoryRec.Code != http.StatusOK {
		t.Fatalf("inventory status=%d body=%s", inventoryRec.Code, inventoryRec.Body.String())
	}
	var inventory []struct {
		MCPServerRecord
		ProxyConfig map[string]any `json:"proxy_config"`
	}
	if err := json.Unmarshal(inventoryRec.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 {
		t.Fatalf("inventory=%#v", inventory)
	}
	if inventory[0].ProxyConfig["transport"] != "http" {
		t.Fatalf("managed server leaked stdio config: %#v", inventory[0].ProxyConfig)
	}
	if got, _ := inventory[0].ProxyConfig["url"].(string); got != authorizeMCPURL("http://127.0.0.1:"+s.port+"/mcp/custom/"+itoa64(created.Server.ID), s.instanceSecret) {
		t.Fatalf("proxy url=%q", got)
	}
	if _, leaked := inventory[0].ProxyConfig["command"]; leaked {
		t.Fatalf("managed command leaked into agent config: %#v", inventory[0].ProxyConfig)
	}
}

func TestManagedMCPResumeRestartsRunningIntent(t *testing.T) {
	s, user, project := newManagedMCPTestServer(t)
	runner := buildManagedMCPRunner(t)
	t.Setenv("APTEVA_MCP_RUNNER_BIN", runner)
	runtimeServer := httptest.NewServer(http.StripPrefix("/api", http.HandlerFunc(s.handleManagedMCPRuntimeGateway)))
	t.Cleanup(runtimeServer.Close)
	parsed, _ := url.Parse(runtimeServer.URL)
	s.port = parsed.Port()

	cfg := normalizeManagedMCPConfig(managedMCPConfig{})
	encrypted, _ := s.encryptManagedMCPConfig(cfg)
	revision, _ := managedMCPRevision(managedDefinition(), cfg)
	record, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: user.ID, Name: "resume-code", Description: "Resume code",
		Source: managedMCPSource, Transport: "stdio", Command: managedMCPCommand,
		Args: "[]", EncryptedEnv: encrypted, ProjectID: project.ID,
		UpstreamID: revision, ToolCount: len(managedDefinition().Tools),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagedMCPSource(s.managedMCPSourceDir(record.ID), managedDefinition()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.startManagedMCP(record, cfg); err != nil {
		t.Fatal(err)
	}
	s.mcpManager.Stop(record.ID) // simulate process loss; DB intent remains running
	s.mcpManager = NewMCPManager()
	s.ResumeManagedMCPs()
	t.Cleanup(func() { s.mcpManager.Stop(record.ID) })
	if !s.mcpManager.IsRunning(record.ID) {
		t.Fatal("running managed MCP intent was not resumed")
	}
}

func TestManagedMCPCreateKeepsEditableRowWhenRunnerFails(t *testing.T) {
	s, user, project := newManagedMCPTestServer(t)
	t.Setenv("APTEVA_MCP_RUNNER_BIN", filepath.Join(t.TempDir(), "missing-runner"))
	body, _ := json.Marshal(managedMCPCreateRequest{
		Name: "repairable-code", Description: "Repairable code",
		ProjectID: project.ID, Definition: managedDefinition(),
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp-servers/managed", bytes.NewReader(body))
	req.Header.Set("X-User-ID", itoa64(user.ID))
	rec := httptest.NewRecorder()
	s.handleCreateManagedMCPServer(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response managedMCPResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Server == nil || response.Server.Status != "failed" || response.Warning == "" {
		t.Fatalf("response=%#v", response)
	}
	if _, err := os.Stat(filepath.Join(s.managedMCPSourceDir(response.Server.ID), "server.json")); err != nil {
		t.Fatalf("editable source was removed after start failure: %v", err)
	}
}

func TestManagedMCPRuntimeGatewayScopesAppAndToken(t *testing.T) {
	s, user, project := newManagedMCPTestServer(t)
	var received map[string]any
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer app-token" {
			t.Errorf("app token=%q", r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &request)
		received = request.Params.Arguments
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": `{"row_id":7}`}},
			},
		})
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID: 44, AppName: "tables", ProjectID: project.ID,
		SidecarURL: sidecar.URL, Token: "app-token",
	})
	cfg := normalizeManagedMCPConfig(managedMCPConfig{
		Bindings: managedMCPBindings{Apps: map[string]int64{"tables": 44}},
	})
	encrypted, err := s.encryptManagedMCPConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := managedMCPRevision(managedDefinition(), cfg)
	record, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: user.ID, Name: "scoped-code", Description: "Scoped code",
		Source: managedMCPSource, Transport: "stdio", Command: managedMCPCommand,
		Args: "[]", EncryptedEnv: encrypted, ProjectID: project.ID, UpstreamID: revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := s.managedMCPToken(record)
	req := httptest.NewRequest(
		http.MethodPost,
		"/managed-mcp-runtime/"+itoa64(record.ID)+"/apps/tables/call",
		strings.NewReader(`{"tool":"rows_create","input":{"_project_id":"attacker","name":"Ada"}}`),
	)
	req.RemoteAddr = "127.0.0.1:41000"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handleManagedMCPRuntimeGateway(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received["_project_id"] != project.ID {
		t.Fatalf("project marker=%#v, want %q", received["_project_id"], project.ID)
	}
	if !strings.Contains(rec.Body.String(), `"row_id":7`) {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}

	unboundReq := httptest.NewRequest(
		http.MethodPost,
		"/managed-mcp-runtime/"+itoa64(record.ID)+"/apps/storage/call",
		strings.NewReader(`{"tool":"read","input":{}}`),
	)
	unboundReq.RemoteAddr = "127.0.0.1:41000"
	unboundReq.Header.Set("Authorization", "Bearer "+token)
	unboundRec := httptest.NewRecorder()
	s.handleManagedMCPRuntimeGateway(unboundRec, unboundReq)
	if unboundRec.Code != http.StatusForbidden {
		t.Fatalf("unbound status=%d body=%s", unboundRec.Code, unboundRec.Body.String())
	}

	record.UpstreamID = "new-revision"
	if err := s.store.UpdateMCPServerUpstreamID(record.ID, record.UpstreamID); err != nil {
		t.Fatal(err)
	}
	staleReq := httptest.NewRequest(
		http.MethodPost,
		"/managed-mcp-runtime/"+itoa64(record.ID)+"/apps/tables/call",
		strings.NewReader(`{"tool":"rows_create","input":{}}`),
	)
	staleReq.RemoteAddr = "127.0.0.1:41000"
	staleReq.Header.Set("Authorization", "Bearer "+token)
	staleRec := httptest.NewRecorder()
	s.handleManagedMCPRuntimeGateway(staleRec, staleReq)
	if staleRec.Code != http.StatusUnauthorized {
		t.Fatalf("stale token status=%d body=%s", staleRec.Code, staleRec.Body.String())
	}
}

func TestManagedMCPRejectsCrossProjectBindings(t *testing.T) {
	s, user, project := newManagedMCPTestServer(t)
	other, err := s.store.CreateProject(user.ID, "Other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps.Add(&InstalledApp{InstallID: 91, AppName: "tables", ProjectID: other.ID})
	err = s.validateManagedMCPBindings(user.ID, project.ID, managedMCPBindings{
		Apps: map[string]int64{"tables": 91},
	})
	if err == nil || !strings.Contains(err.Error(), "another project") {
		t.Fatalf("cross-project binding error=%v", err)
	}
}

func TestManagedMCPIsSharedWithProjectEditors(t *testing.T) {
	s, owner, project := newManagedMCPTestServer(t)
	editor, err := s.store.CreateUser("managed-editor@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddProjectMember(project.ID, editor.ID, ProjectEditor, owner.ID); err != nil {
		t.Fatal(err)
	}
	cfg := normalizeManagedMCPConfig(managedMCPConfig{})
	encrypted, _ := s.encryptManagedMCPConfig(cfg)
	record, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: owner.ID, Name: "shared-code", Description: "Shared code",
		Source: managedMCPSource, Transport: "stdio", Command: managedMCPCommand,
		Args: "[]", EncryptedEnv: encrypted, ProjectID: project.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/mcp-servers?project_id="+url.QueryEscape(project.ID), nil)
	listReq.Header.Set("X-User-ID", itoa64(editor.ID))
	listRec := httptest.NewRecorder()
	s.handleListMCPServers(listRec, listReq)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), `"shared-code"`) {
		t.Fatalf("editor list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	gatewayRows, err := listGatewayMCPServers(
		s.store, editor.ID, project.ID, map[string]any{}, "5280", s.instanceSecret,
	)
	if err != nil || len(gatewayRows) != 1 {
		t.Fatalf("gateway project inventory=%#v err=%v", gatewayRows, err)
	}
	if gatewayRows[0].ProxyConfig["transport"] != "http" ||
		gatewayRows[0].MCPURL != authorizeMCPURL("http://127.0.0.1:5280/mcp/custom/"+itoa64(record.ID), s.instanceSecret) {
		t.Fatalf("gateway managed config=%#v", gatewayRows[0])
	}

	scopeReq := httptest.NewRequest(
		http.MethodPut,
		"/mcp-servers/"+itoa64(record.ID)+"/tools",
		strings.NewReader(`{"allowed_tools":["echo"]}`),
	)
	scopeReq.Header.Set("X-User-ID", itoa64(editor.ID))
	scopeRec := httptest.NewRecorder()
	s.handleUpdateMCPServerAllowedTools(scopeRec, scopeReq)
	if scopeRec.Code != http.StatusOK {
		t.Fatalf("editor scope status=%d body=%s", scopeRec.Code, scopeRec.Body.String())
	}
	fresh, _, err := s.store.GetMCPServer(owner.ID, record.ID)
	if err != nil || len(fresh.AllowedTools) != 1 || fresh.AllowedTools[0] != "echo" {
		t.Fatalf("allowed tools not persisted by editor: %#v err=%v", fresh, err)
	}
}
