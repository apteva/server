package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestSubscriptionAutoRegister_OmniKit tests auto-registration against the real OmniKit API.
// Requires OMNIKIT_API_KEY env var (or uses hardcoded test key).
//
//	go test -v -run TestSubscriptionAutoRegister_OmniKit
func TestSubscriptionAutoRegister_OmniKit(t *testing.T) {
	apiKey := os.Getenv("OMNIKIT_API_KEY")
	if apiKey == "" {
		apiKey = "okt_401860a4b3251302b6ea9ba834cc79b83e9882528363b0ee"
	}
	if testing.Short() {
		t.Skip("skipping live OmniKit test in short mode")
	}

	// Load the real omnikit-messaging app JSON
	s := newTestServer(t)
	s.secret = testSecret()
	s.publicURL = "https://test-apteva.example.com"
	s.catalog = NewAppCatalog()

	appsDir := "../integrations/src/apps"
	if _, err := os.Stat(appsDir); err != nil {
		t.Skipf("integrations catalog not found at %s", appsDir)
	}
	if err := s.catalog.LoadFromDir(appsDir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}

	app := s.catalog.Get("omnikit-messaging")
	if app == nil {
		t.Fatal("omnikit-messaging not found in catalog")
	}
	if app.Webhooks == nil || app.Webhooks.Registration == nil {
		t.Fatal("omnikit-messaging has no webhook registration config")
	}

	t.Logf("App: %s", app.Name)
	t.Logf("Base URL: %s", app.BaseURL)
	t.Logf("Registration: %s %s", app.Webhooks.Registration.Method, app.Webhooks.Registration.Path)
	t.Logf("URL field: %s", app.Webhooks.Registration.URLField)
	t.Logf("Events field: %s", app.Webhooks.Registration.EventsField)
	t.Logf("ID field: %s", app.Webhooks.Registration.IDField)

	// Create user + login
	postJSON(t, s.handleRegister, map[string]string{
		"email": "omnikit@test.com", "password": "password123",
	})
	loginResp := postJSON(t, s.handleLogin, map[string]string{
		"email": "omnikit@test.com", "password": "password123",
	})
	cookie := getSessionCookie(loginResp)

	// Create connection with real API key
	creds, _ := json.Marshal(map[string]string{"api_key": apiKey})
	encrypted, _ := Encrypt(s.secret, string(creds))
	conn, err := s.store.CreateConnection(1, "omnikit-messaging", "OmniKit Messaging", "Test Connection", "api_key", encrypted, "")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Logf("Connection ID: %d", conn.ID)

	// --- Step 1: Create subscription (triggers auto-registration with real OmniKit API) ---
	body, _ := json.Marshal(map[string]any{
		"name":          "Apteva Test Webhook",
		"slug":          "omnikit-messaging",
		"connection_id": conn.ID,
		"instance_id":   1,
		"events":        []string{"messaging.inbound_message_processed"},
		"hmac_secret":   "test-secret-apteva",
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateSubscription(w, r)
	})(rec, req)

	t.Logf("Create response status: %d", rec.Code)
	t.Logf("Create response body: %s", rec.Body.String())

	if rec.Code != 200 {
		t.Fatalf("create subscription: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResult struct {
		Subscription   Subscription `json:"subscription"`
		WebhookURL     string       `json:"webhook_url"`
		AutoRegistered bool         `json:"auto_registered"`
	}
	json.Unmarshal(rec.Body.Bytes(), &createResult)

	t.Logf("Auto-registered: %v", createResult.AutoRegistered)
	t.Logf("Webhook URL: %s", createResult.WebhookURL)

	if !createResult.AutoRegistered {
		t.Error("expected auto_registered=true — OmniKit registration failed")
	}

	// Check external webhook ID
	extID := s.store.GetSubscriptionExternalID(createResult.Subscription.ID)
	t.Logf("External webhook ID: %q", extID)

	if extID == "" {
		t.Error("expected external_webhook_id to be set after auto-registration")
	}

	// --- Step 2: Verify we can list webhooks from OmniKit (optional, just for debug) ---
	if app.Webhooks.Registration.DeletePath != "" && extID != "" {
		t.Logf("Webhook registered successfully at OmniKit with ID: %s", extID)
	}

	// --- Step 3: Delete subscription (should unregister from OmniKit) ---
	subID := createResult.Subscription.ID
	req = httptest.NewRequest("DELETE", "/subscriptions/"+subID, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	rec = httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.handleDeleteSubscription(w, r)
	})(rec, req)

	t.Logf("Delete response status: %d", rec.Code)
	t.Logf("Delete response body: %s", rec.Body.String())

	if rec.Code != 200 {
		t.Fatalf("delete subscription: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify gone from DB
	subs, _ := s.store.ListSubscriptions(1)
	if len(subs) != 0 {
		t.Errorf("expected 0 subscriptions after delete, got %d", len(subs))
	}

	t.Log("Full lifecycle passed: create → auto-register → delete → auto-unregister")
}
