package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionCRUD(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create
	sub, err := s.store.CreateSubscription(1, 1, 0, "GitHub pushes", "github", "Push events", "webhook-abc", "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.Name != "GitHub pushes" {
		t.Errorf("expected 'GitHub pushes', got %s", sub.Name)
	}
	if sub.WebhookPath != "webhook-abc" {
		t.Errorf("expected webhook-abc, got %s", sub.WebhookPath)
	}
	if sub.NotifyAgent {
		t.Error("expected agent notifications off by default")
	}

	// List
	subs, err := s.store.ListSubscriptions(1)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1, got %d", len(subs))
	}

	// Get by path
	found, _, err := s.store.GetSubscriptionByPath("webhook-abc")
	if err != nil {
		t.Fatalf("GetSubscriptionByPath: %v", err)
	}
	if found.Name != "GitHub pushes" {
		t.Errorf("expected 'GitHub pushes', got %s", found.Name)
	}

	// Disable
	s.store.SetSubscriptionEnabled(1, sub.ID, false)
	subs2, _ := s.store.ListSubscriptions(1)
	if subs2[0].Enabled {
		t.Error("expected disabled")
	}

	// Enable
	s.store.SetSubscriptionEnabled(1, sub.ID, true)
	subs3, _ := s.store.ListSubscriptions(1)
	if !subs3[0].Enabled {
		t.Error("expected enabled")
	}

	// Delete
	s.store.DeleteSubscription(1, sub.ID)
	subs4, _ := s.store.ListSubscriptions(1)
	if len(subs4) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(subs4))
	}

	notifying, err := s.store.CreateSubscription(1, 1, 0, "Notify", "notify", "", "webhook-notify", "", "", "", nil, true)
	if err != nil {
		t.Fatalf("CreateSubscription notify_agent: %v", err)
	}
	fetched, err := s.store.GetSubscription(1, notifying.ID)
	if err != nil {
		t.Fatalf("GetSubscription notify_agent: %v", err)
	}
	if !fetched.NotifyAgent {
		t.Error("expected agent notifications on when requested")
	}
	if err := s.store.SetSubscriptionNotifyAgent(1, notifying.ID, false); err != nil {
		t.Fatalf("SetSubscriptionNotifyAgent: %v", err)
	}
	fetched, err = s.store.GetSubscription(1, notifying.ID)
	if err != nil {
		t.Fatalf("GetSubscription notify_agent off: %v", err)
	}
	if fetched.NotifyAgent {
		t.Error("expected agent notifications off after update")
	}
}

func TestAppEventSubscriptionsUseUniqueInternalWebhookPaths(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	first, err := s.store.CreateAppEventSubscription(1, 11, "CRM contact added", "crm:contact.added", "", "main", "project-a")
	if err != nil {
		t.Fatalf("CreateAppEventSubscription first: %v", err)
	}
	second, err := s.store.CreateAppEventSubscription(1, 22, "CRM contact added", "crm:contact.added", "", "main", "project-b")
	if err != nil {
		t.Fatalf("CreateAppEventSubscription second same app/topic in another project: %v", err)
	}

	if first.WebhookPath == "" || second.WebhookPath == "" {
		t.Fatalf("internal webhook paths should not be empty: first=%q second=%q", first.WebhookPath, second.WebhookPath)
	}
	if first.WebhookPath == second.WebhookPath {
		t.Fatalf("internal webhook paths should be unique, both were %q", first.WebhookPath)
	}
	if !strings.HasPrefix(first.WebhookPath, "app-event-") || !strings.HasPrefix(second.WebhookPath, "app-event-") {
		t.Fatalf("expected app-event internal paths, got %q and %q", first.WebhookPath, second.WebhookPath)
	}

	if _, _, err := s.store.GetSubscriptionByPath(first.WebhookPath); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("internal app-event path should not be publicly routable, err=%v", err)
	}

	subs, err := s.store.ListSubscriptions(1)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subscriptions, got %d", len(subs))
	}
}

