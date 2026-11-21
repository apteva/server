package claudepool

import (
	"testing"
	"time"
)

// Busy also counts processes that only wait for the caller's tools (an
// agent pacing between turns), so Status says which are generating.
func TestStatusCountsGeneratingAndWaitingProcesses(t *testing.T) {
	p := &Pool{sessions: map[string]*session{}}
	add := func(id, state string, busy, held bool) {
		sess := newSession("agent-"+id, "")
		sess.id = id
		sess.busy, sess.turnHeld = busy, held
		sess.setState(state, "")
		sess.proc = &process{pid: len(p.sessions) + 1, startedAt: time.Now()}
		p.sessions[id] = sess
	}
	add("a", "replying", true, true)
	add("b", "finishing", true, true)
	add("c", "starting", false, true)
	add("d", "waiting_tool", true, false)
	add("e", "waiting_tool", true, false)
	add("f", "idle", false, false)
	gone := newSession("agent-g", "")
	gone.setState("idle", "")
	p.sessions["g"] = gone // no process: not counted

	st := p.Status()
	if st.Running != 6 || st.Busy != 5 || st.Generating != 3 || st.WaitingTool != 2 {
		t.Fatalf("running %d busy %d generating %d waiting_tool %d; want 6 5 3 2", st.Running, st.Busy, st.Generating, st.WaitingTool)
	}
}
