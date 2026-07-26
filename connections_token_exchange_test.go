package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestCredentialTokenExchangeAndUnauthorizedRetry(t *testing.T) {
	var exchanges atomic.Int32
	var apiCalls atomic.Int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("token content type=%q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if form.Get("name") != "automation" || form.Get("secret") != "secret-value" {
			t.Fatalf("unexpected token form: %v", form)
		}
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('0'+n)),
			"expires_in":   14400,
		})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		want := "Bearer token-" + string(rune('0'+n))
		if got := r.Header.Get("Authorization"); got != want {
			t.Fatalf("authorization=%q want=%q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer apiServer.Close()

	app := &AppTemplate{
		Slug:    "token-exchange-test",
		BaseURL: apiServer.URL,
		Auth: AppAuthConfig{
			Headers: map[string]string{"Authorization": "Bearer {{access_token}}"},
			TokenExchange: &CredentialTokenExchangeConfig{
				URL:         tokenServer.URL,
				ContentType: "application/x-www-form-urlencoded",
				BodyParams: map[string]string{
					"name":   "{{credential.api_key_name}}",
					"secret": "{{credential.api_key_secret}}",
				},
			},
		},
	}
	tool := &AppToolDef{
		Name:        "list",
		Method:      http.MethodGet,
		Path:        "/items",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
	credentials := map[string]string{
		"api_key_name":   "automation",
		"api_key_secret": "secret-value",
	}
	persisted := 0
	result, err := executeIntegrationToolWithRefresh(
		app,
		tool,
		credentials,
		map[string]any{},
		"",
		func(map[string]string) error {
			persisted++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v", result)
	}
	if exchanges.Load() != 2 || apiCalls.Load() != 2 {
		t.Fatalf("exchanges=%d api_calls=%d", exchanges.Load(), apiCalls.Load())
	}
	if persisted != 2 {
		t.Fatalf("persisted=%d want=2", persisted)
	}
	if credentials["access_token"] != "token-2" || credentials["token_expires_at"] == "" {
		t.Fatalf("credentials not updated: %#v", credentials)
	}
}
