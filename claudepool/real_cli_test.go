package claudepool

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealCLI runs a short conversation through the installed, signed-in
// claude binary. It uses the subscription, so it only runs when asked:
//
//	CLAUDEPOOL_REAL_CLI=1 go test -run TestRealCLI -v ./claudepool
func TestRealCLI(t *testing.T) {
	tp := newRealPool(t, 2*time.Minute)
	pool := tp.Pool

	// A tool call is pending while the model changes.
	req := Request{
		Owner: "real", Model: "haiku", System: "You are a terse test assistant.", Tools: []Tool{lookupTool},
		Messages: []Message{user(`Call the lookup tool with q set to "x". After you get its result, reply with only the result text.`)},
	}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "lookup" || !strings.Contains(r.model, "haiku") {
		t.Fatalf("first reply = %+v", r)
	}
	req.Model = "sonnet"
	req.Messages = append(req.Messages, Message{Role: "assistant", ToolCalls: r.calls}, Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "PURPLE-42"})
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "PURPLE-42") || !strings.Contains(r.model, "sonnet") {
		t.Fatalf("reply after the tool result = %+v", r)
	}
	t.Logf("tool turn: %s, then %s; usage %+v", "haiku", r.model, r.usage)

	// Between turns, back to haiku in the same process.
	req.Model = "haiku"
	req.Messages = append(req.Messages, assistant(r.text), user("What was the result again? Reply with only it."))
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "PURPLE-42") || !strings.Contains(r.model, "haiku") {
		t.Fatalf("follow-up = %+v", r)
	}
	if st := pool.Status(); st.Running != 1 || st.Processes[0].Turns != 3 {
		t.Fatalf("status = %+v", st)
	}
}

// TestRealCLIToolsLoadedMidTurn checks that the installed claude reads a
// tool list that grows while a call is held, and calls the new tool in the
// same turn, as an agent does after search_tools loads a schema.
//
//	CLAUDEPOOL_REAL_CLI=1 go test -run TestRealCLIToolsLoadedMidTurn -v ./claudepool
func TestRealCLIToolsLoadedMidTurn(t *testing.T) {
	tp := newRealPool(t, 2*time.Minute)
	req := Request{
		Owner: "real-tools", Model: "haiku", System: "You are a terse test assistant.", Tools: []Tool{searchTool},
		Messages: []Message{user(`Call the search tool once. It makes a tool named lookup available. Then call lookup with q set to "x", and tell me what it returned.`)},
	}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "search" {
		t.Fatalf("first reply = %+v", r)
	}

	req = withResults(req, r.calls, `The lookup tool is now available. Call it with q set to "x".`)
	req.Tools = []Tool{searchTool, lookupTool}
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "lookup" {
		t.Fatalf("Claude did not call the tool loaded mid-turn: %+v", r)
	}

	req = withResults(req, r.calls, "PURPLE-42")
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "PURPLE-42") {
		t.Fatalf("final reply = %+v", r)
	}
	if st := tp.Status(); st.Running != 1 || st.Processes[0].Turns != 3 {
		t.Fatalf("status = %+v, want one process for the whole turn", st)
	}
}

// The tests below hold a tool call for several minutes, past Claude Code's
// silence limit (300s by default), so they only run when asked:
//
//	CLAUDEPOOL_REAL_CLI=1 CLAUDEPOOL_REAL_CLI_LONG=1 go test -run 'TestRealCLI(DroppedCall|ProgressKeepsCallAlive)' -v -timeout 20m ./claudepool

// TestRealCLIDroppedCall lets the installed claude give up on a silent held
// call and checks that the pool closes the process and the caller's next
// request reaches a Claude that has its history.
func TestRealCLIDroppedCall(t *testing.T) {
	skipUnlessLong(t)
	defer func(d time.Duration) { progressInterval = d }(progressInterval)
	progressInterval = time.Hour // silence
	// If Claude Code honours the idle setting, it gives up after about a
	// minute (it checks every 30s); otherwise after its default 300s.
	defer func(d time.Duration) { toolIdleTimeout = d }(toolIdleTimeout)
	toolIdleTimeout = 45 * time.Second
	tp := newRealPool(t, 10*time.Minute)
	r, req := realHeldCall(t, tp, "real-drop")

	start := time.Now()
	for len(tp.Status().Processes) > 0 {
		if time.Since(start) > 7*time.Minute {
			t.Fatalf("process still running: %+v", tp.Status().Processes)
		}
		time.Sleep(time.Second)
	}
	t.Logf("Claude gave up and the process closed %s after the tool call", time.Since(start).Round(time.Second))

	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "PURPLE-42"},
		user("Then, on a new line, also write BLUE-7."))
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "PURPLE-42") || !strings.Contains(r.text, "BLUE-7") {
		t.Fatalf("reply after the dropped call = %+v", r)
	}
}

