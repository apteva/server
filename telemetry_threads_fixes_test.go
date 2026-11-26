package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Review and live check 2026-11-25. A worker that calls done mid-turn never
// logs that turn's end: its last model step stayed "running" forever.
func TestThreadActivityClosesStepsWhenTheThreadEnds(t *testing.T) {
	s, install := telemetryReadFixture(t)
	now := time.Now().UTC().Add(-time.Minute)
	at := func(ms int) time.Time { return now.Add(time.Duration(ms) * time.Millisecond) }
	seedThreadTelemetry(t, s,
		threadEvent("d1", "probe", "thread.spawn", at(0), `{"parent_id":"main"}`),
		threadEvent("d2", "probe", "llm.start", at(100), `{"model":"m","iteration":1}`),
		threadEvent("d3", "probe", "tool.call", at(2000), `{"id":"c1","name":"done","tool_span_id":"s1"}`),
		threadEvent("d4", "probe", "thread.done", at(2500), `{"parent_id":"main","outcome":"completed"}`),
	)
	page, response := threadActivity(t, s, install, "thread_id=probe")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	got := []string{}
	for _, step := range page.Steps {
		got = append(got, step.Kind+":"+step.Status)
		if step.Kind != "end" && (step.EndedAt.IsZero() || !step.EndedAt.Equal(at(2500))) {
			t.Errorf("%s step not closed at the end: %+v", step.Kind, step)
		}
	}
	if fmt.Sprint(got) != "[end:done tool:ended model:ended]" {
		t.Fatalf("steps %v", got)
	}
	if model := page.Steps[2]; model.DurationMS != 2400 {
		t.Fatalf("model step duration %d, want 2400", model.DurationMS)
	}
}

// A turn cut off by a Core restart, or by the thread being started again under
// the same id, must not be merged into the next turn.
func TestThreadActivityKeepsLostTurnsApart(t *testing.T) {
	s, install := telemetryReadFixture(t)
	now := time.Now().UTC().Add(-time.Hour / 2)
	at := func(seconds int) time.Time { return now.Add(time.Duration(seconds) * time.Second) }
	seedThreadTelemetry(t, s,
		// main: turn 7 never ends (Core stopped); after restart turn 1 runs.
		threadEvent("r1", "main", "llm.start", at(0), `{"model":"m","iteration":7}`),
		threadEvent("r2", "main", "llm.start", at(60), `{"model":"m","iteration":1}`),
		threadEvent("r3", "main", "llm.done", at(62), `{"model":"m"}`),
		// a real retry of one turn: same iteration, seconds apart.
		threadEvent("r4", "main", "llm.start", at(100), `{"model":"m","iteration":2}`),
		threadEvent("r5", "main", "llm.start", at(103), `{"model":"m2","iteration":2}`),
		threadEvent("r6", "main", "llm.done", at(105), `{"model":"m2"}`),
		// worker: killed mid-turn without any end, then spawned again.
		threadEvent("k1", "w", "thread.spawn", at(0), `{"parent_id":"main"}`),
		threadEvent("k2", "w", "llm.start", at(1), `{"model":"m","iteration":1}`),
		threadEvent("k3", "w", "tool.call", at(2), `{"id":"c1","name":"crm_list","tool_span_id":"s1"}`),
		threadEvent("k4", "w", "thread.spawn", at(30), `{"parent_id":"main"}`),
		threadEvent("k5", "w", "llm.start", at(31), `{"model":"m","iteration":1}`),
		threadEvent("k6", "w", "llm.done", at(33), `{"model":"m"}`),
	)
	page, _ := threadActivity(t, s, install, "thread_id=main")
	got := []string{}
	for _, step := range page.Steps {
		got = append(got, fmt.Sprintf("%s:%s@%d", step.Kind, step.Status, int(step.StartedAt.Sub(now).Seconds())))
	}
	if fmt.Sprint(got) != "[model:done@100 model:done@60 model:unknown@0]" {
		t.Fatalf("main steps %v", got)
	}
	page, _ = threadActivity(t, s, install, "thread_id=w")
	got = got[:0]
	for _, step := range page.Steps {
		got = append(got, fmt.Sprintf("%s:%s@%d", step.Kind, step.Status, int(step.StartedAt.Sub(now).Seconds())))
	}
	if fmt.Sprint(got) != "[model:done@31 tool:unknown@2 model:unknown@1]" {
		t.Fatalf("worker steps %v", got)
	}
}

