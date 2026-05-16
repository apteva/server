package main

// eval_http_test.go — exercises the HTTP handlers in agent_evals.go
// end-to-end through the same routing the dashboard hits, but
// short-circuits the auth middleware by stamping X-User-ID directly
// (getUserID reads that header — see auth.go).
//
// What's covered:
//   - handleAgentEvals routing (list/create/get/update/delete) +
//     ownership check (foreign agent IDs return 404 so the eval
//     table isn't enumerable cross-user).
//   - handleEvalMockGateway: initialize / tools/list / tools/call,
//     mock-hit vs stub-default vs strict violation vs declared
//     error. The mock gateway is the seam between spawned core and
//     trajectory recording — getting this wrong silently produces
//     useless judge inputs.
//   - handleApplyEvalSuggestions: directive append + audit row.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setupServerWithAgent gives every test a fresh Server + a user-owned
// agent. Returns (server, userID, agent) so callers can build requests
// against the agent without re-doing the boilerplate.
func setupServerWithAgent(t *testing.T) (*Server, int64, *Agent) {
	t.Helper()
	s := newTestServer(t)
	user, err := s.store.CreateUser("evals-http@example.com", "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	agent, err := s.store.CreateAgent(user.ID, "agent-under-test", "be polite", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return s, user.ID, agent
}

func evalsAuthedReq(method, path string, userID int64, body any) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		buf, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(buf))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-User-ID", i64s(userID))
	return r
}

// ─── handleAgentEvals: collection + item routing ──────────────────

func TestHandleAgentEvals_List_Empty(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	w := httptest.NewRecorder()
	s.handleAgentEvals(w, evalsAuthedReq("GET", "/instances/"+i64s(agent.ID)+"/evals", userID, nil))
	if w.Code != 200 {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got []Eval
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %d", len(got))
	}
}

func TestHandleAgentEvals_Create_GeneratesIDAndPersists(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	w := httptest.NewRecorder()
	body := map[string]any{
		"name":        "Greet flow",
		"description": "Greet the user politely",
		"goals":       []string{"says hello"},
	}
	s.handleAgentEvals(w, evalsAuthedReq("POST", "/instances/"+i64s(agent.ID)+"/evals", userID, body))
	if w.Code != 200 {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var created Eval
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" {
		t.Error("server should mint an ID when caller omits one")
	}
	if !strings.HasPrefix(created.ID, "usr-") {
		t.Errorf("unexpected id shape %q", created.ID)
	}
	if created.AgentID != agent.ID {
		t.Errorf("agent_id should be set from URL, got %d", created.AgentID)
	}
	if created.Source != "user" {
		t.Errorf("source should be 'user', got %q", created.Source)
	}

	// Confirm it's actually in the DB.
	got, _ := s.store.GetAgentEval(created.ID)
	if got == nil {
		t.Error("create did not persist")
	}
}

