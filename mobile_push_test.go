package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type mobilePushRelayRecorder struct {
	mu             sync.Mutex
	registerCalls  int
	deliveryCalls  int
	providerToken  string
	platform       string
	deliveryGrant  string
	deliveryBodies []map[string]any
	deliveryStatus int
}

func (r *mobilePushRelayRecorder) handler(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/v1/devices/register":
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.registerCalls++
		r.providerToken, _ = body["provider_token"].(string)
		r.platform, _ = body["platform"].(string)
		r.mu.Unlock()
		writeJSON(w, map[string]any{
			"device": map[string]any{"id": "device-123"},
			"grant":  "push_test_grant",
			"expires_at": time.Now().UTC().
				Add(24 * time.Hour).
				Format(time.RFC3339Nano),
		})
	case req.Method == http.MethodPost && req.URL.Path == "/v1/deliveries":
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.deliveryCalls++
		r.deliveryGrant = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		r.deliveryBodies = append(r.deliveryBodies, body)
		status := r.deliveryStatus
		r.mu.Unlock()
		if status != 0 {
			writeJSONStatus(w, status, map[string]any{"error": "relay temporarily unavailable"})
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{
			"id":     "delivery-123",
			"status": "sent",
		})
	case req.Method == http.MethodPost && req.URL.Path == "/v1/devices/device-123/test":
		writeJSONStatus(w, http.StatusCreated, map[string]any{
			"id":     "test-123",
			"status": "sent",
		})
	case req.Method == http.MethodDelete && req.URL.Path == "/v1/grants/current":
		writeJSON(w, map[string]any{"revoked": true})
	default:
		http.NotFound(w, req)
	}
}

