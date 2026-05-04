package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- MCP JSON-RPC types (same as core/mcp.go) ---

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpToolsListResult struct {
	Tools []mcpToolDef `json:"tools"`
}

// --- DB model ---

type MCPServerRecord struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"user_id"`
	Name         string    `json:"name"`
	Command      string    `json:"command"`
	Args         string    `json:"args"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	ToolCount    int       `json:"tool_count"`
	Pid          int       `json:"pid"`
	Source       string    `json:"source"`     // 'custom' | 'local' | 'remote'
	Transport    string    `json:"transport"`  // 'stdio' | 'http'
	URL          string    `json:"url,omitempty"`
	ProviderID   int64     `json:"provider_id,omitempty"`
	ConnectionID int64     `json:"connection_id"`
	ProjectID    string    `json:"project_id,omitempty"`
	// AllowedTools restricts which tools are exposed. nil / empty = all
	// tools from the underlying source. Populated = only these names are
	// returned by tools/list and only these are accepted by tools/call.
	//
	// For source=local we enforce this in mcp_http.go on every request.
	// For source=remote (Composio) we pass the list as `actions` to the
	// hosted MCP create endpoint; Composio then filters on its side.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// UpstreamID is the external identifier for source=remote rows — e.g.
	// the Composio MCP server id. We rotate this when the tool filter
	// changes because the upstream create call is not idempotent for
	// action-list updates.
	UpstreamID string    `json:"upstream_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// MCPServerInput carries the full field set for creating any MCP server row.
type MCPServerInput struct {
	UserID       int64
	Name         string
	Description  string
	Source       string // '' → 'custom'
	Transport    string // '' → 'stdio'
	Command      string
	Args         string // JSON array
	EncryptedEnv string
	URL          string
	ProviderID   int64
	ProjectID    string
	ConnectionID int64    // FK into connections; 0 if not connection-backed
	AllowedTools []string // nil/empty = all tools exposed; populated = filter
	UpstreamID   string   // external identifier (composio server id, …)
	ToolCount    int      // initial tool_count; local rows trust the DB column
}

// --- Store methods ---

func (s *Store) CreateMCPServer(userID int64, name, command, args, encryptedEnv, description string, projectID ...string) (*MCPServerRecord, error) {
	pid := ""
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	return s.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: name, Description: description,
		Source: "custom", Transport: "stdio",
		Command: command, Args: args, EncryptedEnv: encryptedEnv,
		ProjectID: pid,
	})
}

func (s *Store) CreateMCPServerExt(in MCPServerInput) (*MCPServerRecord, error) {
	if in.Source == "" {
		in.Source = "custom"
	}
	if in.Transport == "" {
		in.Transport = "stdio"
	}
	allowedJSON := ""
	if len(in.AllowedTools) > 0 {
		b, _ := json.Marshal(in.AllowedTools)
		allowedJSON = string(b)
	}
	result, err := s.db.Exec(
		`INSERT INTO mcp_servers (user_id, name, command, args, encrypted_env, description, project_id, source, transport, url, provider_id, connection_id, allowed_tools, upstream_id, tool_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.Name, in.Command, in.Args, in.EncryptedEnv, in.Description, in.ProjectID,
		in.Source, in.Transport, in.URL, in.ProviderID, in.ConnectionID, allowedJSON, in.UpstreamID, in.ToolCount,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &MCPServerRecord{
		ID: id, UserID: in.UserID, Name: in.Name, Command: in.Command, Args: in.Args,
		Description: in.Description, Status: "stopped",
		Source: in.Source, Transport: in.Transport, URL: in.URL, ProviderID: in.ProviderID,
		ConnectionID: in.ConnectionID, ProjectID: in.ProjectID,
		AllowedTools: in.AllowedTools, UpstreamID: in.UpstreamID,
	}, nil
}

