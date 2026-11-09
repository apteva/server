package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestAppOAuthReconnectPreservesConnection130(t *testing.T) {
	const channel = "UCabcdefghijklmnopqrstuv"
	for _, tc := range []struct {
		name, providerChannel, providerError, want    string
		cancel, changeCredentials, duringVerification bool
	}{
		{name: "same channel", providerChannel: channel, want: "authorized"},
		{name: "wrong channel", providerChannel: "UCzyxwvutsrqponmlkjihgfe", want: "wrong_account"},
		{name: "Google cancel", providerError: "access_denied", want: "cancelled"},
		{name: "Social cancel", providerChannel: channel, cancel: true, want: "cancelled"},
		{name: "cancel while checking", providerChannel: channel, duringVerification: true, cancel: true, want: "cancelled"},
		{name: "stale consent", providerChannel: channel, duringVerification: true, changeCredentials: true, want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ensureTestAdmin(t, s)
			install := managedConnectionTestInstall(t, s, "social", sdk.PermOAuthStart)
			var verifyAction func()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/token":
					if r.FormValue("code") != "test-code" {
						t.Error("missing candidate authorization code")
					}
					fmt.Fprint(w, `{"access_token":"candidate","refresh_token":"new-refresh","expires_in":3600}`)
				case "/channels":
					if r.Header.Get("Authorization") != "Bearer candidate" || r.URL.Query().Get("mine") != "true" {
						t.Error("identity must be read with the candidate credentials")
					}
					if verifyAction != nil {
						verifyAction()
					}
					fmt.Fprintf(w, `{"items":[{"id":%q}]}`, tc.providerChannel)
				default:
					t.Errorf("unexpected provider call: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			app := reconnectTestTemplate("youtube-api", "get_my_channel", "/channels?mine=true", upstream.URL)
			s.catalog = NewAppCatalog()
			s.catalog.apps[app.Slug] = app
			old, err := Encrypt(s.secret, `{"access_token":"original","refresh_token":"original-refresh","client_id":"test-client","scope":"old-scope","extra_setting":"keep-me"}`)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := s.store.CreateConnectionExt(ConnectionInput{UserID: 1, AppSlug: app.Slug, AppName: "YouTube", Name: "Black Iron", AuthType: "oauth2",
				EncryptedCreds: old, ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: install})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.db.Exec("UPDATE connections SET id=130 WHERE id=?", conn.ID); err != nil {
				t.Fatal(err)
			}
			call := func(suffix string, body map[string]any) appOAuthReconnect {
				t.Helper()
				raw, _ := json.Marshal(body)
				req := httptest.NewRequest(http.MethodPost, "/apps/callback/oauth/reconnect"+suffix, bytes.NewReader(raw))
				req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(install))
				rec := httptest.NewRecorder()
				s.handleAppCallback(rec, req)
				var out appOAuthReconnect
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
					t.Fatalf("reconnect request failed: %d %s", rec.Code, rec.Body.String())
				}
				return out
			}
			start := call("", map[string]any{"connection_id": 130, "project_id": "proj-1",
				"expected_identities": []oauthReconnectIdentity{{Kind: "youtube", ID: channel}}})
			authorize, err := url.Parse(start.AuthorizeURL)
			if err != nil || start.ConnectionID != 130 || start.Status != "pending" || authorize.Query().Get("state") == "" {
				t.Fatalf("unexpected start: %+v", start)
			}
			status := func(cancel bool) appOAuthReconnect {
				return call("/status", map[string]any{"id": start.ID, "project_id": "proj-1", "cancel": cancel})
			}
			wantCreds := old
			action := func() {
				if tc.cancel {
					status(true)
				}
				if tc.changeCredentials {
					wantCreds, _ = Encrypt(s.secret, `{"access_token":"concurrent-change"}`)
					if err := s.store.UpdateConnectionCredentials(130, wantCreds); err != nil {
						t.Error(err)
					}
				}
			}
			if tc.duringVerification {
				verifyAction = action
			} else {
				action()
			}
			query := url.Values{"state": {authorize.Query().Get("state")}, "code": {"test-code"}, "error": {tc.providerError}}
			s.handleLocalOAuthCallback(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/oauth/local/callback?"+query.Encode(), nil))
			if result := status(false); result.Status != tc.want || result.ConnectionID != 130 {
				t.Fatalf("reconnect result: %+v, want %s on connection 130", result, tc.want)
			}
			existing, encrypted, err := s.store.GetConnection(1, 130)
			if err != nil || existing.Name != "Black Iron" || existing.Status != "active" {
				t.Fatalf("existing connection lost: %+v %v", existing, err)
			}
			var count int
			if err := s.store.db.QueryRow("SELECT count(*) FROM connections").Scan(&count); err != nil || count != 1 {
				t.Fatalf("connections=%d, err=%v", count, err)
			}
			if tc.want == "authorized" {
				current, err := s.reconnectCredentials(encrypted)
				if err != nil || current["refresh_token"] != "new-refresh" || current["access_token"] != "candidate" ||
					current["extra_setting"] != "keep-me" || current["scope"] != "" {
					t.Fatal("verified credentials were not persisted cleanly on the same connection")
				}
			} else if encrypted != wantCreds {
				t.Fatal("unverified authorization replaced existing credentials")
			}
		})
	}
}

