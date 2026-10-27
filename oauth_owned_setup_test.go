package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestOAuthSetupReusesOnlyCallingInstallActiveAccount(t *testing.T) {
	for _, authType := range []string{"oauth1", "oauth2"} {
		t.Run(authType, func(t *testing.T) {
			s, app := oauthSetupFixture(t)
			owner := seedInstall(t, s, "social", "proj-1")
			other := seedInstall(t, s, "unrelated", "proj-1")
			saved := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "prior-social-account", ProjectID: "proj-1", Status: "active", AuthType: authType, CreatedVia: "app_install", OwnerAppInstallID: owner}, map[string]string{"api_host": "sandbox.example", "tenant": "same-tenant", "client_id": "saved-client", "client_secret": "saved-secret", "token": "existing-access", "access_token": "existing-access-alias", "refresh_token": "existing-refresh", "oauth_token": "oauth1-account", "oauth_token_secret": "oauth1-secret"})
			for _, input := range []ConnectionInput{
				{Name: "other-app", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: other},
				{Name: "other-project", ProjectID: "proj-2", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "global-account", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "pending-account", ProjectID: "proj-1", Status: "pending", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "failed-account", ProjectID: "proj-1", Status: "failed", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "disconnected-account", ProjectID: "proj-1", Status: "disconnected", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "non-oauth-account", ProjectID: "proj-1", Status: "active", AuthType: "bearer", CreatedVia: "app_install", OwnerAppInstallID: owner},
				{Name: "owner-missing", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install"},
			} {
				saveOAuthSetupFixture(t, s, app, input, map[string]string{"api_host": "production.example", "client_id": "wrong-client", "client_secret": "wrong-secret"})
			}
			setup, err := s.findStoredOAuthSetup(1, "proj-1", app, owner)
			if err != nil || setup.ClientID != "saved-client" || setup.ClientSecret != "saved-secret" || setup.Credentials["api_host"] != "sandbox.example" || setup.Credentials["tenant"] != "same-tenant" {
				t.Fatalf("owned setup not reused: %+v %v", setup, err)
			}
			for _, key := range []string{"token", "access_token", "refresh_token", "oauth_token", "oauth_token_secret"} {
				if _, ok := setup.Credentials[key]; ok {
					t.Fatalf("account credential copied: %s", key)
				}
			}
			_, before, _ := s.store.GetConnection(1, saved.ID)
			if _, err = s.findStoredOAuthSetup(1, "proj-1", app); err == nil {
				t.Fatal("operator lookup reused an app-owned account")
			}
			_, after, _ := s.store.GetConnection(1, saved.ID)
			if before != after {
				t.Fatal("lookup modified existing account credentials")
			}
		})
	}
}

func TestOAuthSetupOperatorConfigurationRemainsAuthoritative(t *testing.T) {
	s, app := oauthSetupFixture(t)
	owner := seedInstall(t, s, "social", "proj-1")
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "operator", ProjectID: "proj-1", Status: "pending"}, map[string]string{"api_host": "production.example", "client_id": "operator-client", "client_secret": "operator-secret"})
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "newer-account", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner}, map[string]string{"api_host": "sandbox.example", "client_id": "owned-client", "client_secret": "owned-secret"})
	setup, err := s.findStoredOAuthSetup(1, "proj-1", app, owner)
	if err != nil || setup.ClientID != "operator-client" || setup.Credentials["api_host"] != "production.example" {
		t.Fatalf("operator setup replaced: %+v %v", setup, err)
	}
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "incomplete-operator", ProjectID: "proj-1", Status: "pending"}, map[string]string{"api_host": "sandbox.example", "client_id": "new-operator"})
	if _, err = s.findStoredOAuthSetup(1, "proj-1", app, owner); err == nil {
		t.Fatal("incomplete operator setup silently fell back to owned account")
	}
}

