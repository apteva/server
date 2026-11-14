package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func connectivityValidationApp(t *testing.T, slug string) AppTemplate {
	t.Helper()
	source, err := os.ReadFile("../integrations/src/apps/" + slug + ".json")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
	if err != nil || !bytes.Equal(source, embedded) {
		t.Fatalf("embedded catalog mismatch for %s: %v", slug, err)
	}
	var app AppTemplate
	if err := json.Unmarshal(source, &app); err != nil {
		t.Fatal(err)
	}
	return app
}

func connectivityValidationTool(t *testing.T, app *AppTemplate, name string) *AppToolDef {
	t.Helper()
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app.Tools[i]
		}
	}
	t.Fatalf("missing tool %s/%s", app.Slug, name)
	return nil
}

func TestConnectivityCatalogRequests(t *testing.T) {
	for _, tc := range []struct {
		slug, name, path, authHeader, authValue string
		input, want                             map[string]any
		credentials                             map[string]string
	}{
		{"telgea", "create_number", "/numbers", "x-api-key", "test-key", map[string]any{"userId": "user-1", "countryCode": "DK", "additionalNumber": false, "idempotency_key": "retry-1"}, map[string]any{"userId": "user-1", "countryCode": "DK", "additionalNumber": false}, map[string]string{"api_key": "test-key"}},
		{"telna", "send_sms", "/a2p-sms/sms-requests", "Authorization", "Bearer test-key-id", map[string]any{"sim": "8912345678901234567", "from": "100", "message": "Hello", "validity_period": 60}, map[string]any{"sim": "8912345678901234567", "from": "100", "message": "Hello", "validity_period": float64(60)}, map[string]string{"token": "test-key-id"}},
		{"esim-go", "apply_bundle", "/esims/apply", "X-API-Key", "test-key", map[string]any{"name": "test-bundle", "iccid": "8912345678901234567", "allowReassign": false}, map[string]any{"name": "test-bundle", "iccid": "8912345678901234567", "allowReassign": false}, map[string]string{"api_key": "test-key"}},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			app := connectivityValidationApp(t, tc.slug)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RequestURI() != tc.path {
					t.Errorf("unexpected route %s %s", r.Method, r.URL)
				}
				if r.Header.Get(tc.authHeader) != tc.authValue || r.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected authentication or content type")
				}
				if tc.slug == "telgea" && r.Header.Get("idempotency-key") != "retry-1" {
					t.Error("missing retry key")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(body, tc.want) {
					t.Errorf("unexpected request body %#v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"result-1"}`)
			}))
			defer server.Close()
			app.BaseURL = server.URL
			result, err := executeIntegrationTool(&app, connectivityValidationTool(t, &app, tc.name), tc.credentials, tc.input, "")
			if err != nil || !result.Success {
				t.Fatalf("execute failed: %v %#v", err, result)
			}
		})
	}
}

func TestConnectivity1GlobalExchange(t *testing.T) {
	app := connectivityValidationApp(t, "1global")
	exchanges := 0
	providerCalls := 0
	rejectNext := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			exchanges++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "test-client" || r.Form.Get("client_secret") != "secret with +&" {
				t.Error("invalid client credentials form")
			}
			io.WriteString(w, `{"access_token":"test-access","expires_in":3600}`)
			return
		}
		providerCalls++
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("Api-Version") != "2026-02-05" || r.Header.Get("Idempotency-Key") != "order-retry-key" {
			t.Error("unexpected Connect headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want := map[string]any{"type": "activate_subscription", "subscriber_id": "subscriber-1", "purchase_location": "DE", "sim_profile": map[string]any{"sim_type": "esim"}, "products": map[string]any{"plan": map[string]any{"product_offering_id": "plan-1"}}}
		if r.URL.RequestURI() != "/orders" || !reflect.DeepEqual(body, want) {
			t.Errorf("unexpected order %s %#v", r.URL, body)
		}
		if rejectNext {
			rejectNext = false
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"expired"}`)
			return
		}
		io.WriteString(w, `{"id":"order-1","status":"accepted"}`)
	}))
	defer server.Close()
	app.BaseURL = server.URL
	app.Auth.TokenExchange.URL = server.URL + "/token"
	creds := map[string]string{"client_id": "test-client", "client_secret": "secret with +&"}
	input := map[string]any{"subscriber_id": "subscriber-1", "purchase_location": "DE", "sim_profile": map[string]any{"sim_type": "esim"}, "products": map[string]any{"plan": map[string]any{"product_offering_id": "plan-1"}}, "idempotency_key": "order-retry-key"}
	tool := connectivityValidationTool(t, &app, "activate_subscription")
	for attempt := 0; attempt < 3; attempt++ {
		if attempt == 2 {
			rejectNext = true
		}
		result, err := executeIntegrationToolWithRefresh(&app, tool, creds, input, "", nil)
		if err != nil || !result.Success {
			t.Fatalf("Connect execution failed: %v %#v", err, result)
		}
	}
	if exchanges != 2 || providerCalls != 4 {
		t.Errorf("unexpected exchange/retry counts: %d %d", exchanges, providerCalls)
	}
}
