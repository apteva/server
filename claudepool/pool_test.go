package claudepool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/claudepool/claudetest"
)

func TestMain(m *testing.M) {
	if os.Getenv("CLAUDEPOOL_FAKE") == "1" {
		os.Exit(claudetest.FakeMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type testPool struct {
	*Pool
	log string
}

func newTestPool(t *testing.T, limits Limits) *testPool {
	t.Helper()
	return newTestPoolWith(t, limits, nil)
}

func newTestPoolWith(t *testing.T, limits Limits, adjust func(*Config)) *testPool {
	t.Helper()
	t.Setenv("CLAUDEPOOL_FAKE", "1")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-claude")
	dir := t.TempDir()
	logFile := filepath.Join(dir, "spawns.log")
	t.Setenv("CLAUDEPOOL_FAKE_LOG", logFile)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var pool *Pool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := Config{
		CLI:         CLI{Binary: exe, ConfigDir: filepath.Join(dir, "claude-config")},
		WorkDir:     filepath.Join(dir, "work"),
		StateDir:    filepath.Join(dir, "state"),
		MCPURL:      srv.URL,
		ToolTimeout: 5 * time.Second,
		Limits:      limits,
		Logf:        t.Logf,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	pool, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &testPool{Pool: pool, log: logFile}
}

func (tp *testPool) spawns(t *testing.T) []claudetest.FakeLog {
	t.Helper()
	data, _ := os.ReadFile(tp.log)
	var out []claudetest.FakeLog
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry claudetest.FakeLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

type reply struct {
	text     string
	thinking string
	calls    []ToolCall
	finish   string
	usage    Usage
	model    string
}

func (tp *testPool) run(t *testing.T, req Request) (reply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var r reply
	err := tp.Run(ctx, req, func(ev Event) error {
		switch ev.Kind {
		case EventText:
			r.text += ev.Text
		case EventThinking:
			r.thinking += ev.Text
		case EventToolStart:
			if ev.ToolIndex != len(r.calls) {
				t.Errorf("tool index %d, want %d", ev.ToolIndex, len(r.calls))
			}
			r.calls = append(r.calls, ToolCall{ID: ev.ToolID, Name: ev.ToolName})
		case EventToolArgs:
			r.calls[ev.ToolIndex].Arguments += ev.Text
		case EventDone:
			r.finish, r.usage, r.model = ev.FinishReason, ev.Usage, ev.Model
		}
		return nil
	})
	return r, err
}

func user(text string) Message      { return Message{Role: "user", Text: text} }
func assistant(text string) Message { return Message{Role: "assistant", Text: text} }

var lookupTool = Tool{Name: "lookup", Description: "Look something up", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}

func TestConversationContinuesInSameSession(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "agent-1", Model: "opus", System: "You are a test.", Messages: []Message{user("hello")}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "echo: hello" || r.finish != "stop" || r.thinking != "hmm" {
		t.Fatalf("reply = %+v", r)
	}
	if r.usage.InputTokens != 10 || r.usage.CacheReadTokens != 100 || r.usage.CacheWriteTokens != 5 || r.usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", r.usage)
	}

	req.Messages = append(req.Messages, assistant(r.text), user("again"))
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "echo: again" {
		t.Fatalf("second reply = %q (want only the new message delivered)", r.text)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 1 {
		t.Fatalf("spawned %d processes, want 1: %+v", len(spawns), spawns)
	}
	s := spawns[0]
	if s.Mode != "new" || s.Model != "opus" || s.System != "You are a test." || s.NoMDs != "1" || s.HasAPIKey {
		t.Fatalf("spawn = %+v", s)
	}
	if s.ToolTimeout != msString(5*time.Second+time.Minute) {
		t.Fatalf("MCP_TOOL_TIMEOUT = %s", s.ToolTimeout)
	}
	if st := tp.Status(); st.Running != 1 || st.Sessions != 1 || st.Processes[0].Turns != 2 || st.Processes[0].Owner != "agent-1" {
		t.Fatalf("status = %+v", st)
	}
	if lim := tp.Limit(); lim == nil || lim.Type != "five_hour" || lim.Utilization == nil || *lim.Utilization != 0.42 {
		t.Fatalf("limit = %+v", lim)
	}
}

func TestToolCallsAreHeldUntilResultsArrive(t *testing.T) {
	tp := newTestPool(t, Limits{})
	other := Tool{Name: "other", InputSchema: json.RawMessage(`{"type":"object"}`)}
	req := Request{Owner: "agent-1", Tools: []Tool{lookupTool, other}, Messages: []Message{user(`CALL lookup {"q":"a"} | other {}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 2 {
		t.Fatalf("reply = %+v", r)
	}
	if r.calls[0].Name != "lookup" || r.calls[0].Arguments != `{"q":"a"}` || r.calls[1].Name != "other" || r.calls[1].Arguments != "{}" {
		t.Fatalf("calls = %+v", r.calls)
	}
	if st := tp.Status(); st.Processes[0].State != "waiting_tool" || st.Processes[0].Detail != "lookup, other" {
		t.Fatalf("status = %+v", st.Processes)
	}

	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "found it"},
		Message{Role: "tool", ToolCallID: r.calls[1].ID, Text: "broken", IsError: true},
	)
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "got: found it | ERR broken" || r.finish != "stop" {
		t.Fatalf("reply after tools = %+v", r)
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawned %d processes, want 1", n)
	}
}

func TestMissingToolResultsAreReportedAsErrors(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user(`CALL lookup {} | lookup {"q":"x"}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[1].ID, Text: "second"},
		user("also this"),
	)
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	want := "got: ERR No result was returned for this tool call. | second\n\n<new_messages>\nalso this\n</new_messages>"
	if r.text != want {
		t.Fatalf("reply = %q\nwant %q", r.text, want)
	}
}

func TestLongToolNamesGetAliases(t *testing.T) {
	tp := newTestPool(t, Limits{})
	long := Tool{Name: "integration.github." + strings.Repeat("create_pull_request_review_", 3), InputSchema: json.RawMessage(`{"type":"object"}`)}
	alias := exposedName(long.Name)
	if len(alias) > maxExposedName || invalidToolChars.MatchString(alias) {
		t.Fatalf("alias %q is not a valid tool name", alias)
	}
	req := Request{Owner: "a", Tools: []Tool{long}, Messages: []Message{user("CALL " + alias + " {}")}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0].Name != long.Name {
		t.Fatalf("calls = %+v", r.calls)
	}
}

func TestIdleProcessIsEvictedAndResumed(t *testing.T) {
	tp := newTestPool(t, Limits{MaxProcs: 1})
	a := Request{Owner: "a", Model: "opus", Messages: []Message{user("one")}}
	ra, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	b := Request{Owner: "b", Messages: []Message{user("two")}}
	if _, err := tp.run(t, b); err != nil {
		t.Fatal(err)
	}
	a.Messages = append(a.Messages, assistant(ra.text), user("three"))
	a.Model = "sonnet"
	ra, err = tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	if ra.text != "echo: three" {
		t.Fatalf("resumed reply = %q", ra.text)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 3 {
		t.Fatalf("spawns = %+v", spawns)
	}
	if spawns[2].Mode != "resume" || spawns[2].Session != spawns[0].Session || spawns[2].Model != "sonnet" {
		t.Fatalf("third spawn = %+v, first = %+v", spawns[2], spawns[0])
	}
	if st := tp.Status(); st.Running != 1 || st.Sessions != 2 {
		t.Fatalf("status = %+v", st)
	}
}

func TestBusyPoolQueuesThenFails(t *testing.T) {
	tp := newTestPool(t, Limits{MaxProcs: 1, QueueTimeout: 300 * time.Millisecond})
	a := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user("CALL lookup {}")}}
	ra, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tp.run(t, Request{Owner: "b", Messages: []Message{user("hi")}})
	var retry *RetryError
	if !errors.As(err, &retry) || !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("err = %v, want pool busy", err)
	}

	// Once a's turn completes its process is idle and b can take the slot.
	done := make(chan error, 1)
	go func() {
		_, err := tp.run(t, Request{Owner: "b", Messages: []Message{user("hi")}})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if st := tp.Status(); st.Waiting != 1 {
		t.Fatalf("waiting = %d, want 1", st.Waiting)
	}
	a.Messages = append(a.Messages, Message{Role: "assistant", ToolCalls: ra.calls}, Message{Role: "tool", ToolCallID: ra.calls[0].ID, Text: "ok"})
	tp.SetLimits(Limits{MaxProcs: 1, QueueTimeout: 10 * time.Second})
	if _, err := tp.run(t, a); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, ErrPoolBusy) {
		t.Fatal(err)
	}
}

func TestWaitingProcessGivesUpItsSlot(t *testing.T) {
	defer func(d time.Duration) { toolGrace = d }(toolGrace)
	toolGrace = 150 * time.Millisecond
	tp := newTestPool(t, Limits{MaxProcs: 1, QueueTimeout: 5 * time.Second})
	a := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user("CALL lookup {}")}}
	ra, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}

	// b waits out the grace period, then takes a's slot.
	start := time.Now()
	rb, err := tp.run(t, Request{Owner: "b", Messages: []Message{user("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if rb.text != "echo: hi" {
		t.Fatalf("b = %+v", rb)
	}
	if waited := time.Since(start); waited < toolGrace/2 {
		t.Fatalf("b took the slot after %s, before the grace period", waited)
	}
	for _, proc := range tp.Status().Processes {
		if proc.Owner == "a" {
			t.Fatalf("a still has a process: %+v", proc)
		}
	}

	// a's results arrive later and continue in a fresh session.
	a.Messages = append(a.Messages, Message{Role: "assistant", ToolCalls: ra.calls}, Message{Role: "tool", ToolCallID: ra.calls[0].ID, Text: "done"})
	r, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<conversation>", "[tool result for call " + ra.calls[0].ID + "]\ndone", "Continue from the last tool results above."} {
		if !strings.Contains(r.text, want) {
			t.Fatalf("a's reseed misses %q:\n%s", want, r.text)
		}
	}
	spawns := tp.spawns(t)
	if len(spawns) != 3 || spawns[2].Session == spawns[0].Session {
		t.Fatalf("spawns = %+v", spawns)
	}
}

func TestToolTimeoutClosesWaitingProcess(t *testing.T) {
	defer func(d time.Duration) { reapInterval = d }(reapInterval)
	reapInterval = 20 * time.Millisecond
	tp := newTestPoolWith(t, Limits{}, func(cfg *Config) { cfg.ToolTimeout = 200 * time.Millisecond })
	a := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user("CALL lookup {}")}}
	ra, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(tp.Status().Processes) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("process still running: %+v", tp.Status().Processes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := tp.Status(); st.Sessions != 0 {
		t.Fatalf("sessions = %d, want the timed-out one gone", st.Sessions)
	}

	a.Messages = append(a.Messages, Message{Role: "assistant", ToolCalls: ra.calls}, Message{Role: "tool", ToolCallID: ra.calls[0].ID, Text: "late"})
	r, err := tp.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "[tool result for call "+ra.calls[0].ID+"]\nlate") {
		t.Fatalf("late results were not carried over:\n%s", r.text)
	}
}

func TestUnknownHistoryIsReseeded(t *testing.T) {
	tp := newTestPool(t, Limits{})
	r, err := tp.run(t, Request{Owner: "a", Messages: []Message{
		user("first"),
		{Role: "assistant", Text: "calling", ToolCalls: []ToolCall{{ID: "call_1", Name: "lookup", Arguments: `{"q":1}`}}},
		{Role: "tool", ToolCallID: "call_1", Text: "42"},
		assistant("the answer is 42"),
		user("thanks"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<conversation>", "[user]\nfirst", "[assistant called tool lookup, id call_1]\n{\"q\":1}", "[tool result for call call_1]\n42", "[assistant]\nthe answer is 42", "</conversation>\n\nthanks"} {
		if !strings.Contains(r.text, want) {
			t.Fatalf("reseed transcript misses %q:\n%s", want, r.text)
		}
	}
}

func TestEditedHistoryStartsNewSession(t *testing.T) {
	tp := newTestPool(t, Limits{})
	r, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("one")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tp.run(t, Request{Owner: "a", Messages: []Message{user("one, edited"), assistant(r.text), user("two")}})
	if err != nil {
		t.Fatal(err)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 2 || spawns[1].Mode != "new" || spawns[1].Session == spawns[0].Session {
		t.Fatalf("spawns = %+v", spawns)
	}
}

func TestOwnersDoNotShareSessions(t *testing.T) {
	tp := newTestPool(t, Limits{})
	r, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("one")}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = tp.run(t, Request{Owner: "b", Messages: []Message{user("one"), assistant(r.text), user("two")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "<conversation>") {
		t.Fatalf("owner b continued owner a's session: %q", r.text)
	}
}

func TestCallerLeavingResetsSession(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Messages: []Message{user("one")}}
	ctx := context.Background()
	gone := errors.New("client gone")
	err := tp.Run(ctx, req, func(ev Event) error {
		if ev.Kind == EventText {
			return gone
		}
		return nil
	})
	if !errors.Is(err, gone) {
		t.Fatalf("err = %v", err)
	}
	r, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("one"), assistant("echo: one"), user("two")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "<conversation>") {
		t.Fatalf("expected a reseeded session, got %q", r.text)
	}
}

func TestNotSignedIn(t *testing.T) {
	tp := newTestPool(t, Limits{})
	t.Setenv("CLAUDEPOOL_FAKE_LOGGEDOUT", "1")
	_, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("hi")}})
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v", err)
	}
	if n := len(tp.spawns(t)); n != 0 {
		t.Fatalf("spawned %d processes while signed out", n)
	}
}

func TestLostLoginDuringTurn(t *testing.T) {
	tp := newTestPool(t, Limits{})
	_, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("AUTHFAIL")}})
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v", err)
	}
	// The pool recovers on the next request.
	if r, err := tp.run(t, Request{Owner: "a", Messages: []Message{user("hi")}}); err != nil || r.text != "echo: hi" {
		t.Fatalf("after recovery: %+v, %v", r, err)
	}
}

