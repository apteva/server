package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// RuntimeManagedMCP is a runtime-owned runner cloned from a project managed
// MCP definition. Its capability and process die with the runtime.
type RuntimeManagedMCP struct {
	SourceID     int64
	Name         string
	Description  string
	Revision     string
	Status       string
	AllowedTools []string
	Token        string
	Config       managedMCPConfig
	Process      *MCPProcess
}

type runtimeManagedMCPSelection struct {
	Record *MCPServerRecord
	Config managedMCPConfig
}

func (s *Server) selectRuntimeManagedMCPs(userID int64, projectID string, ids []int64) ([]runtimeManagedMCPSelection, []int64, error) {
	out := make([]runtimeManagedMCPSelection, 0, len(ids))
	appIDs := []int64{}
	seenIDs := map[int64]bool{}
	seenNames := map[string]bool{}
	seenApps := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		record, err := s.store.GetMCPServerByIDUnscoped(id)
		if err != nil || record == nil || record.Source != managedMCPSource || record.ProjectID != projectID {
			return nil, nil, fmt.Errorf("managed MCP server %d not found in project", id)
		}
		if seenNames[record.Name] {
			return nil, nil, fmt.Errorf("duplicate managed MCP name %q", record.Name)
		}
		seenNames[record.Name] = true
		_, encrypted, err := s.store.GetMCPServer(record.UserID, record.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("load managed MCP %d: %w", id, err)
		}
		cfg, err := s.decryptManagedMCPConfig(encrypted)
		if err != nil {
			return nil, nil, fmt.Errorf("decode managed MCP %d: %w", id, err)
		}
		for _, installID := range cfg.Bindings.Apps {
			if installID <= 0 || seenApps[installID] {
				continue
			}
			var boundProject string
			if err := s.store.db.QueryRow(`SELECT COALESCE(project_id, '') FROM app_installs WHERE id = ?`, installID).Scan(&boundProject); err != nil {
				return nil, nil, fmt.Errorf("managed MCP %q bound app %d is unavailable", record.Name, installID)
			}
			if boundProject != "" && boundProject != projectID {
				return nil, nil, fmt.Errorf("managed MCP %q bound app %d belongs to another project", record.Name, installID)
			}
			seenApps[installID] = true
			appIDs = append(appIDs, installID)
		}
		for _, connectionID := range cfg.Bindings.Integrations {
			conn, _, err := s.store.GetConnection(record.UserID, connectionID)
			if err != nil || conn == nil || (conn.ProjectID != "" && conn.ProjectID != projectID) {
				return nil, nil, fmt.Errorf("managed MCP %q bound integration %d is unavailable", record.Name, connectionID)
			}
		}
		out = append(out, runtimeManagedMCPSelection{Record: record, Config: cfg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.Name < out[j].Record.Name })
	sort.Slice(appIDs, func(i, j int) bool { return appIDs[i] < appIDs[j] })
	_ = userID // project membership was already enforced by runtimeCallerProject.
	return out, appIDs, nil
}

