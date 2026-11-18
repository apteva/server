package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/claudepool"
	"github.com/apteva/server/claudepool/claudetest"
)

// The server test binary doubles as a fake `claude` CLI for the Claude
// subscription provider tests (see claudetest).
func TestMain(m *testing.M) {
	if os.Getenv("CLAUDEPOOL_FAKE") == "1" {
		os.Exit(claudetest.FakeMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type claudeSubFixture struct {
	s       *Server
	srv     *httptest.Server
	userID  int64
	agentID int64
	coreKey string
	connID  int64
}

func newClaudeSubFixture(t *testing.T) *claudeSubFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEPOOL_FAKE", "1")
	t.Setenv("APTEVA_CLAUDE_BIN", exe)
	t.Setenv("APTEVA_CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-config"))

	s := newTestServer(t)
	s.dataDir = t.TempDir()
	s.catalog = NewAppCatalog()
	if _, err := s.catalog.LoadFromFS(os.DirFS("integrations-catalog")); err != nil {
		t.Fatal(err)
	}
	if app := s.catalog.Get(claudeSubscriptionSlug); app == nil || app.Runtime == nil || !connectionUsesLocalCLI(app) {
		t.Fatalf("catalog entry missing or malformed: %+v", app)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/llm/claude-subscription/chat/completions", s.handleClaudeSubscriptionChat)
	mux.HandleFunc("/api/llm/claude-subscription/mcp", s.handleClaudeSubscriptionMCP)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	s.port = port
	t.Cleanup(func() {
		if pool := s.claudeSub.ready.Load(); pool != nil {
			pool.Close()
		}
	})

	admin := createTestAdmin(t, s)
	project, _ := s.store.CreateProject(admin.ID, "Personal", "", "")
	agent, err := s.store.CreateAgent(admin.ID, "Helper", "", "cautious", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent.CoreAPIKey = "core_claude_sub_test"
	agent.Status = "running"
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}
	encrypted, _ := Encrypt(s.secret, `{}`)
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: admin.ID, AppSlug: claudeSubscriptionSlug, AppName: "Claude subscription",
		Name: "Claude", AuthType: connectionAuthTypeLocalCLI, EncryptedCreds: encrypted, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &claudeSubFixture{s: s, srv: srv, userID: admin.ID, agentID: agent.ID, coreKey: agent.CoreAPIKey, connID: conn.ID}
}

type sseResult struct {
	status   int
	header   http.Header
	content  string
	thinking string
	calls    []map[string]string
	finish   string
	usage    map[string]any
	done     bool
	errBody  string
}

func (f *claudeSubFixture) chat(t *testing.T, body map[string]any) sseResult {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/llm/claude-subscription/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+f.coreKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := sseResult{status: resp.StatusCode, header: resp.Header}
	if resp.StatusCode != http.StatusOK {
		var b strings.Builder
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			b.WriteString(scanner.Text())
		}
		out.errBody = b.String()
		return out
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			out.done = true
			break
		}
		var chunk struct {
			Error   map[string]any `json:"error"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		if chunk.Error != nil {
			out.errBody = data
			continue
		}
		if chunk.Usage != nil {
			out.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			out.content += choice.Delta.Content
			out.thinking += choice.Delta.ReasoningContent
			for _, call := range choice.Delta.ToolCalls {
				for len(out.calls) <= call.Index {
					out.calls = append(out.calls, map[string]string{})
				}
				c := out.calls[call.Index]
				if call.ID != "" {
					c["id"] = call.ID
				}
				if call.Function.Name != "" {
					c["name"] = call.Function.Name
				}
				c["arguments"] += call.Function.Arguments
			}
			if choice.FinishReason != nil {
				out.finish = *choice.FinishReason
			}
		}
	}
	return out
}

var claudeSubLookupTool = map[string]any{"type": "function", "function": map[string]any{
	"name": "lookup", "description": "Look something up",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
}}

func TestClaudeSubscriptionChatRunsToolTurns(t *testing.T) {
	f := newClaudeSubFixture(t)
	messages := []any{
		map[string]any{"role": "system", "content": "You are Helper."},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": `CALL lookup {"q":"x"}`}}},
	}
	r := f.chat(t, map[string]any{"model": "opus", "stream": true, "messages": messages, "tools": []any{claudeSubLookupTool}})
	if r.status != http.StatusOK || !r.done || r.errBody != "" {
		t.Fatalf("first turn: %+v", r)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0]["name"] != "lookup" || r.calls[0]["arguments"] != `{"q":"x"}` || !strings.HasPrefix(r.calls[0]["id"], "toolu_") {
		t.Fatalf("tool call: %+v", r)
	}
	if r.usage == nil {
		t.Fatalf("no usage chunk")
	}

	messages = append(messages,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": r.calls[0]["id"], "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q": "x"}`},
		}}},
		map[string]any{"role": "tool", "tool_call_id": r.calls[0]["id"], "content": "it is 42"},
	)
	r = f.chat(t, map[string]any{"model": "opus", "stream": true, "messages": messages, "tools": []any{claudeSubLookupTool}})
	if r.status != http.StatusOK || r.content != "got: it is 42" || r.finish != "stop" || r.thinking != "hmm" || !r.done {
		t.Fatalf("second turn: %+v", r)
	}
	if got := r.usage["prompt_tokens"]; got != float64(115) {
		t.Fatalf("prompt_tokens = %v (want input + cache read + cache write)", got)
	}
	details, _ := r.usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(100) || details["cache_write_tokens"] != float64(5) {
		t.Fatalf("usage details = %+v", details)
	}

	pool, _ := f.s.claudePool()
	st := pool.Status()
	if st.Running != 1 || st.Processes[0].OwnerLabel != "Helper" || st.Processes[0].Owner != claudeSubscriptionOwner(f.agentID) {
		t.Fatalf("pool status = %+v", st)
	}
	usage := f.s.claudeSubscriptionUsage(t.Context(), f.connID, false)
	if !usage.Supported || usage.Plan != "max" || usage.Stale || usage.FetchedAt.IsZero() || len(usage.Limits) != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	plan, fable := usage.Limits[0], usage.Limits[1]
	if plan.ID != "claude" || plan.Reached || len(plan.Windows) != 2 ||
		plan.Windows[0].UsedPercent != 45 || plan.Windows[0].DurationMinutes != 300 || plan.Windows[0].ResetsAt == nil ||
		plan.Windows[1].UsedPercent != 54 || plan.Windows[1].DurationMinutes != 7*24*60 {
		t.Fatalf("plan limit = %+v", plan)
	}
	if fable.ID != "model:fable" || fable.Label != "Fable (weekly)" || len(fable.Windows) != 1 || fable.Windows[0].UsedPercent != 0 {
		t.Fatalf("model limit = %+v", fable)
	}

	f.s.claudeSubscriptionAgentStopped(f.agentID)
	deadline := time.Now().Add(5 * time.Second)
	for pool.Status().Sessions != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("sessions after agent stop = %d", pool.Status().Sessions)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClaudeSubscriptionChatRejectsBadCallers(t *testing.T) {
	f := newClaudeSubFixture(t)
	body := map[string]any{"model": "opus", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	f.coreKey = "core_wrong"
	if r := f.chat(t, body); r.status != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d", r.status)
	}
	f.coreKey = "core_claude_sub_test"

	body["model"] = "--dangerous"
	if r := f.chat(t, body); r.status != http.StatusBadRequest {
		t.Fatalf("bad model status = %d", r.status)
	}
	body["model"] = "opus"

	if _, err := f.s.store.CreateUser("second@test", "hash"); err != nil {
		t.Fatal(err)
	}
	if r := f.chat(t, body); r.status != http.StatusForbidden || !strings.Contains(r.errBody, "single_user_only") {
		t.Fatalf("multi-user status = %d %s", r.status, r.errBody)
	}
	for _, entry := range f.s.runtimeCatalogForRole("llm") {
		if entry.Slug == claudeSubscriptionSlug {
			t.Fatal("multi-user install still lists the Claude subscription provider")
		}
	}
}

