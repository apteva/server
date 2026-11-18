package claudepool

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRealCLI runs a short conversation through the installed, signed-in
// claude binary. It uses the subscription, so it only runs when asked:
//
//	CLAUDEPOOL_REAL_CLI=1 go test -run TestRealCLI -v ./claudepool
func TestRealCLI(t *testing.T) {
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
	pool, err = New(Config{
		CLI:         CLI{Binary: bin},
		WorkDir:     dir + "/work",
		StateDir:    dir + "/state",
		MCPURL:      srv.URL,
		ToolTimeout: 2 * time.Minute,
		Logf:        t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = os.RemoveAll(pool.projectDir()) // this test's own session folder
	})
	tp := &testPool{Pool: pool}

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
