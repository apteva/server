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

// A global connection is visible from every project, so its OAuth client is
// the project's setup unless the project has its own.
func TestOAuthSetupFallsBackToGlobalConnection(t *testing.T) {
	s, app := oauthSetupFixture(t)
	t.Setenv("OAUTH_SETUP_TEST_CLIENT_ID", "env-id")
	t.Setenv("OAUTH_SETUP_TEST_CLIENT_SECRET", "env-secret")
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "global", Status: "active"}, map[string]string{"api_host": "sandbox.example", "client_id": "global-id", "client_secret": "global-secret", "token": "never-copy"})
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "other-project", ProjectID: "proj-2", Status: "active"}, map[string]string{"api_host": "production.example", "client_id": "other-id", "client_secret": "other-secret"})
	setup, err := s.findStoredOAuthSetup(1, "proj-1", app)
	if err != nil || setup.ClientID != "global-id" || setup.ClientSecret != "global-secret" || setup.Credentials["api_host"] != "sandbox.example" || setup.Credentials["token"] != "" {
		t.Fatalf("global setup not used: %+v %v", setup, err)
	}

	// The project's own setup wins over a newer global one.
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "project", ProjectID: "proj-1", Status: "pending"}, map[string]string{"api_host": "production.example", "client_id": "project-id", "client_secret": "project-secret"})
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "newer-global", Status: "active"}, map[string]string{"api_host": "sandbox.example", "client_id": "newer-global-id", "client_secret": "newer-global-secret"})
	if setup, err = s.findStoredOAuthSetup(1, "proj-1", app); err != nil || setup.ClientID != "project-id" {
		t.Fatalf("project setup replaced by global: %+v %v", setup, err)
	}
	// Projects without their own setup take the newest global one.
	if setup, err = s.findStoredOAuthSetup(1, "proj-3", app); err != nil || setup.ClientID != "newer-global-id" {
		t.Fatalf("newest global setup not used: %+v %v", setup, err)
	}
}

func TestOAuthSetupGlobalOperatorBeatsGlobalOwnedAndProjectOwnedBeatsGlobal(t *testing.T) {
	s, app := oauthSetupFixture(t)
	owner := seedInstall(t, s, "social", "")
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "global-operator", Status: "active"}, map[string]string{"api_host": "production.example", "client_id": "operator-id", "client_secret": "operator-secret"})
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "global-owned", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner}, map[string]string{"api_host": "sandbox.example", "client_id": "global-owned-id", "client_secret": "global-owned-secret"})
	if setup, err := s.findStoredOAuthSetup(1, "proj-1", app, owner); err != nil || setup.ClientID != "operator-id" {
		t.Fatalf("global operator setup not authoritative: %+v %v", setup, err)
	}
	saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "project-owned", ProjectID: "proj-1", Status: "active", CreatedVia: "app_install", OwnerAppInstallID: owner}, map[string]string{"api_host": "sandbox.example", "client_id": "project-owned-id", "client_secret": "project-owned-secret"})
	if setup, err := s.findStoredOAuthSetup(1, "proj-1", app, owner); err != nil || setup.ClientID != "project-owned-id" {
		t.Fatalf("project account not preferred over global setup: %+v %v", setup, err)
	}
}

// The reported case: Social in a project with no Facebook/YouTube connection
// of its own, and a global connection holding the OAuth client.
func TestOAuthStartUsesGlobalCatalogClient(t *testing.T) {
	for _, slug := range []string{"facebook-api", "youtube-api", "tiktok-api", "twitter-api", "pinterest"} {
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
			s.catalog.apps[app.Slug] = &app
			creds := map[string]string{"client_id": "global-client", "client_secret": "global-secret", "access_token": "never-share"}
			for _, field := range app.Auth.CredentialFields {
				if field.Source == "user" && !field.Hidden && field.Default != "" {
					creds[field.Name] = field.Default
				}
			}
			saveOAuthSetupFixture(t, s, &app, ConnectionInput{Name: "global", Status: "active"}, creds)
			manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "social"}
			manifest.Requires.Permissions = []sdk.Permission{sdk.PermOAuthStart}
			installID := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
			req := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"integration_slug":"`+slug+`","return_url":"https://app.example/return","name":"new-account"}`))
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
			if !strings.Contains(result.AuthorizeURL, url.QueryEscape("global-client")) {
				t.Fatalf("authorization did not use the global client: %s", result.AuthorizeURL)
			}
			conn, _, err := s.store.GetConnection(1, result.ConnectionID)
			if err != nil || conn.ProjectID != "proj-1" || conn.Status != "pending" {
				t.Fatalf("new account not created in the caller's project: %+v %v", conn, err)
			}
		})
	}
}
