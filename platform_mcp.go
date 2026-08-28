package main

// platform_mcp.go exposes the existing apteva-server management gateway over
// the same Streamable HTTP MCP shape used by normal apps. Helper keeps one
// control-plane MCP on main, while server-validated conversation threads can
// inherit it with a trusted project default.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type platformGatewayExecuteFunc func(context.Context, int64, string, []byte) ([]byte, error)

var projectConversationGatewayTools = map[string]bool{
	"agents_list": true, "agents_get": true, "agents_create": true,
	"agents_update": true, "agents_start": true, "agents_stop": true,
	"agents_send_event": true, "agents_delete": true, "agent_list_activity": true,
	"apps_list": true, "apps_marketplace": true, "apps_install": true,
	"apps_upgrade": true, "apps_uninstall": true,
	"list_connections": true, "list_mcp_servers": true, "list_server_tools": true,
}

func (s *Server) handlePlatformMCP(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Core adds the agent header only for the trusted local app-MCP URL and
	// injects the opaque thread id as a hidden tool argument. Promote that
	// hidden value to a server-owned header before inspecting the request.
	if err := extractCallerThreadFromMCPRequest(r); err != nil {
		http.Error(w, "invalid MCP caller context", http.StatusBadRequest)
		return
	}
	agentID, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Agent")), 10, 64)
	if err != nil || agentID <= 0 {
		http.Error(w, "trusted caller agent required", http.StatusUnauthorized)
		return
	}
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil || agent == nil || agent.Kind != "platform_helper" {
		http.Error(w, "platform helper required", http.StatusForbidden)
		return
	}

	threadID := strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Thread"))
	projectID, err := s.store.AgentThreadProjectForUser(agent.UserID, agent.ID, threadID)
	if err != nil {
		http.Error(w, "resolve trusted project context", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "read MCP request", http.StatusBadRequest)
		return
	}
	if projectID != "" {
		body, err = s.scopeProjectGatewayRequest(body, projectID)
		if err != nil {
			writePlatformGatewayToolError(w, body, err)
			return
		}
	}

	execute := s.platformGatewayExec
	if execute == nil {
		execute = s.executePlatformGatewaySubprocess
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	response, err := execute(ctx, agent.UserID, projectID, body)
	if err != nil {
		http.Error(w, "platform gateway unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response)
}

func (s *Server) scopeProjectGatewayRequest(body []byte, projectID string) ([]byte, error) {
	var rpc map[string]any
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC")
	}
	if method, _ := rpc["method"].(string); method != "tools/call" {
		return body, nil
	}
	params, _ := rpc["params"].(map[string]any)
	name, _ := params["name"].(string)
	if !projectConversationGatewayTools[name] {
		return body, fmt.Errorf("tool %q is not available in a project conversation", name)
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
		params["arguments"] = args
	}
	// The server-owned thread binding always wins over model arguments.
	args["project_id"] = projectID
	delete(args, "registry_url")
	if name == "apps_install" {
		args["global"] = false
	}

	switch name {
	case "agents_get", "agents_update", "agents_start", "agents_stop", "agents_send_event", "agents_delete":
		id, parseErr := parseIntArg(args["id"])
		if parseErr != nil || id <= 0 {
			return body, fmt.Errorf("id must be a positive integer")
		}
		target, getErr := s.store.GetAgentByID(id)
		if getErr != nil || target == nil || target.ProjectID != projectID {
			return body, fmt.Errorf("target agent is not in the trusted project")
		}
	case "apps_upgrade", "apps_uninstall":
		installID, parseErr := parseInstallIDArg(args)
		if parseErr != nil {
			return body, parseErr
		}
		var installProject string
		if scanErr := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&installProject); scanErr != nil || installProject != projectID {
			return body, fmt.Errorf("target app installation is not in the trusted project")
		}
	}
	return json.Marshal(rpc)
}

func writePlatformGatewayToolError(w http.ResponseWriter, request []byte, callErr error) {
	var rpc map[string]any
	_ = json.Unmarshal(request, &rpc)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      rpc["id"],
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "error: " + callErr.Error()}},
			"isError": true,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) executePlatformGatewaySubprocess(ctx context.Context, userID int64, projectID string, request []byte) ([]byte, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, selfPath, "--mcp-gateway", "--user-id="+strconv.FormatInt(userID, 10))
	cmd.Env = append(os.Environ(),
		"DB_PATH="+s.dbPath,
		"DATA_DIR="+s.dataDir,
		"APPS_DIR="+s.appsDir,
		"PORT="+s.port,
		"PROJECT_ID="+projectID,
		"INSTANCE_SECRET="+s.instanceSecret,
		"AGENT_SECRET="+s.instanceSecret,
		"APTEVA_INTERNAL_SERVER_URL=http://127.0.0.1:"+s.port,
	)
	cmd.Stdin = bytes.NewReader(append(append([]byte(nil), request...), '\n'))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return nil, fmt.Errorf("%w: %s", err, detail)
	}
	response := bytes.TrimSpace(stdout.Bytes())
	if len(response) == 0 {
		return nil, fmt.Errorf("empty gateway response")
	}
	return append([]byte(nil), response...), nil
}
