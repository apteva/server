package main

// eval_pure_test.go — pure-function tests for the eval system. No
// SQLite, no HTTP, no spawned cores. Covers the bits that misbehave
// silently and that the rest of the system trusts:
//
//   - matchMock / looseEqual — silent argument-matching bugs here
//     would make every mock either over- or under-fire and break the
//     trajectory the judge sees.
//   - runSession recording + snapshot — the trajectory is what the
//     judge grades; if turns vanish or get out of order the verdict
//     is meaningless.
//   - The two in-memory registries (eval_sessions, eval_control) —
//     missing register/unregister would leak runs across tests.
//   - parseJudgeReply — tolerates the prose/codefence irregularities
//     the judge LLM emits even with a strict system prompt.
//   - buildJudgePrompt — the [improvements: on/off] flag and trajectory
//     rendering shape the judge's actual behaviour.
//   - finalRollup / triggerToText / newEvalRunID — small but
//     load-bearing helpers.

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─── matchMock ────────────────────────────────────────────────────

func TestMatchMock_NoArgsMatch_FirstAppToolWins(t *testing.T) {
	mocks := []EvalMock{
		{App: "messaging", Tool: "send", Return: json.RawMessage(`{"id":1}`)},
		{App: "messaging", Tool: "send", Return: json.RawMessage(`{"id":2}`)},
	}
	got, ok := matchMock(mocks, "messaging", "send", json.RawMessage(`{"to":"alice"}`))
	if !ok {
		t.Fatal("expected match")
	}
	if string(got.Return) != `{"id":1}` {
		t.Errorf("first match should win, got %s", got.Return)
	}
}

func TestMatchMock_AppOrToolMismatch_NoMatch(t *testing.T) {
	mocks := []EvalMock{{App: "messaging", Tool: "send", Return: json.RawMessage(`{}`)}}
	if _, ok := matchMock(mocks, "messaging", "delete", nil); ok {
		t.Error("tool mismatch should not match")
	}
	if _, ok := matchMock(mocks, "email", "send", nil); ok {
		t.Error("app mismatch should not match")
	}
}

func TestMatchMock_ArgsMatch_SubsetSemantics(t *testing.T) {
	// ArgsMatch is a SUBSET — args may carry extra fields the mock
	// doesn't pin. This lets operators say "match any send to alice"
	// without enumerating every other field the agent passes.
	mocks := []EvalMock{{
		App:       "messaging",
		Tool:      "send",
		ArgsMatch: map[string]any{"to": "alice"},
		Return:    json.RawMessage(`{"ok":true}`),
	}}
	args := json.RawMessage(`{"to":"alice","body":"hi","priority":2}`)
	if _, ok := matchMock(mocks, "messaging", "send", args); !ok {
		t.Error("subset ArgsMatch should match when all pinned keys are present")
	}
}

func TestMatchMock_ArgsMatch_MissingKeyFails(t *testing.T) {
	mocks := []EvalMock{{
		App:       "messaging",
		Tool:      "send",
		ArgsMatch: map[string]any{"to": "alice"},
		Return:    json.RawMessage(`{"ok":true}`),
	}}
	if _, ok := matchMock(mocks, "messaging", "send", json.RawMessage(`{"body":"hi"}`)); ok {
		t.Error("missing pinned key should fail the match")
	}
}

func TestMatchMock_ArgsMatch_ValueMismatchFails(t *testing.T) {
	mocks := []EvalMock{{
		App:       "messaging",
		Tool:      "send",
		ArgsMatch: map[string]any{"to": "alice"},
		Return:    json.RawMessage(`{"ok":true}`),
	}}
	if _, ok := matchMock(mocks, "messaging", "send", json.RawMessage(`{"to":"bob"}`)); ok {
		t.Error("differing pinned value should fail the match")
	}
}