func TestEmptyWebhookPathFallbackIsUniqueAndNotPubliclyRoutable(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	first, err := s.store.CreateSubscription(1, 11, 101, "Provider trigger A", "crm", "", "", "", "main", "project-a", []string{"contact.added"})
	if err != nil {
		t.Fatalf("CreateSubscription first empty path: %v", err)
	}
	second, err := s.store.CreateSubscription(1, 22, 102, "Provider trigger B", "crm", "", "", "", "main", "project-b", []string{"contact.added"})
	if err != nil {
		t.Fatalf("CreateSubscription second empty path: %v", err)
	}

	if first.WebhookPath == "" || second.WebhookPath == "" {
		t.Fatalf("fallback paths should not be empty: first=%q second=%q", first.WebhookPath, second.WebhookPath)
	}
	if first.WebhookPath == second.WebhookPath {
		t.Fatalf("fallback paths should be unique, both were %q", first.WebhookPath)
	}
	if !strings.HasPrefix(first.WebhookPath, "internal-") || !strings.HasPrefix(second.WebhookPath, "internal-") {
		t.Fatalf("expected internal fallback paths, got %q and %q", first.WebhookPath, second.WebhookPath)
	}
	if _, _, err := s.store.GetSubscriptionByPath(first.WebhookPath); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("internal fallback path should not be publicly routable, err=%v", err)
	}
}

func TestPollSubscriptionWebhookPathIsNotPubliclyRoutable(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	sub, err := s.store.CreatePollSubscription(1, 11, 101, "Polling trigger", "crm", "", "main", "project-a", []string{"contact.added"}, "{}", time.Now())
	if err != nil {
		t.Fatalf("CreatePollSubscription: %v", err)
	}
	if !strings.HasPrefix(sub.WebhookPath, "poll-") {
		t.Fatalf("expected poll internal path, got %q", sub.WebhookPath)
	}
	if _, _, err := s.store.GetSubscriptionByPath(sub.WebhookPath); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("poll internal path should not be publicly routable, err=%v", err)
	}
}

func TestMigrateEmptySubscriptionWebhookPaths(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	_, err := s.store.db.Exec(`
		INSERT INTO subscriptions
			(id, user_id, agent_id, connection_id, name, slug, webhook_path, source, delivery)
		VALUES
			('legacy-app-event', 1, 11, 0, 'Legacy app event', 'crm:contact.added', '', 'app_event', 'webhook')
	`)
	if err != nil {
		t.Fatalf("insert legacy empty webhook_path row: %v", err)
	}

	migrateEmptySubscriptionWebhookPaths(s.store.db)

	var path string
	if err := s.store.db.QueryRow(`SELECT webhook_path FROM subscriptions WHERE id = 'legacy-app-event'`).Scan(&path); err != nil {
		t.Fatalf("query migrated row: %v", err)
	}
	if path != "app-event-legacy-app-event" {
		t.Fatalf("migrated path = %q, want app-event-legacy-app-event", path)
	}
	if _, _, err := s.store.GetSubscriptionByPath(path); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("migrated internal path should not be publicly routable, err=%v", err)
	}
}

