package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type helperBrokerFixture struct {
	s               *Server
	agent           *Agent
	project, thread string
	install         int64
	called          int
	operator        bool
	lastArgs        map[string]any
}

func newHelperBrokerFixture(t *testing.T) *helperBrokerFixture {
	t.Helper()
	s, user, project := newAppProxyProjectUser(t, ProjectEditor)
	s.instanceSecret = "helper-broker-fixture-secret-32bytes"
	agent, err := s.store.GetOrCreatePlatformHelper(user, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	f := &helperBrokerFixture{s: s, agent: agent, project: project, thread: "chat-conv-broker-test", operator: true}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/operator-context" {
			if !f.operator {
				http.Error(w, "operator required", 403)
				return
			}
			writeJSON(w, map[string]any{"project_id": project, "thread_id": r.Header.Get("X-Apteva-Caller-Thread"), "agent_id": agent.ID})
			return
		}
		var rpc struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		var result any
		if rpc.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{"name": "tickets_list", "description": "List tickets", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer"}}}}, map[string]any{"name": "internal_secret", "description": "private"}}}
		} else {
			f.called++
			f.lastArgs = rpc.Params.Arguments
			if r.Header.Get("X-Apteva-Caller-Thread") != f.thread || r.Header.Get("X-Apteva-Project-ID") != project {
				t.Error("lost trusted thread/project")
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"tickets\":[{\"id\":71,\"title\":\"Printer offline\",\"status\":\"open\"}]}"}}}
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(sidecar.Close)
	conv := seedAppWithTools(t, s, "conversations", "", []string{"conversations_send"})
	s.installedApps.Add(&InstalledApp{InstallID: conv, AppName: "conversations", SidecarURL: sidecar.URL})
	if _, err = s.store.BindAgentThreadScope(agent.ID, f.thread, project, conv); err != nil {
		t.Fatal(err)
	}
	f.install = seedAppWithTools(t, s, "tickets", project, []string{"tickets_list"})
	s.installedApps.Add(&InstalledApp{InstallID: f.install, AppName: "tickets", ProjectID: project, SidecarURL: sidecar.URL, Manifest: sdk.Manifest{Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{Name: "tickets_list"}}}}})
	if err = s.registerAppMCP(f.install); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *helperBrokerFixture) request(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	args["_apteva_caller_thread"] = f.thread
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req := httptest.NewRequest("POST", "/api/apps/apteva-server/mcp", strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:42123"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(f.agent.ID))
	rec := httptest.NewRecorder()
	f.s.handlePlatformMCP(rec, req)
	var reply map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &reply) != nil {
		t.Fatalf("invalid reply: %s", rec.Body.String())
	}
	return reply["result"].(map[string]any)
}
func (f *helperBrokerFixture) search(t *testing.T) string {
	t.Helper()
	result := f.request(t, "app_tool_search", map[string]any{"query": "list tickets"})
	if result["isError"] == true {
		t.Fatalf("search failed: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "internal_secret") || strings.Contains(text, "api_key") {
		t.Fatal("private catalog data leaked")
	}
	var out struct {
		Tools []struct {
			Reference string         `json:"reference"`
			Schema    map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	if len(out.Tools) != 1 || out.Tools[0].Schema == nil {
		t.Fatalf("missing bounded schema result: %s", text)
	}
	if len(out.Tools[0].Reference) != 34 {
		t.Fatalf("tool reference length=%d, want short opaque handle", len(out.Tools[0].Reference))
	}
	return out.Tools[0].Reference
}
func TestHelperAppToolsSearchAndCall(t *testing.T) {
	f := newHelperBrokerFixture(t)
	ref := f.search(t)
	result := f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{"limit": 5}})
	if result["isError"] == true || f.called != 1 {
		t.Fatalf("call failed: %v", result)
	}
	if f.lastArgs["_project_id"] != f.project {
		t.Fatal("project was not injected")
	}
}

func TestHelperProxyResponseDoesNotRejectValidLargeResult(t *testing.T) {
	recorder := &helperProxyResponse{header: make(http.Header)}
	payload := strings.Repeat("x", 64*1024)
	if _, err := recorder.Write([]byte(payload)); err != nil {
		t.Fatalf("large app result rejected: %v", err)
	}
	if recorder.body.Len() != len(payload) {
		t.Fatalf("large app result bytes=%d want=%d", recorder.body.Len(), len(payload))
	}
}
func TestHelperAppToolsRecheckRestrictions(t *testing.T) {
	for _, kind := range []string{"tampered", "expired", "other-thread", "other-project", "disabled", "stopped", "schema-changed", "public", "revoked", "forged-project", "reserved"} {
		t.Run(kind, func(t *testing.T) {
			f := newHelperBrokerFixture(t)
			ref := f.search(t)
			args := map[string]any{"limit": 5}
			switch kind {
			case "tampered":
				ref += "x"
			case "expired", "other-thread", "other-project":
				v, _ := f.s.readHelperTool(ref)
				if kind == "expired" {
					v.Expires = time.Now().Add(-time.Hour).Unix()
				} else if kind == "other-thread" {
					v.Thread = "chat-other"
				} else {
					v.Project = "other"
				}
				ref = f.s.signHelperTool(v)
			case "disabled":
				_, _ = f.s.store.db.Exec(`UPDATE mcp_servers SET allowed_tools='[]' WHERE upstream_id=?`, appMCPUpstreamID(f.install))
			case "stopped":
				_, _ = f.s.store.db.Exec(`UPDATE app_installs SET status='stopped' WHERE id=?`, f.install)
			case "schema-changed":
				v, _ := f.s.readHelperTool(ref)
				v.Schema = "old-schema"
				ref = f.s.signHelperTool(v)
			case "public":
				f.operator = false
			case "revoked":
				_, _ = f.s.store.db.Exec(`DELETE FROM project_members WHERE project_id=? AND user_id=?`, f.project, f.agent.UserID)
			case "forged-project":
				args["project_id"] = "another"
			case "reserved":
				args["_apteva_caller_thread"] = "main"
			}
			result := f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": args})
			if result["isError"] != true || f.called != 0 {
				t.Fatalf("restriction %s bypassed: %v", kind, result)
			}
		})
	}
}