func (s *Store) ListMCPServers(userID int64, projectID ...string) ([]MCPServerRecord, error) {
	const cols = `id, name, command, args, description, status, tool_count, pid,
		COALESCE(source,'custom'), COALESCE(transport,'stdio'), COALESCE(url,''), COALESCE(provider_id,0),
		COALESCE(connection_id,0), COALESCE(project_id,''),
		COALESCE(allowed_tools,''), COALESCE(upstream_id,''), created_at`
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(`SELECT `+cols+` FROM mcp_servers WHERE user_id = ? AND project_id = ?`, userID, projectID[0])
	} else {
		rows, err = s.db.Query(`SELECT `+cols+` FROM mcp_servers WHERE user_id = ?`, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []MCPServerRecord
	for rows.Next() {
		var r MCPServerRecord
		var createdAt, allowedJSON string
		rows.Scan(&r.ID, &r.Name, &r.Command, &r.Args, &r.Description, &r.Status, &r.ToolCount, &r.Pid,
			&r.Source, &r.Transport, &r.URL, &r.ProviderID,
			&r.ConnectionID, &r.ProjectID,
			&allowedJSON, &r.UpstreamID, &createdAt)
		r.UserID = userID
		r.CreatedAt, _ = parseTime(createdAt)
		if allowedJSON != "" {
			json.Unmarshal([]byte(allowedJSON), &r.AllowedTools)
		}
		servers = append(servers, r)
	}
	return servers, nil
}

func (s *Store) GetMCPServer(userID, serverID int64) (*MCPServerRecord, string, error) {
	var r MCPServerRecord
	var encryptedEnv, createdAt, allowedJSON string
	err := s.db.QueryRow(
		`SELECT id, name, command, args, encrypted_env, description, status, tool_count, pid,
			COALESCE(source,'custom'), COALESCE(transport,'stdio'), COALESCE(url,''), COALESCE(provider_id,0),
			COALESCE(connection_id,0), COALESCE(project_id,''),
			COALESCE(allowed_tools,''), COALESCE(upstream_id,''), created_at
		 FROM mcp_servers WHERE id = ? AND user_id = ?`,
		serverID, userID,
	).Scan(&r.ID, &r.Name, &r.Command, &r.Args, &encryptedEnv, &r.Description, &r.Status, &r.ToolCount, &r.Pid,
		&r.Source, &r.Transport, &r.URL, &r.ProviderID,
		&r.ConnectionID, &r.ProjectID,
		&allowedJSON, &r.UpstreamID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	r.UserID = userID
	r.CreatedAt, _ = parseTime(createdAt)
	if allowedJSON != "" {
		json.Unmarshal([]byte(allowedJSON), &r.AllowedTools)
	}
	return &r, encryptedEnv, nil
}

// GetMCPServerByIDUnscoped looks up an mcp_servers row by id WITHOUT a
// user-id check. Used by the localhost MCP HTTP endpoint, which has no
// session cookie — access is gated by knowing the row id (which is only
// emitted to the local core via the gateway). Returns nil + nil when
// the row doesn't exist (so callers can fall back to legacy lookups).
func (s *Store) GetMCPServerByIDUnscoped(serverID int64) (*MCPServerRecord, error) {
	var r MCPServerRecord
	var encryptedEnv, createdAt, allowedJSON string
	err := s.db.QueryRow(
		`SELECT id, user_id, name, command, args, encrypted_env, description, status, tool_count, pid,
			COALESCE(source,'custom'), COALESCE(transport,'stdio'), COALESCE(url,''), COALESCE(provider_id,0),
			COALESCE(connection_id,0), COALESCE(project_id,''),
			COALESCE(allowed_tools,''), COALESCE(upstream_id,''), created_at
		 FROM mcp_servers WHERE id = ?`,
		serverID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Command, &r.Args, &encryptedEnv, &r.Description, &r.Status, &r.ToolCount, &r.Pid,
		&r.Source, &r.Transport, &r.URL, &r.ProviderID,
		&r.ConnectionID, &r.ProjectID,
		&allowedJSON, &r.UpstreamID, &createdAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt, _ = parseTime(createdAt)
	if allowedJSON != "" {
		json.Unmarshal([]byte(allowedJSON), &r.AllowedTools)
	}
	return &r, nil
}

// UpdateMCPServerAllowedTools overwrites the allowed_tools filter on an
// existing row. Passing nil or an empty slice clears the filter (all tools
// are exposed again).
func (s *Store) UpdateMCPServerAllowedTools(userID, serverID int64, allowed []string) error {
	allowedJSON := ""
	if len(allowed) > 0 {
		b, _ := json.Marshal(allowed)
		allowedJSON = string(b)
	}
	res, err := s.db.Exec(
		"UPDATE mcp_servers SET allowed_tools = ? WHERE id = ? AND user_id = ?",
		allowedJSON, serverID, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mcp_server %d not found", serverID)
	}
	return nil
}

// UpdateMCPServerUpstreamID sets the external identifier for a remote server.
// Used by the Composio reconciler when a versioned rename rotates the id.
func (s *Store) UpdateMCPServerUpstreamID(serverID int64, upstreamID string) error {
	_, err := s.db.Exec("UPDATE mcp_servers SET upstream_id = ? WHERE id = ?", upstreamID, serverID)
	return err
}

// FindMCPServerByID returns a server without the user_id scope check — used
// internally by the mcp_http proxy when resolving /mcp/<conn_id> to an mcp
// server row by the shared connection.
func (s *Store) FindMCPServerByConnection(connectionID int64) (*MCPServerRecord, error) {
	var r MCPServerRecord
	var createdAt, allowedJSON string
	err := s.db.QueryRow(
		`SELECT id, user_id, name, command, args, description, status, tool_count, pid,
			COALESCE(source,'custom'), COALESCE(transport,'stdio'), COALESCE(url,''), COALESCE(provider_id,0),
			COALESCE(connection_id,0), COALESCE(project_id,''),
			COALESCE(allowed_tools,''), COALESCE(upstream_id,''), created_at
		 FROM mcp_servers WHERE connection_id = ? ORDER BY id DESC LIMIT 1`,
		connectionID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Command, &r.Args, &r.Description, &r.Status, &r.ToolCount, &r.Pid,
		&r.Source, &r.Transport, &r.URL, &r.ProviderID,
		&r.ConnectionID, &r.ProjectID,
		&allowedJSON, &r.UpstreamID, &createdAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt, _ = parseTime(createdAt)
	if allowedJSON != "" {
		json.Unmarshal([]byte(allowedJSON), &r.AllowedTools)
	}
	return &r, nil
}

// FindMCPServerByProviderProject returns an existing remote MCP server for a
// given (user, provider, project) tuple, if one exists. Used by the Composio
// reconciler to find the aggregate server for a project.
func (s *Store) FindMCPServerByProviderProject(userID, providerID int64, projectID string) (*MCPServerRecord, error) {
	var r MCPServerRecord
	var createdAt, allowedJSON string
	err := s.db.QueryRow(
		`SELECT id, name, command, args, description, status, tool_count, pid,
			COALESCE(source,'custom'), COALESCE(transport,'stdio'), COALESCE(url,''), COALESCE(provider_id,0),
			COALESCE(connection_id,0), COALESCE(project_id,''),
			COALESCE(allowed_tools,''), COALESCE(upstream_id,''), created_at
		 FROM mcp_servers WHERE user_id = ? AND provider_id = ? AND project_id = ? AND source = 'remote'
		 LIMIT 1`,
		userID, providerID, projectID,
	).Scan(&r.ID, &r.Name, &r.Command, &r.Args, &r.Description, &r.Status, &r.ToolCount, &r.Pid,
		&r.Source, &r.Transport, &r.URL, &r.ProviderID,
		&r.ConnectionID, &r.ProjectID,
		&allowedJSON, &r.UpstreamID, &createdAt)
	if err != nil {
		return nil, err
	}
	r.UserID = userID
	r.CreatedAt, _ = parseTime(createdAt)
	if allowedJSON != "" {
		json.Unmarshal([]byte(allowedJSON), &r.AllowedTools)
	}
	return &r, nil
}

// UpdateMCPServerURL replaces the remote URL on an existing mcp_servers row.
func (s *Store) UpdateMCPServerURL(serverID int64, url string) error {
	_, err := s.db.Exec("UPDATE mcp_servers SET url = ? WHERE id = ?", url, serverID)
	return err
}

func (s *Store) UpdateMCPServerStatus(serverID int64, status string, toolCount, pid int) {
	s.db.Exec("UPDATE mcp_servers SET status=?, tool_count=?, pid=? WHERE id=?", status, toolCount, pid, serverID)
}

func (s *Store) DeleteMCPServer(userID, serverID int64) error {
	_, err := s.db.Exec("DELETE FROM mcp_servers WHERE id = ? AND user_id = ?", serverID, userID)
	return err
}

// --- MCP Process (running MCP server) ---

type MCPProcess struct {
	ServerID int64
	Name     string
	// stdio fields — nil for remote transport
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	// remote fields — empty for stdio transport
	remoteURL string
	// shared
	mu      sync.Mutex
	nextID  atomic.Int64
	pending map[int64]chan jsonRPCResponse
	pendMu  sync.Mutex
	Tools   []mcpToolDef
}

func (p *MCPProcess) isRemote() bool { return p.cmd == nil && p.remoteURL != "" }

func (p *MCPProcess) readLoop() {
	for p.scanner.Scan() {
		line := p.scanner.Text()
		if line == "" {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		p.pendMu.Lock()
		if ch, ok := p.pending[resp.ID]; ok {
			ch <- resp
			delete(p.pending, resp.ID)
		}
		p.pendMu.Unlock()
	}
}

func (p *MCPProcess) call(method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)

	ch := make(chan jsonRPCResponse, 1)
	p.pendMu.Lock()
	p.pending[id] = ch
	p.pendMu.Unlock()

	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}

	p.mu.Lock()
	data, _ := json.Marshal(req)
	_, err := fmt.Fprintf(p.stdin, "%s\n", data)
	p.mu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(30 * time.Second):
		p.pendMu.Lock()
		delete(p.pending, id)
		p.pendMu.Unlock()
		return nil, fmt.Errorf("timeout after 30s")
	}
}

func (p *MCPProcess) ListTools() ([]mcpToolDef, error) {
	result, err := p.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var list mcpToolsListResult
	if err := json.Unmarshal(result, &list); err != nil {
		return nil, err
	}
	return list.Tools, nil
}

func (p *MCPProcess) Close() {
	if p.isRemote() {
		return
	}
	if p.stdin != nil {
		p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
	}
}

// --- MCP Manager (manages running MCP processes) ---

type MCPManager struct {
	mu        sync.RWMutex
	processes map[int64]*MCPProcess // serverID → process
}

func NewMCPManager() *MCPManager {
	return &MCPManager{
		processes: make(map[int64]*MCPProcess),
	}
}

func (m *MCPManager) Start(record *MCPServerRecord, env map[string]string) (*MCPProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, running := m.processes[record.ID]; running {
		return nil, fmt.Errorf("MCP server %d already running", record.ID)
	}

	// Remote HTTP transport: probe the URL with a tools/list and cache tools.
	// There is no subprocess to spawn; cores connect directly to record.URL
	// using the proxy_config emitted by handleListMCPServers.
	if record.Transport == "http" || record.Source == "remote" {
		if record.URL == "" {
			return nil, fmt.Errorf("remote MCP server %d has no URL", record.ID)
		}
		tools, err := probeRemoteMCP(record.URL, env)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", record.URL, err)
		}
		proc := &MCPProcess{
			ServerID:  record.ID,
			Name:      record.Name,
			remoteURL: record.URL,
			pending:   make(map[int64]chan jsonRPCResponse),
			Tools:     tools,
		}
		m.processes[record.ID] = proc
		return proc, nil
	}

	var args []string
	json.Unmarshal([]byte(record.Args), &args)

	cmd := exec.Command(record.Command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", record.Command, err)
	}

	proc := &MCPProcess{
		ServerID: record.ID,
		Name:     record.Name,
		cmd:      cmd,
		stdin:    stdin,
		scanner:  bufio.NewScanner(stdout),
		pending:  make(map[int64]chan jsonRPCResponse),
	}
	proc.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	go proc.readLoop()

	// Initialize MCP protocol
	_, err = proc.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "apteva-server", "version": "1.0.0"},
	})
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// Send initialized notification
	req := jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	data, _ := json.Marshal(req)
	proc.mu.Lock()
	fmt.Fprintf(proc.stdin, "%s\n", data)
	proc.mu.Unlock()

	// Discover tools
	tools, err := proc.ListTools()
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("list tools: %w", err)
	}
	proc.Tools = tools

	m.processes[record.ID] = proc

	// Wait for exit in background
	go func() {
		cmd.Wait()
		m.mu.Lock()
		delete(m.processes, record.ID)
		m.mu.Unlock()
	}()

	return proc, nil
}