func TestVerifyHMAC(t *testing.T) {
	secret := "mysecret"
	body := []byte(`{"action":"push"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyHMAC(body, sig, secret) {
		t.Error("valid signature should pass")
	}

	if verifyHMAC(body, "sha256=invalid", secret) {
		t.Error("invalid signature should fail")
	}

	// Empty secret = always pass
	if !verifyHMAC(body, "", "") {
		t.Error("empty secret should pass")
	}
}

func TestWebhookHTTPFlow(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	// Create subscription via HTTP
	body, _ := json.Marshal(map[string]any{
		"name":        "Test webhook",
		"slug":        "test",
		"instance_id": 1,
		"description": "Test subscription",
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResult struct {
		Subscription Subscription `json:"subscription"`
		WebhookURL   string       `json:"webhook_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &createResult)

	if createResult.Subscription.Name != "Test webhook" {
		t.Errorf("expected 'Test webhook', got %s", createResult.Subscription.Name)
	}
	if createResult.WebhookURL == "" {
		t.Error("expected webhook URL")
	}
	if createResult.Subscription.WebhookPath == "" {
		t.Error("expected webhook path")
	}

	// List subscriptions
	req = httptest.NewRequest("GET", "/subscriptions", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleListSubscriptions(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var listed []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1, got %d", len(listed))
	}
	if listed[0]["webhook_url"] == nil || listed[0]["webhook_url"] == "" {
		t.Error("expected webhook_url in list response")
	}
}

func TestWebhookHMACVerification(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create subscription with HMAC secret
	hmacSecret := "test-hmac-secret"
	encSecret, _ := Encrypt(s.secret, hmacSecret)
	sub, _ := s.store.CreateSubscription(1, 1, 0, "HMAC test", "test", "", "hmac-webhook", encSecret, "", "", nil)

	// Valid signature
	payload := []byte(`{"event":"test"}`)
	mac := hmac.New(sha256.New, []byte(hmacSecret))
	mac.Write(payload)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/webhooks/"+sub.WebhookPath, bytes.NewReader(payload))
	req.Header.Set("x-hub-signature-256", validSig)
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	// Will fail with "instance not available" since we don't have a running instance,
	// but it should NOT fail with "invalid signature"
	if rec.Code == 401 {
		t.Error("valid signature should not return 401")
	}

	// Invalid signature
	req2 := httptest.NewRequest("POST", "/webhooks/"+sub.WebhookPath, bytes.NewReader(payload))
	req2.Header.Set("x-hub-signature-256", "sha256=invalid")
	rec2 := httptest.NewRecorder()
	s.handleWebhook(rec2, req2)

	if rec2.Code != 401 {
		t.Errorf("invalid signature should return 401, got %d", rec2.Code)
	}
}

func TestSubscriptionDisabledRejects(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	sub, _ := s.store.CreateSubscription(1, 1, 0, "Disabled", "test", "", "disabled-webhook", "", "", "", nil)
	s.store.SetSubscriptionEnabled(1, sub.ID, false)

	req := httptest.NewRequest("POST", "/webhooks/disabled-webhook", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)

	if rec.Code != 403 {
		t.Errorf("disabled subscription should return 403, got %d", rec.Code)
	}
}

func TestSubscriptionThreadID(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create subscription with thread_id
	sub, err := s.store.CreateSubscription(1, 1, 0, "Webhook listener", "omnikit", "", "thread-webhook", "", "webhook-listener", "", nil)
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.ThreadID != "webhook-listener" {
		t.Errorf("expected thread_id 'webhook-listener', got %q", sub.ThreadID)
	}

	// Verify thread_id persists in list
	subs, _ := s.store.ListSubscriptions(1)
	if len(subs) != 1 {
		t.Fatalf("expected 1, got %d", len(subs))
	}
	if subs[0].ThreadID != "webhook-listener" {
		t.Errorf("list: expected thread_id 'webhook-listener', got %q", subs[0].ThreadID)
	}

	// Verify thread_id persists in get by ID
	got, err := s.store.GetSubscription(1, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.ThreadID != "webhook-listener" {
		t.Errorf("get: expected thread_id 'webhook-listener', got %q", got.ThreadID)
	}

	// Verify thread_id persists in get by path
	byPath, _, err := s.store.GetSubscriptionByPath("thread-webhook")
	if err != nil {
		t.Fatalf("GetSubscriptionByPath: %v", err)
	}
	if byPath.ThreadID != "webhook-listener" {
		t.Errorf("getByPath: expected thread_id 'webhook-listener', got %q", byPath.ThreadID)
	}
}

func TestSubscriptionThreadID_Empty(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create subscription without thread_id — should default to empty (main)
	sub, _ := s.store.CreateSubscription(1, 1, 0, "Default", "test", "", "no-thread", "", "", "", nil)
	if sub.ThreadID != "" {
		t.Errorf("expected empty thread_id, got %q", sub.ThreadID)
	}

	byPath, _, _ := s.store.GetSubscriptionByPath("no-thread")
	if byPath.ThreadID != "" {
		t.Errorf("expected empty thread_id from path lookup, got %q", byPath.ThreadID)
	}
}

func TestSubscriptionHTTP_ThreadID(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	// Create subscription with thread_id via HTTP
	body, _ := json.Marshal(map[string]any{
		"name":        "Thread webhook",
		"slug":        "test",
		"instance_id": 1,
		"thread_id":   "my-listener",
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		Subscription Subscription `json:"subscription"`
	}
	json.Unmarshal(rec.Body.Bytes(), &result)

	if result.Subscription.ThreadID != "my-listener" {
		t.Errorf("expected thread_id 'my-listener', got %q", result.Subscription.ThreadID)
	}
}
