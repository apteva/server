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

// findStoredOAuthClient looks up OAuth client credentials a user has already
// saved for this app+project combination. Strategy:
//
//   1. Walk the user's existing connections for the same project + slug + source=local,
//      newest first.
//   2. Decrypt each one's credentials blob and check for client_id/client_secret
//      keys. The first hit wins.
//
// Returns ("", "") when nothing is found — callers fall back to env vars.
//
// Why connections-table reuse instead of a separate oauth_clients table:
// it's the same encryption key, the same per-user/project scoping, and a
// connection that already authorized successfully has, by definition, valid
// client creds. Subsequent connects to the same app reuse them transparently
// without the user having to re-enter anything.
func (s *Server) findStoredOAuthClient(userID int64, projectID, slug string) (clientID, clientSecret string) {
	conns, err := s.store.ListConnections(userID, projectID)
	if err != nil {
		return "", ""
	}
	for _, c := range conns {
		if c.AppSlug != slug || c.Source != "local" {
			continue
		}
		// We don't filter by status — even a 'pending' or 'failed' row
		// might have client creds the user just saved before the OAuth
		// dance broke. Better to reuse them than ask twice.
		_, encData, err := s.store.GetConnection(userID, c.ID)
		if err != nil || encData == "" {
			continue
		}
		plain, err := Decrypt(s.secret, encData)
		if err != nil {
			continue
		}
		var data map[string]string
		if err := json.Unmarshal([]byte(plain), &data); err != nil {
			continue
		}
		id := data["client_id"]
		secret := data["client_secret"]
		if id != "" {
			return id, secret
		}
	}
	return "", ""
}

// handleOAuthClientStatus tells the dashboard whether OAuth client credentials
// are already on file for a given user+project+app. It NEVER returns the
// secret value — only a boolean and the (non-secret) client_id, plus the
// callback URL the user will need to register with the upstream provider.
//
// GET /api/oauth/local/client?app_slug=github&project_id=abc
func (s *Server) handleOAuthClientStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	slug := r.URL.Query().Get("app_slug")
	projectID := r.URL.Query().Get("project_id")
	if slug == "" {
		http.Error(w, "app_slug required", http.StatusBadRequest)
		return
	}
	id, secret := s.findStoredOAuthClient(userID, projectID, slug)
	envID := oauthEnvClientID(slug)
	resolved := id != "" || envID != ""
	writeJSON(w, map[string]any{
		"has_client_id":     id != "" || envID != "",
		"has_client_secret": secret != "" || oauthEnvClientSecret(slug) != "",
		"client_id":         id, // empty if only env-var path is set; we don't reveal env values
		"source":            map[bool]string{true: "stored", false: "env"}[id != ""],
		"resolved":          resolved,
		"callback_url":      s.localOAuthRedirectURI(),
	})
}

