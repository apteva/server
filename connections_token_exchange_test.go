package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestCredentialTokenExchangeResolvesURLTemplate(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/security/oauth2/token" {
			t.Fatalf("token path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"templated-token","expires_in":1800}`))
	}))
	defer tokenServer.Close()

	credentials := map[string]string{
		"token_host": tokenServer.URL,
		"client_id":  "client",
		"secret":     "secret",
	}
	app := &AppTemplate{
		Auth: AppAuthConfig{
			TokenExchange: &CredentialTokenExchangeConfig{
				URL:         "{{credential.token_host}}/v1/security/oauth2/token",
				ContentType: "application/x-www-form-urlencoded",
				BodyParams: map[string]string{
					"grant_type":    "client_credentials",
					"client_id":     "{{credential.client_id}}",
					"client_secret": "{{credential.secret}}",
				},
			},
		},
	}

	changed, err := ensureCredentialExchangeToken(app, credentials, false)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !changed || credentials["access_token"] != "templated-token" {
		t.Fatalf("changed=%v credentials=%#v", changed, credentials)
	}
}

func TestCredentialTokenExchangeSelectsURLFromCredential(t *testing.T) {
	selector := &CredentialTokenURLSelector{
		CredentialField: "credential_version",
		Values: map[string]string{
			"3.1": "https://api.amazon.com/auth/o2/token",
			"3.2": "https://api.amazon.co.uk/auth/o2/token",
			"3.3": "https://api.amazon.co.jp/auth/o2/token",
		},
	}
	cfg := &CredentialTokenExchangeConfig{
		URL:         "https://api.amazon.com/auth/o2/token",
		URLSelector: selector,
	}

	got, err := credentialTokenExchangeURL(cfg, map[string]string{"credential_version": "3.2"})
	if err != nil {
		t.Fatalf("select exchange URL: %v", err)
	}
	if want := "https://api.amazon.co.uk/auth/o2/token"; got != want {
		t.Fatalf("exchange URL=%q want=%q", got, want)
	}
	if _, err := credentialTokenExchangeURL(cfg, map[string]string{"credential_version": "2.1"}); err == nil {
		t.Fatal("expected unsupported credential version error")
	}
}

func TestAmazonAssociatesCatalogUsesCredentialExchange(t *testing.T) {
	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	app := catalog.Get("amazon-associates")
	if app == nil || app.Auth.TokenExchange == nil {
		t.Fatal("Amazon Associates token exchange missing")
	}
	if got := app.Auth.Headers["Authorization"]; got != "Bearer {{access_token}}" {
		t.Fatalf("authorization header=%q", got)
	}
	wantFields := []string{"credential_id", "credential_secret", "credential_version", "marketplace"}
	if len(app.Auth.CredentialFields) != len(wantFields) {
		t.Fatalf("credential fields=%#v", app.Auth.CredentialFields)
	}
	for i, want := range wantFields {
		if got := app.Auth.CredentialFields[i].Name; got != want {
			t.Fatalf("credential field %d=%q want=%q", i, got, want)
		}
	}
	credentials := applyCredentialFieldDefaults(app, map[string]string{
		"credential_id":     "creator-id",
		"credential_secret": "creator-secret",
	})
	if credentials["credential_version"] != "3.1" || credentials["marketplace"] != "www.amazon.com" {
		t.Fatalf("Amazon credential defaults not applied: %#v", credentials)
	}
	if got, err := credentialTokenExchangeURL(app.Auth.TokenExchange, credentials); err != nil || got != "https://api.amazon.com/auth/o2/token" {
		t.Fatalf("default exchange URL=%q err=%v", got, err)
	}
}

func TestCredentialTokenExchangeCJJSONExpiryAndCachedReuse(t *testing.T) {
	expiresAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	var exchanges atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type=%q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode exchange body: %v", err)
		}
		if got := body["apiKey"]; got != "cj-api-key" {
			t.Fatalf("apiKey=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result":  true,
			"message": "Success",
			"data": map[string]any{
				"accessToken":           "cj-access-token",
				"accessTokenExpiryDate": expiresAt.Format(time.RFC3339),
			},
			// The absolute timestamp must win even when a duration exists.
			"expires_in": 1,
		})
	}))
	defer tokenServer.Close()

	app := cjTokenExchangeTestApp(tokenServer.URL)
	credentials := map[string]string{"api_key": "cj-api-key"}
	changed, err := ensureCredentialExchangeToken(app, credentials, false)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !changed || credentials["access_token"] != "cj-access-token" {
		t.Fatalf("changed=%v credentials=%#v", changed, credentials)
	}
	parsed, err := time.Parse(time.RFC3339, credentials["token_expires_at"])
	if err != nil {
		t.Fatalf("parse cached expiry: %v", err)
	}
	if !parsed.Equal(expiresAt) {
		t.Fatalf("expiry=%s want=%s", parsed, expiresAt)
	}

	changed, err = ensureCredentialExchangeToken(app, credentials, false)
	if err != nil {
		t.Fatalf("cached exchange: %v", err)
	}
	if changed || exchanges.Load() != 1 {
		t.Fatalf("changed=%v exchanges=%d, want cached reuse", changed, exchanges.Load())
	}
}

