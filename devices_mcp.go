package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Keep the server prefix short enough for Node's longest hardware tool names.
func deviceMCPName(device string) string {
	sum := sha256.Sum256([]byte(device))
	return "node_" + hex.EncodeToString(sum[:8])
}
func deviceMCPPath(device string) string { return "/mcp/devices/" + device }

func (s *Server) deviceMCPToken(device string, agent int64, revision string) string {
	mac := hmac.New(sha256.New, []byte(s.instanceSecret))
	fmt.Fprintf(mac, "node-mcp-v1:%s:%d:%s", device, agent, revision)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) authorizeDeviceMCPConfig(a *Agent, entry map[string]any, raw *url.URL) error {
	device := strings.TrimPrefix(raw.Path, "/mcp/devices/")
	d, _, g, err := s.authorizeDeviceToolGrant(device, a.ID)
	if err != nil {
		return err
	}
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	u := fmt.Sprintf("http://127.0.0.1:%s%s?agent_id=%d&mcp_token=%s", port, deviceMCPPath(d.ID), a.ID, s.deviceMCPToken(d.ID, a.ID, g.Revision))
	schemaHash := sha256.Sum256(d.Tools)
	u += "&schema=" + hex.EncodeToString(schemaHash[:8])
	for k := range entry {
		delete(entry, k)
	}
	entry["name"], entry["transport"], entry["url"] = deviceMCPName(d.ID), "http", u
	return nil
}

func (s *Server) attachDeviceMCP(r *http.Request, d *AccountDevice, a *Agent, attach bool) error {
	id, err := s.ensureDeviceMCP(d)
	if err != nil {
		return err
	}
	action := "remove"
	if attach {
		action = "add"
	}
	body, _ := json.Marshal(map[string]any{"action": action, "mcp_server_ids": []int64{id}})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, fmt.Sprintf("http://local/instances/%d/mcp-servers", a.ID), bytes.NewReader(body))
	req.Header.Set("X-User-ID", strconv.FormatInt(d.UserID, 10))
	req.Header.Set("X-Apteva-Operator-ID", strconv.FormatInt(d.UserID, 10))
	w := newDeviceResponse()
	s.handleAgentMCPServers(w, req)
	if w.status < 200 || w.status >= 300 {
		return fmt.Errorf("agent configuration rejected (%d): %s", w.status, strings.TrimSpace(w.body.String()))
	}
	return nil
}

func (s *Server) handleDeviceMCP(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.NotFound(w, r)
		return
	}
	device := strings.TrimPrefix(r.URL.Path, "/mcp/devices/")
	aid, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	if err != nil {
		http.Error(w, "agent capability required", 403)
		return
	}
	d, _, g, err := s.authorizeDeviceToolGrant(device, aid)
	if err != nil || !hmac.Equal([]byte(r.URL.Query().Get("mcp_token")), []byte(s.deviceMCPToken(device, aid, gRevision(g)))) {
		http.Error(w, "device capability is not authorized", 403)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&rpc) != nil || rpc.JSONRPC != "2.0" {
		http.Error(w, "invalid MCP request", 400)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(rpc.ID) == 0 {
		if rpc.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any) { writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result}) }
	toolError := func(message string) {
		reply(map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": message}}})
	}
	switch rpc.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": d.ID, "version": "1"}})
	case "ping":
		reply(map[string]any{})
	case "tools/list":
		var declared []deviceTool
		_ = json.Unmarshal(d.Tools, &declared)
		tools := []map[string]any{}
		for _, tool := range declared {
			if !deviceGrantAllows(g, tool.Name) {
				continue
			}
			description := tool.Description
			if tool.Confirm != "" && tool.Confirm != "none" {
				description += " Requires confirmation on the device (" + tool.Confirm + ")."
			}
			tools = append(tools, map[string]any{"name": tool.Name, "description": description, "inputSchema": tool.InputSchema, "annotations": map[string]any{"readOnlyHint": tool.ReadOnly, "destructiveHint": !tool.ReadOnly, "idempotentHint": tool.ReadOnly}})
		}
		reply(map[string]any{"tools": tools})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(rpc.Params, &params) != nil {
			toolError("invalid tool arguments")
			return
		}
		if !deviceGrantAllows(g, params.Name) {
			toolError("tool is not granted to this agent")
			return
		}
		result, err := s.invokeDeviceTool(r.Context(), d, params.Name, params.Arguments, aid)
		if err != nil {
			toolError(err.Error())
			return
		}
		var out struct {
			OK     bool            `json:"ok"`
			Denied bool            `json:"denied"`
			Data   json.RawMessage `json:"data"`
			Error  string          `json:"error"`
		}
		if json.Unmarshal(result, &out) != nil {
			toolError("invalid device result")
			return
		}
		text := string(result)
		if out.OK && len(out.Data) > 0 {
			text = string(out.Data)
		}
		content := []map[string]any{{"type": "text", "text": text}}
		if out.OK {
			var photo struct {
				MIME string `json:"mime_type"`
				Data string `json:"data"`
			}
			if json.Unmarshal(out.Data, &photo) == nil && strings.HasPrefix(photo.MIME, "image/") && photo.Data != "" {
				content = []map[string]any{{"type": "image", "mimeType": photo.MIME, "data": photo.Data}}
			}
		}
		reply(map[string]any{"content": content, "isError": !out.OK, "structuredContent": json.RawMessage(result)})
	default:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}
