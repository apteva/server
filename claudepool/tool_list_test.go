package claudepool

import (
	"encoding/json"
	"testing"
	"time"
)

var searchTool = Tool{Name: "search", Description: "Load tool schemas", InputSchema: json.RawMessage(`{"type":"object"}`)}

// withResults appends the assistant's calls and their results to req.
func withResults(req Request, calls []ToolCall, results ...string) Request {
	req.Messages = append(append([]Message(nil), req.Messages...), Message{Role: "assistant", ToolCalls: calls})
	for i, call := range calls {
		req.Messages = append(req.Messages, Message{Role: "tool", ToolCallID: call.ID, Text: results[i]})
	}
	return req
}

// The caller's tool list grows mid-turn, as when search_tools loads a
// schema. Claude reads the new list before it gets the result, and the
// tool it just found works in the same turn.
func TestToolsLoadedMidTurnReachClaude(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Tools: []Tool{searchTool}, Messages: []Message{user(`CALL search {} ;; lookup {"q":"a"}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "search" {
		t.Fatalf("first reply = %+v", r)
	}

	req = withResults(req, r.calls, "loaded lookup")
	req.Tools = []Tool{searchTool, lookupTool}
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "lookup" || r.calls[0].Arguments != `{"q":"a"}` {
		t.Fatalf("reply after the tool list grew = %+v", r)
	}

	req = withResults(req, r.calls, "found it")
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "got: loaded lookup | found it" || r.finish != "stop" {
		t.Fatalf("final reply = %+v; Claude could not run the tool it was given mid-turn", r)
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawned %d processes, want 1: the new list must reach the running process", n)
	}
}

// Claude Code answers a call to a tool it does not have by itself. The call
// must not reach the caller, which would run it while the model is told it
// failed.
func TestCallsClaudeAnswersItselfAreNotPassedOn(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user(`CALL nosuch {} | raw:lookup {}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("passed on calls Claude never made: %+v", r.calls)
	}
	if r.text != "got: NOTLISTED nosuch | NOTLISTED lookup" || r.finish != "stop" {
		t.Fatalf("reply = %+v", r)
	}

	// The session still matches Claude's copy: the next message continues it.
	req.Messages = append(req.Messages, assistant(r.text), user("again"))
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "echo: again" {
		t.Fatalf("next reply = %q", r.text)
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawned %d processes, want 1", n)
	}
}

// When one reply calls a tool Claude has and one it does not, only the call
// Claude makes reaches the caller.
func TestOnlyCallsClaudeMakesArePassedOn(t *testing.T) {
	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user(`CALL lookup {"q":"a"} | nosuch {}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "lookup" {
		t.Fatalf("reply = %+v", r)
	}
	r, err = tp.run(t, withResults(req, r.calls, "found it"))
	if err != nil {
		t.Fatal(err)
	}
	if r.text != "got: found it | NOTLISTED nosuch" || r.finish != "stop" {
		t.Fatalf("reply after the result = %+v", r)
	}
}

// A Claude Code that does not read a list it was told changed cannot call
// the new tool in that turn; the call is not passed on, and the process
// restarts with the current list at the next turn.
func TestUnreadToolListRestartsProcessAtNextTurn(t *testing.T) {
	t.Setenv("CLAUDEPOOL_FAKE_IGNORE_LIST_CHANGED", "1")
	old := relistTimeout
	relistTimeout = 200 * time.Millisecond
	t.Cleanup(func() { relistTimeout = old })

	tp := newTestPool(t, Limits{})
	req := Request{Owner: "a", Tools: []Tool{searchTool}, Messages: []Message{user(`CALL search {} ;; lookup {}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	req = withResults(req, r.calls, "loaded lookup")
	req.Tools = []Tool{searchTool, lookupTool}
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 || r.text != "got: loaded lookup | NOTLISTED lookup" || r.finish != "stop" {
		t.Fatalf("reply = %+v", r)
	}

	req.Messages = append(req.Messages, assistant(r.text), user("CALL lookup {}"))
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0].Name != "lookup" {
		t.Fatalf("next turn = %+v", r)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 2 || spawns[1].Mode != "resume" {
		t.Fatalf("spawns = %+v, want a resumed process with the current list", spawns)
	}
}