func reconnectTestTemplate(slug, tool, path, base string) *AppTemplate {
	return &AppTemplate{Slug: slug, BaseURL: base, Auth: AppAuthConfig{
		Headers: map[string]string{"Authorization": "Bearer {{token}}"},
		OAuth2:  &OAuthConfig{AuthorizeURL: base + "/authorize", TokenURL: base + "/token"},
	}, Tools: []AppToolDef{{Name: tool, Method: http.MethodGet, Path: path}}}
}

func TestOAuthReconnectProviderIdentities(t *testing.T) {
	for _, tc := range []struct {
		slug, tool, body string
		expected         []oauthReconnectIdentity
		want             bool
	}{
		{"twitter-api", "get_me", `{"data":{"id":"321"}}`, []oauthReconnectIdentity{{"twitter", "321"}}, true},
		{"twitter-api", "get_me", `{"data":{"id":"654"}}`, []oauthReconnectIdentity{{"twitter", "321"}}, false},
		{"tiktok-api", "get_user_info", `{"data":{"user":{"open_id":"owner-1"}}}`, []oauthReconnectIdentity{{"tiktok", "owner-1"}}, true},
		{"tiktok-api", "get_user_info", `{"data":{"user":{"open_id":"owner-2"}}}`, []oauthReconnectIdentity{{"tiktok", "owner-1"}}, false},
		{"pinterest", "get_user_account", `{"id":"321","username":"owner"}`, []oauthReconnectIdentity{{"pinterest", "321"}}, true},
		{"pinterest", "get_user_account", `{"id":"654","username":"owner"}`, []oauthReconnectIdentity{{"pinterest", "321"}}, false},
		{"facebook-api", "list_pages", `{"data":[{"id":"123","access_token":"page-token","instagram_business_account":{"id":"456"}}]}`, []oauthReconnectIdentity{{"facebook", "123"}, {"instagram", "456"}}, true},
		{"facebook-api", "list_pages", `{"data":[{"id":"123","access_token":"page-token","instagram_business_account":{"id":"789"}}]}`, []oauthReconnectIdentity{{"facebook", "123"}, {"instagram", "456"}}, false},
		{"facebook-api", "list_pages", `{"data":[{"id":"123","instagram_business_account":{"id":"456"}}]}`, []oauthReconnectIdentity{{"facebook", "123"}, {"instagram", "456"}}, false},
	} {
		t.Run(fmt.Sprintf("%s/match=%t", tc.slug, tc.want), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			app := reconnectTestTemplate(tc.slug, tc.tool, "/identity", upstream.URL)
			if !validReconnectIdentities(tc.slug, tc.expected) {
				t.Fatal("valid identity was rejected")
			}
			got, err := verifyOAuthReconnectIdentities(httptest.NewRequest(http.MethodGet, "/", nil), app, map[string]string{"access_token": "candidate"}, tc.expected)
			if err != nil || got != tc.want {
				t.Fatalf("match=%t err=%v, want %t", got, err, tc.want)
			}
		})
	}
}