func TestMatchMock_FallsThroughToNextMockWhenArgsMismatch(t *testing.T) {
	mocks := []EvalMock{
		{App: "messaging", Tool: "send", ArgsMatch: map[string]any{"to": "bob"}, Return: json.RawMessage(`{"who":"bob"}`)},
		{App: "messaging", Tool: "send", ArgsMatch: map[string]any{"to": "alice"}, Return: json.RawMessage(`{"who":"alice"}`)},
	}
	got, ok := matchMock(mocks, "messaging", "send", json.RawMessage(`{"to":"alice"}`))
	if !ok {
		t.Fatal("expected second mock to match")
	}
	if string(got.Return) != `{"who":"alice"}` {
		t.Errorf("unexpected match: %s", got.Return)
	}
}

func TestMatchMock_EmptyMockList(t *testing.T) {
	if _, ok := matchMock(nil, "x", "y", nil); ok {
		t.Error("empty mock list should never match")
	}
}

// ─── looseEqual ───────────────────────────────────────────────────

func TestLooseEqual_StringifiedEquivalence(t *testing.T) {
	// looseEqual exists because JSON args land as float64 / string /
	// bool from json.Unmarshal but operators write ArgsMatch literals
	// in YAML / JSON-as-map[string]any with arbitrary numeric types.
	// We compare via fmt.Sprintf so "1" matches 1, 1.0 matches 1, etc.
	cases := []struct {
		a, b any
		want bool
	}{
		{"alice", "alice", true},
		{1, 1, true},
		{float64(1), 1, true},
		{1.0, "1", true},
		{true, true, true},
		{"alice", "bob", false},
		{1, 2, false},
		{nil, nil, true},
		{nil, "", false}, // <nil> vs empty string stringify differently
	}
	for _, c := range cases {
		if got := looseEqual(c.a, c.b); got != c.want {
			t.Errorf("looseEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// ─── runSession ───────────────────────────────────────────────────

func TestRunSession_RecordsUserAgentJudgeSystem_InOrder(t *testing.T) {
	ev := &Eval{ID: "ev-1", MaxTurns: 5}
	s := newRunSession(ev)
	s.recordUser("brief")
	s.recordAgent("acknowledged")
	s.recordSystem("--- iteration 2 of 5 ---")
	s.recordJudge(2, "you forgot to greet")
	s.recordAgent("hi there")

	snap := s.snapshot()
	if len(snap.Turns) != 5 {
		t.Fatalf("want 5 turns, got %d", len(snap.Turns))
	}
	wantRoles := []string{"user", "agent", "system", "judge", "agent"}
	for i, want := range wantRoles {
		if snap.Turns[i].Role != want {
			t.Errorf("turn %d role: want %q, got %q", i, want, snap.Turns[i].Role)
		}
	}
	if snap.Turns[3].Iteration != 2 {
		t.Errorf("judge turn iteration = %d, want 2", snap.Turns[3].Iteration)
	}
	if s.turnsUsed != 2 {
		t.Errorf("turnsUsed = %d, want 2 (one per agent reply)", s.turnsUsed)
	}
}

func TestRunSession_NewMaxTurnsDefaultsTo5(t *testing.T) {
	ev := &Eval{ID: "ev-2", MaxTurns: 0}
	s := newRunSession(ev)
	if s.maxTurns != 5 {
		t.Errorf("default maxTurns = %d, want 5", s.maxTurns)
	}
	if ev.MaxTurns != 5 {
		t.Errorf("newRunSession should normalise ev.MaxTurns to 5, got %d", ev.MaxTurns)
	}
}

func TestRunSession_Snapshot_IsACopy(t *testing.T) {
	// snapshot must hand callers a copy — the runner takes one
	// mid-run to pass to the judge, while the gateway may still be
	// appending tool calls. A shared slice header here would race.
	s := newRunSession(&Eval{ID: "ev-3"})
	s.recordUser("a")
	snap := s.snapshot()
	s.recordUser("b")
	if len(snap.Turns) != 1 {
		t.Errorf("snapshot mutated after later record; want len 1 got %d", len(snap.Turns))
	}
}

func TestRunSession_ResolveToolCall_MockHit_RecordsResponse(t *testing.T) {
	ev := &Eval{
		ID: "ev-4",
		Mocks: []EvalMock{{
			App: "messaging", Tool: "send", Return: json.RawMessage(`{"id":42}`),
		}},
	}
	s := newRunSession(ev)
	rec := s.resolveToolCall("messaging", "send", json.RawMessage(`{}`))
	if !rec.Mocked {
		t.Error("expected Mocked=true on a match")
	}
	if string(rec.Response) != `{"id":42}` {
		t.Errorf("unexpected response: %s", rec.Response)
	}
	if rec.Error != "" {
		t.Errorf("expected no error, got %q", rec.Error)
	}
	snap := s.snapshot()
	if len(snap.Turns) != 1 || snap.Turns[0].Role != "tool" {
		t.Fatal("tool call should have been recorded as a turn")
	}
}

func TestRunSession_ResolveToolCall_MockError_RecordsError(t *testing.T) {
	ev := &Eval{
		ID: "ev-5",
		Mocks: []EvalMock{{
			App: "messaging", Tool: "send", Error: "rate limited",
		}},
	}
	s := newRunSession(ev)
	rec := s.resolveToolCall("messaging", "send", nil)
	if rec.Error != "rate limited" {
		t.Errorf("expected error 'rate limited', got %q", rec.Error)
	}
	if !rec.Mocked {
		t.Error("declared-error mock should still set Mocked=true")
	}
	if len(rec.Response) != 0 {
		t.Errorf("error path must not carry a response: %s", rec.Response)
	}
}

func TestRunSession_ResolveToolCall_NoMatch_StubDefault(t *testing.T) {
	s := newRunSession(&Eval{ID: "ev-6"})
	rec := s.resolveToolCall("unknown", "tool", json.RawMessage(`{"x":1}`))
	if rec.Mocked {
		t.Error("Mocked must be false on stub-default path")
	}
	if rec.Warning == "" {
		t.Error("stub-default must surface a warning so the trajectory is honest")
	}
	if string(rec.Response) != `{"ok":true,"_stub":true}` {
		t.Errorf("unexpected stub body: %s", rec.Response)
	}
}

func TestRunSession_ResolveToolCall_StrictMode_RecordsViolation(t *testing.T) {
	s := newRunSession(&Eval{ID: "ev-7"})
	s.strict = true
	rec := s.resolveToolCall("unknown", "tool", nil)
	if rec.Error == "" {
		t.Error("strict mode must surface an error to the agent")
	}
	if !strings.Contains(rec.Error, "strict mocks") {
		t.Errorf("error should mention strict mocks, got %q", rec.Error)
	}
	if len(s.strictViolations) != 1 {
		t.Errorf("violations count = %d, want 1", len(s.strictViolations))
	}
}

// ─── Eval session registry ────────────────────────────────────────

func TestEvalSessionRegistry_RoundTrip(t *testing.T) {
	tok := "tok-" + t.Name()
	sess := newRunSession(&Eval{ID: "ev-r"})
	if got := lookupEvalSession(tok); got != nil {
		t.Fatal("registry should start empty for a fresh token")
	}
	registerEvalSession(tok, sess)
	t.Cleanup(func() { unregisterEvalSession(tok) })
	if got := lookupEvalSession(tok); got != sess {
		t.Error("lookup should return the registered session")
	}
	unregisterEvalSession(tok)
	if got := lookupEvalSession(tok); got != nil {
		t.Error("lookup should return nil after unregister")
	}
}

// ─── Eval control registry + run id minting ───────────────────────

func TestEvalControlRegistry_RoundTrip(t *testing.T) {
	id := newEvalRunID()
	ch := make(chan StepDecision, 1)
	if got := lookupEvalControl(id); got != nil {
		t.Fatal("registry should start empty")
	}
	registerEvalControl(id, ch)
	t.Cleanup(func() { unregisterEvalControl(id) })
	if got := lookupEvalControl(id); got != ch {
		t.Error("lookup should return the registered channel")
	}
	unregisterEvalControl(id)
	if got := lookupEvalControl(id); got != nil {
		t.Error("lookup should return nil after unregister")
	}
}

func TestNewEvalRunID_Format(t *testing.T) {
	// Distinctness is best-effort and depends on wall-clock resolution
	// — on some platforms two back-to-back calls land in the same
	// nanosecond. The prefix + non-empty suffix is the load-bearing
	// contract (the routing in evalControlByRun uses the full string).
	id := newEvalRunID()
	if !strings.HasPrefix(id, "er-") {
		t.Errorf("id should be er-prefixed, got %q", id)
	}
	if len(id) <= len("er-") {
		t.Errorf("id has no suffix: %q", id)
	}
}

// ─── finalRollup ──────────────────────────────────────────────────

func TestFinalRollup_NilsEmptyButPreservesNonEmpty(t *testing.T) {
	if finalRollup(nil) != nil {
		t.Error("nil in → nil out")
	}
	if got := finalRollup(&RunSuggestions{}); got != nil {
		t.Error("empty suggestions should collapse to nil so suggestions_json stays NULL")
	}
	in := &RunSuggestions{DirectiveEdits: []DirectiveEditSuggestion{{ID: "edit-1", Add: "be polite"}}}
	if got := finalRollup(in); got != in {
		t.Error("non-empty suggestions should pass through unchanged")
	}
}

// ─── triggerToText ────────────────────────────────────────────────

func TestTriggerToText_AllVariants(t *testing.T) {
	cases := []struct {
		name    string
		in      EvalTrigger
		contain []string
	}{
		{
			name: "chat_message_full",
			in: EvalTrigger{Type: "chat_message", Payload: map[string]any{
				"text": "hello", "from": "alice", "channel": "general",
			}},
			contain: []string{"Incoming chat message", "alice", "general", "hello"},
		},
		{
			name:    "chat_message_minimal",
			in:      EvalTrigger{Type: "chat_message", Payload: map[string]any{"text": "ping"}},
			contain: []string{"Incoming chat message", "ping"},
		},
		{
			name:    "webhook",
			in:      EvalTrigger{Type: "webhook", Payload: map[string]any{"event": "push"}},
			contain: []string{"Incoming webhook event", "push"},
		},
		{
			name:    "scheduled_tick",
			in:      EvalTrigger{Type: "scheduled_tick", Payload: map[string]any{"iso_time": "2026-05-16T10:00:00Z"}},
			contain: []string{"Scheduled tick", "2026-05-16T10:00:00Z"},
		},
		{
			name:    "unknown_falls_back_to_generic",
			in:      EvalTrigger{Type: "custom_thing", Payload: map[string]any{"k": "v"}},
			contain: []string{"Event of type custom_thing", "\"k\""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := triggerToText(c.in)
			for _, want := range c.contain {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q: %s", want, out)
				}
			}
		})
	}
}

// ─── parseJudgeReply ──────────────────────────────────────────────

func TestParseJudgeReply_PlainJSON(t *testing.T) {
	raw := `{"overall":"pass","reasoning":"good","per_goal":[{"goal":"greet","verdict":"pass","why":"said hi"}]}`
	v, err := parseJudgeReply(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Overall != "pass" || len(v.PerGoal) != 1 || v.PerGoal[0].Verdict != "pass" {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseJudgeReply_StripsCodeFence(t *testing.T) {
	raw := "```json\n{\"overall\":\"fail\",\"per_goal\":[{\"goal\":\"x\",\"verdict\":\"fail\",\"why\":\"\"}]}\n```"
	v, err := parseJudgeReply(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Overall != "fail" {
		t.Errorf("unexpected overall: %s", v.Overall)
	}
}

func TestParseJudgeReply_StripsLeadingProse(t *testing.T) {
	raw := `Here is the JSON: {"overall":"pass","per_goal":[]}`
	v, err := parseJudgeReply(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Overall != "pass" {
		t.Errorf("unexpected: %+v", v)
	}
}

func TestParseJudgeReply_RollsUpOverallFromPerGoal(t *testing.T) {
	// Model forgot "overall" — parser derives it: any fail → fail.
	raw := `{"per_goal":[{"goal":"a","verdict":"pass","why":""},{"goal":"b","verdict":"fail","why":""}]}`
	v, err := parseJudgeReply(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Overall != "fail" {
		t.Errorf("rollup should produce fail when any per_goal fails, got %s", v.Overall)
	}
}

func TestParseJudgeReply_AllPassRollupIsPass(t *testing.T) {
	raw := `{"per_goal":[{"goal":"a","verdict":"pass","why":""}]}`
	v, err := parseJudgeReply(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Overall != "pass" {
		t.Errorf("rollup should produce pass when every per_goal passes, got %s", v.Overall)
	}
}

func TestParseJudgeReply_GarbageReturnsError(t *testing.T) {
	if _, err := parseJudgeReply("nope not json at all"); err == nil {
		t.Error("expected error on garbage input")
	}
}

// ─── buildJudgePrompt ─────────────────────────────────────────────

func TestBuildJudgePrompt_RendersAllSections(t *testing.T) {
	ev := &Eval{
		Description: "greet the user politely",
		Goals:       []string{"says hello", "addresses the user by name"},
	}
	traj := Trajectory{Turns: []TrajectoryTurn{
		{Role: "user", Content: "hi"},
		{Role: "agent", Content: "Hello, Alice!"},
		{Role: "tool", ToolCall: &ToolCallRecord{
			App: "messaging", Tool: "send",
			Args:     json.RawMessage(`{"to":"alice"}`),
			Response: json.RawMessage(`{"ok":true}`),
			Mocked:   true,
		}},
		{Role: "judge", Iteration: 2, Content: "try again"},
		{Role: "system", Content: "iteration 2"},
	}}
	out := buildJudgePrompt(ev, traj, "Standing directive: be kind.", true)

	wantContains := []string{
		"# Description",
		"greet the user politely",
		"# Agent directive",
		"Standing directive: be kind.",
		"# Goals",
		"1. says hello",
		"2. addresses the user by name",
		"# Trajectory",
		"USER: hi",
		"AGENT: Hello, Alice!",
		"TOOL CALL: messaging.send",
		`"to":"alice"`,
		"[mocked]",
		"JUDGE FEEDBACK (iteration 2)",
		"SYSTEM: iteration 2",
		"[improvements: on]",
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestBuildJudgePrompt_ImprovementsOff(t *testing.T) {
	out := buildJudgePrompt(&Eval{Description: "x", Goals: []string{"y"}}, Trajectory{}, "", false)
	if !strings.Contains(out, "[improvements: off]") {
		t.Error("expected [improvements: off]")
	}
	if strings.Contains(out, "[improvements: on]") {
		t.Error("did not expect [improvements: on]")
	}
}

func TestBuildJudgePrompt_EmptyDirectiveRendersNone(t *testing.T) {
	out := buildJudgePrompt(&Eval{Description: "x", Goals: []string{"y"}}, Trajectory{}, "   ", true)
	if !strings.Contains(out, "# Agent directive\n(none)") {
		t.Error("empty/whitespace directive should render as (none)")
	}
}

func TestBuildJudgePrompt_ToolCallError_RendersErrorNotResponse(t *testing.T) {
	traj := Trajectory{Turns: []TrajectoryTurn{
		{Role: "tool", ToolCall: &ToolCallRecord{
			App: "x", Tool: "y", Error: "boom",
		}},
	}}
	out := buildJudgePrompt(&Eval{Description: "d", Goals: []string{"g"}}, traj, "", true)
	if !strings.Contains(out, "error: boom") {
		t.Errorf("expected error rendered, got:\n%s", out)
	}
	if strings.Contains(out, "response:") {
		t.Errorf("error path should not render a response line:\n%s", out)
	}
}
