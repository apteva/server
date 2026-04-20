package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"
)

// --- DB Model ---

type Connection struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	AppSlug    string    `json:"app_slug"`
	AppName    string    `json:"app_name"`
	Name       string    `json:"name"`
	AuthType   string    `json:"auth_type"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`                 // 'local' | 'composio'
	ProviderID int64     `json:"provider_id,omitempty"`  // FK → providers (for hosted sources)
	ExternalID string    `json:"external_id,omitempty"`  // composio connected_account_id, etc.
	ProjectID  string    `json:"project_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ConnectionInput carries the full set of fields for creating a connection via
// any source (local, composio, ...). Use this for new code paths; the legacy
// CreateConnection(...) helper below is kept so existing tests and mcp_gateway
// don't need to change.
// containsString returns true when needle appears in haystack.
// Tiny helper used by the auth-type selector — pulled out so the switch
// statement above stays readable.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

type ConnectionInput struct {
	UserID         int64
	AppSlug        string
	AppName        string
	Name           string
	AuthType       string
	EncryptedCreds string
	ProjectID      string
	Source         string // '' → 'local'
	Status         string // '' → 'active'
	ProviderID     int64
	ExternalID     string
}

// --- Store methods ---

// CreateConnection is the legacy helper — local-source, active status, no provider.
// Prefer CreateConnectionExt for new code.
func (s *Store) CreateConnection(userID int64, appSlug, appName, name, authType, encryptedCreds, projectID string) (*Connection, error) {
	return s.CreateConnectionExt(ConnectionInput{
		UserID: userID, AppSlug: appSlug, AppName: appName, Name: name,
		AuthType: authType, EncryptedCreds: encryptedCreds, ProjectID: projectID,
	})
}

func (s *Store) CreateConnectionExt(in ConnectionInput) (*Connection, error) {
	if in.Source == "" {
		in.Source = "local"
	}
	if in.Status == "" {
		in.Status = "active"
	}
	result, err := s.db.Exec(
		"INSERT INTO connections (user_id, app_slug, app_name, name, auth_type, encrypted_credentials, status, project_id, source, provider_id, external_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.UserID, in.AppSlug, in.AppName, in.Name, in.AuthType, in.EncryptedCreds, in.Status, in.ProjectID, in.Source, in.ProviderID, in.ExternalID,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Connection{
		ID: id, UserID: in.UserID, AppSlug: in.AppSlug, AppName: in.AppName, Name: in.Name,
		AuthType: in.AuthType, Status: in.Status, Source: in.Source, ProviderID: in.ProviderID,
		ExternalID: in.ExternalID, ProjectID: in.ProjectID, CreatedAt: time.Now(),
	}, nil
}

func (s *Store) ListConnections(userID int64, projectID ...string) ([]Connection, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			`SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''), created_at
			 FROM connections WHERE user_id = ? AND project_id = ?`, userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			`SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''), created_at
			 FROM connections WHERE user_id = ?`, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conns []Connection
	for rows.Next() {
		var c Connection
		var createdAt string
		rows.Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &c.Status, &c.Source, &c.ProviderID, &c.ExternalID, &c.ProjectID, &createdAt)
		c.UserID = userID
		c.CreatedAt, _ = parseTime(createdAt)
		conns = append(conns, c)
	}
	return conns, nil
}

func (s *Store) GetConnection(userID, connID int64) (*Connection, string, error) {
	var c Connection
	var encCreds, createdAt string
	err := s.db.QueryRow(
		`SELECT id, app_slug, app_name, name, auth_type, encrypted_credentials, status, COALESCE(source,'local'), COALESCE(provider_id,0), COALESCE(external_id,''), COALESCE(project_id,''), created_at
		 FROM connections WHERE id = ? AND user_id = ?`,
		connID, userID,
	).Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &encCreds, &c.Status, &c.Source, &c.ProviderID, &c.ExternalID, &c.ProjectID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	c.UserID = userID
	c.CreatedAt, _ = parseTime(createdAt)
	return &c, encCreds, nil
}

// UpdateConnectionStatus flips a connection's status (pending → active → failed).
func (s *Store) UpdateConnectionStatus(connID int64, status string) error {
	_, err := s.db.Exec("UPDATE connections SET status = ? WHERE id = ?", status, connID)
	return err
}

// UpdateConnectionCredentials replaces the encrypted credential blob (used after
// local OAuth token exchange and on refresh).
func (s *Store) UpdateConnectionCredentials(connID int64, encryptedCreds string) error {
	_, err := s.db.Exec("UPDATE connections SET encrypted_credentials = ? WHERE id = ?", encryptedCreds, connID)
	return err
}

func (s *Store) DeleteConnection(userID, connID int64) error {
	_, err := s.db.Exec("DELETE FROM connections WHERE id = ? AND user_id = ?", connID, userID)
	return err
}

// CreateMCPServerFromConnection creates an MCP server entry for a local
// integration. allowedTools is optional — pass nil or empty for "all tools
// exposed" (legacy behaviour). A populated list scopes the resulting MCP
// server row to that subset, enforced by handleMCPEndpoint on every request.
//
// `name` is the CANONICAL SLUG (e.g. "omnikit-storage"), not the human
// display name. The slug is what shows up everywhere downstream:
//   - Entry name in the instance's config.json
//   - Prefix in the system prompt's [AVAILABLE MCP SERVERS] block
//   - Prefix of tool names registered with core ("omnikit-storage_get_file")
//   - Exact-match key when a sub-thread looks up an MCP by name at spawn
//     time (core/thread.go does string equality there)
//
// The display name (conn.AppName, e.g. "OmniKit Storage") goes into the
// description so the dashboard can still show it, but the canonical name
// is the slug. Mixing them was the bug behind "spawn(mcp=\"omnikit-storage\")
// silently produces a worker with zero tools" — the LLM used the slug (which
// it inferred from tool prefixes) but the config stored the display name, so
// the lookup failed.
func (s *Store) CreateMCPServerFromConnection(userID int64, conn *Connection, toolCount int, allowedTools ...[]string) (int64, error) {
	var allowedJSON string
	if len(allowedTools) > 0 && len(allowedTools[0]) > 0 {
		b, _ := json.Marshal(allowedTools[0])
		allowedJSON = string(b)
	}
	// Pick a unique slug for this MCP row. The user-chosen integration
	// name takes precedence — if they typed "mybusiness-socialcast" on
	// create, sub-threads reference it as `mcp="mybusiness-socialcast"`
	// and tool-name prefixes come from that slug (e.g.
	// `mybusiness-socialcast_post`). We slugify the name rather than
	// accepting it verbatim so the result stays safe for prompts and
	// downstream consumers that treat it as an identifier. Only if the
	// name is empty or slugifies to nothing do we fall back to the raw
	// app slug.
	base := slugify(conn.Name)
	if base == "" {
		base = conn.AppSlug
	}
	mcpName := s.uniqueMCPName(userID, conn.ProjectID, base, conn.ID)
	// Description is what the dashboard renders as the row's headline.
	// Use the user-chosen connection name so two connections of the same
	// app are visually distinguishable (e.g. "SocialCast work" vs
	// "SocialCast personal"). Fall back to the app's display name for
	// legacy callers that didn't set a connection name.
	description := conn.Name
	if description == "" {
		description = conn.AppName
	}
	result, err := s.db.Exec(
		"INSERT INTO mcp_servers (user_id, name, description, status, tool_count, source, connection_id, project_id, allowed_tools) VALUES (?, ?, ?, 'running', ?, 'local', ?, ?, ?)",
		userID, mcpName, description, toolCount, conn.ID, conn.ProjectID, allowedJSON,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// CanonicalMCPNameForConnection returns the canonical MCP server name used
// as the tool-name prefix for a connection. It prefers the default (non-
// scoped) mcp_servers row bound to that connection; if none is found (e.g.
// the default row was deleted), it falls back to the app slug. This is the
// string every dashboard-facing "list tools for connection X" path should
// use to prefix bare tool names so agents can tell two connections for the
// same app apart.
func (s *Store) CanonicalMCPNameForConnection(connID int64) string {
	var name string
	// Prefer the oldest row (= auto-created default) over scoped copies.
	s.db.QueryRow(
		"SELECT name FROM mcp_servers WHERE connection_id = ? ORDER BY id ASC LIMIT 1",
		connID,
	).Scan(&name)
	if name != "" {
		return name
	}
	// Fallback: look up the connection's app slug directly.
	var slug string
	s.db.QueryRow("SELECT app_slug FROM connections WHERE id = ?", connID).Scan(&slug)
	return slug
}

// uniqueMCPName returns a per-project unique MCP server name for the given
// app slug. First connection of the app in the project keeps the bare slug
// (backward-compat with existing scenarios + tool-prefix expectations);
// any subsequent connection gets `${slug}-${connID}`, with a counter
// appended if that's also already taken (can happen when a legacy row was
// renamed by migration to exactly the suffix we'd otherwise generate).
// slugify collapses a human-friendly label into a lowercase identifier
// safe to use as an MCP name. Keeps letters, digits, dot, dash, and
// underscore; everything else (spaces, punctuation, emoji) becomes a
// single dash. Leading / trailing / doubled dashes are trimmed.
//
// Examples:
//
//	"MyBusiness — SocialCast"  → "mybusiness-socialcast"
//	"Acme / Gmail Inbox"        → "acme-gmail-inbox"
//	"  "                        → ""
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	return out
}

func (s *Store) uniqueMCPName(userID int64, projectID, appSlug string, connID int64) string {
	nameTaken := func(candidate string) bool {
		var count int
		s.db.QueryRow(
			"SELECT COUNT(*) FROM mcp_servers WHERE user_id = ? AND project_id = ? AND name = ?",
			userID, projectID, candidate,
		).Scan(&count)
		return count > 0
	}
	if !nameTaken(appSlug) {
		return appSlug
	}
	base := fmt.Sprintf("%s-%d", appSlug, connID)
	if !nameTaken(base) {
		return base
	}
	// Walk a counter until we land on a free name. Bounded to 1000 to
	// avoid an infinite loop if the DB is in an unexpected state.
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !nameTaken(candidate) {
			return candidate
		}
	}
	return base // caller will still fail the insert — better than hanging
}

func (s *Store) DeleteMCPServerByConnection(connID int64) {
	s.db.Exec("DELETE FROM mcp_servers WHERE connection_id = ?", connID)
}

// --- HTTP Executor ---

type ExecuteResult struct {
	Success bool   `json:"success"`
	Status  int    `json:"status"`
	Data    any    `json:"data"`
}

// onCredsRefresh is the optional callback executeIntegrationTool invokes
// when it auto-refreshes an OAuth2 access token. Callers wire it to write
// the new credentials map back to the DB so the refreshed tokens survive
// process restarts. Pass nil to skip persistence (e.g. dry-run / tests).
type onCredsRefresh func(updated map[string]string) error

// executeIntegrationToolWithRefresh wraps executeIntegrationTool with an
// auto-refresh + retry-once loop on HTTP 401. The credentials map is
// mutated in place when a refresh succeeds, and the optional onRefresh
// callback fires so the caller can persist the new tokens.
//
// Refresh fires only when:
//   1. The HTTP response status is 401 (Unauthorized)
//   2. The app has an OAuth2 config (so we know the token endpoint)
//   3. The credentials map contains a refresh_token (or refreshToken)
//
// All other failure modes (network, 4xx other than 401, 5xx) bubble up
// unchanged. Refresh failures are non-fatal — we return the original 401
// so the caller can surface the auth error to the user.
func executeIntegrationToolWithRefresh(
	app *AppTemplate,
	tool *AppToolDef,
	credentials map[string]string,
	input map[string]any,
	onRefresh onCredsRefresh,
) (*ExecuteResult, error) {
	result, err := executeIntegrationTool(app, tool, credentials, input)
	if err != nil {
		return result, err
	}
	if result.Status != 401 {
		return result, nil
	}
	// 401 — try to refresh and retry once.
	if app.Auth.OAuth2 == nil {
		return result, nil
	}
	rt := credentials["refresh_token"]
	if rt == "" {
		rt = credentials["refreshToken"]
	}
	if rt == "" {
		return result, nil
	}
	if err := refreshOAuthAccessToken(app, credentials); err != nil {
		// Refresh failed — surface the original 401 so the caller knows
		// the connection needs manual re-auth. Log so the operator can
		// see why refresh isn't working (likely revoked refresh token,
		// missing client_id/secret, or upstream provider error).
		fmt.Fprintf(os.Stderr, "[oauth-refresh] %s: %v\n", app.Slug, err)
		return result, nil
	}
	// Persist the refreshed tokens before retrying so a crash mid-retry
	// doesn't lose them.
	if onRefresh != nil {
		if err := onRefresh(credentials); err != nil {
			fmt.Fprintf(os.Stderr, "[oauth-refresh] persist failed for %s: %v\n", app.Slug, err)
			// Don't bail — the refreshed token still works in this
			// process, we just lose it on restart. Better than a hard
			// failure on what was a successful refresh.
		}
	}
	// Retry the original call with the refreshed token. executeIntegrationTool
	// reads from the same credentials map so the new token is picked up.
	return executeIntegrationTool(app, tool, credentials, input)
}

// refreshOAuthAccessToken POSTs to the app's OAuth2 token endpoint with
// grant_type=refresh_token and merges the response back into the credentials
// map. Mutates credentials in place. Returns an error if the refresh fails.
//
// Some providers (notably Google) only return a NEW access_token on
// refresh — they do NOT return a new refresh_token. We preserve the
// existing refresh_token in that case. Other providers (Microsoft, some
// Atlassian flows) rotate the refresh_token on every refresh — we accept
// the new one and overwrite. The merge handles both correctly: any field
// present in the response replaces the matching field in credentials.
func refreshOAuthAccessToken(app *AppTemplate, credentials map[string]string) error {
	cfg := app.Auth.OAuth2
	if cfg == nil || cfg.TokenURL == "" {
		return fmt.Errorf("no oauth2 token_url for %s", app.Slug)
	}
	rt := credentials["refresh_token"]
	if rt == "" {
		rt = credentials["refreshToken"]
	}
	if rt == "" {
		return fmt.Errorf("no refresh_token in credentials")
	}
	clientID := credentials["client_id"]
	if clientID == "" {
		clientID = credentials["clientId"]
	}
	clientSecret := credentials["client_secret"]
	if clientSecret == "" {
		clientSecret = credentials["clientSecret"]
	}
	// Fall back to env vars so headless deploys without inline creds
	// (the original env-var-only flow) still get auto-refresh.
	if clientID == "" {
		clientID = oauthEnvClientID(app.Slug)
	}
	if clientSecret == "" {
		clientSecret = oauthEnvClientSecret(app.Slug)
	}
	if clientID == "" {
		return fmt.Errorf("no client_id available for refresh")
	}

	form := neturl.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequest("POST", cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token endpoint http %d: %s", resp.StatusCode, string(body))
	}

	// Accept either JSON or form-encoded responses (matching exchangeOAuthCode).
	out := make(map[string]string)
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || (len(body) > 0 && body[0] == '{') {
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("json decode: %w", err)
		}
		for k, v := range raw {
			out[k] = fmt.Sprint(v)
		}
	} else {
		values, err := neturl.ParseQuery(string(body))
		if err != nil {
			return fmt.Errorf("form decode: %w", err)
		}
		for k := range values {
			out[k] = values.Get(k)
		}
	}
	if out["access_token"] == "" {
		return fmt.Errorf("no access_token in refresh response: %s", string(body))
	}
	// Merge new tokens into credentials. Don't clobber the refresh_token
	// if the response didn't include a new one (Google's behavior).
	for k, v := range out {
		credentials[k] = v
	}
	// Update the camelCase mirrors so resolveTemplate's normalization
	// stays consistent for templates that use {{accessToken}} etc.
	if v := out["access_token"]; v != "" {
		credentials["accessToken"] = v
		credentials["token"] = v
	}
	if v := out["refresh_token"]; v != "" {
		credentials["refreshToken"] = v
	}
	return nil
}

func executeIntegrationTool(app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any) (*ExecuteResult, error) {
	// Coerce input values to match the tool's schema types.
	// LLMs often send scalars where arrays are expected (e.g. account_ids=33 instead of [33]).
	if props, ok := tool.InputSchema["properties"].(map[string]any); ok {
		for k, v := range input {
			propDef, exists := props[k].(map[string]any)
			if !exists {
				continue
			}
			schemaType, _ := propDef["type"].(string)
			if schemaType == "array" {
				if _, isSlice := v.([]any); !isSlice {
					// Scalar value for an array field — wrap it
					input[k] = []any{v}
				}
			}
		}
	}

	// Build URL with path param interpolation
	url := buildURL(app.BaseURL, tool.Path, input)

	// Add auth query params
	url += buildAuthQuery(app.Auth.QueryParams, credentials)

	// Build headers
	headers := buildHeaders(app.Auth.Headers, credentials)
	headers["Accept"] = "application/json"

	// Tool-level query_params: a set of input field names that must be
	// sent in the URL query string regardless of HTTP method. Required
	// for APIs that mix query+body on POST/PUT (Google Sheets'
	// values:append wants valueInputOption in the URL but the ValueRange
	// in the body). The set is built once and consulted for both the
	// body-building path (POST/PUT/PATCH) and the all-params-to-query
	// path (GET/DELETE) below. Empty when the template doesn't declare
	// query_params, in which case the new code path is a complete no-op
	// and behavior is identical to before — which is why this change is
	// safe for the other 261 templates that don't use the field.
	toolQuerySet := make(map[string]bool, len(tool.QueryParams))
	for _, name := range tool.QueryParams {
		toolQuerySet[name] = true
	}
	// Collect tool-declared query params from input. Skip empty-string
	// values so optional fields don't become noisy ?foo= in the URL.
	var toolQueryParts []string
	for _, name := range tool.QueryParams {
		v, ok := input[name]
		if !ok || v == nil {
			continue
		}
		if str, isStr := v.(string); isStr && str == "" {
			continue
		}
		toolQueryParts = append(toolQueryParts, fmt.Sprintf("%s=%v", name, v))
	}
	if len(toolQueryParts) > 0 {
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url += sep + strings.Join(toolQueryParts, "&")
	}

	// Build body for POST/PUT/PATCH
	var bodyReader io.Reader
	if tool.Method != "GET" && tool.Method != "DELETE" {
		// Merge default credential fields into body
		bodyMap := make(map[string]any)
		for _, f := range app.Auth.CredentialFields {
			if val, ok := credentials[f.Name]; ok {
				// Map credential fields to common input names
				if f.Name == "user_key" {
					bodyMap["user"] = val
				}
			}
		}
		// Merge user input (overrides defaults, skip empty values)
		for k, v := range input {
			// Skip path params
			if strings.Contains(tool.Path, "{"+k+"}") {
				continue
			}
			// Skip tool-declared query params — they were peeled out
			// above and added to the URL.
			if toolQuerySet[k] {
				continue
			}
			// Don't override credential defaults with empty values
			if str, ok := v.(string); ok && str == "" {
				continue
			}
			bodyMap[k] = v
		}
		if len(bodyMap) > 0 {
			data, _ := json.Marshal(bodyMap)
			bodyReader = strings.NewReader(string(data))
			headers["Content-Type"] = "application/json"
		}
	} else {
		// GET/DELETE: add remaining params as query string. Skip
		// path params and tool-declared query params (already added
		// above) to avoid emitting them twice.
		var qparts []string
		for k, v := range input {
			if strings.Contains(tool.Path, "{"+k+"}") {
				continue
			}
			if toolQuerySet[k] {
				continue
			}
			qparts = append(qparts, fmt.Sprintf("%s=%v", k, v))
		}
		if len(qparts) > 0 {
			sep := "&"
			if !strings.Contains(url, "?") {
				sep = "?"
			}
			url += sep + strings.Join(qparts, "&")
		}
	}

	req, err := http.NewRequest(tool.Method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10_000_000))

	var data any
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "json") {
		json.Unmarshal(respBody, &data)
	} else {
		data = string(respBody)
	}

	// Apply response_path extraction
	if tool.ResponsePath != nil && data != nil {
		if m, ok := data.(map[string]any); ok {
			data = extractPath(m, *tool.ResponsePath)
		}
	}

	return &ExecuteResult{
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Status:  resp.StatusCode,
		Data:    data,
	}, nil
}

func extractPath(data map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = data
	for _, p := range parts {
		if m, ok := current.(map[string]any); ok {
			current = m[p]
		} else {
			return current
		}
	}
	return current
}

// --- HTTP Handlers ---

// POST /connections
//
// Source dispatch:
//   - source=='local' (default) + auth_type=='oauth2' → startLocalOAuth, return authorize_url
//   - source=='local' otherwise → existing api_key / basic path, return active connection
//   - source=='composio' → InitiateConnection on Composio, return redirect_url and pending row
func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		Source      string            `json:"source"`
		AppSlug     string            `json:"app_slug"`
		Name        string            `json:"name"`
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
		ProjectID   string            `json:"project_id"`
		ProviderID  int64             `json:"provider_id"` // required for source=composio
		// Local OAuth2 only: the user's own OAuth app credentials, collected
		// from the dashboard form on first connect to a given app+project.
		// Folded into the connection's encrypted blob so subsequent connects
		// to the same app skip the form entirely.
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		// Composio-only: which upstream auth mode to configure (OAUTH2, API_KEY, BASIC, ...)
		// and two credential maps — one for auth_config creation and one for
		// the per-connection link (Composio schema distinguishes them).
		ComposioAuthMode    string            `json:"composio_auth_mode"`
		ComposioConfigCreds map[string]string `json:"composio_config_creds"`
		ComposioInitCreds   map[string]string `json:"composio_init_creds"`
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

	// --- Composio (hosted) ---
	if body.Source == "composio" {
		if body.ProviderID == 0 {
			http.Error(w, "provider_id required for composio source", http.StatusBadRequest)
			return
		}
		client, err := s.composioClientFor(userID, body.ProviderID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		endUserID := composioEndUserID(userID, body.ProjectID)
		acct, redirectURL, err := client.InitiateConnection(
			body.AppSlug, body.ComposioAuthMode, endUserID,
			body.ComposioConfigCreds, body.ComposioInitCreds,
		)
		if err != nil {
			http.Error(w, "composio initiate: "+err.Error(), http.StatusBadGateway)
			return
		}
		connName := body.Name
		if connName == "" {
			connName = body.AppSlug
		}
		// Composio's hosted flow is the source of truth for credential
		// collection. Every new connection starts as pending and flips to
		// active only after the user completes the Connect Link on
		// Composio's side. Reconcile runs later in the polling path
		// (handleGetConnection) when we observe the upstream ACTIVE state.
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:     userID,
			AppSlug:    body.AppSlug,
			AppName:    body.AppSlug,
			Name:       connName,
			AuthType:   "composio",
			ProjectID:  body.ProjectID,
			Source:     "composio",
			Status:     "pending",
			ProviderID: body.ProviderID,
			ExternalID: acct.ID,
		})
		if err != nil {
			http.Error(w, "failed to create connection", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"connection":   conn,
			"redirect_url": redirectURL,
		})
		return
	}

	// --- Local catalog ---
	app := s.catalog.Get(body.AppSlug)
	if app == nil {
		http.Error(w, "app not found in catalog", http.StatusNotFound)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if body.AuthType == "" {
		// Auto-pick the most appropriate auth type for this app. Many
		// templates list both "bearer" and "oauth2" — bearer because the
		// access token IS a bearer token, oauth2 because that's how to
		// obtain it. We always prefer oauth2 in that case (and prefer
		// it whenever an oauth2 block exists), otherwise fall back to
		// the first declared type, otherwise default to api_key.
		//
		// Without this preference, Google Sheets and similar apps were
		// silently routed through the non-OAuth path on connect: the
		// server stored an empty credentials blob and marked the row
		// active without ever triggering the OAuth popup.
		switch {
		case app.Auth.OAuth2 != nil && containsString(app.Auth.Types, "oauth2"):
			body.AuthType = "oauth2"
		case len(app.Auth.Types) > 0:
			body.AuthType = app.Auth.Types[0]
		default:
			body.AuthType = "api_key"
		}
	}

	// Enforce (user, project, app, name) uniqueness upfront so the user
	// gets a readable error instead of a raw UNIQUE constraint violation.
	var existingCount int
	s.store.db.QueryRow(
		"SELECT COUNT(*) FROM connections WHERE user_id = ? AND project_id = ? AND app_slug = ? AND name = ?",
		userID, body.ProjectID, body.AppSlug, body.Name,
	).Scan(&existingCount)
	if existingCount > 0 {
		http.Error(w, "a connection for this app with that name already exists in this project — pick a different name", http.StatusConflict)
		return
	}

	// Local OAuth2 — two-phase: start flow, return authorize URL, finish in callback.
	if body.AuthType == "oauth2" {
		conn, authURL, err := s.startLocalOAuth(userID, app, body.Name, body.ProjectID, body.ClientID, body.ClientSecret)
		if err != nil {
			http.Error(w, "oauth start: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"connection":   conn,
			"redirect_url": authURL,
		})
		return
	}

	// Local non-OAuth (api_key, basic, bearer, ...): store creds immediately.
	credsJSON, _ := json.Marshal(body.Credentials)
	encrypted, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID:         userID,
		AppSlug:        body.AppSlug,
		AppName:        app.Name,
		Name:           body.Name,
		AuthType:       body.AuthType,
		EncryptedCreds: encrypted,
		ProjectID:      body.ProjectID,
		Source:         "local",
		Status:         "active",
	})
	if err != nil {
		http.Error(w, "failed to create connection", http.StatusInternalServerError)
		return
	}
	if _, merr := s.store.CreateMCPServerFromConnection(userID, conn, len(app.Tools)); merr != nil {
		log.Printf("[CONN-CREATE] auto-mcp failed for conn %d (%s/%s): %v", conn.ID, conn.AppSlug, conn.Name, merr)
	}
	writeJSON(w, conn)
}

// GET /connections/:id — single connection (used by dashboard to poll pending
// states during OAuth flows).
func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Composio pending connections: poll upstream and flip to active on ACTIVE.
	if conn.Source == "composio" && conn.Status == "pending" && conn.ExternalID != "" {
		if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil {
			if acct, perr := client.GetConnectedAccount(conn.ExternalID); perr == nil {
				switch strings.ToUpper(acct.Status) {
				case "ACTIVE":
					s.store.UpdateConnectionStatus(conn.ID, "active")
					conn.Status = "active"
					// Reconcile the project's aggregate Composio MCP server.
					if rerr := s.reconcileComposioMCPServer(userID, conn.ProviderID, conn.ProjectID); rerr != nil {
						fmt.Fprintf(os.Stderr, "composio reconcile: %v\n", rerr)
					}
				case "FAILED", "EXPIRED":
					s.store.UpdateConnectionStatus(conn.ID, "failed")
					conn.Status = "failed"
				}
			}
		}
	}
	writeJSON(w, conn)
}

// PATCH /connections/:id — rename an existing connection.
// Body: { "name": "..." }. Only the name is editable via this endpoint;
// credential swap goes through the invite flow or the OAuth callback.
func (s *Server) handleRenameConnection(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if conn.Name == name {
		writeJSON(w, conn)
		return
	}
	// Uniqueness: (user, project, app, name) must stay unique — match the
	// guard in handleCreateConnection so rename failures are readable.
	var existing int
	s.store.db.QueryRow(
		"SELECT COUNT(*) FROM connections WHERE user_id = ? AND project_id = ? AND app_slug = ? AND name = ? AND id != ?",
		userID, conn.ProjectID, conn.AppSlug, name, connID,
	).Scan(&existing)
	if existing > 0 {
		http.Error(w, "a connection with that name already exists for this app in this project", http.StatusConflict)
		return
	}
	if _, err := s.store.db.Exec(
		"UPDATE connections SET name = ? WHERE id = ? AND user_id = ?",
		name, connID, userID,
	); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	conn.Name = name
	writeJSON(w, conn)
}

// GET /connections
func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	conns, err := s.store.ListConnections(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if conns == nil {
		conns = []Connection{}
	}

	// Enrich with tool count from catalog
	type ConnectionWithTools struct {
		Connection
		ToolCount int `json:"tool_count"`
	}
	var enriched []ConnectionWithTools
	for _, c := range conns {
		tc := 0
		if app := s.catalog.Get(c.AppSlug); app != nil {
			tc = len(app.Tools)
		}
		enriched = append(enriched, ConnectionWithTools{Connection: c, ToolCount: tc})
	}
	writeJSON(w, enriched)
}

// DELETE /connections/:id
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/connections/")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	// Load the row first so we know the source and can revoke upstream.
	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Cascade delete any subscriptions bound to this connection. When the
	// app template has webhook registration config we also try to
	// unregister each external webhook upstream — best-effort, we don't
	// block the local delete on a 4xx/5xx from a third-party.
	if subs, _ := s.store.ListSubscriptionsByConnection(userID, connID); len(subs) > 0 {
		app := s.catalog.Get(conn.AppSlug)
		for _, sub := range subs {
			if app != nil && app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.DeletePath != "" && sub.ExternalWebhookID != "" {
				s.unregisterUpstreamWebhook(conn, app, sub.ExternalWebhookID)
			}
			s.store.DeleteSubscription(userID, sub.ID)
		}
	}

	switch conn.Source {
	case "composio":
		if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil && conn.ExternalID != "" {
			if rerr := client.RevokeConnection(conn.ExternalID); rerr != nil {
				fmt.Fprintf(os.Stderr, "composio revoke %s: %v\n", conn.ExternalID, rerr)
			}
		}
		s.store.DeleteConnection(userID, connID)
		if rerr := s.reconcileComposioMCPServer(userID, conn.ProviderID, conn.ProjectID); rerr != nil {
			fmt.Fprintf(os.Stderr, "composio reconcile: %v\n", rerr)
		}
	default:
		s.store.DeleteMCPServerByConnection(connID)
		s.store.DeleteConnection(userID, connID)
	}

	writeJSON(w, map[string]string{"status": "deleted"})
}

// GET /connections/:id/tools
func (s *Server) handleConnectionTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/tools")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}

	// Return tools with prefixed names
	type ToolInfo struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Method      string         `json:"method"`
		Path        string         `json:"path"`
		InputSchema map[string]any `json:"input_schema"`
	}
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tools []ToolInfo
	for _, t := range app.Tools {
		tools = append(tools, ToolInfo{
			Name:        prefix + "_" + t.Name,
			Description: fmt.Sprintf("[%s] %s", app.Name, t.Description),
			Method:      t.Method,
			Path:        t.Path,
			InputSchema: t.InputSchema,
		})
	}
	writeJSON(w, tools)
}

// POST /connections/:id/execute
func (s *Server) handleExecuteTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/execute")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}

	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Find the tool. Accept the bare name, the canonical MCP-prefixed form
	// (for this specific connection), or the legacy app-slug-prefixed form
	// so scenarios created before unique MCP names keep working.
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == body.Tool || prefix+"_"+t.Name == body.Tool || conn.AppSlug+"_"+t.Name == body.Tool {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}

	// Decrypt credentials
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		http.Error(w, "decryption failed", http.StatusInternalServerError)
		return
	}
	var credentials map[string]string
	json.Unmarshal([]byte(plain), &credentials)

	// Auto-refresh OAuth tokens on 401 + persist back to DB.
	persist := func(updated map[string]string) error {
		blob, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(s.secret, string(blob))
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionCredentials(connID, enc)
	}
	result, err := executeIntegrationToolWithRefresh(app, tool, credentials, body.Input, persist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, result)
}

// handleCreateScopedMCP creates an additional mcp_servers row over an
// existing connection with a specific tool subset. Lets the dashboard
// give different scopes to different sub-threads (read-only worker,
// full-access main, etc.) without re-authorizing the upstream service.
//
// Body: { name: "google-sheets-readonly", allowed_tools: ["read_range", ...] }
//
// Validation:
//   - name is required and unique within the project
//   - allowed_tools must be non-empty (otherwise the user should just
//     use the default unscoped MCP that gets created automatically)
//   - every tool name must exist on the underlying app template
//
// The new row gets a fresh URL keyed on its mcp_servers.id, so two
// scoped views over the same connection have distinct routing.
func (s *Server) handleCreateScopedMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	// Path: /connections/:id/mcp
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	idStr := strings.TrimSuffix(path, "/mcp")
	connID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid connection ID", http.StatusBadRequest)
		return
	}

	conn, _, err := s.store.GetConnection(userID, connID)
	if err != nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	var body struct {
		Name         string   `json:"name"`
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if len(body.AllowedTools) == 0 {
		http.Error(w, "allowed_tools required — use the default mcp_server if you want all tools", http.StatusBadRequest)
		return
	}

	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		http.Error(w, "app not found in catalog", http.StatusNotFound)
		return
	}

	// Validate every tool name against the app template. Accept bare
	// names, canonical-MCP-prefixed names (for this specific connection),
	// and the legacy app-slug-prefixed form. The agent might emit any of
	// these depending on how it discovered the tool.
	canonPrefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	valid := make(map[string]bool, len(app.Tools)*3)
	for _, t := range app.Tools {
		valid[t.Name] = true
		valid[canonPrefix+"_"+t.Name] = true
		valid[conn.AppSlug+"_"+t.Name] = true
	}
	var bad []string
	for _, name := range body.AllowedTools {
		if !valid[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		http.Error(w, "unknown tool name(s): "+strings.Join(bad, ", "), http.StatusBadRequest)
		return
	}

	// Insert the scoped row.
	row, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID:       userID,
		Name:         body.Name,
		Description:  fmt.Sprintf("Scoped view of %s — %d tools", conn.AppName, len(body.AllowedTools)),
		Source:       "local",
		Transport:    "http",
		ConnectionID: conn.ID,
		ProjectID:    conn.ProjectID,
		AllowedTools: body.AllowedTools,
		ToolCount:    len(app.Tools),
	})
	if err != nil {
		http.Error(w, "create scoped mcp_server: "+err.Error(), http.StatusInternalServerError)
		return
	}

	serverPort := s.port
	if serverPort == "" {
		serverPort = "8080"
	}
	writeJSON(w, map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"connection_id": conn.ID,
		"app_slug":      conn.AppSlug,
		"allowed_tools": body.AllowedTools,
		"url":           fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", serverPort, row.ID),
	})
}
