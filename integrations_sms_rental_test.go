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

func TestSmsRentalCatalogs(t *testing.T) {
	for _, tc := range []struct {
		slug, name, route string
		input, body       map[string]any
	}{
		{"quackr", "purchase_numbers", "/purchase", map[string]any{"locale": "US", "duration": "oneMonth", "autoRenew": false}, map[string]any{"locale": "US", "duration": "oneMonth", "autoRenew": false}},
		{"quackr", "receive_sms", "/receive-sms", map[string]any{"phoneNumber": "+12025550100"}, map[string]any{"phoneNumber": "+12025550100"}},
		{"fetch-sms", "create_rental", "/rentals", map[string]any{"service": 1, "days": 30}, map[string]any{"service": float64(1), "days": float64(30)}},
		{"fetch-sms", "extend_rental", "/rentals/rental-1/extend", map[string]any{"rental_id": "rental-1", "days": 7}, map[string]any{"days": float64(7)}},
		{"fetch-sms", "cancel_rental", "/rentals/rental-1/cancel", map[string]any{"rental_id": "rental-1"}, nil},
	} {
		t.Run(tc.slug+"/"+tc.name, func(t *testing.T) {
			source, err := os.ReadFile("../integrations/src/apps/" + tc.slug + ".json")
			if err != nil {
				t.Fatal(err)
			}
			embedded, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + tc.slug + ".json")
			if err != nil || !bytes.Equal(source, embedded) {
				t.Fatalf("embedded/source mismatch: %v", err)
			}
			var app AppTemplate
			if err := json.Unmarshal(embedded, &app); err != nil {
				t.Fatal(err)
			}
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatal("tool missing")
			}
			fail := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != tc.route || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("invalid request %s %s", r.Method, r.URL)
				}
				if tc.slug == "quackr" && r.Header.Get("x-api-key") != "test-key" {
					t.Error("missing Quackr key")
				}
				if tc.slug == "fetch-sms" && r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing Fetch SMS key")
				}
				raw, _ := io.ReadAll(r.Body)
				if tc.body == nil {
					if len(raw) != 0 {
						t.Errorf("expected empty body: %s", raw)
					}
				} else {
					var body map[string]any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(body, tc.body) {
						t.Errorf("unexpected body: %#v", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if fail {
					io.WriteString(w, `{"success":false,"error":"Insufficient balance"}`)
				} else {
					io.WriteString(w, `{"success":true,"id":"resource-1"}`)
				}
			}))
			defer server.Close()
			app.BaseURL = server.URL
			credentials := map[string]string{"api_key": "test-key", "token": "test-token"}
			result, err := executeIntegrationTool(&app, tool, credentials, tc.input, "")
			if err != nil || !result.Success {
				t.Fatalf("execute failed: %v %#v", err, result)
			}
			if tc.slug == "quackr" {
				fail = true
				result, err = executeIntegrationTool(&app, tool, credentials, tc.input, "")
				if err != nil || result.Success {
					t.Fatalf("provider failure lost: %v %#v", err, result)
				}
				if result.Data.(map[string]any)["message"] != "Insufficient balance" {
					t.Fatalf("error detail lost: %#v", result)
				}
			}
		})
	}
}
