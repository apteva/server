package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Local OAuth2 flow for connections whose source='local' and whose catalog
// entry declares auth.oauth2. Composio connections do NOT use this module —
// Composio hosts its own OAuth flow.
//
// Flow:
//  1. handleCreateConnection detects oauth2 + local source, calls
//     startLocalOAuth which:
//       a. creates a connections row in status='pending' with empty creds,
//       b. mints + persists a random state in oauth_states,
//       c. returns the authorize URL to the dashboard,
//  2. user authorizes in popup, provider redirects to
//     GET /oauth/local/callback?state=...&code=...,
//  3. handleLocalOAuthCallback exchanges code → tokens at the catalog's
//     token_url using client credentials from env, stores the tokens encrypted,
//     flips connection to status='active', auto-creates the local mcp_servers
//     shim row, and renders a small "you can close this" HTML response.

// Client credentials for each local OAuth2 app are read from env vars:
//   OAUTH_<UPPER_SLUG>_CLIENT_ID
//   OAUTH_<UPPER_SLUG>_CLIENT_SECRET
//
// The apteva-server README should document this per-app.

func oauthEnvClientID(slug string) string {
	return os.Getenv("OAUTH_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_CLIENT_ID")
}

func oauthEnvClientSecret(slug string) string {
	return os.Getenv("OAUTH_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_CLIENT_SECRET")
}

// localOAuthRedirectURI returns the redirect URI we register with upstream
// providers. In local dev this is http://localhost:<port>/oauth/local/callback;
// in hosted environments PUBLIC_URL overrides the scheme/host.
func (s *Server) localOAuthRedirectURI() string {
	if s.publicURL != "" {
		return strings.TrimRight(s.publicURL, "/") + "/oauth/local/callback"
	}
	return "http://localhost:" + s.port + "/oauth/local/callback"
}

