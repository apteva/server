package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type oauth1RequestCredentials struct {
	Token       string `json:"token"`
	TokenSecret string `json:"token_secret"`
}

func (s *Store) updateOAuthStateVerifier(state, verifier string) error {
	result, err := s.db.Exec(`UPDATE oauth_states SET pkce_verifier=? WHERE state=?`, verifier, state)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("OAuth state no longer exists")
	}
	return nil
}

func (s *Server) startLocalOAuth1(userID int64, app *AppTemplate, connName, projectID,
	explicitClientID, explicitClientSecret string, supplementalCredentials map[string]string,
	ownerAppInstallID int64, returnURL string, autoMCP *bool) (*Connection, string, error) {
	if app.Auth.OAuth1 == nil {
		return nil, "", fmt.Errorf("app %s has no oauth1 config", app.Slug)
	}
	clientID, clientSecret := s.resolveOAuthClient(userID, projectID, app.Slug, explicitClientID, explicitClientSecret)
	if app.Auth.OAuth1.ClientIDRequired && (clientID == "" || clientSecret == "") {
		return nil, "", fmt.Errorf("missing OAuth consumer key or secret for %s — set them in the connect form, on a prior connection, or via OAUTH_%s_CLIENT_ID and OAUTH_%s_CLIENT_SECRET",
			app.Slug, oauthEnvSlug(app.Slug), oauthEnvSlug(app.Slug))
	}

	creds := make(map[string]string, len(supplementalCredentials)+2)
	for key, value := range supplementalCredentials {
		if strings.TrimSpace(key) != "" {
			creds[key] = value
		}
	}
	creds["client_id"] = clientID
	creds["client_secret"] = clientSecret
	initialJSON, _ := json.Marshal(creds)
	initialBlob, err := Encrypt(s.secret, string(initialJSON))
	if err != nil {
		return nil, "", fmt.Errorf("encrypt pending credentials: %w", err)
	}
	connInput := ConnectionInput{
		UserID: userID, AppSlug: app.Slug, AppName: app.Name, Name: connName,
		AuthType: "oauth1", ProjectID: projectID, Source: "local", Status: "pending",
		EncryptedCreds: initialBlob, AutoMCP: autoMCP,
	}
	if ownerAppInstallID > 0 {
		connInput.CreatedVia = "app_install"
		connInput.OwnerAppInstallID = ownerAppInstallID
	}
	conn, err := s.store.CreateConnectionExt(connInput)
	if err != nil {
		return nil, "", err
	}
	return s.startOAuth1Authorization(userID, app, conn, clientID, clientSecret, ownerAppInstallID, returnURL, oauthStatePurposeConnect)
}

func (s *Server) startOAuth1Authorization(userID int64, app *AppTemplate, conn *Connection,
	clientID, clientSecret string, ownerAppInstallID int64, returnURL, purpose string) (*Connection, string, error) {
	state, err := s.store.mintOAuthState(userID, conn.ID, app.Slug, "", 10*time.Minute, ownerAppInstallID, returnURL, purpose)
	if err != nil {
		return nil, "", err
	}
	callback := s.localOAuthRedirectURI()
	sep := "?"
	if strings.Contains(callback, "?") {
		sep = "&"
	}
	callback += sep + "state=" + url.QueryEscape(state)
	requestToken, requestSecret, err := requestOAuth1Token(app.Auth.OAuth1.RequestTokenURL, clientID, clientSecret, callback)
	if err != nil {
		s.store.db.Exec(`DELETE FROM oauth_states WHERE state=?`, state)
		if purpose != oauthStatePurposeReauth {
			_ = s.store.UpdateConnectionStatus(conn.ID, "failed")
		}
		return nil, "", err
	}
	ephemeralJSON, _ := json.Marshal(oauth1RequestCredentials{Token: requestToken, TokenSecret: requestSecret})
	ephemeral, err := Encrypt(s.secret, string(ephemeralJSON))
	if err != nil {
		return nil, "", err
	}
	if err := s.store.updateOAuthStateVerifier(state, ephemeral); err != nil {
		return nil, "", err
	}
	authorizeURL, err := url.Parse(app.Auth.OAuth1.AuthorizeURL)
	if err != nil {
		return nil, "", err
	}
	q := authorizeURL.Query()
	q.Set("oauth_token", requestToken)
	authorizeURL.RawQuery = q.Encode()
	return conn, authorizeURL.String(), nil
}

func requestOAuth1Token(endpoint, consumerKey, consumerSecret, callback string) (string, string, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	if err := signOAuth1Request(req, nil, consumerKey, consumerSecret, "", "", map[string]string{"oauth_callback": callback}, "", ""); err != nil {
		return "", "", err
	}
	values, err := executeOAuth1Exchange(req)
	if err != nil {
		return "", "", fmt.Errorf("request-token endpoint: %w", err)
	}
	if values.Get("oauth_callback_confirmed") != "true" {
		return "", "", fmt.Errorf("provider did not confirm OAuth callback")
	}
	if values.Get("oauth_token") == "" || values.Get("oauth_token_secret") == "" {
		return "", "", fmt.Errorf("provider returned an incomplete request token")
	}
	return values.Get("oauth_token"), values.Get("oauth_token_secret"), nil
}

func (s *Server) exchangeOAuth1AccessToken(app *AppTemplate, verifier, returnedToken, encryptedRequest string,
	consumerKey, consumerSecret string) (map[string]string, error) {
	plain, err := Decrypt(s.secret, encryptedRequest)
	if err != nil {
		return nil, fmt.Errorf("decrypt request token: %w", err)
	}
	var requestCreds oauth1RequestCredentials
	if err := json.Unmarshal([]byte(plain), &requestCreds); err != nil {
		return nil, fmt.Errorf("parse request token: %w", err)
	}
	if returnedToken == "" || returnedToken != requestCreds.Token {
		return nil, fmt.Errorf("OAuth request token mismatch")
	}
	req, err := http.NewRequest(http.MethodPost, app.Auth.OAuth1.AccessTokenURL, nil)
	if err != nil {
		return nil, err
	}
	if err := signOAuth1Request(req, nil, consumerKey, consumerSecret, requestCreds.Token, requestCreds.TokenSecret,
		map[string]string{"oauth_verifier": verifier}, "", ""); err != nil {
		return nil, err
	}
	values, err := executeOAuth1Exchange(req)
	if err != nil {
		return nil, fmt.Errorf("access-token endpoint: %w", err)
	}
	if values.Get("oauth_token") == "" || values.Get("oauth_token_secret") == "" {
		return nil, fmt.Errorf("provider returned an incomplete access token")
	}
	out := map[string]string{
		"oauth_token":        values.Get("oauth_token"),
		"oauth_token_secret": values.Get("oauth_token_secret"),
	}
	for _, key := range []string{"user_id", "screen_name"} {
		if value := values.Get(key); value != "" {
			out[key] = value
		}
	}
	return out, nil
}

func executeOAuth1Exchange(req *http.Request) (url.Values, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("decode form response: %w", err)
	}
	return values, nil
}

func oauthEnvSlug(slug string) string {
	return strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))
}
