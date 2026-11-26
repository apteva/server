package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// tiktokAdsOAuthApp mirrors the tiktok-ads catalog entry: a JSON code
// exchange of exactly {app_id, secret, auth_code}, a token object under
// "data", and errors reported as HTTP 200 with a non-zero code.
func tiktokAdsOAuthApp(tokenURL string) *AppTemplate {
	return &AppTemplate{
		Slug: "tiktok-ads",
		Auth: AppAuthConfig{
			Types: []string{"oauth2"},
			OAuth2: &OAuthConfig{
				AuthorizeURL:         "https://business-api.tiktok.com/portal/auth",
				TokenURL:             tokenURL,
				ClientIDRequired:     true,
				ClientSecretRequired: true,
				ClientIDParamName:    "app_id",
				TokenRequest: &OAuthTokenRequest{
					BodyFormat: "json",
					Fields: map[string]string{
						"app_id":    "{{client_id}}",
						"secret":    "{{client_secret}}",
						"auth_code": "{{code}}",
					},
				},
				TokenResponsePath: "data",
				TokenError: &ResponseErrorDef{
					Type:         "json_status",
					CodePath:     "code",
					SuccessCodes: []any{0},
					MessagePath:  "message",
				},
			},
		},
	}
}

func TestExchangeOAuthCodeSendsDeclaredJSONTokenRequest(t *testing.T) {
	s := newTestServer(t)
	var body map[string]any
	var contentType, authorization, rawQuery string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		authorization = r.Header.Get("Authorization")
		rawQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %q", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"message":"OK","request_id":"req-1","data":{"access_token":"tiktok-token","advertiser_ids":["7123456789012345678","7000000000000000001"],"scope":[4,5]}}`)
	}))
	defer provider.Close()

	tokens, err := s.exchangeOAuthCode(tiktokAdsOAuthApp(provider.URL), "auth-code-1", "", 1, "app-1", "secret-1", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	want := map[string]any{"app_id": "app-1", "secret": "secret-1", "auth_code": "auth-code-1"}
	if len(body) != len(want) {
		t.Errorf("body = %v, want exactly %v", body, want)
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("body[%q] = %v, want %v", key, body[key], value)
		}
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if authorization != "" {
		t.Errorf("Authorization = %q, want no Basic auth", authorization)
	}
	if rawQuery != "" {
		t.Errorf("query = %q, want none", rawQuery)
	}

	if tokens["access_token"] != "tiktok-token" {
		t.Errorf("access_token = %q", tokens["access_token"])
	}
	if got := tokens["advertiser_ids"]; got != `["7123456789012345678","7000000000000000001"]` {
		t.Errorf("advertiser_ids = %q, want a JSON array", got)
	}
	if got := tokens["scope"]; got != "[4,5]" {
		t.Errorf("scope = %q, want a JSON array", got)
	}
	// Advertiser tokens neither expire nor refresh: a revoked one means
	// reconnecting, not a refresh attempt.
	if tokens["expires_at"] != "" {
		t.Errorf("expires_at = %q, want none", tokens["expires_at"])
	}
	if oauthCanRefresh(tiktokAdsOAuthApp(provider.URL).Auth.OAuth2, tokens) {
		t.Error("a TikTok advertiser token must not count as refreshable")
	}
}

func TestExchangeOAuthCodeKeepsLargeNumericIDsExact(t *testing.T) {
	s := newTestServer(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"message":"OK","data":{"access_token":"tiktok-token","advertiser_ids":[7123456789012345678]}}`)
	}))
	defer provider.Close()

	tokens, err := s.exchangeOAuthCode(tiktokAdsOAuthApp(provider.URL), "auth-code-1", "", 1, "app-1", "secret-1", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := tokens["advertiser_ids"]; got != "[7123456789012345678]" {
		t.Errorf("advertiser_ids = %q, want every digit kept", got)
	}
}

func TestExchangeOAuthCodeSurfacesTokenErrorFromHTTP200(t *testing.T) {
	s := newTestServer(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":40001,"message":"No permission to operate advertiser","request_id":"req-2","data":{}}`)
	}))
	defer provider.Close()

	_, err := s.exchangeOAuthCode(tiktokAdsOAuthApp(provider.URL), "used-code", "", 1, "app-1", "secret-1", nil)
	if err == nil {
		t.Fatal("exchange succeeded on code 40001")
	}
	if !strings.Contains(err.Error(), "40001") || !strings.Contains(err.Error(), "No permission to operate advertiser") {
		t.Errorf("error = %q, want TikTok's code and message", err)
	}
}

func TestExchangeOAuthCodeTokenRequestNeedsEveryTemplateValue(t *testing.T) {
	s := newTestServer(t)
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer provider.Close()

	_, err := s.exchangeOAuthCode(tiktokAdsOAuthApp(provider.URL), "auth-code-1", "", 1, "app-1", "", nil)
	if err == nil || !strings.Contains(err.Error(), `"secret"`) {
		t.Fatalf("error = %v, want the unfilled secret field named", err)
	}
	if called {
		t.Error("the provider was called with an unfilled placeholder")
	}
}

