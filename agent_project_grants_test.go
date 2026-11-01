package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

type grantFixture struct {
	s                     *Server
	owner                 int64
	agent                 *Agent
	home, shopA, shopB    string
	shopC                 string
	billing, shopCBilling int64
	mu                    sync.Mutex
	calls                 []grantSidecarCall
	tools                 []string
}

type grantSidecarCall struct {
	install int64
	project string
	query   string // the project_id query parameter the app received
	method  string
	tool    string
	args    map[string]any
}

// newGrantFixture builds a "Management" project whose agent has the global
// billing install attached, plus three other projects: Shop A (owner is
// editor), Shop B (owner is only a viewer) and Shop C (owner is editor, but
// the project has its own billing install).
func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	user, err := s.store.CreateUser("grant-owner@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	f := &grantFixture{s: s, owner: user.ID, tools: []string{"invoices_list", "invoice_send"}}
	project := func(name string, role ProjectRole) string {
		p, err := s.store.CreateProject(1, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.AddProjectMember(p.ID, user.ID, role, 1); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	f.home = project("Management", ProjectEditor)
	f.shopA = project("Shop A", ProjectEditor)
	f.shopB = project("Shop B", ProjectViewer)
	f.shopC = project("Shop C", ProjectEditor)
	f.agent, err = s.store.CreateAgent(user.ID, "Manager", "manage", "autonomous", `{"include_apteva_server":true}`, f.home)
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps = NewInstalledAppsRegistry()
	f.billing = f.attach(t, "billing", "")
	res, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by)
		SELECT app_id, ?, 'running', 1 FROM app_installs WHERE id=?`, f.shopC, f.billing)
	if err != nil {
		t.Fatal(err)
	}
	f.shopCBilling, _ = res.LastInsertId()
	f.sidecar(t, "billing", f.shopCBilling, f.shopC)
	return f
}

// attach installs app (globally when project is "") and attaches it to the
// agent behind a recording sidecar.
func (f *grantFixture) attach(t *testing.T, app, project string) int64 {
	t.Helper()
	install := seedAppWithTools(t, f.s, app, project, f.tools)
	if _, err := f.s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, install, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	f.sidecar(t, app, install, project)
	return install
}

func (f *grantFixture) sidecar(t *testing.T, app string, installID int64, project string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		f.mu.Lock()
		f.calls = append(f.calls, grantSidecarCall{installID, r.Header.Get("X-Apteva-Project-ID"), r.URL.Query().Get("project_id"), rpc.Method, rpc.Params.Name, rpc.Params.Arguments})
		f.mu.Unlock()
		// Calls answer with JSON naming the project they ran in, or fail
		// where the arguments ask them to.
		project := r.Header.Get("X-Apteva-Project-ID")
		reply, _ := json.Marshal(map[string]any{"project": project, "tool": rpc.Params.Name})
		var result any = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(reply)}}}
		if fail, _ := rpc.Params.Arguments["fail_in"].(string); fail != "" && fail == project {
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "error: no ledger here"}}}
		}
		if rpc.Method == "tools/list" {
			list := []any{}
			for _, name := range f.tools {
				tool := map[string]any{"name": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"project_id": map[string]any{"type": "string"}}}}
				if name == "invoices_list" {
					tool["annotations"] = map[string]any{"readOnlyHint": true}
				}
				list = append(list, tool)
			}
			result = map[string]any{"tools": list}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	f.s.installedApps.Add(&InstalledApp{InstallID: installID, AppName: app, ProjectID: project, SidecarURL: server.URL})
}

// call sends what Core sends on the agent's billing attachment: the signed
// agent identity in the URL and the attachment's app credential.
func (f *grantFixture) call(t *testing.T, signed bool, method, tool string, args map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return f.callApp(t, "billing", f.billing, signed, method, tool, args)
}

func (f *grantFixture) callApp(t *testing.T, app string, install int64, signed bool, method, tool string, args map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	path := "/apps/" + app + "/mcp"
	query := url.Values{"install_id": {itoa64(install)}, "project_id": {f.home}}
	if signed {
		query.Set("file_agent", itoa64(f.agent.ID))
		query.Set("file_auth", fileCallerSignature(f.s.instanceSecret, f.agent.ID, "/api"+path, itoa64(install)))
	}
	rpc := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if method == "tools/call" {
		if args == nil {
			args = map[string]any{}
		}
		rpc["params"] = map[string]any{"name": tool, "arguments": args}
	}
	body, _ := json.Marshal(rpc)
	req := httptest.NewRequest(http.MethodPost, path+"?"+query.Encode(), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("X-Apteva-App-Install-ID", itoa64(install))
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(f.agent.ID))
	rec := httptest.NewRecorder()
	f.s.handleAppProxy(rec, req)
	return rec
}

func (f *grantFixture) grant(t *testing.T, g AgentProjectGrant) {
	t.Helper()
	g.AgentID, g.Target = f.agent.ID, "billing"
	if g.CreatedBy == 0 {
		g.CreatedBy = f.owner
	}
	if err := f.s.store.SaveAgentProjectGrant(g); err != nil {
		t.Fatal(err)
	}
}

func (f *grantFixture) lastCall(t *testing.T) grantSidecarCall {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("sidecar was not called")
	}
	return f.calls[len(f.calls)-1]
}

func listedProjectParam(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var reply struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode tools/list: %v %s", err, rec.Body.String())
	}
	// The fixture's tools declare project_id without a description, like
	// several apps do; only a grant describes it.
	out := map[string]string{}
	for _, tool := range reply.Result.Tools {
		if description, _ := tool.InputSchema.Properties[grantProjectParam]["description"].(string); description != "" {
			out[tool.Name] = description
		}
	}
	return out
}

func TestAgentWithoutGrantKeepsTodaysRouting(t *testing.T) {
	f := newGrantFixture(t)

	rec := f.call(t, true, "tools/list", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := listedProjectParam(t, rec); len(got) != 0 {
		t.Fatalf("tool schemas changed without a grant: %v", got)
	}

	// Without a grant project_id means nothing to the platform: the call
	// stays home and reaches the app exactly as sent.
	rec = f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := f.lastCall(t)
	if got.install != f.billing || got.project != f.home || got.args[grantProjectParam] != "Shop A" {
		t.Fatalf("ungranted call changed: %+v", got)
	}
}

func TestGrantedAgentRunsAppToolInNamedProject(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})

	rec := f.call(t, true, "tools/list", "", nil)
	params := listedProjectParam(t, rec)
	if len(params) != 2 || !strings.Contains(params["invoices_list"], "Shop A ("+f.shopA+")") || !strings.Contains(params["invoices_list"], "(Management)") {
		t.Fatalf("project parameter not listed: %v", params)
	}
	if strings.Contains(params["invoices_list"], "Shop B") {
		t.Fatalf("ungranted project listed: %s", params["invoices_list"])
	}

	// No parameter: today's behaviour.
	if rec := f.call(t, true, "tools/call", "invoices_list", nil); rec.Code != http.StatusOK {
		t.Fatalf("home status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := f.lastCall(t); got.install != f.billing || got.project != f.home {
		t.Fatalf("home call rerouted: %+v", got)
	}

	// By name, any case, and by id.
	for _, value := range []string{"shop a", f.shopA} {
		rec := f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: value})
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "isError") {
			t.Fatalf("granted call %q status=%d body=%s", value, rec.Code, rec.Body.String())
		}
		got := f.lastCall(t)
		if got.install != f.billing || got.project != f.shopA || got.query != f.shopA || got.args["_project_id"] != f.shopA {
			t.Fatalf("granted call %q not routed to Shop A: %+v", value, got)
		}
		if _, leaked := got.args[grantProjectParam]; leaked {
			t.Fatal("project parameter reached the app")
		}
	}

	// Naming the home project is the same as omitting it.
	f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Management"})
	if got := f.lastCall(t); got.install != f.billing || got.project != f.home || got.args[grantProjectParam] != nil {
		t.Fatalf("home by name rerouted: %+v", got)
	}
}

func TestGrantRefusesProjectsOutsideTheGrantOrRoles(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	refused := func(value, want string) {
		t.Helper()
		before := len(f.calls)
		rec := f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: value})
		if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%q: body=%s", value, rec.Body.String())
		}
		if len(f.calls) != before {
			t.Fatalf("%q reached a sidecar: %+v", value, f.lastCall(t))
		}
	}
	// The refusal names what the agent can use instead.
	refused("Shop B", "It can use Shop A ("+f.shopA+"), or omit project_id for its own project.")
	refused("Shop Z", "cannot run tools in project")

	// All projects still stops where the owner may only view.
	f.grant(t, AgentProjectGrant{AllProjects: true})
	refused(f.shopB, "cannot run tools in project")

	// Shop C has its own billing install, so its billing data is not the
	// global install's to reach.
	refused("Shop C", "cannot run tools in project")
	if params := listedProjectParam(t, f.call(t, true, "tools/list", "", nil)); strings.Contains(params["invoices_list"], "Shop C") {
		t.Fatalf("project with its own install listed: %s", params["invoices_list"])
	}

	// A grant written by someone without access to Shop A carries nothing there.
	outsider, err := f.s.store.CreateUser("grant-outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}, CreatedBy: outsider.ID})
	refused("Shop A", "cannot run tools in project")

	// Losing the role takes effect on the next call.
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	if err := f.s.store.UpdateProjectMember(f.shopA, f.owner, ProjectViewer); err != nil {
		t.Fatal(err)
	}
	refused("Shop A", "cannot run tools in project")
}

func TestGrantLimitedToChosenTools(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}, Tools: []string{"invoices_list"}})

	params := listedProjectParam(t, f.call(t, true, "tools/list", "", nil))
	if _, ok := params["invoice_send"]; ok || params["invoices_list"] == "" {
		t.Fatalf("tool limit not reflected in schemas: %v", params)
	}
	rec := f.call(t, true, "tools/call", "invoice_send", map[string]any{grantProjectParam: "Shop A"})
	if !strings.Contains(rec.Body.String(), `not granted outside this agent's project`) {
		t.Fatalf("ungranted tool body=%s", rec.Body.String())
	}
	f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if got := f.lastCall(t); got.project != f.shopA {
		t.Fatalf("granted tool not routed: %+v", got)
	}
}