func TestOAuthStartOwnedFallbackCreatesSeparatePendingAccount(t *testing.T) {
	s, app := oauthSetupFixture(t)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "new-account-code" || r.Form.Get("client_id") != "same-client" || r.Form.Get("client_secret") != "same-secret" {
			t.Error("token exchange lost the saved client or new account authorization code")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-account-access","refresh_token":"new-account-refresh","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	app.Auth.OAuth2.TokenURL = tokenServer.URL
	// The same callback fixture used by operator-setup tests exercises the broker
	// permission and project checks; only its source is an owned active account.
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "social"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermOAuthStart}
	owner := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
	saved := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "working-account", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner}, map[string]string{"api_host": "sandbox.example", "client_id": "same-client", "client_secret": "same-secret", "token": "never-copy-access", "refresh_token": "never-copy-refresh"})
	_, before, _ := s.store.GetConnection(1, saved.ID)
	req := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"integration_slug":"setup-test","return_url":"https://app.example/return","name":"new-account"}`))
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(owner, 10))
	req.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleCallbackOAuth(response, req, []string{"start"})
	if response.Code != http.StatusOK {
		t.Fatalf("start HTTP %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		ConnectionID int64  `json:"connection_id"`
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(result.AuthorizeURL)
	if err != nil || u.Query().Get("client_id") != "same-client" || result.ConnectionID == saved.ID {
		t.Fatal("new authorization did not use separate account and saved OAuth app")
	}
	conn, encrypted, err := s.store.GetConnection(1, result.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]string
	if err = json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatal(err)
	}
	if len(creds) != 3 || creds["client_id"] != "same-client" || creds["client_secret"] != "same-secret" || creds["api_host"] != "sandbox.example" {
		t.Fatal("unexpected copied configuration or account tokens")
	}
	if conn.Status != "pending" || conn.OwnerAppInstallID != owner || conn.ProjectID != "proj-1" {
		t.Fatalf("incorrect new account ownership: %+v", conn)
	}
	old, after, _ := s.store.GetConnection(1, saved.ID)
	if old.Status != "active" || before != after {
		t.Fatal("existing connected account changed")
	}
	callback := httptest.NewRecorder()
	s.handleLocalOAuthCallback(callback, httptest.NewRequest(http.MethodGet, "/oauth/local/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=new-account-code", nil))
	if callback.Code != http.StatusFound {
		t.Fatalf("callback HTTP %d", callback.Code)
	}
	location, err := url.Parse(callback.Header().Get("Location"))
	if err != nil || location.Query().Get("status") != "ok" || location.Query().Get("conn_id") != strconv.FormatInt(result.ConnectionID, 10) {
		t.Fatal("callback did not hand off the new account identity")
	}
	connected, encrypted, err := s.store.GetConnection(1, result.ConnectionID)
	if err != nil || connected.Status != "active" {
		t.Fatal("new account did not become active")
	}
	plain, err = Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatal(err)
	}
	if creds["access_token"] != "new-account-access" || creds["refresh_token"] != "new-account-refresh" || creds["client_id"] != "same-client" || creds["api_host"] != "sandbox.example" {
		t.Fatal("new authorization tokens or saved setup were lost")
	}
	old, after, _ = s.store.GetConnection(1, saved.ID)
	if old.Status != "active" || before != after {
		t.Fatal("callback changed the existing account")
	}
}

func TestSocialOAuthCatalogOwnedClientReuse(t *testing.T) {
	for _, slug := range []string{"tiktok-api", "youtube-api", "facebook-api", "twitter-api", "pinterest"} {
		t.Run(slug, func(t *testing.T) {
			s, _ := oauthSetupFixture(t)
			raw, err := os.ReadFile("integrations-catalog/" + slug + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var app AppTemplate
			if err = json.Unmarshal(raw, &app); err != nil {
				t.Fatal(err)
			}
			owner := seedInstall(t, s, "social", "proj-1")
			creds := map[string]string{"client_id": "saved-client", "client_secret": "saved-secret", "token": "never-share-access", "refresh_token": "never-share-refresh"}
			for _, field := range app.Auth.CredentialFields {
				if field.Source == "user" && !field.Hidden && field.Default != "" {
					creds[field.Name] = field.Default
				}
			}
			if slug == "pinterest" {
				creds["api_host"] = "api-sandbox.pinterest.com"
			}
			saveOAuthSetupFixture(t, s, &app, ConnectionInput{ProjectID: "proj-1", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner}, creds)
			setup, err := s.findStoredOAuthSetup(1, "proj-1", &app, owner)
			if err != nil || setup.ClientID != "saved-client" || setup.ClientSecret != "saved-secret" {
				t.Fatalf("catalog setup reuse failed: %v", err)
			}
			if setup.Credentials["token"] != "" || setup.Credentials["refresh_token"] != "" {
				t.Fatal("account tokens copied")
			}
			if slug == "pinterest" && setup.Credentials["api_host"] != "api-sandbox.pinterest.com" {
				t.Fatal("Pinterest environment changed")
			}
			if slug == "tiktok-api" {
				if app.Auth.OAuth2 == nil || app.Auth.OAuth2.ClientIDParamName != "client_key" {
					t.Fatal("TikTok OAuth client parameter changed")
				}
				conn, authorize, err := s.startLocalOAuthResolved(1, &app, "new-tiktok", "proj-1", setup.ClientID, setup.ClientSecret, setup.Credentials, owner, "https://app.example/return", nil)
				if err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(authorize)
				if err != nil || u.Query().Get("client_key") != "saved-client" || u.Query().Get("client_id") != "" || conn.Status != "pending" {
					t.Fatal("TikTok authorization URL/client binding incorrect")
				}
			}
		})
	}
}