func TestHandleAgentEvals_Create_NameRequired(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	w := httptest.NewRecorder()
	body := map[string]any{"description": "x", "goals": []string{"g"}}
	s.handleAgentEvals(w, evalsAuthedReq("POST", "/instances/"+i64s(agent.ID)+"/evals", userID, body))
	if w.Code != 400 {
		t.Errorf("expected 400 for missing name, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAgentEvals_Get_Update_Delete(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	ev, _ := s.store.CreateAgentEval(sampleEval(agent.ID, "ev-crud", "Crud"))

	// GET
	w := httptest.NewRecorder()
	s.handleAgentEvals(w, evalsAuthedReq("GET", "/instances/"+i64s(agent.ID)+"/evals/"+ev.ID, userID, nil))
	if w.Code != 200 {
		t.Errorf("GET status: %d body=%s", w.Code, w.Body.String())
	}

	// PUT
	w = httptest.NewRecorder()
	body := map[string]any{
		"name":        "Renamed via HTTP",
		"description": "updated",
		"goals":       []string{"goal a"},
		"max_turns":   7,
	}
	s.handleAgentEvals(w, evalsAuthedReq("PUT", "/instances/"+i64s(agent.ID)+"/evals/"+ev.ID, userID, body))
	if w.Code != 200 {
		t.Fatalf("PUT status: %d body=%s", w.Code, w.Body.String())
	}
	got, _ := s.store.GetAgentEval(ev.ID)
	if got.Name != "Renamed via HTTP" {
		t.Errorf("update didn't persist name; got %q", got.Name)
	}
	if got.MaxTurns != 7 {
		t.Errorf("update didn't persist max_turns; got %d", got.MaxTurns)
	}

	// DELETE
	w = httptest.NewRecorder()
	s.handleAgentEvals(w, evalsAuthedReq("DELETE", "/instances/"+i64s(agent.ID)+"/evals/"+ev.ID, userID, nil))
	if w.Code != 200 {
		t.Errorf("DELETE status: %d body=%s", w.Code, w.Body.String())
	}
	if got, _ := s.store.GetAgentEval(ev.ID); got != nil {
		t.Error("DELETE did not remove the row")
	}
}

func TestHandleAgentEvals_PUT_PreservesSource(t *testing.T) {
	// Operator must not be able to relabel a template-seeded row as
	// 'user' or vice versa via PUT — provenance is server-owned.
	s, userID, agent := setupServerWithAgent(t)
	seed := sampleEval(agent.ID, "ev-src", "seeded")
	seed.Source = "template"
	seed.SourceRef = "tpl-x"
	s.store.CreateAgentEval(seed)

	w := httptest.NewRecorder()
	body := map[string]any{
		"name":        "renamed",
		"description": "x",
		"goals":       []string{"g"},
		"source":      "user",    // operator tries to relabel
		"source_ref":  "spoofed", // operator tries to overwrite
	}
	s.handleAgentEvals(w, evalsAuthedReq("PUT", "/instances/"+i64s(agent.ID)+"/evals/"+seed.ID, userID, body))
	if w.Code != 200 {
		t.Fatalf("PUT status: %d body=%s", w.Code, w.Body.String())
	}
	got, _ := s.store.GetAgentEval(seed.ID)
	if got.Source != "template" {
		t.Errorf("source got overwritten: %q (handler should preserve)", got.Source)
	}
	if got.SourceRef != "tpl-x" {
		t.Errorf("source_ref got overwritten: %q (handler should preserve)", got.SourceRef)
	}
}

func TestHandleAgentEvals_ForeignAgent_ReturnsNotFound(t *testing.T) {
	// Two users, each with their own agent. User A must not be able
	// to read/list/create evals against User B's agent — the table is
	// indexed by id but the handler scopes via ownership check.
	s := newTestServer(t)
	uA, _ := s.store.CreateUser("a@example.com", "x")
	uB, _ := s.store.CreateUser("b@example.com", "x")
	agentB, _ := s.store.CreateAgent(uB.ID, "B's agent", "", "autonomous", "{}", "")

	w := httptest.NewRecorder()
	s.handleAgentEvals(w, evalsAuthedReq("GET", "/instances/"+i64s(agentB.ID)+"/evals", uA.ID, nil))
	if w.Code != 404 {
		t.Errorf("user A reading user B's evals should 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAgentEvals_MalformedAgentID(t *testing.T) {
	s, userID, _ := setupServerWithAgent(t)
	w := httptest.NewRecorder()
	s.handleAgentEvals(w, evalsAuthedReq("GET", "/instances/not-a-number/evals", userID, nil))
	if w.Code != 400 {
		t.Errorf("expected 400 for malformed agent id, got %d", w.Code)
	}
}

// ─── handleEvalMockGateway ────────────────────────────────────────

// jsonRPCCall builds the inner POST that a spawned core would emit
// against the eval-mock gateway URL.
func jsonRPCCall(t *testing.T, s *Server, token, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/eval-mock-gateway/"+token+"/mcp", bytes.NewReader(buf))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleEvalMockGateway(w, r)
	if w.Code != 200 {
		t.Fatalf("gateway non-200 for %s: %d body=%s", method, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	return out
}

func TestHandleEvalMockGateway_Initialize(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-init"
	sess := newRunSession(&Eval{ID: "ev-init"})
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "initialize", map[string]any{})
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("missing result: %v", resp)
	}
	if pv, _ := result["protocolVersion"].(string); pv == "" {
		t.Error("initialize should report a protocolVersion")
	}
}

func TestHandleEvalMockGateway_ToolsList_OneEntryPerMock(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-tools"
	sess := newRunSession(&Eval{
		ID: "ev-tools",
		Mocks: []EvalMock{
			{App: "messaging", Tool: "send", Return: json.RawMessage(`{}`)},
			{App: "messaging", Tool: "send", ArgsMatch: map[string]any{"to": "alice"}, Return: json.RawMessage(`{}`)}, // dup
			{App: "email", Tool: "send", Return: json.RawMessage(`{}`)},
		},
	})
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "tools/list", map[string]any{})
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("want 2 unique tools, got %d: %v", len(tools), tools)
	}
	names := map[string]bool{}
	for _, raw := range tools {
		t := raw.(map[string]any)
		names[t["name"].(string)] = true
	}
	if !names["messaging.send"] || !names["email.send"] {
		t.Errorf("unexpected tool names: %v", names)
	}
}

func TestHandleEvalMockGateway_ToolsCall_MockHit_WrapsResponse(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-call-hit"
	sess := newRunSession(&Eval{
		ID: "ev-hit",
		Mocks: []EvalMock{{
			App: "messaging", Tool: "send",
			Return: json.RawMessage(`{"id":"m_42"}`),
		}},
	})
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "tools/call", map[string]any{
		"name":      "messaging.send",
		"arguments": map[string]any{"to": "alice"},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("MCP envelope should have one content part, got %d", len(content))
	}
	part := content[0].(map[string]any)
	if part["type"] != "text" {
		t.Errorf("unexpected type %v", part["type"])
	}
	if text, _ := part["text"].(string); text != `{"id":"m_42"}` {
		t.Errorf("inner JSON mismatch: %q", text)
	}

	// Trajectory got the call recorded with Mocked=true.
	snap := sess.snapshot()
	if len(snap.Turns) != 1 || snap.Turns[0].ToolCall == nil {
		t.Fatalf("expected one tool turn recorded, got %+v", snap.Turns)
	}
	tc := snap.Turns[0].ToolCall
	if !tc.Mocked || tc.App != "messaging" || tc.Tool != "send" {
		t.Errorf("trajectory record wrong: %+v", tc)
	}
}

func TestHandleEvalMockGateway_ToolsCall_NoMatch_StubDefault(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-call-miss"
	sess := newRunSession(&Eval{ID: "ev-miss"})
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "tools/call", map[string]any{
		"name":      "weather.forecast",
		"arguments": map[string]any{"city": "Berlin"},
	})
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("non-strict miss should not produce JSON-RPC error: %v", resp)
	}
	snap := sess.snapshot()
	tc := snap.Turns[0].ToolCall
	if tc.Mocked {
		t.Error("stub-default path must record Mocked=false")
	}
	if tc.Warning == "" {
		t.Error("stub-default path must surface a warning")
	}
}

