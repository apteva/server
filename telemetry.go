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
// Used by the console logger to render all activity.
func (b *TelemetryBroadcaster) SubscribeAll() chan TelemetryEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan TelemetryEvent, 200)
	// Use instanceID -1 as the "all" sentinel
	b.subscribers[-1] = append(b.subscribers[-1], ch)
	return ch
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

	// Enrich llm.done events with server-side cost calculation
	for i, ev := range events {
		if ev.Type == "llm.done" && ev.Data != nil {
			var data map[string]any
			if json.Unmarshal(ev.Data, &data) == nil {
				model, _ := data["model"].(string)
				tokIn, _ := data["tokens_in"].(float64)
				tokOut, _ := data["tokens_out"].(float64)
				if model != "" && (tokIn > 0 || tokOut > 0) {
					// Determine provider type from instance
					providerType := ""
					if ev.InstanceID > 0 {
						if inst, err := s.store.GetInstanceByID(ev.InstanceID); err == nil {
							pi := s.GetProviderInfo(inst.UserID)
							providerType = pi.Type
						}
					}
					if providerType != "" {
						inputCost, outputCost := GetModelPricing(providerType, model)
						if inputCost > 0 || outputCost > 0 {
							cost := (tokIn * inputCost / 1_000_000) + (tokOut * outputCost / 1_000_000)
							data["cost_usd"] = cost
							events[i].Data, _ = json.Marshal(data)
						}
					}
				}
			}
		}
	}

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

	// Require instance secret
	if s.instanceSecret != "" {
		if r.Header.Get("X-Instance-Secret") != s.instanceSecret {
			log.Printf("[TELEMETRY] live: unauthorized — header=%q expected=%q (first8)", r.Header.Get("X-Instance-Secret")[:min(8, len(r.Header.Get("X-Instance-Secret")))], s.instanceSecret[:min(8, len(s.instanceSecret))])
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var events []TelemetryEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		log.Printf("[TELEMETRY] live: bad json: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	log.Printf("[TELEMETRY] live: received %d events: %v", len(events), types)

	s.broadcaster.Broadcast(events)
	writeJSON(w, map[string]int{"broadcast": len(events)})
}

// GET /telemetry/stream?instance_id=1 — SSE stream of live events
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

	instanceID, _ := strconv.ParseInt(r.URL.Query().Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "instance_id required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.broadcaster.Subscribe(instanceID)
	defer s.broadcaster.Unsubscribe(instanceID, ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
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
