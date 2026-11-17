package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func pageToolsFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s, id := widgetSettingsFixture(t)
	app := s.installedApps.Get(id)
	app.Manifest.Provides.UIComponents[0].SupportedSizes = []string{"half", "full"}
	raw, _ := json.Marshal(app.Manifest)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	return s, id
}

func pageToolRequest(t *testing.T, s *Server, user int64, action string, args map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"action": action, "project_id": "p"}
	for key, value := range args {
		body[key] = value
	}
	req := authedRequest(t, http.MethodPost, "/ui-layout/tools", "", body)
	req.Header.Set("X-User-ID", itoa(user))
	rec := httptest.NewRecorder()
	s.handleWorkspacePageTool(rec, req)
	return rec
}

func readPageToolState(t *testing.T, rec *httptest.ResponseRecorder) workspacePageState {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var state workspacePageState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func seedToolPage(t *testing.T, s *Server, user int64, project, id string) {
	t.Helper()
	if _, _, err := s.store.mutateUIPage(user, project, workspacePage{ID: id, Title: id, Scope: "project", Kind: "custom"}, http.MethodPost); err != nil {
		t.Fatal(err)
	}
}

func TestPageMCPWidgetsPersistAndRetryWithoutDuplicates(t *testing.T) {
	s, _ := pageToolsFixture(t)
	seedToolPage(t, s, 1, "p", "report")
	seedToolPage(t, s, 1, "other", "private-page")
	legacy := json.RawMessage(`["dormant:legacy",{"id":"offline","component":"offline:metric","size":"full","settings":{"preserve":"old"},"future_field":true}]`)
	if _, _, err := s.store.PatchUserUILayoutSurface(1, "p", "page.report", legacy, nil); err != nil {
		t.Fatal(err)
	}
	state := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": "report"}))
	if !strings.Contains(string(state.Widgets[0]), `"id":"legacy:0:dormant:legacy"`) {
		t.Fatal("legacy widget identity does not match the dashboard canvas")
	}
	originalRevision := state.Revision
	add := map[string]any{"page_id": "report", "widget_id": "metric-one", "component": "metrics:metric", "settings": map[string]any{"count": 0, "enabled": false}, "revision": originalRevision}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", add))
	if !state.Changed || len(state.Widgets) != 3 {
		t.Fatalf("add result %+v", state)
	}
	retry := readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", add))
	if retry.Changed || retry.Revision != state.Revision || len(retry.Widgets) != 3 {
		t.Fatal("retry duplicated a widget or changed the revision")
	}
	add["settings"] = map[string]any{"count": 3}
	if rec := pageToolRequest(t, s, 1, "widget_add", add); rec.Code != 409 {
		t.Fatalf("conflicting instance ID accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := pageToolRequest(t, s, 1, "widget_configure", map[string]any{"page_id": "report", "widget_id": "metric-one", "revision": originalRevision, "size": "full"}); rec.Code != 409 {
		t.Fatal("stale write accepted")
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", map[string]any{"page_id": "report", "widget_id": "metric-two", "component": "metrics:metric", "settings": map[string]any{"count": 2}, "revision": state.Revision}))
	if len(state.Widgets) != 4 {
		t.Fatal("distinct instances of the same component were prevented")
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_move", map[string]any{"page_id": "report", "widget_id": "metric-two", "position": 0, "revision": state.Revision}))
	if !strings.Contains(string(state.Widgets[0]), `"metric-two"`) {
		t.Fatal("widget was not moved")
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_configure", map[string]any{"page_id": "report", "widget_id": "metric-one", "size": "full", "settings": map[string]any{"count": 4}, "revision": state.Revision}))
	var configured struct {
		ID, Size string
		Settings map[string]any
	}
	for _, raw := range state.Widgets {
		var widget struct{ ID string }
		json.Unmarshal(raw, &widget)
		if widget.ID == "metric-one" {
			json.Unmarshal(raw, &configured)
		}
	}
	if configured.Size != "full" || configured.Settings["count"] != float64(4) || configured.Settings["enabled"] != false {
		t.Fatalf("configuration lost typed values: %+v", configured)
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_remove", map[string]any{"page_id": "report", "widget_id": "metric-two", "revision": state.Revision}))
	if len(state.Widgets) != 3 {
		t.Fatal("remove affected another instance")
	}
	saved, _ := s.store.GetUserUILayoutWithRevision(1)
	for _, text := range []string{`"dormant:legacy"`, `"future_field":true`, `"private-page"`} {
		if !strings.Contains(string(saved), text) {
			t.Fatalf("unrelated data lost: %s", text)
		}
	}
	// Reopen the real storage to verify durable persistence through a restart.
	path := s.store.path
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s.store = reopened
	persisted := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": "report"}))
	if persisted.Revision != state.Revision || len(persisted.Widgets) != 3 {
		t.Fatal("saved page did not survive reopening")
	}
	if strings.Contains(pageToolRequest(t, s, 1, "pages_list", nil).Body.String(), "private-page") {
		t.Fatal("page listing exposed another workspace")
	}
}

func TestPageMCPValidationAndScope(t *testing.T) {
	s, id := pageToolsFixture(t)
	seedToolPage(t, s, 1, "p", "report")
	seedToolPage(t, s, 1, "other", "other-page")
	state := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": "report"}))
	before, _ := s.store.GetUserUILayoutWithRevision(1)
	for _, patch := range []map[string]any{
		{"component": "missing:widget"},
		{"component": "metrics:metric", "settings": map[string]any{"count": "bad"}},
		{"component": "metrics:metric", "settings": map[string]any{"count": 11}},
		{"component": "metrics:metric", "settings": []any{}},
		{"component": "metrics:metric", "size": "giant", "settings": map[string]any{"count": 1}},
		{"component": "metrics:metric", "install_id": id + 999, "settings": map[string]any{"count": 1}},
		{"component": "native:apps", "install_id": id},
		{"component": "native:apps", "placement": "invalid"},
		{"component": "native:apps", "position": -1},
		{"component": "native:apps", "user_id": 2},
	} {
		args := map[string]any{"page_id": "report", "widget_id": "invalid", "revision": state.Revision}
		for key, value := range patch {
			args[key] = value
		}
		if rec := pageToolRequest(t, s, 1, "widget_add", args); rec.Code != 400 {
			t.Fatalf("invalid placement accepted %+v: %d %s", patch, rec.Code, rec.Body.String())
		}
	}
	after, _ := s.store.GetUserUILayoutWithRevision(1)
	if string(before) != string(after) {
		t.Fatal("rejected mutation changed layout")
	}
	if rec := pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": "other-page"}); rec.Code != 404 {
		t.Fatal("cross-workspace page accepted")
	}
	if rec := pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": "no-revision", "component": "native:apps"}); rec.Code != 400 {
		t.Fatal("write without revision accepted")
	}
	user, err := s.store.CreateUser("separate-pages@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddProjectMember("p", user.ID, ProjectViewer, 1); err != nil {
		t.Fatal(err)
	}
	if rec := pageToolRequest(t, s, user.ID, "page_get", map[string]any{"page_id": "report"}); rec.Code != 404 {
		t.Fatal("another user's page was exposed")
	}
	if rec := pageToolRequest(t, s, user.ID, "pages_list", map[string]any{"project_id": "other"}); rec.Code != 403 {
		t.Fatalf("foreign workspace status %d", rec.Code)
	}
	if rec := pageToolRequest(t, s, 1, "pages_list", map[string]any{"scope": "global"}); rec.Code != 400 {
		t.Fatal("global operation accepted a project scope")
	}
}

func TestPageMCPRejectsUnsupportedEmbeddingAndOptionalInstallation(t *testing.T) {
	s, id := pageToolsFixture(t)
	app := s.installedApps.Get(id)
	app.Manifest.Provides.UIComponents[0].Slots = []string{"chat.message_attachment"}
	s.installedApps.Add(app)
	home := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", nil))
	if rec := pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": "bad-slot", "component": "metrics:metric", "settings": map[string]any{"count": 1}, "revision": home.Revision}); rec.Code != 400 {
		t.Fatal("non-embeddable component placed")
	}
	app.Manifest.Provides.UIComponents[0].Slots = []string{"dashboard.home"}
	s.installedApps.Add(app)
	var appID int64
	s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, id).Scan(&appID)
	manifest, _ := json.Marshal(app.Manifest)
	res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,manifest_json,installation_key,installation_name) VALUES(?,'p','running',?,'candidate','Candidate')`, appID, string(manifest))
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := res.LastInsertId()
	clone := *app
	clone.InstallID = candidate
	clone.MCPName = installationMCPName(app.AppName, "candidate", candidate)
	s.installedApps.Add(&clone)
	defaults, err := s.loadInstallationDefaults()
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps.defaults = defaults
	catalog := pageToolRequest(t, s, 1, "widgets_list", nil)
	if catalog.Code != 200 || strings.Contains(catalog.Body.String(), clone.MCPName+":") {
		t.Fatalf("normal discovery exposed advanced namespaces: %s", catalog.Body.String())
	}
	state := readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": "default", "component": "metrics:metric", "settings": map[string]any{"count": 1}, "revision": home.Revision}))
	state = readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": "specific", "component": "metrics:metric", "install_id": candidate, "settings": map[string]any{"count": 2}, "revision": state.Revision}))
	var target map[string]any
	json.Unmarshal(state.Widgets[1], &target)
	if target["component"] != clone.MCPName+":metric" || target["install_id"] != float64(candidate) {
		t.Fatalf("optional selector ignored: %+v", target)
	}
	// A stopped installation cannot be used even if its old UI metadata remains cached.
	s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, candidate)
	if rec := pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": "stopped", "component": "metrics:metric", "install_id": candidate, "settings": map[string]any{"count": 2}, "revision": state.Revision}); rec.Code != 400 {
		t.Fatal("stopped app placed")
	}
}

func TestPageMCPConflictingConcurrentUpdatesAndGlobalScope(t *testing.T) {
	s, _ := pageToolsFixture(t)
	state := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", nil))
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			rec := pageToolRequest(t, s, 1, "widget_add", map[string]any{"widget_id": fmt.Sprintf("concurrent-%d", index), "component": "native:apps", "revision": state.Revision})
			statuses <- rec.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent write results: %v", counts)
	}
	global := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"scope": "global", "project_id": ""}))
	global = readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", map[string]any{"scope": "global", "project_id": "", "widget_id": "global-inbox", "component": "native:apps", "revision": global.Revision}))
	if len(global.Widgets) != 1 {
		t.Fatal("global layout missing")
	}
	if rec := pageToolRequest(t, s, 1, "widget_configure", map[string]any{"scope": "global", "project_id": "", "widget_id": "global-inbox", "size": "full", "revision": global.Revision - 1}); rec.Code != 409 {
		t.Fatal("stale global edit accepted")
	}
	project := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", nil))
	if len(project.Widgets) != 1 {
		t.Fatal("global edit overwrote project layout")
	}
}

func TestPlatformMCPPageToolsUseTrustedScopeAndAreListed(t *testing.T) {
	s, _ := pageToolsFixture(t)
	s.instanceSecret = "page-tools-fixture-secret"
	helper, err := s.store.GetOrCreatePlatformHelper(1, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.BindAgentThreadScope(helper.ID, "pages-conversation", "p", 91); err != nil {
		t.Fatal(err)
	}
	called := false
	s.platformGatewayExec = func(_ context.Context, userID int64, project string, body []byte) ([]byte, error) {
		called = true
		if userID != 1 {
			t.Errorf("untrusted execution identity %d %s", userID, project)
		}
		var call struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.Unmarshal(body, &call)
		if call.Method == "tools/list" {
			return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": workspacePageToolDefinitions()}})
		}
		if project != "p" {
			t.Errorf("untrusted project %s", project)
		}
		if call.Params.Arguments["project_id"] != "p" || call.Params.Arguments["scope"] != "project" {
			t.Error("project scope was not enforced")
		}
		call.Params.Arguments["action"] = call.Params.Name
		rec := pageToolRequest(t, s, userID, call.Params.Name, call.Params.Arguments)
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": rec.Body.String()}}}})
	}
	invoke := func(method, tool string, args map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		if args == nil {
			args = map[string]any{}
		}
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"name": tool, "arguments": args}}
		if method == "tools/call" {
			args["_apteva_caller_thread"] = "pages-conversation"
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, helper)), strings.NewReader(string(raw)))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Apteva-Caller-Agent", itoa(helper.ID))
		if method == "tools/list" {
			req.Header.Set("X-Apteva-Caller-Thread", "pages-conversation")
		}
		rec := httptest.NewRecorder()
		s.handlePlatformMCP(rec, req)
		return rec
	}
	rec := invoke("tools/list", "", nil)
	for name := range workspacePageToolNames {
		if !strings.Contains(rec.Body.String(), `"name":"`+name+`"`) {
			t.Fatalf("tool missing from Helper catalog: %s", name)
		}
	}
	called = false
	rec = invoke("tools/call", "pages_list", map[string]any{"project_id": "other"})
	if !called || rec.Code != 200 {
		t.Fatalf("Page tools not dispatched: %d %s", rec.Code, rec.Body.String())
	}
	decode := func(rec *httptest.ResponseRecorder) workspacePageState {
		t.Helper()
		var result struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || len(result.Result.Content) != 1 {
			t.Fatalf("invalid Helper MCP response: %s", rec.Body.String())
		}
		var state workspacePageState
		if err := json.Unmarshal([]byte(result.Result.Content[0].Text), &state); err != nil || state.Page.ID == "" {
			t.Fatalf("invalid Helper Page result: %s", result.Result.Content[0].Text)
		}
		return state
	}
	created := decode(invoke("tools/call", "page_create", map[string]any{"title": "From Helper", "project_id": "other"}))
	if created.ProjectID != "p" || created.Page.Scope != "project" {
		t.Fatal("Helper page creation escaped trusted project")
	}
	updated := decode(invoke("tools/call", "page_update", map[string]any{"page_id": created.Page.ID, "revision": created.Revision, "title": "Edited by Helper"}))
	if updated.Page.Title != "Edited by Helper" {
		t.Fatal("Helper page edit was not dispatched")
	}
	called = false
	rec = invoke("tools/call", "pages_list", map[string]any{"scope": "global"})
	if called || !strings.Contains(rec.Body.String(), "project conversations can only") {
		t.Fatal("project conversation escaped to global scope")
	}
}

func TestPageMCPGatewayUsesAuthenticatedUserAPI(t *testing.T) {
	s, _ := pageToolsFixture(t)
	s.instanceSecret = "page-api-gateway-fixture"
	server := httptest.NewServer(s.authMiddleware(http.HandlerFunc(s.handleWorkspacePageTool)))
	defer server.Close()
	api := gatewayAPIClient{baseURL: server.URL, userID: 1, instanceSecret: s.instanceSecret}
	out, err := handleGatewayPageTool("pages_list", map[string]any{"project_id": "other", "_project_id": "other"}, "p", api)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"project_id":"p"`) {
		t.Fatalf("trusted default scope missing: %s", raw)
	}
	if _, err := handleGatewayPageTool("pages_list", map[string]any{"scope": "global"}, "p", api); err == nil {
		t.Fatal("gateway escaped project context")
	}
	if _, err := handleGatewayPageTool("page_get", map[string]any{"user_id": 2}, "p", api); err == nil {
		t.Fatal("caller selected another user's preferences")
	}
	created, err := handleGatewayPageTool("page_create", map[string]any{"title": "Gateway page", "project_id": "other"}, "p", api)
	if err != nil {
		t.Fatal(err)
	}
	createdJSON, _ := json.Marshal(created)
	var state workspacePageState
	if err := json.Unmarshal(createdJSON, &state); err != nil || state.ProjectID != "p" || state.Page.ID == "" {
		t.Fatalf("gateway page creation did not use trusted scope: %s", createdJSON)
	}
	if _, err := handleGatewayPageTool("page_update", map[string]any{"page_id": state.Page.ID, "revision": state.Revision, "title": "Gateway edited"}, "p", api); err != nil {
		t.Fatal(err)
	}
	searched, err := handleGatewayPageTool("widgets_list", map[string]any{"query": "NATIVE:APPS", "page_id": state.Page.ID}, "p", api)
	if err != nil {
		t.Fatal(err)
	}
	searchJSON, _ := json.Marshal(searched)
	var results struct {
		Widgets []dashboardWidgetDefinition `json:"widgets"`
	}
	if err := json.Unmarshal(searchJSON, &results); err != nil || len(results.Widgets) != 1 || results.Widgets[0].Component != "native:apps" {
		t.Fatalf("gateway search failed: %s", searchJSON)
	}
	for _, tool := range workspacePageToolDefinitions() {
		if tool.Name == "widget_add" && containsString(tool.InputSchema.Required, "install_id") {
			t.Fatal("normal widget placement requires an installation selector")
		}
	}
}

