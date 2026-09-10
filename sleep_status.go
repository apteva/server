package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

type latestLLMDoneCache struct {
	mu     sync.RWMutex
	events map[int64]map[string]TelemetryEvent
}

func (c *latestLLMDoneCache) remember(events []TelemetryEvent) {
	if len(events) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.events == nil {
		c.events = make(map[int64]map[string]TelemetryEvent)
	}
	for _, ev := range events {
		if ev.Type != "llm.done" || ev.AgentID == 0 {
			continue
		}
		threadID := ev.ThreadID
		if threadID == "" {
			threadID = "main"
		}
		byThread := c.events[ev.AgentID]
		if byThread == nil {
			byThread = make(map[string]TelemetryEvent)
			c.events[ev.AgentID] = byThread
		}
		if prev, ok := byThread[threadID]; !ok || ev.Time.After(prev.Time) {
			ev.ThreadID = threadID
			byThread[threadID] = ev
		}
	}
}

func (c *latestLLMDoneCache) snapshot(instanceID int64) map[string]TelemetryEvent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	src := c.events[instanceID]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]TelemetryEvent, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *Server) enrichAgentStatusBody(instanceID int64, body []byte, now time.Time, agents ...*Agent) ([]byte, bool) {
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		return body, false
	}
	if len(agents) > 0 && agents[0] != nil {
		s.behaviorMetadata(agents[0], status)
	} else if s.store != nil {
		if inst, err := s.store.GetAgentByID(instanceID); err == nil {
			s.behaviorMetadata(inst, status)
		}
	}
	state := s.computeAgentSleepStatus(instanceID, status, now)
	applySleepStatus(status, state)
	out, err := json.Marshal(status)
	if err != nil {
		return body, false
	}
	return out, true
}

func (s *Server) enrichAgentThreadsBody(instanceID int64, body []byte, now time.Time) ([]byte, bool) {
	var threads []map[string]any
	if err := json.Unmarshal(body, &threads); err != nil {
		return body, false
	}
	if len(threads) == 0 {
		return body, false
	}
	latest := s.latestLLMDoneByThread(instanceID, len(threads)*2)
	for _, thread := range threads {
		threadID := sleepStringValue(thread["id"])
		if threadID == "" {
			threadID = "main"
		}
		state := computeThreadSleepStatus(thread, latest[threadID], now)
		applySleepStatus(thread, state)
	}
	out, err := json.Marshal(threads)
	if err != nil {
		return body, false
	}
	return out, true
}

func (s *Server) computeAgentSleepStatus(instanceID int64, status map[string]any, now time.Time) sleepStatus {
	// /status describes main. A worker's later llm.done must not supply its
	// scheduling metadata or make main appear active.
	latest := s.latestLLMDoneByThread(instanceID, 100)
	return computeThreadSleepStatus(status, latest["main"], now)
}

func computeThreadSleepStatus(runtime map[string]any, ev TelemetryEvent, now time.Time) sleepStatus {
	state := sleepStatus{State: "waiting", ThreadID: sleepStringValue(runtime["id"]), Iteration: sleepIntValue(runtime["iteration"])}
	if state.ThreadID == "" {
		state.ThreadID = "main"
	}
	if ev.Type == "llm.done" {
		state.StartedAt = ev.Time
	}

	// The deadline is authoritative. Rate is retained even after clear_wake,
	// and a missing thread deadline is omitted by core's JSON encoder.
	// Never manufacture a deadline from llm.done time + rate.
	rawWake := sleepStringValue(runtime["next_wake_at"])
	if rawWake != "" {
		wake, err := time.Parse(time.RFC3339Nano, rawWake)
		if err != nil {
			state.State = "unknown"
		} else if !wake.IsZero() {
			state.NextWakeAt = wake
		}
	}
	switch {
	case sleepStringValue(runtime["rate"]) == "stopped":
		state.State = "stopped"
	case sleepBoolValue(runtime["paused"]):
		state.State = "paused"
	case sleepBoolValue(runtime["llm_active"]):
		state.State = "active"
	case !state.NextWakeAt.IsZero():
		state.Remaining = state.NextWakeAt.Sub(now)
		if state.Remaining > 0 {
			state.State = "sleeping"
		} else {
			state.State = "overdue"
			state.Remaining = 0
		}
	}
	if state.State == "sleeping" || state.State == "overdue" {
		// Duration only supplies optional progress decoration. Symbolic thread
		// rates (e.g. "slow") are not a reliable duration for a custom pace.
		if total, err := time.ParseDuration(sleepStringValue(runtime["rate"])); err == nil && total > 0 && total >= state.Remaining {
			state.Total = total
		}
	}
	return state
}

func (s *Server) latestLLMDoneByThread(instanceID int64, limit int) map[string]TelemetryEvent {
	if limit < 100 {
		limit = 100
	}
	out := s.latestLLMDone.snapshot(instanceID)
	if out == nil {
		out = make(map[string]TelemetryEvent)
	}
	if s.store != nil {
		events, err := s.store.QueryTelemetry(instanceID, "llm.done", time.Time{}, limit)
		if err == nil {
			for _, ev := range events {
				threadID := ev.ThreadID
				if threadID == "" {
					threadID = "main"
				}
				if prev, ok := out[threadID]; !ok || ev.Time.After(prev.Time) {
					ev.ThreadID = threadID
					out[threadID] = ev
				}
			}
		}
	}
	return out
}

type sleepStatus struct {
	State      string
	ThreadID   string
	StartedAt  time.Time
	NextWakeAt time.Time
	Total      time.Duration
	Remaining  time.Duration
	Iteration  int
}

func applySleepStatus(dst map[string]any, state sleepStatus) {
	if state.State == "" {
		state.State = "unknown"
	}
	// Clear old derived decoration on every response. Keep core's deadline
	// untouched, including an empty value or a timer retained during an early
	// event wake, pause, or active LLM call.
	for _, key := range []string{"sleep_thread_id", "sleep_started_at", "sleep_total_ms", "sleep_iteration"} {
		delete(dst, key)
	}
	dst["sleep_state"] = state.State
	dst["sleep_remaining_ms"] = state.Remaining.Milliseconds()
	if state.ThreadID != "" {
		dst["sleep_thread_id"] = state.ThreadID
	}
	if !state.StartedAt.IsZero() {
		dst["sleep_started_at"] = state.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if state.Total > 0 {
		dst["sleep_total_ms"] = state.Total.Milliseconds()
	}
	if state.Iteration > 0 {
		dst["sleep_iteration"] = state.Iteration
	}
}

func sleepStringValue(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return strings.TrimSpace(x.String())
	default:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(strconv.FormatFloat(sleepFloatValue(v), 'f', -1, 64))
	}
}

func sleepIntValue(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	default:
		return int(sleepFloatValue(v))
	}
}

func sleepBoolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func sleepFloatValue(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0
		}
		return f
	default:
		return 0
	}
}
