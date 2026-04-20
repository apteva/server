package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// --- Connection invites ---
//
// A stateless shareable link that lets a non-dashboard user (a client, an
// end-user, anyone without a login) complete an integration setup on behalf
// of the operator who minted the link. No DB table: the invite IS the
// signed token (HMAC-SHA256 over the payload with s.secret as the key).
//
// What it supports:
//   - local API-key apps: create new connection OR update creds on an
//     existing connection (true "swap the API key" flow)
//   - local OAuth2 apps: create new connection (full consent screen round-trip)
//   - composio apps: create new connection via Composio's hosted flow
//
// What it gives up (vs. a stored-invite table):
//   - No pre-use revocation — a leaked link is valid until exp.
//     Mitigation: short default TTL (1h), operator bumps when needed.
//   - No "who clicked" audit trail. Connection row after fulfill is the
//     only record. Acceptable for MVP.
//
// Single-use-ish enforcement relies on the existing UNIQUE
// (user_id, project_id, app_slug, name) index on connections: replayed
// "create new" attempts hit a 409. "Update existing" is idempotent by
// design — re-fulfilling just re-sets the same creds, which is fine.

// InvitePayload is what gets signed into the token. Keep keys short — the
// token ends up in the URL and we'd rather not waste bytes.
type InvitePayload struct {
	Op      int64  `json:"op"`             // operator user_id — the invite acts on their behalf
	Proj    string `json:"proj,omitempty"` // project_id the connection lands in
	App     string `json:"app"`            // app_slug (e.g. "gmail")
	Src     string `json:"src"`            // "local" | "composio"
	ConnID  int64  `json:"cid,omitempty"`  // if set, update an existing connection; else create new
	ProvID  int64  `json:"pid,omitempty"`  // composio provider_id (for src=composio)
	Tools   string `json:"tools,omitempty"`
	Name    string `json:"name,omitempty"`
	Exp     int64  `json:"exp"`
}

// signInvite produces a `payload.sig` URL-safe token.
func (s *Server) signInvite(p InvitePayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, nil
}

// verifyInvite returns the payload if the token's signature is valid AND
// it hasn't expired. Otherwise returns an error suitable for HTTP 400.
func (s *Server) verifyInvite(token string) (*InvitePayload, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed token")
	}
	body, sig := parts[0], parts[1]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return nil, fmt.Errorf("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("bad payload encoding")
	}
	var p InvitePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("bad payload json")
	}
	if p.Exp > 0 && time.Now().Unix() > p.Exp {
		return nil, fmt.Errorf("expired")
	}
	return &p, nil
}

// --- Operator endpoints ---

// POST /api/invites — operator mints a shareable URL.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		ProjectID    string `json:"project_id"`
		AppSlug      string `json:"app_slug"`
		Source       string `json:"source"`                   // "local" | "composio"
		ConnectionID int64  `json:"connection_id"`            // optional — reauth an existing conn
		ProviderID   int64  `json:"provider_id,omitempty"`    // composio
		Tools        string `json:"allowed_tools,omitempty"`
		Name         string `json:"name,omitempty"`
		TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.AppSlug == "" {
		http.Error(w, "app_slug required", http.StatusBadRequest)
		return
	}
	if body.Source == "" {
		body.Source = "local"
	}

	// If reauthing, validate the connection belongs to this operator.
	if body.ConnectionID != 0 {
		conn, _, err := s.store.GetConnection(userID, body.ConnectionID)
		if err != nil || conn == nil {
			http.Error(w, "connection not found", http.StatusNotFound)
			return
		}
		// Lock app/source/project to the connection so the invite can't be
		// retargeted to a different integration after the fact.
		body.AppSlug = conn.AppSlug
		body.Source = conn.Source
		body.ProjectID = conn.ProjectID
	}

	ttl := time.Duration(body.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = time.Hour // default 1h; cap 7 days
	}

	payload := InvitePayload{
		Op:     userID,
		Proj:   body.ProjectID,
		App:    body.AppSlug,
		Src:    body.Source,
		ConnID: body.ConnectionID,
		ProvID: body.ProviderID,
		Tools:  body.Tools,
		Name:   body.Name,
		Exp:    time.Now().Add(ttl).Unix(),
	}
	token, err := s.signInvite(payload)
	if err != nil {
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"token":      token,
		"url":        s.publicBaseURL() + "/connect/" + token,
		"expires_at": time.Unix(payload.Exp, 0).UTC().Format(time.RFC3339),
	})
}

