package main

// AppEventDispatcher — server-side bridge from AppEventBus to
// subscriptions of source='app_event'. The agent gets woken up by
// sibling-app emits (tables.row.inserted, storage.file.added, ...)
// using exactly the same /event POST path that external webhooks
// already use, but the trigger source is the in-process bus instead
// of an HTTP webhook.
//
// Efficiency: one bus subscriber per (app, project) lane, regardless
// of how many subscription rows point at that lane. The lane's
// current row set is held in-memory; CRUD on subscriptions just
// mutates the slice + (de)allocates lane subscribers as the set of
// distinct (app, project) pairs changes. No goroutine churn on
// subscription edits.
//
// The bus ring is process-local, so it cannot replay events across a server
// restart. We do persist last_seq_delivered on every successful delivery and
// restore each lane's producer high-water mark before subscribing. That keeps
// newly published events deliverable after restart. Durable historical replay
// remains the source app's responsibility.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AppEventDispatcher owns the bus→subscription bridge. One per Server.
type AppEventDispatcher struct {
	server *Server

	mu    sync.Mutex
	lanes map[busKey]*appEventLane
}

type appEventLane struct {
	app       string
	projectID string

	// rows: every subscription row currently bound to this lane.
	// Replaced wholesale on reconcile rather than mutated in place
	// so the goroutine sees a stable snapshot per event.
	rows []*Subscription

	// Bus subscription handle: channel + cancel + the seq we joined at.
	ch     chan AppEvent
	cancel func()
}

// NewAppEventDispatcher constructs but does not start. Caller invokes
// Start() once the bus is up. Safe to call before subscriptions exist.
func NewAppEventDispatcher(server *Server) *AppEventDispatcher {
	return &AppEventDispatcher{
		server: server,
		lanes:  make(map[busKey]*appEventLane),
	}
}

// Start scans subscriptions and brings every (app, project) lane
// online. Idempotent — calling twice is a no-op as long as no rows
// have changed in between.
func (d *AppEventDispatcher) Start() {
	if err := d.Reconcile(); err != nil {
		log.Printf("[APP-SUB] startup reconcile failed: %v", err)
	}
}