func TestPageMCPUnconfiguredPageMatchesEmptyDashboardCanvas(t *testing.T) {
	s, id := pageToolsFixture(t)
	s.installedApps.Get(id).Manifest.Provides.UIComponents[0].Suggested = true
	seedToolPage(t, s, 1, "p", "empty")
	state := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": "empty"}))
	if len(state.Widgets) != 0 || state.Explicit {
		t.Fatal("unconfigured page invented widgets absent from the dashboard")
	}
}

func TestPageMCPCreateAndPartialUpdatePreserveWidgetsAndMetadata(t *testing.T) {
	s, _ := pageToolsFixture(t)
	seedToolPage(t, s, 1, "other", "untouched")
	created := readPageToolState(t, pageToolRequest(t, s, 1, "page_create", map[string]any{
		"title": "  Operations  ", "description": "  Daily work  ", "pinned": true,
	}))
	page := created.Page
	if len(page.ID) != 32 || page.Title != "Operations" || page.Description != "Daily work" || !page.Pinned || page.Layout != "grid" || page.CreatedAt == "" || page.Kind != "custom" || page.Scope != "project" || !created.Changed {
		t.Fatalf("invalid created page: %+v", created)
	}
	state := readPageToolState(t, pageToolRequest(t, s, 1, "widget_add", map[string]any{
		"page_id": page.ID, "revision": created.Revision, "widget_id": "apps", "component": "native:apps",
	}))
	// Simulate future metadata saved by a newer dashboard client.
	raw, rev := s.store.GetUserUILayoutWithRevision(1)
	raw = json.RawMessage(strings.Replace(string(raw), `"title":"Operations"`, `"title":"Operations","future_metadata":{"preserve":true}`, 1))
	if _, err := s.store.db.Exec(`UPDATE user_preferences SET ui_layout=? WHERE user_id=1 AND ui_layout_revision=?`, string(raw), rev); err != nil {
		t.Fatal(err)
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "page_update", map[string]any{
		"page_id": page.ID, "revision": state.Revision, "title": "Renamed", "layout": "workspace",
	}))
	if state.Page.Title != "Renamed" || state.Page.Layout != "workspace" || state.Page.Description != page.Description || !state.Page.Pinned || state.Page.CreatedAt != page.CreatedAt || len(state.Widgets) != 1 {
		t.Fatalf("partial update lost metadata or widgets: %+v", state)
	}
	if stale := pageToolRequest(t, s, 1, "page_update", map[string]any{"page_id": page.ID, "revision": created.Revision, "title": "Stale"}); stale.Code != 409 {
		t.Fatalf("stale page edit status=%d", stale.Code)
	}
	state = readPageToolState(t, pageToolRequest(t, s, 1, "page_update", map[string]any{
		"page_id": page.ID, "revision": state.Revision, "pinned": false, "description": "",
	}))
	if state.Page.Pinned || state.Page.Description != "" || state.Page.Title != "Renamed" || state.Page.Layout != "workspace" {
		t.Fatal("false/empty metadata edits were not applied independently")
	}
	noop := readPageToolState(t, pageToolRequest(t, s, 1, "page_update", map[string]any{
		"page_id": page.ID, "revision": state.Revision, "title": state.Page.Title,
	}))
	if noop.Changed || noop.Revision != state.Revision {
		t.Fatal("unchanged page edit rewrote the layout")
	}
	saved, _ := s.store.GetUserUILayoutWithRevision(1)
	for _, expected := range []string{`"future_metadata":{"preserve":true}`, `"untouched"`, `"native:apps"`} {
		if !strings.Contains(string(saved), expected) {
			t.Fatalf("page mutation lost %s", expected)
		}
	}
	// Page readers see the same persisted metadata and layout.
	listed := pageToolRequest(t, s, 1, "pages_list", nil)
	if !strings.Contains(listed.Body.String(), `"title":"Renamed"`) || strings.Contains(listed.Body.String(), "untouched") {
		t.Fatalf("page list is not scoped: %s", listed.Body.String())
	}
	reloaded := readPageToolState(t, pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": page.ID}))
	if reloaded.Page != state.Page || len(reloaded.Widgets) != 1 {
		t.Fatal("page metadata and widgets were not persisted together")
	}
}

