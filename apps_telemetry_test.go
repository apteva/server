package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// telemetryTestServer: registered user 1 owning agents 41 (proj-1) and
// 42 (proj-2), a stranger's agent 99, and an install with the telemetry
// permission.
func telemetryTestServer(t *testing.T, installProject string, permissions ...string) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	s.secret = testSecret()
	postJSON(t, s.handleRegister, map[string]string{
		"email": "telemetry@test.com", "password": "password123",
	})
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO agents (id, user_id, name, directive, mode, config, project_id) VALUES
		(41, 1, 'A', '', 'learn', '{}', 'proj-1'),
		(42, 1, 'B', '', 'learn', '{}', 'proj-2')`)
	mustExec(`INSERT INTO users (id, email, password_hash) VALUES (2, 'other@test.com', 'x')`)
	mustExec(`INSERT INTO agents (id, user_id, name, directive, mode, config, project_id) VALUES
		(99, 2, 'Foreign', '', 'learn', '{}', 'proj-1')`)

	permsJSON, _ := json.Marshal(permissions)
	mustExec(`INSERT INTO apps (id, name, source, repo, ref, manifest_json) VALUES (700, 'streamer', 'registry', '', '', '{}')`)
	mustExec(`INSERT INTO app_installs (id, app_id, project_id, status, permissions_json, installed_by)
		VALUES (7001, 700, ?, 'running', ?, 1)`, installProject, string(permsJSON))
	return s, 7001
}

func telemetryRequest(ctx context.Context, query string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/apps/callback/telemetry"+query, nil).WithContext(ctx)
	req.Header.Set("X-User-ID", "1")
	return req
}

// sseRecorder implements Flusher and lets the handler stream while the
// test reads what has been written so far.
type sseRecorder struct {
	*httptest.ResponseRecorder
}

func (r *sseRecorder) Flush() {}

func collectSSEEvents(t *testing.T, body string) []TelemetryEvent {
	t.Helper()
	var events []TelemetryEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev TelemetryEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad SSE payload %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func TestCallbackTelemetryRequiresPermission(t *testing.T) {
	s, installID := telemetryTestServer(t, "", "db.write.app") // no telemetry perm
	rec := httptest.NewRecorder()
	s.handleCallbackTelemetry(rec, telemetryRequest(context.Background(), "?events=llm.tool_chunk"), installID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCallbackTelemetryRequiresEventFilter(t *testing.T) {
	s, installID := telemetryTestServer(t, "", "platform.telemetry.read")
	rec := httptest.NewRecorder()
	// The firehose is dominated by token deltas — "everything" must be
	// an explicit list, never an omission.
	s.handleCallbackTelemetry(rec, telemetryRequest(context.Background(), ""), installID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestCallbackTelemetryFiltersStream is the core contract: event-type,
// ownership, and thread-prefix filters all enforced server-side.
func TestCallbackTelemetryFiltersStream(t *testing.T) {
	s, installID := telemetryTestServer(t, "", "platform.telemetry.read")

	ctx, cancel := context.WithCancel(context.Background())
	rec := &sseRecorder{httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleCallbackTelemetry(rec, telemetryRequest(ctx, "?events=llm.tool_chunk&thread_prefix=chat-"), installID)
	}()
	time.Sleep(50 * time.Millisecond) // let SubscribeAll attach

	publish := func(agentID int64, threadID, eventType string) {
		s.broadcaster.Broadcast([]TelemetryEvent{{
			ID: fmt.Sprintf("ev-%d-%s", agentID, eventType), AgentID: agentID,
			ThreadID: threadID, Type: eventType, Time: time.Now(),
			Data: json.RawMessage(`{"text":"tok"}`),
		}})
	}
	publish(41, "chat-conv-1", "llm.tool_chunk") // ✓ everything matches
	publish(41, "worker-7", "llm.tool_chunk")    // ✗ thread prefix
	publish(41, "chat-conv-1", "thought")        // ✗ event type
	publish(99, "chat-conv-9", "llm.tool_chunk") // ✗ foreign agent
	publish(42, "chat-conv-2", "llm.tool_chunk") // ✓ second owned agent

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	events := collectSSEEvents(t, rec.Body.String())
	if len(events) != 2 {
		t.Fatalf("events = %d (%+v), want exactly the 2 allowed", len(events), events)
	}
	if events[0].AgentID != 41 || events[1].AgentID != 42 {
		t.Fatalf("wrong agents passed the filter: %+v", events)
	}
	for _, ev := range events {
		if !strings.HasPrefix(ev.ThreadID, "chat-") || ev.Type != "llm.tool_chunk" {
			t.Fatalf("filter leak: %+v", ev)
		}
	}
}

// A project-scoped install must not see the user's agents from other
// projects — same boundary callbackAgentForInstall enforces on spawns.
func TestCallbackTelemetryScopesToInstallProject(t *testing.T) {
	s, installID := telemetryTestServer(t, "proj-1", "platform.telemetry.read")

	owned, err := s.ownedAgentIDsForInstall(1, installID)
	if err != nil {
		t.Fatal(err)
	}
	if !owned[41] || owned[42] || owned[99] {
		t.Fatalf("owned = %v, want only agent 41 (proj-1, user 1)", owned)
	}
}

func TestTelemetryFilterUnitCases(t *testing.T) {
	filter := &telemetryFilter{
		events:       map[string]bool{"llm.tool_chunk": true},
		threadPrefix: "chat-",
		ownedAgents:  map[int64]bool{41: true},
	}
	cases := []struct {
		name string
		ev   TelemetryEvent
		want bool
	}{
		{"allowed", TelemetryEvent{Type: "llm.tool_chunk", AgentID: 41, ThreadID: "chat-x"}, true},
		{"wrong type", TelemetryEvent{Type: "thought", AgentID: 41, ThreadID: "chat-x"}, false},
		{"unowned agent", TelemetryEvent{Type: "llm.tool_chunk", AgentID: 99, ThreadID: "chat-x"}, false},
		{"wrong thread", TelemetryEvent{Type: "llm.tool_chunk", AgentID: 41, ThreadID: "main"}, false},
	}
	for _, tc := range cases {
		if got := filter.allows(tc.ev); got != tc.want {
			t.Errorf("%s: allows = %v, want %v", tc.name, got, tc.want)
		}
	}
	// agent_id narrows further even among owned agents.
	filter.agentID = 41
	filter.ownedAgents[42] = true
	if filter.allows(TelemetryEvent{Type: "llm.tool_chunk", AgentID: 42, ThreadID: "chat-x"}) {
		t.Error("agent_id filter must exclude other owned agents")
	}
}
