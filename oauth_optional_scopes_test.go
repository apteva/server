package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

const testMembershipsScope = "https://www.googleapis.com/auth/youtube.channel-memberships.creator"

// optionalScopesFixture registers an OAuth2 integration with two base scopes
// and one declared optional scope.
func optionalScopesFixture(t *testing.T) (*Server, *AppTemplate) {
	t.Helper()
	s, app := oauthSetupFixture(t)
	app.Auth.OAuth2.Scopes = []string{"scope.read", "scope.write"}
	app.Auth.OAuth2.OptionalScopes = []OAuthOptionalScope{{Scope: testMembershipsScope, Label: "Channel memberships"}}
	return s, app
}

func countTableRows(t *testing.T, s *Server, table string) int {
	t.Helper()
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func authorizeScope(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("scope")
}

func TestLocalOAuthAuthorizeURLOptionalScopes(t *testing.T) {
	s, app := optionalScopesFixture(t)
	base := s.localOAuthAuthorizeURL(app, "st", "ch", "client")
	if got := authorizeScope(t, base); got != "scope.read scope.write" {
		t.Fatalf("default scope = %q", got)
	}
	// No extras (nil or empty variadic) must be byte-identical to the default.
	if again := s.localOAuthAuthorizeURL(app, "st", "ch", "client", []string{}...); again != base {
		t.Fatalf("empty extras changed URL:\n%s\n%s", again, base)
	}
	// Extras are appended after base scopes, de-duplicated.
	withExtra := s.localOAuthAuthorizeURL(app, "st", "ch", "client", testMembershipsScope, "scope.read", testMembershipsScope)
	if got := authorizeScope(t, withExtra); got != "scope.read scope.write "+testMembershipsScope {
		t.Fatalf("scope with optional = %q", got)
	}
	// Same separator as the base scopes.
	app.Auth.OAuth2.ScopeSeparator = ","
	if got := authorizeScope(t, s.localOAuthAuthorizeURL(app, "st", "ch", "client", testMembershipsScope)); got != "scope.read,scope.write,"+testMembershipsScope {
		t.Fatalf("comma scope = %q", got)
	}
}

func TestResolveOptionalOAuthScopes(t *testing.T) {
	_, app := optionalScopesFixture(t)
	got, err := resolveOptionalOAuthScopes(app, []string{" " + testMembershipsScope, "", testMembershipsScope})
	if err != nil || len(got) != 1 || got[0] != testMembershipsScope {
		t.Fatalf("resolve = %v, %v", got, err)
	}
	if _, err := resolveOptionalOAuthScopes(app, []string{"scope.read"}); err == nil {
		t.Fatal("base scope accepted as optional scope")
	}
	if _, err := resolveOptionalOAuthScopes(app, []string{"https://evil.example/all"}); err == nil {
		t.Fatal("undeclared scope accepted")
	}
}

func dashboardReauth(s *Server, connID int64, body string) *httptest.ResponseRecorder {
	var req *http.Request
	path := "/connections/" + strconv.FormatInt(connID, 10) + "/reauth"
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	}
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleReauthConnection(rec, req)
	return rec
}

func TestDashboardReauthOptionalScopes(t *testing.T) {
	s, app := optionalScopesFixture(t)
	conn := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "yt", ProjectID: "proj-1", Status: "active", Source: "local"},
		map[string]string{"api_host": "sandbox.example", "client_id": "c", "client_secret": "cs", "access_token": "old"})

	decode := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			RedirectURL string `json:"redirect_url"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return authorizeScope(t, out.RedirectURL)
	}

	for _, body := range []string{"", "{}", `{"optional_scopes":[]}`} {
		if got := decode(dashboardReauth(s, conn.ID, body)); got != "scope.read scope.write" {
			t.Fatalf("body %q: scope = %q", body, got)
		}
	}
	if got := decode(dashboardReauth(s, conn.ID, `{"optional_scopes":["`+testMembershipsScope+`"]}`)); got != "scope.read scope.write "+testMembershipsScope {
		t.Fatalf("optional scope = %q", got)
	}

	before := countTableRows(t, s, "oauth_states")
	rec := dashboardReauth(s, conn.ID, `{"optional_scopes":["https://evil.example/all"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a declared optional scope") {
		t.Fatalf("undeclared: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if after := countTableRows(t, s, "oauth_states"); after != before {
		t.Fatalf("undeclared scope minted state: %d -> %d", before, after)
	}
}

func appReauth(s *Server, installID int64, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/oauth/reauth", strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleCallbackOAuth(rec, req, []string{"reauth"})
	return rec
}

func reauthBody(connID int64, scopes ...string) string {
	b, _ := json.Marshal(map[string]any{
		"connection_id":   connID,
		"optional_scopes": scopes,
		"return_url":      "https://app.example/api/apps/social/accounts/oauth_done?x=1",
	})
	return string(b)
}

func seedReauthInstall(t *testing.T, s *Server, name string, perms ...sdk.Permission) int64 {
	t.Helper()
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: name}
	manifest.Requires.Permissions = perms
	return seedInstallWithBindings(t, s, name, manifest, nil)
}