// Reconcile rebuilds the in-memory lane → row map from the database
// and brings the bus subscribers in line: starts new lanes that
// gained their first row, stops lanes that lost their last row, and
// updates the row slice on lanes that already existed.
//
// Called at boot and after every CRUD on subscriptions. Cheap — one
// query + a few map operations even at high subscription count.
func (d *AppEventDispatcher) Reconcile() error {
	if err := d.server.store.CleanupExpiredEphemeralSubscriptions(); err != nil {
		log.Printf("[APP-SUB] cleanup expired ephemeral subscriptions: %v", err)
	}
	rows, err := d.server.store.ListAllAppEventSubscriptions()
	if err != nil {
		return err
	}

	// Group rows by lane key, derived from slug (format "app:pattern").
	desired := make(map[busKey][]*Subscription)
	for _, sub := range rows {
		if !sub.Enabled {
			continue
		}
		app, _, ok := splitAppEventSlug(sub.Slug)
		if !ok {
			log.Printf("[APP-SUB] skip sub=%s: malformed slug %q (want 'app:topic_pattern')",
				sub.ID, sub.Slug)
			continue
		}
		k := busKey{app: app, projectID: sub.ProjectID}
		desired[k] = append(desired[k], sub)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Drop lanes that no longer have any subscribers.
	for k, lane := range d.lanes {
		if _, want := desired[k]; !want {
			lane.cancel()
			delete(d.lanes, k)
			log.Printf("[APP-SUB] lane stopped app=%s project=%s (no rows)", k.app, k.projectID)
		}
	}

	// Bring up new lanes; refresh row sets on existing ones.
	for k, subs := range desired {
		since, sequenceFloor := appEventLaneCursors(subs)
		// A fresh AppEventBus starts every counter at zero, while these
		// per-row cursors survive in SQLite. Restore the highest observed
		// cursor before any subscriber attaches so the next Publish cannot
		// be rejected as an old duplicate by every row in this lane.
		d.server.appBus.EnsureLaneSequenceAtLeast(k.app, k.projectID, sequenceFloor)
		if existing, ok := d.lanes[k]; ok {
			existing.rows = subs
			continue
		}
		lane := &appEventLane{app: k.app, projectID: k.projectID, rows: subs}
		// Use the lowest last_seq_delivered across the lane's rows
		// as the resubscribe cursor — anything newer than that is
		// owed to at least one subscriber, so we want the bus to
		// replay it. Each row's per-row delivery filter still
		// applies (rows that already received a higher seq just
		// see those replays as duplicates and skip via the
		// last_seq_delivered guard at deliver-time).
		ch, replay, cancel := d.server.appBus.Subscribe(k.app, k.projectID, since)
		lane.ch = ch
		lane.cancel = cancel
		d.lanes[k] = lane
		go d.run(lane, replay)
		log.Printf("[APP-SUB] lane started app=%s project=%s rows=%d since=%d floor=%d replay=%d",
			k.app, k.projectID, len(subs), since, sequenceFloor, len(replay))
	}
	return nil
}

// appEventLaneCursors returns two deliberately different cursors:
//
//   - replaySince is the lowest delivered sequence, so an in-process
//     reconciliation can replay anything still owed to at least one row.
//   - sequenceFloor is the highest delivered sequence, so a newly-created bus
//     never restarts its producer counter below any persisted row.
func appEventLaneCursors(subs []*Subscription) (replaySince, sequenceFloor uint64) {
	if len(subs) == 0 {
		return 0, 0
	}
	replaySince = subs[0].LastSeqDelivered
	sequenceFloor = replaySince
	for _, sub := range subs[1:] {
		if sub.LastSeqDelivered < replaySince {
			replaySince = sub.LastSeqDelivered
		}
		if sub.LastSeqDelivered > sequenceFloor {
			sequenceFloor = sub.LastSeqDelivered
		}
	}
	return replaySince, sequenceFloor
}

// run is the per-lane goroutine. Drains the lane's channel and
// dispatches each event to every row whose pattern matches. Replays
// from the ring fire first.
func (d *AppEventDispatcher) run(lane *appEventLane, replay []AppEvent) {
	for _, ev := range replay {
		d.dispatch(lane, ev)
	}
	for ev := range lane.ch {
		d.dispatch(lane, ev)
	}
}

// dispatch fans an event to every matching row in the lane. Rows are
// snapshot-stable per event (we copy the slice header to avoid
// holding the map mutex during HTTP delivery).
func (d *AppEventDispatcher) dispatch(lane *appEventLane, ev AppEvent) {
	d.mu.Lock()
	rows := lane.rows
	d.mu.Unlock()
	for _, sub := range rows {
		// Filter: skip events the row already saw on a prior
		// boot (the lane resubscribes at min(last_seq_delivered),
		// so other rows in the lane may be ahead).
		if ev.Seq <= sub.LastSeqDelivered {
			continue
		}
		_, pattern, ok := splitAppEventSlug(sub.Slug)
		if !ok {
			continue
		}
		if !appEventSubscriptionTopicMatches(sub, pattern, ev.Topic) {
			continue
		}
		if !subscriptionPayloadMatches(sub, ev.Data) {
			continue
		}
		if err := d.deliver(sub, ev); err != nil {
			log.Printf("[APP-SUB] deliver failed sub=%s: %v", sub.ID, err)
			continue
		}
		// Persist progress so a restart skips already-delivered
		// events. Cheap — UPDATE by primary key, and the row
		// pointer is a fresh copy each Reconcile so we mutate
		// safely under the dispatcher's mu when we write back.
		d.markDelivered(sub, ev.Seq)
		if sub.DeleteOnMatch {
			if err := d.server.store.DeleteEphemeralSubscriptionWaitGroup(sub.WaitGroupID); err != nil {
				log.Printf("[APP-SUB] delete wait group sub=%s group=%q: %v", sub.ID, sub.WaitGroupID, err)
				continue
			}
			if err := d.Reconcile(); err != nil {
				log.Printf("[APP-SUB] reconcile after one-shot delete sub=%s: %v", sub.ID, err)
			}
		}
	}
}

// deliver is the actual core /event POST. Mirrors
// handleSubscriptionWebhook's delivery section: same instance lookup,
// same authorization, same payload shape — the only difference is
// the prefix tag.
func (d *AppEventDispatcher) deliver(sub *Subscription, ev AppEvent) error {
	inst, err := d.server.store.GetAgent(sub.UserID, sub.AgentID)
	if err != nil {
		return fmt.Errorf("instance %d not found: %w", sub.AgentID, err)
	}
	port := d.server.agents.GetPort(inst.ID)
	if port == 0 {
		return fmt.Errorf("instance %d not running", inst.ID)
	}

	// Truncate large payloads — same 2KB cap webhook delivery uses.
	dataStr := string(ev.Data)
	if len(dataStr) > 2000 {
		dataStr = dataStr[:2000] + "...[truncated]"
	}
	eventMsg := fmt.Sprintf("[app:%s:%s] %s", ev.App, ev.Topic, dataStr)

	body := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		body["thread_id"] = sub.ThreadID
	}
	bodyJSON, _ := json.Marshal(body)
	url := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(bodyJSON)))
	req.Header.Set("Content-Type", "application/json")
	if k := d.server.agents.GetCoreAPIKey(inst.ID); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("core rejected http %d", resp.StatusCode)
	}
	return nil
}

