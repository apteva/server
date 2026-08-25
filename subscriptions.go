package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Subscription struct {
	ID                string   `json:"id"`
	UserID            int64    `json:"user_id"`
	AgentID           int64    `json:"instance_id"`
	ConnectionID      int64    `json:"connection_id"`
	Name              string   `json:"name"`
	Slug              string   `json:"slug"`
	Description       string   `json:"description"`
	WebhookPath       string   `json:"webhook_path"`
	Enabled           bool     `json:"enabled"`
	NotifyAgent       bool     `json:"notify_agent"`
	ThreadID          string   `json:"thread_id,omitempty"`
	ProjectID         string   `json:"project_id,omitempty"`
	Events            []string `json:"events"`
	ExternalWebhookID string   `json:"external_webhook_id,omitempty"`
	// Source: 'webhook' (default — ingress via /webhooks/<token>) or
	// 'app_event' (ingress via the in-process AppEventBus, slug
	// carries '<app>:<topic_pattern>').
	Source           string         `json:"source,omitempty"`
	Delivery         string         `json:"delivery,omitempty"`
	PollConfigJSON   string         `json:"-"`
	PollStateJSON    string         `json:"-"`
	LastRunAt        string         `json:"last_run_at,omitempty"`
	NextRunAt        string         `json:"next_run_at,omitempty"`
	LastError        string         `json:"last_error,omitempty"`
	FailureCount     int            `json:"failure_count,omitempty"`
	LastSeqDelivered uint64         `json:"last_seq_delivered,omitempty"`
	Kind             string         `json:"kind,omitempty"`
	MatchJSON        string         `json:"-"`
	Filters          map[string]any `json:"filters,omitempty"`
	WaitGroupID      string         `json:"wait_group_id,omitempty"`
	ExpiresAt        string         `json:"expires_at,omitempty"`
	DeleteOnMatch    bool           `json:"delete_on_match,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
}

// ListAllAppEventSubscriptions returns every source='app_event' row
// in the database, regardless of user. Used by AppEventDispatcher's
// reconcile pass at boot + on subscription CRUD. Read-only,
// short-lived — no need to scope by user since the dispatcher routes
// by lane key, not by ownership.
func (s *Store) ListAllAppEventSubscriptions() ([]*Subscription, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, agent_id, connection_id, name, slug, description,
			webhook_path, enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
			COALESCE(project_id,''), COALESCE(source,'webhook'),
			COALESCE(last_seq_delivered,0),
			COALESCE(kind,'user'), COALESCE(match_json,''), COALESCE(wait_group_id,''),
			COALESCE(expires_at,''), COALESCE(delete_on_match,0)
		 FROM subscriptions
		 WHERE source = 'app_event'
		   AND enabled = 1
		   AND (COALESCE(expires_at,'') = '' OR expires_at > datetime('now'))`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		sub := &Subscription{}
		var enabled, notifyAgent, deleteOnMatch int
		var eventsJSON string
		if err := rows.Scan(
			&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID,
			&sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath,
			&enabled, &notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
			&sub.Source, &sub.LastSeqDelivered, &sub.Kind, &sub.MatchJSON,
			&sub.WaitGroupID, &sub.ExpiresAt, &deleteOnMatch,
		); err != nil {
			return nil, err
		}
		sub.Enabled = enabled != 0
		sub.NotifyAgent = notifyAgent != 0
		sub.DeleteOnMatch = deleteOnMatch != 0
		if eventsJSON != "" {
			_ = json.Unmarshal([]byte(eventsJSON), &sub.Events)
		}
		hydrateSubscriptionFilters(sub)
		out = append(out, sub)
	}
	return out, nil
}

// ListSubscriptionsByConnection returns every subscription bound to a
// given connection (all projects, all threads). Used by the connection
// delete cascade — before tearing down a connection we fetch its subs
// so we can unregister each upstream webhook and then remove the rows.
func (s *Store) ListSubscriptionsByConnection(userID, connectionID int64) ([]Subscription, error) {
	rows, err := s.db.Query(
		"SELECT id, agent_id, name, slug, webhook_path, COALESCE(external_webhook_id,'') FROM subscriptions WHERE user_id = ? AND connection_id = ?",
		userID, connectionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		rows.Scan(&sub.ID, &sub.AgentID, &sub.Name, &sub.Slug, &sub.WebhookPath, &sub.ExternalWebhookID)
		sub.UserID = userID
		sub.ConnectionID = connectionID
		subs = append(subs, sub)
	}
	return subs, nil
}

// --- Store methods ---

func internalSubscriptionWebhookPath(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "internal"
	}
	return kind + "-" + generateToken(16)
}

func compactSubscriptionEvents(events []string) []string {
	if len(events) == 0 {
		return nil
	}
	out := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		event = strings.TrimSpace(event)
		if event == "" || seen[event] {
			continue
		}
		seen[event] = true
		out = append(out, event)
	}
	return out
}

func isInternalSubscriptionWebhookPath(path string) bool {
	return strings.HasPrefix(path, "app-event-") ||
		strings.HasPrefix(path, "poll-") ||
		strings.HasPrefix(path, "internal-")
}

