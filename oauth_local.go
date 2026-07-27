package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

const (
	oauthStatePurposeConnect = "connect"
	oauthStatePurposeReauth  = "reauth"
)

func oauthEnvClientID(slug string) string {
	return os.Getenv("OAUTH_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_CLIENT_ID")
}

func oauthEnvClientSecret(slug string) string {
	return os.Getenv("OAUTH_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_CLIENT_SECRET")
}

// findStoredOAuthClient looks up OAuth client credentials a user has already
// saved for this app+project combination. Strategy:
//
//  1. Walk the user's existing connections for the same project + slug + source=local,
//     newest first.
//  2. Decrypt each one's credentials blob and check for client_id/client_secret
//     keys. The first hit wins.
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
// Public URL, push relay, and agent lifecycle settings live here so headless
// installs can configure them without editing SQLite directly.
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
		pushRelay := s.mobilePushRelayConfig()
		writeJSON(w, map[string]any{
			"public_url": map[string]any{
				"value":          dbURL,
				"env_value":      envURL,
				"effective":      effective,
				"source":         source,
				"oauth_callback": s.localOAuthRedirectURI(),
			},
			"push_relay": map[string]any{
				"value":     pushRelay.DBURL,
				"env_value": pushRelay.EnvURL,
				"effective": pushRelay.Effective,
				"source":    pushRelay.Source,
				"enabled":   true,
			},
			"agent_lifecycle": map[string]any{
				"update_policy":        s.agentUpdatePolicy(),
				"boot_resume":          s.agentBootResumeMode(),
				"boot_resume_delay":    s.agentBootResumeDelay().String(),
				"rollout_delay":        s.agentRolloutDelay().String(),
				"legacy_detach_active": s.agentShutdownPolicy() == "detach",
			},
		})
		return

	case http.MethodPut:
		var body struct {
			PublicURL            *string `json:"public_url"`
			PushRelayURL         *string `json:"push_relay_url"`
			AgentUpdatePolicy    *string `json:"agent_update_policy"`
			AgentBootResume      *string `json:"agent_boot_resume"`
			AgentBootResumeDelay *string `json:"agent_boot_resume_delay"`
			AgentRolloutDelay    *string `json:"agent_rollout_delay"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if (body.PushRelayURL != nil || body.AgentUpdatePolicy != nil || body.AgentBootResume != nil || body.AgentBootResumeDelay != nil || body.AgentRolloutDelay != nil) && !s.isAdmin(getUserID(r)) {
			http.Error(w, "admin access required", http.StatusForbidden)
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
		if body.PushRelayURL != nil {
			v := strings.TrimRight(strings.TrimSpace(*body.PushRelayURL), "/")
			if v != "" {
				parsed, err := url.Parse(v)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
					http.Error(w, "push_relay_url must be an absolute http:// or https:// URL", http.StatusBadRequest)
					return
				}
			}
			if err := s.store.SetSetting("push_relay_url", v); err != nil {
				http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.AgentUpdatePolicy != nil {
			policy := normalizeAgentUpdatePolicy(*body.AgentUpdatePolicy)
			if policy == "" {
				http.Error(w, "agent_update_policy must be restart, rolling, or preserve", http.StatusBadRequest)
				return
			}
			if err := s.store.SetSetting("agent_update_policy", policy); err != nil {
				http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.AgentBootResume != nil {
			mode := strings.ToLower(strings.TrimSpace(*body.AgentBootResume))
			if mode != "auto" && mode != "staggered" && mode != "manual" {
				http.Error(w, "agent_boot_resume must be auto, staggered, or manual", http.StatusBadRequest)
				return
			}
			if err := s.store.SetSetting("agent_boot_resume", mode); err != nil {
				http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		for key, value := range map[string]*string{
			"agent_boot_resume_delay": body.AgentBootResumeDelay,
			"agent_rollout_delay":     body.AgentRolloutDelay,
		} {
			if value == nil {
				continue
			}
			d, err := time.ParseDuration(strings.TrimSpace(*value))
			invalidZero := key == "agent_boot_resume_delay" && d == 0
			if err != nil || d < 0 || invalidZero || d > time.Hour {
				http.Error(w, key+" must be a valid duration no longer than 1h", http.StatusBadRequest)
				return
			}
			if err := s.store.SetSetting(key, d.String()); err != nil {
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
//  1. Explicit creds passed by the caller (the dashboard's create-connection
//     form sends them when the user types into the inline client_id/secret
//     fields).
//  2. Already-saved creds on a prior local connection for the same user +
//     project + app slug. Lets the user enter creds once per app per project.
//  3. OAUTH_<SLUG>_CLIENT_ID / OAUTH_<SLUG>_CLIENT_SECRET env vars. Preserves
//     the original headless deployment story and existing tests.
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
//  1. server_settings.public_url — admin-editable from Settings → Server.
//     Lets a self-hosted user fix the OAuth callback without restarting
//     or shelling into the box. Stored in the same DB as everything else
//     so it survives container redeploys and lives under SERVER_SECRET.
//  2. PUBLIC_URL env var — the original boot-time setting, kept for
//     headless deploys that prefer 12-factor config.
//  3. http://localhost:<PORT> — the dev fallback. OAuth providers can't
//     reach this, but everything else (links in logs, internal URLs)
//     still works locally.
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
//
// appInstallID + returnURL are populated only when the OAuth dance is
// initiated by an app sidecar via platform.oauth.start. Zero / empty
// means a regular operator-initiated dance from the Integrations admin.
func (s *Store) mintOAuthState(userID, connID int64, appSlug, pkceVerifier string, ttl time.Duration, appInstallID int64, returnURL, purpose string) (string, error) {
	if purpose == "" {
		purpose = oauthStatePurposeConnect
	}
	buf := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	state := "st_" + hex.EncodeToString(buf)
	_, err := s.db.Exec(
		`INSERT INTO oauth_states (state, user_id, connection_id, app_slug, pkce_verifier, expires_at, app_install_id, return_url, purpose)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		state, userID, connID, appSlug, pkceVerifier, time.Now().Add(ttl).UTC(), appInstallID, returnURL, purpose,
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
	AppInstallID int64
	ReturnURL    string
	Purpose      string
	Expired      bool
}

