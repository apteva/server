package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// ─── response unwrapping ─────────────────────────────────────────────

func TestUnwrapTokenResponse(t *testing.T) {
	instagram := map[string]any{
		"data": []any{
			map[string]any{"access_token": "ig-token", "user_id": float64(17841400000000000)},
		},
	}
	flat := map[string]any{"access_token": "std-token"}

	t.Run("instagram data.0", func(t *testing.T) {
		out, err := unwrapTokenResponse(instagram, "data.0")
		if err != nil {
			t.Fatalf("unwrap: %v", err)
		}
		if out["access_token"] != "ig-token" {
			t.Errorf("access_token = %v", out["access_token"])
		}
	})

	// The standard shape must cost nothing — every other catalog entry
	// leaves the path empty.
	t.Run("empty path is identity", func(t *testing.T) {
		out, err := unwrapTokenResponse(flat, "")
		if err != nil || out["access_token"] != "std-token" {
			t.Fatalf("out=%v err=%v", out, err)
		}
	})

	for _, tc := range []struct{ name, path string }{
		{"missing key", "nope"},
		{"index past end", "data.5"},
		{"index into object", "data.0.access_token.0"},
		{"path to scalar", "data.0.access_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := unwrapTokenResponse(instagram, tc.path); err == nil {
				t.Errorf("expected an error for path %q", tc.path)
			}
		})
	}
}

// ─── token calls ─────────────────────────────────────────────────────

func TestRunOAuthTokenCallSendsClientSecretOnlyWhenDeclared(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "long-lived", "expires_in": 5184000,
		})
	}))
	defer server.Close()

	cfg := &OAuthConfig{}
	creds := map[string]string{"access_token": "short-lived"}

	t.Run("long-lived exchange sends it", func(t *testing.T) {
		call := &OAuthTokenCall{
			URL:              server.URL,
			Params:           map[string]string{"grant_type": "ig_exchange_token"},
			SendClientSecret: true,
		}
		out, err := runOAuthTokenCall(call, cfg, creds, "client-id", "client-secret")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if out["access_token"] != "long-lived" {
			t.Errorf("access_token = %q", out["access_token"])
		}
		if got.Get("client_secret") != "client-secret" {
			t.Error("client_secret missing from the long-lived exchange")
		}
		if got.Get("grant_type") != "ig_exchange_token" {
			t.Errorf("grant_type = %q", got.Get("grant_type"))
		}
		if got.Get("access_token") != "short-lived" {
			t.Errorf("access_token param = %q", got.Get("access_token"))
		}
	})

	// Instagram's refresh endpoint rejects a client_secret outright,
	// which is why the flag is per-call rather than per-app.
	t.Run("refresh omits it", func(t *testing.T) {
		call := &OAuthTokenCall{
			URL:    server.URL,
			Params: map[string]string{"grant_type": "ig_refresh_token"},
		}
		if _, err := runOAuthTokenCall(call, cfg, creds, "client-id", "client-secret"); err != nil {
			t.Fatalf("call: %v", err)
		}
		if got.Get("client_secret") != "" {
			t.Error("client_secret sent on a call that did not declare it")
		}
	})
}

func TestRunOAuthTokenCallDefaultsToGET(t *testing.T) {
	var method string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t"})
	}))
	defer server.Close()

	// Both Instagram calls are GETs, unlike the standard grant's POST.
	if _, err := runOAuthTokenCall(&OAuthTokenCall{URL: server.URL}, &OAuthConfig{},
		map[string]string{"access_token": "cur"}, "id", "secret"); err != nil {
		t.Fatalf("call: %v", err)
	}
	if method != http.MethodGet {
		t.Errorf("method = %s, want GET", method)
	}
}

func TestRunOAuthTokenCallUnwrapsAndValidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"access_token": "nested"}},
		})
	}))
	defer server.Close()

	out, err := runOAuthTokenCall(&OAuthTokenCall{URL: server.URL},
		&OAuthConfig{TokenResponsePath: "data.0"},
		map[string]string{"access_token": "cur"}, "id", "secret")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out["access_token"] != "nested" {
		t.Errorf("access_token = %q", out["access_token"])
	}
}

func TestRunOAuthTokenCallErrors(t *testing.T) {
	t.Run("upstream failure surfaces the body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"expired"}}`, http.StatusBadRequest)
		}))
		defer server.Close()
		_, err := runOAuthTokenCall(&OAuthTokenCall{URL: server.URL}, &OAuthConfig{},
			map[string]string{"access_token": "cur"}, "id", "secret")
		if err == nil {
			t.Fatal("expected an error on 400")
		}
	})

	t.Run("missing credential", func(t *testing.T) {
		_, err := runOAuthTokenCall(&OAuthTokenCall{URL: "https://example.test"}, &OAuthConfig{},
			map[string]string{}, "id", "secret")
		if err == nil {
			t.Fatal("expected an error when the credential is absent")
		}
	})

	t.Run("client_secret required but unavailable", func(t *testing.T) {
		_, err := runOAuthTokenCall(&OAuthTokenCall{URL: "https://example.test", SendClientSecret: true},
			&OAuthConfig{}, map[string]string{"access_token": "cur"}, "id", "")
		if err == nil {
			t.Fatal("expected an error when client_secret is required but missing")
		}
	})
}

