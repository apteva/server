package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOAuthReauthProviderErrorPreservesActiveConnection(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.CreateUser("oauth-reauth@test.local", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conn, err := store.CreateConnectionExt(ConnectionInput{
		UserID:   1,
		AppSlug:  "google-sheets",
		AppName:  "Google Sheets",
		Name:     "Google Sheets",
		AuthType: "oauth2",
		Status:   "active",
	})
	if err != nil {
		t.Fatalf("CreateConnectionExt: %v", err)
	}
	state, err := store.mintOAuthState(1, conn.ID, conn.AppSlug, "", time.Minute, 0, "", oauthStatePurposeReauth)
	if err != nil {
		t.Fatalf("mintOAuthState: %v", err)
	}
	s := &Server{store: store}

	req := httptest.NewRequest("GET", "/oauth/local/callback?state="+state+"&error=access_denied", nil)
	rec := httptest.NewRecorder()
	s.handleLocalOAuthCallback(rec, req)

	got, _, err := store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active connection to survive failed re-auth, got %q", got.Status)
	}
}

func TestOAuthConnectProviderErrorMarksPendingConnectionFailed(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.CreateUser("oauth-connect@test.local", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conn, err := store.CreateConnectionExt(ConnectionInput{
		UserID:   1,
		AppSlug:  "google-sheets",
		AppName:  "Google Sheets",
		Name:     "Google Sheets",
		AuthType: "oauth2",
		Status:   "pending",
	})
	if err != nil {
		t.Fatalf("CreateConnectionExt: %v", err)
	}
	state, err := store.mintOAuthState(1, conn.ID, conn.AppSlug, "", time.Minute, 0, "", oauthStatePurposeConnect)
	if err != nil {
		t.Fatalf("mintOAuthState: %v", err)
	}
	s := &Server{store: store}

	req := httptest.NewRequest("GET", "/oauth/local/callback?state="+state+"&error=access_denied", nil)
	rec := httptest.NewRecorder()
	s.handleLocalOAuthCallback(rec, req)

	got, _, err := store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Status != "failed" {
		t.Fatalf("expected pending connection to be failed after connect error, got %q", got.Status)
	}
}

func TestLocalOAuthPreservesSupplementalCredentialsThroughCallback(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("token method=%s, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "code=auth-code") {
			t.Fatalf("token form=%q, missing authorization code", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"oauth-access",
			"refresh_token":"oauth-refresh",
			"expires_in":3600,
			"token_type":"Bearer"
		}`))
	}))
	defer tokenServer.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.CreateUser("oauth-supplemental@test.local", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	app := &AppTemplate{
		Slug: "google-ads-test",
		Name: "Google Ads Test",
		Auth: AppAuthConfig{
			Types: []string{"bearer", "oauth2"},
			OAuth2: &OAuthConfig{
				AuthorizeURL:     "https://accounts.example.test/oauth",
				TokenURL:         "{{credential.token_url}}",
				ClientIDRequired: true,
			},
		},
	}
	catalog := NewAppCatalog()
	catalog.apps[app.Slug] = app
	s := &Server{
		store:   store,
		catalog: catalog,
		secret:  []byte("0123456789abcdef0123456789abcdef"),
		port:    "5280",
	}
	autoMCP := false
	conn, authorizeURL, err := s.startLocalOAuth(
		1,
		app,
		"Google Ads",
		"",
		"oauth-client",
		"oauth-secret",
		map[string]string{
			"developer_token":     "developer-secret",
			"manager_customer_id": "1234567890",
			"token_url":           tokenServer.URL,
		},
		0,
		"",
		&autoMCP,
	)
	if err != nil {
		t.Fatalf("startLocalOAuth: %v", err)
	}

	assertConnectionCredentials := func(want map[string]string) {
		t.Helper()
		_, encrypted, err := store.GetConnection(1, conn.ID)
		if err != nil {
			t.Fatalf("GetConnection: %v", err)
		}
		plain, err := Decrypt(s.secret, encrypted)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		var credentials map[string]string
		if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
			t.Fatalf("credentials JSON: %v", err)
		}
		for key, value := range want {
			if credentials[key] != value {
				t.Fatalf("credential %s=%q, want %q (all keys=%v)", key, credentials[key], value, filterKeys(credentials))
			}
		}
	}
	assertConnectionCredentials(map[string]string{
		"developer_token":     "developer-secret",
		"manager_customer_id": "1234567890",
		"token_url":           tokenServer.URL,
		"client_id":           "oauth-client",
		"client_secret":       "oauth-secret",
	})

	parsedAuthorizeURL, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	state := parsedAuthorizeURL.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL missing state")
	}
	req := httptest.NewRequest(
		http.MethodGet,
		"/oauth/local/callback?state="+url.QueryEscape(state)+"&code=auth-code",
		nil,
	)
	rec := httptest.NewRecorder()
	s.handleLocalOAuthCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", rec.Code, rec.Body.String())
	}

	assertConnectionCredentials(map[string]string{
		"developer_token":     "developer-secret",
		"manager_customer_id": "1234567890",
		"token_url":           tokenServer.URL,
		"client_id":           "oauth-client",
		"client_secret":       "oauth-secret",
		"access_token":        "oauth-access",
		"refresh_token":       "oauth-refresh",
		"expires_in":          "3600",
		"token_type":          "Bearer",
	})
	got, _, err := store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection after callback: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("connection status=%q, want active", got.Status)
	}
}

func TestCollectOAuthSupplementalCredentialsAppliesAndValidatesSelectDefault(t *testing.T) {
	required := true
	app := &AppTemplate{Auth: AppAuthConfig{CredentialFields: []CredentialField{{
		Name: "api_host", Label: "API environment", Source: "user", Required: &required,
		Type: "select", Options: []string{"api.pinterest.com", "api-sandbox.pinterest.com"},
		Default: "api.pinterest.com",
	}}}}

	got, err := collectOAuthSupplementalCredentials(app, nil)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if got["api_host"] != "api.pinterest.com" {
		t.Fatalf("api_host=%q, want production default", got["api_host"])
	}
	if _, err := collectOAuthSupplementalCredentials(app, map[string]string{"api_host": "attacker.invalid"}); err == nil {
		t.Fatal("expected closed select validation error")
	}
}