func (m *MCPManager) Stop(serverID int64) {
	m.mu.Lock()
	proc, ok := m.processes[serverID]
	delete(m.processes, serverID)
	m.mu.Unlock()
	if ok {
		proc.Close()
	}
}

func (m *MCPManager) IsRunning(serverID int64) bool {
	m.mu.RLock()
	_, ok := m.processes[serverID]
	m.mu.RUnlock()
	return ok
}

func (m *MCPManager) GetTools(serverID int64) []mcpToolDef {
	m.mu.RLock()
	proc, ok := m.processes[serverID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return proc.Tools
}

// processByID returns the running MCPProcess for a server id, if any. Used
// by the tool-call handler to dispatch tools/call against a custom stdio
// subprocess.
func (m *MCPManager) processByID(serverID int64) (*MCPProcess, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	proc, ok := m.processes[serverID]
	return proc, ok
}

// --- HTTP Handlers ---

// POST /mcp-servers
func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		Name        string            `json:"name"`
		Command     string            `json:"command"`
		Args        []string          `json:"args"`
		Env         map[string]string `json:"env"`
		Description string            `json:"description"`
		ProjectID   string            `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.Command == "" {
		http.Error(w, "name and command required", http.StatusBadRequest)
		return
	}

	argsJSON, _ := json.Marshal(body.Args)

	// Encrypt env if provided
	encryptedEnv := ""
	if len(body.Env) > 0 {
		envJSON, _ := json.Marshal(body.Env)
		enc, err := Encrypt(s.secret, string(envJSON))
		if err != nil {
			http.Error(w, "encryption failed", http.StatusInternalServerError)
			return
		}
		encryptedEnv = enc
	}

	record, err := s.store.CreateMCPServer(userID, body.Name, body.Command, string(argsJSON), encryptedEnv, body.Description, body.ProjectID)
	if err != nil {
		http.Error(w, "failed to create", http.StatusInternalServerError)
		return
	}

	writeJSON(w, record)
}

// GET /mcp-servers
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	servers, err := s.store.ListMCPServers(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Update running status
	for i := range servers {
		if servers[i].Source == "local" {
			// Local integration servers are always "running" — no subprocess needed
			servers[i].Status = "running"
		} else if servers[i].Source == "app" {
			// Apps-bridge rows mirror the install state. The bridge is
			// only present while the install is running (registerAppMCP
			// is called at the running flip; unregisterAppMCP at uninstall).
			// So the existence of the row implies running.
			servers[i].Status = "running"
		} else if servers[i].Source == "remote" {
			// Remote MCP endpoints live outside our process; status is
			// "reachable" once we've probed tools/list successfully.
			if s.mcpManager.IsRunning(servers[i].ID) {
				servers[i].Status = "reachable"
				servers[i].ToolCount = len(s.mcpManager.GetTools(servers[i].ID))
			} else if servers[i].Status == "" {
				servers[i].Status = "unprobed"
			}
		} else if s.mcpManager.IsRunning(servers[i].ID) {
			servers[i].Status = "running"
			servers[i].ToolCount = len(s.mcpManager.GetTools(servers[i].ID))
		} else {
			servers[i].Status = "stopped"
		}
	}

	if servers == nil {
		servers = []MCPServerRecord{}
	}

	// Enrich servers with connection config
	selfPath, _ := os.Executable()
	type enrichedServer struct {
		MCPServerRecord
		ProxyConfig *map[string]any `json:"proxy_config,omitempty"`
	}
	var enriched []enrichedServer
	for _, srv := range servers {
		es := enrichedServer{MCPServerRecord: srv}
		if srv.Source == "remote" && srv.URL != "" {
			// Hosted MCP endpoint (Composio, Pipedream, ...). Cores connect
			// directly to the upstream URL — we do not proxy.
			es.ProxyConfig = &map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       srv.URL,
			}
		} else if srv.Source == "app" && srv.URL != "" {
			// Apps-bridge: the URL points at our own /api/apps/<name>/mcp
			// proxy with the install's APTEVA_APP_TOKEN as ?api_key=.
			// Cores connect directly to it — same shape as remote.
			es.ProxyConfig = &map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       srv.URL,
			}
		} else if srv.Source == "local" && srv.ConnectionID > 0 {
			// Streamable HTTP endpoint served by apteva-server itself.
			// URL keyed on the mcp_servers row id (not the connection
			// id) so two scoped views over the same connection get
			// distinct URLs and the dashboard's instance config can
			// attach them independently.
			es.ProxyConfig = &map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", s.port, srv.ID),
			}
		} else if srv.Command != "" {
			// stdio process
			var args []string
			json.Unmarshal([]byte(srv.Args), &args)
			es.ProxyConfig = &map[string]any{
				"name":      srv.Name,
				"transport": "stdio",
				"command":   selfPath,
				"args":      args,
			}
		}
		enriched = append(enriched, es)
	}
	writeJSON(w, enriched)
}

// POST /mcp-servers/:id/start
func (s *Server) handleStartMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/start")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	record, encEnv, err := s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Decrypt env
	env := map[string]string{}
	if encEnv != "" {
		plain, err := Decrypt(s.secret, encEnv)
		if err == nil {
			json.Unmarshal([]byte(plain), &env)
		}
	}

	proc, err := s.mcpManager.Start(record, env)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pid := 0
	if !proc.isRemote() && proc.cmd != nil && proc.cmd.Process != nil {
		pid = proc.cmd.Process.Pid
	}
	status := "running"
	if proc.isRemote() {
		status = "reachable"
	}
	s.store.UpdateMCPServerStatus(record.ID, status, len(proc.Tools), pid)

	writeJSON(w, map[string]any{
		"status":     "running",
		"tool_count": len(proc.Tools),
		"tools":      proc.Tools,
	})
}

// POST /mcp-servers/:id/stop
func (s *Server) handleStopMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/stop")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	// Verify ownership
	_, _, err = s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.mcpManager.Stop(serverID)
	s.store.UpdateMCPServerStatus(serverID, "stopped", 0, 0)

	writeJSON(w, map[string]string{"status": "stopped"})
}

// GET /mcp-servers/:id/tools
//
// Returns the canonical list of tools the server *can* expose, regardless of
// the current allowed_tools filter. The UI uses this to render the picker:
// every tool is shown as a checkbox, and the checkboxes that match the
// server row's allowed_tools come back pre-ticked.
//
// Response shape: {"tools": [...], "allowed_tools": [...]}. The tools array
// is the full catalog; allowed_tools is the currently-persisted filter (may
// be empty = all tools enabled).
func (s *Server) handleMCPServerTools(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/tools")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	// Verify ownership + get record
	record, encEnv, err := s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var tools []mcpToolDef
	if record != nil && record.Source == "local" && record.ConnectionID > 0 {
		conn, _, err := s.store.GetConnection(userID, record.ConnectionID)
		if err == nil {
			if app := s.catalog.Get(conn.AppSlug); app != nil {
				// Prefix with the MCP row's own name rather than the app
				// slug. For the default row these are the same, but for
				// additional connections of the same app (socialcast-2,
				// socialcast-3) this lets the agent distinguish them.
				prefix := record.Name
				if prefix == "" {
					prefix = conn.AppSlug
				}
				for _, t := range app.Tools {
					tools = append(tools, mcpToolDef{
						Name:        prefix + "_" + t.Name,
						Description: fmt.Sprintf("[%s] %s", app.Name, t.Description),
						InputSchema: t.InputSchema,
					})
				}
			}
		}
	}
	// Apps-bridge rows: fetch tools/list directly from the sidecar via
	// our own proxy URL. The URL already carries ?api_key=dev-<id> so
	// the auth middleware accepts it; the proxy injects APTEVA_APP_TOKEN
	// before forwarding to the sidecar's /mcp.
	if len(tools) == 0 && record != nil && record.Source == "app" && record.URL != "" {
		appTools, err := probeRemoteMCP(record.URL, nil)
		if err == nil {
			tools = append(tools, appTools...)
			if len(tools) > 0 && len(tools) != record.ToolCount {
				s.store.UpdateMCPServerStatus(record.ID, "running", len(tools), record.Pid)
			}
		}
	}
	// Composio remote rows: fetch the toolkit action list so the picker has
	// something to render. One row = one toolkit, so we use the row Name as
	// the slug (matches how reconcileComposioMCPServer stores it).
	//
	// Discriminator: ProviderID > 0. Composio rows always carry a provider
	// foreign-key (the Composio account they were minted against). Our
	// kind=remote_mcp rows leave provider_id at zero — without this guard,
	// they fall through to ListToolkitActions(record.Name) and surface
	// Composio's HubSpot/Linear/Notion toolkit catalog instead of the real
	// upstream's tools/list result. Confusing because Composio's HubSpot
	// catalog happens to have ~230 actions, the same order of magnitude as
	// HubSpot's hosted MCP — picker shows the wrong tool names.
	if len(tools) == 0 && record != nil && record.Source == "remote" && record.ProviderID > 0 {
		if client := s.newComposioClient(userID); client != nil {
			if actions, err := client.ListToolkitActions(record.Name); err == nil {
				for _, a := range actions {
					tools = append(tools, mcpToolDef{
						Name:        a.Slug,
						Description: a.Description,
						InputSchema: a.InputParameters,
					})
				}
				// Keep tool_count in sync with the catalog so the header
				// pill ("N/total tools") matches the expanded list's
				// denominator even before the reconcile probe runs again.
				if len(tools) > 0 && len(tools) != record.ToolCount {
					s.store.UpdateMCPServerStatus(record.ID, record.Status, len(tools), record.Pid)
				}
			}
		}
	}

	// Vendor-hosted MCP rows (kind=remote_mcp connections, NOT Composio):
	// re-probe the upstream now to get the live tool list. probeRemoteMCP
	// runs the full session-id + notifications/initialized handshake, so
	// the result reflects the user's OAuth scope. We DON'T rely on
	// MCPManager.GetTools alone because the row may not be "started" yet
	// when the picker first opens (a fresh connection's row sits in
	// status=stopped until something asks for it). Each call here costs
	// one round-trip to the vendor — acceptable for an interactive picker.
	if len(tools) == 0 && record != nil && record.Source == "remote" && record.ProviderID == 0 && record.URL != "" {
		env := map[string]string{}
		if encEnv != "" {
			if plain, derr := Decrypt(s.secret, encEnv); derr == nil {
				_ = json.Unmarshal([]byte(plain), &env)
			}
		}
		if probed, perr := probeRemoteMCP(record.URL, env); perr == nil {
			tools = append(tools, probed...)
			if len(tools) > 0 && len(tools) != record.ToolCount {
				s.store.UpdateMCPServerStatus(record.ID, "reachable", len(tools), record.Pid)
			}
		} else {
			log.Printf("[TOOLS] remote_mcp probe failed server=%d url=%s: %v", record.ID, record.URL, perr)
		}
	}
	// Fall back to MCP manager for process-based servers
	if len(tools) == 0 {
		tools = s.mcpManager.GetTools(serverID)
	}
	if tools == nil {
		tools = []mcpToolDef{}
	}
	writeJSON(w, map[string]any{
		"tools":         tools,
		"allowed_tools": record.AllowedTools,
	})
}

// PUT /mcp-servers/:id/tools
//
// Body: {"allowed_tools": ["tool_a", "tool_b"]} — pass an empty array to
// clear the filter (all tools re-enabled).
//
// For source=local servers the change takes effect immediately on the next
// tools/list / tools/call, since handleMCPEndpoint reads the filter fresh
// per request. For source=remote (Composio) servers, the next reconcile
// rotates the upstream server to a new versioned name so Composio picks up
// the new action set — the dashboard triggers /composio/reconcile after
// writing the filter.
func (s *Server) handleUpdateMCPServerAllowedTools(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/tools")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	var body struct {
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// De-dup + trim — be forgiving about what the client sends.
	seen := map[string]bool{}
	clean := make([]string, 0, len(body.AllowedTools))
	for _, name := range body.AllowedTools {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		clean = append(clean, name)
	}

	if err := s.store.UpdateMCPServerAllowedTools(userID, serverID, clean); err != nil {
		log.Printf("[MCP-TOOLS] user=%d server=%d UpdateMCPServerAllowedTools failed: %v", userID, serverID, err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("[MCP-TOOLS] user=%d server=%d allowed_tools set (%d entries)", userID, serverID, len(clean))

	// For Composio remote rows, rotate the upstream hosted server to
	// match the new action set. Without this, Composio keeps exposing
	// the old tool list even though our local filter has moved on.
	// Best-effort: if reconcile fails we still report success on the
	// local update so the user isn't stuck; next manual reconcile or
	// connection touch will pick it up.
	if rec, _, err := s.store.GetMCPServer(userID, serverID); err == nil && rec != nil &&
		rec.Source == "remote" && rec.ProviderID > 0 {
		log.Printf("[MCP-TOOLS] reconciling composio provider=%d project=%s after tool-filter change",
			rec.ProviderID, rec.ProjectID)
		if rerr := s.reconcileComposioMCPServer(userID, rec.ProviderID, rec.ProjectID); rerr != nil {
			log.Printf("[MCP-TOOLS] composio reconcile after tool change FAILED provider=%d project=%s: %v",
				rec.ProviderID, rec.ProjectID, rerr)
		} else {
			log.Printf("[MCP-TOOLS] composio reconcile ok provider=%d project=%s", rec.ProviderID, rec.ProjectID)
		}
	}

	writeJSON(w, map[string]any{
		"status":        "updated",
		"allowed_tools": clean,
	})
}

// handleListComposioToolkitActions — GET /composio/toolkits/:slug/actions
//
// Returns the action menu for a Composio toolkit so the dashboard tool
// picker has something to render before a connection exists. Uses the
// per-user composio provider credentials; 404 if the user has no composio
// provider configured.
func (s *Server) handleListComposioToolkitActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/composio/toolkits/")
	slug := strings.TrimSuffix(path, "/actions")
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	client := s.newComposioClient(userID)
	if client == nil {
		http.Error(w, "composio provider not configured", http.StatusNotFound)
		return
	}
	actions, err := client.ListToolkitActions(slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, actions)
}

// POST /mcp-servers/:id/call-tool
//
// Body: {"tool": "<tool name>", "args": {...}}
//
// Dispatches on the server row's source:
//   - remote (Composio, Pipedream, ...) → callRemoteMCPTool against the
//     stored URL, using the row's decrypted env for any auth headers.
//   - custom (stdio subprocess managed by MCPManager) → call through the
//     already-running process's client. We use the same call() helper that
//     probeRemoteMCP internalized, but for stdio we invoke directly via
//     MCPProcess.call("tools/call", ...).
//   - local (Apteva catalog shim) → return 400 and hint to use
//     /connections/:id/execute instead, since catalog tools are executed
//     as HTTP calls, not MCP ones.
//
// The response shape mirrors /connections/:id/execute so the dashboard can
// render both uniformly:
//   {"success": bool, "status": int, "data": any}
func (s *Server) handleCallMCPTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/call-tool")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	record, encEnv, err := s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool name required", http.StatusBadRequest)
		return
	}
	if body.Args == nil {
		body.Args = map[string]any{}
	}

	// Decrypt env so callRemoteMCPTool can inject auth headers if present.
	env := map[string]string{}
	if encEnv != "" {
		plain, derr := Decrypt(s.secret, encEnv)
		if derr == nil {
			_ = json.Unmarshal([]byte(plain), &env)
		}
	}

	switch record.Source {
	case "remote":
		if record.URL == "" {
			http.Error(w, "remote mcp row has no URL", http.StatusInternalServerError)
			return
		}
		result, err := callRemoteMCPTool(record.URL, body.Tool, body.Args, env)
		if err != nil && isRemoteMcpAuthError(err) && record.ConnectionID != 0 {
			// Access token most likely expired (HubSpot OAuth tokens
			// die after 30 min). Refresh against the source connection
			// + retry once before giving up. If the refresh itself
			// fails (no refresh_token, revoked, network), surface the
			// ORIGINAL 401 rather than the refresh error so the user
			// knows to re-auth at the connection level.
			log.Printf("[CALL-TOOL] upstream 401 server=%d conn=%d — attempting auto-refresh", serverID, record.ConnectionID)
			if rerr := s.refreshRemoteMcpAuth(serverID, record.ConnectionID, env); rerr != nil {
				log.Printf("[CALL-TOOL] auto-refresh FAILED server=%d: %v", serverID, rerr)
				writeJSON(w, map[string]any{"success": false, "status": 401, "data": "upstream returned 401 and refresh failed: " + rerr.Error() + " — disconnect + reconnect this integration"})
				return
			}
			log.Printf("[CALL-TOOL] auto-refresh OK server=%d — retrying tools/call", serverID)
			result, err = callRemoteMCPTool(record.URL, body.Tool, body.Args, env)
		}
		if err != nil {
			writeJSON(w, map[string]any{"success": false, "status": 0, "data": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"success": true, "status": 200, "data": json.RawMessage(result)})
	case "app":
		// Apps-bridge: same shape as remote — call the bridge URL via
		// the same callRemoteMCPTool helper. The URL already carries
		// ?api_key=dev-<install_id> so the auth middleware accepts it.
		if record.URL == "" {
			http.Error(w, "app mcp row has no URL — install may have failed", http.StatusInternalServerError)
			return
		}
		result, err := callRemoteMCPTool(record.URL, body.Tool, body.Args, env)
		if err != nil {
			writeJSON(w, map[string]any{"success": false, "status": 0, "data": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"success": true, "status": 200, "data": json.RawMessage(result)})
	case "custom":
		proc, ok := s.mcpManager.processByID(serverID)
		if !ok {
			http.Error(w, "custom MCP server not running — start it first", http.StatusConflict)
			return
		}
		result, err := proc.call("tools/call", map[string]any{
			"name":      body.Tool,
			"arguments": body.Args,
		})
		if err != nil {
			writeJSON(w, map[string]any{"success": false, "status": 0, "data": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"success": true, "status": 200, "data": json.RawMessage(result)})
	case "local":
		http.Error(w, "local catalog tools — use /connections/:id/execute", http.StatusBadRequest)
	default:
		http.Error(w, "unknown source: "+record.Source, http.StatusInternalServerError)
	}
}

// DELETE /mcp-servers/:id
// PATCH /mcp-servers/:id — update the DISPLAY name of an MCP server row.
// Body: {"description": "..."}.
//
// We deliberately only touch `description` here, not `name`. The name is
// the canonical slug used everywhere downstream — instance config entries,
// tool-name prefixes, exact-match lookups when a sub-thread spawns — so
// renaming it would silently break any already-running or saved agent
// that referenced the old slug. The display name (description) is what
// the dashboard shows as the row headline; changing it is cosmetic and
// safe at any time.
func (s *Server) handleRenameMCPServer(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	var body struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	desc := strings.TrimSpace(body.Description)
	if desc == "" {
		http.Error(w, "description required", http.StatusBadRequest)
		return
	}
	record, _, err := s.store.GetMCPServer(userID, serverID)
	if err != nil || record == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if record.Description == desc {
		writeJSON(w, record)
		return
	}
	if _, err := s.store.db.Exec(
		"UPDATE mcp_servers SET description = ? WHERE id = ? AND user_id = ?",
		desc, serverID, userID,
	); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	record.Description = desc
	writeJSON(w, record)
}

func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	s.mcpManager.Stop(serverID)
	s.store.DeleteMCPServer(userID, serverID)
	writeJSON(w, map[string]string{"status": "deleted"})
}

// isRemoteMcpAuthError detects whether an error returned by
// callRemoteMCPTool / probeRemoteMCP looks like an upstream auth
// failure that's worth retrying after an OAuth refresh. The errors
// surface as wrapped strings ("initialize: http 401: ...",
// "tools/call: http 401: ...") because the underlying call helper
// formats them with fmt.Errorf("http %d: %s", ...). We match on the
// 401 status substring rather than wrapping every layer in a typed
// error — keeps the change small and works through the existing
// fmt.Errorf chain.
func isRemoteMcpAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "http 401") || strings.Contains(msg, "HTTP 401")
}

// callRemoteMCPTool invokes a single tool on a Streamable-HTTP MCP server
// (tools/call method) and returns the raw result payload as JSON. It uses
// the same redirect / SSE-parsing path as probeRemoteMCP and is safe for
// repeated invocation.
//
// The returned []byte is the JSON result from the server, which matches the
// MCP spec shape: `{ content: [{ type: "text" | "image", text?, data?, ... }], isError?: bool }`.
// Callers are responsible for surfacing that shape to the dashboard.
func callRemoteMCPTool(rawURL, toolName string, args map[string]any, env map[string]string) (json.RawMessage, error) {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/event-stream",
	}
	if tok, ok := env["AUTHORIZATION"]; ok && tok != "" {
		headers["Authorization"] = tok
	}
	if key, ok := env["API_KEY"]; ok && key != "" {
		headers["X-Api-Key"] = key
	}

	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// MCP Streamable HTTP spec: the server's initialize response MAY
	// return a Mcp-Session-Id header that the client MUST echo on every
	// subsequent request. Without this, some servers (HubSpot's hosted
	// MCP being one) reject tools/call with a generic "Unknown tool:
	// invalid_tool_name" error because the call can't be tied back to a
	// session. We capture the header from the initialize response and
	// inject it into headers for the next call.
	var capturedSessionID string

	call := func(id int64, method string, params any, targetURL string) (json.RawMessage, string, error) {
		req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
		body, _ := json.Marshal(req)
		log.Printf("[CALL-RPC] → POST %s method=%s body_len=%d", targetURL, method, len(body))
		currentURL := targetURL
		for attempt := 0; attempt < 4; attempt++ {
			httpReq, err := http.NewRequest("POST", currentURL, strings.NewReader(string(body)))
			if err != nil {
				return nil, "", err
			}
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}
			resp, err := client.Do(httpReq)
			if err != nil {
				return nil, "", err
			}
			if resp.StatusCode == 307 || resp.StatusCode == 308 {
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				if loc == "" {
					return nil, "", fmt.Errorf("redirect with no Location header")
				}
				currentURL = loc
				continue
			}
			// Persist a session id whenever the server returns one. The
			// MCP spec uses the literal header name "Mcp-Session-Id"
			// (case-insensitive in HTTP/1.1 but exact-cased in HTTP/2).
			if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
				capturedSessionID = sid
				headers["Mcp-Session-Id"] = sid
				log.Printf("[CALL-RPC] captured Mcp-Session-Id len=%d", len(sid))
			}
			defer resp.Body.Close()
			respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 10_000_000))
			log.Printf("[CALL-RPC] ← %d ct=%s body_len=%d preview=%s", resp.StatusCode, resp.Header.Get("Content-Type"), len(respBytes), truncateProbeErr(string(respBytes), 300))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, "", fmt.Errorf("http %d: %s", resp.StatusCode, string(respBytes))
			}
			payload, err := decodeMCPResponseBody(resp.Header.Get("Content-Type"), respBytes)
			if err != nil {
				return nil, "", fmt.Errorf("decode: %w (body=%s)", err, truncateProbeErr(string(respBytes), 200))
			}
			var rpc jsonRPCResponse
			if err := json.Unmarshal(payload, &rpc); err != nil {
				return nil, "", fmt.Errorf("parse rpc: %w (payload=%s)", err, truncateProbeErr(string(payload), 200))
			}
			if rpc.Error != nil {
				return nil, "", fmt.Errorf("mcp error %d: %s", rpc.Error.Code, rpc.Error.Message)
			}
			return rpc.Result, currentURL, nil
		}
		return nil, "", fmt.Errorf("too many redirects")
	}

	// Initialize first to land on the post-redirect URL, then issue the tool
	// call on that URL so we don't redirect twice.
	_, resolvedURL, err := call(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "apteva-server", "version": "1.0.0"},
	}, rawURL)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// MCP spec: after initialize, the client MUST send a
	// notifications/initialized notification before any other request.
	// Some servers tolerate skipping it; HubSpot's MCP enforces it and
	// rejects subsequent calls if you don't. Notification = no `id`
	// field, no response expected.
	notifReq := jsonRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"}
	notifBody, _ := json.Marshal(notifReq)
	notifHTTP, _ := http.NewRequest("POST", resolvedURL, strings.NewReader(string(notifBody)))
	for k, v := range headers {
		notifHTTP.Header.Set(k, v)
	}
	if notifResp, nerr := client.Do(notifHTTP); nerr == nil {
		log.Printf("[CALL-RPC] notifications/initialized → %d", notifResp.StatusCode)
		notifResp.Body.Close()
	} else {
		log.Printf("[CALL-RPC] notifications/initialized FAILED: %v", nerr)
	}

	log.Printf("[CALL-RPC] tools/call name=%s args_keys=%d session_id_present=%t", toolName, len(args), capturedSessionID != "")
	result, _, err := call(2, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	}, resolvedURL)
	if err != nil {
		return nil, fmt.Errorf("tools/call: %w", err)
	}
	return result, nil
}

// jsonRPCNotification is an MCP notification (no `id` field — the
// server does not respond to it). Used for the post-initialize
// `notifications/initialized` handshake step that HubSpot's hosted
// MCP requires.
type jsonRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// probeRemoteMCP issues a minimal MCP handshake + tools/list against a
// Streamable-HTTP MCP endpoint. Used when "starting" a remote MCP server — we
// do not run a subprocess, we only verify the endpoint is reachable and cache
// its tool list.
//
// Compatibility notes for real-world MCP servers (observed against Composio):
//   - Some servers return SSE-framed responses (`Content-Type: text/event-stream`
//     with `event: message\ndata: {...}\n\n` bodies) even for POSTs. We parse
//     both plain JSON and SSE frames.
//   - Some servers host the MCP endpoint at a sub-path (`.../v3/mcp/<id>/mcp`)
//     and respond to the parent path with 307 → the `/mcp` suffix. Go's POST
//     redirect behavior strips the body, so we handle the redirect ourselves
//     by retrying against the Location.
//   - Auth: `env["AUTHORIZATION"]` (e.g. `Bearer <token>`) and `env["API_KEY"]`
//     are added as headers. Many hosted MCPs (Composio) embed the auth token
//     in the URL and need no extra headers.
func probeRemoteMCP(rawURL string, env map[string]string) ([]mcpToolDef, error) {
	headers := map[string]string{
		"Content-Type": "application/json",
		// Accept both JSON and SSE — Composio returns SSE for POSTs.
		"Accept": "application/json, text/event-stream",
	}
	if tok, ok := env["AUTHORIZATION"]; ok && tok != "" {
		headers["Authorization"] = tok
	}
	if key, ok := env["API_KEY"]; ok && key != "" {
		headers["X-Api-Key"] = key
	}

	// Disable auto-redirects so we can manually re-issue the POST with the
	// body intact against the Location header.
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	call := func(id int64, method string, params any, targetURL string) (json.RawMessage, string, error) {
		req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
		body, _ := json.Marshal(req)

		// Try original URL, then follow up to 3 307/308 redirects.
		currentURL := targetURL
		for attempt := 0; attempt < 4; attempt++ {
			httpReq, err := http.NewRequest("POST", currentURL, strings.NewReader(string(body)))
			if err != nil {
				return nil, "", err
			}
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}
			resp, err := client.Do(httpReq)
			if err != nil {
				return nil, "", err
			}
			if resp.StatusCode == 307 || resp.StatusCode == 308 {
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				if loc == "" {
					return nil, "", fmt.Errorf("redirect with no Location header")
				}
				currentURL = loc
				continue
			}
			defer resp.Body.Close()
			respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4_000_000))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, "", fmt.Errorf("http %d: %s", resp.StatusCode, string(respBytes))
			}
			payload, err := decodeMCPResponseBody(resp.Header.Get("Content-Type"), respBytes)
			if err != nil {
				return nil, "", fmt.Errorf("decode: %w (body=%s)", err, truncateProbeErr(string(respBytes), 200))
			}
			var rpc jsonRPCResponse
			if err := json.Unmarshal(payload, &rpc); err != nil {
				return nil, "", fmt.Errorf("parse rpc: %w (payload=%s)", err, truncateProbeErr(string(payload), 200))
			}
			if rpc.Error != nil {
				return nil, "", fmt.Errorf("mcp error %d: %s", rpc.Error.Code, rpc.Error.Message)
			}
			return rpc.Result, currentURL, nil
		}
		return nil, "", fmt.Errorf("too many redirects")
	}

	// Run initialize to land on the final URL (following redirects), then
	// issue tools/list against the same URL so we don't re-redirect.
	_, resolvedURL, err := call(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "apteva-server", "version": "1.0.0"},
	}, rawURL)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	result, _, err := call(2, "tools/list", nil, resolvedURL)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var list mcpToolsListResult
	if err := json.Unmarshal(result, &list); err != nil {
		return nil, fmt.Errorf("parse tools: %w", err)
	}
	return list.Tools, nil
}

// decodeMCPResponseBody extracts the JSON-RPC payload from either a plain
// JSON body or an SSE-framed `event: message\ndata: {...}` body. Returns the
// raw JSON bytes suitable for unmarshaling into jsonRPCResponse.
func decodeMCPResponseBody(contentType string, body []byte) ([]byte, error) {
	ct := strings.ToLower(contentType)
	trimmed := strings.TrimSpace(string(body))
	// SSE path — walk lines, look for "data: {…}" and return the last data
	// payload (the final event in the stream for a single JSON-RPC call).
	if strings.Contains(ct, "text/event-stream") || (strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:")) {
		var lastData string
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "data:") {
				lastData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if lastData == "" {
			return nil, fmt.Errorf("SSE body had no data: line")
		}
		return []byte(lastData), nil
	}
	// Plain JSON path
	return body, nil
}

func truncateProbeErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
