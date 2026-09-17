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

	body := []byte(`{"iteration":7,"rate":"5.0m","paused":false,"llm_active":false,"next_wake_at":"2026-06-20T10:05:00Z"}`)
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

	out, ok := s.enrichAgentStatusBody(1, []byte(`{"iteration":3,"rate":"30.0s","paused":false,"llm_active":false,"next_wake_at":"2026-06-20T10:09:30Z"}`), now)
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
		{"id":"main","iteration":5,"rate":"sleep","next_wake_at":"2026-06-20T10:03:30Z"},
		{"id":"worker","iteration":2,"rate":"sleep","next_wake_at":"2026-06-20T10:52:00Z"}
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

// Agent 1103 cleared its wake. A retained 30s rate and completed thought
// previously invented a countdown, then an "overdue"/"active" state forever.
func TestClearedWakeNeverCreatesCountdown(t *testing.T) {
	s := newTestServer(t)
	doneAt := time.Date(2026, 9, 10, 7, 20, 55, 0, time.UTC)
	ev := makeTelemetryEvent("llm.done", "main", map[string]any{"iteration": 1, "rate": "30.0s"})
	ev.Time = doneAt
	s.latestLLMDone.remember([]TelemetryEvent{ev})
	for _, elapsed := range []time.Duration{4 * time.Second, 30 * time.Second, 90 * time.Second, 24 * time.Hour} {
		for _, body := range []string{
			`{"iteration":1,"rate":"30.0s","llm_active":false,"next_wake_at":""}`,
			`{"id":"main","iteration":1,"rate":"slow"}`,
			`{"id":"main","iteration":1,"rate":"slow","next_wake_at":"0001-01-01T00:00:00Z"}`,
		} {
			var input map[string]any
			if err := json.Unmarshal([]byte(body), &input); err != nil {
				t.Fatal(err)
			}
			originalWake, hadWake := input["next_wake_at"]
			// Even existing derived decoration must be removed, not retained.
			input["sleep_total_ms"] = 30000
			input["sleep_remaining_ms"] = 26000
			state := s.computeAgentSleepStatus(1, input, doneAt.Add(elapsed))
			applySleepStatus(input, state)
			if input["sleep_state"] != "waiting" || input["sleep_remaining_ms"] != int64(0) {
				t.Fatalf("elapsed=%s input=%v", elapsed, input)
			}
			if _, ok := input["sleep_total_ms"]; ok {
				t.Fatal("stale countdown duration retained")
			}
			wake, hasWake := input["next_wake_at"]
			if wake != originalWake || hasWake != hadWake {
				t.Fatal("core deadline changed")
			}
		}
	}
}

func TestSleepStatusUsesLiveDeadlineAndActivity(t *testing.T) {
	now := time.Date(2026, 9, 10, 7, 0, 0, 0, time.UTC)
	ev := makeTelemetryEvent("llm.done", "main", map[string]any{"iteration": 1, "rate": "30.0s"})
	ev.Time = now.Add(-time.Hour)
	cases := []struct {
		name, body, state string
		remaining         time.Duration
	}{
		{"replaced timer", `{"iteration":2,"rate":"5m","llm_active":false,"next_wake_at":"2026-09-10T07:04:00Z"}`, "sleeping", 4 * time.Minute},
		{"event interrupts sleep", `{"llm_active":true,"next_wake_at":"2026-09-10T07:04:00Z"}`, "active", 0},
		{"working without timer", `{"llm_active":true,"next_wake_at":""}`, "active", 0},
		{"completed iteration is not activity", `{"iteration":100,"llm_active":false,"next_wake_at":""}`, "waiting", 0},
		{"real timer due", `{"llm_active":false,"next_wake_at":"2026-09-10T06:59:00Z"}`, "overdue", 0},
		{"paused timer", `{"paused":true,"llm_active":true,"next_wake_at":"2026-09-10T07:04:00Z"}`, "paused", 0},
		{"stopped", `{"rate":"stopped","next_wake_at":"2026-09-10T07:04:00Z"}`, "stopped", 0},
		{"invalid deadline", `{"next_wake_at":"invalid","rate":"30s"}`, "unknown", 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var input map[string]any
			json.Unmarshal([]byte(tt.body), &input)
			wake := input["next_wake_at"]
			got := computeThreadSleepStatus(input, ev, now)
			if got.State != tt.state || got.Remaining != tt.remaining {
				t.Fatalf("got=%+v", got)
			}
			applySleepStatus(input, got)
			if input["next_wake_at"] != wake {
				t.Fatal("changed authoritative deadline")
			}
		})
	}
	// Scheduling works without telemetry, including after a server restart.
	input := map[string]any{"next_wake_at": "2026-09-10T07:04:00Z"}
	if got := computeThreadSleepStatus(input, TelemetryEvent{}, now); got.State != "sleeping" || got.Remaining != 4*time.Minute {
		t.Fatal(got)
	}
}

func TestMainSleepStatusDoesNotUseWorkerTelemetry(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	main := makeTelemetryEvent("llm.done", "main", map[string]any{"iteration": 1, "rate": "30s"})
	main.Time = now.Add(-time.Minute)
	worker := makeTelemetryEvent("llm.done", "worker", map[string]any{"iteration": 42, "rate": "1h"})
	worker.Time = now
	s.latestLLMDone.remember([]TelemetryEvent{main, worker})
	state := s.computeAgentSleepStatus(1, map[string]any{"iteration": 1, "llm_active": false, "next_wake_at": ""}, now)
	if state.State != "waiting" || state.ThreadID != "main" || !state.StartedAt.Equal(main.Time) {
		t.Fatalf("worker contaminated main: %+v", state)
	}
}
