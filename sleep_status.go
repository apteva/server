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

func (s *Server) enrichAgentStatusBody(instanceID int64, body []byte, now time.Time) ([]byte, bool) {
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		return body, false
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
	if sleepStringValue(status["rate"]) == "stopped" {
		return sleepStatus{State: "stopped"}
	}
	if sleepBoolValue(status["paused"]) {
		return sleepStatus{State: "paused"}
	}
	latest := s.latestLLMDoneByThread(instanceID, 100)
	ev, ok := newestTelemetryEvent(latest)
	if !ok {
		if total, ok := parseSleepDisplayDuration(sleepStringValue(status["rate"])); ok {
			return sleepStatus{State: "unknown", Total: total}
		}
		return sleepStatus{State: "unknown"}
	}
	state := sleepStatusFromTelemetry(ev, now)
	if currentIter := sleepIntValue(status["iteration"]); currentIter > 0 && state.Iteration > 0 && currentIter > state.Iteration {
		state.State = "active"
		state.Remaining = 0
		state.NextWakeAt = time.Time{}
	}
	return state
}

func computeThreadSleepStatus(thread map[string]any, ev TelemetryEvent, now time.Time) sleepStatus {
	if sleepStringValue(thread["rate"]) == "stopped" {
		return sleepStatus{State: "stopped"}
	}
	if ev.Type != "llm.done" {
		if total, ok := parseSleepDisplayDuration(sleepStringValue(thread["rate"])); ok {
			return sleepStatus{State: "unknown", Total: total}
		}
		return sleepStatus{State: "unknown"}
	}
	state := sleepStatusFromTelemetry(ev, now)
	if currentIter := sleepIntValue(thread["iteration"]); currentIter > 0 && state.Iteration > 0 && currentIter > state.Iteration {
		state.State = "active"
		state.Remaining = 0
		state.NextWakeAt = time.Time{}
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

func sleepStatusFromTelemetry(ev TelemetryEvent, now time.Time) sleepStatus {
	data := map[string]any{}
	_ = json.Unmarshal(ev.Data, &data)
	total, ok := parseSleepDisplayDuration(sleepStringValue(data["rate"]))
	if !ok {
		total = 0
	}
	next := time.Time{}
	remaining := time.Duration(0)
	state := "unknown"
	if !ev.Time.IsZero() && total > 0 {
		next = ev.Time.Add(total)
		remaining = time.Until(next)
		if !now.IsZero() {
			remaining = next.Sub(now)
		}
		if remaining > 0 {
			state = "sleeping"
		} else {
			state = "overdue"
			remaining = 0
		}
	}
	threadID := ev.ThreadID
	if threadID == "" {
		threadID = "main"
	}
	return sleepStatus{
		State:      state,
		ThreadID:   threadID,
		StartedAt:  ev.Time,
		NextWakeAt: next,
		Total:      total,
		Remaining:  remaining,
		Iteration:  sleepIntValue(data["iteration"]),
	}
}

func applySleepStatus(dst map[string]any, state sleepStatus) {
	if state.State == "" {
		state.State = "unknown"
	}
	dst["sleep_state"] = state.State
	if state.ThreadID != "" {
		dst["sleep_thread_id"] = state.ThreadID
	}
	if !state.StartedAt.IsZero() {
		dst["sleep_started_at"] = state.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !state.NextWakeAt.IsZero() && state.State != "active" {
		dst["next_wake_at"] = state.NextWakeAt.UTC().Format(time.RFC3339Nano)
	}
	if state.Total > 0 {
		dst["sleep_total_ms"] = state.Total.Milliseconds()
	}
	if state.Remaining > 0 {
		dst["sleep_remaining_ms"] = state.Remaining.Milliseconds()
	} else {
		dst["sleep_remaining_ms"] = 0
	}
	if state.Iteration > 0 {
		dst["sleep_iteration"] = state.Iteration
	}
}

func newestTelemetryEvent(events map[string]TelemetryEvent) (TelemetryEvent, bool) {
	var newest TelemetryEvent
	ok := false
	for _, ev := range events {
		if !ok || ev.Time.After(newest.Time) {
			newest = ev
			ok = true
		}
	}
	return newest, ok
}

func parseSleepDisplayDuration(raw string) (time.Duration, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, false
	}
	switch s {
	case "reactive":
		return 500 * time.Millisecond, true
	case "fast":
		return 2 * time.Second, true
	case "normal":
		return 10 * time.Second, true
	case "slow":
		return 30 * time.Second, true
	case "sleep":
		return 2 * time.Minute, true
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	if d < 500*time.Millisecond {
		d = 500 * time.Millisecond
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d, true
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