func TestPageMCPCreateUpdateValidationAndScope(t *testing.T) {
	s, _ := pageToolsFixture(t)
	for _, fields := range []map[string]any{
		{}, {"title": " "}, {"title": strings.Repeat("x", 161)}, {"title": "Test", "description": strings.Repeat("x", 2001)},
		{"title": "Test", "layout": "unknown"}, {"title": false}, {"title": "Test", "page_id": "chosen-id"}, {"title": "Test", "user_id": 2},
	} {
		if rec := pageToolRequest(t, s, 1, "page_create", fields); rec.Code != 400 {
			t.Fatalf("invalid page create accepted: %v HTTP %d", fields, rec.Code)
		}
	}
	created := readPageToolState(t, pageToolRequest(t, s, 1, "page_create", map[string]any{"title": "Private"}))
	for _, fields := range []map[string]any{
		{"revision": created.Revision, "title": "Home rename"},
		{"page_id": "dashboard.home", "revision": created.Revision, "title": "Home rename"},
		{"page_id": created.Page.ID, "title": "No revision"},
		{"page_id": created.Page.ID, "revision": created.Revision, "title": ""},
		{"page_id": created.Page.ID, "revision": created.Revision, "layout": ""},
	} {
		if rec := pageToolRequest(t, s, 1, "page_update", fields); rec.Code != 400 {
			t.Fatalf("invalid page edit accepted: %v HTTP %d", fields, rec.Code)
		}
	}
	seedToolPage(t, s, 1, "other", "foreign")
	if rec := pageToolRequest(t, s, 1, "page_update", map[string]any{"page_id": "foreign", "title": "Cross-project", "revision": created.Revision}); rec.Code != 404 {
		t.Fatal("foreign page edited through current project")
	}
	user, err := s.store.CreateUser("page-edit@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddProjectMember("p", user.ID, ProjectViewer, 1); err != nil {
		t.Fatal(err)
	}
	if rec := pageToolRequest(t, s, user.ID, "page_update", map[string]any{"page_id": created.Page.ID, "title": "Another user", "revision": created.Revision}); rec.Code != 404 {
		t.Fatal("another user's personal page was edited")
	}
	if rec := pageToolRequest(t, s, user.ID, "page_create", map[string]any{"project_id": "other", "title": "Forbidden"}); rec.Code != 403 {
		t.Fatal("page created without project access")
	}
	global := readPageToolState(t, pageToolRequest(t, s, 1, "page_create", map[string]any{"project_id": "", "scope": "global", "title": "Global", "layout": "workspace"}))
	global = readPageToolState(t, pageToolRequest(t, s, 1, "page_update", map[string]any{"project_id": "", "scope": "global", "page_id": global.Page.ID, "revision": global.Revision, "title": "Global renamed"}))
	if global.Page.Title != "Global renamed" || global.Page.Scope != "global" || global.Page.Layout != "workspace" {
		t.Fatal("global page edit failed")
	}
	if rec := pageToolRequest(t, s, 1, "page_get", map[string]any{"page_id": global.Page.ID}); rec.Code != 404 {
		t.Fatal("global page leaked into project scope")
	}
}

