package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

func (s *Store) migrateSubscriptionOutbox() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_subscription_outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, event_key TEXT NOT NULL, subscription_id TEXT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
 subscription_json TEXT NOT NULL,event_json TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,
 UNIQUE(event_key,subscription_id));
 CREATE INDEX IF NOT EXISTS idx_app_subscriptions_match ON subscriptions(source,enabled,slug,project_id);
 CREATE INDEX IF NOT EXISTS idx_subscription_outbox_due ON app_subscription_outbox(status,next_attempt)`)
	return err
}

func appSubscriptionMatches(sub *Subscription, ev AppEvent) bool {
	app, pattern, ok := splitAppEventSlug(sub.Slug)
	return ok && sub.Enabled && app == ev.App && (sub.ProjectID == "" || sub.ProjectID == ev.ProjectID) && appEventSubscriptionTopicMatches(sub, pattern, ev.Topic) && subscriptionPayloadMatches(sub, ev.Data)
}

// Commit all matching deliveries before acknowledging or publishing an emit.
// Subscriber channel overflow/restart cannot discard the durable work.
func (s *Server) queueAppSubscriptions(ev AppEvent) error {
	subs, err := s.store.listAppEventSubscriptions(ev.App, ev.ProjectID)
	if err != nil {
		return err
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := generateID()
	raw, _ := json.Marshal(ev)
	for _, sub := range subs {
		if appSubscriptionMatches(sub, ev) {
			snapshot, _ := json.Marshal(sub)
			if _, err := tx.Exec(`INSERT INTO app_subscription_outbox(event_key,subscription_id,subscription_json,event_json,created_at) VALUES(?,?,?,?,?)`, key, sub.ID, string(snapshot), string(raw), time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (d *AppEventDispatcher) enqueueAndDeliver(sub *Subscription, ev AppEvent) {
	key := fmt.Sprintf("bus:%s:%s:%d", ev.App, ev.ProjectID, ev.Seq)
	raw, _ := json.Marshal(ev)
	snapshot, _ := json.Marshal(sub)
	_, err := d.server.store.db.Exec(`INSERT OR IGNORE INTO app_subscription_outbox(event_key,subscription_id,subscription_json,event_json,created_at) VALUES(?,?,?,?,?)`, key, sub.ID, string(snapshot), string(raw), time.Now().Unix())
	if err != nil {
		log.Printf("[APP-SUB] queue: %v", err)
		return
	}
	var id int64
	var state string
	if err = d.server.store.db.QueryRow(`SELECT id,status FROM app_subscription_outbox WHERE event_key=? AND subscription_id=?`, key, sub.ID).Scan(&id, &state); err == nil && state == "pending" {
		d.deliverOutbox(id, sub, ev)
	}
}

func (d *AppEventDispatcher) deliverOutbox(id int64, sub *Subscription, ev AppEvent) {
	// Multiple wakeups/replays may race. Claim only this row; ordinary fan-out
	// to independent agents remains parallel. Reset claims on dispatcher boot.
	result, err := d.server.store.db.Exec("UPDATE app_subscription_outbox SET status='sending' WHERE id=? AND status='pending'", id)
	if err != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return
	}
	current, err := d.server.store.GetSubscription(sub.UserID, sub.ID)
	if err != nil || current == nil || !current.Enabled || current.AgentID != sub.AgentID {
		d.server.store.db.Exec("UPDATE app_subscription_outbox SET status='canceled' WHERE id=?", id)
		return
	}
	ev.deliveryID = fmt.Sprintf("app-subscription-%d", id)
	err = d.deliver(sub, ev)
	if err != nil {
		var attempts int
		d.server.store.db.QueryRow("SELECT attempts FROM app_subscription_outbox WHERE id=?", id).Scan(&attempts)
		attempts++
		state := "pending"
		if attempts >= 12 {
			state = "failed"
		}
		d.server.store.db.Exec("UPDATE app_subscription_outbox SET status=?,attempts=?,next_attempt=?,last_error=? WHERE id=?", state, attempts, time.Now().Add(agentEventDeliveryBackoff(attempts)).UnixMilli(), truncate(err.Error(), 1000), id)
		log.Printf("[APP-SUB] delivery sub=%s attempt=%d status=%s: %v", sub.ID, attempts, state, err)
		return
	}
	d.server.store.db.Exec("UPDATE app_subscription_outbox SET status='delivered',last_error='' WHERE id=?", id)
	if ev.Seq > 0 {
		d.markDelivered(sub, ev.Seq)
	}
	if sub.DeleteOnMatch {
		d.server.store.DeleteEphemeralSubscriptionWaitGroup(sub.WaitGroupID)
		d.Reconcile()
	}
}

func (d *AppEventDispatcher) drainOutbox(ctx context.Context) {
	rows, err := d.server.store.db.QueryContext(ctx, "SELECT id,subscription_json,event_json FROM app_subscription_outbox WHERE status='pending' AND next_attempt<=? ORDER BY id LIMIT 32", time.Now().UnixMilli())
	if err != nil {
		return
	}
	type job struct {
		id  int64
		sub Subscription
		ev  AppEvent
	}
	jobs := []job{}
	for rows.Next() {
		var j job
		var sub, ev string
		if rows.Scan(&j.id, &sub, &ev) == nil && json.Unmarshal([]byte(sub), &j.sub) == nil && json.Unmarshal([]byte(ev), &j.ev) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	parallelJobs(ctx, len(jobs), 8, func(i int) { d.deliverOutbox(jobs[i].id, &jobs[i].sub, jobs[i].ev) })
}
func (d *AppEventDispatcher) wakeOutbox() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
func (d *AppEventDispatcher) runOutbox(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		d.drainOutbox(ctx)
		d.server.store.db.Exec("DELETE FROM app_subscription_outbox WHERE id IN (SELECT id FROM app_subscription_outbox WHERE status IN ('delivered','canceled') AND created_at<? LIMIT 1000)", time.Now().Add(-7*24*time.Hour).Unix())
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}
func (d *AppEventDispatcher) Stop() {
	d.mu.Lock()
	d.stopped = true
	if d.stopOutbox != nil {
		d.stopOutbox()
	}
	for _, lane := range d.lanes {
		lane.cancel()
	}
	d.lanes = map[busKey]*appEventLane{}
	d.mu.Unlock()
	d.wg.Wait()
}