// ─── expiry ──────────────────────────────────────────────────────────

func TestApplyTokenExpiry(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	t.Run("records absolute expiry from expires_in", func(t *testing.T) {
		creds := map[string]string{}
		applyTokenExpiry(creds, map[string]string{"expires_in": "5184000"}, now) // 60 days
		want := now.Add(60 * 24 * time.Hour).Format(time.RFC3339)
		if creds["expires_at"] != want {
			t.Errorf("expires_at = %q, want %q", creds["expires_at"], want)
		}
	})

	// Most catalog entries never report expires_in; absence must not
	// invent an expiry, or every call would try to refresh.
	for _, tc := range []struct{ name, value string }{
		{"absent", ""},
		{"unparseable", "soon"},
		{"zero", "0"},
		{"negative", "-1"},
	} {
		t.Run("no expiry when "+tc.name, func(t *testing.T) {
			creds := map[string]string{}
			applyTokenExpiry(creds, map[string]string{"expires_in": tc.value}, now)
			if _, present := creds["expires_at"]; present {
				t.Errorf("expires_at set from %q", tc.value)
			}
		})
	}
}

func TestOAuthTokenNeedsRefresh(t *testing.T) {
	skew := 10 * time.Minute
	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"well in future", time.Now().Add(48 * time.Hour).Format(time.RFC3339), false},
		{"inside the skew", time.Now().Add(2 * time.Minute).Format(time.RFC3339), true},
		{"already past", time.Now().Add(-time.Hour).Format(time.RFC3339), true},
		// Unknown expiry means "assume fine" — treating silence as
		// expiry would refresh on every single call.
		{"unset", "", false},
		{"malformed", "not-a-date", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := map[string]string{}
			if tc.expiresAt != "" {
				creds["expires_at"] = tc.expiresAt
			}
			if got := oauthTokenNeedsRefresh(creds, skew); got != tc.want {
				t.Errorf("needsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOAuthCanRefresh covers the guard that made the other three knobs
// unreachable: the on-401 path used to bail whenever credentials carried
// no refresh_token, and Instagram never issues one.
func TestOAuthCanRefresh(t *testing.T) {
	cases := []struct {
		name  string
		cfg   *OAuthConfig
		creds map[string]string
		want  bool
	}{
		{"standard refresh_token", &OAuthConfig{}, map[string]string{"refresh_token": "rt"}, true},
		{"camelCase variant", &OAuthConfig{}, map[string]string{"refreshToken": "rt"}, true},
		{"declared refresh needs no token", &OAuthConfig{Refresh: &OAuthTokenCall{URL: "u"}}, map[string]string{}, true},
		{"nothing available", &OAuthConfig{}, map[string]string{}, false},
		{"no oauth config", nil, map[string]string{"refresh_token": "rt"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oauthCanRefresh(tc.cfg, tc.creds); got != tc.want {
				t.Errorf("canRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─── refresh integration ─────────────────────────────────────────────

// TestRefreshOAuthAccessTokenUsesDeclaredCall is the Instagram shape end
// to end: GET, vendor grant_type, current access token standing in for a
// refresh_token, no client_secret, and a fresh expiry recorded.
func TestRefreshOAuthAccessTokenUsesDeclaredCall(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-ig-token",
			"expires_in":   5184000,
		})
	}))
	defer server.Close()

	app := &AppTemplate{
		Slug: "instagram-api",
		Auth: AppAuthConfig{
			Types: []string{"oauth2"},
			OAuth2: &OAuthConfig{
				Refresh: &OAuthTokenCall{
					URL:    server.URL,
					Params: map[string]string{"grant_type": "ig_refresh_token"},
				},
			},
		},
	}
	creds := map[string]string{"access_token": "current-ig-token", "client_id": "id"}

	if err := refreshOAuthAccessToken(app, creds); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if creds["access_token"] != "refreshed-ig-token" {
		t.Errorf("access_token = %q", creds["access_token"])
	}
	if got.Get("grant_type") != "ig_refresh_token" {
		t.Errorf("grant_type = %q", got.Get("grant_type"))
	}
	if got.Get("access_token") != "current-ig-token" {
		t.Errorf("sent access_token = %q, want the current one", got.Get("access_token"))
	}
	if got.Get("client_secret") != "" {
		t.Error("client_secret sent — Instagram's refresh endpoint rejects it")
	}
	// Without a recorded expiry the proactive path can never fire, which
	// is the whole reason this provider needs one.
	if creds["expires_at"] == "" {
		t.Error("refresh did not record a new expires_at")
	}
}

func TestRefreshOAuthAccessTokenRequiresSomeConfig(t *testing.T) {
	app := &AppTemplate{Slug: "x", Auth: AppAuthConfig{}}
	if err := refreshOAuthAccessToken(app, map[string]string{}); err == nil {
		t.Fatal("expected an error with no oauth2 config")
	}
}