// markDelivered persists sub.LastSeqDelivered so a restart picks up
// where we left off. In-memory pointer is updated under the
// dispatcher mutex so the dispatch loop's seq guard reads a current
// value on the next event.
func (d *AppEventDispatcher) markDelivered(sub *Subscription, seq uint64) {
	d.mu.Lock()
	sub.LastSeqDelivered = seq
	d.mu.Unlock()
	if _, err := d.server.store.db.Exec(
		`UPDATE subscriptions SET last_seq_delivered = ? WHERE id = ? AND last_seq_delivered < ?`,
		seq, sub.ID, seq,
	); err != nil {
		log.Printf("[APP-SUB] persist seq sub=%s: %v", sub.ID, err)
	}
}

// splitAppEventSlug parses 'app:topic_pattern' from the subscription
// slug. Returns ok=false on malformed input.
func splitAppEventSlug(slug string) (app, pattern string, ok bool) {
	idx := strings.IndexByte(slug, ':')
	if idx <= 0 || idx == len(slug)-1 {
		return "", "", false
	}
	return slug[:idx], slug[idx+1:], true
}

func appEventSubscriptionTopicMatches(sub *Subscription, legacyPattern, topic string) bool {
	if sub != nil && len(sub.Events) > 0 {
		for _, event := range sub.Events {
			if matchTopic(strings.TrimSpace(event), topic) {
				return true
			}
		}
		return false
	}
	return matchTopic(legacyPattern, topic)
}

func subscriptionPayloadMatches(sub *Subscription, payload json.RawMessage) bool {
	if sub == nil || strings.TrimSpace(sub.MatchJSON) == "" {
		return true
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(sub.MatchJSON), &want); err != nil {
		return false
	}
	if len(want) == 0 {
		return true
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		return false
	}
	for key, wantValue := range want {
		gotValue, ok := got[key]
		if !ok || !jsonScalarEqual(gotValue, wantValue) {
			return false
		}
	}
	return true
}

func jsonScalarEqual(a, b any) bool {
	switch av := a.(type) {
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		case json.Number:
			f, _ := bv.Float64()
			return av == f
		}
	case string:
		return av == fmt.Sprint(b)
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// matchTopic — the topic pattern grammar:
//
//	"foo.bar"   exact match
//	"foo.*"     prefix match — matches "foo.x", "foo.x.y", but not "foo"
//	"*"         match anything
//
// Anything else falls back to exact match. Kept simple so consumers
// can reason about it without reaching for the regex chapter.
func matchTopic(pattern, topic string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, "*") // keeps the trailing dot
		return strings.HasPrefix(topic, prefix)
	}
	return pattern == topic
}
