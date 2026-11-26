package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// fakeThreadCore serves GET /threads and /status for agent 41.
func fakeThreadCore(t *testing.T, s *Server, paused bool, rows ...map[string]any) {
	t.Helper()
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer core-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/threads":
			json.NewEncoder(w).Encode(rows)
		case "/status":
			json.NewEncoder(w).Encode(map[string]any{"paused": paused})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(core.Close)
	parsed, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)
	if s.agents == nil {
		s.agents = NewAgentManager(t.TempDir(), "unused-core")
	}
	s.agents.processes[41] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
}

func threadTopology(t *testing.T, s *Server, install int64, query string) sdk.AgentTopology {
	t.Helper()
	response := telemetryReadRequest(s, install, "/agents/41/topology"+query)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var topology sdk.AgentTopology
	if err := json.Unmarshal(response.Body.Bytes(), &topology); err != nil {
		t.Fatal(err)
	}
	return topology
}

func observedThread(topology sdk.AgentTopology, id string) *sdk.ObservedThread {
	for i := range topology.Threads {
		if topology.Threads[i].ID == id {
			return &topology.Threads[i]
		}
	}
	return nil
}

func seedThreadTelemetry(t *testing.T, s *Server, events ...TelemetryEvent) {
	t.Helper()
	for i := range events {
		if events[i].AgentID == 0 {
			events[i].AgentID = 41
		}
	}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
}

func threadEvent(id, thread, kind string, at time.Time, data string) TelemetryEvent {
	return TelemetryEvent{ID: id, ThreadID: thread, Type: kind, Time: at, Data: json.RawMessage(data)}
}