func TestAppOAuthReauthAuthorization(t *testing.T) {
	s, app := optionalScopesFixture(t)
	social := seedReauthInstall(t, s, "social", sdk.PermOAuthStart, sdk.PermConnectionsExecute)
	noStart := seedReauthInstall(t, s, "no-start", sdk.PermConnectionsExecute)
	noExecute := seedReauthInstall(t, s, "no-execute", sdk.PermOAuthStart)
	other := seedReauthInstall(t, s, "other", sdk.PermOAuthStart, sdk.PermConnectionsExecute)
	creds := map[string]string{"api_host": "sandbox.example", "client_id": "c", "client_secret": "cs", "access_token": "old"}
	owned := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "owned", ProjectID: "proj-1", Status: "active", Source: "local", CreatedVia: "app_install", OwnerAppInstallID: social}, creds)
	otherOwned := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "other-owned", ProjectID: "proj-1", Status: "active", Source: "local", CreatedVia: "app_install", OwnerAppInstallID: other}, creds)
	operatorBearer := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "operator-bearer", ProjectID: "proj-1", Status: "active", Source: "local", AuthType: "bearer", CreatedVia: "integration"}, creds)

	before := countTableRows(t, s, "oauth_states")
	cases := []struct {
		name    string
		install int64
		body    string
		status  int
	}{
		{"missing oauth.start", noStart, reauthBody(owned.ID, testMembershipsScope), http.StatusForbidden},
		{"missing connections.execute", noExecute, reauthBody(owned.ID, testMembershipsScope), http.StatusForbidden},
		{"owned by another install", social, reauthBody(otherOwned.ID, testMembershipsScope), http.StatusForbidden},
		{"unknown connection", social, reauthBody(999999, testMembershipsScope), http.StatusNotFound},
		{"undeclared scope", social, reauthBody(owned.ID, "https://evil.example/all"), http.StatusBadRequest},
		{"not oauth2", social, reauthBody(operatorBearer.ID), http.StatusBadRequest},
		{"missing return_url", social, `{"connection_id":` + strconv.FormatInt(owned.ID, 10) + `}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := appReauth(s, tc.install, tc.body); rec.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
	if after := countTableRows(t, s, "oauth_states"); after != before {
		t.Fatalf("rejected reauth minted state: %d -> %d", before, after)
	}
}

func TestAppOAuthReauthUpdatesSameConnectionAndReturns(t *testing.T) {
	s, app := optionalScopesFixture(t)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "reauth-code" || r.Form.Get("client_id") != "c" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	app.Auth.OAuth2.TokenURL = tokenServer.URL

	social := seedReauthInstall(t, s, "social", sdk.PermOAuthStart, sdk.PermConnectionsExecute)
	conn := saveOAuthSetupFixture(t, s, app, ConnectionInput{Name: "yt", ProjectID: "proj-1", Status: "active", Source: "local", CreatedVia: "app_install", OwnerAppInstallID: social},
		map[string]string{"api_host": "sandbox.example", "client_id": "c", "client_secret": "cs", "access_token": "old"})
	connsBefore := countTableRows(t, s, "connections")

	rec := appReauth(s, social, reauthBody(conn.ID, testMembershipsScope))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		ConnectionID int64  `json:"connection_id"`
		AuthorizeURL string `json:"authorize_url"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ConnectionID != conn.ID || res.ExpiresAt == "" {
		t.Fatalf("result = %+v", res)
	}
	if got := authorizeScope(t, res.AuthorizeURL); got != "scope.read scope.write "+testMembershipsScope {
		t.Fatalf("authorize scope = %q", got)
	}
	u, _ := url.Parse(res.AuthorizeURL)
	state := u.Query().Get("state")
	var purpose, returnURL string
	var stateConn, stateInstall int64
	if err := s.store.db.QueryRow(`SELECT purpose, connection_id, app_install_id, return_url FROM oauth_states WHERE state=?`, state).
		Scan(&purpose, &stateConn, &stateInstall, &returnURL); err != nil {
		t.Fatal(err)
	}
	if purpose != oauthStatePurposeReauth || stateConn != conn.ID || stateInstall != social || returnURL != "https://app.example/api/apps/social/accounts/oauth_done?x=1" {
		t.Fatalf("state row purpose=%q conn=%d install=%d return=%q", purpose, stateConn, stateInstall, returnURL)
	}
	if countTableRows(t, s, "connections") != connsBefore {
		t.Fatal("reauth start created a connection")
	}

	callback := httptest.NewRecorder()
	s.handleLocalOAuthCallback(callback, httptest.NewRequest(http.MethodGet, "/oauth/local/callback?state="+url.QueryEscape(state)+"&code=reauth-code", nil))
	if callback.Code != http.StatusFound {
		t.Fatalf("callback HTTP %d: %s", callback.Code, callback.Body.String())
	}
	loc, err := url.Parse(callback.Header().Get("Location"))
	if err != nil || loc.Host != "app.example" || loc.Path != "/api/apps/social/accounts/oauth_done" ||
		loc.Query().Get("x") != "1" || loc.Query().Get("conn_id") != strconv.FormatInt(conn.ID, 10) || loc.Query().Get("status") != "ok" {
		t.Fatalf("redirect = %q", callback.Header().Get("Location"))
	}
	if countTableRows(t, s, "connections") != connsBefore {
		t.Fatal("reauth callback created a connection")
	}
	got, encrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil || got.Status != "active" || got.OwnerAppInstallID != social {
		t.Fatalf("connection after reauth: %+v %v", got, err)
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]string
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		t.Fatal(err)
	}
	if creds["access_token"] != "new-access" || creds["refresh_token"] != "new-refresh" || creds["client_id"] != "c" || creds["api_host"] != "sandbox.example" {
		t.Fatalf("credentials not refreshed in place: keys=%v", creds)
	}
}
