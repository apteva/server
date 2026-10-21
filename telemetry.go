package main

import (
	"context"
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
	"unicode/utf8"
)

// TelemetryBroadcaster fans out events to SSE subscribers.
type TelemetryBroadcaster struct {
	replayEpoch string
	mu          sync.Mutex
	subscribers map[int64][]chan TelemetryEvent // instanceID → channels
	nextID      int64
	nextSeq     uint64
	replay      []TelemetryEvent
	replayHead  int
	replaySize  int
	dropped     uint64
}

const telemetryReplayCap = 8192

func NewTelemetryBroadcaster() *TelemetryBroadcaster {
	return &TelemetryBroadcaster{
		subscribers: make(map[int64][]chan TelemetryEvent),
		replayEpoch: generateID(),
		replay:      make([]TelemetryEvent, telemetryReplayCap),
	}
}

// Subscribe returns a channel that receives events for the given instance.
func (b *TelemetryBroadcaster) Subscribe(instanceID int64) chan TelemetryEvent {
	ch, _ := b.SubscribeCursor(instanceID)
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

// SubscribeAll returns a channel that receives events for all agents.
// Used by the console logger to render all activity, and by the dashboard's
// Instances list page to render a per-row live activity strip without
// opening N concurrent SSE connections.
func (b *TelemetryBroadcaster) SubscribeAll() chan TelemetryEvent {
	ch, _ := b.SubscribeAllCursor()
	return ch
}

// SubscribeAllCursor atomically attaches to the same subscription registry and
// returns the live starting sequence, before any eligibility/readiness work.
func (b *TelemetryBroadcaster) SubscribeAllCursor() (chan TelemetryEvent, uint64) {
	return b.SubscribeCursor(-1)
}
func (b *TelemetryBroadcaster) SubscribeCursor(instanceID int64) (chan TelemetryEvent, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	capacity := 100
	if instanceID == -1 {
		capacity = 200
	}
	ch := make(chan TelemetryEvent, capacity)
	b.subscribers[instanceID] = append(b.subscribers[instanceID], ch)
	return ch, b.nextSeq
}
func (b *TelemetryBroadcaster) UnsubscribeAll(ch chan TelemetryEvent) {
	b.Unsubscribe(-1, ch)
}

// Broadcast sends events to all subscribers for the given instance,
// plus any "all" subscribers (instanceID -1).
func (b *TelemetryBroadcaster) Broadcast(events []TelemetryEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, input := range events {
		b.nextSeq++
		ev := input
		ev.Seq = b.nextSeq
		if ev.ID == "" {
			ev.ID = fmt.Sprintf("live:%s:%d", b.replayEpoch, ev.Seq)
		}
		b.replay[b.replayHead] = ev
		b.replayHead = (b.replayHead + 1) % telemetryReplayCap
		if b.replaySize < telemetryReplayCap {
			b.replaySize++
		}
		for _, ch := range b.subscribers[ev.AgentID] {
			select {
			case ch <- ev:
			default:
				b.recordDrop(ev, "agent")
			}
		}
		// Fan out to "all" subscribers
		for _, ch := range b.subscribers[-1] {
			select {
			case ch <- ev:
			default:
				b.recordDrop(ev, "all")
			}
		}
	}
}

func (b *TelemetryBroadcaster) recordDrop(ev TelemetryEvent, lane string) {
	b.dropped++
	if b.dropped%100 == 1 {
		log.Printf("[TELEMETRY] subscriber buffer full lane=%s agent=%d seq=%d total_drops=%d (replay ring will reconcile)",
			lane, ev.AgentID, ev.Seq, b.dropped)
	}
}

// ReplaySince returns an ordered copy of recent events newer than since.
// reset is true when the cursor belongs to an older server process or fell
// behind the bounded ring; callers should tell clients that only the retained
// suffix can be replayed. since=0 intentionally means "live from subscribe".
func (b *TelemetryBroadcaster) ReplaySince(since uint64) ([]TelemetryEvent, bool) {
	if since == 0 {
		return nil, false
	}
	events, _, _, reason := b.replayTelemetry(b.replayEpoch, since)
	return events, reason != ""
}

// TelemetryEvent is the unified event format.
type TelemetryEvent struct {
	ID       string          `json:"id"`
	AgentID  int64           `json:"instance_id"`
	ThreadID string          `json:"thread_id"`
	Type     string          `json:"type"`
	Time     time.Time       `json:"time"`
	Data     json.RawMessage `json:"data"`
	Seq      uint64          `json:"seq,omitempty"`
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
	AgentID         int64   `json:"instance_id"`
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	LLMCalls        int     `json:"llm_calls"`
	TokensIn        int     `json:"tokens_in"`
	TokensOut       int     `json:"tokens_out"`
	TokensCached    int     `json:"tokens_cached"`
	Cost            float64 `json:"cost"`
	Errors          int     `json:"errors"`
	ToolCalls       int     `json:"tool_calls"`
	AvgDurationMs   float64 `json:"avg_duration_ms"`
	DistinctThreads int     `json:"distinct_threads"`
}

// ProjectTimelineBucket: one time slice of project-wide spend with a
// per-instance breakdown. CostByInstance is keyed by instance id
// (stringified — JSON object keys must be strings).
type ProjectTimelineBucket struct {
	Time            string             `json:"time"`
	Cost            float64            `json:"cost"`
	TokensIn        int                `json:"tokens_in"`
	TokensOut       int                `json:"tokens_out"`
	LLMCalls        int                `json:"llm_calls"`
	Errors          int                `json:"errors"`
	CostByInstance  map[string]float64 `json:"cost_by_instance"`
	CallsByInstance map[string]int     `json:"calls_by_instance"`
}

func generateID() string {
	// Simple time-prefixed random ID
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
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
		"INSERT OR IGNORE INTO telemetry (id, agent_id, thread_id, type, time, data) VALUES (?, ?, ?, ?, ?, ?)",
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
		timeStr := e.Time.UTC().Format(time.RFC3339Nano)
		dataStr := "{}"
		if e.Data != nil {
			dataStr = string(e.Data)
		}
		if _, err := stmt.Exec(e.ID, e.AgentID, e.ThreadID, e.Type, timeStr, dataStr); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) QueryTelemetry(instanceID int64, eventType string, since time.Time, limit int, threadID ...string) ([]TelemetryEvent, error) {
	return s.QueryTelemetryContext(context.Background(), instanceID, eventType, since, limit, threadID...)
}

func (s *Store) DeleteTelemetryAfter(instanceID int64, cutoff time.Time) error {
	if instanceID == 0 || cutoff.IsZero() {
		return nil
	}
	_, err := s.db.Exec(
		"DELETE FROM telemetry WHERE agent_id=? AND time>?",
		instanceID, cutoff.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// ChatHistoryMessage is a reconstructed chat message from telemetry.
type ChatHistoryMessage struct {
	ID   string `json:"id"`
	Role string `json:"role"` // "user", "agent", "tool", "status"
	Text string `json:"text"`
	Time string `json:"time"`
	// Tool fields (only for role=tool)
	ToolName       string `json:"tool_name,omitempty"`
	ToolDone       bool   `json:"tool_done,omitempty"`
	ToolDurationMs int64  `json:"tool_duration_ms,omitempty"`
	ToolSuccess    bool   `json:"tool_success,omitempty"`
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
		WHERE agent_id = ?
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
		"channels_respond": true, "channels_send": true, "channels_status": true,
		"channels_publish": true, "channels_set_status": true,
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
			lowerName := strings.ToLower(strings.TrimSpace(name))
			if strings.HasSuffix(lowerName, "_set_status") ||
				strings.HasSuffix(lowerName, "_publish") ||
				strings.HasSuffix(lowerName, "_notify") ||
				strings.HasSuffix(lowerName, "_list_channels") {
				continue
			}

			if name == "channels_respond" || name == "channels_send" || name == "channels_status" {
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

func jsonFloat(data map[string]any, key string) float64 {
	value, _ := data[key].(float64)
	return value
}

func jsonInt(data map[string]any, key string) int {
	return int(jsonFloat(data, key))
}

func (s *Store) CleanOldTelemetry(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	var total int64
	for {
		result, err := s.db.Exec(`DELETE FROM telemetry WHERE id IN (
 SELECT id FROM telemetry WHERE time < ? AND NOT EXISTS
 (SELECT 1 FROM agent_event_deliveries d WHERE d.telemetry_id=telemetry.id AND delivered_at IS NULL) LIMIT 1000)`, cutoff)
		if err != nil {
			return total, err
		}
		n, err := result.RowsAffected()
		total += n
		if err != nil || n < 1000 {
			return total, err
		}
	}

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

	// Require the agent secret. Phase 2: accept either X-Agent-Secret
	// (canonical) or X-Instance-Secret (legacy alias still written by
	// older apteva-core builds). One core release switches to writing
	// X-Agent-Secret first and Phase 4 drops the legacy reader.
	if s.instanceSecret != "" {
		got := r.Header.Get("X-Agent-Secret")
		if got == "" {
			got = r.Header.Get("X-Instance-Secret")
		}
		if got != s.instanceSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var events []TelemetryEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&events); err != nil {
		log.Printf("[TELEMETRY] POST /telemetry: invalid JSON: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if len(events) == 0 {
		writeJSON(w, map[string]int{"inserted": 0})
		return
	}
	if len(events) > 10000 {
		http.Error(w, "telemetry batch too large", http.StatusRequestEntityTooLarge)
		return
	}
	normalizeAgentLifecycleTelemetryIDs(events)
	sanitizeStoredToolResultTelemetry(events)

	// Enrich llm.done events with server-side cost calculation.
	// Pricing data lives here, not in core — core emits raw token
	// counts + model, and this pass layers in cost_usd on persist.
	s.enrichCostInPlace(events)
	var err error
	events, err = s.applyTelemetryRestores(events)
	if err != nil {
		http.Error(w, "restore prune failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.store.InsertTelemetry(events); err != nil {
		http.Error(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// React to directive changes — update DB so dashboard sees it immediately
	for _, ev := range events {
		if ev.Type == "directive.evolved" && ev.AgentID > 0 {
			var data struct {
				New string `json:"new"`
			}
			if json.Unmarshal(ev.Data, &data) == nil && data.New != "" {
				if _, err := s.store.db.Exec("UPDATE agents SET directive=? WHERE id=?", data.New, ev.AgentID); err == nil {
					// Do not make Core's telemetry ingestion wait on re-entrant
					// per-thread HTTP updates. Persistence is complete; each thread's
					// next delivery also performs the same drift check defensively.
					go s.refreshChannelChatConversationDirectives(ev.AgentID)
				}
			}
		}
	}

	// No broadcast here — /telemetry/live handles real-time broadcast.
	// This endpoint is for DB persistence only.

	writeJSON(w, map[string]int{"inserted": len(events)})
}

const storedToolResultPreviewBytes = 1000

// sanitizeStoredToolResultTelemetry is a defensive server-side bound. New
// cores already send a preview plus exact size metadata, but this also keeps
// inline results from older cores from persisting an unbounded payload.
func sanitizeStoredToolResultTelemetry(events []TelemetryEvent) {
	for i := range events {
		if events[i].Type != "tool.result" || len(events[i].Data) == 0 {
			continue
		}
		var data map[string]any
		if json.Unmarshal(events[i].Data, &data) != nil {
			continue
		}
		result, ok := data["result"].(string)
		if !ok {
			continue
		}
		alreadyTruncated, _ := data["result_truncated"].(bool)
		if len(result) > storedToolResultPreviewBytes && !(alreadyTruncated && len(result) <= storedToolResultPreviewBytes+3) {
			end := storedToolResultPreviewBytes
			for end > 0 && !utf8.ValidString(result[:end]) {
				end--
			}
			result = result[:end] + "..."
			data["result"] = result
			alreadyTruncated = true
		}
		data["result_preview_bytes"] = len(result)
		data["result_truncated"] = alreadyTruncated
		if raw, err := json.Marshal(data); err == nil {
			events[i].Data = raw
		}
	}
}

func (s *Server) applyTelemetryRestores(events []TelemetryEvent) ([]TelemetryEvent, error) {
	if len(events) == 0 {
		return events, nil
	}
	keep := make([]bool, len(events))
	for i := range keep {
		keep[i] = true
	}
	for i, ev := range events {
		if ev.Type != "execution.restored" {
			continue
		}
		var data struct {
			CheckpointTime time.Time `json:"checkpoint_time"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || data.CheckpointTime.IsZero() {
			continue
		}
		if err := s.store.DeleteTelemetryAfter(ev.AgentID, data.CheckpointTime); err != nil {
			return nil, err
		}
		for j := 0; j < i; j++ {
			if events[j].AgentID == ev.AgentID && events[j].Time.After(data.CheckpointTime) {
				keep[j] = false
			}
		}
	}
	out := events[:0]
	for i, ev := range events {
		if keep[i] {
			out = append(out, ev)
		}
	}
	return out, nil
}

// POST /telemetry/live — broadcast-only (not stored), for streaming chunks
func (s *Server) handleLiveTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Require the agent secret — accept X-Agent-Secret (canonical) or
	// X-Instance-Secret (legacy). Mismatches stay silent; add a log
	// here scoped to the failing path during debugging if needed.
	if s.instanceSecret != "" {
		got := r.Header.Get("X-Agent-Secret")
		if got == "" {
			got = r.Header.Get("X-Instance-Secret")
		}
		if got != s.instanceSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var events []TelemetryEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&events); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(events) > 5000 {
		http.Error(w, "telemetry batch too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Enrich live events too — the dashboard's per-instance streaming
	// cost + context bars all read cost_usd. If we skip this, the
	// streaming path shows $0.00 until the persistence ingest catches
	// up and the caller refetches.
	s.enrichCostInPlace(events)
	s.latestLLMDone.remember(events)

	s.broadcaster.Broadcast(events)

	// Channelchat streaming tap. Set by startApps when the channelchat
	// app is loaded. Skipped when CHANNELCHAT_STREAMING=0 (the hook
	// itself isn't installed in that mode). Runs sync because the
	// streamer is non-blocking — it parses + publishes to the per-chat
	// hub, which has drop-on-full semantics.
	if s.liveTelemetryHook != nil {
		s.liveTelemetryHook(events)
	}

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

// The dashboard/API endpoint and app callback share one telemetry SSE engine.
func (s *Server) handleTelemetryStream(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") != "delivery" && r.URL.Query().Get("all") != "1" && r.URL.Query().Get("agent_id") == "" && r.URL.Query().Get("instance_id") == "" {
		http.Error(w, "agent_id required (or ?all=1 for all-instances stream)", http.StatusBadRequest)
		return
	}
	s.handleTelemetryRead(w, r, 0, "/stream")
}

// GET /telemetry?agent_id=1&type=llm.done&since=...&limit=100
func (s *Server) handleQueryTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "page" {
		s.handleTelemetryRead(w, r, 0, "/history")
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("agent_id"), 10, 64)
	if instanceID == 0 {
		instanceID, _ = strconv.ParseInt(q.Get("instance_id"), 10, 64)
	}
	if instanceID == 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireAgentAccess(w, r, instanceID, ProjectViewer); !ok {
		return
	}

	eventType := q.Get("type")
	threadID := q.Get("thread_id")
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))

	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	events, err := s.queryPublicTelemetry(r.Context(), instanceID, eventType, since, limit, threadID)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
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
	if _, err := s.store.GetAgent(userID, instanceID); err != nil {
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

// GET /telemetry/stats?agent_id=1&period=1h
func (s *Server) handleTelemetryStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireAgentAccess(w, r, instanceID, ProjectViewer); !ok {
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

	stats, err := s.store.TelemetryStatsContext(r.Context(), instanceID, since)
	if err != nil {
		http.Error(w, "stats failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, stats)
}

// TimelineBucket holds aggregated data for one time interval.
type TimelineBucket struct {
	Time      string         `json:"time"`
	LLMCalls  int            `json:"llm_calls"`
	TokensIn  int            `json:"tokens_in"`
	TokensOut int            `json:"tokens_out"`
	Cost      float64        `json:"cost"`
	ToolCalls int            `json:"tool_calls"`
	Errors    int            `json:"errors"`
	Threads   map[string]int `json:"threads"` // thread_id → call count
}

func (s *Server) handleTelemetryProjectStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()
	projectID := q.Get("project_id")
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	since := parsePeriod(q.Get("period"))
	stats, err := s.store.TelemetryStatsByProjectContext(r.Context(), userID, projectID, since)
	if err != nil {
		http.Error(w, "stats failed", http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = []InstanceStats{}
	}
	writeJSON(w, stats)
}

// ProjectToolStat is one row of the project-wide tool breakdown.
// Aggregated from tool.call + tool.result events; success/failure
// split is derived from tool.result.is_error so it survives core
// rewrites that change the error-event names.
type ProjectToolStat struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Errors int    `json:"errors"`
	Agents int    `json:"agents"` // distinct instances that called it
}

// GET /telemetry/project-tools?project_id=X&period=24h
//
// Top-N tool names by call count across every instance in the
// project, with success/error split. Used by the dashboard's
// ToolsUsageCard to surface "what's the system doing right now"
// and which tools are failing.
func (s *Server) handleTelemetryProjectTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()
	projectID := q.Get("project_id")
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	since := parsePeriod(q.Get("period"))

	insts, err := s.store.listAgentsForTelemetry(userID, projectID)
	if err != nil {
		http.Error(w, "list instances failed", http.StatusInternalServerError)
		return
	}
	if len(insts) == 0 {
		writeJSON(w, []ProjectToolStat{})
		return
	}
	ids := make([]any, 0, len(insts))
	placeholders := make([]string, 0, len(insts))
	for i := range insts {
		ids = append(ids, insts[i].ID)
		placeholders = append(placeholders, "?")
	}
	args := append([]any{}, ids...)
	args = append(args, since.UTC().Format(time.RFC3339))

	rows, err := s.store.db.Query(
		fmt.Sprintf(
			"SELECT agent_id, type, data FROM telemetry "+
				"WHERE agent_id IN (%s) AND time >= ? "+
				"AND type IN ('tool.call','tool.result')",
			strings.Join(placeholders, ","),
		),
		args...,
	)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type agg struct {
		calls  int
		errors int
		agents map[int64]struct{}
	}
	byTool := map[string]*agg{}
	bump := func(name string, instanceID int64) *agg {
		a, ok := byTool[name]
		if !ok {
			a = &agg{agents: map[int64]struct{}{}}
			byTool[name] = a
		}
		a.agents[instanceID] = struct{}{}
		return a
	}
	for rows.Next() {
		var instanceID int64
		var evType, dataStr string
		if err := rows.Scan(&instanceID, &evType, &dataStr); err != nil {
			continue
		}
		var d map[string]any
		if json.Unmarshal([]byte(dataStr), &d) != nil {
			continue
		}
		name, _ := d["name"].(string)
		if name == "" {
			name, _ = d["tool"].(string)
		}
		if name == "" {
			continue
		}
		a := bump(name, instanceID)
		switch evType {
		case "tool.call":
			a.calls++
		case "tool.result":
			if isErr, _ := d["is_error"].(bool); isErr {
				a.errors++
			}
		}
	}

	out := make([]ProjectToolStat, 0, len(byTool))
	for name, a := range byTool {
		out = append(out, ProjectToolStat{
			Name:   name,
			Calls:  a.calls,
			Errors: a.errors,
			Agents: len(a.agents),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, out)
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
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	period := q.Get("period")
	since := parsePeriod(period)
	bucketMinutes := bucketWidthFor(period)
	timeline, err := s.store.TelemetryTimelineByProjectContext(r.Context(), userID, projectID, since, bucketMinutes)
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

// GET /telemetry/timeline?agent_id=1&period=24h
func (s *Server) handleTelemetryTimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	instanceID, _ := strconv.ParseInt(q.Get("instance_id"), 10, 64)
	if instanceID == 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireAgentAccess(w, r, instanceID, ProjectViewer); !ok {
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

	timeline, err := s.store.TelemetryTimelineContext(r.Context(), instanceID, since, bucketMinutes)
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
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	deleted, err := s.store.WipeTelemetry()
	if err != nil {
		http.Error(w, "wipe failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"deleted": deleted})
}
