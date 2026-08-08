package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOAuth1RequestAndAccessTokenExchange(t *testing.T) {
	var sawRequestToken, sawAccessToken bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if r.Method != http.MethodPost || !strings.HasPrefix(authorization, "OAuth ") {
			t.Fatalf("unsigned OAuth request: method=%s authorization=%q", r.Method, authorization)
		}
		switch r.URL.Path {
		case "/request_token":
			sawRequestToken = true
			if !strings.Contains(authorization, "oauth_callback=") {
				t.Fatalf("request token signature omitted callback: %s", authorization)
			}
			_, _ = w.Write([]byte("oauth_token=request-token&oauth_token_secret=request-secret&oauth_callback_confirmed=true"))
		case "/access_token":
			sawAccessToken = true
			if !strings.Contains(authorization, `oauth_token="request-token"`) || !strings.Contains(authorization, "oauth_verifier=") {
				t.Fatalf("access token signature omitted verifier or request token: %s", authorization)
			}
			_, _ = w.Write([]byte("oauth_token=access-token&oauth_token_secret=access-secret&user_id=42&screen_name=apteva"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	token, secret, err := requestOAuth1Token(provider.URL+"/request_token", "consumer", "consumer-secret", "https://example.com/callback?state=opaque")
	if err != nil || token != "request-token" || secret != "request-secret" {
		t.Fatalf("request token: token=%q secret=%q err=%v", token, secret, err)
	}

	server := &Server{secret: testSecret()}
	temporary, _ := json.Marshal(oauth1RequestCredentials{Token: token, TokenSecret: secret})
	encrypted, err := Encrypt(server.secret, string(temporary))
	if err != nil {
		t.Fatal(err)
	}
	app := &AppTemplate{Auth: AppAuthConfig{OAuth1: &OAuth1Config{AccessTokenURL: provider.URL + "/access_token"}}}
	credentials, err := server.exchangeOAuth1AccessToken(app, "verifier", token, encrypted, "consumer", "consumer-secret")
	if err != nil {
		t.Fatal(err)
	}
	if credentials["oauth_token"] != "access-token" || credentials["oauth_token_secret"] != "access-secret" || credentials["screen_name"] != "apteva" {
		t.Fatalf("access credentials=%#v", credentials)
	}
	if !sawRequestToken || !sawAccessToken {
		t.Fatalf("incomplete OAuth handshake: request=%t access=%t", sawRequestToken, sawAccessToken)
	}
}

func TestOAuth1AccessTokenRejectsRequestTokenSwap(t *testing.T) {
	server := &Server{secret: testSecret()}
	temporary, _ := json.Marshal(oauth1RequestCredentials{Token: "expected", TokenSecret: "secret"})
	encrypted, _ := Encrypt(server.secret, string(temporary))
	app := &AppTemplate{Auth: AppAuthConfig{OAuth1: &OAuth1Config{AccessTokenURL: "https://example.invalid/access"}}}
	_, err := server.exchangeOAuth1AccessToken(app, "verifier", "attacker-token", encrypted, "consumer", "consumer-secret")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected token mismatch, got %v", err)
	}
}
