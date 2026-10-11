package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type proxyErrorDetail struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}
type proxyLegActivity struct {
	Leg                  string    `json:"leg"`
	ReadBytes            int64     `json:"read_bytes"`
	WriteBytes           int64     `json:"write_bytes"`
	Reads                uint64    `json:"reads"`
	Writes               uint64    `json:"writes"`
	LastRead             time.Time `json:"last_successful_read"`
	LastWrite            time.Time `json:"last_successful_write"`
	LastReadMonotonicNS  int64     `json:"last_read_monotonic_ns"`
	LastWriteMonotonicNS int64     `json:"last_write_monotonic_ns"`
	WriteDurationNS      int64     `json:"write_duration_ns"`
	MaxWriteDurationNS   int64     `json:"max_write_duration_ns"`
}
type proxyForwardingSnapshot struct {
	At                 time.Time           `json:"observed_at"`
	MonotonicNS        int64               `json:"monotonic_ns"`
	Sequence           uint64              `json:"event_sequence"`
	IntervalMS         int64               `json:"interval_ms"`
	Closed             bool                `json:"connection_closed"`
	Activity           [2]proxyLegActivity `json:"activity"`
	ReadBytes          [2]int64            `json:"interval_read_bytes"`
	WriteBytes         [2]int64            `json:"interval_write_bytes"`
	WriteDurationNS    [2]int64            `json:"interval_write_duration_ns"`
	MaxWriteDurationNS [2]int64            `json:"interval_max_write_duration_ns"`
}

