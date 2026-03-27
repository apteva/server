package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func makeTelemetryEvent(eventType, threadID string, data map[string]any) TelemetryEvent {
	d, _ := json.Marshal(data)
	return TelemetryEvent{
		ID:         generateID(),
		InstanceID: 1,
		ThreadID:   threadID,
		Type:       eventType,
		Time:       time.Now(),
		Data:       json.RawMessage(d),
	}
}

func TestTelemetryInsertAndQuery(t *testing.T) {
	s := newTestServer(t)

	events := []TelemetryEvent{
		makeTelemetryEvent("llm.done", "main", map[string]any{
			"tokens_in": 100, "tokens_out": 50, "cost_usd": 0.001, "duration_ms": 1500,
		}),
		makeTelemetryEvent("thread.spawn", "worker-1", map[string]any{
			"parent_id": "main", "directive": "research",
		}),
		makeTelemetryEvent("tool.call", "main", map[string]any{
			"name": "web",
		}),
	}

	err := s.store.InsertTelemetry(events)
	if err != nil {
		t.Fatalf("InsertTelemetry: %v", err)
	}

	// Query all for instance 1
	results, err := s.store.QueryTelemetry(1, "", time.Time{}, 100)
	if err != nil {
		t.Fatalf("QueryTelemetry: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3, got %d", len(results))
	}

	// Query by type
	llmResults, _ := s.store.QueryTelemetry(1, "llm.done", time.Time{}, 100)
	if len(llmResults) != 1 {
		t.Fatalf("expected 1 llm.done, got %d", len(llmResults))
	}

	// Query by prefix
	threadResults, _ := s.store.QueryTelemetry(1, "thread", time.Time{}, 100)
	if len(threadResults) != 1 {
		t.Fatalf("expected 1 thread.*, got %d", len(threadResults))
	}
}

func TestTelemetryStats(t *testing.T) {
	s := newTestServer(t)

	events := []TelemetryEvent{
		makeTelemetryEvent("llm.done", "main", map[string]any{
			"tokens_in": 100, "tokens_out": 50, "cost_usd": 0.001, "duration_ms": 1500,
		}),
		makeTelemetryEvent("llm.done", "main", map[string]any{
			"tokens_in": 200, "tokens_out": 80, "cost_usd": 0.002, "duration_ms": 2000,
		}),
		makeTelemetryEvent("thread.spawn", "worker-1", map[string]any{}),
		makeTelemetryEvent("thread.done", "worker-1", map[string]any{}),
		makeTelemetryEvent("tool.call", "main", map[string]any{"name": "web"}),
		makeTelemetryEvent("tool.call", "main", map[string]any{"name": "web"}),
		makeTelemetryEvent("llm.error", "main", map[string]any{"error": "timeout"}),
	}

	s.store.InsertTelemetry(events)

	stats, err := s.store.TelemetryStats(1, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("TelemetryStats: %v", err)
	}

	if stats.TotalEvents != 7 {
		t.Errorf("expected 7 events, got %d", stats.TotalEvents)
	}
	if stats.LLMCalls != 2 {
		t.Errorf("expected 2 llm calls, got %d", stats.LLMCalls)
	}
	if stats.TotalTokensIn != 300 {
		t.Errorf("expected 300 tokens in, got %d", stats.TotalTokensIn)
	}
	if stats.TotalTokensOut != 130 {
		t.Errorf("expected 130 tokens out, got %d", stats.TotalTokensOut)
	}
	if stats.TotalCost < 0.002 || stats.TotalCost > 0.004 {
		t.Errorf("expected ~0.003 cost, got %f", stats.TotalCost)
	}
	if stats.AvgDurationMs < 1700 || stats.AvgDurationMs > 1800 {
		t.Errorf("expected ~1750 avg duration, got %f", stats.AvgDurationMs)
	}
	if stats.ThreadsSpawned != 1 {
		t.Errorf("expected 1 thread spawned, got %d", stats.ThreadsSpawned)
	}
	if stats.ThreadsDone != 1 {
		t.Errorf("expected 1 thread done, got %d", stats.ThreadsDone)
	}
	if stats.ToolCalls != 2 {
		t.Errorf("expected 2 tool calls, got %d", stats.ToolCalls)
	}
	if stats.Errors != 1 {
		t.Errorf("expected 1 error, got %d", stats.Errors)
	}
}

func TestTelemetryCleanup(t *testing.T) {
	s := newTestServer(t)

	// Insert old event
	old := makeTelemetryEvent("llm.done", "main", map[string]any{})
	old.Time = time.Now().Add(-48 * time.Hour)
	s.store.InsertTelemetry([]TelemetryEvent{old})

	// Insert recent event
	recent := makeTelemetryEvent("llm.done", "main", map[string]any{})
	s.store.InsertTelemetry([]TelemetryEvent{recent})

	// Clean events older than 24h
	deleted, err := s.store.CleanOldTelemetry(24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanOldTelemetry: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	remaining, _ := s.store.QueryTelemetry(1, "", time.Time{}, 100)
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(remaining))
	}
}

