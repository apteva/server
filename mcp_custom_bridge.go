package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
)

type bridgeRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type bridgeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type bridgeRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *bridgeRPCError `json:"error,omitempty"`
}

func managedMCPAllowed(record *MCPServerRecord, tool string) bool {
	if len(record.AllowedTools) == 0 {
		return true
	}
	for _, allowed := range record.AllowedTools {
		if allowed == tool {
			return true
		}
	}
	return false
}

func filterMCPTools(tools []mcpToolDef, allowed []string) []mcpToolDef {
	if len(allowed) == 0 {
		return tools
	}
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	out := make([]mcpToolDef, 0, len(tools))
	for _, tool := range tools {
		if set[tool.Name] {
			out = append(out, tool)
		}
	}
	return out
}

// handleCustomMCPBridge is the single agent-facing transport for legacy
// custom stdio servers and managed custom MCP servers. Agents receive an
// ordinary Streamable-HTTP MCP URL and never receive subprocess commands,
// source paths, environment variables, or runtime gateway tokens.
func (s *Server) handleCustomMCPBridge(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) || !s.authorizedInternalMCPRequest(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	idPart := strings.TrimPrefix(r.URL.Path, "/mcp/custom/")
	serverID, err := strconv.ParseInt(strings.Trim(idPart, "/"), 10, 64)
	if err != nil || serverID <= 0 {
		http.Error(w, "invalid MCP server id", http.StatusBadRequest)
		return
	}
	record, err := s.store.GetMCPServerByIDUnscoped(serverID)
	if err != nil || record == nil || (record.Source != "custom" && record.Source != managedMCPSource) {
		http.Error(w, "custom MCP server not found", http.StatusNotFound)
		return
	}
	if record.Status == "stopped" {
		http.Error(w, "custom MCP server is stopped", http.StatusConflict)
		return
	}
	proc, ok := s.mcpManager.processByID(serverID)
	if !ok {
		switch record.Source {
		case managedMCPSource:
			proc, err = s.ensureManagedMCPRunning(record)
		default:
			_, encrypted, getErr := s.store.GetMCPServer(record.UserID, record.ID)
			if getErr != nil {
				err = getErr
				break
			}
			env := map[string]string{}
			if encrypted != "" {
				if plain, decErr := Decrypt(s.secret, encrypted); decErr == nil {
					_ = json.Unmarshal([]byte(plain), &env)
				}
			}
			proc, err = s.mcpManager.Start(record, env)
		}
		if err != nil {
			http.Error(w, "start custom MCP server: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	var req bridgeRPCRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeBridgeRPCError(w, nil, -32700, "parse error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "initialize":
		writeBridgeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": record.Name, "version": "1"},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeBridgeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeBridgeRPCResult(w, req.ID, mcpToolsListResult{
			Tools: filterMCPTools(proc.Tools, record.AllowedTools),
		})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || strings.TrimSpace(params.Name) == "" {
			writeBridgeRPCError(w, req.ID, -32602, "invalid tools/call parameters")
			return
		}
		if !managedMCPAllowed(record, params.Name) {
			writeBridgeRPCError(w, req.ID, -32601, "tool is not enabled: "+params.Name)
			return
		}
		result, err := proc.call("tools/call", map[string]any{
			"name": params.Name, "arguments": params.Arguments,
		})
		if err != nil {
			writeBridgeRPCError(w, req.ID, -32603, err.Error())
			return
		}
		writeBridgeRawRPCResult(w, req.ID, result)
	default:
		writeBridgeRPCError(w, req.ID, -32601, "method not found")
	}
}

func writeBridgeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	writeBridgeRawRPCResult(w, id, raw)
}

func writeBridgeRawRPCResult(w http.ResponseWriter, id, result json.RawMessage) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(w).Encode(bridgeRPCResponse{
		JSONRPC: "2.0", ID: id, Result: result,
	})
}

func writeBridgeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(w).Encode(bridgeRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &bridgeRPCError{Code: code, Message: message},
	})
}

