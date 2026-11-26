package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
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

// A project connection serves agents of that project and agents outside any
// project, like the Helper, which are offered it too; not other projects.
func TestClaudeSubscriptionProjectConnectionScope(t *testing.T) {
	f := newClaudeSubFixture(t)
	other, err := f.s.store.CreateProject(f.userID, "Studio", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.dbExec(`UPDATE connections SET project_id=? WHERE id=?`, other.ID, f.connID); err != nil {
		t.Fatal(err)
	}
	var own string
	if err := f.s.store.dbQueryRow(`SELECT project_id FROM agents WHERE id=?`, f.agentID).Scan(&own); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"model": "opus", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, tc := range []struct {
		name, project string
		ok            bool
	}{
		{"another project", own, false},
		{"no project", "", true},
		{"the connection's project", other.ID, true},
	} {
		if _, err := f.s.store.dbExec(`UPDATE agents SET project_id=? WHERE id=?`, tc.project, f.agentID); err != nil {
			t.Fatal(err)
		}
		r := f.chat(t, body)
		if tc.ok && (r.status != http.StatusOK || r.content != "echo: hi") {
			t.Fatalf("%s: status = %d content = %q %s", tc.name, r.status, r.content, r.errBody)
		}
		if !tc.ok && (r.status != http.StatusServiceUnavailable || !strings.Contains(r.errBody, "not_connected")) {
			t.Fatalf("%s: status = %d %s, want not_connected", tc.name, r.status, r.errBody)
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
	if pool["generating"] != float64(0) || pool["waiting_tool"] != float64(0) {
		t.Fatalf("pool must say what its processes are doing: %+v", pool)
	}
	if _, settings, _ := f.s.claudeSubscriptionConnection(); settings.PoolSize != 2 || settings.IdleSeconds != 45 {
		t.Fatalf("settings not saved on the connection: %+v", settings)
	}

	// Sleeping agents give up their process unless told to keep it;
	// settings saved before the option existed do the same.
	if settings, _ := get()["settings"].(map[string]any); settings["hold_sleeping"] != false {
		t.Fatalf("hold_sleeping default = %v", settings["hold_sleeping"])
	}
	if f.s.claudeSubscriptionLimits().HoldSleeping {
		t.Fatal("pool holds sleeping agents by default")
	}
	if rec := put(`{"hold_sleeping":"yes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("hold_sleeping \"yes\" accepted: %d", rec.Code)
	}
	if rec := put(`{"hold_sleeping":true}`); rec.Code != http.StatusOK {
		t.Fatalf("hold_sleeping update: %d %s", rec.Code, rec.Body.String())
	}
	if settings, _ := get()["settings"].(map[string]any); settings["hold_sleeping"] != true || settings["pool_size"] != float64(2) {
		t.Fatalf("settings after hold_sleeping = %+v", settings)
	}
	if !f.s.claudeSubscriptionLimits().HoldSleeping {
		t.Fatal("hold_sleeping did not reach the pool limits")
	}
	if rec := put(`{"idle_seconds":60}`); rec.Code != http.StatusOK {
		t.Fatalf("settings update: %d", rec.Code)
	}
	if !f.s.claudeSubscriptionLimits().HoldSleeping {
		t.Fatal("updating another setting reset hold_sleeping")
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
	if got := req.Messages[0]; got.Text != "look" || len(got.Images) != 1 || got.Images[0].MediaType != "image/png" || got.Images[0].Data != "AA==" {
		t.Fatalf("user message with an image = %+v", got)
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

func TestClaudeContentReadsImageParts(t *testing.T) {
	raw := `[
		{"type":"text","text":"several"},
		{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/","detail":"high"}},
		{"type":"input_image","image_url":"https://images.example/a.png"},
		{"type":"image_url","image_url":{"url":"data:image/svg+xml,%3Csvg%2F%3E"}},
		{"type":"image_url","image_url":{"url":"ftp://images.example/b.png"}},
		{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}
	]`
	text, images := claudeContent(json.RawMessage(raw))
	want := []claudepool.Image{
		{MediaType: "image/jpeg", Data: "/9j/"},
		{URL: "https://images.example/a.png"},
		{MediaType: "image/svg+xml", Data: base64.StdEncoding.EncodeToString([]byte("<svg/>"))},
	}
	if len(images) != len(want) {
		t.Fatalf("images = %+v", images)
	}
	for i := range want {
		if images[i].MediaType != want[i].MediaType || images[i].Data != want[i].Data || images[i].URL != want[i].URL {
			t.Fatalf("image %d = %+v, want %+v", i, images[i], want[i])
		}
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 || lines[0] != "several" || !strings.Contains(lines[1], "address could not be read") || !strings.Contains(lines[2], "audio clip") {
		t.Fatalf("text = %q", text)
	}
}

// claudeSubPNG is a small PNG data URI; shade tells images apart.
func claudeSubPNG(t *testing.T, shade uint8) (string, string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+3] = shade, 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	return "data:image/png;base64," + data, claudetest.ImageMarker("image/png", data)
}

// Images reach Claude the way core sends them: attached to a user message,
// and in a tool result, like a screenshot.
func TestClaudeSubscriptionChatPassesImages(t *testing.T) {
	f := newClaudeSubFixture(t)
	photo, photoShown := claudeSubPNG(t, 10)
	screen, screenShown := claudeSubPNG(t, 20)
	// A link to this machine is not fetched.
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the image link on a private address was fetched")
	}))
	defer local.Close()

	messages := []any{
		map[string]any{"role": "system", "content": "You are Helper."},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": photo}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": local.URL + "/x.png"}},
		}},
	}
	r := f.chat(t, map[string]any{"model": "sonnet", "stream": true, "messages": messages, "tools": []any{claudeSubLookupTool}})
	if r.status != http.StatusOK || r.errBody != "" {
		t.Fatalf("first turn: %+v", r)
	}
	if !strings.HasPrefix(r.content, "echo: what is this?\n"+photoShown+"\n[An image linked from "+local.URL+"/x.png was attached here, but it could not be fetched") {
		t.Fatalf("reply = %q", r.content)
	}

	messages = append(messages,
		map[string]any{"role": "assistant", "content": r.content},
		map[string]any{"role": "user", "content": `CALL lookup {"q":"screen"}`},
	)
	r = f.chat(t, map[string]any{"model": "sonnet", "stream": true, "messages": messages, "tools": []any{claudeSubLookupTool}})
	if r.finish != "tool_calls" || len(r.calls) != 1 {
		t.Fatalf("tool turn: %+v", r)
	}
	messages = append(messages,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": r.calls[0]["id"], "type": "function", "function": map[string]any{"name": "lookup", "arguments": r.calls[0]["arguments"]},
		}}},
		map[string]any{"role": "tool", "tool_call_id": r.calls[0]["id"], "content": []any{
			map[string]any{"type": "text", "text": "Screenshot taken."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": screen, "detail": "high"}},
		}},
	)
	r = f.chat(t, map[string]any{"model": "sonnet", "stream": true, "messages": messages, "tools": []any{claudeSubLookupTool}})
	if want := "got: Screenshot taken.\n" + screenShown; r.status != http.StatusOK || r.content != want {
		t.Fatalf("after the screenshot: %q, want %q (%+v)", r.content, want, r)
	}
	pool, _ := f.s.claudePool()
	if st := pool.Status(); st.Sessions != 1 || st.Running != 1 {
		t.Fatalf("pool = %+v, want the conversation kept in one session", st)
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

func TestClaudeSubscriptionSignInWithACode(t *testing.T) {
	f := newClaudeSubFixture(t)
	call := func(method, path, body string) (int, map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/claude-subscription"+path, strings.NewReader(body))
		req.Header.Set("X-User-ID", jsonID(f.userID))
		rec := httptest.NewRecorder()
		f.s.handleClaudeSubscription(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	login := func() map[string]any {
		_, out, _ := call(http.MethodGet, "?refresh=1", "")
		state, _ := out["login"].(map[string]any)
		return state
	}
	waitFor := func(what string, ok func(map[string]any) bool) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			state := login()
			if ok(state) {
				return state
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, state)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if code, out, _ := call(http.MethodPost, "/login/code", `{"code":"x"}`); code != http.StatusConflict || out["error"] != "not_running" {
		t.Fatalf("code without a sign-in: %d %+v", code, out)
	}
	if code, out, _ := call(http.MethodPost, "/login", ""); code != http.StatusOK || out["started"] != true {
		t.Fatalf("start: %d %+v", code, out)
	}
	state := waitFor("the sign-in link", func(s map[string]any) bool { return s["url"] != nil && s["awaiting_code"] == true })
	if !strings.HasPrefix(state["url"].(string), "https://claude.com/") {
		t.Fatalf("link = %v", state["url"])
	}
	if code, out, _ := call(http.MethodPost, "/login/code", `{"code":"two\nlines"}`); code != http.StatusBadRequest || out["error"] != "invalid_code" {
		t.Fatalf("malformed code: %d %+v", code, out)
	}
	if code, out, _ := call(http.MethodPost, "/login/code", `not json`); code != http.StatusBadRequest || out["error"] != "invalid_code" {
		t.Fatalf("malformed body: %d %+v", code, out)
	}
	code, out, raw := call(http.MethodPost, "/login/code", `{"code":"`+claudetest.FakeLoginCode+`"}`)
	if code != http.StatusOK || strings.Contains(raw, "good-code") {
		t.Fatalf("submit: %d %s", code, raw)
	}
	if state, _ := out["login"].(map[string]any); state["code_submitted"] != true {
		t.Fatalf("after submit: %+v", out)
	}
	state = waitFor("the sign-in to finish", func(s map[string]any) bool { return s["running"] == false })
	if state["last_error"] != nil || state["url"] != nil {
		t.Fatalf("finished: %+v", state)
	}
	if _, out, _ := call(http.MethodGet, "?refresh=1", ""); out["auth"].(map[string]any)["logged_in"] != true {
		t.Fatalf("auth after sign-in: %+v", out["auth"])
	}

	// A wrong code reports Claude Code's message; cancel stops a sign-in.
	call(http.MethodPost, "/login", "")
	waitFor("the prompt", func(s map[string]any) bool { return s["awaiting_code"] == true })
	call(http.MethodPost, "/login/code", `{"code":"wrong#code"}`)
	state = waitFor("the sign-in to fail", func(s map[string]any) bool { return s["running"] == false })
	if msg, _ := state["last_error"].(string); !strings.Contains(msg, "wasn't accepted") {
		t.Fatalf("wrong code: %+v", state)
	}
	call(http.MethodPost, "/login", "")
	waitFor("the prompt", func(s map[string]any) bool { return s["awaiting_code"] == true })
	if code, out, _ := call(http.MethodPost, "/login/cancel", ""); code != http.StatusOK || out["canceled"] != true {
		t.Fatalf("cancel: %d %+v", code, out)
	}
	if state := login(); state["running"] != false || state["last_error"] != nil {
		t.Fatalf("after cancel: %+v", state)
	}

	if _, err := f.s.store.CreateUser("second@test", "hash"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/login", "/login/code", "/login/cancel"} {
		if code, out, _ := call(http.MethodPost, path, `{"code":"x"}`); code != http.StatusForbidden || out["error"] != "single_user_only" {
			t.Fatalf("%s on a multi-user install: %d %+v", path, code, out)
		}
	}
}

// Core sizes its request budget from model_capabilities. Without them the
// bare Claude Code aliases fall back to its 131K default.
func TestClaudeSubscriptionProviderSendsContextWindows(t *testing.T) {
	providers := buildCoreProviderConfigs([]ProviderInfo{
		{Type: claudeSubscriptionProviderKey, ModelLarge: "opus", ModelMedium: "sonnet[1m]", ModelSmall: "claude-haiku-4-5"},
		{Type: "anthropic", ModelLarge: "claude-opus-4-7", ModelMedium: "claude-sonnet-4-6", ModelSmall: "claude-haiku-4-5"},
	}, claudeSubscriptionProviderKey)
	byName := map[string]map[string]any{}
	for _, provider := range providers {
		byName[provider["name"].(string)] = provider
	}
	sub := byName[claudeSubscriptionProviderKey]
	if sub == nil {
		t.Fatalf("claude subscription provider missing: %v", providers)
	}
	caps, ok := sub["model_capabilities"].(map[string]ProviderModelCapabilities)
	if !ok {
		t.Fatalf("model_capabilities = %T", sub["model_capabilities"])
	}
	for id, want := range map[string]int{"opus": 200000, "sonnet": 200000, "haiku": 200000, "opus[1m]": 1000000, "sonnet[1m]": 1000000, "claude-haiku-4-5": 200000} {
		if got := caps[id].ContextWindow; got != want {
			t.Fatalf("%s context window = %d, want %d", id, got, want)
		}
	}
	if _, ok := byName["anthropic"]["model_capabilities"]; ok {
		t.Fatal("other providers gained model capabilities")
	}

	// An agent's own model is covered too.
	applyAgentModelOverride(providers, claudeSubscriptionProviderKey, "claude-opus-4-7[1m]")
	caps = sub["model_capabilities"].(map[string]ProviderModelCapabilities)
	if got := caps["claude-opus-4-7[1m]"].ContextWindow; got != 1000000 {
		t.Fatalf("override context window = %d", got)
	}

	// Entries decoded from JSON keep the windows they already carry.
	decoded := map[string]any{
		"name":               claudeSubscriptionProviderKey,
		"models":             map[string]any{"large": "opus", "medium": "custom-model", "small": "haiku"},
		"model_capabilities": map[string]any{"opus": map[string]any{"context_window": 123}},
	}
	withClaudeSubscriptionCapabilities(decoded)
	caps = decoded["model_capabilities"].(map[string]ProviderModelCapabilities)
	if caps["opus"].ContextWindow != 123 || caps["custom-model"].ContextWindow != 200000 {
		t.Fatalf("decoded capabilities %+v", caps)
	}
}
