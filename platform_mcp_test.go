package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlatformMCPUsesTrustedConversationProject(t *testing.T) {
	s := newTestServer(t)
	s.instanceSecret = "platform-mcp-fixture-secret"
	user, err := s.store.CreateUser("platform-mcp@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPlatformRole(user.ID, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	const projectID = "project-trusted"
	const threadID = "chat-conv-trusted"
	if _, err := s.store.BindAgentThreadScope(helper.ID, threadID, projectID, 91); err != nil {
		t.Fatal(err)
	}

	var gotProject string
	var gotRequest map[string]any
	s.platformGatewayExec = func(_ context.Context, userID int64, project string, request []byte) ([]byte, error) {
		if userID != user.ID {
			t.Fatalf("user_id=%d, want %d", userID, user.ID)
		}
		gotProject = project
		if err := json.Unmarshal(request, &gotRequest); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`), nil
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agents_create","arguments":{"name":"Worker","project_id":"project-forged","_apteva_caller_thread":"` + threadID + `"}}}`
	req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, helper)), strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(helper.ID))
	rec := httptest.NewRecorder()
	s.handlePlatformMCP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotProject != projectID {
		t.Fatalf("project=%q, want %q", gotProject, projectID)
	}
	params := gotRequest["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if args["project_id"] != projectID {
		t.Fatalf("forwarded project_id=%v, want %q", args["project_id"], projectID)
	}
	if _, leaked := args["_apteva_caller_thread"]; leaked {
		t.Fatal("hidden caller thread leaked to the gateway")
	}
}

func TestPlatformMCPRejectsCrossProjectAgentTarget(t *testing.T) {
	s := newTestServer(t)
	s.instanceSecret = "platform-mcp-fixture-secret"
	user, err := s.store.CreateUser("platform-mcp-cross@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPlatformRole(user.ID, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.store.CreateAgent(user.ID, "Other project", "test", "autonomous", "{}", "project-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.BindAgentThreadScope(helper.ID, "chat-project-a", "project-a", 91); err != nil {
		t.Fatal(err)
	}
	called := false
	s.platformGatewayExec = func(context.Context, int64, string, []byte) ([]byte, error) {
		called = true
		return nil, nil
	}
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"agents_update","arguments":{"id":"` + itoa64(target.ID) + `","name":"Wrong","_apteva_caller_thread":"chat-project-a"}}}`
	req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, helper)), strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43211"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(helper.ID))
	rec := httptest.NewRecorder()
	s.handlePlatformMCP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not in the trusted project") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("gateway executed a rejected cross-project mutation")
	}
}

func TestManagementGatewayConfigUsesSharedHTTPTransport(t *testing.T) {
	helper := managementGatewayConfig(&Agent{ID: 1, UserID: 7, Kind: "platform_helper"}, "/server", "5280")
	if helper["transport"] != "http" || helper["no_spawn"] == true || !strings.Contains(helper["url"].(string), "/api/apps/apteva-server/mcp") {
		t.Fatalf("helper gateway=%#v", helper)
	}
	ordinary := managementGatewayConfig(&Agent{ID: 2, UserID: 7, Kind: "user"}, "/server", "5280")
	if ordinary["transport"] != "http" || ordinary["command"] != nil || ordinary["url"] != helper["url"] {
		t.Fatalf("ordinary gateway=%#v", ordinary)
	}
}

func TestPlatformMCPAppDetailsAreAvailableAndProjectScoped(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for _, project := range []string{"details-project", "details-other"} {
		if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name,description) VALUES(?,1,?,'')`, project, project); err != nil {
			t.Fatal(err)
		}
	}
	own := seedAppWithTools(t, s, "own-app", "details-project", []string{"own_get"})
	global := seedAppWithTools(t, s, "global-app", "", []string{"global_get"})
	other := seedAppWithTools(t, s, "other-app", "details-other", []string{"other_get"})
	if !projectConversationGatewayTools["apps_get"] {
		t.Fatal("apps_get absent from project tools")
	}
	for _, tc := range []struct {
		id      int64
		allowed bool
	}{{own, true}, {global, true}, {other, false}, {999999, false}} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "apps_get", "arguments": map[string]any{"install_id": tc.id, "project_id": "details-other"}}})
		scoped, err := s.scopeProjectGatewayRequest(body, "details-project")
		if (err == nil) != tc.allowed {
			t.Fatalf("installation %d allowed=%v error=%v", tc.id, tc.allowed, err)
		}
		if tc.allowed {
			var rpc map[string]any
			_ = json.Unmarshal(scoped, &rpc)
			args := rpc["params"].(map[string]any)["arguments"].(map[string]any)
			if args["project_id"] != "details-project" {
				t.Fatalf("forged project scope survived: %#v", args)
			}
		}
	}
}
