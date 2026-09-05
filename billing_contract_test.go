package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBillingStripeRecoveryContracts(t *testing.T) {
	raw, e := integrationsCatalogFS.ReadFile("integrations-catalog/stripe.json")
	if e != nil {
		t.Fatal(e)
	}
	var app AppTemplate
	if e = json.Unmarshal(raw, &app); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name, method, path string
		input              map[string]any
	}{
		{"create_refund", "POST", "/refunds", map[string]any{"payment_intent": "pi_test", "amount": 100, "idempotency_key": "stable"}},
		{"create_customer", "POST", "/customers", map[string]any{"email": "test@example.com", "idempotency_key": "stable"}},
		{"cancel_payment_intent", "POST", "/payment_intents/pi_test/cancel", map[string]any{"payment_intent_id": "pi_test"}},
		{"expire_checkout_session", "POST", "/checkout/sessions/cs_test/expire", map[string]any{"checkout_session_id": "cs_test"}},
		{"get_refund", "GET", "/refunds/re_test", map[string]any{"refund_id": "re_test"}},
		{"create_checkout_session", "POST", "/checkout/sessions", map[string]any{"mode": "setup", "customer": "cus_test", "success_url": "https://example.com/success", "cancel_url": "https://example.com/cancel", "payment_method_types[0]": "card", "payment_method_types[1]": "sepa_debit", "idempotency_key": "stable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			var method, path, key string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method = r.Method
				path = r.URL.Path
				key = r.Header.Get("Idempotency-Key")
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"test"}`))
			}))
			defer s.Close()
			app.BaseURL = s.URL
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatal("missing tool")
			}
			r, e := executeIntegrationTool(&app, tool, map[string]string{"token": "sk_test_fixture"}, tc.input, "")
			if e != nil || !r.Success {
				t.Fatalf("%+v %v", r, e)
			}
			if method != tc.method || path != tc.path {
				t.Fatalf("%s %s", method, path)
			}
			form, e := url.ParseQuery(string(body))
			if e != nil {
				t.Fatal(e)
			}
			if tc.input["idempotency_key"] != nil && (key != "stable" || form.Has("idempotency_key")) {
				t.Fatalf("idempotency missing from header or leaked into body: %q %s", key, body)
			}
			if tc.name == "create_checkout_session" && (form.Get("payment_method_types[0]") != "card" || form.Get("payment_method_types[1]") != "sepa_debit" || form.Has("payment_method_types")) {
				t.Fatalf("bad setup encoding: %s", body)
			}
		})
	}
}