func TestTelemetryQuerySince(t *testing.T) {
	s := newTestServer(t)

	ev1 := makeTelemetryEvent("llm.done", "main", map[string]any{})
	ev1.Time = time.Now().Add(-2 * time.Hour)
	ev2 := makeTelemetryEvent("llm.done", "main", map[string]any{})

	s.store.InsertTelemetry([]TelemetryEvent{ev1, ev2})

	// Query since 1 hour ago — should only get ev2
	results, _ := s.store.QueryTelemetry(1, "", time.Now().Add(-1*time.Hour), 100)
	if len(results) != 1 {
		t.Errorf("expected 1 recent event, got %d", len(results))
	}
}

func TestTelemetryHTTPIngestAndQuery(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	// Register + login to get a cookie
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)
	if cookie == "" {
		t.Fatal("no session cookie")
	}

	// Ingest a batch of events (POST /telemetry — unauthenticated)
	events := []TelemetryEvent{
		makeTelemetryEvent("llm.done", "main", map[string]any{
			"tokens_in": 500, "tokens_out": 100, "cost_usd": 0.005, "duration_ms": 3000,
			"iteration": 1, "model": "test-model",
		}),
		makeTelemetryEvent("thread.spawn", "worker-1", map[string]any{
			"parent_id": "main", "directive": "research topic",
		}),
		makeTelemetryEvent("thread.message", "worker-1", map[string]any{
			"from": "worker-1", "to": "main", "message": "found results",
		}),
		makeTelemetryEvent("tool.call", "main", map[string]any{
			"name": "web", "args": "url=https://example.com",
		}),
		makeTelemetryEvent("tool.result", "main", map[string]any{
			"name": "web", "duration_ms": 200, "success": true,
		}),
		makeTelemetryEvent("thread.done", "worker-1", map[string]any{
			"parent_id": "main", "result": "task complete",
		}),
	}

	ingestBody, _ := json.Marshal(events)
	w := httptest.NewRequest("POST", "/telemetry", bytes.NewReader(ingestBody))
	w.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleIngestTelemetry(rec, w)

	if rec.Code != 200 {
		t.Fatalf("ingest: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ingestResult map[string]int
	json.Unmarshal(rec.Body.Bytes(), &ingestResult)
	if ingestResult["inserted"] != 6 {
		t.Errorf("expected 6 inserted, got %d", ingestResult["inserted"])
	}

	// Query via GET /telemetry (authenticated)
	req := httptest.NewRequest("GET", "/telemetry?instance_id=1&limit=100", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(s.handleQueryTelemetry)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("query: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var queryResults []TelemetryEvent
	json.Unmarshal(rec.Body.Bytes(), &queryResults)
	if len(queryResults) != 6 {
		t.Errorf("expected 6, got %d", len(queryResults))
	}

	// Query stats via GET /telemetry/stats (authenticated)
	req = httptest.NewRequest("GET", "/telemetry/stats?instance_id=1&period=1h", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(s.handleTelemetryStats)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("stats: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var stats TelemetryStats
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if stats.LLMCalls != 1 {
		t.Errorf("stats: expected 1 llm call, got %d", stats.LLMCalls)
	}
	if stats.ThreadsSpawned != 1 {
		t.Errorf("stats: expected 1 thread spawned, got %d", stats.ThreadsSpawned)
	}
	if stats.ThreadsDone != 1 {
		t.Errorf("stats: expected 1 thread done, got %d", stats.ThreadsDone)
	}
	if stats.ToolCalls != 1 {
		t.Errorf("stats: expected 1 tool call, got %d", stats.ToolCalls)
	}
	if stats.TotalTokensIn != 500 {
		t.Errorf("stats: expected 500 tokens in, got %d", stats.TotalTokensIn)
	}
}

func TestTelemetryChunksNotStored(t *testing.T) {
	// llm.chunk events are live-only in core — they should NOT be sent to server.
	// This test verifies that if they somehow arrive, they're treated like any event,
	// but the real filtering happens in core (EmitLive vs Emit).
	// Here we verify the DB doesn't blow up with high-frequency inserts.
	s := newTestServer(t)

	var chunks []TelemetryEvent
	for i := 0; i < 100; i++ {
		chunks = append(chunks, makeTelemetryEvent("llm.chunk", "main", map[string]any{
			"text": "token", "iteration": 1,
		}))
	}
	chunks = append(chunks, makeTelemetryEvent("llm.done", "main", map[string]any{
		"tokens_in": 100, "tokens_out": 50,
	}))

	err := s.store.InsertTelemetry(chunks)
	if err != nil {
		t.Fatalf("InsertTelemetry: %v", err)
	}

	// All 101 should be stored (server doesn't filter — core does)
	results, _ := s.store.QueryTelemetry(1, "", time.Time{}, 200)
	if len(results) != 101 {
		t.Errorf("expected 101, got %d", len(results))
	}

	// But querying just llm.done should return 1
	doneResults, _ := s.store.QueryTelemetry(1, "llm.done", time.Time{}, 100)
	if len(doneResults) != 1 {
		t.Errorf("expected 1 llm.done, got %d", len(doneResults))
	}
}

func TestTelemetryDuplicateIgnored(t *testing.T) {
	s := newTestServer(t)

	ev := makeTelemetryEvent("llm.done", "main", map[string]any{})

	// Insert same event twice
	s.store.InsertTelemetry([]TelemetryEvent{ev})
	s.store.InsertTelemetry([]TelemetryEvent{ev})

	results, _ := s.store.QueryTelemetry(1, "", time.Time{}, 100)
	if len(results) != 1 {
		t.Errorf("expected 1 (duplicate ignored), got %d", len(results))
	}
}