func (t *websocketProxyTrace) setRoute(path string) {
	category := "diagnostic"
	// Only fixed route families are retained. IDs, tokens, queries and paths
	// themselves never enter telemetry. Apps can supply an authoritative category.
	if strings.Contains(path, "/softphone/media/") || strings.HasSuffix(path, "/softphone/media") {
		category = "browser_media"
	} else if strings.Contains(path, "/media/") || strings.HasSuffix(path, "/media") {
		category = "carrier_media"
	}
	t.mu.Lock()
	t.routeCategory = category
	t.mu.Unlock()
}
func (t *websocketProxyTrace) adoptResponse(resp *http.Response) {
	if resp == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if callID := boundedProxyCallID(resp.Header.Get(proxyCallIDHeader)); callID != "" {
		t.callID = callID
	}
	switch category := resp.Header.Get(proxyRouteCategoryHeader); category {
	case "browser_media", "carrier_media", "diagnostic":
		t.routeCategory = category
	}
}
func (t *websocketProxyTrace) observeIO(leg, operation string, n int, err error, duration time.Duration) {
	at := time.Now()
	index := 0
	if leg == "backend" {
		index = 1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// A realtime close handler can observe the same read error before
	// ReadMessage returns; count and report that operation only once.
	key := leg + ":" + operation
	if err != nil && t.seenErrors[key] {
		return
	}
	activity := &t.activity[index]
	activity.Leg = leg
	if operation == "read" {
		activity.Reads++
		activity.ReadBytes += int64(n)
		if n > 0 {
			activity.LastRead = at.UTC()
			activity.LastReadMonotonicNS = at.Sub(proxyProcessStart).Nanoseconds()
		}
	} else if operation == "write" {
		activity.Writes++
		activity.WriteBytes += int64(n)
		activity.WriteDurationNS += duration.Nanoseconds()
		activity.MaxWriteDurationNS = max(activity.MaxWriteDurationNS, duration.Nanoseconds())
		t.writeIntervalMax[index] = max(t.writeIntervalMax[index], duration.Nanoseconds())
		if n > 0 {
			activity.LastWrite = at.UTC()
			activity.LastWriteMonotonicNS = at.Sub(proxyProcessStart).Nanoseconds()
		}
	}
	if err == nil {
		return
	}
	// Repeated EOFs/close callbacks do not overwrite the first observation.
	t.seenErrors[key] = true
	source, destination := leg, "proxy"
	if operation == "write" {
		source, destination = "proxy", leg
	}
	category := realtimeProxyErrorCategory(err)
	if errors.Is(err, io.EOF) {
		category = "eof"
	}
	result := proxyDirectionTelemetry{Direction: leg, Operation: operation, EndedAt: at.UTC(), Bytes: int64(n),
		Error: proxyTransportError(err), ErrorType: fmt.Sprintf("%T", err), Category: category,
		SourceLeg: source, DestinationLeg: destination, MonotonicNS: at.Sub(proxyProcessStart).Nanoseconds(),
		EventSequence: proxyEventSequence.Add(1), Activity: t.activity, AfterFirstTermination: t.first != nil,
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		result.CloseCode = closeErr.Code
	}
	for _, id := range t.localClose {
		result.LocalCloseRequested = true
		result.LocalCloseEventID = id
		break
	}
	for current, depth := err, 0; current != nil && depth < 8; current, depth = errors.Unwrap(current), depth+1 {
		result.ErrorChain = append(result.ErrorChain, proxyErrorDetail{Type: fmt.Sprintf("%T", current), Error: proxyTransportError(current)})
	}
	kind := "cleanup"
	if t.first == nil {
		kind = "first_termination"
	}
	result.EventID = generateID()
	if t.first == nil {
		saved := result
		t.first = &saved
		t.incidentAt = at
	}
	t.emitLocked(kind, at, map[string]any{"termination": result, "event_id": result.EventID, "event_sequence": result.EventSequence, "first_termination_event_id": t.first.EventID})
}
func (t *websocketProxyTrace) setCloser(leg string, closer io.Closer) {
	t.mu.Lock()
	t.closers[leg] = closer
	reason := t.closeReason
	t.mu.Unlock()
	if reason == "request_canceled" || reason == "server_shutdown" {
		t.requestClose(leg, "close", reason, "")
		_ = closer.Close()
	}
}
func (t *websocketProxyTrace) requestClose(leg, operation, reason, trigger string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := leg + ":" + operation
	if _, exists := t.localClose[key]; exists {
		return
	}
	if reason == "" {
		reason = t.closeReason
		// ReverseProxy also watches context cancellation. Its close may run
		// before our watcher; derive the reason from the source signal too.
		if t.requestContext != nil && t.requestContext.Err() != nil {
			reason = "request_canceled"
		}
		if t.server != nil {
			select {
			case <-t.server.proxyTelemetry().shutdown:
				reason = "server_shutdown"
			default:
			}
		}
	}
	if trigger == "" && t.first != nil {
		trigger = t.first.EventID
	}
	if reason == "" {
		if t.first != nil {
			reason = "other_leg_terminated"
			if t.first.SourceLeg == leg || t.first.DestinationLeg == leg {
				reason = "leg_terminated"
			}
		} else {
			reason = "backend_unavailable"
		}
	}
	if trigger == "" || reason == "server_shutdown" || reason == "request_canceled" {
		trigger = t.handshakeEventID
		if trigger == "" || reason == "server_shutdown" || reason == "request_canceled" {
			if t.closeCauseIDs == nil {
				t.closeCauseIDs = make(map[string]string)
			}
			trigger = t.closeCauseIDs[reason]
			if trigger == "" {
				trigger = t.emitLocked("close_trigger", time.Now(), map[string]any{"reason": reason})
				t.closeCauseIDs[reason] = trigger
			}
		}
	}
	t.localClose[key] = t.emitLocked("local_close_requested", time.Now(), map[string]any{"operation": operation, "destination_leg": leg, "reason": reason, "trigger_event_id": trigger})
}
func (t *websocketProxyTrace) closeConnections(reason string) {
	t.mu.Lock()
	if reason != "" {
		t.closeReason = reason
	}
	closers := make(map[string]io.Closer, len(t.closers))
	for leg, closer := range t.closers {
		closers[leg] = closer
	}
	t.mu.Unlock()
	for leg, closer := range closers {
		t.requestClose(leg, "close", reason, "")
		_ = closer.Close()
	}
}
func (t *websocketProxyTrace) watch(r *http.Request) func() {
	done := make(chan struct{})
	var shutdown <-chan struct{}
	if t.server != nil {
		shutdown = t.server.proxyTelemetry().shutdown
	}
	go func() {
		select {
		case <-done:
		case <-shutdown:
			t.closeConnections("server_shutdown")
		case <-r.Context().Done():
			t.closeConnections("request_canceled")
		}
	}()
	return func() { close(done) }
}

// sample retains at most 60 pre-incident + 31 boundary/post-incident samples.
// A delayed ticker is represented by interval_ms, never fabricated samples.
func (t *websocketProxyTrace) sample(at time.Time, final bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active.Load() {
		return final
	}
	interval := time.Second
	if !t.lastSample.IsZero() {
		interval = at.Sub(t.lastSample)
	}
	sample := proxyForwardingSnapshot{At: at.UTC(), MonotonicNS: at.Sub(proxyProcessStart).Nanoseconds(), Sequence: proxyEventSequence.Add(1), IntervalMS: interval.Milliseconds(), Closed: t.closed, Activity: t.activity}
	for i := range t.activity {
		sample.ReadBytes[i] = t.activity[i].ReadBytes - t.previousActivity[i].ReadBytes
		sample.WriteBytes[i] = t.activity[i].WriteBytes - t.previousActivity[i].WriteBytes
		sample.WriteDurationNS[i] = t.activity[i].WriteDurationNS - t.previousActivity[i].WriteDurationNS
		sample.MaxWriteDurationNS[i] = t.writeIntervalMax[i]
		t.writeIntervalMax[i] = 0
	}
	t.previousActivity = t.activity
	t.lastSample = at
	t.history = append(t.history, sample)
	cutoffNS := at.Sub(proxyProcessStart).Nanoseconds() - int64(60*time.Second)
	if !t.incidentAt.IsZero() {
		cutoffNS = t.incidentAt.Sub(proxyProcessStart).Nanoseconds() - int64(60*time.Second)
	}
	for len(t.history) > 0 && t.history[0].MonotonicNS < cutoffNS {
		t.history = t.history[1:]
	}
	if len(t.history) > 91 {
		t.history = t.history[len(t.history)-91:]
	}
	complete := !t.incidentAt.IsZero() && !at.Before(t.incidentAt.Add(30*time.Second))
	if !complete && !final {
		return false
	}
	// Small chunks bound queued payload memory and the size of each DB event.
	count := (len(t.history) + 7) / 8
	for start := 0; start < len(t.history); start += 8 {
		samples := append([]proxyForwardingSnapshot(nil), t.history[start:min(start+8, len(t.history))]...)
		trigger := ""
		if t.first != nil {
			trigger = t.first.EventID
		}
		t.emitLocked("forwarding_history", at, map[string]any{"samples": samples, "part": start/8 + 1, "parts": count, "incident_event_id": trigger, "pre_seconds": 60, "post_seconds": 30, "window_complete": complete, "shutdown_truncated": final && !complete})
	}
	t.history = nil
	return true
}
