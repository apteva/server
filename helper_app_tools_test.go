package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	// When set, answers a tool call in place of the normal result.
	callReply func(w http.ResponseWriter, id any)
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
			if f.callReply != nil {
				f.callReply(w, rpc.ID)
				return
			}
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
func (f *helperBrokerFixture) requestHTTP(t *testing.T, name string, args map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	args["_apteva_caller_thread"] = f.thread
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(f.s.instanceSecret, f.agent)), strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:42123"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(f.agent.ID))
	rec := httptest.NewRecorder()
	f.s.handlePlatformMCP(rec, req)
	return rec
}
func (f *helperBrokerFixture) request(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	rec := f.requestHTTP(t, name, args)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status=%d: %s", rec.Code, rec.Body.String())
	}
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
			if kind == "revoked" {
				rec := f.requestHTTP(t, "app_tool_call", map[string]any{"reference": ref, "arguments": args})
				if rec.Code != http.StatusForbidden || f.called != 0 {
					t.Fatalf("revoked project access bypassed: status=%d body=%s", rec.Code, rec.Body.String())
				}
				return
			}
			result := f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": args})
			if kind == "revoked" && result["http_status"] != http.StatusForbidden {
				t.Fatalf("revocation not enforced at ingress: %#v", result)
			}
			if result["isError"] != true || f.called != 0 {
				t.Fatalf("restriction %s bypassed: %v", kind, result)
			}
		})
	}
}

