package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedAgentToolsApp(t *testing.T, s *Server, name, projectID string, permissions []sdk.Permission, deps []sdk.RequiredAppRef, bindings map[string]any) int64 {
	t.Helper()
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: name, DisplayName: name, Version: "1.0.0",
		Scopes:   []sdk.Scope{sdk.ScopeGlobal},
		Requires: sdk.Requires{Permissions: permissions, Apps: deps},
		Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: name + "_tool", Description: "test tool"}}},
	}
	installID := seedInstallWithBindings(t, s, name, manifest, bindings)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id=?`, projectID, installID); err != nil {
		t.Fatal(err)
	}
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	return installID
}

func callAgentTools(t *testing.T, s *Server, installID int64, body sdk.EnsureAppToolsRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/agent-tools/ensure-attached", bytes.NewReader(raw))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func TestCallbackAgentToolsEnsuresSelfAndDeclaredDependencyAdditively(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	agent, err := s.store.CreateAgent(1, "builder-target", "directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = []any{map[string]any{"name": "existing", "transport": "http", "url": "https://example.test/mcp"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	conversationsID := seedAgentToolsApp(t, s, "conversations-tools-test", "", nil, nil, nil)
	builderID := seedAgentToolsApp(t, s, "builder-tools-test", "", []sdk.Permission{sdk.PermMCPAttach},
		[]sdk.RequiredAppRef{{Name: "conversations-tools-test"}}, map[string]any{"conversations-tools-test": conversationsID})

	rec := callAgentTools(t, s, builderID, sdk.EnsureAppToolsRequest{
		AgentID: agent.ID, IncludeRequiredApps: []string{"conversations-tools-test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result sdk.EnsureAppToolsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.Applied || result.AgentRunning {
		t.Fatalf("result = %#v", result)
	}
	wantInstalls := []int64{builderID, conversationsID}
	if builderID > conversationsID {
		wantInstalls[0], wantInstalls[1] = wantInstalls[1], wantInstalls[0]
	}
	if !reflect.DeepEqual(result.AttachedInstallIDs, wantInstalls) {
		t.Fatalf("attached installs=%v want=%v", result.AttachedInstallIDs, wantInstalls)
	}
	servers, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"existing", "builder-tools-test", "conversations-tools-test"} {
		if !hasMCPName(servers, name) {
			t.Fatalf("missing MCP %q in %#v", name, servers)
		}
	}
	for _, installID := range []int64{builderID, conversationsID} {
		var count int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, installID, agent.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("binding install=%d count=%d err=%v", installID, count, err)
		}
	}

	rec = callAgentTools(t, s, builderID, sdk.EnsureAppToolsRequest{
		AgentID: agent.ID, IncludeRequiredApps: []string{"conversations-tools-test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second ensure status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("idempotent ensure reported changed: %#v", result)
	}
}

func TestCallbackAgentToolsEnforcesPermissionDeclarationBindingAndReadiness(t *testing.T) {
	tests := []struct {
		name       string
		permission bool
		deps       []sdk.RequiredAppRef
		bindings   map[string]any
		register   bool
		requested  []string
		wantCode   string
	}{
		{name: "permission", register: true, wantCode: "permission_denied"},
		{name: "undeclared", permission: true, register: true, requested: []string{"other"}, wantCode: agentToolsAppNotDeclared},
		{name: "unbound", permission: true, register: true, deps: []sdk.RequiredAppRef{{Name: "other"}}, requested: []string{"other"}, wantCode: agentToolsAppNotBound},
		{name: "caller not ready", permission: true, wantCode: agentToolsCallerNotReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			ensureTestAdmin(t, s)
			agent, err := s.store.CreateAgent(1, "target", "directive", "autonomous", `{}`, "proj-1")
			if err != nil {
				t.Fatal(err)
			}
			perms := []sdk.Permission{}
			if tt.permission {
				perms = append(perms, sdk.PermMCPAttach)
			}
			manifest := sdk.Manifest{
				Schema: sdk.SchemaCurrent, Name: "caller-" + tt.name, Version: "1.0.0",
				Requires: sdk.Requires{Permissions: perms, Apps: tt.deps},
				Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "tool", Description: "tool"}}},
			}
			callerID := seedInstallWithBindings(t, s, manifest.Name, manifest, tt.bindings)
			if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, callerID); err != nil {
				t.Fatal(err)
			}
			if tt.register {
				if err := s.registerAppMCP(callerID); err != nil {
					t.Fatal(err)
				}
			}
			rec := callAgentTools(t, s, callerID, sdk.EnsureAppToolsRequest{AgentID: agent.ID, IncludeRequiredApps: tt.requested})
			if rec.Code < 400 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var problem map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &problem)
			if problem["code"] != tt.wantCode {
				t.Fatalf("code=%v want=%s body=%s", problem["code"], tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestCallbackAgentToolsEnsuresPlatformHelperAndRepairsBindings(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	helper, err := s.store.GetOrCreatePlatformHelper(1, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	dependencyID := seedAgentToolsApp(t, s, "helper-dependency-test", "", nil, nil, nil)
	callerID := seedAgentToolsApp(t, s, "helper-caller-test", "", []sdk.Permission{sdk.PermMCPAttach},
		[]sdk.RequiredAppRef{{Name: "helper-dependency-test"}}, map[string]any{"helper-dependency-test": dependencyID})

	rec := callAgentTools(t, s, callerID, sdk.EnsureAppToolsRequest{
		AgentKind: sdk.AgentKindPlatformHelper, IncludeRequiredApps: []string{"helper-dependency-test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure Helper status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result sdk.EnsureAppToolsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AgentID != helper.ID || !result.Changed || !result.Applied || result.AgentRunning {
		t.Fatalf("result=%#v", result)
	}
	wantedMCPIDs := append([]int64{}, result.MCPServerIDs...)
	reloaded, err := s.store.GetPlatformHelper(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(helperSelectedGlobalMCPServerIDs(reloaded), wantedMCPIDs) {
		t.Fatalf("selected=%v want=%v", helperSelectedGlobalMCPServerIDs(reloaded), wantedMCPIDs)
	}
	for _, installID := range []int64{callerID, dependencyID} {
		var count int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, installID, helper.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("Helper binding install=%d count=%d err=%v", installID, count, err)
		}
	}

	if _, err := s.store.db.Exec(`DELETE FROM app_agent_bindings WHERE install_id=? AND agent_id=?`, callerID, helper.ID); err != nil {
		t.Fatal(err)
	}
	rec = callAgentTools(t, s, callerID, sdk.EnsureAppToolsRequest{
		AgentKind: sdk.AgentKindPlatformHelper, IncludeRequiredApps: []string{"helper-dependency-test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repair status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("metadata repair changed MCP selection: %#v", result)
	}
	var repaired int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=?`, callerID, helper.ID).Scan(&repaired)
	if repaired != 1 {
		t.Fatalf("binding repair count=%d", repaired)
	}
}

