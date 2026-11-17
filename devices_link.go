package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type deviceFrame struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}
type deviceTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
	Confirm     string          `json:"confirm,omitempty"`
}
type deviceHello struct {
	Protocol string       `json:"protocol"`
	Name     string       `json:"name"`
	Version  string       `json:"version"`
	Tools    []deviceTool `json:"tools"`
	Events   []struct {
		Capability  string `json:"capability"`
		Kind        string `json:"kind"`
		Description string `json:"description"`
	} `json:"events"`
	Renders []string `json:"renders"`
}
type deviceConnection struct {
	server         *Server
	device         *AccountDevice
	conn           *websocket.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	writeMu        sync.Mutex
	mu             sync.Mutex
	projectID      string
	agentID        string
	hello          deviceHello
	pending        map[string]chan json.RawMessage
	pendingAgents  map[string]int64
	pendingCancels map[string]context.CancelFunc
}

func (c *deviceConnection) send(typ, id string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = c.conn.WriteJSON(deviceFrame{Type: typ, ID: id, Data: raw})
	if err != nil {
		c.cancel()
		_ = c.conn.Close()
	}
	return err
}
func (c *deviceConnection) close(reason string) {
	c.closeOnce.Do(func() {
		c.cancel()
		code := 4001
		if reason == "device revoked" || reason == "device signed out" || reason == "account sessions revoked" || reason == "account removed" {
			code = 4003
		}
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		_ = c.conn.Close()
	})
}
func (s *Server) closeDeviceLink(id, reason string) {
	if value, ok := s.deviceLinks.Load(id); ok {
		value.(*deviceConnection).close(reason)
	}
}
func (s *Server) closeUserDeviceLinks(userID int64, reason string) {
	s.deviceLinks.Range(func(_, value any) bool {
		c := value.(*deviceConnection)
		if userID == 0 || c.device.UserID == userID {
			c.close(reason)
		}
		return true
	})
}
func (c *deviceConnection) authorizedRequest(method, path string) *http.Request {
	r, _ := http.NewRequestWithContext(c.ctx, method, "http://device.internal"+path, nil)
	r.Header.Set("X-User-ID", strconv.FormatInt(c.device.UserID, 10))
	r.Header.Set("X-Apteva-Operator-ID", strconv.FormatInt(c.device.UserID, 10))
	return r
}
func (c *deviceConnection) valid() bool {
	d, err := c.server.store.accountDevice(c.device.ID, c.device.UserID)
	if err != nil || d.RevokedAt != 0 {
		return false
	}
	_, err = c.server.store.GetUserByID(c.device.UserID)
	return err == nil
}
func (c *deviceConnection) sendDirectory() error {
	projects, err := c.server.store.ListProjectsForUser(c.device.UserID)
	if err != nil {
		return err
	}
	if projects == nil {
		projects = []Project{}
	}
	if err = c.send("projects", "", map[string]any{"projects": projects}); err != nil {
		return err
	}
	agents, err := c.server.store.ListVisibleAgents(c.device.UserID)
	if err != nil {
		return err
	}
	projectNames := map[string]string{}
	for _, project := range projects {
		projectNames[project.ID] = project.Name
	}
	out := []map[string]any{}
	for _, a := range agents {
		state := "paused"
		if a.Status == "running" {
			state = "idle"
		}
		out = append(out, map[string]any{"id": strconv.FormatInt(a.ID, 10), "name": a.Name, "state": state, "project_id": a.ProjectID, "project_name": projectNames[a.ProjectID]})
	}
	return c.send("agents", "", map[string]any{"agents": out})
}
func (c *deviceConnection) selectContext(projectID, agentID, id string) error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	r := c.authorizedRequest(http.MethodGet, "/")
	// Capture the same handlers' role failures without exposing their headers.
	w := newDeviceResponse()
	if !c.server.validateDeviceDefaults(w, r, projectID, agentID) {
		return fmt.Errorf("project or agent is not accessible")
	}
	var name, icon string
	if agentID != "" {
		aid, _ := strconv.ParseInt(agentID, 10, 64)
		a, err := c.server.store.GetAgentByID(aid)
		if err != nil {
			return err
		}
		name, icon = a.Name, a.Icon
	}
	result, err := c.server.store.dbExec("UPDATE account_devices SET project_id=?,agent_id=? WHERE id=? AND user_id=? AND revoked_at=0", projectID, agentID, c.device.ID, c.device.UserID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("device revoked")
	}
	c.mu.Lock()
	c.projectID, c.agentID = projectID, agentID
	c.mu.Unlock()
	return c.send("welcome", id, map[string]any{"device_id": c.device.ID, "project_id": projectID, "agent_id": agentID, "agent": name, "icon": icon, "protocol": "1"})
}

