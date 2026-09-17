package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExecutionTelemetryAdditiveCompatibility(t *testing.T) {
	s := newTestServer(t)
	legacy := []TelemetryEvent{
		makeTelemetryEvent("llm.done", "main", map[string]any{"model": "test-model", "tokens_in": 100, "tokens_out": 20, "duration_ms": 50}),
		makeTelemetryEvent("tool.call", "main", map[string]any{"id": "call_0", "name": "probe"}),
		makeTelemetryEvent("thread.spawn", "worker", map[string]any{}),
		makeTelemetryEvent("thread.done", "worker", map[string]any{}),
	}
	if err := s.store.InsertTelemetry(legacy); err != nil {
		t.Fatal(err)
	}
	before, err := s.store.TelemetryStats(1, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var extra []TelemetryEvent
	for _, kind := range []string{"llm.request.queued", "llm.request.started", "llm.request.first_output", "llm.request.finished", "llm.http.started", "llm.http.headers", "llm.http.finished", "llm.retry.scheduled", "llm.retry.finished", "tool.execution.queued", "tool.execution.started", "tool.execution.finished", "worker.created", "worker.ready", "worker.first_inference", "worker.first_action", "worker.finished"} {
		extra = append(extra, makeTelemetryEvent(kind, "worker", map[string]any{"request_id": "req-1", "worker_run_id": "run-1", "execution_ids": []string{"exe-1"}, "tokens_in": 100, "tokens_out": 20, "duration_ms": 500, "outcome": "failed"}))
	}
	body, _ := json.Marshal(extra)
	req := httptest.NewRequest(http.MethodPost, "/telemetry", bytes.NewReader(body))
	if s.instanceSecret != "" {
		req.Header.Set("X-Agent-Secret", s.instanceSecret)
	}
	w := httptest.NewRecorder()
	s.handleIngestTelemetry(w, req)
	if w.Code != 200 {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
	after, err := s.store.TelemetryStats(1, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if before.LLMCalls != after.LLMCalls || before.TotalTokensIn != after.TotalTokensIn || before.TotalTokensOut != after.TotalTokensOut || before.TotalCost != after.TotalCost || before.AvgDurationMs != after.AvgDurationMs || before.ToolCalls != after.ToolCalls || before.ThreadsSpawned != after.ThreadsSpawned || before.ThreadsDone != after.ThreadsDone || before.Errors != after.Errors {
		t.Fatalf("legacy aggregates changed: before=%+v after=%+v", before, after)
	}
	stored, err := s.store.QueryTelemetry(1, "llm.request.finished", time.Time{}, 100)
	if err != nil || len(stored) != 1 {
		t.Fatalf("query: %v %+v", err, stored)
	}
	var data map[string]any
	if err := json.Unmarshal(stored[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["request_id"] != "req-1" || data["worker_run_id"] != "run-1" {
		t.Fatalf("IDs lost: %+v", data)
	}
	if _, ok := data["cost_usd"]; ok {
		t.Fatal("detailed event incorrectly priced")
	}
}