func TestCallbackAgentToolsInactiveHelperIsTypedPendingSetup(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	callerID := seedAgentToolsApp(t, s, "inactive-helper-caller", "", []sdk.Permission{sdk.PermMCPAttach}, nil, nil)
	rec := callAgentTools(t, s, callerID, sdk.EnsureAppToolsRequest{AgentKind: sdk.AgentKindPlatformHelper})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var problem map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	if problem["code"] != agentToolsTargetNotFound || problem["agent_kind"] != string(sdk.AgentKindPlatformHelper) {
		t.Fatalf("problem=%#v", problem)
	}
}

func TestUninstallImmediatelyDetachesAppToolsFromOrdinaryAgent(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	agent, err := s.store.CreateAgent(1, "uninstall-target", "directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	installID := seedAgentToolsApp(t, s, "uninstall-tools-test", "", []sdk.Permission{sdk.PermMCPAttach}, nil, nil)
	rec := callAgentTools(t, s, installID, sdk.EnsureAppToolsRequest{AgentID: agent.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", rec.Code, rec.Body.String())
	}

	req := authedRequest(t, http.MethodDelete, "/apps/installs/"+itoa(installID), "", nil)
	rec = httptest.NewRecorder()
	s.handleUninstallApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall status=%d body=%s", rec.Code, rec.Body.String())
	}
	servers, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasMCPName(servers, "uninstall-tools-test") {
		t.Fatalf("uninstalled app remained in agent config: %#v", servers)
	}
}

func TestUninstallImmediatelyDetachesAppToolsFromHelperSelection(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	helper, err := s.store.GetOrCreatePlatformHelper(1, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	installID := seedAgentToolsApp(t, s, "uninstall-helper-tools-test", "", []sdk.Permission{sdk.PermMCPAttach}, nil, nil)
	rec := callAgentTools(t, s, installID, sdk.EnsureAppToolsRequest{AgentKind: sdk.AgentKindPlatformHelper})
	if rec.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(helperSelectedGlobalMCPServerIDs(mustPlatformHelper(t, s, 1))) != 1 {
		t.Fatal("Helper selection was not attached")
	}

	req := authedRequest(t, http.MethodDelete, "/apps/installs/"+itoa(installID), "", nil)
	rec = httptest.NewRecorder()
	s.handleUninstallApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall status=%d body=%s", rec.Code, rec.Body.String())
	}
	reloaded := mustPlatformHelper(t, s, 1)
	if selected := helperSelectedGlobalMCPServerIDs(reloaded); len(selected) != 0 {
		t.Fatalf("Helper retained uninstalled MCP selection: %v", selected)
	}
	servers, err := s.currentAgentMCPServers(helper, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasMCPName(servers, "uninstall-helper-tools-test") {
		t.Fatalf("Helper retained uninstalled MCP config: %#v", servers)
	}
}

func mustPlatformHelper(t *testing.T, s *Server, userID int64) *Agent {
	t.Helper()
	helper, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	return helper
}
