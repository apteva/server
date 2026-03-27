package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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
	Source       string    `json:"source"`
	ConnectionID int64     `json:"connection_id"`
	ProjectID    string    `json:"project_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// --- Store methods ---

func (s *Store) CreateMCPServer(userID int64, name, command, args, encryptedEnv, description string) (*MCPServerRecord, error) {
	result, err := s.db.Exec(
		"INSERT INTO mcp_servers (user_id, name, command, args, encrypted_env, description) VALUES (?, ?, ?, ?, ?, ?)",
		userID, name, command, args, encryptedEnv, description,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &MCPServerRecord{ID: id, UserID: userID, Name: name, Command: command, Args: args, Description: description, Status: "stopped"}, nil
}

func (s *Store) ListMCPServers(userID int64, projectID ...string) ([]MCPServerRecord, error) {
	var rows *sql.Rows
	var err error
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			"SELECT id, name, command, args, description, status, tool_count, pid, COALESCE(source,'custom'), COALESCE(connection_id,0), COALESCE(project_id,''), created_at FROM mcp_servers WHERE user_id = ? AND project_id = ?", userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			"SELECT id, name, command, args, description, status, tool_count, pid, COALESCE(source,'custom'), COALESCE(connection_id,0), COALESCE(project_id,''), created_at FROM mcp_servers WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []MCPServerRecord
	for rows.Next() {
		var r MCPServerRecord
		var createdAt string
		rows.Scan(&r.ID, &r.Name, &r.Command, &r.Args, &r.Description, &r.Status, &r.ToolCount, &r.Pid, &r.Source, &r.ConnectionID, &r.ProjectID, &createdAt)
		r.UserID = userID
		r.CreatedAt, _ = parseTime(createdAt)
		servers = append(servers, r)
	}
	return servers, nil
}

func (s *Store) GetMCPServer(userID, serverID int64) (*MCPServerRecord, string, error) {
	var r MCPServerRecord
	var encryptedEnv, createdAt string
	err := s.db.QueryRow(
		"SELECT id, name, command, args, encrypted_env, description, status, tool_count, pid, COALESCE(source,'custom'), COALESCE(connection_id,0), created_at FROM mcp_servers WHERE id = ? AND user_id = ?",
		serverID, userID,
	).Scan(&r.ID, &r.Name, &r.Command, &r.Args, &encryptedEnv, &r.Description, &r.Status, &r.ToolCount, &r.Pid, &r.Source, &r.ConnectionID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	r.UserID = userID
	r.CreatedAt, _ = parseTime(createdAt)
	return &r, encryptedEnv, nil
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
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	scanner  *bufio.Scanner
	mu       sync.Mutex
	nextID   atomic.Int64
	pending  map[int64]chan jsonRPCResponse
	pendMu   sync.Mutex
	Tools    []mcpToolDef
}

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
	p.stdin.Close()
	if p.cmd.Process != nil {
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

	record, err := s.store.CreateMCPServer(userID, body.Name, body.Command, string(argsJSON), encryptedEnv, body.Description)
	if err != nil {
		http.Error(w, "failed to create", http.StatusInternalServerError)
		return
	}

	writeJSON(w, record)
}

// GET /mcp-servers
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	servers, err := s.store.ListMCPServers(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Update running status
	for i := range servers {
		if servers[i].Source == "local" {
			// Local integration servers are always "running" — no subprocess needed
			servers[i].Status = "running"
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
		if srv.Source == "local" && srv.ConnectionID > 0 {
			// Streamable HTTP endpoint
			es.ProxyConfig = &map[string]any{
				"name":      srv.Name,
				"transport": "http",
				"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", s.port, srv.ConnectionID),
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

	s.store.UpdateMCPServerStatus(record.ID, "running", len(proc.Tools), proc.cmd.Process.Pid)

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
func (s *Server) handleMCPServerTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idStr := strings.TrimSuffix(path, "/tools")
	serverID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}

	// Verify ownership + get record
	record, _, err := s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var tools []mcpToolDef
	if record != nil && record.Source == "local" && record.ConnectionID > 0 {
		conn, _, err := s.store.GetConnection(userID, record.ConnectionID)
		if err == nil {
			if app := s.catalog.Get(conn.AppSlug); app != nil {
				for _, t := range app.Tools {
					tools = append(tools, mcpToolDef{
						Name:        conn.AppSlug + "_" + t.Name,
						Description: fmt.Sprintf("[%s] %s", app.Name, t.Description),
						InputSchema: t.InputSchema,
					})
				}
			}
		}
	}
	// Fall back to MCP manager for process-based servers
	if len(tools) == 0 {
		tools = s.mcpManager.GetTools(serverID)
	}
	if tools == nil {
		tools = []mcpToolDef{}
	}
	writeJSON(w, tools)
}

// DELETE /mcp-servers/:id
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