// handleServerSettings exposes the small key/value settings table to the
// dashboard. GET returns every effective server-level setting plus where
// each value came from (DB / env / unset). PUT upserts the keys provided
// in the body, treating empty string as "delete".
//
// Today the only real key is public_url; the shape generalizes so we can
// add more without touching the route.
func (s *Server) handleServerSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		dbURL := s.store.GetSetting("public_url")
		envURL := s.publicURL // captured at boot
		effective := dbURL
		source := "db"
		if effective == "" {
			effective = envURL
			source = "env"
			if effective == "" {
				source = "unset"
			}
		}
		writeJSON(w, map[string]any{
			"public_url": map[string]any{
				"value":          dbURL,
				"env_value":      envURL,
				"effective":      effective,
				"source":         source,
				"oauth_callback": s.localOAuthRedirectURI(),
			},
		})
		return

	case http.MethodPut:
		var body struct {
			PublicURL *string `json:"public_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.PublicURL != nil {
			v := strings.TrimSpace(*body.PublicURL)
			// Light validation: if non-empty, must look like a URL with a
			// scheme. We don't want to silently store garbage that breaks
			// every webhook on the system.
			if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				http.Error(w, "public_url must start with http:// or https://", http.StatusBadRequest)
				return
			}
			if err := s.store.SetSetting("public_url", v); err != nil {
				http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		// Re-read and return the new state so the dashboard can refresh
		// its form fields without a second round-trip.
		s.handleServerSettings(w, &http.Request{Method: http.MethodGet, Header: r.Header})
		return

	default:
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
	}
}

// resolveOAuthClient is the canonical OAuth client lookup. Order of precedence:
//
//   1. Explicit creds passed by the caller (the dashboard's create-connection
//      form sends them when the user types into the inline client_id/secret
//      fields).
//   2. Already-saved creds on a prior local connection for the same user +
//      project + app slug. Lets the user enter creds once per app per project.
//   3. OAUTH_<SLUG>_CLIENT_ID / OAUTH_<SLUG>_CLIENT_SECRET env vars. Preserves
//      the original headless deployment story and existing tests.
//
// Returns empty strings if nothing is set anywhere — caller decides whether
// that's an error (it is for ClientIDRequired apps).
func (s *Server) resolveOAuthClient(userID int64, projectID, slug, explicitID, explicitSecret string) (string, string) {
	if explicitID != "" {
		return explicitID, explicitSecret
	}
	storedID, storedSecret := s.findStoredOAuthClient(userID, projectID, slug)
	if storedID != "" {
		return storedID, storedSecret
	}
	return oauthEnvClientID(slug), oauthEnvClientSecret(slug)
}

// localOAuthRedirectURI returns the redirect URI we register with upstream
// providers. Built off s.publicBaseURL() so it follows the DB → env → localhost
// precedence and updates the moment the admin saves a new public URL in
// Settings → Server (no restart required).
func (s *Server) localOAuthRedirectURI() string {
	return s.publicBaseURL() + "/oauth/local/callback"
}

// publicBaseURL is the canonical "where am I reachable from the outside"
// resolver. Precedence:
//
//   1. server_settings.public_url — admin-editable from Settings → Server.
//      Lets a self-hosted user fix the OAuth callback without restarting
//      or shelling into the box. Stored in the same DB as everything else
//      so it survives container redeploys and lives under SERVER_SECRET.
//   2. PUBLIC_URL env var — the original boot-time setting, kept for
//      headless deploys that prefer 12-factor config.
//   3. http://localhost:<PORT> — the dev fallback. OAuth providers can't
//      reach this, but everything else (links in logs, internal URLs)
//      still works locally.
//
// Trailing slashes are stripped so callers can append paths directly.
func (s *Server) publicBaseURL() string {
	if v := s.store.GetSetting("public_url"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if s.publicURL != "" {
		return strings.TrimRight(s.publicURL, "/")
	}
	return "http://localhost:" + s.port
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
func (s *Server) startLocalOAuth(userID int64, app *AppTemplate, connName, projectID, explicitClientID, explicitClientSecret string) (*Connection, string, error) {
	if app.Auth.OAuth2 == nil {
		return nil, "", fmt.Errorf("app %s has no oauth2 config", app.Slug)
	}
	cfg := app.Auth.OAuth2
	clientID, clientSecret := s.resolveOAuthClient(userID, projectID, app.Slug, explicitClientID, explicitClientSecret)
	if cfg.ClientIDRequired && clientID == "" {
		return nil, "", fmt.Errorf("missing client_id for %s — set it in the connect form, on a prior connection, or via env var OAUTH_%s_CLIENT_ID",
			app.Slug, strings.ToUpper(strings.ReplaceAll(app.Slug, "-", "_")))
	}

	// Create pending row. If we have client credentials at this point, fold
	// them into the encrypted blob immediately so the OAuth callback can
	// read them back without trusting state. The blob is empty for users
	// relying purely on env vars (existing behavior).
	var initialBlob string
	if clientID != "" {
		creds := map[string]string{"client_id": clientID}
		if clientSecret != "" {
			creds["client_secret"] = clientSecret
		}
		credsJSON, _ := json.Marshal(creds)
		enc, err := Encrypt(s.secret, string(credsJSON))
		if err != nil {
			return nil, "", fmt.Errorf("encrypt client creds: %w", err)
		}
		initialBlob = enc
	}

	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         userID,
		AppSlug:        app.Slug,
		AppName:        app.Name,
		Name:           connName,
		AuthType:       "oauth2",
		ProjectID:      projectID,
		Source:         "local",
		Status:         "pending",
		EncryptedCreds: initialBlob,
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
	// Merge in provider-specific authorize params. We DO NOT let these
	// override the standard params above (response_type / client_id /
	// redirect_uri / scope / state / code_challenge*) — those are
	// flow-critical and a malformed template shouldn't be able to break
	// the OAuth handshake. Only "new" keys land. The clobber-protection
	// is also why we can't just use q.Set blindly here.
	for k, v := range cfg.ExtraAuthorizeParams {
		if q.Get(k) == "" {
			q.Set(k, v)
		}
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

	// Recover any client_id/client_secret the user supplied at start. They
	// were stored on the pending connection's encrypted blob by
	// startLocalOAuth so the callback doesn't have to trust state and so
	// env-var fallback still works for headless deploys.
	var preBlobCreds map[string]string
	if _, encData, err := s.store.GetConnection(row.UserID, row.ConnectionID); err == nil && encData != "" {
		if plain, err := Decrypt(s.secret, encData); err == nil {
			_ = json.Unmarshal([]byte(plain), &preBlobCreds)
		}
	}
	preClientID, _ := preBlobCreds["client_id"]
	preClientSecret, _ := preBlobCreds["client_secret"]

	tokens, err := s.exchangeOAuthCode(app, code, row.PKCEVerifier, row.UserID, preClientID, preClientSecret)
	if err != nil {
		s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		renderOAuthResult(w, false, "token exchange failed: "+err.Error())
		return
	}

	// Merge the token bundle back onto the existing blob so we KEEP the
	// client credentials in the row. This is what lets the next "Connect
	// GitHub" within the same project skip the credentials form.
	merged := make(map[string]string)
	for k, v := range preBlobCreds {
		merged[k] = v
	}
	for k, v := range tokens {
		merged[k] = v
	}
	encJSON, _ := json.Marshal(merged)
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
//
// The userID + projectID + explicit ID/secret args drive the same
// resolveOAuthClient precedence used at start: explicit creds win, then
// stored creds on prior connections, then env vars. The pending row's
// own blob (set by startLocalOAuth) is what the callback passes here as
// the explicit args.
func (s *Server) exchangeOAuthCode(app *AppTemplate, code, pkceVerifier string, userID int64, explicitClientID, explicitClientSecret string) (map[string]string, error) {
	cfg := app.Auth.OAuth2
	clientID, clientSecret := s.resolveOAuthClient(userID, "", app.Slug, explicitClientID, explicitClientSecret)

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
