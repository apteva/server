package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestIntegrationWebhookEnsureAndVerifyStripe(t *testing.T) {
	var mu sync.Mutex
	registerCalls := 0
	deleteCalls := 0
	var registeredForm url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/webhook_endpoints/we_platform_1" {
			mu.Lock()
			deleteCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"we_platform_1","deleted":true}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/webhook_endpoints" {
			t.Errorf("provider request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse provider form: %v", err)
		}
		mu.Lock()
		registerCalls++
		registeredForm = form
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"we_platform_1","secret":"whsec_platform_only"}`)
	}))
	defer upstream.Close()

	s := newTestServer(t)
	s.secret = bytes.Repeat([]byte{0x2a}, 32)
	s.publicURL = "https://agents.example.test"
	if _, err := s.store.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES (1, 'billing@test.local', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO projects (id, user_id, name) VALUES ('proj-1', 1, 'Billing')`); err != nil {
		t.Fatal(err)
	}
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug:    "stripe",
		Name:    "Stripe",
		BaseURL: upstream.URL,
		Auth: AppAuthConfig{Headers: map[string]string{
			"Authorization": "Bearer {{token}}",
			"Content-Type":  "application/x-www-form-urlencoded",
		}},
		Webhooks: &AppWebhookConfig{
			Events: []AppWebhookEvent{
				{Name: "checkout.session.completed"},
				{Name: "setup_intent.succeeded"},
			},
			Registration: &WebhookRegConfig{
				Method:              http.MethodPost,
				Path:                "/webhook_endpoints",
				URLField:            "url",
				EventsField:         "enabled_events[]",
				IDField:             "id",
				ResponseSecretField: "secret",
				DeletePath:          "/webhook_endpoints/{id}",
				DeleteMethod:        http.MethodDelete,
			},
		},
	})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "billing",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsExecute},
			Integrations: []sdk.IntegrationDep{{
				Role: "payment_processor", Kind: "integration",
				CompatibleSlugs: []string{"stripe"},
			}},
		},
		Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{
			Method: http.MethodPost, Prefix: "/webhooks/stripe", NoAuth: true,
		}}},
	}
	encryptedCredentials, err := Encrypt(s.secret, `{"token":"sk_test_platform"}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnection(
		1, "stripe", "Stripe", "Stripe Test", "bearer",
		encryptedCredentials, "proj-1",
	)
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	installID := seedInstallWithBindings(t, s, "billing", manifest, map[string]any{
		"payment_processor": conn.ID,
	})

	ensureBody := sdk.IntegrationWebhookEnsureRequest{
		ConnectionID: conn.ID,
		Role:         "payment_processor",
		CallbackPath: "/webhooks/stripe",
		Events:       []string{"setup_intent.succeeded", "checkout.session.completed", "setup_intent.succeeded"},
	}
	rec := postIntegrationWebhookRequest(t, s, installID, "ensure", ensureBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure = %d: %s", rec.Code, rec.Body.String())
	}
	var status sdk.IntegrationWebhookStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode ensure status: %v", err)
	}
	if status.Status != "ready" || status.ExternalID != "we_platform_1" {
		t.Fatalf("unexpected ensure status: %#v", status)
	}
	if strings.Contains(rec.Body.String(), "whsec_") {
		t.Fatalf("signing secret leaked in ensure response: %s", rec.Body.String())
	}

	mu.Lock()
	gotCalls := registerCalls
	gotForm := registeredForm
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("registration calls = %d, want 1", gotCalls)
	}
	if gotForm.Get("url") != "https://agents.example.test/api/apps/billing/webhooks/stripe" {
		t.Fatalf("registered url = %q", gotForm.Get("url"))
	}
	gotEvents := gotForm["enabled_events[]"]
	if strings.Join(gotEvents, ",") != "checkout.session.completed,setup_intent.succeeded" {
		t.Fatalf("registered events = %#v", gotEvents)
	}
	record, err := s.integrationWebhookByInstallRole(installID, "payment_processor")
	if err != nil {
		t.Fatalf("load webhook record: %v", err)
	}
	if record.SecretEncrypted == "" || record.SecretEncrypted == "whsec_platform_only" {
		t.Fatalf("signing secret was not encrypted at rest")
	}

	// Repeated reconciliation with the same binding, URL, and event set is a no-op.
	rec = postIntegrationWebhookRequest(t, s, installID, "ensure", ensureBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent ensure = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotCalls = registerCalls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("idempotent ensure made %d registration calls, want 1", gotCalls)
	}

	payload := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_1"}}}`
	timestamp := time.Now().UTC().Unix()
	signature := stripeTestSignature(payload, "whsec_platform_only", timestamp)
	rec = postIntegrationWebhookRequest(t, s, installID, "verify", sdk.IntegrationWebhookVerifyRequest{
		Role: "payment_processor", Payload: payload, Signature: signature,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", rec.Code, rec.Body.String())
	}
	var verified sdk.IntegrationWebhookVerifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil {
		t.Fatalf("decode verify result: %v", err)
	}
	if verified.Provider != "stripe" || string(verified.Event) != payload {
		t.Fatalf("unexpected verify result: %#v", verified)
	}

	rec = postIntegrationWebhookRequest(t, s, installID, "verify", sdk.IntegrationWebhookVerifyRequest{
		Role: "payment_processor", Payload: payload, Signature: "t=1,v1=bad",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid signature = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET integration_bindings='{"payment_processor":null}' WHERE id=?`,
		installID,
	); err != nil {
		t.Fatal(err)
	}
	s.cleanupInactiveIntegrationWebhooks(installID, false)
	mu.Lock()
	gotDeleteCalls := deleteCalls
	mu.Unlock()
	if gotDeleteCalls != 1 {
		t.Fatalf("provider delete calls = %d, want 1 after unbind", gotDeleteCalls)
	}
	if _, err := s.integrationWebhookByInstallRole(installID, "payment_processor"); !errors.Is(err, errIntegrationWebhookNotFound) {
		t.Fatalf("webhook record remained after unbind: %v", err)
	}
}

func postIntegrationWebhookRequest(t *testing.T, s *Server, installID int64, action string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/apps/callback/integration-webhooks/"+action,
		bytes.NewReader(data),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func stripeTestSignature(payload, secret string, timestamp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(payload))
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