// --- Public endpoints (no auth) ---

// GET /connect/:token — returns JSON describing the invite so the public
// page can render the right form. No side effects.
func (s *Server) handlePublicInvite(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/connect/")
	token = strings.TrimSuffix(token, "/")
	if i := strings.Index(token, "/"); i >= 0 {
		token = token[:i]
	}
	p, err := s.verifyInvite(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := map[string]any{
		"source":        p.Src,
		"app_slug":      p.App,
		"project_id":    p.Proj,
		"connection_id": p.ConnID,
		"name":          p.Name,
		"allowed_tools": p.Tools,
		"expires_at":    time.Unix(p.Exp, 0).UTC().Format(time.RFC3339),
	}

	// Enrich with the app template so the public page can render the right
	// credential fields / OAuth button without making a second auth'd call.
	if p.Src == "local" {
		app := s.catalog.Get(p.App)
		if app == nil {
			http.Error(w, "app not in catalog", http.StatusNotFound)
			return
		}
		resp["app_name"] = app.Name
		resp["auth_types"] = app.Auth.Types
		resp["credential_fields"] = app.Auth.CredentialFields
		resp["has_oauth2"] = app.Auth.OAuth2 != nil
	} else if p.Src == "composio" {
		resp["app_name"] = p.App
	}

	// If reauthing an existing connection, surface the human-readable
	// connection name (handy for the client to confirm "yes, I'm
	// refreshing the Gmail creds for marketing@acme.com").
	if p.ConnID != 0 {
		if c, _, err := s.store.GetConnection(p.Op, p.ConnID); err == nil && c != nil {
			resp["connection_name"] = c.Name
			resp["app_name"] = c.AppName
		}
	}

	writeJSON(w, resp)
}

// POST /connect/:token/fulfill — client submits credentials (api_key flow)
// or triggers the OAuth / Composio redirect. Body:
//   { credentials?: {...}, name?: "..." }
// Response:
//   { status: "connected", connection_id }         // api_key path, new
//   { status: "updated",   connection_id }         // api_key path, reauth
//   { status: "redirect",  redirect_url }          // oauth2 / composio
func (s *Server) handleFulfillInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/connect/")
	token = strings.TrimSuffix(token, "/fulfill")
	p, err := s.verifyInvite(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var body struct {
		Credentials map[string]string `json:"credentials"`
		Name        string            `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = p.Name
	}

	switch p.Src {
	case "local":
		s.fulfillLocalInvite(w, p, &body)
	case "composio":
		s.fulfillComposioInvite(w, p)
	default:
		http.Error(w, "unknown source", http.StatusBadRequest)
	}
}

// fulfillLocalInvite handles new-connection OR reauth for local-catalog apps.
// For OAuth2, returns a redirect URL. For api_key/basic/bearer, writes the
// credentials immediately and returns the connection id.
func (s *Server) fulfillLocalInvite(w http.ResponseWriter, p *InvitePayload, body *struct {
	Credentials map[string]string `json:"credentials"`
	Name        string            `json:"name"`
}) {
	app := s.catalog.Get(p.App)
	if app == nil {
		http.Error(w, "app not in catalog", http.StatusNotFound)
		return
	}

	// Pick auth type: prefer oauth2 when configured (matches
	// handleCreateConnection's logic so invite flow == dashboard flow).
	authType := ""
	switch {
	case app.Auth.OAuth2 != nil && containsString(app.Auth.Types, "oauth2"):
		authType = "oauth2"
	case len(app.Auth.Types) > 0:
		authType = app.Auth.Types[0]
	default:
		authType = "api_key"
	}

	// --- Reauth path: patch credentials on an existing connection ---
	if p.ConnID != 0 {
		conn, _, err := s.store.GetConnection(p.Op, p.ConnID)
		if err != nil || conn == nil {
			http.Error(w, "connection not found", http.StatusNotFound)
			return
		}
		if authType == "oauth2" {
			// Full reauth via OAuth isn't in this MVP — would require
			// deleting the pending row created by startLocalOAuth and
			// patching the original on callback. Point users at the
			// dashboard for now.
			http.Error(w, "OAuth reauth via invite not yet supported — delete and recreate the connection", http.StatusNotImplemented)
			return
		}
		if len(body.Credentials) == 0 {
			http.Error(w, "credentials required", http.StatusBadRequest)
			return
		}
		credsJSON, _ := json.Marshal(body.Credentials)
		enc, err := Encrypt(s.secret, string(credsJSON))
		if err != nil {
			http.Error(w, "encrypt failed", http.StatusInternalServerError)
			return
		}
		if _, err := s.store.db.Exec(
			"UPDATE connections SET encrypted_credentials = ?, status = 'active' WHERE id = ? AND user_id = ?",
			enc, conn.ID, p.Op,
		); err != nil {
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"status": "updated", "connection_id": conn.ID})
		return
	}

	// --- New connection path ---
	connName := body.Name
	if connName == "" {
		connName = app.Name
	}

	if authType == "oauth2" {
		// startLocalOAuth uses the operator's saved OAuth client creds
		// (resolveOAuthClient walks DB → env). The client never sees the
		// client_id/secret. Standard callback creates the connection.
		conn, authURL, err := s.startLocalOAuth(p.Op, app, connName, p.Proj, "", "")
		if err != nil {
			http.Error(w, "oauth start: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"status":        "redirect",
			"redirect_url":  authURL,
			"connection_id": conn.ID,
		})
		return
	}

	if len(body.Credentials) == 0 {
		http.Error(w, "credentials required", http.StatusBadRequest)
		return
	}
	credsJSON, _ := json.Marshal(body.Credentials)
	enc, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		http.Error(w, "encrypt failed", http.StatusInternalServerError)
		return
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         p.Op,
		AppSlug:        app.Slug,
		AppName:        app.Name,
		Name:           connName,
		AuthType:       authType,
		EncryptedCreds: enc,
		ProjectID:      p.Proj,
		Source:         "local",
		Status:         "active",
	})
	if err != nil {
		// Most likely a UNIQUE violation — the invite was already fulfilled.
		http.Error(w, "create failed: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"status": "connected", "connection_id": conn.ID})
}

// fulfillComposioInvite kicks off the Composio hosted connect flow on
// behalf of the operator and returns the redirect URL the client should
// be sent to. Composio handles the callback itself; the connection flips
// from pending→active via the existing poll path in handleGetConnection.
func (s *Server) fulfillComposioInvite(w http.ResponseWriter, p *InvitePayload) {
	if p.ConnID != 0 {
		http.Error(w, "Composio reauth via invite not yet supported", http.StatusNotImplemented)
		return
	}
	if p.ProvID == 0 {
		http.Error(w, "invite missing composio provider_id", http.StatusBadRequest)
		return
	}
	client, err := s.composioClientFor(p.Op, p.ProvID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	endUserID := composioEndUserID(p.Op, p.Proj)
	// authMode + creds pass empty — the public page can't collect a custom
	// OAuth client. Relies on Composio's default auth config for the toolkit.
	acct, redirectURL, err := client.InitiateConnection(p.App, "", endUserID, nil, nil)
	if err != nil {
		http.Error(w, "composio initiate: "+err.Error(), http.StatusBadGateway)
		return
	}
	connName := p.Name
	if connName == "" {
		connName = p.App
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:     p.Op,
		AppSlug:    p.App,
		AppName:    p.App,
		Name:       connName,
		AuthType:   "composio",
		ProjectID:  p.Proj,
		Source:     "composio",
		Status:     "pending",
		ProviderID: p.ProvID,
		ExternalID: acct.ID,
	})
	if err != nil {
		http.Error(w, "create failed: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{
		"status":        "redirect",
		"redirect_url":  redirectURL,
		"connection_id": conn.ID,
	})
}