// Topology: an older turn that never ended is not the current step.
func TestOpenThreadStepIgnoresAnOlderUnendedTurn(t *testing.T) {
	now := time.Now().UTC()
	row := func(n int64, kind string, at time.Time, iteration int) threadEventRow {
		return threadEventRow{row: n, thread: "main", kind: kind, at: at, data: map[string]any{"iteration": float64(iteration)}}
	}
	// Newest first: a current turn (iteration 1) after a lost one (iteration 7).
	events := []threadEventRow{row(2, "llm.start", now.Add(-time.Second), 1), row(1, "llm.start", now.Add(-5*time.Minute), 7)}
	step := openThreadStep(events, time.Time{}, now)
	if step == nil || !step.Since.Equal(now.Add(-time.Second)) {
		t.Fatalf("current step %+v, want the newest turn", step)
	}
	// Two attempts of one turn still count from the first.
	events = []threadEventRow{row(2, "llm.start", now.Add(-time.Second), 3), row(1, "llm.start", now.Add(-4*time.Second), 3)}
	if step = openThreadStep(events, time.Time{}, now); step == nil || !step.Since.Equal(now.Add(-4*time.Second)) {
		t.Fatalf("retried step %+v, want the first attempt", step)
	}
}

// Content arguments must not carry platform-internal keys, and results are
// capped by the server whatever Core sends.
func TestThreadActivityCleansArgumentsAndCapsResults(t *testing.T) {
	s, reader := telemetryTestServer(t, "proj-1", "platform.telemetry.read")
	now := time.Now().UTC().Add(-time.Minute)
	big := strings.Repeat("x", 10<<10)
	seedThreadTelemetry(t, s,
		threadEvent("a1", "w", "tool.arguments", now, `{"id":"c1","json":"{\"_apteva_caller_thread\":\"main\",\"_apteva_tool_call_id\":\"c1\",\"_reason\":\"why\",\"status\":\"open\"}"}`),
		threadEvent("a2", "w", "tool.call", now, `{"id":"c1","name":"crm_list","tool_span_id":"s1"}`),
		threadEvent("a3", "w", "tool.result", now.Add(time.Second), `{"id":"c1","name":"crm_list","tool_span_id":"s1","success":true,"result":"`+big+`"}`),
		threadEvent("b1", "w", "tool.arguments", now.Add(2*time.Second), `{"id":"c2","json":"{\"_apteva_caller_thread\":\"main\",\"query\":\"Ja","truncated":true}`),
		threadEvent("b2", "w", "tool.call", now.Add(2*time.Second), `{"id":"c2","name":"crm_search","tool_span_id":"s2"}`),
	)
	page, response := threadActivity(t, s, reader, "thread_id=w&projection=content")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "_apteva_") || strings.Contains(response.Body.String(), `\"_reason\"`) {
		t.Fatalf("internal keys leaked: %s", response.Body.String())
	}
	search, list := page.Steps[0], page.Steps[1]
	if list.Arguments != `{"status":"open"}` || list.ArgumentsTruncated {
		t.Fatalf("cleaned arguments %q truncated=%v", list.Arguments, list.ArgumentsTruncated)
	}
	if len(list.Result) > 4<<10+8 || !list.ResultTruncated {
		t.Fatalf("result not capped: %d bytes truncated=%v", len(list.Result), list.ResultTruncated)
	}
	if search.Arguments != "" || !search.ArgumentsTruncated {
		t.Fatalf("cut-off arguments shown: %q truncated=%v", search.Arguments, search.ArgumentsTruncated)
	}
}