func TestPageMCPConcurrentMetadataEditsConflict(t *testing.T) {
	s, _ := pageToolsFixture(t)
	created := readPageToolState(t, pageToolRequest(t, s, 1, "page_create", map[string]any{"title": "Concurrent"}))
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses <- pageToolRequest(t, s, 1, "page_update", map[string]any{"page_id": created.Page.ID, "revision": created.Revision, "title": fmt.Sprintf("Edit %d", i)}).Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for code := range statuses {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("conflicting page edits: %v", counts)
	}
}

func TestPageMCPWidgetListSearch(t *testing.T) {
	s, id := pageToolsFixture(t)
	component := &s.installedApps.Get(id).Manifest.Provides.UIComponents[0]
	component.Label, component.Description = "Daily summary", "Track throughput"
	list := func(args map[string]any) []dashboardWidgetDefinition {
		t.Helper()
		rec := pageToolRequest(t, s, 1, "widgets_list", args)
		if rec.Code != 200 {
			t.Fatalf("widget search HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var response struct {
			Widgets []dashboardWidgetDefinition `json:"widgets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Widgets
	}
	all := list(nil)
	if len(list(map[string]any{"query": "  "})) != len(all) {
		t.Fatal("blank query changed the normal widget list")
	}
	for _, query := range []string{"  METRICS  ", "metrics:metric", "DAILY SUMMARY", "throughput"} {
		found := list(map[string]any{"query": query})
		if len(found) != 1 || found[0].Component != "metrics:metric" || found[0].SettingsSchema == nil {
			t.Fatalf("query %q result: %+v", query, found)
		}
	}
	if len(list(map[string]any{"query": "no-such-widget"})) != 0 || len(list(map[string]any{"query": "no-such-widget", "install_id": id})) != 0 {
		t.Fatal("no-match query did not return an empty list")
	}
	if len(list(map[string]any{"query": "native:apps"})) != 1 {
		t.Fatal("builtin widget was not searchable")
	}
	if len(list(map[string]any{"query": "metrics", "project_id": "other"})) != 0 {
		t.Fatal("search revealed another project's app widgets")
	}
	if rec := pageToolRequest(t, s, 1, "widgets_list", map[string]any{"query": "", "install_id": id + 999}); rec.Code != 404 {
		t.Fatal("search bypassed installation visibility")
	}
	for _, tool := range workspacePageToolDefinitions() {
		if tool.Name == "widgets_list" && (tool.InputSchema.Properties["query"].Type != "string" || containsString(tool.InputSchema.Required, "query")) {
			t.Fatal("widget search is not an optional list parameter")
		}
		if tool.Name == "widgets_search" || tool.Name == "widget_search" {
			t.Fatal("search added an unnecessary separate tool")
		}
	}
}