// Callers pick a model per request, usually with tool calls pending.
func TestModelSwitchesInPlace(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Model: "opus", Tools: []Tool{lookupTool}, Messages: []Message{user("CALL lookup {}")}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || r.model != "opus" {
		t.Fatalf("first reply = %+v", r)
	}

	req.Model = "haiku"
	req.Messages = append(req.Messages, Message{Role: "assistant", ToolCalls: r.calls}, Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "ok"})
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "got: ok" || r.model != "haiku" {
		t.Fatalf("reply after switching with a tool pending = %+v", r)
	}

	req.Model = "sonnet"
	req.Messages = append(req.Messages, assistant(r.text), user("more"))
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "echo: more" || r.model != "sonnet" {
		t.Fatalf("reply after switching between turns = %+v", r)
	}
	if spawns := tp.spawns(t); len(spawns) != 1 {
		t.Fatalf("spawned %d processes, want 1: %+v", len(spawns), spawns)
	}
	if st := tp.Status(); st.Processes[0].Model != "sonnet" {
		t.Fatalf("status model = %q", st.Processes[0].Model)
	}
}

func TestToolChangeRestartsProcessBetweenTurns(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Messages: []Message{user("one")}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, assistant(r.text), user("CALL lookup {}"))
	req.Tools = []Tool{lookupTool}
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("reply = %+v", r)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 2 || spawns[1].Mode != "resume" {
		t.Fatalf("spawns = %+v", spawns)
	}
}