// handleManagedMCPRuntimeGateway is called only by an isolated runner. Its
// HMAC capability is scoped to one mcp_servers row and one immutable revision.
// Aliases are resolved exclusively from that row's encrypted bindings.
func (s *Server) handleManagedMCPRuntimeGateway(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/managed-mcp-runtime/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[3] != "call" || (parts[1] != "integrations" && parts[1] != "apps") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serverID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || serverID <= 0 {
		http.Error(w, "invalid MCP server id", http.StatusBadRequest)
		return
	}
	record, err := s.store.GetMCPServerByIDUnscoped(serverID)
	if err != nil || record == nil || record.Source != managedMCPSource {
		http.Error(w, "managed MCP server not found", http.StatusNotFound)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" || !s.validateManagedMCPToken(record, token) {
		http.Error(w, "invalid managed MCP capability", http.StatusUnauthorized)
		return
	}
	_, encrypted, err := s.store.GetMCPServer(record.UserID, record.ID)
	if err != nil {
		http.Error(w, "managed MCP configuration not found", http.StatusNotFound)
		return
	}
	cfg, err := s.decryptManagedMCPConfig(encrypted)
	if err != nil {
		http.Error(w, "managed MCP configuration is invalid", http.StatusInternalServerError)
		return
	}
	alias := parts[2]
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Tool == "" {
		http.Error(w, "tool is required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	// The project marker is platform-owned. A handler cannot override it in
	// order to make a global app/connection operate on another project.
	if record.ProjectID != "" {
		body.Input["_project_id"] = record.ProjectID
	} else {
		delete(body.Input, "_project_id")
	}
	log.Printf(
		"[MANAGED-MCP] server=%d project=%s capability=%s alias=%s tool=%s",
		record.ID, record.ProjectID, parts[1], alias, body.Tool,
	)
	switch parts[1] {
	case "integrations":
		connectionID, ok := cfg.Bindings.Integrations[alias]
		if !ok {
			http.Error(w, "integration alias is not bound", http.StatusForbidden)
			return
		}
		s.callManagedMCPIntegration(w, record, connectionID, body.Tool, body.Input)
	case "apps":
		installID, ok := cfg.Bindings.Apps[alias]
		if !ok {
			http.Error(w, "app alias is not bound", http.StatusForbidden)
			return
		}
		s.callManagedMCPApp(w, record, installID, body.Tool, body.Input)
	}
}

func (s *Server) callManagedMCPIntegration(w http.ResponseWriter, record *MCPServerRecord, connectionID int64, tool string, input map[string]any) {
	conn, _, err := s.store.GetConnection(record.UserID, connectionID)
	if err != nil || conn == nil {
		http.Error(w, "bound integration no longer exists", http.StatusGone)
		return
	}
	if conn.ProjectID != "" && conn.ProjectID != record.ProjectID {
		http.Error(w, "bound integration belongs to another project", http.StatusForbidden)
		return
	}
	raw, _ := json.Marshal(map[string]any{"tool": tool, "input": input})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/connections/%d/execute", connectionID), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", strconv.FormatInt(record.UserID, 10))
	recorder := httptest.NewRecorder()
	s.handleExecuteTool(recorder, req)
	if recorder.Code < 200 || recorder.Code >= 300 {
		http.Error(w, strings.TrimSpace(recorder.Body.String()), recorder.Code)
		return
	}
	var result ExecuteResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		http.Error(w, "invalid integration response", http.StatusBadGateway)
		return
	}
	if !result.Success {
		status := result.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeJSONStatus(w, status, map[string]any{"error": "integration tool failed", "data": result.Data})
		return
	}
	writeJSON(w, map[string]any{"data": result.Data})
}

func (s *Server) callManagedMCPApp(w http.ResponseWriter, record *MCPServerRecord, installID int64, tool string, input map[string]any) {
	if s.installedApps == nil {
		http.Error(w, "bound app is not running", http.StatusGone)
		return
	}
	app := s.installedApps.Get(installID)
	if app == nil {
		http.Error(w, "bound app is not running", http.StatusGone)
		return
	}
	if app.ProjectID != "" && app.ProjectID != record.ProjectID {
		http.Error(w, "bound app belongs to another project", http.StatusForbidden)
		return
	}
	result, err := callAppMCPTool(strings.TrimRight(app.SidecarURL, "/")+"/mcp", app.Token, tool, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	data, err := unwrapMCPToolResult(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"data": data})
}

func unwrapMCPToolResult(raw json.RawMessage) (any, error) {
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode MCP tool result: %w", err)
	}
	if result.IsError {
		message := "app tool failed"
		if len(result.Content) > 0 && result.Content[0].Text != "" {
			message = result.Content[0].Text
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		var value any
		if err := json.Unmarshal(result.StructuredContent, &value); err == nil {
			return value, nil
		}
	}
	if len(result.Content) == 0 {
		return map[string]any{}, nil
	}
	text := result.Content[0].Text
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value, nil
	}
	return text, nil
}
