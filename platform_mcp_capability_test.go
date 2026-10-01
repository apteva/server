package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlatformMCPCapabilityIsBoundToAgentAndCurrentGrant(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	s.instanceSecret = "platform-capability-test-secret"
	s.port = "5280"
	agent, err := s.store.CreateAgent(userID, "Project manager", "test", "autonomous", `{"include_apteva_server":true}`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"mcp_servers": []any{managementGatewayConfig(agent, "", s.port)}}
	if err := s.authorizeAgentMCPConfig(agent, config); err != nil {
		t.Fatal(err)
	}
	endpoint := config["mcp_servers"].([]any)[0].(map[string]any)["url"].(string)
	calls := 0
	s.platformGatewayExec = func(_ context.Context, caller int64, project string, _ []byte) ([]byte, error) {
		calls++
		if caller != userID || project != projectID {
			t.Fatalf("gateway scope user=%d project=%q", caller, project)
		}
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"agents_list"},{"name":"create_mcp_server"}]}}`), nil
	}
	request := func(id int64, url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.RemoteAddr = "127.0.0.1:42123"
		req.Header.Set("X-Apteva-Caller-Agent", itoa64(id))
		rec := httptest.NewRecorder()
		s.handlePlatformMCP(rec, req)
		return rec
	}
	valid := request(agent.ID, endpoint)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), "agents_list") || strings.Contains(valid.Body.String(), "create_mcp_server") || calls != 1 {
		t.Fatalf("project tools status=%d calls=%d body=%s", valid.Code, calls, valid.Body.String())
	}
	other, err := s.store.CreateAgent(userID, "Other manager", "test", "autonomous", agent.Config, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := request(other.ID, endpoint); rec.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("cross-agent token accepted: status=%d calls=%d", rec.Code, calls)
	}
	if rec := request(agent.ID, platformMCPPath); rec.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("missing token accepted: status=%d calls=%d", rec.Code, calls)
	}
	agent.Config = `{"include_apteva_server":false}`
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	if rec := request(agent.ID, endpoint); rec.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("detached grant accepted: status=%d calls=%d", rec.Code, calls)
	}
	agent.Config = `{"include_apteva_server":true}`
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`DELETE FROM project_members WHERE project_id=? AND user_id=?`, projectID, userID); err != nil {
		t.Fatal(err)
	}
	if rec := request(agent.ID, endpoint); rec.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("revoked project grant accepted: status=%d calls=%d", rec.Code, calls)
	}
}

func TestPlatformMCPInventoryIsStableAndServerManaged(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for i := 0; i < 2; i++ {
		if err := s.store.ensurePlatformMCPInventory(1); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.store.ListMCPServers(1)
	if err != nil || len(rows) != 1 || rows[0].Source != platformMCPSource || rows[0].Name != platformMCPName {
		t.Fatalf("built-in inventory rows=%#v err=%v", rows, err)
	}
	if err := s.store.DeleteMCPServer(1, rows[0].ID); err == nil {
		t.Fatal("built-in capability was deleted")
	}
	if err := s.store.UpdateMCPServerAllowedTools(1, rows[0].ID, []string{"agents_list"}); err == nil {
		t.Fatal("built-in tool definitions were edited")
	}
	if _, err := s.store.CreateMCPServerExt(MCPServerInput{UserID: 1, Name: platformMCPName, Source: "custom"}); err == nil {
		t.Fatal("built-in identity was shadowed")
	}
}