func TestThreadPurposeSkipsLayout(t *testing.T) {
	for directive, want := range map[string]string{
		"# Role\n\nYou are the Messaging Agent. Triage the inbox.": "You are the Messaging Agent. Triage the inbox.",
		"**Role**\n- Check the CRM inbox every hour":                "Check the CRM inbox every hour",
		"Role:\n1. Close stale tickets":                             "Close stale tickets",
		"<!-- internal\nnote -->\n---\n> Reply to Jane":              "Reply to Jane",
		"```\ncode\n```":                                            "code",
		"Run the shop.\nSecond line":                                "Run the shop.",
		"## Only headings\n### here":                                "",
	} {
		if got := threadPurpose(directive); got != want {
			t.Errorf("threadPurpose(%q) = %q, want %q", directive, got, want)
		}
	}
}

// Core no longer lists an ended thread; its purpose comes from its spawn.
func TestEndedThreadPurposeComesFromItsSpawn(t *testing.T) {
	now := time.Now().UTC()
	s, install := telemetryTestServer(t, "proj-1", "platform.telemetry.read", "platform.instances.read")
	fakeThreadCore(t, s, false, map[string]any{"id": "main", "rate": "normal"})
	seedThreadTelemetry(t, s,
		threadEvent("e1", "probe", "thread.spawn", now.Add(-time.Minute), `{"parent_id":"main","directive":"# Task\nCheck the inbox and report."}`),
		threadEvent("e2", "probe", "thread.done", now.Add(-30*time.Second), `{"parent_id":"main","outcome":"completed"}`),
	)
	topology := threadTopology(t, s, install, "?include_ended=3600")
	probe := observedThread(topology, "probe")
	if probe == nil || probe.Purpose != "Check the inbox and report." {
		t.Fatalf("ended thread purpose: %+v", probe)
	}
}

// When the telemetry detail runs out of time, the list says so.
func TestThreadTopologyMarksPartialLists(t *testing.T) {
	s, install := telemetryReadFixture(t)
	fakeThreadCore(t, s, false, map[string]any{"id": "main", "rate": "normal"})
	previous := threadTelemetryTimeout
	threadTelemetryTimeout = 0
	t.Cleanup(func() { threadTelemetryTimeout = previous })
	topology := threadTopology(t, s, install, "")
	if !topology.Partial || !topology.ThreadsAvailable || observedThread(topology, "main") == nil {
		t.Fatalf("partial list: %+v", topology)
	}
	threadTelemetryTimeout = previous
	if topology = threadTopology(t, s, install, ""); topology.Partial {
		t.Fatal("full list marked partial")
	}
}

// Every topology answer is marked, so the SDK can tell a missing agent from a
// server without the route.
func TestThreadTopologyMarksMissingAgents(t *testing.T) {
	s, install := telemetryReadFixture(t)
	response := telemetryReadRequest(s, install, "/agents/999999/topology")
	if response.Code != 404 || response.Header().Get("X-Apteva-Thread-Activity") != "1" {
		t.Fatalf("missing agent: %d header=%q", response.Code, response.Header().Get("X-Apteva-Thread-Activity"))
	}
}

func queryPlan(t *testing.T, s *Server, query string, args ...any) string {
	t.Helper()
	rows, err := s.store.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := []string{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

// The spawn-link and ended-thread reads use indexes instead of scanning the
// agent's history.
func TestThreadListReadsUseIndexes(t *testing.T) {
	s, _ := telemetryReadFixture(t)
	spawn := queryPlan(t, s, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND thread_id IN (?,?) AND +type IN (?,?) ORDER BY rowid DESC LIMIT ?`,
		41, "a", "b", "worker.created", "thread.spawn", 16)
	if !strings.Contains(spawn, "idx_telem_agent_thread") {
		t.Fatalf("spawn-link plan: %s", spawn)
	}
	ended := queryPlan(t, s, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type='thread.done' AND time>=? ORDER BY rowid DESC LIMIT 200`,
		41, time.Now().UTC().Format(time.RFC3339))
	if !strings.Contains(ended, "idx_telem_agent_type_time") || strings.Contains(ended, "SCAN telemetry") {
		t.Fatalf("ended-thread plan: %s", ended)
	}
}

