package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscriptionCRUD(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create
	sub, err := s.store.CreateSubscription(1, 1, 0, "GitHub pushes", "github", "Push events", "webhook-abc", "", "")
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.Name != "GitHub pushes" {
		t.Errorf("expected 'GitHub pushes', got %s", sub.Name)
	}
	if sub.WebhookPath != "webhook-abc" {
		t.Errorf("expected webhook-abc, got %s", sub.WebhookPath)
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
	sub, _ := s.store.CreateSubscription(1, 1, 0, "HMAC test", "test", "", "hmac-webhook", encSecret, "")

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

	sub, _ := s.store.CreateSubscription(1, 1, 0, "Disabled", "test", "", "disabled-webhook", "", "")
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
	sub, err := s.store.CreateSubscription(1, 1, 0, "Webhook listener", "omnikit", "", "thread-webhook", "", "webhook-listener")
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
	sub, _ := s.store.CreateSubscription(1, 1, 0, "Default", "test", "", "no-thread", "", "")
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
