package main

// Real-LLM checks for agent project grants, driven by OpenAI Codex through a
// real apteva-core. A finance agent in the Head Office project has a globally
// installed billing app attached:
//
//   - with a grant for two shops, it reads and writes invoices in a named shop,
//     reads every reachable project with one project_id "*" call, and cannot
//     touch a shop outside the grant;
//   - without a grant, naming another project's id in the app's own
//     project_id argument is refused before it reaches the app.
//
// Run: APTEVA_RUN_REAL_LLM_TESTS=1 go test -run 'RealLLM_Codex_.*Project' -v -timeout 20m .

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const realGrantDirective = `# Role
You are the finance assistant for the Northwind group of shops. You work from the Head Office project.
# Methods
Use the billing tools for invoice questions and report only what the tools return. Answer the person in the chat.`

type realGrantProjects struct {
	home, lyon, lille, nantes string
}

// realBillingApp is a billing sidecar with one ledger per project. Like
// several real apps, it trusts a project_id argument over the project the
// platform routed the call to, so a leak would show up as another project's
// invoices. Its tool list carries no annotations: invoices_search is declared
// read_only in the installed manifest only, which is what the platform reads.
type realBillingApp struct {
	mu       sync.Mutex
	invoices map[string][]map[string]any
	calls    []realBillingCall
}

type realBillingCall struct {
	routed   string // X-Apteva-Project-ID
	project  string // the ledger the app used
	toolCall string // X-Apteva-Tool-Call-ID
	tool     string
	args     map[string]any
}

func newRealBillingApp(p realGrantProjects) *realBillingApp {
	invoice := func(id, customer string, amount int, status, due string) map[string]any {
		return map[string]any{"id": id, "customer": customer, "amount_eur": amount, "status": status, "due": due}
	}
	return &realBillingApp{invoices: map[string][]map[string]any{
		p.home: {
			invoice("INV-H-001", "Harbor Lease Co", 1200, "overdue", "2026-10-05"),
			invoice("INV-H-002", "Office Supplies SA", 75, "paid", "2026-10-01"),
		},
		p.lyon: {
			invoice("INV-A-102", "Cafe Rive", 340, "overdue", "2026-10-12"),
			invoice("INV-A-103", "Marche Central", 90, "open", "2026-11-20"),
		},
		p.lille:  {invoice("INV-B-207", "Atelier Nord", 980, "overdue", "2026-10-18")},
		p.nantes: {invoice("INV-C-301", "Loire Imports", 410, "overdue", "2026-10-09")},
	}}
}