func (s *Store) consumeOAuthState(state string) (*oauthStateRow, error) {
	var row oauthStateRow
	var expiresAt string
	err := s.db.QueryRow(
		`SELECT user_id, connection_id, app_slug, COALESCE(pkce_verifier,''), expires_at,
		        COALESCE(app_install_id,0), COALESCE(return_url,''), COALESCE(purpose,'connect')
		 FROM oauth_states WHERE state = ?`,
		state,
	).Scan(&row.UserID, &row.ConnectionID, &row.AppSlug, &row.PKCEVerifier, &expiresAt, &row.AppInstallID, &row.ReturnURL, &row.Purpose)
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
//
// When ownerAppInstallID > 0, the connection is marked as owned by that
// install (created_via=app_install) and returnURL is recorded on the state
// token so the callback can 302 the browser back into the app's panel
// instead of rendering the dashboard's auto-close page.
func (s *Server) startLocalOAuth(userID int64, app *AppTemplate, connName, projectID, explicitClientID, explicitClientSecret string, supplementalCredentials map[string]string, ownerAppInstallID int64, returnURL string, autoMCP *bool) (*Connection, string, error) {
	if app.Auth.OAuth2 == nil {
		return nil, "", fmt.Errorf("app %s has no oauth2 config", app.Slug)
	}
	cfg := app.Auth.OAuth2
	clientID, clientSecret := s.resolveOAuthClient(userID, projectID, app.Slug, explicitClientID, explicitClientSecret)
	if cfg.ClientIDRequired && clientID == "" {
		return nil, "", fmt.Errorf("missing client_id for %s — set it in the connect form, on a prior connection, or via env var OAUTH_%s_CLIENT_ID",
			app.Slug, strings.ToUpper(strings.ReplaceAll(app.Slug, "-", "_")))
	}

	// Create the pending row with every user-supplied supplemental credential
	// plus any explicit/resolved OAuth client credentials. The callback merges
	// provider-generated tokens onto this blob, preserving values such as a
	// Google Ads developer token or optional manager customer id.
	var initialBlob string
	creds := make(map[string]string, len(supplementalCredentials)+2)
	for key, value := range supplementalCredentials {
		if strings.TrimSpace(key) != "" {
			creds[key] = value
		}
	}
	if clientID != "" {
		creds["client_id"] = clientID
		if clientSecret != "" {
			creds["client_secret"] = clientSecret
		}
	}
	if len(creds) > 0 {
		credsJSON, _ := json.Marshal(creds)
		enc, err := Encrypt(s.secret, string(credsJSON))
		if err != nil {
			return nil, "", fmt.Errorf("encrypt pending credentials: %w", err)
		}
		initialBlob = enc
	}

	connInput := ConnectionInput{
		UserID:         userID,
		AppSlug:        app.Slug,
		AppName:        app.Name,
		Name:           connName,
		AuthType:       "oauth2",
		ProjectID:      projectID,
		Source:         "local",
		Status:         "pending",
		EncryptedCreds: initialBlob,
		AutoMCP:        autoMCP,
	}
	if ownerAppInstallID > 0 {
		// App-initiated: tag the connection so it doesn't auto-create an
		// MCP server (the app is the only intended consumer) and so the
		// operator's Integrations admin can filter it out of its list.
		connInput.CreatedVia = "app_install"
		connInput.OwnerAppInstallID = ownerAppInstallID
	}
	conn, err := s.store.CreateConnectionExt(connInput)
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

	state, err := s.store.mintOAuthState(userID, conn.ID, app.Slug, verifier, 10*time.Minute, ownerAppInstallID, returnURL, oauthStatePurposeConnect)
	if err != nil {
		return nil, "", err
	}

	return conn, s.localOAuthAuthorizeURL(app, state, challenge, clientID), nil
}

func (s *Server) startLocalOAuthReauth(userID, connID int64) (*Connection, string, error) {
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		return nil, "", fmt.Errorf("connection not found")
	}
	if conn.Source != "" && conn.Source != "local" {
		return nil, "", fmt.Errorf("re-auth is only supported for local OAuth integrations")
	}
	if conn.AuthType != "oauth2" {
		return nil, "", fmt.Errorf("connection does not use OAuth2")
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Auth.OAuth2 == nil {
		return nil, "", fmt.Errorf("app %s has no oauth2 config", conn.AppSlug)
	}

	creds := map[string]string{}
	if encCreds != "" {
		plain, err := Decrypt(s.secret, encCreds)
		if err != nil {
			return nil, "", fmt.Errorf("decrypt credentials: %w", err)
		}
		if err := json.Unmarshal([]byte(plain), &creds); err != nil {
			return nil, "", fmt.Errorf("parse credentials: %w", err)
		}
	}
	clientID, clientSecret := creds["client_id"], creds["client_secret"]
	if clientID == "" {
		clientID, clientSecret = s.resolveOAuthClient(userID, conn.ProjectID, app.Slug, "", "")
		if clientID != "" {
			creds["client_id"] = clientID
			if clientSecret != "" {
				creds["client_secret"] = clientSecret
			}
			credsJSON, _ := json.Marshal(creds)
			enc, err := Encrypt(s.secret, string(credsJSON))
			if err != nil {
				return nil, "", fmt.Errorf("encrypt client creds: %w", err)
			}
			if err := s.store.UpdateConnectionCredentials(conn.ID, enc); err != nil {
				return nil, "", fmt.Errorf("persist client creds: %w", err)
			}
		}
	}
	if app.Auth.OAuth2.ClientIDRequired && clientID == "" {
		return nil, "", fmt.Errorf("missing client_id for %s — set it on an OAuth connection or via env var OAUTH_%s_CLIENT_ID",
			app.Slug, strings.ToUpper(strings.ReplaceAll(app.Slug, "-", "_")))
	}

	var verifier, challenge string
	if app.Auth.OAuth2.PKCE {
		verifier, challenge, err = pkcePair()
		if err != nil {
			return nil, "", err
		}
	}
	state, err := s.store.mintOAuthState(userID, conn.ID, app.Slug, verifier, 10*time.Minute, 0, "", oauthStatePurposeReauth)
	if err != nil {
		return nil, "", err
	}
	return conn, s.localOAuthAuthorizeURL(app, state, challenge, clientID), nil
}

// POST /connections/:id/oauth/reauth — start a provider OAuth flow that writes
// the resulting tokens back onto the existing connection row.
func (s *Server) handleReauthConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/connections/"), "/oauth/reauth")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	conn, authURL, err := s.startLocalOAuthReauth(userID, connID)
	if err != nil {
		http.Error(w, "oauth reauth: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"connection":   conn,
		"redirect_url": authURL,
	})
}

