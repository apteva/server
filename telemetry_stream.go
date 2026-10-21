package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type telemetryStreamCursor struct {
	Epoch string `json:"e"`
	Seq   uint64 `json:"s"`
	Query string `json:"q"`
}

func encodeTelemetryCursor(epoch string, seq uint64, q sdk.TelemetryQuery) string {
	raw, _ := json.Marshal(telemetryStreamCursor{epoch, seq, telemetryFingerprint(q)})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Atomically copy the existing bounded ring, including the process epoch. An
// epoch prevents a cursor from accidentally matching a later Server process.
func (b *TelemetryBroadcaster) replayTelemetry(epoch string, since uint64) ([]TelemetryEvent, string, uint64, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.replayEpoch
	start := 0
	if b.replaySize == telemetryReplayCap {
		start = b.replayHead
	}
	oldest := b.nextSeq + 1
	if b.replaySize > 0 {
		oldest = b.replay[start].Seq
	}
	reason := ""
	if epoch != "" && epoch != current {
		reason = "server_restarted"
	} else if since > b.nextSeq {
		reason = "cursor_reset"
	} else if epoch != "" && since+1 < oldest {
		reason = "retention_exceeded"
	}
	if reason != "" || epoch == "" {
		since = oldest - 1
	}
	out := []TelemetryEvent{}
	for i := 0; i < b.replaySize; i++ {
		ev := b.replay[(start+i)%telemetryReplayCap]
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out, current, since, reason
}
func matchesTelemetry(q sdk.TelemetryQuery, ev TelemetryEvent) bool {
	if q.AgentID != 0 && q.AgentID != ev.AgentID || q.ThreadID != "" && q.ThreadID != ev.ThreadID || q.ThreadPrefix != "" && !strings.HasPrefix(ev.ThreadID, q.ThreadPrefix) {
		return false
	}
	if !q.Since.IsZero() && ev.Time.Before(q.Since) || !q.Until.IsZero() && ev.Time.After(q.Until) {
		return false
	}
	if len(q.Events) == 0 {
		return true
	}
	for _, filter := range q.Events {
		if ev.Type == filter || !strings.Contains(filter, ".") && strings.HasPrefix(ev.Type, filter+".") {
			return true
		}
	}
	return false
}
func (s *Server) streamTelemetry(w http.ResponseWriter, r *http.Request, p telemetryPrincipal, q sdk.TelemetryQuery) {
	rich := r.URL.Query().Get("format") == "delivery"
	if len(q.Events) == 0 && (p.installID != 0 || rich) {
		http.Error(w, "events filter required for live telemetry", 400)
		return
	}
	if s.broadcaster == nil {
		http.Error(w, "telemetry unavailable", 503)
		return
	}
	if !rich && r.URL.Query().Get("backfill") == "true" {
		http.Error(w, "backfill requires format=delivery", 400)
		return
	}
	if r.URL.Query().Get("replay") == "false" && r.URL.Query().Get("backfill") == "true" {
		http.Error(w, "live-only subscriptions cannot request backfill", 400)
		return
	}
	var cursor telemetryStreamCursor
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Epoch == "" || cursor.Query != telemetryFingerprint(q) {
			http.Error(w, "invalid stream cursor or changed filters", 400)
			return
		}
	}
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ch, liveCursor := s.broadcaster.SubscribeAllCursor()
	if r.URL.Query().Get("replay") == "false" || !rich && q.Cursor == "" {
		cursor = telemetryStreamCursor{Epoch: s.broadcaster.replayEpoch, Seq: liveCursor}
	}
	defer s.broadcaster.UnsubscribeAll(ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	// Preserve the single-agent CLI channel's connection lifecycle.
	if p.installID == 0 && p.legacy && q.AgentID != 0 && s.agents != nil {
		if channels := s.agents.GetChannels(q.AgentID); channels != nil && channels.cli != nil {
			channels.cli.Connect()
			defer channels.cli.Disconnect()
		}
	}
	lastHeartbeat := time.Now()
	send := func(frame sdk.TelemetryDelivery) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if !rich {
			switch frame.Kind {
			case "event":
				raw, err := json.Marshal(frame.Event)
				if err != nil {
					return false
				}
				if frame.Event.Seq > 0 {
					if _, err = fmt.Fprintf(w, "id: %d\n", frame.Event.Seq); err != nil {
						return false
					}
				}
				if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
					return false
				}
			case "ready":
				if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
					return false
				}
			case "gap":
				if p.installID == 0 {
					if _, err := fmt.Fprintf(w, "event: reset\ndata: {\"reason\":%s}\n\n", strconv.Quote(frame.Reason)); err != nil {
						return false
					}
				} else {
					if _, err := fmt.Fprintf(w, ": gap %s\n\n", frame.Reason); err != nil {
						return false
					}
				}
			case "checkpoint":
				if time.Since(lastHeartbeat) < 20*time.Second {
					return true
				}
				lastHeartbeat = time.Now()
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return false
				}
			case "error":
				return false
			default:
				return true
			}
			return controller.Flush() == nil
		}
		frame.ServerID = s.telemetryServerID()
		raw, e := json.Marshal(frame)
		if e != nil {
			return false
		}
		if frame.Cursor != "" {
			if _, e = fmt.Fprintf(w, "id: %s\n", frame.Cursor); e != nil {
				return false
			}
		}
		if _, e = fmt.Fprintf(w, "data: %s\n\n", raw); e != nil {
			return false
		}
		return controller.Flush() == nil
	}
	epoch, last := cursor.Epoch, cursor.Seq
	initial := true
	checkedBackfill := false
	refresh := time.NewTicker(time.Second)
	defer refresh.Stop()
	// Authorization is recomputed at least each second, and immediately after
	// an agent scope revision. No permission decision survives a reconnect.
	var agents map[int64]telemetryAgent
	var checked time.Time
	var revision uint64
	refreshScope := func() bool {
		if checked.IsZero() || time.Since(checked) >= time.Second || revision != s.store.agentEligibilityRevision.Load() {
			recorder := telemetryAuthWriter{}
			currentRevision := s.store.agentEligibilityRevision.Load()
			fresh, ok := s.telemetryPrincipal(&recorder, r, p.installID, p.legacy)
			if !ok || !fresh.telemetryAllowed(q) || fresh.project != q.ProjectID || fresh.userID != p.userID {
				send(sdk.TelemetryDelivery{Kind: "error", Error: "telemetry access revoked"})
				return false
			}
			var e error
			agents, e = s.telemetryAgents(fresh, q)
			if e != nil {
				send(sdk.TelemetryDelivery{Kind: "error", Error: "telemetry scope no longer available"})
				return false
			}
			p = fresh
			checked = time.Now()
			revision = currentRevision
		}
		return true
	}
	deliver := func() bool {
		if !refreshScope() {
			return false
		}
		events, current, start, reason := s.broadcaster.replayTelemetry(epoch, last)
		if initial {
			if reason != "" {
				if !send(sdk.TelemetryDelivery{Kind: "gap", Reason: reason, Cursor: encodeTelemetryCursor(current, start, q)}) {
					return false
				}
				reason = ""
			}
			if !send(sdk.TelemetryDelivery{Kind: "ready", Cursor: encodeTelemetryCursor(current, start, q)}) {
				return false
			}

			initial = false
		}
		if reason != "" {
			if !send(sdk.TelemetryDelivery{Kind: "gap", Reason: reason, Cursor: encodeTelemetryCursor(current, start, q)}) {
				return false
			}
		}
		if r.URL.Query().Get("backfill") == "true" && (checkedBackfill == false || reason != "") {
			historyQuery := q
			historyQuery.Cursor = ""
			history, e := s.queryTelemetryPage(r, historyQuery, agents)
			if e != nil {
				send(sdk.TelemetryDelivery{Kind: "error", Error: "telemetry history unavailable"})
				return false
			}
			if !send(sdk.TelemetryDelivery{Kind: "history", History: &history}) {
				return false
			}
			checkedBackfill = true
		}
		epoch = current
		last = start
		for _, ev := range events {
			if !refreshScope() {
				return false
			}
			if a, ok := agents[ev.AgentID]; ok && matchesTelemetry(q, ev) {
				event := s.projectTelemetryEvent(ev, a, q.Projection)
				if !send(sdk.TelemetryDelivery{Kind: "event", Event: &event, Cursor: encodeTelemetryCursor(epoch, ev.Seq, q)}) {
					return false
				}
			}
			last = ev.Seq
		}
		return send(sdk.TelemetryDelivery{Kind: "checkpoint", Cursor: encodeTelemetryCursor(epoch, last, q)})
	}
	if !deliver() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
			for len(ch) > 0 {
				<-ch
			}
			if !deliver() {
				return
			}
		case <-refresh.C:
			if !deliver() {
				return
			}
		}
	}
}

// Rechecking an open stream must not write HTTP errors into an SSE body.
type telemetryAuthWriter struct{}

func (w *telemetryAuthWriter) Header() http.Header         { return http.Header{} }
func (w *telemetryAuthWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *telemetryAuthWriter) WriteHeader(int)             {}