func TestMCPRejectsUnknownToken(t *testing.T) {
	tp := newTestPool(t, Limits{})
	srv := httptest.NewServer(tp.Pool)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestPlanFor(t *testing.T) {
	s := newSession("a", "")
	s.history = []Message{user("one"), {Role: "assistant", ToolCalls: []ToolCall{{ID: "t1"}}}}
	s.awaiting = []ToolCall{{ID: "t1"}}
	msgs := append(append([]Message(nil), s.history...), Message{Role: "tool", ToolCallID: "t1", Text: "r"}, user("more"))
	p := s.planFor(msgs)
	if p.reseed || len(p.toolResults) != 1 || len(p.userText) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	// Moving on without tool results needs a fresh session.
	if p := s.planFor(append(append([]Message(nil), s.history...), user("skip"))); !p.reseed {
		t.Fatalf("plan = %+v", p)
	}
}

func TestSameMessageIgnoresReformatting(t *testing.T) {
	a := Message{Role: "assistant", Text: "x", ToolCalls: []ToolCall{{ID: "1", Arguments: `{"a": 1}`}, {ID: "2"}}}
	b := Message{Role: "assistant", Text: "x ", ToolCalls: []ToolCall{{ID: "2"}, {ID: "1", Arguments: `{"a":1}`}}}
	if !sameMessage(a, b) {
		t.Fatal("tool call order or argument formatting should not matter")
	}
	if !sameMessage(user("a  b\n"), user("a b")) {
		t.Fatal("whitespace should not matter")
	}
}

func TestProjectDirName(t *testing.T) {
	if got := projectDirName("/Users/x/.apteva/claude_sub/work"); got != "-Users-x--apteva-claude-sub-work" {
		t.Fatalf("got %q", got)
	}
}