func TestMobilePushAndroidRegistrationIsForwardedToRelay(t *testing.T) {
	s, userID := newMobilePushTestServer(t)
	relay := &mobilePushRelayRecorder{}
	relayServer := httptest.NewServer(http.HandlerFunc(relay.handler))
	t.Cleanup(relayServer.Close)
	t.Setenv("APTEVA_PUSH_RELAY_URL", relayServer.URL)

	body, _ := json.Marshal(map[string]any{
		"installation_id": "android-installation-123",
		"provider_token":  "firebase-installation-id",
		"platform":        "android",
		"bundle_id":       "ai.apteva.mobile",
		"app_version":     "1.0",
		"device_name":     "Test Android",
	})
	req := httptest.NewRequest(http.MethodPost, "/mobile/push/subscriptions", bytes.NewReader(body))
	req.Header.Set("X-User-ID", strconv.FormatInt(userID, 10))
	rec := httptest.NewRecorder()
	s.handleMobilePushSubscriptions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	subscription, err := s.mobilePushSubscriptionByInstallation("android-installation-123")
	if err != nil {
		t.Fatal(err)
	}
	if subscription.Platform != "android" || subscription.Environment != "production" {
		t.Fatalf("subscription=%+v", subscription)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.platform != "android" {
		t.Fatalf("relay platform=%q, want android", relay.platform)
	}
}

func newMobilePushTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	user, err := s.store.CreateUser("mobile@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "Mobile Agent", "", "autonomous", "{}", "project-mobile")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.db.Exec(`
		CREATE TABLE channel_chat_chats (
			id TEXT PRIMARY KEY,
			agent_id INTEGER NOT NULL,
			project_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE channel_chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id TEXT NOT NULL,
			agent_id INTEGER,
			components_json TEXT NOT NULL DEFAULT '[]'
		);
		INSERT INTO channel_chat_chats (id, agent_id, project_id)
		VALUES (?, ?, ?)`,
		"default-"+strconv.FormatInt(agent.ID, 10), agent.ID, agent.ProjectID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return s, user.ID
}

func registerMobilePushForTest(
	t *testing.T,
	s *Server,
	userID int64,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"installation_id": "ios-installation-123",
		"provider_token":  token,
		"bundle_id":       "ai.apteva.mobile",
		"environment":     "sandbox",
		"app_version":     "1.0",
		"device_name":     "Test iPhone",
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/mobile/push/subscriptions",
		bytes.NewReader(body),
	)
	req.Header.Set("X-User-ID", strconv.FormatInt(userID, 10))
	rec := httptest.NewRecorder()
	s.handleMobilePushSubscriptions(rec, req)
	return rec
}

func TestMobilePushRelayURLPrecedence(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("APTEVA_PUSH_RELAY_URL", "")

	config := s.mobilePushRelayConfig()
	if config.Effective != defaultMobilePushRelayURL || config.Source != "default" {
		t.Fatalf("default config=%+v", config)
	}

	t.Setenv("APTEVA_PUSH_RELAY_URL", "https://relay.example.com/")
	config = s.mobilePushRelayConfig()
	if config.Effective != "https://relay.example.com" || config.Source != "env" {
		t.Fatalf("env config=%+v", config)
	}

	if err := s.store.SetSetting("push_relay_url", "https://saved.example.com/"); err != nil {
		t.Fatal(err)
	}
	config = s.mobilePushRelayConfig()
	if config.Effective != "https://saved.example.com" || config.Source != "db" {
		t.Fatalf("saved config=%+v", config)
	}
}

func TestServerSettingsReportsDefaultMobilePushRelay(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("APTEVA_PUSH_RELAY_URL", "")

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	s.handleServerSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", rec.Code, rec.Body.String())
	}

	body := decodeJSON(t, rec)
	pushRelay, ok := body["push_relay"].(map[string]any)
	if !ok {
		t.Fatalf("missing push_relay settings: %+v", body)
	}
	if pushRelay["effective"] != defaultMobilePushRelayURL ||
		pushRelay["source"] != "default" ||
		pushRelay["enabled"] != true {
		t.Fatalf("push_relay=%+v", pushRelay)
	}
}

func TestMobilePushRegistrationEncryptsRelayGrant(t *testing.T) {
	s, userID := newMobilePushTestServer(t)
	relay := &mobilePushRelayRecorder{}
	relayServer := httptest.NewServer(http.HandlerFunc(relay.handler))
	t.Cleanup(relayServer.Close)
	t.Setenv("APTEVA_PUSH_RELAY_URL", relayServer.URL)

	token := strings.Repeat("a", 64)
	rec := registerMobilePushForTest(t, s, userID, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "push_test_grant") {
		t.Fatalf("registration response exposed a secret: %s", rec.Body.String())
	}

	subscription, err := s.mobilePushSubscriptionByInstallation("ios-installation-123")
	if err != nil {
		t.Fatal(err)
	}
	if subscription.UserID != userID || subscription.RelayDeviceID != "device-123" {
		t.Fatalf("unexpected subscription: %+v", subscription)
	}
	if subscription.RelayGrantEncrypted == "push_test_grant" {
		t.Fatal("relay grant was stored in plaintext")
	}
	grant, err := Decrypt(s.secret, subscription.RelayGrantEncrypted)
	if err != nil || grant != "push_test_grant" {
		t.Fatalf("stored grant decrypt=%q err=%v", grant, err)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.registerCalls != 1 || relay.providerToken != token {
		t.Fatalf("relay registration calls=%d token=%q", relay.registerCalls, relay.providerToken)
	}
}

func TestMobilePushWorkerDeliversInboxOnceAndAdvancesCursor(t *testing.T) {
	s, userID := newMobilePushTestServer(t)
	relay := &mobilePushRelayRecorder{}
	relayServer := httptest.NewServer(http.HandlerFunc(relay.handler))
	t.Cleanup(relayServer.Close)
	t.Setenv("APTEVA_PUSH_RELAY_URL", relayServer.URL)

	rec := registerMobilePushForTest(t, s, userID, strings.Repeat("b", 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	var chatID string
	if err := s.store.db.QueryRow(`SELECT id FROM channel_chat_chats LIMIT 1`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	components := `[{"app":"channel-chat","name":"approval-card","props":{"title":"Review","status":"pending"}}]`
	result, err := s.store.db.Exec(
		`INSERT INTO channel_chat_messages (chat_id, agent_id, components_json)
		 VALUES (?, (SELECT agent_id FROM channel_chat_chats WHERE id = ?), ?)`,
		chatID, chatID, components,
	)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := result.LastInsertId()

	if err := s.deliverMobilePushCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverMobilePushCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	relay.mu.Lock()
	if relay.deliveryCalls != 1 {
		t.Fatalf("delivery calls=%d, want 1", relay.deliveryCalls)
	}
	if relay.deliveryGrant != "push_test_grant" {
		t.Fatalf("delivery grant=%q", relay.deliveryGrant)
	}
	delivery := relay.deliveryBodies[0]
	relay.mu.Unlock()
	if delivery["type"] != "approval" || delivery["item_id"] != strconv.FormatInt(messageID, 10) {
		t.Fatalf("unexpected delivery: %+v", delivery)
	}
	if delivery["project_id"] != "project-mobile" {
		t.Fatalf("project_id=%v, want project-mobile", delivery["project_id"])
	}
	if delivery["badge"] != float64(1) {
		t.Fatalf("badge=%v, want 1", delivery["badge"])
	}

	subscription, err := s.mobilePushSubscriptionByInstallation("ios-installation-123")
	if err != nil {
		t.Fatal(err)
	}
	if subscription.LastInboxMessageID != messageID || subscription.LastBadge != 1 {
		t.Fatalf("cursor/badge not advanced: %+v", subscription)
	}
}

func TestMobilePushWorkerRetriesWithoutAdvancingCursor(t *testing.T) {
	s, userID := newMobilePushTestServer(t)
	relay := &mobilePushRelayRecorder{deliveryStatus: http.StatusServiceUnavailable}
	relayServer := httptest.NewServer(http.HandlerFunc(relay.handler))
	t.Cleanup(relayServer.Close)
	t.Setenv("APTEVA_PUSH_RELAY_URL", relayServer.URL)

	rec := registerMobilePushForTest(t, s, userID, strings.Repeat("c", 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}
	_, err := s.store.db.Exec(`
		INSERT INTO channel_chat_messages (chat_id, agent_id, components_json)
		SELECT id, agent_id,
		       '[{"app":"channel-chat","name":"alert-card","props":{"title":"Warning","status":"sent"}}]'
		  FROM channel_chat_chats LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.deliverMobilePushCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	subscription, err := s.mobilePushSubscriptionByInstallation("ios-installation-123")
	if err != nil {
		t.Fatal(err)
	}
	if subscription.LastInboxMessageID != 0 {
		t.Fatalf("cursor advanced after failure: %d", subscription.LastInboxMessageID)
	}
	if subscription.RetryCount != 1 || subscription.NextRetryAt == "" || subscription.LastError == "" {
		t.Fatalf("retry state not recorded: %+v", subscription)
	}
}

func TestMobilePushInboxKindMatchesAttentionState(t *testing.T) {
	tests := []struct {
		name       string
		components string
		kind       string
		actionable bool
	}{
		{
			name:       "pending approval",
			components: `[{"app":"channel-chat","name":"approval-card","props":{"status":"pending"}}]`,
			kind:       "approval",
			actionable: true,
		},
		{
			name:       "decided approval",
			components: `[{"app":"channel-chat","name":"approval-card","props":{"status":"approved"}}]`,
			kind:       "approval",
			actionable: false,
		},
		{
			name:       "dismissed report",
			components: `[{"app":"channel-chat","name":"report-card","props":{"dismissed":true}}]`,
			actionable: false,
		},
		{
			name:       "alert",
			components: `[{"app":"channel-chat","name":"alert-card","props":{"severity":"warning"}}]`,
			kind:       "alert",
			actionable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, actionable := mobilePushInboxKind(tt.components)
			if kind != tt.kind || actionable != tt.actionable {
				t.Fatalf("kind=%q actionable=%v, want %q/%v", kind, actionable, tt.kind, tt.actionable)
			}
		})
	}
}