func appendUniqueInt64(dst []int64, values ...int64) []int64 {
	seen := make(map[int64]bool, len(dst)+len(values))
	out := make([]int64, 0, len(dst)+len(values))
	for _, value := range append(dst, values...) {
		if value > 0 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func (s *Server) startRuntimeManagedMCP(runtime *Environment, selected runtimeManagedMCPSelection) error {
	if runtime == nil || selected.Record == nil {
		return errors.New("runtime and managed MCP required")
	}
	runner, err := s.managedMCPRunnerBinary()
	if err != nil {
		return fmt.Errorf("find apteva-mcp-runner: %w", err)
	}
	token := randomRuntimeToken(24)
	record := *selected.Record
	record.Command = runner
	record.Args = "[]"
	record.Transport = "stdio"
	env := map[string]string{}
	for key, value := range selected.Config.Env {
		if !strings.HasPrefix(key, "APTEVA_MCP_") {
			env[key] = value
		}
	}
	env["APTEVA_MCP_WORKSPACE"] = s.managedMCPSourceDir(record.ID)
	env["APTEVA_MCP_GATEWAY_URL"] = fmt.Sprintf(
		"http://127.0.0.1:%s/api/runtime-managed-mcp/%s/%s",
		s.port, url.PathEscape(runtime.ID), url.PathEscape(token),
	)
	env["APTEVA_MCP_TOKEN"] = token
	env["HTTP_PROXY"] = runtime.ProxyURL()
	env["HTTPS_PROXY"] = runtime.ProxyURL()
	env["NO_PROXY"] = "127.0.0.1,localhost"
	proc, err := runtime.managedMCPManager.StartIsolatedWithStderr(&record, env, nil)
	if err != nil {
		return fmt.Errorf("start managed MCP %q: %w", record.Name, err)
	}
	mcp := &RuntimeManagedMCP{
		SourceID: record.ID, Name: record.Name, Description: record.Description,
		Revision: record.UpstreamID, Status: "running",
		AllowedTools: append([]string(nil), record.AllowedTools...),
		Token:        token, Config: selected.Config, Process: proc,
	}
	if err := runtime.AddManagedMCP(mcp); err != nil {
		runtime.managedMCPManager.Stop(record.ID)
		return err
	}
	return nil
}

func (s *Server) handleRuntimeManagedMCPBridge(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) || s.environments == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/mcp/runtime/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	runtimeID, _ := url.PathUnescape(parts[0])
	token, _ := url.PathUnescape(parts[1])
	runtime, ok := s.environments.Get(runtimeID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mcp := runtime.ManagedMCPByToken(token)
	if mcp == nil || mcp.Process == nil {
		http.NotFound(w, r)
		return
	}
	s.serveRuntimeManagedMCP(w, r, mcp)
}

func (s *Server) serveRuntimeManagedMCP(w http.ResponseWriter, r *http.Request, mcp *RuntimeManagedMCP) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req bridgeRPCRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeBridgeRPCError(w, nil, -32700, "parse error")
		return
	}
	switch req.Method {
	case "initialize":
		writeBridgeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcp.Name, "version": "runtime"},
		})
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeBridgeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeBridgeRPCResult(w, req.ID, mcpToolsListResult{Tools: filterMCPTools(mcp.Process.Tools, mcp.AllowedTools)})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &params) != nil || strings.TrimSpace(params.Name) == "" {
			writeBridgeRPCError(w, req.ID, -32602, "invalid tools/call parameters")
			return
		}
		record := &MCPServerRecord{AllowedTools: mcp.AllowedTools}
		if !managedMCPAllowed(record, params.Name) {
			writeBridgeRPCError(w, req.ID, -32601, "tool is not enabled: "+params.Name)
			return
		}
		result, err := mcp.Process.call("tools/call", map[string]any{"name": params.Name, "arguments": params.Arguments})
		if err != nil {
			writeBridgeRPCError(w, req.ID, -32603, err.Error())
			return
		}
		writeBridgeRawRPCResult(w, req.ID, result)
	default:
		writeBridgeRPCError(w, req.ID, -32601, "method not found")
	}
}

// handleRuntimeManagedMCPGateway is the outbound capability surface available
// to a runtime-owned runner. It can only call aliases declared by its source
// definition and resolves those aliases against this runtime.
func (s *Server) handleRuntimeManagedMCPGateway(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) || s.environments == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/runtime-managed-mcp/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 5 || parts[4] != "call" || (parts[2] != "integrations" && parts[2] != "apps") {
		http.NotFound(w, r)
		return
	}
	runtimeID, _ := url.PathUnescape(parts[0])
	token, _ := url.PathUnescape(parts[1])
	runtime, ok := s.environments.Get(runtimeID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mcp := runtime.ManagedMCPByToken(token)
	if mcp == nil {
		http.Error(w, "invalid runtime MCP capability", http.StatusUnauthorized)
		return
	}
	if bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); bearer != token {
		http.Error(w, "invalid runtime MCP capability", http.StatusUnauthorized)
		return
	}
	var body struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body) != nil || strings.TrimSpace(body.Tool) == "" {
		http.Error(w, "tool is required", http.StatusBadRequest)
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	alias := parts[3]
	switch parts[2] {
	case "apps":
		s.callRuntimeManagedMCPApp(w, runtime, mcp, alias, body.Tool, body.Input)
	case "integrations":
		s.callRuntimeManagedMCPIntegration(w, runtime, mcp, alias, body.Tool, body.Input)
	}
}