func (a *realBillingApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name            string         `json:"name"`
			Arguments       map[string]any `json:"arguments"`
			ProtocolVersion string         `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&rpc)
	if len(rpc.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch rpc.Method {
	case "initialize":
		version := rpc.Params.ProtocolVersion
		if version == "" {
			version = "2025-03-26"
		}
		result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "billing", "version": "1.0.0"},
		}
	case "tools/list":
		project := map[string]any{"type": "string", "description": "Project whose ledger to use. Defaults to the caller's project."}
		result = map[string]any{"tools": []any{
			map[string]any{
				"name":        "invoices_search",
				"description": "List the invoices in a project's ledger, optionally only those with one status.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"status":     map[string]any{"type": "string", "enum": []string{"open", "overdue", "paid", "all"}, "description": "Only invoices with this status. Defaults to all."},
					"project_id": project,
				}},
			},
			map[string]any{
				"name":        "invoice_mark_paid",
				"description": "Mark one invoice in a project's ledger as paid.",
				"inputSchema": map[string]any{"type": "object", "required": []string{"invoice_id"}, "properties": map[string]any{
					"invoice_id": map[string]any{"type": "string", "description": "Invoice id, such as INV-H-001."},
					"project_id": project,
				}},
			},
		}}
	case "tools/call":
		result = a.call(r, rpc.Params.Name, rpc.Params.Arguments)
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
}

func (a *realBillingApp) call(r *http.Request, tool string, args map[string]any) any {
	routed := r.Header.Get("X-Apteva-Project-ID")
	project, _ := args["project_id"].(string)
	if project == "" {
		project, _ = args["_project_id"].(string)
	}
	if project == "" {
		project = routed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, realBillingCall{routed, project, r.Header.Get("X-Apteva-Tool-Call-ID"), tool, args})
	text := func(v any) any {
		b, _ := json.Marshal(v)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
	}
	switch tool {
	case "invoices_search":
		status, _ := args["status"].(string)
		out := []map[string]any{}
		for _, inv := range a.invoices[project] {
			if status == "" || status == "all" || inv["status"] == status {
				out = append(out, inv)
			}
		}
		return text(map[string]any{"invoices": out})
	case "invoice_mark_paid":
		id, _ := args["invoice_id"].(string)
		for _, inv := range a.invoices[project] {
			if inv["id"] == id {
				inv["status"] = "paid"
				return text(map[string]any{"invoice_id": id, "status": "paid"})
			}
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "error: invoice " + id + " is not in this ledger"}}}
	}
	return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "error: unknown tool " + tool}}}
}

func (a *realBillingApp) snapshot() []realBillingCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]realBillingCall(nil), a.calls...)
}

func (a *realBillingApp) status(project, id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, inv := range a.invoices[project] {
		if inv["id"] == id {
			return inv["status"].(string)
		}
	}
	return ""
}

// prepareRealGrantFixture creates the four projects, moves the agent into
// Head Office and installs billing globally behind the fake sidecar. Every
// project belongs to the agent's owner, so only a grant decides where the
// agent can go.
func prepareRealGrantFixture(t *testing.T, s *Server, userID int64, agent *Agent) (realGrantProjects, *realBillingApp, int64) {
	t.Helper()
	project := func(name string) string {
		p, err := s.store.CreateProject(userID, name, "", "")
		if err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}
		return p.ID
	}
	p := realGrantProjects{home: project("Head Office"), lyon: project("Lyon Shop"), lille: project("Lille Shop"), nantes: project("Nantes Shop")}
	agent.ProjectID = p.home

	app := newRealBillingApp(p)
	sidecar := httptest.NewServer(app)
	t.Cleanup(sidecar.Close)
	install := seedAppWithTools(t, s, "billing", "", []string{"invoices_search", "invoice_mark_paid"})
	s.installedApps.Add(&InstalledApp{InstallID: install, AppName: "billing", SidecarURL: sidecar.URL,
		Manifest: sdk.Manifest{Name: "billing", Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{
			{Name: "invoices_search", ReadOnly: true},
			{Name: "invoice_mark_paid"},
		}}}})
	// The attachment URL is the loopback proxy on this test server's port.
	t.Setenv("PORT", s.port)
	if err := s.registerAppMCP(install); err != nil {
		t.Fatalf("register billing MCP: %v", err)
	}
	return p, app, install
}

// attachRealBilling attaches the install the way the dashboard does, which
// signs the agent's identity into the URL and hot-reloads Core.
func attachRealBilling(t *testing.T, h *realChannelChatHarness, install int64) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": "add", "bound_app_install_ids": []int64{install}})
	req := httptest.NewRequest(http.MethodPost, "/instances/"+itoa64(h.agent.ID)+"/mcp-servers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.server.authMiddleware(h.server.handleAgentMCPServers)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach billing status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bound int
	if err := h.server.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, install, h.agent.ID).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("billing not bound to the agent: count=%d err=%v", bound, err)
	}
}

// billingProjectParams lists billing's tools through the attachment URL Core
// was given and returns each tool's project_id description.
func billingProjectParams(t *testing.T, h *realChannelChatHarness) map[string]string {
	t.Helper()
	servers, err := h.server.currentAgentMCPServers(h.agent, h.server.agents.GetPort(h.agent.ID))
	if err != nil {
		t.Fatalf("read agent MCP servers: %v", err)
	}
	attachment := ""
	for _, srv := range servers {
		if srv["name"] == "billing" {
			attachment, _ = srv["url"].(string)
		}
	}
	if attachment == "" {
		t.Fatalf("billing is not in the agent's MCP servers: %v", servers)
	}
	resp, err := http.Post(attachment, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	defer resp.Body.Close()
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
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode tools/list (status %d): %v", resp.StatusCode, err)
	}
	out := map[string]string{}
	for _, tool := range reply.Result.Tools {
		out[tool.Name], _ = tool.InputSchema.Properties[grantProjectParam]["description"].(string)
	}
	return out
}

func latestChatMessageID(t *testing.T, h *realChannelChatHarness) int64 {
	t.Helper()
	var id int64
	if err := h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&id); err != nil {
		t.Fatalf("latest chat message: %v", err)
	}
	return id
}

// askAndWait posts one message and returns the agent's replies to it once one
// contains marker.
func askAndWait(t *testing.T, h *realChannelChatHarness, content, marker string) string {
	t.Helper()
	after := latestChatMessageID(t, h)
	h.post(t, content)
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		rows, err := h.server.store.db.Query(`SELECT content FROM channel_chat_messages WHERE chat_id=? AND role='agent' AND id>? ORDER BY id`, h.chatID, after)
		if err != nil {
			t.Fatalf("read replies: %v", err)
		}
		var replies []string
		for rows.Next() {
			var text string
			_ = rows.Scan(&text)
			replies = append(replies, text)
		}
		rows.Close()
		joined := strings.Join(replies, "\n")
		if strings.Contains(joined, marker) {
			t.Logf("agent reply (%s):\n%s", marker, joined)
			return joined
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no reply containing %s within 4 minutes", marker)
	return ""
}

// billingToolCalls returns the agent's billing calls since baseline, as Core
// reported them.
func billingToolCalls(t *testing.T, h *realChannelChatHarness, baseline map[string]bool) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baseline) {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		if strings.HasSuffix(data.Name, "invoices_search") || strings.HasSuffix(data.Name, "invoice_mark_paid") {
			out = append(out, map[string]any{"name": data.Name, "args": data.Args})
		}
	}
	return out
}

func callsSince(app *realBillingApp, n int) []realBillingCall {
	return app.snapshot()[n:]
}

func TestProjectGrants_RealLLM_Codex_AgentWorksInGrantedProjects(t *testing.T) {
	providerState := loadOpenAICodexProviderState(t)
	var (
		p       realGrantProjects
		app     *realBillingApp
		install int64
	)
	h := setupRealChannelChatHarnessWithProviderPrepared(t, "Northwind Finance", realGrantDirective,
		`{"include_channels":true}`, 15, "llm", "OpenAI Codex", providerState,
		func(s *Server, userID int64, agent *Agent) {
			p, app, install = prepareRealGrantFixture(t, s, userID, agent)
			if err := s.store.SaveAgentProjectGrant(AgentProjectGrant{
				AgentID: agent.ID, Target: "billing", ProjectIDs: []string{p.lyon, p.lille}, CreatedBy: userID,
			}); err != nil {
				t.Fatalf("save grant: %v", err)
			}
		})
	waitForInitialAgentTurn(t, h)
	attachRealBilling(t, h, install)
	params := billingProjectParams(t, h)
	t.Logf("project_id as the agent sees it: %v", params)
	if !strings.Contains(params["invoices_search"], "Lyon Shop") || !strings.Contains(params["invoices_search"], `"*"`) ||
		!strings.Contains(params["invoice_mark_paid"], "Lille Shop") || strings.Contains(params["invoice_mark_paid"], `"*"`) {
		t.Fatalf("billing tools do not describe the granted projects")
	}
	names := map[string]string{p.home: "Head Office", p.lyon: "Lyon Shop", p.lille: "Lille Shop", p.nantes: "Nantes Shop"}
	logCalls := func(calls []realBillingCall) {
		for _, c := range calls {
			t.Logf("  app call %s in %s (routed %s) call_id=%s args=%v", c.tool, names[c.project], names[c.routed], c.toolCall, c.args)
		}
	}

	// 1. One named shop.
	seen := len(app.snapshot())
	baseline := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	reply := askAndWait(t, h, "Which invoices are overdue at the Lyon Shop? List each invoice id with its amount, and end your reply with STEP-1-DONE.", "STEP-1-DONE")
	calls := callsSince(app, seen)
	logCalls(calls)
	t.Logf("  core calls: %v", billingToolCalls(t, h, baseline))
	reachedLyon := false
	for _, c := range calls {
		if c.project != c.routed {
			t.Errorf("app read ledger %s but the platform routed the call to %s", names[c.project], names[c.routed])
		}
		reachedLyon = reachedLyon || (c.tool == "invoices_search" && c.routed == p.lyon)
	}
	if !reachedLyon {
		t.Fatalf("no invoices_search ran in Lyon Shop")
	}
	if !strings.Contains(reply, "INV-A-102") {
		t.Errorf("reply does not name Lyon's overdue invoice INV-A-102")
	}

	// 2. Every reachable project in one call.
	seen = len(app.snapshot())
	baseline = telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	reply = askAndWait(t, h, "Now find every overdue invoice across all the projects you can reach, including this one. Get them with a single billing tool call, then list the invoice ids grouped by project and end your reply with STEP-2-DONE.", "STEP-2-DONE")
	calls = callsSince(app, seen)
	logCalls(calls)
	coreCalls := billingToolCalls(t, h, baseline)
	t.Logf("  core calls: %v", coreCalls)
	starCalls := 0
	for _, c := range coreCalls {
		if args, _ := c["args"].(map[string]any); args["project_id"] == grantAllProjects {
			starCalls++
		}
	}
	if starCalls == 0 {
		t.Errorf("agent never passed project_id %q", grantAllProjects)
	}
	// The per-project calls of one fan-out share the agent's tool call id.
	byCall := map[string]map[string]bool{}
	for _, c := range calls {
		if c.tool != "invoices_search" {
			continue
		}
		if byCall[c.toolCall] == nil {
			byCall[c.toolCall] = map[string]bool{}
		}
		byCall[c.toolCall][c.routed] = true
	}
	fannedOut := false
	for _, projects := range byCall {
		if projects[p.home] && projects[p.lyon] && projects[p.lille] && !projects[p.nantes] {
			fannedOut = true
		}
	}
	if !fannedOut {
		t.Errorf("no single call ran in Head Office, Lyon Shop and Lille Shop: %v", byCall)
	}
	for _, want := range []string{"INV-H-001", "INV-A-102", "INV-B-207"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply does not name overdue invoice %s", want)
		}
	}
	if strings.Contains(reply, "INV-C-301") {
		t.Errorf("reply names Nantes Shop's invoice, which is outside the grant")
	}

	// 3. A write in a named shop.
	seen = len(app.snapshot())
	askAndWait(t, h, "Mark invoice INV-A-102 at the Lyon Shop as paid, then end your reply with STEP-3-DONE.", "STEP-3-DONE")
	logCalls(callsSince(app, seen))
	if got := app.status(p.lyon, "INV-A-102"); got != "paid" {
		t.Errorf("INV-A-102 status=%q, want paid", got)
	}

	// 4. A shop outside the grant.
	seen = len(app.snapshot())
	baseline = telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	askAndWait(t, h, "The Nantes Shop has an overdue invoice too, INV-C-301. Mark it as paid if you can; if you cannot, say why. End your reply with STEP-4-DONE.", "STEP-4-DONE")
	calls = callsSince(app, seen)
	logCalls(calls)
	t.Logf("  core calls: %v", billingToolCalls(t, h, baseline))
	for _, c := range calls {
		if c.routed == p.nantes || c.project == p.nantes {
			t.Errorf("a call reached Nantes Shop, which is outside the grant: %+v", c)
		}
	}
	if got := app.status(p.nantes, "INV-C-301"); got != "overdue" {
		t.Errorf("INV-C-301 status=%q, want overdue", got)
	}
}

func TestProjectArgs_RealLLM_Codex_AgentWithoutGrantCannotNameAnotherProject(t *testing.T) {
	providerState := loadOpenAICodexProviderState(t)
	var (
		p       realGrantProjects
		app     *realBillingApp
		install int64
	)
	h := setupRealChannelChatHarnessWithProviderPrepared(t, "Northwind Finance", realGrantDirective,
		`{"include_channels":true}`, 15, "llm", "OpenAI Codex", providerState,
		func(s *Server, userID int64, agent *Agent) {
			p, app, install = prepareRealGrantFixture(t, s, userID, agent)
		})
	waitForInitialAgentTurn(t, h)
	attachRealBilling(t, h, install)

	baseline := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	reply := askAndWait(t, h, fmt.Sprintf("Look up the overdue invoices of the Lyon Shop: pass its project id %s to the billing tool. List what you find and end your reply with STEP-1-DONE.", p.lyon), "STEP-1-DONE")
	for _, c := range app.snapshot() {
		t.Logf("  app call %s ledger=%s routed=%s args=%v", c.tool, c.project, c.routed, c.args)
		if c.project == p.lyon || c.routed == p.lyon {
			t.Errorf("an ungranted call reached the Lyon Shop ledger: %+v", c)
		}
	}
	tried := false
	for _, c := range billingToolCalls(t, h, baseline) {
		t.Logf("  core call: %v", c)
		if args, _ := c["args"].(map[string]any); args["project_id"] == p.lyon {
			tried = true
		}
	}
	if !tried {
		t.Fatalf("agent never named the Lyon Shop's project id, so nothing was tested")
	}
	refused := false
	for _, ev := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.result", baseline) {
		refused = refused || strings.Contains(string(ev.Data), "is another project")
	}
	if !refused {
		t.Errorf("no tool result carried the platform's refusal")
	}
	if strings.Contains(reply, "INV-A-102") {
		t.Errorf("reply names the Lyon Shop's invoice INV-A-102")
	}
}
