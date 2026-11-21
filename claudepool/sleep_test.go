package claudepool

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

var paceTool = Tool{Name: "pace", Description: "Sleep until the next event", InputSchema: json.RawMessage(`{"type":"object","properties":{"sleep":{"type":"string"}}}`)}

// newSleepPool returns a pool that parks pace calls, with a fast reaper,
// and counts the parks.
func newSleepPool(t *testing.T, limits Limits) (*testPool, func() int) {
	t.Helper()
	oldReap, oldObserver, oldPause := reapInterval, parkObserver, parkPause
	t.Cleanup(func() { reapInterval, parkObserver, parkPause = oldReap, oldObserver, oldPause })
	reapInterval = 20 * time.Millisecond
	var mu sync.Mutex
	parks := 0
	parkObserver = func(string, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		parks++
	}
	tp := newTestPoolWith(t, limits, func(cfg *Config) {
		cfg.ToolTimeout = time.Minute
		cfg.ParkTools = []string{"pace"}
		cfg.TerminalTools = []string{"done"}
	})
	return tp, func() int {
		mu.Lock()
		defer mu.Unlock()
		return parks
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sleepRequest starts a conversation whose reply sleeps with pace.
func sleepRequest(t *testing.T, tp *testPool, owner string) (Request, reply) {
	t.Helper()
	req := Request{Owner: owner, Tools: []Tool{paceTool, lookupTool}, Messages: []Message{user(`CALL pace {"sleep":"10m"}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "pace" {
		t.Fatalf("sleep reply = %+v", r)
	}
	return req, r
}

// wake adds the pace result and a new event, as core sends them when the
// agent wakes.
func wake(req Request, r reply) Request {
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "set sleep=10m"},
		user("event: new lead"))
	return req
}

func wakeEcho(r reply) string {
	return "echo: [tool result for call " + r.calls[0].ID + "]\nset sleep=10m\n\n<new_messages>\nevent: new lead\n</new_messages>"
}

func processState(tp *testPool, owner string) string {
	for _, proc := range tp.Status().Processes {
		if proc.Owner == owner {
			return proc.State
		}
	}
	return ""
}

func TestSleepingAgentIsParkedAndResumed(t *testing.T) {
	tp, parks := newSleepPool(t, Limits{IdleTimeout: 200 * time.Millisecond})
	req, r := sleepRequest(t, tp, "a")
	if state := processState(tp, "a"); state != "waiting_tool" {
		t.Fatalf("state before the idle timeout = %q, want the call held", state)
	}
	waitFor(t, "the sleeping process to close", func() bool { return tp.Status().Running == 0 })
	if st := tp.Status(); st.Sessions != 1 || parks() != 1 {
		t.Fatalf("status = %+v, parks = %d, want the session kept", st, parks())
	}

	req = wake(req, r)
	woke, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if woke.text != wakeEcho(r) {
		t.Fatalf("wake reply = %q\nwant %q", woke.text, wakeEcho(r))
	}
	spawns := tp.spawns(t)
	if len(spawns) != 2 || spawns[1].Mode != "resume" || spawns[1].Session != spawns[0].Session {
		t.Fatalf("spawns = %+v", spawns)
	}

	// The resumed process carries on as usual.
	req.Messages = append(req.Messages, assistant(woke.text), user("more"))
	if r, err := tp.run(t, req); err != nil || r.text != "echo: more" {
		t.Fatalf("next turn = %+v, %v", r, err)
	}
	if n := len(tp.spawns(t)); n != 2 {
		t.Fatalf("spawned %d processes, want 2", n)
	}
}

// A wake before the idle timeout reaches the held call, as before.
func TestShortSleepIsHeld(t *testing.T) {
	tp, parks := newSleepPool(t, Limits{IdleTimeout: time.Minute})
	req, r := sleepRequest(t, tp, "a")
	time.Sleep(200 * time.Millisecond)
	woke, err := tp.run(t, wake(req, r))
	if err != nil {
		t.Fatal(err)
	}
	if want := "got: set sleep=10m\n\n<new_messages>\nevent: new lead\n</new_messages>"; woke.text != want {
		t.Fatalf("reply = %q, want %q", woke.text, want)
	}
	if n := len(tp.spawns(t)); n != 1 || parks() != 0 {
		t.Fatalf("spawns = %d, parks = %d", n, parks())
	}
}

// Another conversation takes a sleeping agent's slot at once, without the
// grace period a waiting tool call gets.
func TestSleepingAgentGivesUpSlotAtOnce(t *testing.T) {
	defer func(d time.Duration) { toolGrace = d }(toolGrace)
	toolGrace = time.Minute
	tp, parks := newSleepPool(t, Limits{MaxProcs: 1, IdleTimeout: time.Minute, QueueTimeout: 5 * time.Second})
	reqA, ra := sleepRequest(t, tp, "a")

	start := time.Now()
	rb, err := tp.run(t, Request{Owner: "b", Messages: []Message{user("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if rb.text != "echo: hi" || time.Since(start) > 3*time.Second {
		t.Fatalf("b = %+v after %s", rb, time.Since(start))
	}
	if st := tp.Status(); st.Sessions != 2 || parks() != 1 {
		t.Fatalf("status = %+v, parks = %d", st, parks())
	}

	woke, err := tp.run(t, wake(reqA, ra))
	if err != nil {
		t.Fatal(err)
	}
	if woke.text != wakeEcho(ra) {
		t.Fatalf("a's wake reply = %q", woke.text)
	}
	spawns := tp.spawns(t)
	if len(spawns) != 3 || spawns[2].Mode != "resume" || spawns[2].Session != spawns[0].Session {
		t.Fatalf("spawns = %+v", spawns)
	}
}

// Results that arrive while the process is being closed wait for it, then
// resume the session.
func TestWakeWhileParking(t *testing.T) {
	tp, _ := newSleepPool(t, Limits{IdleTimeout: 100 * time.Millisecond})
	entered, release := make(chan struct{}), make(chan struct{})
	parkPause = func() {
		close(entered)
		<-release
	}
	req, r := sleepRequest(t, tp, "a")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sleeping session was not parked")
	}
	type result struct {
		r   reply
		err error
	}
	done := make(chan result, 1)
	go func() {
		woke, err := tp.run(t, wake(req, r))
		done <- result{woke, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the wake did not wait for the park: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.r.text != wakeEcho(r) {
		t.Fatalf("wake reply = %q", got.r.text)
	}
	if spawns := tp.spawns(t); len(spawns) != 2 || spawns[1].Mode != "resume" {
		t.Fatalf("spawns = %+v", spawns)
	}
}

// Only sessions waiting on park tools alone are sleeping.
func TestPaceWithAnotherToolIsHeld(t *testing.T) {
	tp, parks := newSleepPool(t, Limits{IdleTimeout: 100 * time.Millisecond})
	req := Request{Owner: "a", Tools: []Tool{paceTool, lookupTool}, Messages: []Message{user(`CALL pace {} | lookup {}`)}}
	if _, err := tp.run(t, req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if state := processState(tp, "a"); state != "waiting_tool" || parks() != 0 {
		t.Fatalf("state = %q, parks = %d", state, parks())
	}
}

func TestHoldSleepingKeepsProcess(t *testing.T) {
	tp, parks := newSleepPool(t, Limits{IdleTimeout: 100 * time.Millisecond, HoldSleeping: true})
	sleepRequest(t, tp, "a")
	time.Sleep(500 * time.Millisecond)
	if state := processState(tp, "a"); state != "waiting_tool" || parks() != 0 {
		t.Fatalf("state = %q, parks = %d", state, parks())
	}
	// The setting applies at once.
	tp.SetLimits(Limits{IdleTimeout: 100 * time.Millisecond})
	waitFor(t, "the sleeping process to close", func() bool { return tp.Status().Running == 0 })
}

// If the session cannot be resumed, the request starts over from the
// caller's history, as it would have without parking.
func TestFailedResumeStartsOver(t *testing.T) {
	tp, _ := newSleepPool(t, Limits{IdleTimeout: 100 * time.Millisecond})
	req, r := sleepRequest(t, tp, "a")
	waitFor(t, "the sleeping process to close", func() bool { return tp.Status().Running == 0 })
	t.Setenv("CLAUDEPOOL_FAKE_RESUME_FAIL", "1")
	woke, err := tp.run(t, wake(req, r))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<conversation>", "[tool result for call " + r.calls[0].ID + "]\nset sleep=10m", "</conversation>\n\nevent: new lead"} {
		if !strings.Contains(woke.text, want) {
			t.Fatalf("reply misses %q:\n%s", want, woke.text)
		}
	}
	spawns := tp.spawns(t)
	if len(spawns) != 3 || spawns[1].Mode != "resume" || spawns[2].Mode != "new" || spawns[2].Session == spawns[0].Session {
		t.Fatalf("spawns = %+v", spawns)
	}
	if st := tp.Status(); st.Sessions != 1 {
		t.Fatalf("sessions = %d, want only the new one", st.Sessions)
	}
}