func (s *Server) callRuntimeManagedMCPApp(w http.ResponseWriter, runtime *Environment, mcp *RuntimeManagedMCP, alias, tool string, input map[string]any) {
	sourceInstallID, ok := mcp.Config.Bindings.Apps[alias]
	if !ok {
		http.Error(w, "app alias is not bound", http.StatusForbidden)
		return
	}
	appName, err := s.lookupAppNameForInstall(sourceInstallID)
	if err != nil {
		http.Error(w, "bound app is unavailable", http.StatusGone)
		return
	}
	results, err := s.ExecuteSeedPlan(runtime, []SeedCall{{App: appName, Tool: tool, Input: input}})
	if err != nil || len(results) == 0 {
		if err == nil {
			err = errors.New("empty app result")
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	data, err := unwrapMCPToolResult(results[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"data": data})
}

func (s *Server) callRuntimeManagedMCPIntegration(w http.ResponseWriter, runtime *Environment, mcp *RuntimeManagedMCP, alias, tool string, input map[string]any) {
	connectionID, ok := mcp.Config.Bindings.Integrations[alias]
	if !ok {
		http.Error(w, "integration alias is not bound", http.StatusForbidden)
		return
	}
	raw, _ := json.Marshal(bridgeRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: mustJSONRaw(map[string]any{"name": tool, "arguments": input}),
	})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/mcp/connection/%d?environment_id=%s", connectionID, url.QueryEscape(runtime.ID)),
		strings.NewReader(string(raw)),
	)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.handleMCPEndpoint(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		http.Error(w, strings.TrimSpace(rec.Body.String()), rec.Code)
		return
	}
	var response bridgeRPCResponse
	if json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Error != nil {
		message := "integration tool failed"
		if response.Error != nil {
			message = response.Error.Message
		}
		http.Error(w, message, http.StatusBadGateway)
		return
	}
	data, err := unwrapMCPToolResult(response.Result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"data": data})
}

func mustJSONRaw(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func publicRuntimeManagedMCP(mcp *RuntimeManagedMCP) sdk.RuntimeManagedMCP {
	status := mcp.Status
	if status == "" {
		status = "running"
	}
	return sdk.RuntimeManagedMCP{
		SourceID: mcp.SourceID, Name: mcp.Name, Description: mcp.Description,
		Revision: mcp.Revision, Status: status, ToolCount: len(filterMCPTools(mcp.Process.Tools, mcp.AllowedTools)),
	}
}

func publicRuntimeManagedMCPs(in []*RuntimeManagedMCP) []sdk.RuntimeManagedMCP {
	out := make([]sdk.RuntimeManagedMCP, 0, len(in))
	for _, mcp := range in {
		out = append(out, publicRuntimeManagedMCP(mcp))
	}
	return out
}

func (s *Server) runtimeManagedMCPURL(runtimeID, token string) string {
	return fmt.Sprintf("http://127.0.0.1:%s/mcp/runtime/%s/%s", s.port, url.PathEscape(runtimeID), url.PathEscape(token))
}

func (s *Server) runtimeCatalogManagedMCPs(projectID string) ([]sdk.RuntimeCatalogManagedMCPServer, error) {
	rows, err := s.store.db.Query(`SELECT id FROM mcp_servers WHERE source = ? AND project_id = ? ORDER BY name`, managedMCPSource, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sdk.RuntimeCatalogManagedMCPServer{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			continue
		}
		record, err := s.store.GetMCPServerByIDUnscoped(id)
		if err != nil || record == nil {
			continue
		}
		out = append(out, sdk.RuntimeCatalogManagedMCPServer{
			ID: record.ID, Name: record.Name, Description: record.Description,
			Status: record.Status, ToolCount: record.ToolCount,
			AllowedTools: append([]string(nil), record.AllowedTools...), Revision: record.UpstreamID,
		})
	}
	return out, rows.Err()
}