func TestHandleEvalMockGateway_ToolsCall_StrictMiss_ReturnsRPCError(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-strict"
	sess := newRunSession(&Eval{ID: "ev-strict"})
	sess.strict = true
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "tools/call", map[string]any{
		"name":      "weather.forecast",
		"arguments": map[string]any{},
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("strict miss should produce JSON-RPC error envelope: %v", resp)
	}
	if len(sess.strictViolations) != 1 {
		t.Errorf("violations recorded: %d", len(sess.strictViolations))
	}
}

func TestHandleEvalMockGateway_ToolsCall_DeclaredError(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-err"
	sess := newRunSession(&Eval{
		ID: "ev-err",
		Mocks: []EvalMock{{
			App: "messaging", Tool: "send", Error: "rate limited",
		}},
	})
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "tools/call", map[string]any{
		"name":      "messaging.send",
		"arguments": map[string]any{"to": "alice"},
	})
	errBlock, hasErr := resp["error"].(map[string]any)
	if !hasErr {
		t.Fatalf("declared-error mock should produce JSON-RPC error: %v", resp)
	}
	if msg, _ := errBlock["message"].(string); msg != "rate limited" {
		t.Errorf("error message round-trip mismatch: %q", msg)
	}
}

func TestHandleEvalMockGateway_UnknownToken(t *testing.T) {
	s := newTestServer(t)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	r := httptest.NewRequest("POST", "/eval-mock-gateway/no-such-token/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleEvalMockGateway(w, r)
	if w.Code != 404 {
		t.Errorf("unknown token should 404, got %d", w.Code)
	}
}

func TestHandleEvalMockGateway_MissingToken(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest("POST", "/eval-mock-gateway/", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.handleEvalMockGateway(w, r)
	if w.Code != 400 {
		t.Errorf("missing token should 400, got %d", w.Code)
	}
}

func TestHandleEvalMockGateway_UnknownMethod(t *testing.T) {
	s := newTestServer(t)
	tok := "tok-meth"
	registerEvalSession(tok, newRunSession(&Eval{ID: "ev-meth"}))
	t.Cleanup(func() { unregisterEvalSession(tok) })

	resp := jsonRPCCall(t, s, tok, "completions/create", map[string]any{})
	errBlock, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method should produce error: %v", resp)
	}
	if !strings.Contains(errBlock["message"].(string), "method not found") {
		t.Errorf("unexpected error: %v", errBlock)
	}
}

// ─── handleApplyEvalSuggestions ───────────────────────────────────

func TestHandleApplyEvalSuggestions_AppendsDirectiveAndWritesHistory(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)

	ev, _ := s.store.CreateAgentEval(sampleEval(agent.ID, "ev-apply", "Apply"))
	runID, _ := s.store.InsertEvalRun(EvalRun{
		EvalID:    ev.ID,
		StartedAt: time.Now(),
		Status:    "fail",
		Suggestions: &RunSuggestions{
			DirectiveEdits: []DirectiveEditSuggestion{
				{ID: "edit-1", Add: "Always greet the user by name."},
				{ID: "edit-2", Add: "Confirm before acting."},
			},
		},
	})

	body := map[string]any{"directive_edit_ids": []string{"edit-1"}}
	w := httptest.NewRecorder()
	s.handleApplyEvalSuggestions(w, evalsAuthedReq("POST", "/", userID, body), userID, agent, ev, runID)
	if w.Code != 200 {
		t.Fatalf("apply status: %d body=%s", w.Code, w.Body.String())
	}

	// Directive on the agent should now end with the applied edit.
	got, _ := s.store.GetAgentByID(agent.ID)
	if !strings.Contains(got.Directive, "Always greet the user by name.") {
		t.Errorf("directive missing applied edit: %q", got.Directive)
	}
	if strings.Contains(got.Directive, "Confirm before acting.") {
		t.Errorf("directive contains unselected edit: %q", got.Directive)
	}

	// History row was written with source='eval_suggestion'.
	var count int
	s.store.db.QueryRow(
		`SELECT COUNT(*) FROM agent_directive_history WHERE agent_id = ? AND source = 'eval_suggestion'`,
		agent.ID,
	).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 audit row, got %d", count)
	}
}

