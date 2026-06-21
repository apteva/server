package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnrichAgentStatusBodyAddsSleepCountdown(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 6, 20, 10, 2, 0, 0, time.UTC)
	ev := makeTelemetryEvent("llm.done", "main", map[string]any{
		"iteration": 7,
		"rate":      "5.0m",
	})
	ev.Time = now.Add(-2 * time.Minute)
	if err := s.store.InsertTelemetry([]TelemetryEvent{ev}); err != nil {
		t.Fatalf("insert telemetry: %v", err)
	}

	body := []byte(`{"iteration":7,"rate":"5.0m","paused":false}`)
	out, ok := s.enrichAgentStatusBody(1, body, now)
	if !ok {
		t.Fatal("expected body to be enriched")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["sleep_state"] != "sleeping" {
		t.Fatalf("sleep_state=%v, want sleeping", got["sleep_state"])
	}
	if got["sleep_thread_id"] != "main" {
		t.Fatalf("sleep_thread_id=%v, want main", got["sleep_thread_id"])
	}
	if got["sleep_total_ms"] != float64((5 * time.Minute).Milliseconds()) {
		t.Fatalf("sleep_total_ms=%v", got["sleep_total_ms"])
	}
	if got["sleep_remaining_ms"] != float64((3 * time.Minute).Milliseconds()) {
		t.Fatalf("sleep_remaining_ms=%v", got["sleep_remaining_ms"])
	}
	if got["next_wake_at"] == "" {
		t.Fatal("next_wake_at missing")
	}
}

func TestEnrichAgentStatusBodyMarksOverdue(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 6, 20, 10, 10, 0, 0, time.UTC)
	ev := makeTelemetryEvent("llm.done", "main", map[string]any{
		"iteration": 3,
		"rate":      "30.0s",
	})
	ev.Time = now.Add(-time.Minute)
	if err := s.store.InsertTelemetry([]TelemetryEvent{ev}); err != nil {
		t.Fatalf("insert telemetry: %v", err)
	}

	out, ok := s.enrichAgentStatusBody(1, []byte(`{"iteration":3,"rate":"30.0s","paused":false}`), now)
	if !ok {
		t.Fatal("expected body to be enriched")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["sleep_state"] != "overdue" {
		t.Fatalf("sleep_state=%v, want overdue", got["sleep_state"])
	}
	if got["sleep_remaining_ms"] != float64(0) {
		t.Fatalf("sleep_remaining_ms=%v, want 0", got["sleep_remaining_ms"])
	}
}

func TestEnrichAgentThreadsBodyAddsPerThreadSleep(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 6, 20, 10, 2, 0, 0, time.UTC)
	mainDone := makeTelemetryEvent("llm.done", "main", map[string]any{
		"iteration": 5,
		"rate":      "2.0m",
	})
	mainDone.Time = now.Add(-30 * time.Second)
	workerDone := makeTelemetryEvent("llm.done", "worker", map[string]any{
		"iteration": 2,
		"rate":      "1.0h",
	})
	workerDone.Time = now.Add(-10 * time.Minute)
	if err := s.store.InsertTelemetry([]TelemetryEvent{mainDone, workerDone}); err != nil {
		t.Fatalf("insert telemetry: %v", err)
	}

	body := []byte(`[
		{"id":"main","iteration":5,"rate":"sleep"},
		{"id":"worker","iteration":2,"rate":"sleep"}
	]`)
	out, ok := s.enrichAgentThreadsBody(1, body, now)
	if !ok {
		t.Fatal("expected body to be enriched")
	}
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["sleep_state"] != "sleeping" || got[0]["sleep_remaining_ms"] != float64((90*time.Second).Milliseconds()) {
		t.Fatalf("main sleep fields wrong: %+v", got[0])
	}
	if got[1]["sleep_state"] != "sleeping" || got[1]["sleep_remaining_ms"] != float64((50*time.Minute).Milliseconds()) {
		t.Fatalf("worker sleep fields wrong: %+v", got[1])
	}
}
