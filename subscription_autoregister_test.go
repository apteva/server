package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSubscriptionAutoRegister tests the full webhook auto-registration flow:
// 1. Mock external service receives the registration request
// 2. Server stores the external webhook ID
// 3. On delete, server calls the external service's delete endpoint
func TestSubscriptionAutoRegister(t *testing.T) {
	// Track what the mock external service received
	var mu sync.Mutex
	var registerCalls []map[string]any
	var deleteCalls []string

	// Mock external service (simulates omnikit/stripe webhook API)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == "POST" && r.URL.Path == "/webhooks-register":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			registerCalls = append(registerCalls, body)
			// Return a webhook ID like a real service would
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"id": "ext-webhook-123",
				},
			})

		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/webhooks/"):
			id := strings.TrimPrefix(r.URL.Path, "/webhooks/")
			deleteCalls = append(deleteCalls, id)
			w.WriteHeader(200)

		default:
			t.Logf("mock: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer mock.Close()

	// Setup test server with catalog containing a test app
	s := newTestServer(t)
	s.secret = testSecret()
	s.publicURL = "https://agents.test.com"
	s.catalog = NewAppCatalog()

	// Register a test app that points to our mock
	s.catalog.Register(&AppTemplate{
		Slug:    "test-messaging",
		Name:    "Test Messaging",
		BaseURL: mock.URL,
		Auth: AppAuthConfig{
			Types:   []string{"api_key"},
			Headers: map[string]string{"X-API-Key": "{{api_key}}"},
		},
		Webhooks: &AppWebhookConfig{
			SignatureHeader: "x-webhook-signature",
			Registration: &WebhookRegConfig{
				Method:      "POST",
				Path:        "/webhooks-register",
				URLField:    "endpoint_url",
				EventsField: "event_types",
				SecretField: "secret_key",
				IDField:     "data.id",
				Extra:       map[string]interface{}{"direction": "outgoing", "name": "Apteva Webhook"},
				DeletePath:  "/webhooks/{id}",
				DeleteMethod: "DELETE",
			},
			Events: []AppWebhookEvent{
				{Name: "message.received", Description: "Incoming message"},
				{Name: "message.sent", Description: "Message sent"},
			},
		},
	})

	// Create user + login
	postJSON(t, s.handleRegister, map[string]string{
		"email": "sub@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "sub@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	// Create a connection with encrypted credentials
	creds, _ := json.Marshal(map[string]string{"api_key": "test-key-123"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	conn, err := s.store.CreateConnection(1, "test-messaging", "Test Messaging", "My Test", "api_key", encrypted, "")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	// --- Step 1: Create subscription (should auto-register) ---
	body, _ := json.Marshal(map[string]any{
		"name":          "Incoming messages",
		"slug":          "test-messaging",
		"connection_id": conn.ID,
		"instance_id":   1,
		"events":        []string{"message.received"},
		"hmac_secret":   "my-secret",
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("create subscription: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResult struct {
		Subscription   Subscription `json:"subscription"`
		WebhookURL     string       `json:"webhook_url"`
		AutoRegistered bool         `json:"auto_registered"`
	}
	json.Unmarshal(rec.Body.Bytes(), &createResult)

	// Verify auto-registration happened
	if !createResult.AutoRegistered {
		t.Error("expected auto_registered=true")
	}

	// Verify webhook URL uses public URL
	if !strings.HasPrefix(createResult.WebhookURL, "https://agents.test.com/webhooks/") {
		t.Errorf("expected public webhook URL, got %s", createResult.WebhookURL)
	}

	// Verify mock received the registration request
	mu.Lock()
	if len(registerCalls) != 1 {
		t.Fatalf("expected 1 register call, got %d", len(registerCalls))
	}
	regCall := registerCalls[0]
	mu.Unlock()

	// Check all fields were sent correctly
	if regCall["endpoint_url"] != createResult.WebhookURL {
		t.Errorf("expected endpoint_url=%s, got %v", createResult.WebhookURL, regCall["endpoint_url"])
	}
	if regCall["direction"] != "outgoing" {
		t.Errorf("expected direction=outgoing, got %v", regCall["direction"])
	}
	if regCall["name"] != "Apteva Webhook" {
		t.Errorf("expected name='Apteva Webhook', got %v", regCall["name"])
	}
	if regCall["secret_key"] != "my-secret" {
		t.Errorf("expected secret_key='my-secret', got %v", regCall["secret_key"])
	}
	// Events should be an array
	events, ok := regCall["event_types"].([]any)
	if !ok || len(events) != 1 || events[0] != "message.received" {
		t.Errorf("expected event_types=[message.received], got %v", regCall["event_types"])
	}

	// Verify external webhook ID was stored in DB
	extID := s.store.GetSubscriptionExternalID(createResult.Subscription.ID)
	if extID != "ext-webhook-123" {
		t.Errorf("expected external_webhook_id='ext-webhook-123', got %q", extID)
	}

	// --- Step 2: Delete subscription (should auto-unregister) ---
	subID := createResult.Subscription.ID
	req = httptest.NewRequest("DELETE", "/subscriptions/"+subID, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleDeleteSubscription(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("delete subscription: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify mock received the delete request with the correct webhook ID
	mu.Lock()
	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(deleteCalls))
	}
	if deleteCalls[0] != "ext-webhook-123" {
		t.Errorf("expected delete for ext-webhook-123, got %s", deleteCalls[0])
	}
	mu.Unlock()

	// Verify subscription is gone from DB
	subs, _ := s.store.ListSubscriptions(1)
	if len(subs) != 0 {
		t.Errorf("expected 0 subscriptions after delete, got %d", len(subs))
	}
}

// TestSubscriptionAutoRegister_NoRegistrationConfig verifies that apps without
// registration config don't attempt auto-registration (and don't error).
func TestSubscriptionAutoRegister_NoRegistrationConfig(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()

	s.catalog.Register(&AppTemplate{
		Slug:    "simple-app",
		Name:    "Simple App",
		BaseURL: "https://example.com",
		Auth:    AppAuthConfig{Types: []string{"api_key"}, Headers: map[string]string{"X-API-Key": "{{api_key}}"}},
		Webhooks: &AppWebhookConfig{
			SignatureHeader: "x-signature",
			// No Registration config
			Events: []AppWebhookEvent{{Name: "test.event", Description: "Test"}},
		},
	})

	postJSON(t, s.handleRegister, map[string]string{
		"email": "no-reg@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "no-reg@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	creds, _ := json.Marshal(map[string]string{"api_key": "k"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	conn, _ := s.store.CreateConnection(1, "simple-app", "Simple App", "Test", "api_key", encrypted, "")

	body, _ := json.Marshal(map[string]any{
		"name":          "No auto-reg",
		"slug":          "simple-app",
		"connection_id": conn.ID,
		"instance_id":   1,
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		AutoRegistered bool `json:"auto_registered"`
	}
	json.Unmarshal(rec.Body.Bytes(), &result)

	if result.AutoRegistered {
		t.Error("expected auto_registered=false for app without registration config")
	}
}

// TestSubscriptionAutoRegister_ExternalServiceError verifies graceful handling
// when the external service returns an error during registration.
func TestSubscriptionAutoRegister_ExternalServiceError(t *testing.T) {
	// Mock that returns 500
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	}))
	defer mock.Close()

	s := newTestServer(t)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()

	s.catalog.Register(&AppTemplate{
		Slug:    "failing-app",
		Name:    "Failing App",
		BaseURL: mock.URL,
		Auth:    AppAuthConfig{Types: []string{"api_key"}, Headers: map[string]string{"X-API-Key": "{{api_key}}"}},
		Webhooks: &AppWebhookConfig{
			Registration: &WebhookRegConfig{
				Method:   "POST",
				Path:     "/webhooks",
				URLField: "url",
				IDField:  "id",
			},
			Events: []AppWebhookEvent{{Name: "test", Description: "Test"}},
		},
	})

	postJSON(t, s.handleRegister, map[string]string{
		"email": "fail@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "fail@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	creds, _ := json.Marshal(map[string]string{"api_key": "k"})
	encrypted, _ := Encrypt(s.secret, string(creds))
	conn, _ := s.store.CreateConnection(1, "failing-app", "Failing App", "Test", "api_key", encrypted, "")

	body, _ := json.Marshal(map[string]any{
		"name":          "Will fail",
		"slug":          "failing-app",
		"connection_id": conn.ID,
		"instance_id":   1,
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	// Subscription should still be created (just not auto-registered)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		AutoRegistered bool `json:"auto_registered"`
	}
	json.Unmarshal(rec.Body.Bytes(), &result)

	if result.AutoRegistered {
		t.Error("expected auto_registered=false when external service fails")
	}

	// External ID should be empty
	subs, _ := s.store.ListSubscriptions(1)
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	extID := s.store.GetSubscriptionExternalID(subs[0].ID)
	if extID != "" {
		t.Errorf("expected empty external_webhook_id, got %q", extID)
	}
}

// TestSubscriptionWebhookURL_PublicURL verifies the webhook URL uses PUBLIC_URL when set.
func TestSubscriptionWebhookURL_PublicURL(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	s.publicURL = "https://my-domain.com"
	s.port = "5280"

	url := s.webhookURL("test-path")
	if url != "https://my-domain.com/webhooks/test-path" {
		t.Errorf("expected https://my-domain.com/webhooks/test-path, got %s", url)
	}

	// Without public URL — falls back to localhost
	s.publicURL = ""
	url = s.webhookURL("test-path")
	if url != "http://127.0.0.1:5280/webhooks/test-path" {
		t.Errorf("expected localhost fallback, got %s", url)
	}
}