func TestHandleApplyEvalSuggestions_UnknownIDRejected(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	ev, _ := s.store.CreateAgentEval(sampleEval(agent.ID, "ev-bad-id", "x"))
	runID, _ := s.store.InsertEvalRun(EvalRun{
		EvalID: ev.ID, StartedAt: time.Now(), Status: "fail",
		Suggestions: &RunSuggestions{DirectiveEdits: []DirectiveEditSuggestion{
			{ID: "edit-1", Add: "x"},
		}},
	})

	body := map[string]any{"directive_edit_ids": []string{"edit-99"}}
	w := httptest.NewRecorder()
	s.handleApplyEvalSuggestions(w, evalsAuthedReq("POST", "/", userID, body), userID, agent, ev, runID)
	if w.Code != 400 {
		t.Errorf("unknown edit id should 400, got %d body=%s", w.Code, w.Body.String())
	}

	// Directive must not have been touched.
	got, _ := s.store.GetAgentByID(agent.ID)
	if got.Directive != "be polite" {
		t.Errorf("directive mutated despite rejected apply: %q", got.Directive)
	}
}

func TestHandleApplyEvalSuggestions_NoSuggestionsOnRun(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	ev, _ := s.store.CreateAgentEval(sampleEval(agent.ID, "ev-no-sug", "x"))
	runID, _ := s.store.InsertEvalRun(EvalRun{
		EvalID: ev.ID, StartedAt: time.Now(), Status: "pass",
	}) // no Suggestions

	body := map[string]any{"directive_edit_ids": []string{"edit-1"}}
	w := httptest.NewRecorder()
	s.handleApplyEvalSuggestions(w, evalsAuthedReq("POST", "/", userID, body), userID, agent, ev, runID)
	if w.Code != 400 {
		t.Errorf("expected 400 when run has no suggestions, got %d", w.Code)
	}
}

func TestHandleApplyEvalSuggestions_EmptyIDList(t *testing.T) {
	s, userID, agent := setupServerWithAgent(t)
	ev, _ := s.store.CreateAgentEval(sampleEval(agent.ID, "ev-empty-ids", "x"))
	runID, _ := s.store.InsertEvalRun(EvalRun{
		EvalID: ev.ID, StartedAt: time.Now(), Status: "fail",
		Suggestions: &RunSuggestions{DirectiveEdits: []DirectiveEditSuggestion{{ID: "edit-1", Add: "x"}}},
	})

	body := map[string]any{"directive_edit_ids": []string{}}
	w := httptest.NewRecorder()
	s.handleApplyEvalSuggestions(w, evalsAuthedReq("POST", "/", userID, body), userID, agent, ev, runID)
	if w.Code != 400 {
		t.Errorf("empty id list should 400, got %d", w.Code)
	}
}
