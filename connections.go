package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// CreateMCPServerFromConnection creates an MCP server entry for a local integration
func (s *Store) CreateMCPServerFromConnection(userID int64, conn *Connection, toolCount int) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO mcp_servers (user_id, name, description, status, tool_count, source, connection_id, project_id) VALUES (?, ?, ?, 'running', ?, 'local', ?, ?)",
		userID, conn.AppName, fmt.Sprintf("Local integration: %s", conn.AppSlug), toolCount, conn.ID, conn.ProjectID,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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
		// GET/DELETE: add remaining params as query string
		var qparts []string
		for k, v := range input {
			if !strings.Contains(tool.Path, "{"+k+"}") {
				qparts = append(qparts, fmt.Sprintf("%s=%v", k, v))
			}
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
		if len(app.Auth.Types) > 0 {
			body.AuthType = app.Auth.Types[0]
		} else {
			body.AuthType = "api_key"
		}
	}

	// Local OAuth2 — two-phase: start flow, return authorize URL, finish in callback.
	if body.AuthType == "oauth2" {
		conn, authURL, err := s.startLocalOAuth(userID, app, body.Name, body.ProjectID)
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
	s.store.CreateMCPServerFromConnection(userID, conn, len(app.Tools))
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

	switch conn.Source {
	case "composio":
		if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil && conn.ExternalID != "" {
			if rerr := client.RevokeConnection(conn.ExternalID); rerr != nil {
				fmt.Fprintf(os.Stderr, "composio revoke %s: %v\n", conn.ExternalID, rerr)
			}
		}
		s.store.DeleteConnection(userID, connID)
		// Reconcile aggregate MCP server (may remove it if this was the last
		// Composio connection in the project).
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
	var tools []ToolInfo
	for _, t := range app.Tools {
		tools = append(tools, ToolInfo{
			Name:        conn.AppSlug + "_" + t.Name,
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

	// Find the tool
	var tool *AppToolDef
	for i, t := range app.Tools {
		if t.Name == body.Tool || conn.AppSlug+"_"+t.Name == body.Tool {
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

	result, err := executeIntegrationTool(app, tool, credentials, body.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, result)
}
