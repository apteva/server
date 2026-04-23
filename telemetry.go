package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TelemetryBroadcaster fans out events to SSE subscribers.
type TelemetryBroadcaster struct {
	mu          sync.Mutex
	subscribers map[int64][]chan TelemetryEvent // instanceID → channels
	nextID      int64
}

func NewTelemetryBroadcaster() *TelemetryBroadcaster {
	return &TelemetryBroadcaster{
		subscribers: make(map[int64][]chan TelemetryEvent),
	}
}

// Subscribe returns a channel that receives events for the given instance.
func (b *TelemetryBroadcaster) Subscribe(instanceID int64) chan TelemetryEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan TelemetryEvent, 100)
	b.subscribers[instanceID] = append(b.subscribers[instanceID], ch)
	return ch
}

// Unsubscribe removes a channel.
func (b *TelemetryBroadcaster) Unsubscribe(instanceID int64, ch chan TelemetryEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[instanceID]
	for i, s := range subs {
		if s == ch {
			b.subscribers[instanceID] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// SubscribeAll returns a channel that receives events for all instances.
// Used by the console logger to render all activity, and by the dashboard's
// Instances list page to render a per-row live activity strip without
// opening N concurrent SSE connections.
func (b *TelemetryBroadcaster) SubscribeAll() chan TelemetryEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan TelemetryEvent, 200)
	// Use instanceID -1 as the "all" sentinel
	b.subscribers[-1] = append(b.subscribers[-1], ch)
	return ch
}

// UnsubscribeAll removes a channel previously returned by SubscribeAll.
// Mirrors Unsubscribe but with the -1 sentinel key.
func (b *TelemetryBroadcaster) UnsubscribeAll(ch chan TelemetryEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[-1]
	for i, s := range subs {
		if s == ch {
			b.subscribers[-1] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Broadcast sends events to all subscribers for the given instance,
// plus any "all" subscribers (instanceID -1).
func (b *TelemetryBroadcaster) Broadcast(events []TelemetryEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range events {
		for _, ch := range b.subscribers[ev.InstanceID] {
			select {
			case ch <- ev:
			default:
			}
		}
		// Fan out to "all" subscribers
		for _, ch := range b.subscribers[-1] {
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// TelemetryEvent is the unified event format.
type TelemetryEvent struct {
	ID         string          `json:"id"`
	InstanceID int64           `json:"instance_id"`
	ThreadID   string          `json:"thread_id"`
	Type       string          `json:"type"`
	Time       time.Time       `json:"time"`
	Data       json.RawMessage `json:"data"`
}

// TelemetryStats holds aggregated statistics.
type TelemetryStats struct {
	TotalEvents    int     `json:"total_events"`
	LLMCalls       int     `json:"llm_calls"`
	TotalTokensIn  int     `json:"total_tokens_in"`
	TotalTokensOut int     `json:"total_tokens_out"`
	TotalCost      float64 `json:"total_cost"`
	AvgDurationMs  float64 `json:"avg_duration_ms"`
	ThreadsSpawned int     `json:"threads_spawned"`
	ThreadsDone    int     `json:"threads_done"`
	ToolCalls      int     `json:"tool_calls"`
	Errors         int     `json:"errors"`
}

// InstanceStats is one row of the per-instance aggregate for a project
// over some period — enough to rank instances by spend and surface
// simple anomaly signals (error count, cache hit rate, burn rate).
// Emitted by TelemetryStatsByProject and the /telemetry/project-stats
// endpoint; the dashboard uses this to render the "biggest spenders"
// view.
type InstanceStats struct {
	InstanceID     int64   `json:"instance_id"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	LLMCalls       int     `json:"llm_calls"`
	TokensIn       int     `json:"tokens_in"`
	TokensOut      int     `json:"tokens_out"`
	TokensCached   int     `json:"tokens_cached"`
	Cost           float64 `json:"cost"`
	Errors         int     `json:"errors"`
	ToolCalls      int     `json:"tool_calls"`
	AvgDurationMs  float64 `json:"avg_duration_ms"`
	DistinctThreads int    `json:"distinct_threads"`
}

// ProjectTimelineBucket: one time slice of project-wide spend with a
// per-instance breakdown. CostByInstance is keyed by instance id
// (stringified — JSON object keys must be strings).
type ProjectTimelineBucket struct {
	Time            string              `json:"time"`
	Cost            float64             `json:"cost"`
	TokensIn        int                 `json:"tokens_in"`
	TokensOut       int                 `json:"tokens_out"`
	LLMCalls        int                 `json:"llm_calls"`
	Errors          int                 `json:"errors"`
	CostByInstance  map[string]float64  `json:"cost_by_instance"`
	CallsByInstance map[string]int      `json:"calls_by_instance"`
}

func generateID() string {
	// Simple time-prefixed random ID
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// --- Store methods ---

func (s *Store) InsertTelemetry(events []TelemetryEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT OR IGNORE INTO telemetry (id, instance_id, thread_id, type, time, data) VALUES (?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range events {
		if e.ID == "" {
			e.ID = generateID()
		}
		if e.ThreadID == "" {
			e.ThreadID = "main"
		}
		timeStr := e.Time.UTC().Format(time.RFC3339)
		dataStr := "{}"
		if e.Data != nil {
			dataStr = string(e.Data)
		}
		stmt.Exec(e.ID, e.InstanceID, e.ThreadID, e.Type, timeStr, dataStr)
	}

	return tx.Commit()
}

func (s *Store) QueryTelemetry(instanceID int64, eventType string, since time.Time, limit int, threadID ...string) ([]TelemetryEvent, error) {
	query := "SELECT id, instance_id, thread_id, type, time, data FROM telemetry WHERE instance_id = ?"
	args := []any{instanceID}

	if len(threadID) > 0 && threadID[0] != "" {
		query += " AND thread_id = ?"
		args = append(args, threadID[0])
	}

	if eventType != "" {
		// Support prefix matching: "llm" matches "llm.start", "llm.done", etc.
		if strings.Contains(eventType, ".") {
			query += " AND type = ?"
			args = append(args, eventType)
		} else {
			query += " AND type LIKE ?"
			args = append(args, eventType+".%")
		}
	}

	if !since.IsZero() {
		query += " AND time >= ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}

	query += " ORDER BY time DESC"

	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []TelemetryEvent
	for rows.Next() {
		var e TelemetryEvent
		var timeStr, dataStr string
		rows.Scan(&e.ID, &e.InstanceID, &e.ThreadID, &e.Type, &timeStr, &dataStr)
		e.Time, _ = parseTime(timeStr)
		e.Data = json.RawMessage(dataStr)
		events = append(events, e)
	}
	return events, nil
}

// ChatHistoryMessage is a reconstructed chat message from telemetry.
type ChatHistoryMessage struct {
	ID   string `json:"id"`
	Role string `json:"role"` // "user", "agent", "tool", "status"
	Text string `json:"text"`
	Time string `json:"time"`
	// Tool fields (only for role=tool)
	ToolName      string `json:"tool_name,omitempty"`
	ToolDone      bool   `json:"tool_done,omitempty"`
	ToolDurationMs int64 `json:"tool_duration_ms,omitempty"`
	ToolSuccess   bool   `json:"tool_success,omitempty"`
}

// QueryChatHistory reconstructs a chat conversation from telemetry events.
// Returns messages in chronological order.
func (s *Store) QueryChatHistory(instanceID int64, limit int) ([]ChatHistoryMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// Query event types that contribute to chat:
	//   event.received (source=cli)  → user messages
	//   tool.call (channels_respond) → agent messages
	//   tool.call (other, main thread) → tool indicators
	//   tool.result (other, main thread) → tool completion
	rows, err := s.db.Query(`
		SELECT id, thread_id, type, time, data FROM telemetry
		WHERE instance_id = ?
		  AND thread_id IN ('main', '')
		  AND type IN ('event.received', 'tool.call', 'tool.result')
		ORDER BY time DESC
		LIMIT ?
	`, instanceID, limit*3) // over-fetch to account for filtering
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hiddenTools := map[string]bool{
		"pace": true, "done": true, "evolve": true, "remember": true, "send": true,
		"channels_respond": true, "channels_status": true,
	}

	type rawEvent struct {
		id       string
		threadID string
		typ      string
		timeStr  string
		data     map[string]any
	}

	var raw []rawEvent
	for rows.Next() {
		var e rawEvent
		var dataStr string
		rows.Scan(&e.id, &e.threadID, &e.typ, &e.timeStr, &dataStr)
		json.Unmarshal([]byte(dataStr), &e.data)
		if e.data == nil {
			e.data = map[string]any{}
		}
		raw = append(raw, e)
	}

	// Reverse to chronological order
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}

	// Build tool result lookup (id → result data)
	toolResults := map[string]rawEvent{}
	for _, e := range raw {
		if e.typ == "tool.result" {
			if id, ok := e.data["id"].(string); ok && id != "" {
				toolResults[id] = e
			}
		}
	}

	var msgs []ChatHistoryMessage
	for _, e := range raw {
		switch e.typ {
		case "event.received":
			source, _ := e.data["source"].(string)
			if source != "cli" {
				continue
			}
			msg, _ := e.data["message"].(string)
			msg = strings.TrimPrefix(msg, "[cli] ")
			if msg == "" || strings.Contains(msg, "connected via dashboard") || strings.Contains(msg, "disconnected from terminal") {
				continue
			}
			msgs = append(msgs, ChatHistoryMessage{
				ID: e.id, Role: "user", Text: msg, Time: e.timeStr,
			})

		case "tool.call":
			name, _ := e.data["name"].(string)
			if name == "" {
				continue
			}

			if name == "channels_respond" {
				args, _ := e.data["args"].(map[string]any)
				if args == nil {
					continue
				}
				channel, _ := args["channel"].(string)
				if channel != "" && channel != "cli" {
					continue
				}
				text, _ := args["text"].(string)
				if text == "" {
					continue
				}
				msgs = append(msgs, ChatHistoryMessage{
					ID: e.id, Role: "agent", Text: text, Time: e.timeStr,
				})
				continue
			}

			if hiddenTools[name] {
				continue
			}

			// Visible tool call
			callID, _ := e.data["id"].(string)
			reason, _ := e.data["reason"].(string)
			m := ChatHistoryMessage{
				ID: e.id, Role: "tool", Text: reason, Time: e.timeStr,
				ToolName: name, ToolDone: true, ToolSuccess: true,
			}
			if res, ok := toolResults[callID]; ok {
				if dur, ok := res.data["duration_ms"].(float64); ok {
					m.ToolDurationMs = int64(dur)
				}
				if success, ok := res.data["success"].(bool); ok {
					m.ToolSuccess = success
				}
			}
			msgs = append(msgs, m)
		}

		if len(msgs) >= limit {
			break
		}
	}

	return msgs, nil
}

func (s *Store) TelemetryStats(instanceID int64, since time.Time) (*TelemetryStats, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	stats := &TelemetryStats{}

	// Total events
	s.db.QueryRow(
		"SELECT COUNT(*) FROM telemetry WHERE instance_id = ? AND time >= ?",
		instanceID, sinceStr,
	).Scan(&stats.TotalEvents)

	// LLM calls + token/cost aggregation from llm.done data
	rows, err := s.db.Query(
		"SELECT data FROM telemetry WHERE instance_id = ? AND type = 'llm.done' AND time >= ?",
		instanceID, sinceStr,
	)
	if err == nil {
		defer rows.Close()
		var totalDuration float64
		for rows.Next() {
			var dataStr string
			rows.Scan(&dataStr)
			var d map[string]any
			if json.Unmarshal([]byte(dataStr), &d) == nil {
				stats.LLMCalls++
				if v, ok := d["tokens_in"].(float64); ok {
					stats.TotalTokensIn += int(v)
				}
				if v, ok := d["tokens_out"].(float64); ok {
					stats.TotalTokensOut += int(v)
				}
				if v, ok := d["cost_usd"].(float64); ok {
					stats.TotalCost += v
				}
				if v, ok := d["duration_ms"].(float64); ok {
					totalDuration += v
				}
			}
		}
		if stats.LLMCalls > 0 {
			stats.AvgDurationMs = totalDuration / float64(stats.LLMCalls)
		}
	}

	// Thread counts
	s.db.QueryRow(
		"SELECT COUNT(*) FROM telemetry WHERE instance_id = ? AND type = 'thread.spawn' AND time >= ?",
		instanceID, sinceStr,
	).Scan(&stats.ThreadsSpawned)

	s.db.QueryRow(
		"SELECT COUNT(*) FROM telemetry WHERE instance_id = ? AND type = 'thread.done' AND time >= ?",
		instanceID, sinceStr,
	).Scan(&stats.ThreadsDone)

	// Tool calls
	s.db.QueryRow(
		"SELECT COUNT(*) FROM telemetry WHERE instance_id = ? AND type = 'tool.call' AND time >= ?",
		instanceID, sinceStr,
	).Scan(&stats.ToolCalls)

	// Errors
	s.db.QueryRow(
		"SELECT COUNT(*) FROM telemetry WHERE instance_id = ? AND type LIKE '%.error' AND time >= ?",
		instanceID, sinceStr,
	).Scan(&stats.Errors)

	return stats, nil
}

func (s *Store) CleanOldTelemetry(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	result, err := s.db.Exec("DELETE FROM telemetry WHERE time < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) WipeTelemetry() (int64, error) {
	result, err := s.db.Exec("DELETE FROM telemetry")
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// --- HTTP Handlers ---

// POST /telemetry — batch ingest from core instances
func (s *Server) handleIngestTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Require instance secret
	if s.instanceSecret != "" {
		if r.Header.Get("X-Instance-Secret") != s.instanceSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var events []TelemetryEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		log.Printf("[TELEMETRY] POST /telemetry: invalid JSON: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if len(events) == 0 {
		writeJSON(w, map[string]int{"inserted": 0})
		return
	}

	// Enrich llm.done events with server-side cost calculation.
	// Pricing data lives here, not in core — core emits raw token
	// counts + model, and this pass layers in cost_usd on persist.
	s.enrichCostInPlace(events)

	if err := s.store.InsertTelemetry(events); err != nil {
		http.Error(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// React to directive changes — update DB so dashboard sees it immediately
	for _, ev := range events {
		if ev.Type == "directive.evolved" && ev.InstanceID > 0 {
			var data struct{ New string `json:"new"` }
			if json.Unmarshal(ev.Data, &data) == nil && data.New != "" {
				s.store.db.Exec("UPDATE instances SET directive=? WHERE id=?", data.New, ev.InstanceID)
			}
		}
	}

	// No broadcast here — /telemetry/live handles real-time broadcast.
	// This endpoint is for DB persistence only.

	writeJSON(w, map[string]int{"inserted": len(events)})
}

// POST /telemetry/live — broadcast-only (not stored), for streaming chunks
func (s *Server) handleLiveTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Require instance secret. Mismatches are silent — if a debugging pass
	// is needed, re-add a log here scoped to the failing path.
	if s.instanceSecret != "" {
		if r.Header.Get("X-Instance-Secret") != s.instanceSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var events []TelemetryEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Enrich live events too — the dashboard's per-instance streaming
	// cost + context bars all read cost_usd. If we skip this, the
	// streaming path shows $0.00 until the persistence ingest catches
	// up and the caller refetches.
	s.enrichCostInPlace(events)

	s.broadcaster.Broadcast(events)
	writeJSON(w, map[string]int{"broadcast": len(events)})
}

// enrichCostInPlace scans events for llm.done payloads and adds a
// server-computed cost_usd to each. The event's own `model` string is
// the lookup key — we match the model across the pricing table
// regardless of which provider the instance was configured against,
// so multi-provider instances (e.g. pool with fireworks default but an
// occasional openai fallback) are priced correctly. Cached tokens are
// priced at the cached rate where the model exposes one.
func (s *Server) enrichCostInPlace(events []TelemetryEvent) {
	for i, ev := range events {
		if ev.Type != "llm.done" || ev.Data == nil {
			continue
		}
		var data map[string]any
		if json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		model, _ := data["model"].(string)
		if model == "" {
			continue
		}
		tokIn, _ := data["tokens_in"].(float64)
		tokCached, _ := data["tokens_cached"].(float64)
		tokOut, _ := data["tokens_out"].(float64)
		if tokIn == 0 && tokOut == 0 {
			continue
		}
		input, cached, output, ok := LookupModelPricing(model)
		if !ok {
			continue
		}
		uncached := tokIn - tokCached
		if uncached < 0 {
			uncached = 0
		}
		cost := (uncached*input + tokCached*cached + tokOut*output) / 1_000_000
		data["cost_usd"] = cost
		events[i].Data, _ = json.Marshal(data)
	}
}

// GET /telemetry/stream
//
// Two modes:
//
//   1. ?instance_id=N — SSE for one specific instance. Used by per-instance
//      panels that need every event from a single core. The caller must own
//      the instance (verified against the session user).
//
//   2. ?all=1[&project_id=…] — SSE for every running instance the caller
//      owns, in the (optional) given project. Used by the Instances list
//      page to render a live activity strip per row without N concurrent
//      connections. Filtering happens inside this handler — the broadcaster
//      itself fans every event to every "all" subscriber.
//
// Both modes require authentication. Auth is handled by the route's
// authMiddleware wrapper in main.go; we extract the user id and use it
// to scope which events are forwarded.
func (s *Server) handleTelemetryStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	userID := getUserID(r)
	q := r.URL.Query()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Mode 2: all-instances stream, scoped to the caller's instances.
	if q.Get("all") == "1" {
		projectID := q.Get("project_id")
		insts, err := s.store.ListInstances(userID, projectID)
		if err != nil {
			http.Error(w, "failed to list instances: "+err.Error(), http.StatusInternalServerError)
			return
		}
		allowed := make(map[int64]bool, len(insts))
		for _, in := range insts {
			allowed[in.ID] = true
		}

		ch := s.broadcaster.SubscribeAll()
		defer s.broadcaster.UnsubscribeAll(ch)

		// Heartbeat so browsers / proxies don't time the stream out
		// when traffic is sparse. SSE comments (`: ping`) are ignored
		// by EventSource.
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()
			case ev := <-ch:
				if !allowed[ev.InstanceID] {
					// New instance created mid-stream — refresh allowed
					// set lazily before dropping. This catches "user starts
					// instance after page load" without polling.
					if newInsts, err := s.store.ListInstances(userID, projectID); err == nil {
						for _, in := range newInsts {
							allowed[in.ID] = true
						}
					}
					if !allowed[ev.InstanceID] {
						continue
					}
				}
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	}

	// Mode 1: single-instance stream.
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "instance_id required (or ?all=1 for all-instances stream)", http.StatusBadRequest)
		return
	}
	// Verify the caller owns this instance.
	if _, err := s.store.GetInstance(userID, instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	ch := s.broadcaster.Subscribe(instanceID)
	defer s.broadcaster.Unsubscribe(instanceID, ch)

	// Track CLI channel connection so the agent knows a user is listening.
	if ic := s.instances.GetChannels(instanceID); ic != nil && ic.cli != nil {
		ic.cli.Connect()
		defer ic.cli.Disconnect()
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// GET /telemetry?instance_id=1&type=llm.done&since=...&limit=100
func (s *Server) handleQueryTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "instance_id required", http.StatusBadRequest)
		return
	}

	eventType := q.Get("type")
	threadID := q.Get("thread_id")
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))

	events, err := s.store.QueryTelemetry(instanceID, eventType, since, limit, threadID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []TelemetryEvent{}
	}
	writeJSON(w, events)
}

// GET /instances/:id/chat-history?limit=50
func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
	instanceID, err := atoi64(parts[0])
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetInstance(userID, instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.store.QueryChatHistory(instanceID, limit)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []ChatHistoryMessage{}
	}
	writeJSON(w, msgs)
}

// GET /telemetry/stats?instance_id=1&period=1h
func (s *Server) handleTelemetryStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "instance_id required", http.StatusBadRequest)
		return
	}

	period := q.Get("period")
	var since time.Time
	switch period {
	case "1h":
		since = time.Now().Add(-1 * time.Hour)
	case "24h":
		since = time.Now().Add(-24 * time.Hour)
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
	default:
		since = time.Now().Add(-24 * time.Hour)
	}

	stats, err := s.store.TelemetryStats(instanceID, since)
	if err != nil {
		http.Error(w, "stats failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, stats)
}

// TimelineBucket holds aggregated data for one time interval.
type TimelineBucket struct {
	Time     string             `json:"time"`
	LLMCalls int                `json:"llm_calls"`
	TokensIn int                `json:"tokens_in"`
	TokensOut int               `json:"tokens_out"`
	Cost     float64            `json:"cost"`
	ToolCalls int               `json:"tool_calls"`
	Errors   int                `json:"errors"`
	Threads  map[string]int     `json:"threads"` // thread_id → call count
}

func (s *Store) TelemetryTimeline(instanceID int64, since time.Time, bucketMinutes int) ([]TimelineBucket, error) {
	rows, err := s.db.Query(
		"SELECT type, thread_id, time, data FROM telemetry WHERE instance_id = ? AND time >= ? ORDER BY time",
		instanceID, since.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := map[string]*TimelineBucket{}

	for rows.Next() {
		var evType, threadID, timeStr, dataStr string
		rows.Scan(&evType, &threadID, &timeStr, &dataStr)

		t, _ := parseTime(timeStr)
		// Round down to bucket
		bucket := t.Truncate(time.Duration(bucketMinutes) * time.Minute).UTC().Format(time.RFC3339)

		b, ok := buckets[bucket]
		if !ok {
			b = &TimelineBucket{Time: bucket, Threads: map[string]int{}}
			buckets[bucket] = b
		}

		switch evType {
		case "llm.done":
			b.LLMCalls++
			b.Threads[threadID]++
			var d map[string]any
			if json.Unmarshal([]byte(dataStr), &d) == nil {
				if v, ok := d["tokens_in"].(float64); ok {
					b.TokensIn += int(v)
				}
				if v, ok := d["tokens_out"].(float64); ok {
					b.TokensOut += int(v)
				}
				if v, ok := d["cost_usd"].(float64); ok {
					b.Cost += v
				}
			}
		case "tool.call":
			b.ToolCalls++
		case "llm.error":
			b.Errors++
		}
	}

	// Sort by time
	var result []TimelineBucket
	for _, b := range buckets {
		result = append(result, *b)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Time < result[j].Time
	})
	return result, nil
}

// TelemetryStatsByProject aggregates llm.done / tool.call / *.error
// events across every instance in (userID, projectID) since `since`.
// Returns one InstanceStats per instance that has at least one event
// in the window, with zero-count instances omitted so the caller can
// do a clean "top N spenders" render. projectID="" means "all projects
// this user owns".
func (s *Store) TelemetryStatsByProject(userID int64, projectID string, since time.Time) ([]InstanceStats, error) {
	insts, err := s.ListInstances(userID, projectID)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return []InstanceStats{}, nil
	}
	// Build lookup + id list for the IN clause.
	byID := map[int64]*InstanceStats{}
	ids := make([]any, 0, len(insts))
	placeholders := make([]string, 0, len(insts))
	for i := range insts {
		byID[insts[i].ID] = &InstanceStats{
			InstanceID: insts[i].ID,
			Name:       insts[i].Name,
			Status:     insts[i].Status,
		}
		ids = append(ids, insts[i].ID)
		placeholders = append(placeholders, "?")
	}

	args := append([]any{}, ids...)
	args = append(args, since.UTC().Format(time.RFC3339))
	q := fmt.Sprintf(
		"SELECT instance_id, thread_id, type, data FROM telemetry WHERE instance_id IN (%s) AND time >= ?",
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Distinct (instance, thread) pairs for DistinctThreads — a
	// cheap proxy for "how many threads ran inside this instance in
	// the window". We count from llm.done rather than thread.spawn
	// because the spawn event is optional in older runs.
	threadSeen := map[int64]map[string]struct{}{}
	durationByInstance := map[int64]float64{}

	for rows.Next() {
		var instanceID int64
		var threadID, evType, dataStr string
		if err := rows.Scan(&instanceID, &threadID, &evType, &dataStr); err != nil {
			continue
		}
		agg, ok := byID[instanceID]
		if !ok {
			continue
		}
		switch evType {
		case "llm.done":
			agg.LLMCalls++
			if threadID != "" {
				seen, ok := threadSeen[instanceID]
				if !ok {
					seen = map[string]struct{}{}
					threadSeen[instanceID] = seen
				}
				seen[threadID] = struct{}{}
			}
			var d map[string]any
			if json.Unmarshal([]byte(dataStr), &d) == nil {
				if v, ok := d["tokens_in"].(float64); ok {
					agg.TokensIn += int(v)
				}
				if v, ok := d["tokens_out"].(float64); ok {
					agg.TokensOut += int(v)
				}
				if v, ok := d["tokens_cached"].(float64); ok {
					agg.TokensCached += int(v)
				}
				if v, ok := d["cost_usd"].(float64); ok {
					agg.Cost += v
				}
				if v, ok := d["duration_ms"].(float64); ok {
					durationByInstance[instanceID] += v
				}
			}
		case "tool.call":
			agg.ToolCalls++
		case "llm.error", "tool.error":
			agg.Errors++
		}
	}
	for id, seen := range threadSeen {
		if agg, ok := byID[id]; ok {
			agg.DistinctThreads = len(seen)
		}
	}
	for id, total := range durationByInstance {
		if agg, ok := byID[id]; ok && agg.LLMCalls > 0 {
			agg.AvgDurationMs = total / float64(agg.LLMCalls)
		}
	}

	// Materialize + drop instances with zero activity so the caller's
	// top-N render doesn't pad with empty rows. Sorted by cost desc.
	out := make([]InstanceStats, 0, len(byID))
	for _, agg := range byID {
		if agg.LLMCalls == 0 && agg.ToolCalls == 0 && agg.Errors == 0 {
			continue
		}
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out, nil
}

// TelemetryTimelineByProject buckets llm.done events by time and
// instance, so the dashboard can render a stacked chart of spend over
// time with one stack per instance. bucketMinutes controls the slice
// width. Instances with zero events in the window are omitted from
// every bucket to keep the payload tight.
func (s *Store) TelemetryTimelineByProject(userID int64, projectID string, since time.Time, bucketMinutes int) ([]ProjectTimelineBucket, error) {
	insts, err := s.ListInstances(userID, projectID)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return []ProjectTimelineBucket{}, nil
	}
	ids := make([]any, 0, len(insts))
	placeholders := make([]string, 0, len(insts))
	for _, inst := range insts {
		ids = append(ids, inst.ID)
		placeholders = append(placeholders, "?")
	}
	args := append([]any{}, ids...)
	args = append(args, since.UTC().Format(time.RFC3339))
	q := fmt.Sprintf(
		"SELECT instance_id, type, time, data FROM telemetry WHERE instance_id IN (%s) AND time >= ? ORDER BY time",
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := map[string]*ProjectTimelineBucket{}
	for rows.Next() {
		var instanceID int64
		var evType, timeStr, dataStr string
		if err := rows.Scan(&instanceID, &evType, &timeStr, &dataStr); err != nil {
			continue
		}
		t, _ := parseTime(timeStr)
		bucket := t.Truncate(time.Duration(bucketMinutes) * time.Minute).UTC().Format(time.RFC3339)
		b, ok := buckets[bucket]
		if !ok {
			b = &ProjectTimelineBucket{
				Time:            bucket,
				CostByInstance:  map[string]float64{},
				CallsByInstance: map[string]int{},
			}
			buckets[bucket] = b
		}
		instKey := strconv.FormatInt(instanceID, 10)
		switch evType {
		case "llm.done":
			b.LLMCalls++
			b.CallsByInstance[instKey]++
			var d map[string]any
			if json.Unmarshal([]byte(dataStr), &d) == nil {
				if v, ok := d["tokens_in"].(float64); ok {
					b.TokensIn += int(v)
				}
				if v, ok := d["tokens_out"].(float64); ok {
					b.TokensOut += int(v)
				}
				if v, ok := d["cost_usd"].(float64); ok {
					b.Cost += v
					b.CostByInstance[instKey] += v
				}
			}
		case "llm.error", "tool.error":
			b.Errors++
		}
	}
	out := make([]ProjectTimelineBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, nil
}

// GET /telemetry/project-stats?project_id=X&period=24h
func (s *Server) handleTelemetryProjectStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()
	projectID := q.Get("project_id")
	since := parsePeriod(q.Get("period"))
	stats, err := s.store.TelemetryStatsByProject(userID, projectID, since)
	if err != nil {
		http.Error(w, "stats failed", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []InstanceStats{}
	}
	writeJSON(w, stats)
}

// GET /telemetry/project-timeline?project_id=X&period=24h
func (s *Server) handleTelemetryProjectTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()
	projectID := q.Get("project_id")
	period := q.Get("period")
	since := parsePeriod(period)
	bucketMinutes := bucketWidthFor(period)
	timeline, err := s.store.TelemetryTimelineByProject(userID, projectID, since, bucketMinutes)
	if err != nil {
		http.Error(w, "timeline failed", http.StatusInternalServerError)
		return
	}
	if timeline == nil {
		timeline = []ProjectTimelineBucket{}
	}
	writeJSON(w, timeline)
}

func parsePeriod(p string) time.Time {
	switch p {
	case "1h":
		return time.Now().Add(-1 * time.Hour)
	case "7d":
		return time.Now().Add(-7 * 24 * time.Hour)
	case "30d":
		return time.Now().Add(-30 * 24 * time.Hour)
	default: // 24h
		return time.Now().Add(-24 * time.Hour)
	}
}

func bucketWidthFor(p string) int {
	switch p {
	case "1h":
		return 1
	case "7d":
		return 360 // 6 hours
	case "30d":
		return 1440 // 1 day
	default:
		return 60 // 24h → hourly
	}
}

// GET /telemetry/timeline?instance_id=1&period=24h
func (s *Server) handleTelemetryTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "instance_id required", http.StatusBadRequest)
		return
	}

	period := q.Get("period")
	var since time.Time
	var bucketMinutes int
	switch period {
	case "1h":
		since = time.Now().Add(-1 * time.Hour)
		bucketMinutes = 1
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
		bucketMinutes = 360 // 6 hours
	default: // 24h
		since = time.Now().Add(-24 * time.Hour)
		bucketMinutes = 60
	}

	timeline, err := s.store.TelemetryTimeline(instanceID, since, bucketMinutes)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if timeline == nil {
		timeline = []TimelineBucket{}
	}
	writeJSON(w, timeline)
}

// DELETE /telemetry — wipe all telemetry data
func (s *Server) handleWipeTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	deleted, err := s.store.WipeTelemetry()
	if err != nil {
		http.Error(w, "wipe failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"deleted": deleted})
}