// Core reports rates (reactive/fast/normal/slow/sleep); status used to be
// filled only for words Core never sends, so it was always empty.
func TestThreadTopologyFillsStatusFromCoreRates(t *testing.T) {
	s, install := telemetryReadFixture(t)
	fakeThreadCore(t, s, false,
		map[string]any{"id": "main", "rate": "normal", "depth": 0},
		map[string]any{"id": "napper", "parent_id": "main", "rate": "sleep", "depth": 1, "next_wake_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	topology := threadTopology(t, s, install, "")
	if topology.ThreadSchema != 1 || !topology.ThreadsAvailable {
		t.Fatalf("topology: %+v", topology)
	}
	if main := observedThread(topology, "main"); main.Status != "waiting" || main.State != sdk.ThreadStateWaiting {
		t.Fatalf("main: %+v", main)
	}
	if napper := observedThread(topology, "napper"); napper.Status != "sleeping" || napper.State != sdk.ThreadStateSleeping || napper.NextWakeAt.IsZero() {
		t.Fatalf("napper: %+v", napper)
	}
}

func TestThreadTopologyStatesAndPermissionTiers(t *testing.T) {
	now := time.Now().UTC()
	rows := []map[string]any{
		{"id": "main", "rate": "normal", "directive": "Run the shop.\nSecond line", "iteration": 9, "model": "large", "provider": "anthropic", "started_at": now.Add(-time.Hour).Format(time.RFC3339Nano)},
		{"id": "worker", "parent_id": "main", "name": "Inbox", "depth": 1, "rate": "fast", "directive": "Close [[private]] refunds", "iteration": 2, "started_at": now.Add(-10 * time.Minute).Format(time.RFC3339Nano)},
		{"id": "stuck", "parent_id": "main", "depth": 1, "rate": "normal", "inference": map[string]any{"blocked_reason": "provider rejected the key"}},
		{"id": "idle", "parent_id": "main", "depth": 1, "rate": "reactive", "realtime": true, "ephemeral": true},
	}
	seed := func(s *Server) {
		seedThreadTelemetry(t, s,
			threadEvent("w1", "worker", "worker.created", now.Add(-10*time.Minute), `{"parent_thread_id":"main","parent_tool_span_id":"span-spawn"}`),
			threadEvent("w2", "worker", "thread.spawn", now.Add(-10*time.Minute), `{"parent_id":"main","directive":"Close refunds","parent_execution_id":"exe-1"}`),
			threadEvent("w3", "worker", "tool.call", now.Add(-time.Minute), `{"id":"call-1","name":"crm_list","tool_span_id":"span-1","reason":"Reading the inbox"}`),
			threadEvent("m1", "main", "llm.start", now.Add(-2*time.Minute), `{"model":"m"}`),
			threadEvent("m2", "main", "llm.done", now.Add(-time.Minute), `{"model":"m","tokens_in":10}`),
			threadEvent("i1", "idle", "tool.call", now.Add(-3*time.Hour), `{"id":"lost","name":"crm_list","tool_span_id":"span-lost"}`),
			threadEvent("f1", "finished", "worker.created", now.Add(-5*time.Minute), `{"parent_thread_id":"worker","parent_tool_span_id":"span-child"}`),
			threadEvent("f2", "finished", "thread.spawn", now.Add(-5*time.Minute), `{"parent_id":"worker"}`),
			threadEvent("f3", "finished", "thread.done", now.Add(-2*time.Minute), `{"parent_id":"worker","outcome":"completed","result":"Closed 3 refunds"}`),
			threadEvent("o1", "old", "thread.done", now.Add(-3*time.Hour), `{"parent_id":"main","outcome":"failed"}`),
		)
	}

	t.Run("observe tier", func(t *testing.T) {
		s, install := telemetryReadFixture(t)
		fakeThreadCore(t, s, false, rows...)
		seed(s)
		topology := threadTopology(t, s, install, "?include_ended=3600")
		main := observedThread(topology, "main")
		if main.State != sdk.ThreadStateWaiting || main.Turns != 9 || main.Model != "large" || main.Provider != "anthropic" || main.StartedAt.IsZero() || main.LastActivityAt.IsZero() {
			t.Fatalf("main: %+v", main)
		}
		worker := observedThread(topology, "worker")
		if worker.State != sdk.ThreadStateWorking || worker.Status != "running" || worker.Current == nil || worker.Current.Kind != "tool" || worker.Current.Tool != "crm_list" || worker.Name != "Inbox" || worker.Depth != 1 {
			t.Fatalf("worker: %+v %+v", worker, worker.Current)
		}
		if worker.SpawnedBy == nil || worker.SpawnedBy.ThreadID != "main" || worker.SpawnedBy.ToolSpanID != "span-spawn" || worker.SpawnedBy.ExecutionID != "exe-1" {
			t.Fatalf("worker spawn link: %+v", worker.SpawnedBy)
		}
		if stuck := observedThread(topology, "stuck"); stuck.State != sdk.ThreadStateBlocked || stuck.BlockedReason != "" {
			t.Fatalf("stuck: %+v", stuck)
		}
		// A start three hours old without an end is a lost call, not work.
		if idle := observedThread(topology, "idle"); idle.State != sdk.ThreadStateWaiting || idle.Current != nil || !idle.Realtime || !idle.Ephemeral {
			t.Fatalf("idle: %+v", idle)
		}
		finished := observedThread(topology, "finished")
		if finished == nil || finished.State != sdk.ThreadStateDone || finished.Ended == nil || finished.Ended.Outcome != "completed" || finished.Ended.Result != "" || finished.ParentID != "worker" || finished.Depth != 2 {
			t.Fatalf("finished: %+v %+v", finished, finished.Ended)
		}
		if finished.SpawnedBy == nil || finished.SpawnedBy.ToolSpanID != "span-child" || finished.StartedAt.IsZero() {
			t.Fatalf("finished spawn link: %+v", finished.SpawnedBy)
		}
		if observedThread(topology, "old") != nil {
			t.Fatal("thread outside the window listed")
		}
		for _, secret := range []string{"Run the shop", "private", "provider rejected", "Closed 3 refunds", "Reading the inbox"} {
			raw, _ := json.Marshal(topology)
			if strings.Contains(string(raw), secret) {
				t.Fatalf("observe tier leaked %q: %s", secret, raw)
			}
		}
	})

	t.Run("content tier", func(t *testing.T) {
		s, install := telemetryTestServer(t, "proj-1", "platform.telemetry.read", "platform.instances.read")
		fakeThreadCore(t, s, false, rows...)
		seed(s)
		topology := threadTopology(t, s, install, "?include_ended=3600")
		if main := observedThread(topology, "main"); main.Purpose != "Run the shop." {
			t.Fatalf("main purpose: %q", main.Purpose)
		}
		if stuck := observedThread(topology, "stuck"); stuck.BlockedReason != "provider rejected the key" {
			t.Fatalf("blocked reason: %+v", stuck)
		}
		if finished := observedThread(topology, "finished"); finished == nil || finished.Ended.Result != "Closed 3 refunds" {
			t.Fatalf("ended result: %+v", finished)
		}
	})

	t.Run("topology tier only", func(t *testing.T) {
		s, install := telemetryTestServer(t, "proj-1", "platform.instances.read")
		fakeThreadCore(t, s, false, rows...)
		seed(s)
		topology := threadTopology(t, s, install, "?include_ended=3600")
		worker := observedThread(topology, "worker")
		if worker.State != sdk.ThreadStateWorking || worker.Current != nil || !worker.LastActivityAt.IsZero() || worker.SpawnedBy != nil {
			t.Fatalf("topology tier got telemetry fields: %+v", worker)
		}
		if observedThread(topology, "finished") != nil {
			t.Fatal("topology tier got ended threads")
		}
	})

	t.Run("paused agent", func(t *testing.T) {
		s, install := telemetryReadFixture(t)
		fakeThreadCore(t, s, true, rows...)
		seed(s)
		for _, thread := range threadTopology(t, s, install, "").Threads {
			if thread.State != sdk.ThreadStatePaused || thread.Status != "paused" {
				t.Fatalf("paused agent thread: %+v", thread)
			}
		}
	})
}

func TestThreadTopologyStoppedAgentUsesSavedConfig(t *testing.T) {
	s, install := telemetryTestServer(t, "proj-1", "platform.telemetry.read", "platform.instances.read")
	s.store.db.Exec(`UPDATE agents SET config=? WHERE id=41`, `{"directive":"Main job","threads":[{"id":"worker","parent_id":"main","name":"Inbox","depth":1,"directive":"Close refunds","realtime":true,"pace":{"rate":"slow"}}]}`)
	s.agents = NewAgentManager(t.TempDir(), "unused-core")
	topology := threadTopology(t, s, install, "")
	worker := observedThread(topology, "worker")
	if len(topology.Threads) != 2 || worker == nil || worker.State != sdk.ThreadStateStopped || worker.Status != "stopped" || worker.Name != "Inbox" || worker.Purpose != "Close refunds" {
		t.Fatalf("stopped topology: %+v", topology.Threads)
	}
	if main := observedThread(topology, "main"); main.Purpose != "Main job" || main.State != sdk.ThreadStateStopped {
		t.Fatalf("stopped main: %+v", main)
	}
}

func TestThreadTopologyRejectsBadWindow(t *testing.T) {
	s, install := telemetryReadFixture(t)
	for _, window := range []string{"-1", "abc", fmt.Sprint(25 * 3600)} {
		if response := telemetryReadRequest(s, install, "/agents/41/topology?include_ended="+window); response.Code != 400 {
			t.Fatalf("include_ended=%s accepted: %d", window, response.Code)
		}
	}
}

func threadActivity(t *testing.T, s *Server, install int64, query string) (sdk.ThreadActivityPage, *httptest.ResponseRecorder) {
	t.Helper()
	response := telemetryReadRequest(s, install, "/agents/41/activity?"+query)
	var page sdk.ThreadActivityPage
	if response.Code == 200 {
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	return page, response
}

func seedWorkerActivity(t *testing.T, s *Server, now time.Time) {
	at := func(seconds int) time.Time { return now.Add(time.Duration(seconds) * time.Second) }
	seedThreadTelemetry(t, s,
		threadEvent("a01", "worker", "llm.start", at(0), `{"model":"claude-x","provider":"anthropic"}`),
		threadEvent("a02", "worker", "llm.done", at(2), `{"model":"claude-x","provider":"anthropic","tokens_in":120,"tokens_out":30,"message":"private model text"}`),
		threadEvent("a03", "worker", "tool.arguments", at(2), `{"id":"call-1","name":"crm_list","stage":"core_parsed","json":"{\"status\":\"open\"}","truncated":false}`),
		threadEvent("a04", "worker", "tool.call", at(2), `{"id":"call-1","name":"crm_list","tool_span_id":"span-1","reason":"Reading the CRM inbox"}`),
		threadEvent("a05", "worker", "tool.result", at(3), `{"id":"call-1","name":"crm_list","tool_span_id":"span-1","success":true,"duration_ms":1200,"result":"14 items","result_original_bytes":900}`),
		threadEvent("a06", "worker", "tool.call", at(4), `{"id":"call-2","name":"spawn","tool_span_id":"span-2","reason":"Delegate"}`),
		threadEvent("a07", "helper", "worker.created", at(4), `{"parent_thread_id":"worker","parent_tool_span_id":"span-2"}`),
		threadEvent("a08", "worker", "tool.result", at(5), `{"id":"call-2","name":"spawn","tool_span_id":"span-2","success":true,"duration_ms":400}`),
		threadEvent("a09", "worker", "tool.call", at(5), `{"id":"call-3","name":"crm_close","tool_span_id":"span-3"}`),
		threadEvent("a10", "worker", "tool.result", at(6), `{"id":"call-3","name":"crm_close","tool_span_id":"span-3","success":false,"duration_ms":400,"result":"already closed"}`),
		threadEvent("a11", "worker", "thread.message", at(7), `{"from":"worker","to":"main","message":"Closed 3 refunds for Jane"}`),
		threadEvent("a12", "worker", "llm.start", at(8), `{"model":"claude-x"}`),
		threadEvent("a13", "worker", "llm.start", at(9), `{"model":"claude-y"}`),
		threadEvent("a14", "worker", "llm.error", at(10), `{"model":"claude-y","error":"overloaded"}`),
		threadEvent("a15", "worker", "worker.finished", at(11), `{"outcome":"completed"}`),
		threadEvent("a16", "worker", "thread.done", at(11), `{"parent_id":"main","outcome":"completed","result":"All refunds closed"}`),
		threadEvent("x01", "other", "tool.call", at(5), `{"id":"call-x","name":"elsewhere","tool_span_id":"span-x"}`),
	)
}

func TestThreadActivityBuildsStepsAtTwoLevels(t *testing.T) {
	s, install := telemetryReadFixture(t)
	now := time.Now().UTC().Add(-time.Minute)
	seedWorkerActivity(t, s, now)

	page, response := threadActivity(t, s, install, "thread_id=worker")
	if response.Code != 200 || response.Header().Get("X-Apteva-Thread-Activity") != "1" {
		t.Fatal(response.Code, response.Body.String())
	}
	kinds := []string{}
	for _, step := range page.Steps {
		kinds = append(kinds, step.Kind+":"+step.Status)
	}
	// Newest first; the two-start retry is one model turn.
	if fmt.Sprint(kinds) != "[end:done model:failed message:done tool:failed tool:done tool:done model:done]" {
		t.Fatalf("steps: %v", kinds)
	}
	model, lister, spawner, closer, message := page.Steps[6], page.Steps[5], page.Steps[4], page.Steps[3], page.Steps[2]
	if model.Model != "claude-x" || model.Provider != "anthropic" || model.TokensIn != 120 || model.TokensOut != 30 || model.DurationMS != 2000 {
		t.Fatalf("model step: %+v", model)
	}
	if lister.Tool != "crm_list" || lister.CallID != "call-1" || lister.ToolSpanID != "span-1" || lister.DurationMS != 1200 {
		t.Fatalf("tool step: %+v", lister)
	}
	if spawner.Tool != "spawn" || spawner.ChildThreadID != "helper" {
		t.Fatalf("spawn step: %+v", spawner)
	}
	if retry := page.Steps[1]; !retry.StartedAt.Equal(now.Add(8*time.Second)) || retry.Model != "claude-y" {
		t.Fatalf("retried turn: %+v", retry)
	}
	if message.To != "main" || page.Steps[0].Outcome != "completed" || closer.Tool != "crm_close" {
		t.Fatalf("message/end: %+v %+v", message, page.Steps[0])
	}
	body := response.Body.String()
	for _, secret := range []string{"Reading the CRM inbox", `"arguments"`, "14 items", "already closed", "Jane", "All refunds closed", "overloaded", "private model text", "elsewhere"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metadata activity leaked %q: %s", secret, body)
		}
	}

	// Content needs telemetry.read.
	if _, response = threadActivity(t, s, install, "thread_id=worker&projection=content"); response.Code != 403 {
		t.Fatal("content without telemetry.read", response.Code)
	}
	s2, reader := telemetryTestServer(t, "proj-1", "platform.telemetry.read")
	seedWorkerActivity(t, s2, now)
	page, response = threadActivity(t, s2, reader, "thread_id=worker&projection=content")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	lister, closer, message = page.Steps[5], page.Steps[3], page.Steps[2]
	if lister.Reason != "Reading the CRM inbox" || lister.Arguments != `{"status":"open"}` || lister.Result != "14 items" || lister.ResultBytes != 900 {
		t.Fatalf("content tool step: %+v", lister)
	}
	if closer.Error != "already closed" || message.Text != "Closed 3 refunds for Jane" || page.Steps[0].Text != "All refunds closed" || page.Steps[1].Error != "overloaded" {
		t.Fatalf("content steps: %+v %+v %+v %+v", closer, message, page.Steps[0], page.Steps[1])
	}
}

func TestThreadActivityPagesWithoutDuplicates(t *testing.T) {
	s, install := telemetryReadFixture(t)
	seedWorkerActivity(t, s, time.Now().UTC().Add(-time.Minute))
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		query := "thread_id=worker&limit=2"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		page, response := threadActivity(t, s, install, query)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		pages++
		for _, step := range page.Steps {
			key := step.Kind + step.StartedAt.String()
			if seen[key] {
				t.Fatalf("duplicate step across pages: %+v", step)
			}
			seen[key] = true
			if step.Kind == "model" && step.Status == "running" {
				t.Fatalf("a page boundary left a finished turn open: %+v", step)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("paging did not end")
		}
	}
	if len(seen) != 7 {
		t.Fatalf("steps across pages: %d", len(seen))
	}
	// A cursor is bound to its filters.
	first, _ := threadActivity(t, s, install, "thread_id=worker&limit=2")
	if _, response := threadActivity(t, s, install, "thread_id=other&limit=2&cursor="+url.QueryEscape(first.NextCursor)); response.Code != 400 {
		t.Fatal("cursor reused across threads", response.Code)
	}
}

func TestThreadActivityScopeAndOldServerSignal(t *testing.T) {
	s, install := telemetryReadFixture(t)
	if _, response := threadActivity(t, s, install, ""); response.Code != 400 {
		t.Fatal("missing thread id", response.Code)
	}
	// Agent 42 is in another project; 99 belongs to another user.
	for _, agent := range []string{"42", "99"} {
		response := telemetryReadRequest(s, install, "/agents/"+agent+"/activity?thread_id=main&projection=metadata")
		if agent == "99" && response.Code == 200 {
			continue // observe may read same-project agents' metadata
		}
		if response.Code != 404 || response.Header().Get("X-Apteva-Thread-Activity") != "1" {
			t.Fatalf("agent %s: %d %q", agent, response.Code, response.Header().Get("X-Apteva-Thread-Activity"))
		}
	}
	reader, readerInstall := telemetryTestServer(t, "proj-1", "platform.telemetry.read")
	if response := telemetryReadRequest(reader, readerInstall, "/agents/99/activity?thread_id=main&projection=content"); response.Code != 404 {
		t.Fatal("content of another user's agent", response.Code)
	}
	none, noneInstall := telemetryTestServer(t, "proj-1", "platform.instances.read")
	if response := telemetryReadRequest(none, noneInstall, "/agents/41/activity?thread_id=main"); response.Code != 403 {
		t.Fatal("activity without telemetry permission", response.Code)
	}
}

func TestThreadActivityIsReadOnlyKeyReadable(t *testing.T) {
	for path, want := range map[string]bool{
		"/telemetry/agents/41/activity?thread_id=main": true,
		"/telemetry/agents/41/topology":                true,
		"/telemetry/agents/x/activity":                 false,
		"/telemetry/agents/0/activity":                 false,
	} {
		if got := readOnlyAPIRequestAllowed(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("%s allowed=%v, want %v", path, got, want)
		}
	}
}

func TestThreadObserverSDKAgainstServer(t *testing.T) {
	s, _ := telemetryReadFixture(t)
	fakeThreadCore(t, s, false, map[string]any{"id": "main", "rate": "normal"}, map[string]any{"id": "worker", "parent_id": "main", "depth": 1, "rate": "fast"})
	now := time.Now().UTC().Add(-time.Minute)
	seedWorkerActivity(t, s, now)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-User-ID", "1")
		s.handleTelemetryRead(w, r, 0, strings.TrimPrefix(r.URL.Path, "/api/telemetry"))
	}))
	defer server.Close()
	client, err := sdk.NewTelemetryClient(server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	observer, ok := client.(sdk.ThreadObserver)
	if !ok {
		t.Fatal("telemetry client is not a thread observer")
	}
	topology, err := observer.GetAgentThreads(t.Context(), sdk.AgentThreadsQuery{ProjectID: "proj-1", AgentID: 41, IncludeEnded: time.Hour})
	if err != nil || len(topology.Threads) != 2 || topology.Threads[1].Depth != 1 {
		t.Fatalf("topology via SDK: %+v %v", topology, err)
	}
	page, err := observer.GetThreadActivity(t.Context(), sdk.ThreadActivityQuery{ProjectID: "proj-1", AgentID: 41, ThreadID: "worker", Projection: "content", Limit: 3})
	// Three starts, two of them one retried model turn: two steps.
	if err != nil || len(page.Steps) != 2 || page.NextCursor == "" || page.Steps[0].Text != "All refunds closed" || page.Steps[1].Error != "overloaded" {
		t.Fatalf("activity via SDK: %+v %v", page, err)
	}
	if _, err = observer.GetThreadActivity(t.Context(), sdk.ThreadActivityQuery{ProjectID: "proj-2", AgentID: 41, ThreadID: "worker"}); err == nil || errors.Is(err, sdk.ErrThreadObservationUnsupported) {
		t.Fatalf("out-of-scope agent: %v", err)
	}
}