func (s *Server) localOAuthAuthorizeURL(app *AppTemplate, state, challenge, clientID string) string {
	cfg := app.Auth.OAuth2
	// Most providers use the OAuth 2.0 standard parameter name "client_id".
	// TikTok is the notable outlier — it demands "client_key" and rejects
	// "client_id" with errCode=10003. Catalog entries can override per-
	// integration via auth.oauth2.client_id_param_name.
	clientIDParam := cfg.ClientIDParamName
	if clientIDParam == "" {
		clientIDParam = "client_id"
	}
	// Most providers accept space-joined scopes per RFC 6749 §3.3.
	// TikTok wants commas; let the catalog override the separator.
	scopeSep := cfg.ScopeSeparator
	if scopeSep == "" {
		scopeSep = " "
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set(clientIDParam, clientID)
	q.Set("redirect_uri", s.localOAuthRedirectURI())
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, scopeSep))
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
	return cfg.AuthorizeURL + sep + q.Encode()
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

	log.Printf("[OAUTH-CB] received state=%s code=%s err=%s", maskMiddle(state, 6, 4), maskMiddle(code, 6, 4), errParam)

	row, err := s.store.consumeOAuthState(state)
	if err != nil {
		log.Printf("[OAUTH-CB] consumeOAuthState FAILED state=%s: %v", maskMiddle(state, 6, 4), err)
		http.Error(w, "unknown or expired state", http.StatusBadRequest)
		return
	}
	log.Printf("[OAUTH-CB] state→connection row: conn=%d user=%d slug=%s purpose=%s app_install=%d return_url=%q has_pkce=%t expired=%t",
		row.ConnectionID, row.UserID, row.AppSlug, row.Purpose, row.AppInstallID, row.ReturnURL, row.PKCEVerifier != "", row.Expired)

	if row.Expired {
		log.Printf("[OAUTH-CB] state expired conn=%d", row.ConnectionID)
		if row.Purpose != oauthStatePurposeReauth {
			s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		}
		http.Error(w, "state expired — re-initiate the connection", http.StatusBadRequest)
		return
	}
	if errParam != "" {
		log.Printf("[OAUTH-CB] provider returned error conn=%d: %s", row.ConnectionID, errParam)
		if row.Purpose != oauthStatePurposeReauth {
			s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		}
		renderOAuthResult(w, false, "provider returned error: "+errParam)
		return
	}
	if code == "" {
		log.Printf("[OAUTH-CB] missing code conn=%d", row.ConnectionID)
		if row.Purpose != oauthStatePurposeReauth {
			s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		}
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	app := s.catalog.Get(row.AppSlug)
	if app == nil || app.Auth.OAuth2 == nil {
		log.Printf("[OAUTH-CB] app missing from catalog or no oauth2 config slug=%s app_present=%t", row.AppSlug, app != nil)
		http.Error(w, "app missing from catalog", http.StatusInternalServerError)
		return
	}
	log.Printf("[OAUTH-CB] catalog hit slug=%s kind=%q has_mcp=%t authorize_url=%s token_url=%s pkce=%t",
		app.Slug, app.Kind, app.MCP != nil, app.Auth.OAuth2.AuthorizeURL, app.Auth.OAuth2.TokenURL, app.Auth.OAuth2.PKCE)

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
	log.Printf("[OAUTH-CB] pre-blob: client_id_present=%t client_secret_present=%t other_keys=%v",
		preClientID != "", preClientSecret != "", filterKeys(preBlobCreds, "client_id", "client_secret"))

	tokens, err := s.exchangeOAuthCode(app, code, row.PKCEVerifier, row.UserID, preClientID, preClientSecret)
	if err != nil {
		log.Printf("[OAUTH-CB] token exchange FAILED conn=%d slug=%s: %v", row.ConnectionID, app.Slug, err)
		if row.Purpose != oauthStatePurposeReauth {
			s.store.UpdateConnectionStatus(row.ConnectionID, "failed")
		}
		renderOAuthResult(w, false, "token exchange failed: "+err.Error())
		return
	}
	tokKeys := make([]string, 0, len(tokens))
	for k := range tokens {
		tokKeys = append(tokKeys, k)
	}
	log.Printf("[OAUTH-CB] token exchange OK conn=%d slug=%s keys=%v",
		row.ConnectionID, app.Slug, tokKeys)

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
		log.Printf("[OAUTH-CB] encryption FAILED conn=%d: %v", row.ConnectionID, err)
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateConnectionCredentials(row.ConnectionID, enc); err != nil {
		log.Printf("[OAUTH-CB] UpdateConnectionCredentials FAILED conn=%d: %v", row.ConnectionID, err)
		http.Error(w, "db update failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateConnectionStatus(row.ConnectionID, "active"); err != nil {
		log.Printf("[OAUTH-CB] UpdateConnectionStatus(active) FAILED conn=%d: %v", row.ConnectionID, err)
	}
	log.Printf("[OAUTH-CB] credentials saved + status=active conn=%d slug=%s merged_keys=%d", row.ConnectionID, app.Slug, len(merged))

	// Auto-create the mcp_servers row for new connects (mirrors the non-OAuth
	// path in handleCreateConnection). Re-auth keeps existing local MCP rows
	// intact; kind=remote_mcp rows are re-written only when one already exists
	// or the connection was configured for auto-MCP.
	//
	// SKIP this entirely when:
	//   - an app owns the connection (created_via=app_install) — the
	//     app is the only intended consumer, exposing tools globally
	//     would defeat the binding model.
	//   - the operator opted out at connect time (auto_mcp=0 on the
	//     connection row).
	shouldAutoMCP := row.AppInstallID == 0 && connectionAutoMCPFlag(s, row.ConnectionID)
	shouldCreateMCP := shouldAutoMCP && row.Purpose != oauthStatePurposeReauth
	shouldRefreshRemoteMCP := app.Kind == "remote_mcp" && (shouldAutoMCP || hasMCPServerForConnection(s, row.ConnectionID))
	if shouldCreateMCP || shouldRefreshRemoteMCP {
		conn, encCreds, err := s.store.GetConnection(row.UserID, row.ConnectionID)
		if err != nil {
			log.Printf("[OAUTH-CB] GetConnection FAILED conn=%d: %v", row.ConnectionID, err)
		} else {
			log.Printf("[OAUTH-CB] dispatch auto-mcp conn=%d slug=%s kind=%q", conn.ID, conn.AppSlug, app.Kind)
			if app.Kind == "remote_mcp" {
				if mcpID, merr := s.createRemoteMcpFromConnection(row.UserID, conn, app, encCreds); merr != nil {
					log.Printf("[OAUTH-CB] remote-mcp auto-mcp FAILED conn=%d slug=%s: %v", conn.ID, conn.AppSlug, merr)
				} else {
					log.Printf("[OAUTH-CB] remote-mcp auto-mcp OK conn=%d slug=%s mcp_id=%d", conn.ID, conn.AppSlug, mcpID)
				}
			} else {
				if mcpID, merr := s.store.CreateMCPServerFromConnection(row.UserID, conn, len(app.Tools)); merr != nil {
					log.Printf("[OAUTH-CB] rest auto-mcp FAILED conn=%d slug=%s: %v", conn.ID, conn.AppSlug, merr)
				} else {
					log.Printf("[OAUTH-CB] rest auto-mcp OK conn=%d slug=%s mcp_id=%d tools=%d", conn.ID, conn.AppSlug, mcpID, len(app.Tools))
				}
			}
		}
	} else {
		if row.Purpose == oauthStatePurposeReauth {
			log.Printf("[OAUTH-CB] skipping local MCP create on reauth conn=%d", row.ConnectionID)
		} else if row.AppInstallID > 0 {
			log.Printf("[OAUTH-CB] skipping auto-mcp conn=%d (app_install_id=%d owns the connection)", row.ConnectionID, row.AppInstallID)
		} else {
			log.Printf("[OAUTH-CB] skipping auto-mcp conn=%d (operator opted out via auto_mcp=false)", row.ConnectionID)
		}
	}

	// App-initiated dance: 302 the browser back into the app's panel,
	// where it can read the conn_id and run the next step (page picker,
	// finalize, etc). Operator-initiated dance: render the auto-close
	// HTML page as before so the popup goes away.
	if row.ReturnURL != "" {
		sep := "?"
		if strings.Contains(row.ReturnURL, "?") {
			sep = "&"
		}
		dest := fmt.Sprintf("%s%sconn_id=%d&status=ok", row.ReturnURL, sep, row.ConnectionID)
		http.Redirect(w, r, dest, http.StatusFound)
		return
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

	// Same client-id-param override as the authorize step — TikTok wants
	// "client_key" for the token exchange too, not just the authorize URL.
	clientIDParam := cfg.ClientIDParamName
	if clientIDParam == "" {
		clientIDParam = "client_id"
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.localOAuthRedirectURI())
	useBasicOnly := cfg.TokenAuthBasicOnly && clientSecret != ""
	if !useBasicOnly {
		form.Set(clientIDParam, clientID)
	}
	if clientSecret != "" && !useBasicOnly {
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
<script>
try {
  if (window.opener) {
    window.opener.postMessage({type:"apteva-oauth-result", ok:%t}, window.location.origin);
  }
} catch (e) {}
setTimeout(function(){window.close()},2500);
</script>
</body></html>`, title, color, title, msg, ok)
}

// maskMiddle returns a redacted view of a secret-ish value for log
// lines: first `head` chars, an ellipsis, then last `tail` chars.
// Empty input renders as "(empty)" so the absence is visible. Short
// inputs that wouldn't have a useful middle to hide are stamped as
// "<short:N>" instead of leaking the whole value.
func maskMiddle(s string, head, tail int) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) <= head+tail+2 {
		return fmt.Sprintf("<short:%d>", len(s))
	}
	return s[:head] + "…" + s[len(s)-tail:] + fmt.Sprintf("(%d)", len(s))
}

// filterKeys returns the keys of `m` minus any listed in `drop`.
// Used in OAuth-callback logs to enumerate which non-credential
// fields were on a pre-existing connection blob without leaking
// values.
func filterKeys(m map[string]string, drop ...string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	out := make([]string, 0, len(m))
	for k := range m {
		if !skip[k] {
			out = append(out, k)
		}
	}
	return out
}