// TestRealCLIProgressKeepsCallAlive holds a call for six minutes with the
// pool's progress notifications, and the same process gets the result.
func TestRealCLIProgressKeepsCallAlive(t *testing.T) {
	skipUnlessLong(t)
	tp := newRealPool(t, 10*time.Minute)
	r, req := realHeldCall(t, tp, "real-alive")

	time.Sleep(6 * time.Minute)
	st := tp.Status()
	if len(st.Processes) != 1 || st.Processes[0].State != "waiting_tool" {
		t.Fatalf("held call was not kept alive: %+v", st.Processes)
	}
	pid := st.Processes[0].PID

	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "PURPLE-42"},
		user("Then, on a new line, also write BLUE-7."))
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text, "PURPLE-42") || !strings.Contains(r.text, "BLUE-7") {
		t.Fatalf("reply after the held call = %+v", r)
	}
	if st := tp.Status(); len(st.Processes) != 1 || st.Processes[0].PID != pid {
		t.Fatalf("the process changed: %+v", st.Processes)
	}
}

func skipUnlessLong(t *testing.T) {
	t.Helper()
	if os.Getenv("CLAUDEPOOL_REAL_CLI_LONG") != "1" {
		t.Skip("set CLAUDEPOOL_REAL_CLI_LONG=1 (with CLAUDEPOOL_REAL_CLI=1) to run tests that hold a call for minutes")
	}
}

// realHeldCall starts a conversation whose first reply is one tool call,
// which the test then holds.
func realHeldCall(t *testing.T, tp *testPool, owner string) (reply, Request) {
	t.Helper()
	req := Request{
		Owner: owner, Model: "haiku", System: "You are a terse test assistant.", Tools: []Tool{lookupTool},
		// Not "only the result text": the follow-up asks for more.
		Messages: []Message{user(`Call the lookup tool with q set to "x". After you get its result, reply with the result text.`)},
	}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 {
		t.Fatalf("first reply = %+v", r)
	}
	return r, req
}

// newRealPool starts a pool on the installed, signed-in claude binary, or
// skips the test unless CLAUDEPOOL_REAL_CLI=1.
func newRealPool(t *testing.T, toolTimeout time.Duration) *testPool {
	t.Helper()
	return newRealPoolWith(t, toolTimeout, nil)
}

func newRealPoolWith(t *testing.T, toolTimeout time.Duration, adjust func(*Config)) *testPool {
	t.Helper()
	if os.Getenv("CLAUDEPOOL_REAL_CLI") != "1" {
		t.Skip("set CLAUDEPOOL_REAL_CLI=1 to run against the installed claude binary")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	var pool *Pool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { pool.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := Config{
		CLI:         CLI{Binary: bin},
		WorkDir:     dir + "/work",
		StateDir:    dir + "/state",
		MCPURL:      srv.URL,
		ToolTimeout: toolTimeout,
		Logf:        t.Logf,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	pool, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = os.RemoveAll(pool.projectDir()) // this test's own session folder
	})
	return &testPool{Pool: pool}
}

// TestRealCLILoginTakesTheCode starts the installed claude binary's sign-in
// with a throwaway home and config dir and no way to open a browser, then
// passes a made-up code. It checks that Claude Code prints Anthropic's link,
// reads the code from the runner and rejects it. Nothing is signed in.
//
//	CLAUDEPOOL_REAL_CLI=1 go test -run TestRealCLILoginTakesTheCode -v ./claudepool
func TestRealCLILoginTakesTheCode(t *testing.T) {
	if os.Getenv("CLAUDEPOOL_REAL_CLI") != "1" {
		t.Skip("set CLAUDEPOOL_REAL_CLI=1 to run against the installed claude binary")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "no-browser"))
	t.Setenv("BROWSER", "/usr/bin/false")
	var l LoginRunner
	done := make(chan struct{})
	if err := l.Start(CLI{Binary: bin, ConfigDir: filepath.Join(home, "claude")}, time.Minute, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	state := waitLogin(t, &l, "the link and prompt", func(s LoginState) bool { return s.URL != "" && s.AwaitingCode })
	t.Logf("link host: %s", strings.SplitN(state.URL, "?", 2)[0])
	if err := l.SubmitCode("not-a-real-code#not-a-real-state"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		l.Cancel()
		t.Fatal("claude did not finish after the code")
	}
	if state = l.State(); state.Running || state.LastError == "" {
		t.Fatalf("a made-up code must fail: %+v", state)
	}
	t.Logf("claude said: %s", state.LastError)
}
