package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func oauthSetupFixture(t *testing.T) (*Server, *AppTemplate) {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	required, optional := true, false
	app := &AppTemplate{Slug: "setup-test", Name: "Setup Test", Auth: AppAuthConfig{
		Types: []string{"oauth2"},
		CredentialFields: []CredentialField{
			{Name: "api_host", Source: "user", Required: &required, Default: "production.example", Options: []string{"production.example", "sandbox.example"}},
			{Name: "tenant", Source: "user", Required: &optional},
			{Name: "token", Source: "oauth", Hidden: true},
		},
		OAuth2: &OAuthConfig{AuthorizeURL: "https://authorize.example/oauth", TokenURL: "https://{{credential.api_host}}/token", ClientIDRequired: true, ClientSecretRequired: true},
	}}
	s.catalog = NewAppCatalog()
	s.catalog.apps[app.Slug] = app
	return s, app
}

func saveOAuthSetupFixture(t *testing.T, s *Server, app *AppTemplate, input ConnectionInput, creds map[string]string) *Connection {
	t.Helper()
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := Encrypt(s.secret, string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	input.AppSlug, input.AppName, input.EncryptedCreds = app.Slug, app.Name, encrypted
	if input.UserID == 0 {
		input.UserID = 1
	}
	if input.AuthType == "" {
		input.AuthType = "oauth2"
	}
	if input.Name == "" {
		input.Name = fmt.Sprintf("setup-%d", len(encoded))
	}
	conn, err := s.store.CreateConnectionExt(input)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestOAuthSetupPendingKeepsEnvironmentAndClientTogether(t *testing.T) {
	s, app := oauthSetupFixture(t)
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "production", ProjectID: "proj-1", Status: "active"}, map[string]string{
		"api_host": "production.example", "client_id": "prod-id", "client_secret": "prod-secret",
	})
	saved := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "sandbox", ProjectID: "proj-1", Status: "pending"}, map[string]string{
		"api_host": "sandbox.example", "client_id": "sandbox-id", "client_secret": "sandbox-secret", "tenant": "demo",
		"token": "never-copy-access", "refresh_token": "never-copy-refresh",
	})
	// None of these newer rows is an authorized shared setup source.
	for _, input := range []ConnectionInput{
		{Name: "app-account", ProjectID: "proj-1", Status: "pending", CreatedVia: "app_install"},
		{Name: "app-active", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install"},
		{Name: "other-project", ProjectID: "proj-2", Status: "active"},
		{Name: "global", Status: "active"},
		{Name: "failed", ProjectID: "proj-1", Status: "failed"},
		{Name: "not-oauth", ProjectID: "proj-1", Status: "pending", AuthType: "bearer"},
	} {
		saveOAuthSetupFixture(t, s, app, input, map[string]string{"api_host": "production.example", "client_id": "wrong-id", "client_secret": "wrong-secret"})
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "oauth-setup-test"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermOAuthStart}
	installID := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
	req := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"integration_slug":"setup-test","return_url":"https://app.example/return","name":"new-account"}`))
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleCallbackOAuth(response, req, []string{"start"})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	var result struct {
		ConnectionID int64  `json:"connection_id"`
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(result.AuthorizeURL)
	if err != nil || u.Query().Get("client_id") != "sandbox-id" {
		t.Fatalf("wrong authorization client: %s", result.AuthorizeURL)
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
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatal(err)
	}
	if creds["api_host"] != "sandbox.example" || creds["client_id"] != "sandbox-id" || creds["client_secret"] != "sandbox-secret" || creds["tenant"] != "demo" {
		t.Fatal("setup fields and client were not copied together")
	}
	if creds["token"] != "" || creds["refresh_token"] != "" {
		t.Fatal("account tokens copied into new flow")
	}
	if conn.Status != "pending" || conn.OwnerAppInstallID != installID || conn.ProjectID != "proj-1" {
		t.Fatalf("unexpected new account metadata: %+v", conn)
	}
	old, _, err := s.store.GetConnection(1, saved.ID)
	if err != nil || old.Status != "pending" {
		t.Fatal("setup record was authorized by reuse")
	}
}

func TestOAuthSetupRejectsIncompleteOrInvalidNewestConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, field, value string }{
		{"missing-host", "api_host", ""},
		{"invalid-host", "api_host", "attacker.example"},
		{"missing-id", "client_id", ""},
		{"missing-secret", "client_secret", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, app := oauthSetupFixture(t)
			saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "older", ProjectID: "proj-1", Status: "active"}, map[string]string{"api_host": "production.example", "client_id": "old-id", "client_secret": "old-secret"})
			creds := map[string]string{"api_host": "sandbox.example", "client_id": "new-id", "client_secret": "new-secret"}
			creds[tc.field] = tc.value
			saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "newer", ProjectID: "proj-1", Status: "pending"}, creds)
			t.Setenv("OAUTH_SETUP_TEST_CLIENT_ID", "env-id")
			t.Setenv("OAUTH_SETUP_TEST_CLIENT_SECRET", "env-secret")
			if _, err := s.findStoredOAuthSetup(1, "proj-1", app); err == nil {
				t.Fatal("incomplete setup accepted or replaced by unrelated credentials")
			}
		})
	}
}

func TestOAuthSetupScopeAndPublicClient(t *testing.T) {
	s, app := oauthSetupFixture(t)
	saveOAuthSetupFixture(t, s, app, ConnectionInput{ProjectID: "proj-2", Status: "pending"}, map[string]string{"api_host": "sandbox.example", "client_id": "other-id", "client_secret": "other-secret"})
	if _, err := s.findStoredOAuthSetup(1, "proj-1", app); err == nil {
		t.Fatal("other project's setup or catalog default accepted")
	}
	if _, err := s.findStoredOAuthSetup(1, "", app); err == nil {
		t.Fatal("unscoped lookup read project configuration")
	}
	if _, err := s.findStoredOAuthSetup(2, "proj-2", app); err == nil {
		t.Fatal("another user's setup accepted")
	}
	// Public PKCE clients do not need a secret, even when Basic auth is
	// preferred for deployments which do have one.
	app.Auth.CredentialFields = nil
	app.Auth.OAuth2.ClientSecretRequired = false
	app.Auth.OAuth2.PKCE = true
	app.Auth.OAuth2.TokenAuthBasicOnly = true
	t.Setenv("OAUTH_SETUP_TEST_CLIENT_ID", "public-id")
	t.Setenv("OAUTH_SETUP_TEST_CLIENT_SECRET", "")
	setup, err := s.findStoredOAuthSetup(1, "proj-1", app)
	if err != nil || setup.ClientID != "public-id" || setup.ClientSecret != "" {
		t.Fatalf("public client rejected: %v", err)
	}
}

func TestOAuthSetupCallbackRejectsOtherProject(t *testing.T) {
	s, _ := oauthSetupFixture(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "oauth-scope-test"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermOAuthStart}
	installID := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
	req := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"integration_slug":"setup-test","project_id":"proj-2","return_url":"https://app.example/return"}`))
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleCallbackOAuth(response, req, []string{"start"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
}

func TestPinterestOAuthSetupRequiresSecretAndPreservesSandbox(t *testing.T) {
	s, _ := oauthSetupFixture(t)
	raw, err := os.ReadFile("integrations-catalog/pinterest.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	if err := validateOAuthSetupClient(&app, "trial-client", ""); err == nil {
		t.Fatal("Pinterest catalog did not require a secret")
	}
	saveOAuthSetupFixture(t, s, &app, ConnectionInput{ProjectID: "proj-1", Status: "pending"}, map[string]string{"api_host": "api-sandbox.pinterest.com", "client_id": "trial-client", "client_secret": "trial-secret"})
	setup, err := s.findStoredOAuthSetup(1, "proj-1", &app)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Credentials["api_host"] != "api-sandbox.pinterest.com" {
		t.Fatal("sandbox environment replaced")
	}
}