func (s *Server) handleDeviceLink(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodGet) {
		return
	}
	d := requestDevice(r.Context())
	if d == nil {
		http.Error(w, "device credential required", http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 10 * time.Second} // default same-origin check; native Nodes send no Origin
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var frame deviceFrame
	var hello deviceHello
	if err = conn.ReadJSON(&frame); err != nil || frame.Type != "hello" || json.Unmarshal(frame.Data, &hello) != nil || !validDeviceHello(hello) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid or unsupported hello"), time.Now().Add(time.Second))
		return
	}
	conn.SetReadLimit(1 << 20) // bounded camera/tool results may exceed the hello limit
	ctx, cancel := context.WithCancel(r.Context())
	c := &deviceConnection{server: s, device: d, conn: conn, ctx: ctx, cancel: cancel, hello: hello, pending: map[string]chan json.RawMessage{}}
	defer c.close("link closed")
	old, loaded := s.deviceLinks.Swap(d.ID, c)
	if loaded {
		old.(*deviceConnection).close("new device connection")
	}
	defer func() {
		if s.deviceLinks.CompareAndDelete(d.ID, c) {
			_, _ = s.store.dbExec("UPDATE mcp_servers SET status='offline' WHERE source='device' AND upstream_id=? AND status!='revoked'", d.ID)
		}
	}()
	if !c.valid() {
		c.close("device revoked")
		return
	}
	tools, _ := json.Marshal(hello.Tools)
	events, _ := json.Marshal(hello.Events)
	_, err = s.store.dbExecContext(r.Context(), "UPDATE account_devices SET version=?,tools=?,events=?,last_seen=? WHERE id=? AND revoked_at=0", hello.Version, string(tools), string(events), time.Now().Unix(), d.ID)
	if err != nil {
		return
	}
	d.Tools = tools
	d.Events = events
	if _, err = s.ensureDeviceMCP(d); err != nil {
		return
	}
	go s.refreshDeviceToolSchemas(d.ID)
	// A successful authenticated hello confirms credential receipt. Until then
	// repeated polling can recover the same encrypted handoff after a lost response.
	if _, err = s.store.dbExecContext(r.Context(), "UPDATE device_pairings SET credential='' WHERE device_id=?", d.ID); err != nil {
		return
	}
	if err = c.selectContext(d.ProjectID, d.AgentID, ""); err != nil {
		if err = c.selectContext("", "", ""); err != nil {
			return
		}
	}
	if err = c.sendDirectory(); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	go c.maintain()
	for {
		var f deviceFrame
		if err = conn.ReadJSON(&f); err != nil {
			return
		}
		if c.ctx.Err() != nil {
			return
		}
		if f.Type != "result" && len(f.Data) > 64<<10 {
			c.fail(f.ID, "frame too large")
			continue
		}
		if len(f.ID) > 128 || !c.valid() {
			c.close("device revoked")
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		c.handle(f)
	}
}

var deviceToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,42}$`)

func validDeviceHello(h deviceHello) bool {
	if h.Protocol != "1" || len(h.Name) > 320 || len(h.Version) > 80 || len(h.Tools) > 128 || len(h.Events) > 128 || len(h.Renders) > 16 {
		return false
	}
	names := map[string]bool{}
	for _, t := range h.Tools {
		var schema map[string]any
		if !deviceToolNamePattern.MatchString(t.Name) || names[t.Name] || len(t.Description) > 4096 || json.Unmarshal(t.InputSchema, &schema) != nil || schema["type"] != "object" {
			return false
		}
		if t.Confirm != "" && t.Confirm != "none" && t.Confirm != "screen" && t.Confirm != "hold" {
			return false
		}
		names[t.Name] = true
	}
	return true
}
func (c *deviceConnection) maintain() {
	ping := time.NewTicker(25 * time.Second)
	refresh := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	defer refresh.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				c.close("heartbeat failed")
				return
			}
		case <-refresh.C:
			if !c.valid() {
				c.close("device revoked")
				return
			}
			c.mu.Lock()
			project, agent := c.projectID, c.agentID
			c.mu.Unlock()
			w := newDeviceResponse()
			if !c.server.validateDeviceDefaults(w, c.authorizedRequest("GET", "/"), project, agent) {
				_ = c.selectContext("", "", "")
			}
			if err := c.sendDirectory(); err != nil {
				c.close("directory unavailable")
				return
			}
			_, _ = c.server.store.dbExec("UPDATE account_devices SET last_seen=? WHERE id=? AND revoked_at=0", time.Now().Unix(), c.device.ID)
		}
	}
}
func (c *deviceConnection) fail(id, code string) {
	_ = c.send("error", id, map[string]string{"error": code})
}
func (c *deviceConnection) handle(f deviceFrame) {
	switch f.Type {
	case "select_agent":
		var selection struct {
			AgentID string `json:"agent_id"`
		}
		if json.Unmarshal(f.Data, &selection) != nil {
			c.fail(f.ID, "invalid agent selection")
			return
		}
		id, err := strconv.ParseInt(selection.AgentID, 10, 64)
		if err != nil {
			c.fail(f.ID, "invalid agent selection")
			return
		}
		w := newDeviceResponse()
		a, ok := c.server.requireAgentAccess(w, c.authorizedRequest("GET", "/"), id, ProjectViewer)
		if !ok {
			c.fail(f.ID, "agent is not accessible")
			return
		}
		if err = c.selectContext(a.ProjectID, selection.AgentID, f.ID); err != nil {
			c.fail(f.ID, err.Error())
		}
	case "select_project":
		var selection struct {
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal(f.Data, &selection) != nil {
			c.fail(f.ID, "invalid project selection")
			return
		}
		if err := c.selectContext(selection.ProjectID, "", f.ID); err != nil {
			c.fail(f.ID, err.Error())
		}
	case "result":
		var result map[string]any
		if json.Unmarshal(f.Data, &result) != nil || result == nil {
			c.fail(f.ID, "invalid tool result")
			return
		}
		c.mu.Lock()
		ch := c.pending[f.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- f.Data:
			default:
			}
		}
	case "app_request":
		c.handleAppRequest(f)
	case "event", "state":
		// Acknowledge receipt with explicit attribution; execution/subscriptions
		// can consume this seam without guessing the active context later.
		c.handleCapabilityEvent(f)
	default:
		c.fail(f.ID, "unsupported frame type")
	}
}
func (s *Server) handleDeviceInvoke(w http.ResponseWriter, r *http.Request, d *AccountDevice) {
	if !deviceMethod(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	if !deviceJSON(w, r, &body) {
		return
	}
	result, err := s.invokeDeviceTool(r.Context(), d, body.Tool, body.Args, 0)
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "offline") {
			status = http.StatusConflict
		}
		if strings.Contains(err.Error(), "arguments") || strings.Contains(err.Error(), "advertised") {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, result)
}

func (c *deviceConnection) handleCapabilityEvent(f deviceFrame) {
	if f.Type == "event" {
		duplicate, err := c.server.persistDeviceEvent(c.device, f)
		if err != nil {
			permanent := strings.Contains(err.Error(), "undeclared") || strings.Contains(err.Error(), "stable ID") || strings.Contains(err.Error(), "identity mismatch") || strings.Contains(err.Error(), "reused")
			_ = c.send("error", f.ID, map[string]any{"error": err.Error(), "permanent": permanent})
			return
		}
		_ = c.send("ack", f.ID, map[string]any{"accepted": true, "duplicate": duplicate})
		return
	}
	var body struct{ Capability string }
	if json.Unmarshal(f.Data, &body) != nil || body.Capability == "" {
		c.fail(f.ID, "invalid capability state")
		return
	}
	var hardware []deviceHardware
	_ = json.Unmarshal(c.device.Hardware, &hardware)
	known := false
	for _, h := range hardware {
		known = known || h.ID == body.Capability
	}
	if !known {
		c.fail(f.ID, "undeclared capability")
		return
	}
	_ = c.send("ack", f.ID, map[string]any{"received": true})
}

// Keep app routing inside the ordinary app proxy. The device's explicit header
// credential is deliberately not forwarded; only the validated operator identity
// and requested project reach the proxy.
func (c *deviceConnection) handleAppRequest(f deviceFrame) {
	if c.server.installedApps == nil {
		_ = c.send("app_response", f.ID, map[string]any{"status": 503, "error": "app framework unavailable"})
		return
	}
	var body struct {
		App       string          `json:"app"`
		Method    string          `json:"method"`
		Path      string          `json:"path"`
		ProjectID *string         `json:"project_id"`
		Query     map[string]any  `json:"query"`
		Body      json.RawMessage `json:"body"`
	}
	if json.Unmarshal(f.Data, &body) != nil {
		c.fail(f.ID, "invalid app request")
		return
	}
	response := func(status int, data json.RawMessage, err string) {
		_ = c.send("app_response", f.ID, map[string]any{"status": status, "body": data, "error": err})
	}
	if body.App == "" || strings.ContainsAny(body.App, "/\\.%?#") || !strings.HasPrefix(body.Path, "/") || strings.HasPrefix(body.Path, "//") || strings.ContainsAny(body.Path, "\\%?#") || strings.Contains(body.Path, "..") {
		response(400, nil, "invalid app path")
		return
	}
	switch body.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		response(400, nil, "unsupported method")
		return
	}
	c.mu.Lock()
	project := c.projectID
	c.mu.Unlock()
	if body.ProjectID != nil {
		project = *body.ProjectID
	}
	if requested, ok := body.Query["project_id"]; ok && fmt.Sprint(requested) != project {
		response(403, nil, "project context mismatch")
		return
	}
	path := "/apps/" + body.App + body.Path
	r := c.authorizedRequest(body.Method, path)
	q := r.URL.Query()
	for key, value := range body.Query {
		if strings.ContainsAny(key, "\r\n") {
			response(400, nil, "invalid query")
			return
		}
		q.Set(key, fmt.Sprint(value))
	}
	if project != "" {
		q.Set("project_id", project)
	}
	r.URL.RawQuery = q.Encode()
	if len(body.Body) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body.Body))
		r.ContentLength = int64(len(body.Body))
		r.Header.Set("Content-Type", "application/json")
	}
	// A bounded synchronous request avoids unlimited goroutines and never
	// permits streaming/upgrades through the JSON response channel.
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	w := newDeviceResponse()
	c.server.handleAppProxy(w, r.WithContext(ctx))
	data := json.RawMessage(w.body.Bytes())
	if !json.Valid(data) {
		response(w.status, nil, strings.TrimSpace(w.body.String()))
		return
	}
	response(w.status, data, "")
}

// deviceResponse bounds JSON responses buffered for a WebSocket frame.
type deviceResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newDeviceResponse() *deviceResponse {
	return &deviceResponse{header: make(http.Header), status: http.StatusOK}
}
func (w *deviceResponse) Header() http.Header    { return w.header }
func (w *deviceResponse) WriteHeader(status int) { w.status = status }
func (w *deviceResponse) Write(p []byte) (int, error) {
	if w.body.Len()+len(p) > 1<<20 {
		return 0, errors.New("device response exceeds 1 MiB")
	}
	return w.body.Write(p)
}