func TestThreadActivityFollowsRenames(t *testing.T) {
	s, install := telemetryReadFixture(t)
	now := time.Now().UTC().Add(-time.Minute)
	seedThreadTelemetry(t, s,
		threadEvent("r1", "draft", "tool.call", now, `{"id":"c1","name":"crm_list","tool_span_id":"s1"}`),
		threadEvent("r2", "draft", "tool.result", now.Add(time.Second), `{"id":"c1","name":"crm_list","tool_span_id":"s1","success":true}`),
		threadEvent("r3", "final", "thread.renamed", now.Add(2*time.Second), `{"old_id":"draft","new_id":"final","name":"Final"}`),
		threadEvent("r4", "final", "tool.call", now.Add(3*time.Second), `{"id":"c2","name":"crm_close","tool_span_id":"s2"}`),
	)
	page, response := threadActivity(t, s, install, "thread_id=final")
	if response.Code != 200 || fmt.Sprint(page.PreviousIDs) != "[draft]" || len(page.Steps) != 2 || page.Steps[1].ThreadID != "draft" || page.Steps[0].Status != "running" {
		t.Fatalf("renamed activity: %d %+v", response.Code, page)
	}
}

func TestThreadMetadataKeepsLifecycleIdentifiers(t *testing.T) {
	s, _ := telemetryReadFixture(t)
	project := func(kind, data string) map[string]any {
		event := s.projectTelemetryEvent(TelemetryEvent{ID: "e", AgentID: 41, ThreadID: "w", Type: kind, Data: json.RawMessage(data)}, telemetryAgent{project: "proj-1"}, "metadata")
		out := map[string]any{}
		json.Unmarshal(event.Data, &out)
		return out
	}
	if got := project("thread.spawn", `{"parent_id":"main","directive":"secret plan"}`); got["parent_id"] != "main" || got["directive"] != nil {
		t.Fatalf("spawn: %v", got)
	}
	if got := project("thread.renamed", `{"old_id":"a","new_id":"b","name":"Free text name"}`); got["old_id"] != "a" || got["new_id"] != "b" || got["name"] != nil {
		t.Fatalf("rename: %v", got)
	}
	if got := project("worker.created", `{"parent_thread_id":"main","parent_tool_span_id":"span-1"}`); got["parent_tool_span_id"] != "span-1" {
		t.Fatalf("worker.created: %v", got)
	}
	if got := project("thread.done", `{"outcome":"failed","result":"customer data"}`); got["outcome"] != "failed" || got["result"] != nil {
		t.Fatalf("done: %v", got)
	}
	if got := project("worker.finished", `{"outcome":"drop table"}`); got["outcome"] != nil {
		t.Fatalf("unknown outcome kept: %v", got)
	}
	if got := project("llm.done", `{"model":"claude-x","provider":"anthropic","message":"text"}`); got["model"] != "claude-x" || got["provider"] != "anthropic" || got["message"] != nil {
		t.Fatalf("llm.done: %v", got)
	}
	if got := project("tool.call", `{"model":"claude-x"}`); got["model"] != nil {
		t.Fatalf("model kept outside llm events: %v", got)
	}
}

func TestThreadReadsUseThePerThreadIndex(t *testing.T) {
	s, _ := telemetryReadFixture(t)
	rows, err := s.store.db.Query(`EXPLAIN QUERY PLAN SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND thread_id=? AND +type IN (`+placeholders(len(openStepTypes))+`) ORDER BY rowid DESC LIMIT 64`,
		append([]any{41, "main"}, func() []any {
			out := []any{}
			for _, kind := range openStepTypes {
				out = append(out, kind)
			}
			return out
		}()...)...)
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
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "idx_telem_agent_thread") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("per-thread read plan: %s", joined)
	}
}