func TestGrantNeedsTheSignedAgentIdentity(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})

	// The caller header alone can be sent by anything holding the app token.
	rec := f.call(t, false, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := f.lastCall(t); got.install != f.billing || got.project != f.home {
		t.Fatalf("unsigned caller was rerouted: %+v", got)
	}
	if got := listedProjectParam(t, f.call(t, false, "tools/list", "", nil)); len(got) != 0 {
		t.Fatalf("unsigned caller saw grant schemas: %v", got)
	}

	// A detached app is no longer the agent's to use elsewhere.
	if _, err := f.s.store.db.Exec(`UPDATE app_agent_bindings SET enabled=0 WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if got := f.lastCall(t); got.project == f.shopA {
		t.Fatalf("detached app was rerouted: %+v", got)
	}
}

// Attaching an app pushes the config to Core, which lists the tools before
// the server records the binding. That first listing must already describe
// the granted projects, or the agent never learns it can use them.
func TestGrantListingDoesNotWaitForTheBinding(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	if _, err := f.s.store.db.Exec(`DELETE FROM app_agent_bindings WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if got := listedProjectParam(t, f.call(t, true, "tools/list", "", nil)); !strings.Contains(got["invoices_list"], "Shop A") {
		t.Fatalf("listing before the binding lacks the granted project: %v", got)
	}
}

func TestRemovingGrantRestoresDefault(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	if err := f.s.store.DeleteAgentProjectGrant(f.agent.ID, "billing"); err != nil {
		t.Fatal(err)
	}
	f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if got := f.lastCall(t); got.install != f.billing || got.project != f.home {
		t.Fatalf("removed grant still routes: %+v", got)
	}
}

func TestGrantNeverAppliesToProjectInstalls(t *testing.T) {
	f := newGrantFixture(t)
	ledger := f.attach(t, "ledger", f.home)
	// Even a grant stored directly does nothing through a project install.
	if err := f.s.store.SaveAgentProjectGrant(AgentProjectGrant{AgentID: f.agent.ID, Target: "ledger", AllProjects: true, CreatedBy: f.owner}); err != nil {
		t.Fatal(err)
	}
	if got := listedProjectParam(t, f.callApp(t, "ledger", ledger, true, "tools/list", "", nil)); len(got) != 0 {
		t.Fatalf("project install schemas changed: %v", got)
	}
	rec := f.callApp(t, "ledger", ledger, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: "Shop A"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := f.lastCall(t); got.install != ledger || got.project != f.home || got.args[grantProjectParam] != "Shop A" {
		t.Fatalf("project install call changed: %+v", got)
	}
}

func (f *grantFixture) platformCall(t *testing.T, tool string, args map[string]any) (string, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var gotProject string
	var gotArgs map[string]any
	f.s.platformGatewayExec = func(_ context.Context, _ int64, project string, request []byte) ([]byte, error) {
		gotProject = project
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(request, &rpc)
		gotArgs = rpc.Params.Arguments
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`), nil
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(f.s.instanceSecret, f.agent)), strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:43220"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(f.agent.ID))
	rec := httptest.NewRecorder()
	f.s.handlePlatformMCP(rec, req)
	return gotProject, gotArgs, rec
}

func TestPlatformToolsStayHomeWithoutGrant(t *testing.T) {
	f := newGrantFixture(t)
	project, args, rec := f.platformCall(t, "agents_list", map[string]any{"project_id": f.shopA})
	if rec.Code != http.StatusOK || project != f.home || args["project_id"] != f.home {
		t.Fatalf("ungranted platform call: project=%q args=%v status=%d body=%s", project, args, rec.Code, rec.Body.String())
	}
}

func TestGrantedPlatformToolsRunInNamedProject(t *testing.T) {
	f := newGrantFixture(t)
	if err := f.s.store.SaveAgentProjectGrant(AgentProjectGrant{AgentID: f.agent.ID, Target: platformMCPName, ProjectIDs: []string{f.shopA}, CreatedBy: f.owner}); err != nil {
		t.Fatal(err)
	}
	project, args, rec := f.platformCall(t, "agents_list", map[string]any{"project_id": "Shop A"})
	if rec.Code != http.StatusOK || project != f.shopA || args["project_id"] != f.shopA {
		t.Fatalf("granted platform call: project=%q args=%v body=%s", project, args, rec.Body.String())
	}
	project, _, _ = f.platformCall(t, "agents_list", nil)
	if project != f.home {
		t.Fatalf("omitted project_id left home: %q", project)
	}
	_, _, rec = f.platformCall(t, "agents_list", map[string]any{"project_id": f.shopB})
	if !strings.Contains(rec.Body.String(), "cannot run tools in project") {
		t.Fatalf("viewer-only project body=%s", rec.Body.String())
	}
}

func TestProjectGrantsAPI(t *testing.T) {
	f := newGrantFixture(t)
	f.attach(t, "ledger", f.home)
	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/instances/"+itoa64(f.agent.ID)+"/project-grants", strings.NewReader(body))
		req.Header.Set("X-User-ID", itoa64(f.owner))
		rec := httptest.NewRecorder()
		f.s.handleAgentProjectGrants(rec, req, f.agent.ID)
		return rec
	}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"target":"billing","project_ids":["` + f.home + `"]}`, http.StatusBadRequest},
		{`{"target":"billing","project_ids":["missing-project"]}`, http.StatusBadRequest},
		{`{"target":"billing","project_ids":["` + f.shopB + `"]}`, http.StatusForbidden},
		{`{"target":"billing","project_ids":["` + f.shopC + `"]}`, http.StatusBadRequest},
		{`{"target":"ledger","all_projects":true}`, http.StatusBadRequest},
		{`{"target":"no-such-app","all_projects":true}`, http.StatusBadRequest},
		{`{"target":"no-such-app","project_ids":[]}`, http.StatusOK},
	} {
		if rec := put(tc.body); rec.Code != tc.code {
			t.Fatalf("%s: status=%d want %d body=%s", tc.body, rec.Code, tc.code, rec.Body.String())
		}
	}
	rec := put(`{"target":"billing","project_ids":["` + f.shopA + `"],"tools":["invoices_list"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Grants   []AgentProjectGrant `json:"grants"`
		Projects []grantProject      `json:"projects"`
		Targets  map[string][]string `json:"targets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if len(reply.Grants) != 1 || reply.Grants[0].CreatedBy != f.owner || reply.Grants[0].Tools[0] != "invoices_list" {
		t.Fatalf("saved grants=%+v", reply.Grants)
	}
	names := []string{}
	for _, p := range reply.Projects {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "Shop A,Shop C" {
		t.Fatalf("grantable projects=%v", names)
	}
	// Only global installs are targets, and Shop C keeps its own billing.
	want := map[string]string{"billing": f.shopA, platformMCPName: f.shopA + "," + f.shopC}
	if len(reply.Targets) != len(want) {
		t.Fatalf("targets=%v", reply.Targets)
	}
	for target, ids := range want {
		if strings.Join(reply.Targets[target], ",") != ids {
			t.Fatalf("targets=%v", reply.Targets)
		}
	}
	if rec := put(`{"target":"billing","project_ids":[]}`); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"target":"billing"`) {
		t.Fatalf("empty grant should restore the default: %s", rec.Body.String())
	}
}

func TestSavingGrantReconnectsOnlyThatAppOnARunningAgent(t *testing.T) {
	f := newGrantFixture(t)
	billingURL := "http://127.0.0.1:5280/api/apps/billing/mcp?install_id=" + itoa64(f.billing)
	otherURL := "http://127.0.0.1:5280/api/apps/notes/mcp?install_id=99"
	current := map[string]any{"mcp_servers": []any{
		map[string]any{"name": "billing", "transport": "http", "url": billingURL},
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
	f.s.agents.processes[f.agent.ID] = &runningAgent{port: core.Listener.Addr().(*net.TCPAddr).Port, coreAPIKey: "core-key", reattached: true}

	req := httptest.NewRequest(http.MethodPut, "/instances/"+itoa64(f.agent.ID)+"/project-grants",
		strings.NewReader(`{"target":"billing","project_ids":["`+f.shopA+`"]}`))
	req.Header.Set("X-User-ID", itoa64(f.owner))
	rec := httptest.NewRecorder()
	f.s.handleAgentProjectGrants(rec, req, f.agent.ID)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "warning") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	servers := mcpMaps(current["mcp_servers"])
	if puts != 1 || !strings.Contains(servers[0]["url"].(string), "project_grant_rev=") || servers[1]["url"] != otherURL {
		t.Fatalf("puts=%d servers=%v", puts, servers)
	}
}

// fanOut sends invoices_list with project_id "*" and decodes the combined
// reply.
func (f *grantFixture) fanOut(t *testing.T, tool string, args map[string]any) (bool, []grantFanOutResult, []grantProject, string) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args[grantProjectParam] = grantAllProjects
	rec := f.call(t, true, "tools/call", tool, args)
	var reply struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Result.Content) == 0 {
		t.Fatalf("decode fan-out reply: %v %s", err, rec.Body.String())
	}
	var combined struct {
		Projects []grantFanOutResult `json:"projects"`
		Skipped  []grantProject      `json:"skipped"`
	}
	if json.Unmarshal([]byte(reply.Result.Content[0].Text), &combined) != nil {
		return reply.Result.IsError, nil, nil, reply.Result.Content[0].Text
	}
	return reply.Result.IsError, combined.Projects, combined.Skipped, reply.Result.Content[0].Text
}

func TestGrantFanOutRunsReadOnlyToolInEveryProject(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{AllProjects: true})

	params := listedProjectParam(t, f.call(t, true, "tools/list", "", nil))
	if !strings.Contains(params["invoices_list"], `Pass "*"`) || strings.Contains(params["invoice_send"], `Pass "*"`) {
		t.Fatalf("fan-out offered on the wrong tools: %v", params)
	}
	f.calls = nil

	// Shop B (viewer) and Shop C (own install) are out of reach.
	isError, results, skipped, _ := f.fanOut(t, "invoices_list", map[string]any{"status": "overdue"})
	if isError || len(results) != 2 || len(skipped) != 0 {
		t.Fatalf("isError=%v results=%+v skipped=%+v", isError, results, skipped)
	}
	for i, want := range []struct{ id, name string }{{f.home, "Management"}, {f.shopA, "Shop A"}} {
		got := results[i]
		reply, _ := got.Result.(map[string]any)
		if got.ProjectID != want.id || got.Project != want.name || got.Error != "" || reply["project"] != want.id {
			t.Fatalf("result %d = %+v", i, got)
		}
	}
	if len(f.calls) != 2 {
		t.Fatalf("sidecar calls=%+v", f.calls)
	}
	for _, call := range f.calls {
		if call.install != f.billing || call.args["status"] != "overdue" || call.args["_project_id"] != call.project {
			t.Fatalf("child call=%+v", call)
		}
		if _, leaked := call.args[grantProjectParam]; leaked {
			t.Fatalf("project_id reached the app: %+v", call)
		}
	}
}

func TestGrantFanOutKeepsGoingWhenAProjectFails(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	isError, results, _, _ := f.fanOut(t, "invoices_list", map[string]any{"fail_in": f.shopA})
	if isError || len(results) != 2 || results[0].Error != "" || results[1].Error != "no ledger here" {
		t.Fatalf("isError=%v results=%+v", isError, results)
	}
	isError, results, _, _ = f.fanOut(t, "invoices_list", map[string]any{"fail_in": f.home})
	if isError || results[0].Error == "" || results[1].Error != "" {
		t.Fatalf("isError=%v results=%+v", isError, results)
	}
}

func TestGrantFanOutRefusesToolsThatMayWrite(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(t, AgentProjectGrant{AllProjects: true})
	// No tools/list first: the server reads the tool list itself.
	isError, _, _, text := f.fanOut(t, "invoice_send", nil)
	if !isError || !strings.Contains(text, "not marked read-only") {
		t.Fatalf("isError=%v text=%s", isError, text)
	}
	f.grant(t, AgentProjectGrant{AllProjects: true, Tools: []string{"invoice_send"}})
	isError, _, _, text = f.fanOut(t, "invoices_list", nil)
	if !isError || !strings.Contains(text, "not granted outside") {
		t.Fatalf("isError=%v text=%s", isError, text)
	}
	if len(f.calls) != 1 || f.calls[0].method != "tools/list" {
		t.Fatalf("refused fan-out reached the app: %+v", f.calls)
	}
}

func TestGrantFanOutTrustsManifestReadOnly(t *testing.T) {
	f := newGrantFixture(t)
	f.tools = append(f.tools, "invoices_overdue")
	f.grant(t, AgentProjectGrant{ProjectIDs: []string{f.shopA}})
	// The app lists invoices_overdue without annotations; the installed
	// manifest declares it read-only.
	f.s.installedApps.Get(f.billing).Manifest.Provides.MCPTools = []sdk.MCPToolSpec{{Name: "invoices_overdue", ReadOnly: true}}

	isError, results, _, text := f.fanOut(t, "invoices_overdue", nil)
	if isError || len(results) != 2 {
		t.Fatalf("isError=%v text=%s", isError, text)
	}
	for _, c := range f.calls {
		if c.method == "tools/list" {
			t.Fatalf("manifest read_only still fetched the tool list: %+v", f.calls)
		}
	}

	params := listedProjectParam(t, f.call(t, true, "tools/list", "", nil))
	if !strings.Contains(params["invoices_overdue"], `"*"`) || strings.Contains(params["invoice_send"], `"*"`) {
		t.Fatalf("listing does not offer * from the manifest: %v", params)
	}
}

func TestStarWithoutGrantReachesTheAppUnchanged(t *testing.T) {
	f := newGrantFixture(t)
	f.call(t, true, "tools/call", "invoices_list", map[string]any{grantProjectParam: grantAllProjects})
	if got := f.lastCall(t); got.project != f.home || got.args[grantProjectParam] != grantAllProjects {
		t.Fatalf("ungranted * changed: %+v", got)
	}
}

func TestGrantFanOutStopsAt32Projects(t *testing.T) {
	f := newGrantFixture(t)
	for i := 0; i < grantFanOutLimit; i++ {
		p, err := f.s.store.CreateProject(1, "Store "+itoa64(int64(100+i)), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.store.AddProjectMember(p.ID, f.owner, ProjectEditor, 1); err != nil {
			t.Fatal(err)
		}
	}
	f.grant(t, AgentProjectGrant{AllProjects: true})
	_, results, skipped, _ := f.fanOut(t, "invoices_list", nil)
	// Management, Shop A and 32 stores: the last two by name are skipped.
	if len(results) != grantFanOutLimit || len(skipped) != 2 || results[0].Project != "Management" {
		t.Fatalf("results=%d skipped=%+v first=%+v", len(results), skipped, results[0])
	}
}