func TestCJDropshippingCatalogUsesAPIKeyExchange(t *testing.T) {
	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	app := catalog.Get("cjdropshipping")
	if app == nil || app.Auth.TokenExchange == nil {
		t.Fatal("CJ Dropshipping token exchange missing")
	}
	if got := app.Auth.TokenExchange.AccessTokenPath; got != "data.accessToken" {
		t.Fatalf("access_token_path=%q", got)
	}
	if got := app.Auth.TokenExchange.ExpiresAtPath; got != "data.accessTokenExpiryDate" {
		t.Fatalf("expires_at_path=%q", got)
	}
	if len(app.Auth.CredentialFields) != 1 || app.Auth.CredentialFields[0].Name != "api_key" {
		t.Fatalf("credential fields=%#v", app.Auth.CredentialFields)
	}
}

func TestCredentialTokenExchangeCJForcedRetryAfterUnauthorized(t *testing.T) {
	var exchanges atomic.Int32
	var calls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode exchange body: %v", err)
		}
		if body["apiKey"] != "cj-api-key" {
			t.Fatalf("unexpected exchange body: %#v", body)
		}
		n := exchanges.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "Success",
			"data": map[string]any{
				"accessToken":           fmt.Sprintf("cj-token-%d", n),
				"accessTokenExpiryDate": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
			},
		})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if got, want := r.Header.Get("CJ-Access-Token"), fmt.Sprintf("cj-token-%d", n); got != want {
			t.Fatalf("CJ-Access-Token=%q want=%q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer apiServer.Close()

	app := cjTokenExchangeTestApp(tokenServer.URL)
	app.BaseURL = apiServer.URL
	tool := &AppToolDef{
		Name:        "products_search",
		Method:      http.MethodGet,
		Path:        "/products",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
	credentials := map[string]string{"api_key": "cj-api-key"}
	persisted := 0
	result, err := executeIntegrationToolWithRefresh(app, tool, credentials, map[string]any{}, "", func(map[string]string) error {
		persisted++
		return nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v", result)
	}
	if exchanges.Load() != 2 || calls.Load() != 2 || persisted != 2 {
		t.Fatalf("exchanges=%d calls=%d persisted=%d", exchanges.Load(), calls.Load(), persisted)
	}
}

func TestCredentialTokenExchangeCJInvalidKeyUsesSafeProviderMessage(t *testing.T) {
	const apiKey = "cj-secret-api-key"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":false,"message":"invalid API key","data":null,"debug":"cj-secret-api-key"}`))
	}))
	defer tokenServer.Close()

	credentials := map[string]string{"api_key": apiKey}
	changed, err := ensureCredentialExchangeToken(cjTokenExchangeTestApp(tokenServer.URL), credentials, false)
	if changed || err == nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got, want := err.Error(), "CJ Dropshipping: invalid API key"; got != want {
		t.Fatalf("error=%q want=%q", got, want)
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("error leaked API key: %q", err)
	}
}

func TestMarkLegacyCJDropshippingConnectionsForReconnect(t *testing.T) {
	s := newTestServer(t)
	secret := []byte("01234567890123456789012345678901")
	s.secret = secret
	user, err := s.store.CreateUser("cj-migration@test.local", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	create := func(name string, credentials map[string]string) *Connection {
		t.Helper()
		raw, _ := json.Marshal(credentials)
		encrypted, err := Encrypt(secret, string(raw))
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		connection, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID: user.ID, AppSlug: "cjdropshipping", AppName: "CJ Dropshipping",
			Name: name, AuthType: "api_key", EncryptedCreds: encrypted, Status: "active",
		})
		if err != nil {
			t.Fatalf("create connection: %v", err)
		}
		return connection
	}
	legacy := create("legacy", map[string]string{"token": "manually-generated-token"})
	current := create("current", map[string]string{"api_key": "cj-api-key", "access_token": "cached-token"})

	marked, err := s.store.MarkLegacyCJDropshippingConnectionsForReconnect(secret)
	if err != nil {
		t.Fatalf("mark legacy: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked=%d want=1", marked)
	}
	legacyRow, _, _ := s.store.GetConnection(user.ID, legacy.ID)
	currentRow, _, _ := s.store.GetConnection(user.ID, current.ID)
	if legacyRow.Status != "failed" || currentRow.Status != "active" {
		t.Fatalf("legacy=%q current=%q", legacyRow.Status, currentRow.Status)
	}
	marked, err = s.store.MarkLegacyCJDropshippingConnectionsForReconnect(secret)
	if err != nil || marked != 0 {
		t.Fatalf("second pass marked=%d err=%v", marked, err)
	}
}

func cjTokenExchangeTestApp(exchangeURL string) *AppTemplate {
	return &AppTemplate{
		Slug: "cjdropshipping",
		Name: "CJ Dropshipping",
		Auth: AppAuthConfig{
			Headers: map[string]string{"CJ-Access-Token": "{{access_token}}"},
			TokenExchange: &CredentialTokenExchangeConfig{
				URL:             exchangeURL,
				Method:          http.MethodPost,
				ContentType:     "application/json",
				BodyParams:      map[string]string{"apiKey": "{{credential.api_key}}"},
				AccessTokenPath: "data.accessToken",
				ExpiresAtPath:   "data.accessTokenExpiryDate",
			},
		},
	}
}
