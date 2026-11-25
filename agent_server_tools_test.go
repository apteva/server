package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// serverToolsFixture makes an agent limited to read-only Apteva Server tools,
// the way a preset grants them, and a gateway client that acts as its owner.
func serverToolsFixture(t *testing.T, tools ...string) (*Server, *Agent, gatewayAPIClient) {
	t.Helper()
	s := newTestServer(t)
	seedPresetProject(t, s, "p")
	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.configWithAgentCapabilities(&Agent{UserID: 1, ProjectID: "p", Config: "{}"}, []AgentCapability{{Name: platformMCPName, AllowedTools: tools}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "CEO", "Run the company", "autonomous", cfg, "p")
	if err != nil {
		t.Fatal(err)
	}
	api := newGatewayAgentAPITestServer(s)
	t.Cleanup(api.Close)
	return s, agent, gatewayAPIClient{baseURL: api.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}
}

func gatewayCall(t *testing.T, s *Server, client gatewayAPIClient, tool string, args map[string]any) (map[string]any, error) {
	t.Helper()
	out, err := handleGatewayAgentTool(tool, args, "p", client, s.store, "/tmp/server")
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(mustJSON(out), &result); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func capabilityServerTools(t *testing.T, s *Server, client gatewayAPIClient, agentID int64) any {
	t.Helper()
	got, err := gatewayCall(t, s, client, "agents_get", map[string]any{"id": agentID})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, _ := got["capabilities"].(map[string]any)
	return capabilities["apteva_server_tools"]
}

func asStrings(value any) []string {
	list, _ := value.([]any)
	out := []string{}
	for _, item := range list {
		name, _ := item.(string)
		out = append(out, name)
	}
	return out
}

// An operator, or the Helper for one, sees the agent's tools, widens them and
// removes the limit; every call is checked against the new set at once.
func TestAgentServerToolsShownWidenedAndReset(t *testing.T) {
	s, agent, client := serverToolsFixture(t, "agents_list", "agents_get")

	if got := asStrings(capabilityServerTools(t, s, client, agent.ID)); !reflect.DeepEqual(got, []string{"agents_list", "agents_get"}) {
		t.Fatalf("agents_get apteva_server_tools = %v", got)
	}
	var listed map[string]any
	if err := client.do(http.MethodGet, fmt.Sprintf("/agents/%d/server-tools", agent.ID), nil, &listed); err != nil {
		t.Fatal(err)
	}
	if listed["attached"] != true || len(asStrings(listed["available"])) != len(projectConversationGatewayTools) {
		t.Fatalf("GET server-tools = %v", listed)
	}

	widened := []any{"agents_list", "agents_get", "agents_update", "agents_start", "agents_stop"}
	result, err := gatewayCall(t, s, client, "agents_update", map[string]any{"id": agent.ID, "apteva_server_tools": widened})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(asStrings(result["changed"]), "apteva_server_tools") || result["apteva_server_tools_apply"] != "on next start" {
		t.Fatalf("agents_update result = %v", result)
	}
	saved, _ := s.store.GetAgentByID(agent.ID)
	if !platformToolAllowed(saved, "agents_start") || !platformToolAllowed(saved, "agents_update") || platformToolAllowed(saved, "agents_delete") {
		t.Fatalf("saved grant = %v", agentServerTools(saved))
	}
	if got := asStrings(capabilityServerTools(t, s, client, agent.ID)); len(got) != len(widened) {
		t.Fatalf("agents_get after widening = %v", got)
	}

	if _, err := gatewayCall(t, s, client, "agents_update", map[string]any{"id": agent.ID, "apteva_server_tools": []any{"all"}}); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.store.GetAgentByID(agent.ID)
	if strings.Contains(saved.Config, "platform_mcp_tools") || !platformToolAllowed(saved, "agents_delete") {
		t.Fatalf("reset left a limit: %s", saved.Config)
	}
	if got := capabilityServerTools(t, s, client, agent.ID); got != serverToolsAll {
		t.Fatalf("agents_get after reset = %v", got)
	}
}

// Bad values are refused before anything is saved; a bad saved value would
// block every tool.
func TestAgentServerToolsRefusesBadValues(t *testing.T) {
	s, agent, client := serverToolsFixture(t, "agents_list", "agents_get")
	before, _ := s.store.GetAgentByID(agent.ID)
	for _, tc := range []struct {
		value any
		want  string
	}{
		{[]any{"agents_nuke"}, "unknown Apteva Server tools: agents_nuke"},
		{[]any{}, "needs at least one tool"},
		{[]any{"all", "agents_list"}, `"all" cannot be combined`},
		{"everything", `must be "all" or a list`},
		{nil, `must be "all" or a list`},
		{[]any{3}, `must be "all" or a list`},
	} {
		_, err := gatewayCall(t, s, client, "agents_update", map[string]any{"id": agent.ID, "apteva_server_tools": tc.value})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("value %v: err=%v, want %q", tc.value, err, tc.want)
		}
	}
	// Changing the limit inside config is refused; sending it back unchanged is not.
	for _, value := range []any{[]any{"agents_delete"}, nil} {
		_, err := gatewayCall(t, s, client, "agents_update", map[string]any{"id": agent.ID, "config": map[string]any{"platform_mcp_tools": value}})
		if err == nil || !strings.Contains(err.Error(), "apteva_server_tools") {
			t.Fatalf("config patch %v: err=%v", value, err)
		}
	}
	if _, err := gatewayCall(t, s, client, "agents_update", map[string]any{"id": agent.ID, "config": map[string]any{"platform_mcp_tools": []any{"agents_list", "agents_get"}, "temperature": 0.2}}); err != nil {
		t.Fatalf("unchanged value refused: %v", err)
	}
	after, _ := s.store.GetAgentByID(agent.ID)
	if !reflect.DeepEqual(agentServerTools(after), agentServerTools(before)) {
		t.Fatalf("tools changed: %v", agentServerTools(after))
	}

	plain, _ := s.store.CreateAgent(1, "Plain", "No server tools", "autonomous", "{}", "p")
	if _, err := s.setAgentServerTools(plain.ID, []string{"agents_list"}); err == nil || !strings.Contains(err.Error(), "attach apteva-server first") {
		t.Fatalf("unattached agent: err=%v", err)
	}
	if _, err := s.setAgentServerTools(plain.ID, nil); err != nil {
		t.Fatalf("removing a limit from an unattached agent: %v", err)
	}
	helper, _ := s.store.CreateAgent(1, "Helper", "help", "autonomous", "{}", "")
	if _, err := s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, helper.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setAgentServerTools(helper.ID, nil); err == nil {
		t.Fatal("changed the Helper's tools")
	}
}

// A limited agent cannot widen its own tools or anyone else's.
func TestLimitedAgentCannotChangeServerTools(t *testing.T) {
	s, limited, _ := serverToolsFixture(t, "agents_update", "agents_get")
	other, _ := s.store.CreateAgent(1, "Other", "test", "autonomous", "{}", "p")
	called := false
	s.platformGatewayExec = func(context.Context, int64, string, []byte) ([]byte, error) {
		called = true
		return []byte(`{"result":{}}`), nil
	}
	for _, target := range []int64{limited.ID, other.ID} {
		called = false
		reply := starterRPC(t, s, limited, "agents_update", map[string]any{"id": target, "apteva_server_tools": []any{"all"}})
		if called || !strings.Contains(string(mustJSON(reply)), `"isError":true`) {
			t.Fatalf("target %d: called=%v reply=%v", target, called, reply)
		}
	}
	saved, _ := s.store.GetAgentByID(limited.ID)
	if platformToolAllowed(saved, "agents_delete") {
		t.Fatal("limited agent widened its own tools")
	}
}

// The agent's tool list follows its saved set.
func TestServerToolListFollowsChangedSet(t *testing.T) {
	s, agent, _ := serverToolsFixture(t, "agents_list", "agents_get")
	s.platformGatewayExec = func(_ context.Context, _ int64, _ string, body []byte) ([]byte, error) {
		tools := []any{}
		for _, name := range serverToolNames() {
			tools = append(tools, map[string]any{"name": name})
		}
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": tools}})
	}
	list := func() []string {
		req := httptest.NewRequest(http.MethodPost, addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, agent)), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Apteva-Caller-Agent", itoa64(agent.ID))
		rec := httptest.NewRecorder()
		s.handlePlatformMCP(rec, req)
		var reply struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("tools/list: %d %s", rec.Code, rec.Body.String())
		}
		names := []string{}
		for _, tool := range reply.Result.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	if got := list(); len(got) != 2 || containsString(got, "agents_start") {
		t.Fatalf("limited list = %v", got)
	}
	if _, err := s.setAgentServerTools(agent.ID, []string{"agents_list", "agents_get", "agents_start"}); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 3 || !containsString(got, "agents_start") {
		t.Fatalf("widened list = %v", got)
	}
	if _, err := s.setAgentServerTools(agent.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != len(projectConversationGatewayTools) {
		t.Fatalf("unlimited list has %d tools", len(got))
	}
}

// A running agent reconnects its Apteva Server tools, and nothing else, so
// its tool list changes without a restart.
func TestServerToolsChangeReconnectsRunningAgent(t *testing.T) {
	s, agent, _ := serverToolsFixture(t, "agents_list", "agents_get")
	serverURL := "http://127.0.0.1:5280" + platformMCPPath + "?mcp_token=t"
	otherURL := "http://127.0.0.1:5280/api/apps/notes/mcp?install_id=99"
	current := map[string]any{"mcp_servers": []any{
		map[string]any{"name": platformMCPName, "transport": "http", "url": serverURL},
		map[string]any{"name": "notes", "transport": "http", "url": otherURL},
	}}
	puts := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			var applied map[string]any
			_ = json.NewDecoder(r.Body).Decode(&applied)
			current["mcp_servers"] = applied["mcp_servers"]
		}
		_ = json.NewEncoder(w).Encode(current)
	}))
	defer core.Close()
	s.agents.processes[agent.ID] = &runningAgent{port: core.Listener.Addr().(*net.TCPAddr).Port, coreAPIKey: "core-key", reattached: true}

	req := httptest.NewRequest(http.MethodPut, "/instances/"+itoa64(agent.ID)+"/server-tools", strings.NewReader(`{"tools":["agents_list","agents_get","agents_start"]}`))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAgentServerTools(rec, req, agent.ID)
	var reply map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if rec.Code != http.StatusOK || reply["applies"] != "now" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	servers := mcpMaps(current["mcp_servers"])
	if puts != 1 || servers[0]["url"] == serverURL || !strings.HasPrefix(servers[0]["url"].(string), "http://127.0.0.1:5280"+platformMCPPath+"?") || servers[1]["url"] != otherURL {
		t.Fatalf("puts=%d servers=%v", puts, servers)
	}
}