func TestExchangeOAuthCodeStandardFormUnchanged(t *testing.T) {
	s := newTestServer(t)
	var form url.Values
	var contentType string
	var basicUser, basicPass string
	var hasBasic bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		basicUser, basicPass, hasBasic = r.BasicAuth()
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"creator-token","refresh_token":"creator-refresh","expires_in":86400,"open_id":"open-1"}`)
	}))
	defer provider.Close()

	// tiktok-api (Login Kit): standard exchange with a renamed client ID.
	app := &AppTemplate{
		Slug: "tiktok-api",
		Auth: AppAuthConfig{
			Types: []string{"oauth2"},
			OAuth2: &OAuthConfig{
				AuthorizeURL:      "https://www.tiktok.com/v2/auth/authorize",
				TokenURL:          provider.URL,
				ClientIDParamName: "client_key",
				ScopeSeparator:    ",",
			},
		},
	}
	tokens, err := s.exchangeOAuthCode(app, "code-1", "", 1, "key-1", "secret-1", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", contentType)
	}
	for key, value := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "code-1",
		"client_key":    "key-1",
		"client_secret": "secret-1",
	} {
		if form.Get(key) != value {
			t.Errorf("form[%q] = %q, want %q", key, form.Get(key), value)
		}
	}
	if form.Get("redirect_uri") == "" {
		t.Error("redirect_uri missing from the standard exchange")
	}
	if !hasBasic || basicUser != "key-1" || basicPass != "secret-1" {
		t.Errorf("Basic auth = %q/%q (%t), want the client credentials", basicUser, basicPass, hasBasic)
	}
	if tokens["access_token"] != "creator-token" || tokens["refresh_token"] != "creator-refresh" || tokens["expires_in"] != "86400" || tokens["open_id"] != "open-1" {
		t.Errorf("tokens = %v", tokens)
	}
	if tokens["expires_at"] == "" {
		t.Error("expires_in no longer sets expires_at")
	}
}

func TestTikTokAdsAuthorizeURLUsesAppID(t *testing.T) {
	s := newTestServer(t)
	authURL, err := url.Parse(s.localOAuthAuthorizeURL(tiktokAdsOAuthApp("https://example.invalid/token"), "state-1", "", "app-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	if q.Get("app_id") != "app-1" || q.Has("client_id") {
		t.Errorf("client ID params = app_id %q, client_id present %t", q.Get("app_id"), q.Has("client_id"))
	}
	if q.Get("state") != "state-1" || q.Get("redirect_uri") == "" {
		t.Errorf("authorize URL missing state or redirect_uri: %s", authURL)
	}
}

func TestRefreshAppliesTokenError(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":40105,"message":"Access token is incorrect or has been revoked","data":{}}`)
	}))
	defer provider.Close()

	app := tiktokAdsOAuthApp(provider.URL)
	creds := map[string]string{"access_token": "old", "refresh_token": "refresh-1", "client_id": "app-1", "client_secret": "secret-1"}
	err := refreshOAuthAccessToken(app, creds)
	if err == nil || !strings.Contains(err.Error(), "40105") {
		t.Fatalf("refresh error = %v, want code 40105", err)
	}
	if creds["access_token"] != "old" {
		t.Errorf("access_token overwritten with %q after a failed refresh", creds["access_token"])
	}
}

func TestFlattenOAuthTokenResponse(t *testing.T) {
	raw, err := decodeOAuthTokenJSON([]byte(`{"access_token":"t","expires_in":5184000,"ids":["1","2"],"meta":{"a":1},"flag":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got := flattenOAuthTokenResponse(raw)
	want := map[string]string{
		"access_token": "t",
		"expires_in":   "5184000",
		"ids":          `["1","2"]`,
		"meta":         `{"a":1}`,
		"flag":         "true",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestIntegrationTransportErrorOmitsQuery(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := provider.URL
	provider.Close() // every request now fails in transport

	app := &AppTemplate{Slug: "tiktok-ads", BaseURL: baseURL, Auth: AppAuthConfig{Types: []string{"oauth2"}}}
	tool := &AppToolDef{Name: "list_advertisers", Method: "GET", Path: "/oauth2/advertiser/get/?app_id={{credential.client_id}}&secret={{credential.client_secret}}"}
	_, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "t", "client_id": "app-1", "client_secret": "secret-value-1"}, map[string]any{}, "")
	if err == nil {
		t.Fatal("request to a closed server succeeded")
	}
	if strings.Contains(err.Error(), "secret-value-1") || strings.Contains(err.Error(), "app-1") {
		t.Errorf("transport error leaks the query: %v", err)
	}
	if !strings.Contains(err.Error(), "/oauth2/advertiser/get/") {
		t.Errorf("transport error lost the path: %v", err)
	}
}