// Core starts thinking before its key reaches the database.
func TestClaudeSubscriptionAcceptsBootingAgent(t *testing.T) {
	f := newClaudeSubFixture(t)
	if _, err := f.s.store.dbExec(`UPDATE agents SET status='stopped', core_api_key='' WHERE id=?`, f.agentID); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"model": "opus", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if r := f.chat(t, body); r.status != http.StatusUnauthorized {
		t.Fatalf("key of no live process: status = %d", r.status)
	}

	f.s.agents.mu.Lock()
	f.s.agents.processes[f.agentID] = &runningAgent{port: 1, reattached: true, coreAPIKey: f.coreKey}
	f.s.agents.mu.Unlock()
	if r := f.chat(t, body); r.status != http.StatusOK || r.content != "echo: hi" {
		t.Fatalf("booting agent: status = %d content = %q %s", r.status, r.content, r.errBody)
	}

	f.s.agents.mu.Lock()
	delete(f.s.agents.processes, f.agentID)
	f.s.agents.mu.Unlock()
	if r := f.chat(t, body); r.status != http.StatusUnauthorized {
		t.Fatalf("after the process ended: status = %d", r.status)
	}
}

func TestClaudeSubscriptionNotSignedIn(t *testing.T) {
	f := newClaudeSubFixture(t)
	t.Setenv("CLAUDEPOOL_FAKE_LOGGEDOUT", "1")
	r := f.chat(t, map[string]any{"model": "opus", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if r.status != http.StatusServiceUnavailable || !strings.Contains(r.errBody, "not_signed_in") || r.header.Get("Retry-After") == "" {
		t.Fatalf("signed-out response: %d %s retry=%q", r.status, r.errBody, r.header.Get("Retry-After"))
	}
}

func TestClaudeSubscriptionStatusAndSettings(t *testing.T) {
	f := newClaudeSubFixture(t)
	get := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/claude-subscription?refresh=1", nil)
		req.Header.Set("X-User-ID", jsonID(f.userID))
		rec := httptest.NewRecorder()
		f.s.handleClaudeSubscription(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	st := get()
	auth, _ := st["auth"].(map[string]any)
	if st["available"] != true || st["installed"] != true || st["version"] != "9.9.9 (Claude Code)" || auth["logged_in"] != true || auth["subscription"] != true {
		t.Fatalf("status = %+v", st)
	}
	if !strings.HasPrefix(st["sign_in_command"].(string), "CLAUDE_CONFIG_DIR=") {
		t.Fatalf("sign-in command ignores the config dir: %v", st["sign_in_command"])
	}
	if _, leaked := st["token"]; leaked {
		t.Fatal("status must not carry credential fields")
	}

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/claude-subscription/settings", strings.NewReader(body))
		req.Header.Set("X-User-ID", jsonID(f.userID))
		rec := httptest.NewRecorder()
		f.s.handleClaudeSubscription(rec, req)
		return rec
	}
	if rec := put(`{"pool_size":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("pool_size 0 accepted: %d", rec.Code)
	}
	if rec := put(`{"queue_seconds":400}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("queue_seconds 400 accepted: %d", rec.Code)
	}
	if rec := put(`{"pool_size":2,"idle_seconds":45}`); rec.Code != http.StatusOK {
		t.Fatalf("settings update: %d %s", rec.Code, rec.Body.String())
	}
	pool, _ := get()["pool"].(map[string]any)
	if pool["max_processes"] != float64(2) || pool["idle_seconds"] != float64(45) || pool["queue_seconds"] != float64(60) {
		t.Fatalf("pool after update = %+v", pool)
	}
	if _, settings, _ := f.s.claudeSubscriptionConnection(); settings.PoolSize != 2 || settings.IdleSeconds != 45 {
		t.Fatalf("settings not saved on the connection: %+v", settings)
	}
}

func TestClaudeSubscriptionMCPOnlyFromLoopback(t *testing.T) {
	f := newClaudeSubFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/llm/claude-subscription/mcp", strings.NewReader(`{}`))
	req.RemoteAddr = "203.0.113.9:4000"
	rec := httptest.NewRecorder()
	f.s.handleClaudeSubscriptionMCP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("remote MCP call status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/llm/claude-subscription/mcp", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:4000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec = httptest.NewRecorder()
	f.s.handleClaudeSubscriptionMCP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("proxied MCP call status = %d", rec.Code)
	}
}

func TestClaudeChatRequestToPool(t *testing.T) {
	var body claudeChatRequest
	raw := `{"model":"sonnet","messages":[
		{"role":"system","content":"one"},
		{"role":"system","content":"two"},
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"done"},
		{"role":"system","content":"late"}
	],"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	req, err := body.toPool("agent:1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "one\n\ntwo" || len(req.Messages) != 4 || len(req.Tools) != 1 {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.Messages[0].Text, "cannot show images") {
		t.Fatalf("image placeholder missing: %q", req.Messages[0].Text)
	}
	want := []claudepool.Message{
		{Role: "assistant", ToolCalls: []claudepool.ToolCall{{ID: "c1", Name: "lookup", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Text: "done"},
		{Role: "user", Text: "[system]\nlate"},
	}
	for i, w := range want {
		got := req.Messages[i+1]
		if got.Role != w.Role || got.Text != w.Text || got.ToolCallID != w.ToolCallID || len(got.ToolCalls) != len(w.ToolCalls) {
			t.Fatalf("message %d = %+v, want %+v", i+1, got, w)
		}
	}
}

func jsonID(id int64) string {
	raw, _ := json.Marshal(id)
	return string(raw)
}

func TestClaudeSubscriptionReachesCoreLikeOtherProviders(t *testing.T) {
	f := newClaudeSubFixture(t)
	projectID := ""
	_ = f.s.store.dbQueryRow(`SELECT COALESCE(project_id,'') FROM agents WHERE id=?`, f.agentID).Scan(&projectID)
	pool := f.s.GetProviderPool(f.userID, projectID)
	var info *ProviderInfo
	for i := range pool {
		if pool[i].Type == claudeSubscriptionProviderKey {
			info = &pool[i]
		}
	}
	if info == nil {
		t.Fatalf("provider pool = %+v", pool)
	}
	if info.ModelLarge != "opus" || info.ModelMedium != "sonnet" || info.ModelSmall != "haiku" {
		t.Fatalf("models = %s/%s/%s", info.ModelLarge, info.ModelMedium, info.ModelSmall)
	}
	env, err := f.s.runtimeEnvFromConnections(f.userID, map[string]string{}, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if env["CLAUDE_SUBSCRIPTION_CONNECTION_ID"] != jsonID(f.connID) || len(env) != 1 {
		t.Fatalf("runtime env = %+v", env)
	}
}

// When Claude Code cannot read the usage meter, the card shows what the
// latest turn reported.
func TestClaudeSubscriptionUsageFallsBackToTurnReport(t *testing.T) {
	t.Setenv("CLAUDEPOOL_FAKE_ASK_FAIL", "1")
	f := newClaudeSubFixture(t)
	if usage := f.s.claudeSubscriptionUsage(t.Context(), f.connID, false); usage.Plan != "max" || len(usage.Limits) != 0 {
		t.Fatalf("usage before any turn = %+v", usage)
	}
	r := f.chat(t, map[string]any{"model": "haiku", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if r.status != http.StatusOK || r.content != "echo: hi" {
		t.Fatalf("turn = %+v", r)
	}
	usage := f.s.claudeSubscriptionUsage(t.Context(), f.connID, true)
	if len(usage.Limits) != 1 || len(usage.Limits[0].Windows) != 1 ||
		usage.Limits[0].Windows[0].UsedPercent != 42 || usage.Limits[0].Windows[0].DurationMinutes != 300 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestClaudeSubscriptionModelsAndDefaultTiers(t *testing.T) {
	f := newClaudeSubFixture(t)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/connections/%d/models", f.connID), nil)
	req.Header.Set("X-User-ID", strconv.FormatInt(f.userID, 10))
	rec := httptest.NewRecorder()
	f.s.handleConnectionModels(rec, req)
	var models []ModelInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &models); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("models %d: %s", rec.Code, rec.Body.String())
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	// The default alias Claude Code lists only as a variant comes first;
	// "default" and disabled rows are left out.
	if strings.Join(ids, ",") != "opus,opus[1m],claude-fable-5-1[1m],sonnet,haiku" {
		t.Fatalf("models = %v", ids)
	}
	// Options are named after the model they point at today.
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "Opus 5.5,Opus 5.5 (1M context),Fable 5.1 (1M context),Sonnet 5,Haiku 4.5" {
		t.Fatalf("names = %v", names)
	}
	if models[2].ContextSize != 1000000 || models[3].ContextSize != 200000 {
		t.Fatalf("fable/sonnet = %+v %+v", models[2], models[3])
	}

	// A new connection lists with its default tiers, before any agent runs.
	req = httptest.NewRequest(http.MethodGet, "/connections/runtime", nil)
	req.Header.Set("X-User-ID", strconv.FormatInt(f.userID, 10))
	rec = httptest.NewRecorder()
	f.s.handleListRuntimeConnections(rec, req)
	var listed []struct {
		ID          int64          `json:"id"`
		RuntimeConf map[string]any `json:"runtime_config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); rec.Code != http.StatusOK || err != nil || len(listed) != 1 {
		t.Fatalf("list %d: %s", rec.Code, rec.Body.String())
	}
	conf := listed[0].RuntimeConf
	if conf["model_large"] != "opus" || conf["model_medium"] != "sonnet" || conf["model_small"] != "haiku" {
		t.Fatalf("runtime_config = %v", conf)
	}
	if saved, err := f.s.store.GetConnectionRuntimeConfig(f.userID, f.connID); err != nil || saved["model_large"] != "opus" {
		t.Fatalf("saved config = %v %v", saved, err)
	}
}

func TestClaudeModelName(t *testing.T) {
	for id, want := range map[string]string{
		"claude-sonnet-5":           "Sonnet 5",
		"claude-opus-5-5[1m]":       "Opus 5.5",
		"claude-haiku-4-5-20251001": "Haiku 4.5",
		"claude-fable-5-1":          "Fable 5.1",
		"sonnet":                    "",
		"claude-3-5-sonnet":         "",
		"":                          "",
	} {
		if got := claudeModelName(id); got != want {
			t.Errorf("claudeModelName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestParseFootprint(t *testing.T) {
	raw := []byte(`======================================================================
claude [52919]: 64-bit    Footprint: 155239744 B (16384 bytes per page)
======================================================================

  Dirty      Clean  Reclaimable    Regions    Category
    ---        ---          ---        ---    ---
    phys_footprint: 155239744 B
claude [51410]: 64-bit    Footprint: 153830744 B (16384 bytes per page)
    phys_footprint: 153830744 B
Summary Footprint: 309004952 B
`)
	got := parseFootprint(raw)
	if len(got) != 2 || got[52919] != 155239744/1024 || got[51410] != 153830744/1024 {
		t.Fatalf("parseFootprint = %v", got)
	}
	if got := parseFootprint([]byte("footprint: no process\n")); len(got) != 0 {
		t.Fatalf("parseFootprint(error) = %v", got)
	}
}

// The status reports memory for live processes, here this test process.
func TestProcessMemoryReadsLiveProcesses(t *testing.T) {
	pid := os.Getpid()
	got := processMemory([]int{pid})
	if got[pid] <= 0 {
		t.Fatalf("processMemory = %v", got)
	}
	if runtime.GOOS == "darwin" {
		if fp := processFootprint([]int{pid}); fp[pid] <= 0 {
			t.Fatalf("processFootprint = %v", fp)
		}
	}
}
