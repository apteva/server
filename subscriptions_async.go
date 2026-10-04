package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/apteva/server/apps/framework"
	"strings"
	"sync"
	"time"
)

const asyncReplayRetention = 24 * time.Hour

// Bound memory while serializing deliveries with server-mediated thread kills.
// Core's /event may lazily create a missing thread, so deletion must not race it.
var asyncThreadDeliveryLocks [128]sync.Mutex

func lockAsyncThreadDelivery(agentID int64, threadID string) func() {
	if threadID == "" {
		threadID = "main"
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", agentID, threadID)))
	lock := &asyncThreadDeliveryLocks[int(sum[0])%len(asyncThreadDeliveryLocks)]
	lock.Lock()
	return lock.Unlock
}

func (s *Store) asyncTargetStillScoped(sub *Subscription) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM agents a WHERE a.id=? AND a.user_id=? AND
 (COALESCE(a.project_id,'')=? OR EXISTS (SELECT 1 FROM agent_thread_scopes t WHERE t.agent_id=a.id AND t.thread_id=? AND t.project_id=?))`,
		sub.AgentID, sub.UserID, sub.ProjectID, sub.ThreadID, sub.ProjectID).Scan(&count)
	return count == 1, err
}

type asyncSubscriptionOptions struct {
	Mode            string
	TerminalEvents  []string
	SourceInstallID int64
	Cursor          int64
	StartedAt       time.Time
	ThreadEpoch     int64
}

// Check existing subthreads before using Core's lazy-creating event endpoint.
// An unavailable runtime is retryable; an authoritative absence retires the
// subscription. Never redirect a notification to main.
func (s *Server) asyncThreadExists(sub *Subscription) (bool, error) {
	if sub.ThreadID == "" || sub.ThreadID == "main" {
		return true, nil
	}
	port := s.agents.GetPort(sub.AgentID)
	if port == 0 {
		return false, fmt.Errorf("agent %d not running", sub.AgentID)
	}
	_, exists, err := s.resolver().inspectOpaqueThread(framework.InstanceInfo{
		ID: sub.AgentID, Port: port, CoreAPIKey: s.agents.GetCoreAPIKey(sub.AgentID),
	}, sub.ThreadID)
	return exists, err
}

func (s *Store) migrateAsyncSubscriptions() error {
	for _, col := range []struct{ name, definition string }{
		{"source_install_id", "INTEGER NOT NULL DEFAULT 0"},
		{"async_mode", "TEXT NOT NULL DEFAULT ''"},
		{"terminal_events", "TEXT NOT NULL DEFAULT '[]'"},
	} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('subscriptions') WHERE name=?`, col.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := s.db.Exec("ALTER TABLE subscriptions ADD COLUMN " + col.name + " " + col.definition); err != nil {
				return err
			}
		}
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_async_event_journal (
 id INTEGER PRIMARY KEY AUTOINCREMENT, event_key TEXT NOT NULL UNIQUE,
 source_install_id INTEGER NOT NULL, project_id TEXT NOT NULL, event_json TEXT NOT NULL, created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_async_journal_source ON app_async_event_journal(source_install_id,project_id,id);
 CREATE INDEX IF NOT EXISTS idx_async_journal_expiry ON app_async_event_journal(created_at);
 CREATE TABLE IF NOT EXISTS async_thread_epochs (
 agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE, thread_id TEXT NOT NULL, epoch INTEGER NOT NULL,
 PRIMARY KEY(agent_id,thread_id));`)
	return err
}

// Captured before dispatch, not when the response arrives. The journal is the
// existing app-event stream's bounded replay storage, not a new publication API.
func (s *Store) asyncSubscriptionCursor(agentID int64, threadID string) (int64, int64, error) {
	var cursor, epoch int64
	err := s.db.QueryRow(`SELECT COALESCE((SELECT MAX(id) FROM app_async_event_journal),0),
 COALESCE((SELECT epoch FROM async_thread_epochs WHERE agent_id=? AND thread_id=?),0)`, agentID, threadID).Scan(&cursor, &epoch)
	return cursor, epoch, err
}

func subscriptionExpired(sub *Subscription) bool {
	if sub.ExpiresAt == "" {
		return false
	}
	at, err := parseTime(sub.ExpiresAt)
	return err != nil || !time.Now().Before(at)
}

func subscriptionTerminal(sub *Subscription, topic string) bool {
	if sub.Kind != "ephemeral" {
		return false
	}
	if sub.AsyncMode != "stream" {
		return sub.DeleteOnMatch
	}
	for _, terminal := range sub.TerminalEvents {
		if topic == terminal {
			return true
		}
	}
	return false
}

func (s *Store) createAsyncSubscription(userID, agentID int64, name, slug, description, threadID, projectID string, events []string, matchJSON, waitGroupID string, expiresAt time.Time, options ...asyncSubscriptionOptions) (*Subscription, error) {
	opts := asyncSubscriptionOptions{Mode: "once"}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Mode == "" {
		opts.Mode = "once"
	}
	if opts.Mode != "once" && opts.Mode != "stream" {
		return nil, errors.New("unsupported async mode")
	}
	if opts.Mode == "stream" && len(opts.TerminalEvents) == 0 {
		return nil, errors.New("stream requires terminal events")
	}
	if !opts.StartedAt.IsZero() && time.Since(opts.StartedAt) >= asyncReplayRetention {
		return nil, errors.New("async call exceeded the event replay window")
	}
	sub := &Subscription{ID: generateID(), UserID: userID, AgentID: agentID, Name: name, Slug: slug,
		Description: description, WebhookPath: internalSubscriptionWebhookPath("app-event"), Enabled: true,
		NotifyAgent: true, ThreadID: strings.TrimSpace(threadID), ProjectID: projectID, Events: compactSubscriptionEvents(events),
		Source: "app_event", Delivery: "app_event", Kind: "ephemeral", MatchJSON: matchJSON, WaitGroupID: waitGroupID,
		DeleteOnMatch: opts.Mode == "once", CreatedAt: time.Now(), SourceInstallID: opts.SourceInstallID, AsyncMode: opts.Mode, TerminalEvents: opts.TerminalEvents}
	if !expiresAt.IsZero() {
		sub.ExpiresAt = expiresAt.UTC().Format("2006-01-02 15:04:05.999999999")
	}
	eventJSON, err := json.Marshal(sub.Events)
	if err != nil {
		return nil, err
	}
	terminalJSON, err := json.Marshal(sub.TerminalEvents)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The epoch prevents an in-flight tool response from re-creating a
	// subscription after the caller thread was killed, including ID reuse.
	if !opts.StartedAt.IsZero() {
		var epoch int64
		if err := tx.QueryRow(`SELECT COALESCE((SELECT epoch FROM async_thread_epochs WHERE agent_id=? AND thread_id=?),0)`, agentID, sub.ThreadID).Scan(&epoch); err != nil {
			return nil, err
		}
		if epoch != opts.ThreadEpoch {
			return nil, errors.New("caller thread was deleted during the async call")
		}
	}
	_, err = tx.Exec(`INSERT INTO subscriptions
 (id,user_id,agent_id,connection_id,name,slug,description,webhook_path,encrypted_hmac_secret,thread_id,project_id,events,source,delivery,notify_agent,kind,match_json,wait_group_id,expires_at,delete_on_match,source_install_id,async_mode,terminal_events)
 VALUES(?,?,?,0,?,?,?,?,'',?,?,?,'app_event','app_event',1,'ephemeral',?,?,?,?,?,?,?)`,
		sub.ID, userID, agentID, name, slug, description, sub.WebhookPath, sub.ThreadID, projectID, string(eventJSON), matchJSON, waitGroupID, sub.ExpiresAt, sub.DeleteOnMatch, sub.SourceInstallID, sub.AsyncMode, string(terminalJSON))
	if err != nil {
		return nil, err
	}
	if !opts.StartedAt.IsZero() {
		if err := replayAsyncEvents(tx, sub, opts.Cursor); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sub, nil
}

func replayAsyncEvents(tx *sql.Tx, sub *Subscription, cursor int64) error {
	rows, err := tx.Query(`SELECT event_key,event_json FROM app_async_event_journal WHERE source_install_id=? AND project_id=? AND id>? ORDER BY id LIMIT 10001`, sub.SourceInstallID, sub.ProjectID, cursor)
	if err != nil {
		return err
	}
	type event struct {
		key, raw string
		value    AppEvent
	}
	events := []event{}
	count := 0
	replayBytes := 0
	for rows.Next() {
		count++
		var item event
		if err := rows.Scan(&item.key, &item.raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(item.raw), &item.value); err != nil {
			rows.Close()
			return fmt.Errorf("invalid async replay event: %w", err)
		}
		if appSubscriptionMatches(sub, item.value) {
			replayBytes += len(item.raw)
			if replayBytes > 16<<20 {
				rows.Close()
				return errors.New("async event replay exceeds 16 MiB; notification registration failed")
			}
			events = append(events, item)
			if subscriptionTerminal(sub, item.value.Topic) {
				break
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if count > 10000 {
		return errors.New("async event replay limit exceeded; notification registration failed")
	}
	snapshot, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	for _, item := range events {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO app_subscription_outbox(event_key,subscription_id,subscription_json,event_json,created_at) VALUES(?,?,?,?,?)`, item.key, sub.ID, string(snapshot), item.raw, time.Now().Unix()); err != nil {
			return err
		}
		if subscriptionTerminal(sub, item.value.Topic) {
			break
		}
	}
	return nil
}