func gRevision(g *DeviceToolGrant) string {
	if g == nil {
		return ""
	}
	return g.Revision
}
func deviceGrantAllows(g *DeviceToolGrant, name string) bool {
	if g != nil {
		for _, t := range g.Tools {
			if t == name {
				return true
			}
		}
	}
	return false
}

func (s *Server) cancelDeviceAgentInvocations(device string, agent int64) {
	if value, ok := s.deviceLinks.Load(device); ok {
		c := value.(*deviceConnection)
		c.mu.Lock()
		defer c.mu.Unlock()
		for id, aid := range c.pendingAgents {
			if aid == agent {
				c.pendingCancels[id]()
			}
		}
	}
}

func (s *Server) invokeDeviceTool(ctx context.Context, d *AccountDevice, name string, args json.RawMessage, agent int64) (json.RawMessage, error) {
	if d.RevokedAt != 0 {
		return nil, errors.New("device revoked")
	}
	value, ok := s.deviceLinks.Load(d.ID)
	if !ok {
		return nil, errors.New("device is offline")
	}
	c := value.(*deviceConnection)
	var selected *deviceTool
	for i := range c.hello.Tools {
		if c.hello.Tools[i].Name == name {
			selected = &c.hello.Tools[i]
			break
		}
	}
	if selected == nil {
		return nil, errors.New("tool is not advertised by this device")
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := validateDeviceToolArguments(selected.InputSchema, args); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id := "call_" + generateToken(12)
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	if len(c.pending) >= 16 {
		c.mu.Unlock()
		return nil, errors.New("device is busy")
	}
	if c.pendingAgents == nil {
		c.pendingAgents = map[string]int64{}
		c.pendingCancels = map[string]context.CancelFunc{}
	}
	c.pending[id] = ch
	c.pendingAgents[id] = agent
	c.pendingCancels[id] = cancel
	c.mu.Unlock()
	completed := false
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingAgents, id)
		delete(c.pendingCancels, id)
		c.mu.Unlock()
		if !completed && c.ctx.Err() == nil {
			_ = c.send("cancel_invoke", id, map[string]string{"reason": "request cancelled or expired"})
		}
	}()
	if !c.valid() {
		return nil, errors.New("device revoked")
	}
	if agent > 0 {
		_, _, grant, err := s.authorizeDeviceToolGrant(d.ID, agent)
		if err != nil || !deviceGrantAllows(grant, name) {
			return nil, errors.New("tool grant revoked")
		}
	}
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	if err := c.send("invoke", id, map[string]any{"tool": name, "args": args}); err != nil {
		return nil, err
	}
	select {
	case result := <-ch:
		if err := callCtx.Err(); err != nil {
			return nil, err
		}
		completed = true
		return result, nil
	case <-callCtx.Done():
		return nil, fmt.Errorf("device invocation cancelled or expired: %w", callCtx.Err())
	case <-c.ctx.Done():
		return nil, errors.New("device disconnected; invocation was not replayed")
	}
}
