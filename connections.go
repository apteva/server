package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// --- DB Model ---

type Connection struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	AppSlug   string    `json:"app_slug"`
	AppName   string    `json:"app_name"`
	Name      string    `json:"name"`
	AuthType  string    `json:"auth_type"`
	Status    string    `json:"status"`
	ProjectID string    `json:"project_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// --- Store methods ---

func (s *Store) CreateConnection(userID int64, appSlug, appName, name, authType, encryptedCreds, projectID string) (*Connection, error) {
	result, err := s.db.Exec(
		"INSERT INTO connections (user_id, app_slug, app_name, name, auth_type, encrypted_credentials, project_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		userID, appSlug, appName, name, authType, encryptedCreds, projectID,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &Connection{ID: id, UserID: userID, AppSlug: appSlug, AppName: appName, Name: name, AuthType: authType, Status: "active", ProjectID: projectID, CreatedAt: time.Now()}, nil
}

func (s *Store) ListConnections(userID int64, projectID ...string) ([]Connection, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			"SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(project_id,''), created_at FROM connections WHERE user_id = ? AND project_id = ?", userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			"SELECT id, app_slug, app_name, name, auth_type, status, COALESCE(project_id,''), created_at FROM connections WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conns []Connection
	for rows.Next() {
		var c Connection
		var createdAt string
		rows.Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &c.Status, &c.ProjectID, &createdAt)
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
		"SELECT id, app_slug, app_name, name, auth_type, encrypted_credentials, status, COALESCE(project_id,''), created_at FROM connections WHERE id = ? AND user_id = ?",
		connID, userID,
	).Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.AuthType, &encCreds, &c.Status, &c.ProjectID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	c.UserID = userID
	c.CreatedAt, _ = parseTime(createdAt)
	return &c, encCreds, nil
}

func (s *Store) DeleteConnection(userID, connID int64) error {
	_, err := s.db.Exec("DELETE FROM connections WHERE id = ? AND user_id = ?", connID, userID)
	return err
}

// CreateMCPServerFromConnection creates an MCP server entry for a local integration
func (s *Store) CreateMCPServerFromConnection(userID int64, conn *Connection, toolCount int) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO mcp_servers (user_id, name, description, status, tool_count, source, connection_id) VALUES (?, ?, ?, 'running', ?, 'local', ?)",
		userID, conn.AppName, fmt.Sprintf("Local integration: %s", conn.AppSlug), toolCount, conn.ID,
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

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 100_000))

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
func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		AppSlug     string            `json:"app_slug"`
		Name        string            `json:"name"`
		AuthType    string            `json:"auth_type"`
		Credentials map[string]string `json:"credentials"`
		ProjectID   string            `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.AppSlug == "" || body.Name == "" {
		http.Error(w, "app_slug and name required", http.StatusBadRequest)
		return
	}

	app := s.catalog.Get(body.AppSlug)
	if app == nil {
		http.Error(w, "app not found in catalog", http.StatusNotFound)
		return
	}

	if body.AuthType == "" {
		if len(app.Auth.Types) > 0 {
			body.AuthType = app.Auth.Types[0]
		} else {
			body.AuthType = "api_key"
		}
	}

	// Encrypt credentials
	credsJSON, _ := json.Marshal(body.Credentials)
	encrypted, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	conn, err := s.store.CreateConnection(userID, body.AppSlug, app.Name, body.Name, body.AuthType, encrypted, body.ProjectID)
	if err != nil {
		http.Error(w, "failed to create connection", http.StatusInternalServerError)
		return
	}

	// Auto-create MCP server entry
	s.store.CreateMCPServerFromConnection(userID, conn, len(app.Tools))

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

	s.store.DeleteMCPServerByConnection(connID)
	s.store.DeleteConnection(userID, connID)
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