func TestHelperAppToolSearchUsesAppMetadata(t *testing.T) {
	f := newHelperBrokerFixture(t)
	entry := f.s.installedApps.Get(f.install)
	entry.Manifest.DisplayName = "Customer Service Desk"
	entry.Manifest.Description = "Investigate onboarding procedures and escalation evidence"
	for _, query := range []string{"onboarding", "escalation", "customer service"} {
		result := f.request(t, "app_tool_search", map[string]any{"query": query})
		if result["isError"] == true {
			t.Fatalf("metadata search failed: %#v", result)
		}
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		var out struct {
			Apps []struct {
				DisplayName string `json:"app_display_name"`
				Description string `json:"app_description"`
			} `json:"apps"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Tools) != 1 || out.Tools[0].Name != "tickets_list" || len(out.Apps) != 1 || out.Apps[0].DisplayName != entry.Manifest.DisplayName || out.Apps[0].Description != entry.Manifest.Description {
			t.Fatalf("app metadata query %q did not discover filtered tools: %s", query, text)
		}
		// The app's description is sent once, in its apps entry.
		if n := strings.Count(text, entry.Manifest.Description); n != 1 {
			t.Fatalf("app description sent %d times, want once: %s", n, text)
		}
		if strings.Contains(text, "internal_secret") {
			t.Fatal("metadata search exposed private tool")
		}
	}
}

// helperWideApp installs an app with many tools whose description mentions
// most of what its tools do, the shape that used to hide tools: every tool
// tied on score and only the first five came back.
type helperWideApp struct {
	install int64
	names   []string
	called  []string
}

func addHelperWideApp(t *testing.T, f *helperBrokerFixture) *helperWideApp {
	t.Helper()
	app := &helperWideApp{names: []string{"workspace_preview_start", "workspace_preview_get", "workspace_preview_logs", "workspace_preview_stop", "workspaces_create", "workspaces_list", "workspaces_get"}}
	for i := 0; i < 23; i++ {
		app.names = append(app.names, "workspace_extra_"+itoa64(int64(i)))
	}
	specs := []sdk.MCPToolSpec{}
	catalog := []any{}
	for _, name := range app.names {
		description := "Operate a workspace: " + strings.ReplaceAll(name, "_", " ")
		specs = append(specs, sdk.MCPToolSpec{Name: name, Description: description})
		catalog = append(catalog, map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}})
	}
	specs = append(specs, sdk.MCPToolSpec{Name: "workspace_internal_sync", Description: "Create and list workspace internals", Exposure: sdk.ToolExposureAppOnly})
	catalog = append(catalog, map[string]any{"name": "workspace_internal_sync", "description": "Create and list workspace internals"})
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		var result any = map[string]any{"tools": catalog}
		if rpc.Method != "tools/list" {
			app.called = append(app.called, rpc.Params.Name)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"id\":\"ws_1\"}"}}}
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(sidecar.Close)
	app.install = seedAppWithTools(t, f.s, "workspaces", f.project, app.names)
	f.s.installedApps.Add(&InstalledApp{InstallID: app.install, AppName: "workspaces", ProjectID: f.project, SidecarURL: sidecar.URL, Manifest: sdk.Manifest{
		DisplayName: "Workspaces",
		Description: "Create, list and run durable development workspaces with commands, previews and file edits.",
		Provides:    sdk.Provides{MCPTools: specs},
	}})
	if err := f.s.registerAppMCP(app.install); err != nil {
		t.Fatal(err)
	}
	return app
}

type helperSearchReply struct {
	Apps []struct {
		App       string `json:"app"`
		ToolCount int    `json:"tool_count"`
		Tools     []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	} `json:"apps"`
	Tools []struct {
		App       string         `json:"app"`
		Name      string         `json:"name"`
		Reference string         `json:"reference"`
		ExpiresAt string         `json:"expires_at"`
		Schema    map[string]any `json:"inputSchema"`
	} `json:"tools"`
	Others []struct {
		App string `json:"app"`
	} `json:"other_apps"`
	NotFound []string `json:"tools_not_found"`
	Omitted  []string `json:"tools_omitted"`
	Note     string   `json:"note"`
	text     string
}

func (f *helperBrokerFixture) searchWith(t *testing.T, args map[string]any) helperSearchReply {
	t.Helper()
	result := f.request(t, "app_tool_search", args)
	if result["isError"] == true {
		t.Fatalf("search %v failed: %v", args, result)
	}
	var out helperSearchReply
	out.text = result["content"].([]any)[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(out.text), &out); err != nil {
		t.Fatalf("invalid search result: %v: %s", err, out.text)
	}
	return out
}

func (r helperSearchReply) app(name string) []string {
	for _, a := range r.Apps {
		if a.App == name {
			names := []string{}
			for _, tool := range a.Tools {
				names = append(names, tool.Name)
			}
			return names
		}
	}
	return nil
}

func TestHelperAppToolSearchListsEveryToolOfAWideApp(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)
	// best is the tool each task search must detail first, so that one
	// search is enough to call it. Empty means no tool matches the query.
	for _, c := range []struct {
		args map[string]any
		best string
	}{
		{map[string]any{"query": "create workspace"}, "workspaces_create"},
		{map[string]any{"query": "create a new workspace"}, "workspaces_create"},
		{map[string]any{"query": "list workspaces"}, "workspaces_list"},
		{map[string]any{"query": "anything at all", "app": "workspaces"}, ""},
	} {
		args := c.args
		out := f.searchWith(t, args)
		got := out.app("workspaces")
		if strings.Join(got, ",") != strings.Join(wide.names, ",") {
			t.Fatalf("search %v listed %d tools, want all %d in manifest order: %v", args, len(got), len(wide.names), got)
		}
		if strings.Contains(out.text, "workspace_internal_sync") {
			t.Fatalf("search %v exposed an app-only tool", args)
		}
		if c.best == "" && len(out.Tools) != 0 {
			t.Fatalf("search %v returned %d schemas with nothing matching, want names only", args, len(out.Tools))
		}
		if c.best != "" {
			if len(out.Tools) == 0 || len(out.Tools) > helperBestMatches || out.Tools[0].Name != c.best {
				t.Fatalf("search %v detailed %d tools, want up to %d led by %s, got first %q", args, len(out.Tools), helperBestMatches, c.best, firstToolName(out))
			}
			for _, tool := range out.Tools {
				if tool.Schema == nil || len(tool.Reference) != 34 {
					t.Fatalf("search %v: best match %s lacks a schema or reference", args, tool.Name)
				}
			}
		}
		if n := strings.Count(out.text, "durable development workspaces"); n != 1 {
			t.Fatalf("search %v sent the app description %d times, want once", args, n)
		}
		if !strings.Contains(out.Note, "search again with tools") {
			t.Fatalf("search %v did not say how to get a reference: %q", args, out.Note)
		}
	}
}

func TestHelperAppToolSearchReturnsReferencesForNamedTools(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)
	out := f.searchWith(t, map[string]any{"query": "create workspace", "tools": []any{"workspaces_create", "workspaces_list"}})
	if len(out.Tools) != 2 || out.Tools[0].Name != "workspaces_create" || out.Tools[1].Name != "workspaces_list" {
		t.Fatalf("named tools not returned: %s", out.text)
	}
	for _, tool := range out.Tools {
		if tool.Schema == nil || len(tool.Reference) != 34 || tool.App != "workspaces" {
			t.Fatalf("named tool %s lacks schema or reference: %s", tool.Name, out.text)
		}
	}
	result := f.request(t, "app_tool_call", map[string]any{"reference": out.Tools[0].Reference, "arguments": map[string]any{"name": "first"}})
	if result["isError"] == true || len(wide.called) != 1 || wide.called[0] != "workspaces_create" {
		t.Fatalf("call through a named reference failed: %v called=%v", result, wide.called)
	}

	// A tool named exactly in the query needs no second search.
	for _, query := range []string{"workspaces_create", "run workspaces_list now"} {
		out = f.searchWith(t, map[string]any{"query": query})
		want := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(query, "run "), " now"))[0]
		if len(out.Tools) != 1 || out.Tools[0].Name != want || out.Tools[0].Reference == "" {
			t.Fatalf("query %q did not return %s: %s", query, want, out.text)
		}
		if len(out.app("workspaces")) != len(wide.names) {
			t.Fatalf("query %q dropped the app listing", query)
		}
	}
}

func TestHelperAppToolSearchRespectsAllowedTools(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)
	allowed, _ := json.Marshal(append([]string{}, wide.names[5:]...))
	if _, err := f.s.store.db.Exec(`UPDATE mcp_servers SET allowed_tools=? WHERE upstream_id=?`, string(allowed), appMCPUpstreamID(wide.install)); err != nil {
		t.Fatal(err)
	}
	out := f.searchWith(t, map[string]any{"query": "create workspace", "tools": []any{"workspaces_create", "workspace_internal_sync", "workspaces_list", "no_such_tool"}})
	listed := strings.Join(out.app("workspaces"), ",")
	if strings.Contains(listed, "workspaces_create") || strings.Contains(listed, "preview") || !strings.Contains(listed, "workspaces_list") {
		t.Fatalf("listing ignores the install's enabled tools: %s", listed)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "workspaces_list" {
		t.Fatalf("disabled or hidden tools got a reference: %s", out.text)
	}
	if strings.Join(out.NotFound, ",") != "no_such_tool,workspace_internal_sync,workspaces_create" {
		t.Fatalf("tools_not_found=%v", out.NotFound)
	}
}

func TestHelperAppToolSearchAcrossApps(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)

	// Both apps match "list"; the one touching more of the query comes first
	// and neither loses tools to the other.
	out := f.searchWith(t, map[string]any{"query": "list workspaces"})
	if len(out.Apps) != 2 || out.Apps[0].App != "workspaces" || out.Apps[1].App != "tickets" {
		t.Fatalf("apps not ordered by query coverage: %s", out.text)
	}
	if out.Apps[0].ToolCount != len(wide.names) || strings.Join(out.app("tickets"), ",") != "tickets_list" {
		t.Fatalf("an app lost tools in a cross-app search: %s", out.text)
	}

	// Apps the query does not touch are named, so the Helper can ask for them.
	out = f.searchWith(t, map[string]any{"query": "preview"})
	if len(out.Apps) != 1 || out.Apps[0].App != "workspaces" || len(out.Others) != 1 || out.Others[0].App != "tickets" {
		t.Fatalf("unmatched apps not reported: %s", out.text)
	}

	// A wrong slug lists what is installed instead of an empty answer.
	out = f.searchWith(t, map[string]any{"query": "create workspace", "app": "workspace"})
	if len(out.Apps) != 0 || len(out.Others) != 2 {
		t.Fatalf("unknown app slug did not list installed apps: %s", out.text)
	}

	// A small listing still returns schemas and references in one call.
	out = f.searchWith(t, map[string]any{"query": "tickets", "app": "tickets"})
	if len(out.Tools) != 1 || out.Tools[0].Name != "tickets_list" || out.Tools[0].Reference == "" {
		t.Fatalf("small app needs a second search: %s", out.text)
	}
}

func TestHelperAppToolSearchBoundsNamedTools(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)
	for name, tools := range map[string]any{
		"too many":   []any{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
		"not a list": "workspaces_create",
		"not names":  []any{7},
		"blank name": []any{" "},
	} {
		result := f.request(t, "app_tool_search", map[string]any{"query": "create workspace", "tools": tools})
		if result["isError"] != true {
			t.Fatalf("%s accepted: %v", name, result)
		}
	}
	ten := []any{}
	for _, tool := range wide.names[:helperToolDetailLimit] {
		ten = append(ten, tool)
	}
	out := f.searchWith(t, map[string]any{"query": "workspace", "tools": ten})
	if len(out.Tools) != helperToolDetailLimit || len(out.NotFound) != 0 || len(out.Omitted) != 0 {
		t.Fatalf("ten named tools not all returned: tools=%d not_found=%v omitted=%v", len(out.Tools), out.NotFound, out.Omitted)
	}
}

func helperCallErrorText(t *testing.T, result map[string]any) string {
	t.Helper()
	if result["isError"] != true {
		t.Fatalf("call did not fail: %v", result)
	}
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

// The app's own reason for refusing a call reaches the Helper.
func TestHelperAppToolCallReturnsAppError(t *testing.T) {
	f := newHelperBrokerFixture(t)
	ref := f.search(t)

	f.callReply = func(w http.ResponseWriter, id any) {
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "ttl_minutes must be between 1 and 480"}})
	}
	text := helperCallErrorText(t, f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{"limit": 5}}))
	if !strings.Contains(text, "app error: ttl_minutes must be between 1 and 480") {
		t.Fatalf("error text = %q, want the app's message", text)
	}

	// An HTTP refusal keeps its status and gains the reason.
	f.callReply = func(w http.ResponseWriter, id any) {
		http.Error(w, "profile apteva is not configured with an image", http.StatusBadRequest)
	}
	text = helperCallErrorText(t, f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{"limit": 5}}))
	if !strings.Contains(text, "HTTP 400") || !strings.Contains(text, "profile apteva is not configured with an image") {
		t.Fatalf("error text = %q, want status and reason", text)
	}

	// A reply with neither result nor error keeps the generic message.
	f.callReply = func(w http.ResponseWriter, id any) { writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id}) }
	text = helperCallErrorText(t, f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{"limit": 5}}))
	if !strings.Contains(text, "app call failed") {
		t.Fatalf("error text = %q, want generic failure", text)
	}
}

func TestHelperAppErrorTextIsBoundedPlainText(t *testing.T) {
	got := helperAppErrorText("bad\n\tvalue\x00 here \x1b[31m" + strings.Repeat("é", 600))
	if strings.ContainsAny(got, "\n\t\x00\x1b") {
		t.Fatalf("control characters kept: %q", got)
	}
	if !strings.HasPrefix(got, "bad value here [31m") || !strings.HasSuffix(got, "…") {
		t.Fatalf("unexpected text: %q", got)
	}
	if len(got) > helperAppErrorLimit+len("…") || !utf8.ValidString(got) {
		t.Fatalf("len=%d valid=%v, want at most %d bytes of valid text", len(got), utf8.ValidString(got), helperAppErrorLimit)
	}
}

// A task search is enough to call the tool: no second search for a reference.
func TestHelperAppToolCallAfterOneSearch(t *testing.T) {
	f := newHelperBrokerFixture(t)
	wide := addHelperWideApp(t, f)
	out := f.searchWith(t, map[string]any{"query": "create workspace"})
	if len(out.Tools) == 0 || out.Tools[0].Name != "workspaces_create" {
		t.Fatalf("first search did not lead with workspaces_create, got %q", firstToolName(out))
	}
	ref := out.Tools[0].Reference
	// One reference serves several calls.
	for i := 0; i < 2; i++ {
		result := f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{"name": "demo"}})
		if result["isError"] == true {
			t.Fatalf("call %d with the first search's reference failed: %v", i+1, result)
		}
	}
	if strings.Join(wide.called, ",") != "workspaces_create,workspaces_create" {
		t.Fatalf("app received %v, want workspaces_create twice", wide.called)
	}
}

// A reference says when it expires, and an expired one says so.
func TestHelperAppToolReferenceLifetime(t *testing.T) {
	f := newHelperBrokerFixture(t)
	addHelperWideApp(t, f)
	out := f.searchWith(t, map[string]any{"query": "create workspace"})
	if len(out.Tools) == 0 {
		t.Fatalf("no detailed tool: %s", out.text)
	}
	expires, err := time.Parse(time.RFC3339, out.Tools[0].ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", out.Tools[0].ExpiresAt, err)
	}
	if left := time.Until(expires); left < helperReferenceTTL-time.Minute || left > helperReferenceTTL+time.Minute {
		t.Fatalf("reference expires in %v, want about %v", left, helperReferenceTTL)
	}
	if !strings.Contains(out.Note, "15 minutes") {
		t.Fatalf("note does not state the lifetime: %q", out.Note)
	}

	ref := out.Tools[0].Reference
	f.s.helperToolRefsMu.Lock()
	stored := f.s.helperToolRefs[ref]
	stored.Expires = time.Now().Add(-time.Minute).Unix()
	f.s.helperToolRefs[ref] = stored
	f.s.helperToolRefsMu.Unlock()
	// Another search must not make the expired reference look unknown.
	f.searchWith(t, map[string]any{"query": "list workspaces"})
	text := helperCallErrorText(t, f.request(t, "app_tool_call", map[string]any{"reference": ref, "arguments": map[string]any{}}))
	if !strings.Contains(text, "reference expired") || !strings.Contains(text, "search again") {
		t.Fatalf("expired reference error = %q", text)
	}
	text = helperCallErrorText(t, f.request(t, "app_tool_call", map[string]any{"reference": "r_" + strings.Repeat("0", 32), "arguments": map[string]any{}}))
	if !strings.Contains(text, "unknown reference") {
		t.Fatalf("unknown reference error = %q", text)
	}
}

func firstToolName(out helperSearchReply) string {
	if len(out.Tools) == 0 {
		return ""
	}
	return out.Tools[0].Name
}
