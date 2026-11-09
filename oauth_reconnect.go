package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const oauthStatePurposeAppReconnect = "app_reconnect"
const oauthReconnectTTL = 10 * time.Minute

type oauthReconnectIdentity struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type appOAuthReconnect struct {
	ID           string `json:"id"`
	ConnectionID int64  `json:"connection_id"`
	Status       string `json:"status"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
	ExpiresAt    string `json:"expires_at"`
}

func (s *Server) handleAppOAuthReconnect(w http.ResponseWriter, r *http.Request, status bool) {
	install, err := requireInstallID(r)
	if err != nil {
		http.Error(w, "app authentication required", http.StatusUnauthorized)
		return
	}
	if !s.requireRuntimePermission(w, install, sdk.PermOAuthStart) {
		return
	}
	var args struct {
		ID                 string                   `json:"id"`
		ConnectionID       int64                    `json:"connection_id"`
		ExpectedIdentities []oauthReconnectIdentity `json:"expected_identities"`
		ProjectID          string                   `json:"project_id"`
		Cancel             bool                     `json:"cancel"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&args); err != nil {
		http.Error(w, "invalid OAuth reconnect request", http.StatusBadRequest)
		return
	}
	user, project, ok := s.runtimeCallerProject(w, r, install, args.ProjectID, ProjectEditor)
	if !ok {
		return
	}
	if status {
		if args.Cancel {
			_, err = s.store.db.Exec(`UPDATE app_oauth_reconnects SET status='cancelled' WHERE id=? AND install_id=? AND user_id=? AND project_id=? AND status IN ('pending','checking')`, args.ID, install, user, project)
			if err != nil {
				http.Error(w, "could not cancel reconnect", http.StatusInternalServerError)
				return
			}
		}
		var out appOAuthReconnect
		var expires int64
		err = s.store.db.QueryRow(`SELECT id,connection_id,status,expires_at FROM app_oauth_reconnects WHERE id=? AND install_id=? AND user_id=? AND project_id=?`, args.ID, install, user, project).
			Scan(&out.ID, &out.ConnectionID, &out.Status, &expires)
		if err != nil {
			http.Error(w, "reconnect request not found", http.StatusNotFound)
			return
		}
		if (out.Status == "pending" || out.Status == "checking") && expires <= time.Now().Unix() {
			out.Status = "expired"
		}
		out.ExpiresAt = time.Unix(expires, 0).UTC().Format(time.RFC3339)
		writeJSON(w, out)
		return
	}
	conn, encrypted, err := s.store.GetConnection(user, args.ConnectionID)
	if err != nil || conn.OwnerAppInstallID != install || conn.ProjectID != project ||
		conn.Source != "local" || conn.AuthType != "oauth2" || conn.CredentialManagement != "user" {
		http.Error(w, "app-owned OAuth connection not found in this project", http.StatusNotFound)
		return
	}
	if !validReconnectIdentities(conn.AppSlug, args.ExpectedIdentities) {
		http.Error(w, "verified reconnect requires exact identities for this integration", http.StatusBadRequest)
		return
	}
	expected, _ := json.Marshal(args.ExpectedIdentities)
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Auth.OAuth2 == nil {
		http.Error(w, "OAuth catalog unavailable", http.StatusConflict)
		return
	}
	old, err := s.reconnectCredentials(encrypted)
	if err != nil {
		http.Error(w, "could not load OAuth configuration", http.StatusInternalServerError)
		return
	}
	clientID := old["client_id"]
	if clientID == "" {
		clientID, _ = s.resolveOAuthClient(user, project, app.Slug, "", "")
	}
	if clientID == "" {
		http.Error(w, "OAuth client is not configured", http.StatusConflict)
		return
	}
	var verifier, challenge string
	if app.Auth.OAuth2.PKCE {
		verifier, challenge, err = pkcePair()
		if err != nil {
			http.Error(w, "could not prepare OAuth", http.StatusInternalServerError)
			return
		}
	}
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "could not prepare OAuth", http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(idBytes)
	state, err := s.store.mintOAuthState(user, conn.ID, app.Slug, verifier, oauthReconnectTTL, install, "", oauthStatePurposeAppReconnect)
	if err != nil {
		http.Error(w, "could not prepare OAuth", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(oauthReconnectTTL).UTC()
	_, err = s.store.db.Exec(`INSERT INTO app_oauth_reconnects(id,oauth_state,connection_id,install_id,user_id,project_id,expected_identities,expires_at,credential_digest)
		VALUES(?,?,?,?,?,?,?,?,?)`, id, state, conn.ID, install, user, project, string(expected), expires.Unix(), reconnectCredentialDigest(encrypted))
	if err != nil {
		_, _ = s.store.db.Exec(`DELETE FROM oauth_states WHERE state=?`, state)
		http.Error(w, "could not save OAuth request", http.StatusInternalServerError)
		return
	}
	_, _ = s.store.db.Exec(`DELETE FROM app_oauth_reconnects WHERE expires_at < unixepoch()-86400`)
	authorizeURL := s.localOAuthAuthorizeURL(app, state, challenge, clientID)
	if app.Slug == "facebook-api" {
		authorizeURL += "&auth_type=rerequest"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, appOAuthReconnect{ID: id, ConnectionID: conn.ID, Status: "pending",
		AuthorizeURL: authorizeURL, ExpiresAt: expires.Format(time.RFC3339)})
}

func reconnectCredentialDigest(encrypted string) string {
	sum := sha256.Sum256([]byte(encrypted))
	return hex.EncodeToString(sum[:])
}

func (s *Server) reconnectCredentials(encrypted string) (map[string]string, error) {
	creds := map[string]string{}
	if encrypted == "" {
		return creds, nil
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(plain), &creds)
	return creds, err
}

func (s *Server) finishAppOAuthReconnect(w http.ResponseWriter, r *http.Request, row *oauthStateRow, state, code, providerError string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	var expected, project, credentialDigest string
	err := s.store.db.QueryRow(`UPDATE app_oauth_reconnects SET status='checking'
		WHERE oauth_state=? AND connection_id=? AND install_id=? AND user_id=? AND status='pending' AND expires_at>unixepoch()
		RETURNING expected_identities,project_id,credential_digest`, state, row.ConnectionID, row.AppInstallID, row.UserID).Scan(&expected, &project, &credentialDigest)
	if err != nil {
		renderOAuthResult(w, false, "This reconnect request is no longer active. Return to Social.")
		return
	}
	finish := func(status, message string) {
		_, _ = s.store.db.Exec(`UPDATE app_oauth_reconnects SET status=? WHERE oauth_state=? AND status='checking'`, status, state)
		renderOAuthResult(w, false, message)
	}
	if providerError == "access_denied" {
		finish("cancelled", "Reconnect cancelled. Return to Social.")
		return
	}
	if row.Expired || providerError != "" || code == "" {
		finish("failed", "Authorization did not finish. Return to Social.")
		return
	}
	conn, encrypted, err := s.store.GetConnection(row.UserID, row.ConnectionID)
	if err != nil || conn.OwnerAppInstallID != row.AppInstallID || conn.ProjectID != project ||
		conn.AppSlug != row.AppSlug || conn.AuthType != "oauth2" || conn.Source != "local" || conn.CredentialManagement != "user" ||
		reconnectCredentialDigest(encrypted) != credentialDigest {
		finish("failed", "The original connection is unavailable. Return to Social.")
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	old, err := s.reconnectCredentials(encrypted)
	if err != nil || app == nil || app.Auth.OAuth2 == nil {
		finish("failed", "OAuth configuration is unavailable. Return to Social.")
		return
	}
	clientID, clientSecret := old["client_id"], old["client_secret"]
	if clientID == "" {
		clientID, clientSecret = s.resolveOAuthClient(row.UserID, project, app.Slug, "", "")
	}
	candidate, err := s.exchangeOAuthCode(app, code, row.PKCEVerifier, row.UserID, clientID, clientSecret, old)
	if err != nil || candidate["access_token"] == "" || (conn.AppSlug != "facebook-api" && candidate["refresh_token"] == "") {
		finish("failed", "The provider did not return a durable authorization. Return to Social.")
		return
	}
	var identities []oauthReconnectIdentity
	if json.Unmarshal([]byte(expected), &identities) != nil || !validReconnectIdentities(conn.AppSlug, identities) {
		finish("failed", "The saved reconnect identities are invalid. Return to Social.")
		return
	}
	// Preserve supplemental connector settings while dropping metadata that
	// describes the previous grant.
	for _, key := range []string{"access_token", "refresh_token", "refreshToken", "expires_at", "token_expires_at", "expires_in", "refresh_expires_in", "token_type", "scope", "scopes", "id_token", "open_id"} {
		delete(old, key)
	}
	for k, v := range candidate {
		old[k] = v
	}
	old["client_id"], old["client_secret"] = clientID, clientSecret
	match, err := verifyOAuthReconnectIdentities(r, app, old, identities)
	if err != nil {
		finish("failed", "Could not verify the authorized account. Return to Social.")
		return
	}
	if !match {
		finish("wrong_account", "The provider authorized a different account or omitted a linked destination. Return to Social.")
		return
	}
	raw, _ := json.Marshal(old)
	replacement, err := Encrypt(s.secret, string(raw))
	if err != nil || s.commitAppOAuthReconnect(r, state, row, project, encrypted, replacement) != nil {
		finish("failed", "The reconnect could not be saved. Return to Social.")
		return
	}
	renderOAuthResult(w, true, "Authorization verified. Return to Social to check account access.")
}

func (s *Server) commitAppOAuthReconnect(r *http.Request, state string, row *oauthStateRow, project, original, replacement string) error {
	tx, err := s.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	out, err := tx.Exec(`UPDATE app_oauth_reconnects SET status='authorized'
		WHERE oauth_state=? AND status='checking' AND expires_at>unixepoch()`, state)
	if err != nil {
		return err
	}
	if count, err := out.RowsAffected(); err != nil || count != 1 {
		return sql.ErrNoRows
	}
	out, err = tx.Exec(`UPDATE connections SET encrypted_credentials=?,status='active'
		WHERE id=? AND user_id=? AND owner_app_install_id=? AND project_id=? AND app_slug=? AND encrypted_credentials=?`,
		replacement, row.ConnectionID, row.UserID, row.AppInstallID, project, row.AppSlug, original)
	if err != nil {
		return err
	}
	if count, err := out.RowsAffected(); err != nil || count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