func (s *Store) CreateSubscription(userID, instanceID, connectionID int64, name, slug, description, webhookPath, encryptedSecret, threadID, projectID string, events []string, notifyAgentOpt ...bool) (*Subscription, error) {
	notifyAgent := len(notifyAgentOpt) > 0 && notifyAgentOpt[0]
	id := generateID()
	if strings.TrimSpace(webhookPath) == "" {
		webhookPath = internalSubscriptionWebhookPath("internal")
	}
	events = compactSubscriptionEvents(events)
	eventsJSON := ""
	if len(events) > 0 {
		if b, merr := json.Marshal(events); merr == nil {
			eventsJSON = string(b)
		}
	}
	_, err := s.db.Exec(
		"INSERT INTO subscriptions (id, user_id, agent_id, connection_id, name, slug, description, webhook_path, encrypted_hmac_secret, thread_id, project_id, events, notify_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, userID, instanceID, connectionID, name, slug, description, webhookPath, encryptedSecret, threadID, projectID, eventsJSON, boolToInt(notifyAgent),
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{ID: id, UserID: userID, AgentID: instanceID, ConnectionID: connectionID, Name: name, Slug: slug, Description: description, WebhookPath: webhookPath, Enabled: true, NotifyAgent: notifyAgent, ThreadID: threadID, ProjectID: projectID, Events: events, Source: "webhook", Delivery: "webhook", CreatedAt: time.Now()}, nil
}

func (s *Store) CreatePollSubscription(userID, instanceID, connectionID int64, name, slug, description, threadID, projectID string, events []string, pollConfigJSON string, nextRunAt time.Time, notifyAgentOpt ...bool) (*Subscription, error) {
	notifyAgent := len(notifyAgentOpt) > 0 && notifyAgentOpt[0]
	id := generateID()
	webhookPath := "poll-" + generateToken(16)
	events = compactSubscriptionEvents(events)
	eventsJSON := ""
	if len(events) > 0 {
		if b, merr := json.Marshal(events); merr == nil {
			eventsJSON = string(b)
		}
	}
	nextRun := formatPollTime(nextRunAt)
	_, err := s.db.Exec(
		`INSERT INTO subscriptions
			(id, user_id, agent_id, connection_id, name, slug, description,
			 webhook_path, encrypted_hmac_secret, thread_id, project_id, events,
			 source, delivery, poll_config_json, poll_state_json, next_run_at, notify_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, 'webhook', 'poll', ?, '', ?, ?)`,
		id, userID, instanceID, connectionID, name, slug, description,
		webhookPath, threadID, projectID, eventsJSON, pollConfigJSON, nextRun, boolToInt(notifyAgent),
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{
		ID: id, UserID: userID, AgentID: instanceID, ConnectionID: connectionID,
		Name: name, Slug: slug, Description: description, WebhookPath: webhookPath,
		Enabled: true, NotifyAgent: notifyAgent, ThreadID: threadID, ProjectID: projectID,
		Events: events, Source: "webhook", Delivery: "poll", PollConfigJSON: pollConfigJSON,
		NextRunAt: nextRun, CreatedAt: time.Now(),
	}, nil
}

// CreateAppEventSubscription is the source='app_event' counterpart
// of CreateSubscription. No public webhook path / encrypted_secret /
// connection_id — the bridge dispatcher routes events from the
// in-process bus straight into the agent. The slug carries the app
// lane plus a legacy topic pattern. When events is non-empty, the
// dispatcher matches against that list instead of the slug topic so a
// single row can subscribe to multiple app topics.
func (s *Store) CreateAppEventSubscription(userID, instanceID int64, name, slug, description, threadID, projectID string, events []string, notifyAgentOpt ...bool) (*Subscription, error) {
	notifyAgent := len(notifyAgentOpt) > 0 && notifyAgentOpt[0]
	return s.CreateAppEventSubscriptionWithFilters(
		userID, instanceID, name, slug, description, threadID, projectID,
		events, nil, notifyAgent,
	)
}

// CreateAppEventSubscriptionWithFilters persists a user-facing app-event
// subscription with an optional flat field/value filter. match_json predates
// public filters and remains the storage representation so this is backwards
// compatible and requires no schema migration.
func (s *Store) CreateAppEventSubscriptionWithFilters(userID, instanceID int64, name, slug, description, threadID, projectID string, events []string, filters map[string]any, notifyAgent bool) (*Subscription, error) {
	filters, matchJSON, err := normalizeSubscriptionFilters(filters)
	if err != nil {
		return nil, err
	}
	id := generateID()
	webhookPath := internalSubscriptionWebhookPath("app-event")
	events = compactSubscriptionEvents(events)
	eventsJSON := ""
	if len(events) > 0 {
		if b, merr := json.Marshal(events); merr == nil {
			eventsJSON = string(b)
		}
	}
	// 14 columns / 14 values. agent_id binds instanceID; connection_id
	// is the literal 0 because app-event subscriptions don't go through
	// a connection. Pre-fix this had fewer values (agent_id was a
	// literal 0 and there was no slot for connection_id/events) so the INSERT
	// errored with "12 values for 13 columns" the moment the dashboard
	// tried to subscribe to an app event.
	_, err = s.db.Exec(
		`INSERT INTO subscriptions
				(id, user_id, agent_id, connection_id, name, slug, description,
				 webhook_path, encrypted_hmac_secret, thread_id, project_id, events,
				 source, delivery, notify_agent, match_json)
			 VALUES (?, ?, ?, 0, ?, ?, ?, ?, '', ?, ?, ?, 'app_event', 'app_event', ?, ?)`,
		id, userID, instanceID, name, slug, description, webhookPath, threadID,
		projectID, eventsJSON, boolToInt(notifyAgent), matchJSON,
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{
		ID: id, UserID: userID, AgentID: instanceID,
		Name: name, Slug: slug, Description: description, WebhookPath: webhookPath,
		Enabled: true, NotifyAgent: notifyAgent, ThreadID: threadID, ProjectID: projectID,
		Events: events, Source: "app_event", Delivery: "app_event",
		MatchJSON: matchJSON, Filters: filters, CreatedAt: time.Now(),
	}, nil
}

const (
	maxSubscriptionFilters     = 20
	maxSubscriptionFiltersJSON = 8 * 1024
)

func normalizeSubscriptionFilters(filters map[string]any) (map[string]any, string, error) {
	if len(filters) == 0 {
		return nil, "", nil
	}
	if len(filters) > maxSubscriptionFilters {
		return nil, "", fmt.Errorf("filters: at most %d fields are allowed", maxSubscriptionFilters)
	}
	normalized := make(map[string]any, len(filters))
	for rawKey, value := range filters {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			return nil, "", errors.New("filters: field names cannot be empty")
		}
		if len(key) > 128 {
			return nil, "", fmt.Errorf("filters: field name %q is too long", key)
		}
		if _, exists := normalized[key]; exists {
			return nil, "", fmt.Errorf("filters: field %q is duplicated", key)
		}
		switch value.(type) {
		case nil, string, bool, float64, float32, int, int32, int64, uint, uint32, uint64, json.Number:
			// Flat scalar values only. When the event field is an array the
			// dispatcher treats this scalar as a membership test.
		default:
			return nil, "", fmt.Errorf("filters.%s: value must be a string, number, boolean, or null", key)
		}
		normalized[key] = value
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", fmt.Errorf("filters: %w", err)
	}
	if len(b) > maxSubscriptionFiltersJSON {
		return nil, "", fmt.Errorf("filters: encoded value exceeds %d bytes", maxSubscriptionFiltersJSON)
	}
	return normalized, string(b), nil
}

func hydrateSubscriptionFilters(sub *Subscription) {
	if sub == nil || strings.TrimSpace(sub.MatchJSON) == "" {
		return
	}
	var filters map[string]any
	if json.Unmarshal([]byte(sub.MatchJSON), &filters) == nil && len(filters) > 0 {
		sub.Filters = filters
	}
}

// CreateEphemeralAppEventSubscription creates a hidden, one-shot
// app-event subscription. It is used for async app tool results where
// the platform should wake the calling agent/thread when a matching
// app event arrives.
func (s *Store) CreateEphemeralAppEventSubscription(userID, agentID int64, name, slug, description, threadID, projectID string, events []string, matchJSON, waitGroupID string, expiresAt time.Time) (*Subscription, error) {
	id := generateID()
	webhookPath := internalSubscriptionWebhookPath("app-event")
	expires := ""
	if !expiresAt.IsZero() {
		expires = expiresAt.UTC().Format("2006-01-02 15:04:05")
	}
	events = compactSubscriptionEvents(events)
	eventsJSON := ""
	if len(events) > 0 {
		if b, merr := json.Marshal(events); merr == nil {
			eventsJSON = string(b)
		}
	}
	_, err := s.db.Exec(
		`INSERT INTO subscriptions
				(id, user_id, agent_id, connection_id, name, slug, description,
				 webhook_path, encrypted_hmac_secret, thread_id, project_id, events,
				 source, delivery, notify_agent, kind, match_json, wait_group_id,
				 expires_at, delete_on_match)
			 VALUES (?, ?, ?, 0, ?, ?, ?, ?, '', ?, ?, ?, 'app_event', 'app_event',
				 1, 'ephemeral', ?, ?, ?, 1)`,
		id, userID, agentID, name, slug, description, webhookPath,
		threadID, projectID, eventsJSON, matchJSON, waitGroupID, expires,
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{
		ID: id, UserID: userID, AgentID: agentID, Name: name, Slug: slug,
		Description: description, WebhookPath: webhookPath, Enabled: true,
		NotifyAgent: true, ThreadID: threadID, ProjectID: projectID,
		Events: events, Source: "app_event", Delivery: "app_event", Kind: "ephemeral",
		MatchJSON: matchJSON, WaitGroupID: waitGroupID, ExpiresAt: expires,
		DeleteOnMatch: true, CreatedAt: time.Now(),
	}, nil
}

func (s *Store) DeleteEphemeralSubscriptionWaitGroup(waitGroupID string) error {
	waitGroupID = strings.TrimSpace(waitGroupID)
	if waitGroupID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`DELETE FROM subscriptions WHERE kind = 'ephemeral' AND wait_group_id = ?`,
		waitGroupID,
	)
	return err
}

func (s *Store) CleanupExpiredEphemeralSubscriptions() error {
	_, err := s.db.Exec(
		`DELETE FROM subscriptions
		 WHERE kind = 'ephemeral'
		   AND COALESCE(expires_at,'') != ''
		   AND expires_at <= datetime('now')`,
	)
	return err
}

func (s *Store) ListSubscriptions(userID int64, projectID ...string) ([]Subscription, error) {
	var rows *sql.Rows
	var err error
	const cols = `id, agent_id, connection_id, name, slug, description, webhook_path,
		enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
		COALESCE(project_id,''), COALESCE(external_webhook_id,''),
		COALESCE(source,'webhook'), COALESCE(delivery,'webhook'),
		COALESCE(last_run_at,''), COALESCE(next_run_at,''), COALESCE(last_error,''),
		COALESCE(failure_count,0), COALESCE(last_seq_delivered,0),
		COALESCE(match_json,''), created_at`
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			"SELECT "+cols+" FROM subscriptions WHERE user_id = ? AND COALESCE(kind,'user') != 'ephemeral' AND (project_id = ? OR project_id = '') ORDER BY created_at, id", userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			"SELECT "+cols+" FROM subscriptions WHERE user_id = ? AND COALESCE(kind,'user') != 'ephemeral' ORDER BY created_at, id", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		var enabled, notifyAgent int
		var createdAt, eventsJSON string
		if err := rows.Scan(
			&sub.ID, &sub.AgentID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description,
			&sub.WebhookPath, &enabled, &notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
			&sub.ExternalWebhookID, &sub.Source, &sub.Delivery,
			&sub.LastRunAt, &sub.NextRunAt, &sub.LastError, &sub.FailureCount,
			&sub.LastSeqDelivered, &sub.MatchJSON, &createdAt,
		); err != nil {
			return nil, err
		}
		sub.UserID = userID
		sub.Enabled = enabled == 1
		sub.NotifyAgent = notifyAgent == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		if eventsJSON != "" {
			_ = json.Unmarshal([]byte(eventsJSON), &sub.Events)
		}
		hydrateSubscriptionFilters(&sub)
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) ListSubscriptionsForAgent(userID, agentID int64) ([]Subscription, error) {
	const cols = `id, user_id, agent_id, connection_id, name, slug, description,
		webhook_path, enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
		COALESCE(project_id,''), COALESCE(external_webhook_id,''),
		COALESCE(source,'webhook'), COALESCE(delivery,'webhook'),
		COALESCE(last_run_at,''), COALESCE(next_run_at,''), COALESCE(last_error,''),
		COALESCE(failure_count,0), COALESCE(last_seq_delivered,0),
		COALESCE(match_json,''), created_at`
	rows, err := s.db.Query(
		"SELECT "+cols+" FROM subscriptions WHERE user_id = ? AND agent_id = ? AND COALESCE(kind,'user') != 'ephemeral' ORDER BY created_at, id",
		userID, agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		var enabled, notifyAgent int
		var eventsJSON, createdAt string
		if err := rows.Scan(
			&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID,
			&sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath,
			&enabled, &notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
			&sub.ExternalWebhookID, &sub.Source, &sub.Delivery,
			&sub.LastRunAt, &sub.NextRunAt, &sub.LastError, &sub.FailureCount,
			&sub.LastSeqDelivered, &sub.MatchJSON, &createdAt,
		); err != nil {
			return nil, err
		}
		sub.Enabled = enabled == 1
		sub.NotifyAgent = notifyAgent == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		if eventsJSON != "" {
			_ = json.Unmarshal([]byte(eventsJSON), &sub.Events)
		}
		hydrateSubscriptionFilters(&sub)
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) GetSubscription(userID int64, id string) (*Subscription, error) {
	var sub Subscription
	var enabled, notifyAgent int
	var createdAt, eventsJSON string
	err := s.db.QueryRow(
		`SELECT id, agent_id, connection_id, name, slug, description,
			webhook_path, enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
			COALESCE(project_id,''), COALESCE(external_webhook_id,''),
			COALESCE(source,'webhook'), COALESCE(delivery,'webhook'),
			COALESCE(last_run_at,''), COALESCE(next_run_at,''), COALESCE(last_error,''),
			COALESCE(failure_count,0), COALESCE(last_seq_delivered,0),
			COALESCE(match_json,''), created_at
		 FROM subscriptions WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(
		&sub.ID, &sub.AgentID, &sub.ConnectionID, &sub.Name,
		&sub.Slug, &sub.Description, &sub.WebhookPath, &enabled,
		&notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
		&sub.ExternalWebhookID, &sub.Source, &sub.Delivery,
		&sub.LastRunAt, &sub.NextRunAt, &sub.LastError, &sub.FailureCount,
		&sub.LastSeqDelivered, &sub.MatchJSON, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	sub.UserID = userID
	sub.Enabled = enabled == 1
	sub.NotifyAgent = notifyAgent == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	if eventsJSON != "" {
		_ = json.Unmarshal([]byte(eventsJSON), &sub.Events)
	}
	hydrateSubscriptionFilters(&sub)
	return &sub, nil
}

func (s *Store) GetSubscriptionByPath(webhookPath string) (*Subscription, string, error) {
	if isInternalSubscriptionWebhookPath(webhookPath) {
		return nil, "", sql.ErrNoRows
	}
	var sub Subscription
	var enabled int
	var encSecret, createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, agent_id, connection_id, name, slug, description, webhook_path, encrypted_hmac_secret, enabled, COALESCE(thread_id,''), created_at FROM subscriptions WHERE webhook_path = ?",
		webhookPath,
	).Scan(&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &encSecret, &enabled, &sub.ThreadID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	sub.Enabled = enabled == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	return &sub, encSecret, nil
}

func (s *Store) DeleteSubscription(userID int64, id string) error {
	_, err := s.db.Exec("DELETE FROM subscriptions WHERE id = ? AND user_id = ?", id, userID)
	return err
}

func (s *Store) SetSubscriptionExternalID(id, externalID string) {
	s.db.Exec("UPDATE subscriptions SET external_webhook_id = ? WHERE id = ?", externalID, id)
}

func (s *Store) GetSubscriptionExternalID(id string) string {
	var extID string
	s.db.QueryRow("SELECT COALESCE(external_webhook_id,'') FROM subscriptions WHERE id = ?", id).Scan(&extID)
	return extID
}

// GetSubscriptionByExternalID looks up the subscription row whose upstream
// webhook id matches the given value.
func (s *Store) GetSubscriptionByExternalID(userID int64, externalID string) (*Subscription, error) {
	const cols = "id, user_id, agent_id, connection_id, name, slug, description, webhook_path, enabled, COALESCE(thread_id,''), COALESCE(events,''), COALESCE(project_id,''), created_at"
	var (
		sub        Subscription
		enabled    int
		createdAt  string
		eventsJSON string
	)
	var err error
	if userID > 0 {
		err = s.db.QueryRow(
			"SELECT "+cols+" FROM subscriptions WHERE external_webhook_id = ? AND user_id = ?",
			externalID, userID,
		).Scan(&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &eventsJSON, &sub.ProjectID, &createdAt)
	} else {
		err = s.db.QueryRow(
			"SELECT "+cols+" FROM subscriptions WHERE external_webhook_id = ?",
			externalID,
		).Scan(&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &eventsJSON, &sub.ProjectID, &createdAt)
	}
	if err != nil {
		return nil, err
	}
	sub.Enabled = enabled == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	if eventsJSON != "" {
		json.Unmarshal([]byte(eventsJSON), &sub.Events)
	}
	sub.ExternalWebhookID = externalID
	return &sub, nil
}

func (s *Store) SetSubscriptionEnabled(userID int64, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec("UPDATE subscriptions SET enabled = ? WHERE id = ? AND user_id = ?", v, id, userID)
	return err
}

func (s *Store) SetSubscriptionNotifyAgent(userID int64, id string, notifyAgent bool) error {
	_, err := s.db.Exec("UPDATE subscriptions SET notify_agent = ? WHERE id = ? AND user_id = ?", boolToInt(notifyAgent), id, userID)
	return err
}

func (s *Store) ListDuePollSubscriptions(now time.Time, limit int) ([]*Subscription, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT id, user_id, agent_id, connection_id, name, slug, description,
			webhook_path, enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
			COALESCE(project_id,''), COALESCE(external_webhook_id,''),
			COALESCE(source,'webhook'), COALESCE(delivery,'webhook'),
			COALESCE(poll_config_json,''), COALESCE(poll_state_json,''),
			COALESCE(last_run_at,''), COALESCE(next_run_at,''), COALESCE(last_error,''),
			COALESCE(failure_count,0), COALESCE(last_seq_delivered,0), created_at
		 FROM subscriptions
		 WHERE enabled = 1
		   AND COALESCE(delivery,'webhook') = 'poll'
		   AND (next_run_at IS NULL OR next_run_at = '' OR next_run_at <= ?)
		 ORDER BY COALESCE(next_run_at, created_at), id
		 LIMIT ?`,
		formatPollTime(now), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*Subscription
	for rows.Next() {
		sub := &Subscription{}
		var enabled, notifyAgent int
		var eventsJSON, createdAt string
		if err := rows.Scan(
			&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID,
			&sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath,
			&enabled, &notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
			&sub.ExternalWebhookID, &sub.Source, &sub.Delivery,
			&sub.PollConfigJSON, &sub.PollStateJSON,
			&sub.LastRunAt, &sub.NextRunAt, &sub.LastError, &sub.FailureCount,
			&sub.LastSeqDelivered, &createdAt,
		); err != nil {
			return nil, err
		}
		sub.Enabled = enabled == 1
		sub.NotifyAgent = notifyAgent == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		if eventsJSON != "" {
			_ = json.Unmarshal([]byte(eventsJSON), &sub.Events)
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) UpdatePollSubscriptionSuccess(id, stateJSON string, lastRunAt, nextRunAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE subscriptions
		 SET poll_state_json = ?, last_run_at = ?, next_run_at = ?, last_error = '', failure_count = 0
		 WHERE id = ?`,
		stateJSON, formatPollTime(lastRunAt), formatPollTime(nextRunAt), id,
	)
	return err
}

func (s *Store) UpdatePollSubscriptionFailure(id, errMsg string, lastRunAt, nextRunAt time.Time, failureCount int) error {
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	_, err := s.db.Exec(
		`UPDATE subscriptions
		 SET last_run_at = ?, next_run_at = ?, last_error = ?, failure_count = ?
		 WHERE id = ?`,
		formatPollTime(lastRunAt), formatPollTime(nextRunAt), errMsg, failureCount, id,
	)
	return err
}

// --- HMAC verification ---

func verifyHMAC(body []byte, signature string, secret string) bool {
	if secret == "" || signature == "" {
		return true // no HMAC configured
	}
	// Strip "sha256=" prefix
	sig := strings.TrimPrefix(signature, "sha256=")
	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}

// verifyStandardWebhook validates a payload signed per the Standard
// Standard Webhooks signature format:
//
//	webhook-id:        msg_xxx
//	webhook-timestamp: 1234567890  (unix seconds)
//	webhook-signature: v1,<base64(HMAC_SHA256(secret, id "." ts "." body))>
//
// webhook-signature may contain multiple space-separated versions; we
// accept if any v1 entry matches. Secret may be base64-encoded or raw
// bytes — we try both to tolerate both conventions.
func verifyStandardWebhook(body []byte, msgID, msgTS, sigHeader, secret string) bool {
	if secret == "" {
		return true
	}
	if msgID == "" || msgTS == "" || sigHeader == "" {
		return false
	}
	toSign := msgID + "." + msgTS + "." + string(body)
	// Try secret as raw bytes and as base64; Standard Webhooks typically
	// uses "whsec_<base64>" but we tolerate either form.
	secretBytes := []byte(secret)
	if stripped := strings.TrimPrefix(secret, "whsec_"); stripped != secret {
		if decoded, err := base64.StdEncoding.DecodeString(stripped); err == nil {
			secretBytes = decoded
		}
	}
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(toSign))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	// sigHeader may contain multiple versions: "v1,sig1 v1,sig2"
	for _, entry := range strings.Fields(sigHeader) {
		parts := strings.SplitN(entry, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		if hmac.Equal([]byte(parts[1]), []byte(expected)) {
			return true
		}
	}
	return false
}

// --- HTTP Handlers ---

// POST /webhooks/:token — unified webhook ingress.
//
// The opaque token matches subscriptions.webhook_path. Local catalog
// integrations that self-register webhooks validate the request with the
// per-subscription secret before delivery.
//
// Neither case uses authenticated sessions — these are public endpoints
// upstream services POST into, with HMAC as the only auth layer. The
// token in the URL is opaque random bytes (16 bytes / 32 hex chars),
// not a guessable id, so URL enumeration is not a concern.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log.Printf("[WEBHOOK-IN] %s remote=%s ua=%q content-type=%q content-length=%s",
		r.Method, r.RemoteAddr, r.Header.Get("User-Agent"),
		r.Header.Get("Content-Type"), r.Header.Get("Content-Length"))

	if r.Method != http.MethodPost {
		log.Printf("[WEBHOOK-IN] rejecting %s — POST only", r.Method)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimPrefix(r.URL.Path, "/webhooks/")
	if token == "" {
		log.Printf("[WEBHOOK-IN] empty token on path %q", r.URL.Path)
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	tokenRef := shortSecretRef(token)
	log.Printf("[WEBHOOK-IN] token_ref=%s len=%d", tokenRef, len(token))

	// Dispatch 1: subscription-backed webhook. Try this first because
	// it's the common case for local templates and has a cheaper
	// lookup.
	sub, encSecret, err := s.store.GetSubscriptionByPath(token)
	if err == nil && sub != nil {
		log.Printf("[WEBHOOK-IN] matched subscription id=%s name=%q slug=%q enabled=%v", sub.ID, sub.Name, sub.Slug, sub.Enabled)
		s.handleSubscriptionWebhook(w, r, sub, encSecret)
		return
	}
	log.Printf("[WEBHOOK-IN] no subscription row for token_ref=%s err=%v", tokenRef, err)

	log.Printf("[WEBHOOK-IN] 404 token_ref=%s — no subscription row matched", tokenRef)
	http.Error(w, "unknown webhook token", http.StatusNotFound)
}

// handleSubscriptionWebhook is the delivery path for /webhooks/<token>
// when the token matches a subscription row. Factored out of the
// top-level handler so validation and delivery remain isolated.
func (s *Server) handleSubscriptionWebhook(w http.ResponseWriter, r *http.Request, sub *Subscription, encSecret string) {
	if sub == nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	if !sub.Enabled {
		log.Printf("[WEBHOOK] sub %s disabled — 403", sub.ID)
		http.Error(w, "subscription disabled", http.StatusForbidden)
		return
	}

	// Read body
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		log.Printf("[WEBHOOK] sub %s body read error: %v", sub.ID, err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	log.Printf("[WEBHOOK] sub %s received body len=%d", sub.ID, len(body))

	// Verify HMAC if configured
	if encSecret != "" {
		secret, err := Decrypt(s.secret, encSecret)
		if err != nil || secret == "" {
			log.Printf("[WEBHOOK] sub %s: HMAC credential unavailable: %v", sub.ID, err)
			http.Error(w, "webhook verification unavailable", http.StatusServiceUnavailable)
			return
		}
		{
			sig := r.Header.Get("x-hub-signature-256")
			if sig == "" {
				sig = r.Header.Get("x-signature-256")
			}
			if sig == "" {
				sig = r.Header.Get("x-webhook-signature")
			}
			log.Printf("[WEBHOOK] sub %s HMAC check — sig header present=%v", sub.ID, sig != "")
			if !verifyHMAC(body, sig, secret) {
				log.Printf("[WEBHOOK] sub %s HMAC verification failed", sub.ID)
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
			log.Printf("[WEBHOOK] sub %s HMAC verified ok", sub.ID)
		}
	} else {
		log.Printf("[WEBHOOK] sub %s has no HMAC secret — skipping verification", sub.ID)
	}

	// Find the target instance
	if sub.AgentID == 0 {
		log.Printf("[WEBHOOK] sub %s: no instance configured", sub.ID)
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetAgent(sub.UserID, sub.AgentID)
	if err != nil {
		log.Printf("[WEBHOOK] sub %s: instance %d not found: %v", sub.ID, sub.AgentID, err)
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	port := s.agents.GetPort(inst.ID)
	if port == 0 {
		log.Printf("[WEBHOOK] sub %s: instance %d not running", sub.ID, inst.ID)
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[WEBHOOK] sub %s: delivering to instance %d port %d", sub.ID, inst.ID, port)

	// Format and inject the event into core
	var payload any
	json.Unmarshal(body, &payload)
	payloadStr := string(body)
	if len(payloadStr) > 2000 {
		payloadStr = payloadStr[:2000] + "...[truncated]"
	}

	eventMsg := fmt.Sprintf("[webhook:%s] %s", sub.Slug, payloadStr)

	// POST to core's /event endpoint with optional thread targeting
	eventPayload := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		eventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(eventPayload)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if ck := s.agents.GetCoreAPIKey(inst.ID); ck != "" {
		req.Header.Set("Authorization", "Bearer "+ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[WEBHOOK] deliver error: %v", err)
		http.Error(w, "failed to deliver", http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[WEBHOOK] core rejected %d: %s", resp.StatusCode, string(respBody))
		http.Error(w, fmt.Sprintf("core rejected: %d %s", resp.StatusCode, string(respBody)), http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]string{"status": "delivered", "subscription": sub.ID})
}

// POST /subscriptions
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		AgentID      int64    `json:"agent_id"`    // Phase 2 canonical
		InstanceID   int64    `json:"instance_id"` // legacy alias
		ConnectionID int64    `json:"connection_id"`
		Name         string   `json:"name"`
		Slug         string   `json:"slug"`
		Description  string   `json:"description"`
		HMACSecret   string   `json:"hmac_secret"`
		Events       []string `json:"events"`
		ThreadID     string   `json:"thread_id"`
		ProjectID    string   `json:"project_id"`
		NotifyAgent  bool     `json:"notify_agent"`
		// Source: 'webhook' (default) or 'app_event'. The two paths
		// share this handler so the dashboard's create form can
		// switch between them without learning two URLs.
		Source          string         `json:"source"`
		Filters         map[string]any `json:"filters"`
		IntervalSeconds int            `json:"interval_seconds"`
		PollInput       map[string]any `json:"poll_input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 {
		body.AgentID = body.InstanceID
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	// app_event subscriptions: no public webhook path, no HMAC, no
	// upstream registration. The slug carries '<app>:<topic_pattern>'
	// for compatibility. When events[] is present, the dispatcher uses
	// that list as the topic matcher so one row can represent multiple
	// app events.
	if body.Source == "app_event" {
		if body.Slug == "" || !strings.Contains(body.Slug, ":") {
			http.Error(w, "slug must be '<app>:<topic_pattern>' for app_event subscriptions", http.StatusBadRequest)
			return
		}
		events := compactSubscriptionEvents(body.Events)
		filters, _, err := normalizeSubscriptionFilters(body.Filters)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sub, err := s.store.CreateAppEventSubscriptionWithFilters(
			userID, body.AgentID, body.Name, body.Slug, body.Description,
			body.ThreadID, body.ProjectID, events, filters, body.NotifyAgent,
		)
		if err != nil {
			http.Error(w, "failed to create: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[SUB-CREATE] sub=%s source=app_event slug=%q events=%v filters=%d agent=%d project=%q",
			sub.ID, body.Slug, events, len(filters), body.AgentID, body.ProjectID)
		if s.appEventDispatcher != nil {
			if err := s.appEventDispatcher.Reconcile(); err != nil {
				log.Printf("[SUB-CREATE] dispatcher reconcile after create: %v", err)
			}
		}
		s.notifySubscriptionCreated(sub)
		writeJSON(w, sub)
		return
	}

	if body.ConnectionID > 0 {
		conn, _, err := s.store.GetConnection(userID, body.ConnectionID)
		if err != nil || conn == nil {
			http.Error(w, "connection not found", http.StatusBadRequest)
			return
		}
		if app := s.catalog.Get(conn.AppSlug); app != nil {
			cfg, event, err := buildStoredPollConfig(app, body.Events, body.IntervalSeconds, body.PollInput)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if cfg != nil && event != nil {
				if len(body.Events) == 0 {
					body.Events = []string{event.Name}
				}
				if body.Slug == "" {
					body.Slug = conn.AppSlug
				}
				if body.Description == "" {
					body.Description = event.Description
				}
				cfgJSON, _ := json.Marshal(cfg)
				nextRun := time.Now().UTC()
				sub, err := s.store.CreatePollSubscription(userID, body.AgentID, body.ConnectionID, body.Name, body.Slug, body.Description, body.ThreadID, body.ProjectID, body.Events, string(cfgJSON), nextRun, body.NotifyAgent)
				if err != nil {
					http.Error(w, "failed to create poll subscription: "+err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("[SUB-CREATE] sub=%s delivery=poll app=%s event=%s tool=%s interval=%ds",
					sub.ID, app.Slug, event.Name, cfg.Tool, cfg.IntervalSeconds)
				if s.pollingDispatcher != nil {
					s.pollingDispatcher.Wake()
				}
				s.notifySubscriptionCreated(sub)
				writeJSON(w, map[string]any{
					"subscription":    sub,
					"delivery":        "poll",
					"events":          body.Events,
					"auto_registered": false,
				})
				return
			}
		}
	}

	// Generate unique webhook path
	webhookPath := generateToken(16)

	// Auto-generate an HMAC secret when the caller didn't supply one —
	// we want HMAC validation to always be on. The plaintext is passed
	// to the upstream service during auto-registration so both sides
	// share the same secret; the encrypted copy is stored locally.
	if body.HMACSecret == "" {
		body.HMACSecret = generateToken(32)
	}
	encSecret, err := Encrypt(s.secret, body.HMACSecret)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	sub, err := s.store.CreateSubscription(userID, body.AgentID, body.ConnectionID, body.Name, body.Slug, body.Description, webhookPath, encSecret, body.ThreadID, body.ProjectID, body.Events, body.NotifyAgent)
	if err != nil {
		http.Error(w, "failed to create", http.StatusInternalServerError)
		return
	}

	webhookURL := s.webhookURL(webhookPath)
	log.Printf("[SUB-CREATE] sub=%s name=%q slug=%q conn=%d agent=%d webhook_ref=%s events=%v",
		sub.ID, body.Name, body.Slug, body.ConnectionID, body.AgentID, shortSecretRef(webhookPath), body.Events)

	// Auto-register webhook with the external service if it has registration config
	var autoRegistered bool
	if body.ConnectionID > 0 {
		conn, encCreds, err := s.store.GetConnection(userID, body.ConnectionID)
		if err != nil || conn == nil {
			log.Printf("[SUB-CREATE] skip auto-reg: connection %d lookup failed: err=%v conn=%v", body.ConnectionID, err, conn)
		} else {
			log.Printf("[SUB-CREATE] connection %d → app=%s name=%q", conn.ID, conn.AppSlug, conn.Name)
			app := s.catalog.Get(conn.AppSlug)
			switch {
			case app == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %q not found in catalog", conn.AppSlug)
			case app.Webhooks == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %s has no webhooks config", conn.AppSlug)
			case app.Webhooks.Registration == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %s has no webhooks.registration config", conn.AppSlug)
			case app.Webhooks.Registration.ManualSetup != "":
				log.Printf("[SUB-CREATE] skip auto-reg: app %s requires manual setup (%s)", conn.AppSlug, app.Webhooks.Registration.ManualSetup)
			default:
				plain, derr := Decrypt(s.secret, encCreds)
				if derr != nil {
					log.Printf("[SUB-CREATE] skip auto-reg: decrypt creds failed: %v", derr)
				} else {
					reg := app.Webhooks.Registration

					headers := map[string]string{"Content-Type": "application/json"}
					for k, v := range app.Auth.Headers {
						headers[k] = resolveCredTemplate(v, plain)
					}

					reqBody := map[string]any{}
					if reg.Extra != nil {
						for k, v := range reg.Extra {
							reqBody[k] = v
						}
					}
					setField(reqBody, reg.URLField, webhookURL)
					if reg.SecretField != "" && body.HMACSecret != "" {
						setField(reqBody, reg.SecretField, body.HMACSecret)
					}
					if reg.EventsField != "" && len(body.Events) > 0 {
						setField(reqBody, reg.EventsField, body.Events)
					}

					regURL := strings.TrimSuffix(app.BaseURL, "/") + reg.Path
					regBody, _ := json.Marshal(reqBody)

					// Redact auth header values for the log line
					logHeaders := make(map[string]string, len(headers))
					for k, v := range headers {
						if k == "Content-Type" {
							logHeaders[k] = v
						} else if len(v) > 8 {
							logHeaders[k] = v[:4] + "…" + v[len(v)-4:]
						} else {
							logHeaders[k] = "***"
						}
					}
					log.Printf("[SUB-CREATE] → %s %s headers=%v body_bytes=%d body_fields=%v", reg.Method, regURL, logHeaders, len(regBody), sortedAnyMapKeys(reqBody))

					req, rerr := http.NewRequest(reg.Method, regURL, strings.NewReader(string(regBody)))
					if rerr != nil {
						log.Printf("[SUB-CREATE] build request failed: %v", rerr)
					} else {
						for k, v := range headers {
							req.Header.Set(k, v)
						}
						resp, herr := http.DefaultClient.Do(req)
						if herr != nil {
							log.Printf("[SUB-CREATE] HTTP error: %v", herr)
						} else {
							respBody, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							log.Printf("[SUB-CREATE] ← %d body_bytes=%d", resp.StatusCode, len(respBody))
							if resp.StatusCode >= 200 && resp.StatusCode < 300 {
								autoRegistered = true
								if reg.IDField != "" {
									var respData map[string]any
									if json.Unmarshal(respBody, &respData) == nil {
										extID := extractJSONPath(respData, reg.IDField)
										log.Printf("[SUB-CREATE] extracted external_id=%q via path %q", extID, reg.IDField)
										if extID != "" {
											s.store.SetSubscriptionExternalID(sub.ID, extID)
										}
									} else {
										log.Printf("[SUB-CREATE] response body is not JSON, cannot extract id")
									}
								}
							}
						}
					}
				}
			}
		}
	} else {
		log.Printf("[SUB-CREATE] skip auto-reg: connection_id=0")
	}
	log.Printf("[SUB-CREATE] done sub=%s auto_registered=%v", sub.ID, autoRegistered)
	s.notifySubscriptionCreated(sub)

	writeJSON(w, map[string]any{
		"subscription":    sub,
		"webhook_url":     webhookURL,
		"auto_registered": autoRegistered,
	})
}

// GET /subscriptions
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	subs, err := s.store.ListSubscriptions(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if subs == nil {
		subs = []Subscription{}
	}

	// Enrich with webhook URLs
	type subWithURL struct {
		Subscription
		WebhookURL string `json:"webhook_url"`
	}
	var enriched []subWithURL
	for _, sub := range subs {
		webhookURL := s.webhookURL(sub.WebhookPath)
		if sub.Delivery == "poll" {
			webhookURL = ""
			sub.WebhookPath = ""
		}
		enriched = append(enriched, subWithURL{
			Subscription: sub,
			WebhookURL:   webhookURL,
		})
	}
	writeJSON(w, enriched)
}

// DELETE /subscriptions/:id
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	id := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	if strings.HasSuffix(id, "/enable") || strings.HasSuffix(id, "/disable") {
		return // handled elsewhere
	}
	subBeforeDelete, _ := s.store.GetSubscription(userID, id)

	// Unregister from external service if we have an external webhook ID
	extID := s.store.GetSubscriptionExternalID(id)
	if extID != "" {
		sub, _ := s.store.GetSubscription(userID, id)
		if sub != nil && sub.ConnectionID > 0 {
			conn, encCreds, err := s.store.GetConnection(userID, sub.ConnectionID)
			if err == nil && conn != nil {
				app := s.catalog.Get(conn.AppSlug)
				if app != nil && app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.DeletePath != "" {
					plain, err := Decrypt(s.secret, encCreds)
					if err == nil {
						reg := app.Webhooks.Registration
						deletePath := strings.ReplaceAll(reg.DeletePath, "{id}", extID)
						deleteURL := strings.TrimSuffix(app.BaseURL, "/") + deletePath

						headers := map[string]string{}
						for k, v := range app.Auth.Headers {
							headers[k] = resolveCredTemplate(v, plain)
						}

						method := reg.DeleteMethod
						if method == "" {
							method = "DELETE"
						}

						req, err := http.NewRequest(method, deleteURL, nil)
						if err == nil {
							for k, v := range headers {
								req.Header.Set(k, v)
							}
							resp, err := http.DefaultClient.Do(req)
							if err == nil {
								resp.Body.Close()
							}
						}
					}
				}
			}
		}
	}

	if err := s.store.DeleteSubscription(userID, id); err != nil {
		http.Error(w, "failed to delete", http.StatusInternalServerError)
		return
	}
	// Reconcile the bus dispatcher: if the deleted row was the last
	// subscriber of its (app, project) lane, the lane goroutine
	// shuts down. No-op when the row was a webhook subscription.
	if s.appEventDispatcher != nil {
		if err := s.appEventDispatcher.Reconcile(); err != nil {
			log.Printf("[SUB-DELETE] dispatcher reconcile after delete: %v", err)
		}
	}
	s.notifySubscriptionDeleted(subBeforeDelete)
	writeJSON(w, map[string]string{"status": "deleted"})
}

// POST /subscriptions/:id/notify-agent
func (s *Server) handleSetSubscriptionNotifyAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	id := strings.TrimSuffix(path, "/notify-agent")
	if id == "" || id == path {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	var body struct {
		NotifyAgent bool `json:"notify_agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.store.SetSubscriptionNotifyAgent(userID, id, body.NotifyAgent); err != nil {
		http.Error(w, "failed to update", http.StatusInternalServerError)
		return
	}
	sub, err := s.store.GetSubscription(userID, id)
	if err == nil && sub != nil && body.NotifyAgent {
		s.notifySubscriptionChange("Subscription agent notifications enabled", sub)
	}
	writeJSON(w, map[string]any{"status": "ok", "notify_agent": body.NotifyAgent})
}

// unregisterUpstreamWebhook calls the app's delete_path upstream to remove
// an external webhook subscription. Best-effort — network errors and 4xx/5xx
// responses are logged but do not fail the caller, because the local DB row
// is the authoritative source of truth for us.
func (s *Server) unregisterUpstreamWebhook(conn *Connection, app *AppTemplate, externalID string) {
	if conn == nil || app == nil || app.Webhooks == nil || app.Webhooks.Registration == nil {
		return
	}
	reg := app.Webhooks.Registration
	if reg.DeletePath == "" || externalID == "" {
		return
	}
	_, encCreds, err := s.store.GetConnection(conn.UserID, conn.ID)
	if err != nil {
		return
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return
	}
	deletePath := strings.ReplaceAll(reg.DeletePath, "{id}", externalID)
	deleteURL := strings.TrimSuffix(app.BaseURL, "/") + deletePath
	headers := map[string]string{}
	for k, v := range app.Auth.Headers {
		headers[k] = resolveCredTemplate(v, plain)
	}
	method := reg.DeleteMethod
	if method == "" {
		method = "DELETE"
	}
	req, err := http.NewRequest(method, deleteURL, nil)
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SUB-UNREG] upstream delete error: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[SUB-UNREG] upstream delete %s → %d", deleteURL, resp.StatusCode)
}

// POST /subscriptions/:id/enable or /disable
func (s *Server) handleToggleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")

	var id string
	var enable bool
	if strings.HasSuffix(path, "/enable") {
		id = strings.TrimSuffix(path, "/enable")
		enable = true
	} else if strings.HasSuffix(path, "/disable") {
		id = strings.TrimSuffix(path, "/disable")
		enable = false
	} else {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if err := s.store.SetSubscriptionEnabled(userID, id, enable); err != nil {
		http.Error(w, "failed to update", http.StatusInternalServerError)
		return
	}
	// Reconcile the bridge dispatcher so flipping enabled/disabled on
	// an app_event row immediately stops/starts delivery (or fully
	// shuts down the lane when this was the last enabled row in it).
	// No-op for webhook rows.
	if s.appEventDispatcher != nil {
		if err := s.appEventDispatcher.Reconcile(); err != nil {
			log.Printf("[SUB-TOGGLE] dispatcher reconcile: %v", err)
		}
	}

	var toggledSub *Subscription
	if sub, err := s.store.GetSubscription(userID, id); err == nil && sub != nil {
		toggledSub = sub
	}
	if toggledSub != nil {
		if enable {
			s.notifySubscriptionEnabled(toggledSub)
		} else {
			s.notifySubscriptionDisabled(toggledSub)
		}
	}

	writeJSON(w, map[string]any{"status": "ok", "enabled": enable})
}

// POST /subscriptions/:id/test — send a fake test event to the instance
func (s *Server) handleTestSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	id := strings.TrimSuffix(path, "/test")
	log.Printf("[SUB-TEST] start user=%d sub=%s", userID, id)

	sub, err := s.store.GetSubscription(userID, id)
	if err != nil {
		log.Printf("[SUB-TEST] subscription %s not found: %v", id, err)
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	log.Printf("[SUB-TEST] sub=%s name=%q slug=%q agent=%d thread=%q", sub.ID, sub.Name, sub.Slug, sub.AgentID, sub.ThreadID)

	// Parse optional body: { "event": "content.created", "payload": { ... } }
	var reqBody struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	json.NewDecoder(r.Body).Decode(&reqBody) // ignore errors — all fields optional
	log.Printf("[SUB-TEST] request body event=%q custom_payload=%v", reqBody.Event, reqBody.Payload != nil)

	// App-event tests run through the same topic + payload matchers as live
	// delivery. A filtered test is useful even when the target agent is stopped,
	// and must never wake it or move the real subscription cursor.
	appName, legacyPattern, isAppEvent := splitAppEventSlug(sub.Slug)
	eventType := strings.TrimSpace(reqBody.Event)
	if eventType == "" && len(sub.Events) > 0 {
		eventType = sub.Events[0]
	}
	if eventType == "" && isAppEvent && legacyPattern != "*" && !strings.HasSuffix(legacyPattern, ".*") {
		eventType = legacyPattern
	}
	if eventType == "" {
		eventType = "test.event"
	}
	appPayload := reqBody.Payload
	if appPayload == nil {
		appPayload = map[string]any{
			"message":           "This is a test event.",
			"subscription_id":   sub.ID,
			"subscription_name": sub.Name,
		}
	}
	appPayloadBytes, _ := json.Marshal(appPayload)
	if sub.Source == "app_event" {
		matched := isAppEvent &&
			appEventSubscriptionTopicMatches(sub, legacyPattern, eventType) &&
			subscriptionPayloadMatches(sub, appPayloadBytes)
		if !matched {
			writeJSON(w, map[string]any{
				"status":  "filtered",
				"matched": false,
				"event":   eventType,
				"payload": appPayload,
			})
			return
		}
	}

	if sub.AgentID == 0 {
		log.Printf("[SUB-TEST] sub=%s has no agent_id configured", sub.ID)
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetAgent(sub.UserID, sub.AgentID)
	if err != nil {
		log.Printf("[SUB-TEST] instance %d not found for user %d: %v", sub.AgentID, sub.UserID, err)
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[SUB-TEST] instance %d → name=%q status=%q", inst.ID, inst.Name, inst.Status)
	testPort := s.agents.GetPort(inst.ID)
	if testPort == 0 {
		log.Printf("[SUB-TEST] instance %d has no local port — core not running or not tracked", inst.ID)
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[SUB-TEST] instance %d local port=%d", inst.ID, testPort)

	// Webhook subscriptions retain their catalog fallback.
	if reqBody.Event == "" && sub.Source != "app_event" {
		if app := s.catalog.Get(sub.Slug); app != nil && app.Webhooks != nil && len(app.Webhooks.Events) > 0 {
			eventType = app.Webhooks.Events[0].Name
		}
	}

	testPayload := map[string]any{
		"_test":     true,
		"event":     eventType,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	// Use provided payload or default
	if reqBody.Payload != nil {
		testPayload["data"] = reqBody.Payload
	} else {
		testPayload["data"] = map[string]any{
			"message":           "This is a test event. Your subscription is working correctly.",
			"subscription_id":   sub.ID,
			"subscription_name": sub.Name,
		}
	}

	payloadBytes, _ := json.Marshal(testPayload)
	eventMsg := fmt.Sprintf("[webhook:%s] %s", sub.Slug, string(payloadBytes))
	responsePayload := any(testPayload)
	if sub.Source == "app_event" {
		eventMsg = fmt.Sprintf("[app:%s:%s] %s", appName, eventType, string(appPayloadBytes))
		responsePayload = appPayload
	}
	testEventPayload := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		testEventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(testEventPayload)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/event", testPort)
	coreKey := s.agents.GetCoreAPIKey(inst.ID)
	log.Printf("[SUB-TEST] → POST %s thread=%q msg_len=%d has_auth=%v", targetURL, sub.ThreadID, len(eventMsg), coreKey != "")

	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SUB-TEST] HTTP error posting to core: %v", err)
		http.Error(w, "failed to deliver test event: "+err.Error(), http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("[SUB-TEST] ← core %d %s", resp.StatusCode, string(respBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("core rejected test event: %d %s", resp.StatusCode, string(respBody)), http.StatusBadGateway)
		return
	}

	log.Printf("[SUB-TEST] delivered sub=%s event=%q", sub.ID, eventType)
	writeJSON(w, map[string]any{
		"status":  "delivered",
		"matched": true,
		"event":   eventType,
		"payload": responsePayload,
	})
}

// resolveCredTemplate replaces {{key}} placeholders with credential values
func resolveCredTemplate(template string, credsJSON string) string {
	var creds map[string]string
	json.Unmarshal([]byte(credsJSON), &creds)
	result := template
	for k, v := range creds {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result
}

// extractJSONPath extracts a value at a dot-notation path from a map (e.g. "data.id")
func extractJSONPath(obj map[string]any, path string) string {
	parts := strings.Split(path, ".")
	var current any = obj
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[part]
	}
	if current == nil {
		return ""
	}
	return fmt.Sprintf("%v", current)
}

func (s *Server) webhookURL(path string) string {
	return s.publicBaseURL() + "/webhooks/" + path
}

// setField sets a value at a dot-notation path in a map
func setField(obj map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := obj
	for i := 0; i < len(parts)-1; i++ {
		if _, ok := current[parts[i]]; !ok {
			current[parts[i]] = map[string]any{}
		}
		current = current[parts[i]].(map[string]any)
	}
	current[parts[len(parts)-1]] = value
}
