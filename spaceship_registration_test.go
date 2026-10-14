package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpaceshipRegistrationPreservesAsyncHandle(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/spaceship.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err = json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "register_domain" {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("missing registration tool")
	}
	for _, header := range []string{"operation123", ""} {
		t.Run("header="+header, func(t *testing.T) {
			seen := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen++
				if r.Method != "POST" || r.URL.Path != "/v1/domains/example.com" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-API-Key") != "test-key" || r.Header.Get("X-API-Secret") != "test-secret" {
					t.Error("missing authentication")
				}
				var body map[string]any
				if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if _, ok := body["domain"]; ok {
					t.Error("path param leaked into body")
				}
				if body["years"] != float64(2) || body["autoRenew"] != false {
					t.Errorf("wrong options %v", body)
				}
				w.Header().Set("Spaceship-Async-OperationId", header)
				w.Header().Set("Set-Cookie", "secret=cookie")
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			result, err := executeIntegrationTool(&app, tool, map[string]string{"api_key": "test-key", "api_secret": "test-secret"}, map[string]any{"domain": "example.com", "years": 2, "autoRenew": false, "privacyProtection": map[string]any{"level": "high", "userConsent": true}, "contacts": map[string]any{"registrant": "1ZdMXpapqp9sle5dl8BlppTJXAzf5"}}, "")
			if err != nil {
				t.Fatal(err)
			}
			if !result.Success || result.Status != 202 || seen != 1 {
				t.Fatalf("unexpected submission %+v calls=%d", result, seen)
			}
			data, ok := result.Data.(map[string]any)
			if !ok {
				t.Fatalf("expected JSON result, got %T", result.Data)
			}
			if header != "" && data["operation_id"] != header {
				t.Fatal(data)
			}
			if header == "" && data["operation_id"] != nil {
				t.Fatal("fabricated operation")
			}
			if len(data) > 1 {
				t.Fatal("unexpected headers copied into result")
			}
		})
	}
}