// mintState generates a cryptographically random state token and persists it
// alongside the pending connection id so the callback can look it up.
func (s *Store) mintOAuthState(userID, connID int64, appSlug, pkceVerifier string, ttl time.Duration) (string, error) {
	buf := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	state := "st_" + hex.EncodeToString(buf)
	_, err := s.db.Exec(
		`INSERT INTO oauth_states (state, user_id, connection_id, app_slug, pkce_verifier, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		state, userID, connID, appSlug, pkceVerifier, time.Now().Add(ttl).UTC(),
	)
	if err != nil {
		return "", err
	}
	return state, nil
}

type oauthStateRow struct {
	UserID       int64
	ConnectionID int64
	AppSlug      string
	PKCEVerifier string
	Expired      bool
}

func (s *Store) consumeOAuthState(state string) (*oauthStateRow, error) {
	var row oauthStateRow
	var expiresAt string
	err := s.db.QueryRow(
		`SELECT user_id, connection_id, app_slug, COALESCE(pkce_verifier,''), expires_at FROM oauth_states WHERE state = ?`,
		state,
	).Scan(&row.UserID, &row.ConnectionID, &row.AppSlug, &row.PKCEVerifier, &expiresAt)
	if err != nil {
		return nil, err
	}
	s.db.Exec("DELETE FROM oauth_states WHERE state = ?", state)
	if t, err := parseTime(expiresAt); err == nil && time.Now().After(t) {
		row.Expired = true
	}
	return &row, nil
}

// --- PKCE helpers (RFC 7636) ---

func pkcePair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// --- Authorize URL builder ---

// startLocalOAuth creates a pending connection and returns the authorize URL
// to redirect the user to. The caller is responsible for returning that URL to
// the dashboard, which opens it in a popup.
func (s *Server) startLocalOAuth(userID int64, app *AppTemplate, connName, projectID string) (*Connection, string, error) {
	if app.Auth.OAuth2 == nil {
		return nil, "", fmt.Errorf("app %s has no oauth2 config", app.Slug)
	}
	cfg := app.Auth.OAuth2
	clientID := oauthEnvClientID(app.Slug)
	if cfg.ClientIDRequired && clientID == "" {
		return nil, "", fmt.Errorf("OAUTH_%s_CLIENT_ID not set", strings.ToUpper(strings.ReplaceAll(app.Slug, "-", "_")))
	}

	// Create pending row with empty credentials
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:    userID,
		AppSlug:   app.Slug,
		AppName:   app.Name,
		Name:      connName,
		AuthType:  "oauth2",
		ProjectID: projectID,
		Source:    "local",
		Status:    "pending",
	})
	if err != nil {
		return nil, "", err
	}

	var verifier, challenge string
	if cfg.PKCE {
		verifier, challenge, err = pkcePair()
		if err != nil {
			return nil, "", err
		}
	}

	state, err := s.store.mintOAuthState(userID, conn.ID, app.Slug, verifier, 10*time.Minute)
	if err != nil {
		return nil, "", err
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", s.localOAuthRedirectURI())
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	q.Set("state", state)
	if cfg.PKCE {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}

	sep := "?"
	if strings.Contains(cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return conn, cfg.AuthorizeURL + sep + q.Encode(), nil
}

// handleLocalOAuthCallback receives the provider redirect, exchanges code for
// tokens, and flips the pending connection to active.
//
// Route: GET /oauth/local/callback
// Public: yes (redirect target — no session cookie required).
func (s *Server) handleLocalOAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	errParam := r.URL.Query().Get("error")

	row, err := s.store.consumeOAuthState(state)
	if err != nil {
		http.Error(w, "unknown or expired state", http.StatusBadRequest)
		return
	}
	if row.Expired {
		s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		http.Error(w, "state expired — re-initiate the connection", http.StatusBadRequest)
		return
	}
	if errParam != "" {
		s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		renderOAuthResult(w, false, "provider returned error: "+errParam)
		return
	}
	if code == "" {
		s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	app := s.catalog.Get(row.AppSlug)
	if app == nil || app.Auth.OAuth2 == nil {
		http.Error(w, "app missing from catalog", http.StatusInternalServerError)
		return
	}

	tokens, err := s.exchangeOAuthCode(app, code, row.PKCEVerifier)
	if err != nil {
		s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		renderOAuthResult(w, false, "token exchange failed: "+err.Error())
		return
	}

	encJSON, _ := json.Marshal(tokens)
	enc, err := Encrypt(s.secret, string(encJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateConnectionCredentials(row.ConnectionID, enc); err != nil {
		http.Error(w, "db update failed", http.StatusInternalServerError)
		return
	}
	s.store.UpdateConnectionStatus(row.ConnectionID, "active")

	// Auto-create local mcp_servers shim row (mirrors the non-OAuth path in
	// handleCreateConnection).
	conn, _, err := s.store.GetConnection(row.UserID, row.ConnectionID)
	if err == nil {
		s.store.CreateMCPServerFromConnection(row.UserID, conn, len(app.Tools))
	}

	renderOAuthResult(w, true, "Connection authorized. You can close this tab.")
}

// exchangeOAuthCode performs the standard RFC 6749 authorization_code grant
// against the catalog's token_url. Returns a flat string map the connection
// executor can read.
func (s *Server) exchangeOAuthCode(app *AppTemplate, code, pkceVerifier string) (map[string]string, error) {
	cfg := app.Auth.OAuth2
	clientID := oauthEnvClientID(app.Slug)
	clientSecret := oauthEnvClientSecret(app.Slug)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.localOAuthRedirectURI())
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if cfg.PKCE && pkceVerifier != "" {
		form.Set("code_verifier", pkceVerifier)
	}

	req, err := http.NewRequest("POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token endpoint: http %d: %s", resp.StatusCode, string(body))
	}

	// Accept either JSON or form-encoded responses.
	out := make(map[string]string)
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || (len(body) > 0 && body[0] == '{') {
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("json decode: %w", err)
		}
		for k, v := range raw {
			out[k] = fmt.Sprint(v)
		}
	} else {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("form decode: %w", err)
		}
		for k := range values {
			out[k] = values.Get(k)
		}
	}
	if out["access_token"] == "" {
		return nil, fmt.Errorf("no access_token in response: %s", string(body))
	}
	return out, nil
}

// renderOAuthResult returns a tiny HTML page for the popup. Dashboards can
// also detect completion by polling GET /connections/:id.
func renderOAuthResult(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	color := "#10b981"
	title := "Connected"
	if !ok {
		color = "#ef4444"
		title = "Connection failed"
	}
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>body{font-family:-apple-system,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#0f172a;color:#e2e8f0}
.card{background:#1e293b;border-radius:12px;padding:32px 40px;border-left:4px solid %s;max-width:400px}
h1{margin:0 0 8px 0;font-size:20px}p{margin:0;color:#94a3b8;font-size:14px}</style>
</head><body>
<div class="card"><h1>%s</h1><p>%s</p></div>
<script>setTimeout(function(){window.close()},2500);</script>
</body></html>`, title, color, title, msg)
}
