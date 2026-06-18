package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultPollIntervalSeconds = 300
	minPollIntervalSeconds     = 60
	maxPollBackoff             = time.Hour
)

type storedWebhookPollConfig struct {
	EventName          string         `json:"event_name"`
	AppSlug            string         `json:"app_slug"`
	Tool               string         `json:"tool"`
	IntervalSeconds    int            `json:"interval_seconds"`
	MinIntervalSeconds int            `json:"min_interval_seconds"`
	Input              map[string]any `json:"input,omitempty"`
	ItemsPath          string         `json:"items_path,omitempty"`
	IDFields           []string       `json:"id_fields"`
	TimestampField     string         `json:"timestamp_field,omitempty"`
	Mode               string         `json:"mode,omitempty"`
	EmitInitial        bool           `json:"emit_initial,omitempty"`
	MaxSeen            int            `json:"max_seen,omitempty"`
}

type pollState struct {
	Seen          map[string]pollSeen `json:"seen,omitempty"`
	Seeded        bool                `json:"seeded,omitempty"`
	LastSuccessAt string              `json:"last_success_at,omitempty"`
}

type pollSeen struct {
	Hash      string `json:"hash,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

type PollingSubscriptionDispatcher struct {
	server  *Server
	mu      sync.Mutex
	running map[string]struct{}
	wake    chan struct{}
}

func NewPollingSubscriptionDispatcher(s *Server) *PollingSubscriptionDispatcher {
	return &PollingSubscriptionDispatcher{
		server:  s,
		running: map[string]struct{}{},
		wake:    make(chan struct{}, 1),
	}
}

func (d *PollingSubscriptionDispatcher) Start() {
	go d.loop()
}

func (d *PollingSubscriptionDispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *PollingSubscriptionDispatcher) loop() {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			d.tick()
			timer.Reset(5 * time.Second)
		case <-d.wake:
			d.tick()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(5 * time.Second)
		}
	}
}

func (d *PollingSubscriptionDispatcher) tick() {
	subs, err := d.server.store.ListDuePollSubscriptions(time.Now().UTC(), 20)
	if err != nil {
		log.Printf("[SUB-POLL] list due: %v", err)
		return
	}
	for _, sub := range subs {
		if !d.markRunning(sub.ID) {
			continue
		}
		go func(sub *Subscription) {
			defer d.clearRunning(sub.ID)
			if err := d.run(sub); err != nil {
				log.Printf("[SUB-POLL] sub=%s error: %v", sub.ID, err)
			}
		}(sub)
	}
}

func (d *PollingSubscriptionDispatcher) markRunning(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.running[id]; ok {
		return false
	}
	d.running[id] = struct{}{}
	return true
}

func (d *PollingSubscriptionDispatcher) clearRunning(id string) {
	d.mu.Lock()
	delete(d.running, id)
	d.mu.Unlock()
}

func (d *PollingSubscriptionDispatcher) run(sub *Subscription) error {
	now := time.Now().UTC()
	var cfg storedWebhookPollConfig
	if err := json.Unmarshal([]byte(sub.PollConfigJSON), &cfg); err != nil {
		return d.recordFailure(sub, now, fmt.Errorf("invalid poll config: %w", err), defaultPollIntervalSeconds)
	}
	interval := normalizePollInterval(cfg.IntervalSeconds, cfg.MinIntervalSeconds)
	if sub.AgentID == 0 {
		return d.recordFailure(sub, now, fmt.Errorf("no target agent configured"), interval)
	}
	if sub.ConnectionID == 0 {
		return d.recordFailure(sub, now, fmt.Errorf("poll subscription missing connection_id"), interval)
	}

	conn, encCreds, err := d.server.store.GetConnection(sub.UserID, sub.ConnectionID)
	if err != nil || conn == nil {
		return d.recordFailure(sub, now, fmt.Errorf("connection not found: %w", err), interval)
	}
	app := d.server.catalog.Get(conn.AppSlug)
	if app == nil {
		return d.recordFailure(sub, now, fmt.Errorf("app %q not found", conn.AppSlug), interval)
	}
	tool := findAppTool(app, cfg.Tool)
	if tool == nil {
		return d.recordFailure(sub, now, fmt.Errorf("tool %q not found on %s", cfg.Tool, app.Slug), interval)
	}

	plain, err := Decrypt(d.server.secret, encCreds)
	if err != nil {
		return d.recordFailure(sub, now, fmt.Errorf("decrypt credentials: %w", err), interval)
	}
	var credentials map[string]string
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return d.recordFailure(sub, now, fmt.Errorf("decode credentials: %w", err), interval)
	}
	input := cloneAnyMap(cfg.Input)
	ctx, err := d.server.resolveConnectionContext(sub.UserID, app, credentials, input)
	if err != nil {
		d.server.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "subscription-poller", tool.Name, input, nil, err))
		return d.recordFailure(sub, now, fmt.Errorf("resolve connection: %w", err), interval)
	}
	persistTargetID := conn.ID
	if ctx.MasterConnID != 0 {
		persistTargetID = ctx.MasterConnID
	}
	persist := func(updated map[string]string) error {
		blob, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(d.server.secret, string(blob))
		if err != nil {
			return err
		}
		return d.server.store.UpdateConnectionCredentials(persistTargetID, enc)
	}
	result, err := executeIntegrationToolWithRefresh(ctx.App, tool, ctx.Credentials, ctx.Input, "", persist)
	if err != nil {
		d.server.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "subscription-poller", tool.Name, input, nil, err))
		return d.recordFailure(sub, now, fmt.Errorf("execute %s: %w", cfg.Tool, err), interval)
	}
	d.server.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "subscription-poller", tool.Name, input, result, nil))
	if result == nil || !result.Success {
		status := 0
		if result != nil {
			status = result.Status
		}
		return d.recordFailure(sub, now, fmt.Errorf("execute %s returned status %d", cfg.Tool, status), interval)
	}

	items := extractPollItems(result.Data, cfg.ItemsPath)
	state := pollState{Seen: map[string]pollSeen{}}
	if strings.TrimSpace(sub.PollStateJSON) != "" {
		_ = json.Unmarshal([]byte(sub.PollStateJSON), &state)
	}
	if state.Seen == nil {
		state.Seen = map[string]pollSeen{}
	}

	var deliveries []map[string]any
	for _, item := range items {
		itemID := pollItemID(item, cfg.IDFields)
		if itemID == "" {
			continue
		}
		hash := hashPollItem(item)
		timestamp := stringFromAny(pollPath(item, cfg.TimestampField))
		prev, exists := state.Seen[itemID]
		changed := exists && prev.Hash != "" && prev.Hash != hash
		shouldEmit := false
		if state.Seeded || cfg.EmitInitial {
			if !exists {
				shouldEmit = true
			} else if cfg.Mode == "updated_items" && changed {
				shouldEmit = true
			}
		}
		state.Seen[itemID] = pollSeen{Hash: hash, Timestamp: timestamp, LastSeen: formatPollTime(now)}
		if shouldEmit {
			eventID := sub.ID + ":" + cfg.EventName + ":" + itemID + ":" + hash
			deliveries = append(deliveries, map[string]any{
				"event":         cfg.EventName,
				"event_id":      eventID,
				"app":           app.Slug,
				"connection_id": sub.ConnectionID,
				"subscription":  sub.ID,
				"polled_at":     formatPollTime(now),
				"item":          item,
			})
		}
	}
	state.Seeded = true
	state.LastSuccessAt = formatPollTime(now)
	prunePollState(&state, cfg.MaxSeen)

	for _, payload := range deliveries {
		if err := d.server.deliverSubscriptionPayload(sub, "webhook:"+app.Slug+":"+cfg.EventName, payload); err != nil {
			return d.recordFailure(sub, now, fmt.Errorf("deliver event: %w", err), interval)
		}
	}

	stateJSON, _ := json.Marshal(state)
	nextRun := now.Add(pollIntervalWithJitter(interval, sub.ID))
	if err := d.server.store.UpdatePollSubscriptionSuccess(sub.ID, string(stateJSON), now, nextRun); err != nil {
		return err
	}
	return nil
}

func (d *PollingSubscriptionDispatcher) recordFailure(sub *Subscription, now time.Time, cause error, intervalSeconds int) error {
	failures := sub.FailureCount + 1
	backoff := time.Duration(intervalSeconds) * time.Second
	for i := 1; i < failures && backoff < maxPollBackoff; i++ {
		backoff *= 2
	}
	if backoff > maxPollBackoff {
		backoff = maxPollBackoff
	}
	_ = d.server.store.UpdatePollSubscriptionFailure(sub.ID, cause.Error(), now, now.Add(backoff), failures)
	return cause
}

func (s *Server) deliverSubscriptionPayload(sub *Subscription, prefix string, payload any) error {
	if sub == nil {
		return fmt.Errorf("subscription is nil")
	}
	if !sub.Enabled {
		return fmt.Errorf("subscription disabled")
	}
	if sub.AgentID == 0 {
		return fmt.Errorf("no target agent configured")
	}
	inst, err := s.store.GetAgent(sub.UserID, sub.AgentID)
	if err != nil {
		return fmt.Errorf("agent %d not found: %w", sub.AgentID, err)
	}
	port := s.agents.GetPort(inst.ID)
	if port == 0 {
		return fmt.Errorf("agent %d not running", inst.ID)
	}
	payloadBytes, _ := json.Marshal(payload)
	payloadStr := string(payloadBytes)
	if len(payloadStr) > 2000 {
		payloadStr = payloadStr[:2000] + "...[truncated]"
	}
	eventPayload := map[string]string{"message": fmt.Sprintf("[%s] %s", prefix, payloadStr)}
	if sub.ThreadID != "" {
		eventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(eventPayload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", port), strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if ck := s.agents.GetCoreAPIKey(inst.ID); ck != "" {
		req.Header.Set("Authorization", "Bearer "+ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("core rejected %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func buildStoredPollConfig(app *AppTemplate, events []string, intervalSeconds int, pollInput map[string]any) (*storedWebhookPollConfig, *AppWebhookEvent, error) {
	event := selectedPollEvent(app, events)
	if event == nil {
		return nil, nil, nil
	}
	if event.Poll == nil {
		return nil, nil, nil
	}
	if len(events) > 1 {
		return nil, event, fmt.Errorf("poll-backed subscriptions support one event at a time")
	}
	if findAppTool(app, event.Poll.Tool) == nil {
		return nil, event, fmt.Errorf("poll tool %q not found on %s", event.Poll.Tool, app.Slug)
	}
	input := cloneAnyMap(event.Poll.Input)
	for k, v := range pollInput {
		input[k] = v
	}
	minInterval := event.Poll.MinIntervalSeconds
	if minInterval <= 0 {
		minInterval = minPollIntervalSeconds
	}
	interval := intervalSeconds
	if interval <= 0 {
		interval = event.Poll.DefaultIntervalSeconds
	}
	interval = normalizePollInterval(interval, minInterval)
	cfg := &storedWebhookPollConfig{
		EventName:          event.Name,
		AppSlug:            app.Slug,
		Tool:               event.Poll.Tool,
		IntervalSeconds:    interval,
		MinIntervalSeconds: minInterval,
		Input:              input,
		ItemsPath:          event.Poll.ItemsPath,
		IDFields:           append([]string(nil), event.Poll.IDFields...),
		TimestampField:     event.Poll.TimestampField,
		Mode:               event.Poll.Mode,
		EmitInitial:        event.Poll.EmitInitial,
		MaxSeen:            event.Poll.MaxSeen,
	}
	if cfg.Mode == "" {
		cfg.Mode = "new_items"
	}
	if cfg.MaxSeen <= 0 {
		cfg.MaxSeen = 1000
	}
	if len(cfg.IDFields) == 0 {
		return nil, event, fmt.Errorf("poll event %q missing id_fields", event.Name)
	}
	return cfg, event, nil
}

func selectedPollEvent(app *AppTemplate, events []string) *AppWebhookEvent {
	if app == nil || app.Webhooks == nil {
		return nil
	}
	selected := map[string]bool{}
	for _, event := range events {
		selected[event] = true
	}
	var onlyPoll *AppWebhookEvent
	for i := range app.Webhooks.Events {
		event := &app.Webhooks.Events[i]
		isPoll := event.Delivery == "poll" || event.Poll != nil
		if !isPoll {
			continue
		}
		if len(events) == 0 {
			if onlyPoll != nil {
				return nil
			}
			onlyPoll = event
			continue
		}
		if selected[event.Name] {
			return event
		}
	}
	if len(events) == 0 {
		return onlyPoll
	}
	return nil
}

func findAppTool(app *AppTemplate, name string) *AppToolDef {
	if app == nil {
		return nil
	}
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app.Tools[i]
		}
	}
	return nil
}

func extractPollItems(data any, path string) []any {
	raw := data
	if path != "" {
		raw = pollPath(data, path)
	}
	switch v := raw.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"items", "data", "calls", "call_list", "records", "results"} {
			if arr, ok := v[key].([]any); ok {
				return arr
			}
		}
		return []any{v}
	default:
		return nil
	}
}

func pollItemID(item any, fields []string) string {
	var parts []string
	for _, field := range fields {
		if v := stringFromAny(pollPath(item, field)); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ":")
}

func pollPath(data any, path string) any {
	if path == "" {
		return data
	}
	current := data
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

func hashPollItem(item any) string {
	b, _ := json.Marshal(item)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizePollInterval(interval, minInterval int) int {
	if minInterval <= 0 {
		minInterval = minPollIntervalSeconds
	}
	if interval <= 0 {
		interval = defaultPollIntervalSeconds
	}
	if interval < minInterval {
		return minInterval
	}
	return interval
}

func pollIntervalWithJitter(intervalSeconds int, key string) time.Duration {
	base := time.Duration(intervalSeconds) * time.Second
	if intervalSeconds <= 10 {
		return base
	}
	sum := sha256.Sum256([]byte(key))
	jitterMax := intervalSeconds / 10
	if jitterMax <= 0 {
		return base
	}
	jitter := int(sum[0]) % (jitterMax + 1)
	return base + time.Duration(jitter)*time.Second
}

func prunePollState(state *pollState, maxSeen int) {
	if state == nil || len(state.Seen) == 0 {
		return
	}
	if maxSeen <= 0 {
		maxSeen = 1000
	}
	if len(state.Seen) <= maxSeen {
		return
	}
	type entry struct {
		key      string
		lastSeen string
	}
	entries := make([]entry, 0, len(state.Seen))
	for key, seen := range state.Seen {
		entries = append(entries, entry{key: key, lastSeen: seen.LastSeen})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastSeen < entries[j].lastSeen })
	for i := 0; i < len(entries)-maxSeen; i++ {
		delete(state.Seen, entries[i].key)
	}
}

func formatPollTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
