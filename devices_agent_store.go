package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

const deviceMCPSource = "device"

type DeviceToolGrant struct {
	AgentID   int64    `json:"agent_id"`
	ProjectID string   `json:"project_id"`
	Tools     []string `json:"tools"`
	Revision  string   `json:"-"`
}

type DeviceEventBinding struct {
	ID              string `json:"id"`
	DeviceID        string `json:"device_id"`
	AgentID         int64  `json:"agent_id"`
	ProjectID       string `json:"project_id"`
	Capability      string `json:"capability"`
	Kind            string `json:"kind"`
	ThreadID        string `json:"thread_id"`
	Instruction     string `json:"instruction"`
	Enabled         bool   `json:"enabled"`
	CooldownSeconds int    `json:"cooldown_seconds"`
	MaxAgeSeconds   int    `json:"max_age_seconds"`
}

func (s *Store) migrateDeviceAgents() error {
	_, err := s.dbExec(
		"CREATE UNIQUE INDEX IF NOT EXISTS device_mcp_identity ON mcp_servers(upstream_id) WHERE source='device';" +
			"CREATE TABLE IF NOT EXISTS device_tool_grants (" +
			"device_id TEXT NOT NULL REFERENCES account_devices(id) ON DELETE CASCADE,agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE," +
			"project_id TEXT NOT NULL,tools TEXT NOT NULL,revision TEXT NOT NULL,PRIMARY KEY(device_id,agent_id));" +
			"CREATE TABLE IF NOT EXISTS device_event_bindings (" +
			"id TEXT PRIMARY KEY,device_id TEXT NOT NULL REFERENCES account_devices(id) ON DELETE CASCADE," +
			"agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,project_id TEXT NOT NULL,capability TEXT NOT NULL,kind TEXT NOT NULL," +
			"thread_id TEXT NOT NULL,instruction TEXT NOT NULL,enabled INTEGER NOT NULL,cooldown_seconds INTEGER NOT NULL,max_age_seconds INTEGER NOT NULL,last_queued INTEGER NOT NULL DEFAULT 0);" +
			"CREATE INDEX IF NOT EXISTS device_event_bindings_source ON device_event_bindings(device_id,capability,kind,enabled);" +
			"CREATE TABLE IF NOT EXISTS device_event_receipts (" +
			"device_id TEXT NOT NULL REFERENCES account_devices(id) ON DELETE CASCADE,event_id TEXT NOT NULL,payload_hash TEXT NOT NULL,event_json TEXT NOT NULL,created_at INTEGER NOT NULL," +
			"PRIMARY KEY(device_id,event_id));" +
			"CREATE TABLE IF NOT EXISTS device_event_outbox (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT,device_id TEXT NOT NULL,event_id TEXT NOT NULL,binding_id TEXT NOT NULL," +
			"binding_json TEXT NOT NULL,message TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0," +
			"expires_at INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '',execution_id TEXT NOT NULL DEFAULT ''," +
			"UNIQUE(device_id,event_id,binding_id),FOREIGN KEY(device_id,event_id) REFERENCES device_event_receipts(device_id,event_id) ON DELETE CASCADE);" +
			"CREATE INDEX IF NOT EXISTS device_event_outbox_due ON device_event_outbox(status,next_attempt);" +
			"CREATE INDEX IF NOT EXISTS device_event_outbox_binding ON device_event_outbox(binding_id,id,status)")
	return err
}

func (s *Server) ensureDeviceMCP(d *AccountDevice) (int64, error) {
	var tools []deviceTool
	if err := json.Unmarshal(d.Tools, &tools); err != nil && len(d.Tools) > 0 {
		return 0, err
	}
	status := "offline"
	if _, ok := s.deviceLinks.Load(d.ID); ok {
		status = "running"
	}
	if d.RevokedAt != 0 {
		status = "revoked"
	}
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	_, err := s.store.dbExec("INSERT INTO mcp_servers(user_id,name,command,args,encrypted_env,description,project_id,source,transport,url,upstream_id,tool_count,status) "+
		"VALUES(?,?,'','','',?,'','device','http',?,?,?,?) ON CONFLICT(upstream_id) WHERE source='device' DO UPDATE SET name=excluded.name,description=excluded.description,url=excluded.url,tool_count=excluded.tool_count,status=excluded.status",
		d.UserID, deviceMCPName(d.ID), "Node: "+d.Name, fmt.Sprintf("http://127.0.0.1:%s/mcp/devices/%s", port, d.ID), d.ID, len(tools), status)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.store.dbQueryRow("SELECT id FROM mcp_servers WHERE source='device' AND upstream_id=?", d.ID).Scan(&id)
	return id, err
}

func (s *Server) reconcileDeviceMCPs() error {
	rows, err := s.store.dbQuery("SELECT " + deviceColumns + " FROM account_devices")
	if err != nil {
		return err
	}
	var devices []*AccountDevice
	for rows.Next() {
		d, e := scanAccountDevice(rows)
		if e != nil {
			rows.Close()
			return e
		}
		devices = append(devices, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range devices {
		if _, err = s.ensureDeviceMCP(d); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) canUseDeviceAgent(d *AccountDevice, a *Agent, project string) bool {
	if d == nil || a == nil || d.RevokedAt != 0 || a.ProjectID != project {
		return false
	}
	if _, err := s.store.GetUserByID(d.UserID); err != nil {
		return false
	}
	if project == "" {
		return a.UserID == d.UserID || s.store.GetPlatformRole(d.UserID) == PlatformAdmin
	}
	return s.effectiveRoleOnProject(d.UserID, project).Rank() >= ProjectEditor.Rank()
}

func (s *Store) deviceToolGrant(device string, agent int64) (*DeviceToolGrant, error) {
	g := &DeviceToolGrant{AgentID: agent}
	var tools string
	err := s.dbQueryRow("SELECT project_id,tools,revision FROM device_tool_grants WHERE device_id=? AND agent_id=?", device, agent).Scan(&g.ProjectID, &tools, &g.Revision)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(tools), &g.Tools); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Server) authorizeDeviceToolGrant(device string, agent int64) (*AccountDevice, *Agent, *DeviceToolGrant, error) {
	var uid int64
	if err := s.store.dbQueryRow("SELECT user_id FROM account_devices WHERE id=? AND revoked_at=0", device).Scan(&uid); err != nil {
		return nil, nil, nil, errors.New("device unavailable")
	}
	d, err := s.store.accountDevice(device, uid)
	if err != nil {
		return nil, nil, nil, err
	}
	a, err := s.store.GetAgentByID(agent)
	if err != nil {
		return nil, nil, nil, errors.New("agent unavailable")
	}
	g, err := s.store.deviceToolGrant(device, agent)
	if err != nil || !s.canUseDeviceAgent(d, a, gProject(g)) {
		return nil, nil, nil, errors.New("device tool access is not granted")
	}
	return d, a, g, nil
}
func gProject(g *DeviceToolGrant) string {
	if g == nil {
		return ""
	}
	return g.ProjectID
}

func (s *Server) saveDeviceGrant(d *AccountDevice, a *Agent, tools []string) (*DeviceToolGrant, error) {
	if !s.canUseDeviceAgent(d, a, a.ProjectID) {
		return nil, errors.New("device owner needs editor access to the agent")
	}
	var declared []deviceTool
	if err := json.Unmarshal(d.Tools, &declared); err != nil {
		return nil, errors.New("connect the device to discover its tools")
	}
	known := map[string]bool{}
	for _, t := range declared {
		known[t.Name] = true
	}
	if tools == nil {
		for _, t := range declared {
			tools = append(tools, t.Name)
		}
	}
	if len(tools) == 0 || len(tools) > 128 {
		return nil, errors.New("select at least one device tool")
	}
	seen := map[string]bool{}
	for _, name := range tools {
		if !known[name] || seen[name] {
			return nil, errors.New("unknown or duplicate device tool")
		}
		seen[name] = true
	}
	g := &DeviceToolGrant{AgentID: a.ID, ProjectID: a.ProjectID, Tools: tools, Revision: generateToken(16)}
	raw, _ := json.Marshal(tools)
	_, err := s.store.dbExec("INSERT INTO device_tool_grants(device_id,agent_id,project_id,tools,revision) VALUES(?,?,?,?,?) "+
		"ON CONFLICT(device_id,agent_id) DO UPDATE SET project_id=excluded.project_id,tools=excluded.tools,revision=excluded.revision", d.ID, a.ID, a.ProjectID, string(raw), g.Revision)
	return g, err
}

func (s *Server) deviceMCPSelection(userID int64, a *Agent, record *MCPServerRecord, removing bool) (map[string]any, error) {
	d, err := s.store.accountDevice(record.UpstreamID, record.UserID)
	if err != nil {
		return nil, errors.New("device not found")
	}
	if removing {
		if userID != d.UserID {
			return nil, errors.New("device owner required")
		}
		if _, err = s.store.dbExec("DELETE FROM device_tool_grants WHERE device_id=? AND agent_id=?", d.ID, a.ID); err != nil {
			return nil, err
		}
		s.cancelDeviceAgentInvocations(d.ID, a.ID)
	} else {
		if _, _, _, err = s.authorizeDeviceToolGrant(d.ID, a.ID); err != nil {
			if userID != d.UserID {
				return nil, err
			}
			if _, err = s.saveDeviceGrant(d, a, nil); err != nil {
				return nil, err
			}
		}
	}
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	return map[string]any{"name": record.Name, "transport": "http", "url": fmt.Sprintf("http://127.0.0.1:%s/mcp/devices/%s", port, d.ID)}, nil
}

func (s *Server) handleDeviceAgentRoutes(w http.ResponseWriter, r *http.Request, d *AccountDevice, parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "grants":
		s.handleDeviceGrants(w, r, d, parts[1:])
		return true
	case "event-bindings":
		s.handleDeviceEventBindings(w, r, d, parts[1:])
		return true
	case "event-deliveries":
		if !deviceMethod(w, r, http.MethodGet) {
			return true
		}
		rows, err := s.store.dbQueryContext(r.Context(), "SELECT event_id,binding_id,status,attempts,last_error,execution_id FROM device_event_outbox WHERE device_id=? ORDER BY id DESC LIMIT 50", d.ID)
		if err != nil {
			http.Error(w, "delivery history unavailable", 503)
			return true
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var event, binding, status, problem, execution string
			var attempts int
			if rows.Scan(&event, &binding, &status, &attempts, &problem, &execution) != nil {
				http.Error(w, "delivery history unavailable", 503)
				return true
			}
			out = append(out, map[string]any{"event_id": event, "binding_id": binding, "status": status, "attempts": attempts, "error": problem, "execution_id": execution})
		}
		writeJSON(w, map[string]any{"deliveries": out})
		return true
	}
	return false
}

func (s *Server) handleDeviceGrants(w http.ResponseWriter, r *http.Request, d *AccountDevice, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		rows, err := s.store.dbQueryContext(r.Context(), "SELECT agent_id,project_id,tools FROM device_tool_grants WHERE device_id=?", d.ID)
		if err != nil {
			http.Error(w, "grants unavailable", 503)
			return
		}
		defer rows.Close()
		out := []DeviceToolGrant{}
		for rows.Next() {
			var g DeviceToolGrant
			var raw string
			if rows.Scan(&g.AgentID, &g.ProjectID, &raw) != nil {
				http.Error(w, "grants unavailable", 503)
				return
			}
			_ = json.Unmarshal([]byte(raw), &g.Tools)
			out = append(out, g)
		}
		writeJSON(w, map[string]any{"grants": out})
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	aid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid agent", 400)
		return
	}
	// Owning the hardware is sufficient to revoke its grants even after
	// losing access to the agent's project. Detachment still uses normal ACLs.
	if r.Method == http.MethodDelete {
		a, lookupErr := s.store.GetAgentByIDContext(r.Context(), aid)

		// Revocation commits even when a stopped/unreachable Core cannot refresh.
		_, err = s.store.dbExecContext(r.Context(), "DELETE FROM device_tool_grants WHERE device_id=? AND agent_id=?", d.ID, aid)
		if err != nil {
			http.Error(w, "grant removal failed", 503)
			return
		}
		s.cancelDeviceAgentInvocations(d.ID, aid)
		detachErr := lookupErr
		if a != nil {
			detachErr = s.attachDeviceMCP(r, d, a, false)
		}
		writeJSON(w, map[string]any{"revoked": true, "detached": detachErr == nil})
		return
	}
	a, ok := s.requireAgentAccess(w, r, aid, ProjectEditor)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Tools []string `json:"tools"`
		}
		if !deviceJSON(w, r, &body) {
			return
		}
		g, err := s.saveDeviceGrant(d, a, body.Tools)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Any pending invocation authorized by the previous grant is cancelled.
		s.cancelDeviceAgentInvocations(d.ID, aid)
		if err = s.attachDeviceMCP(r, d, a, true); err != nil {
			http.Error(w, "grant saved; attachment failed: "+err.Error(), 502)
			return
		}
		writeJSON(w, g)
	default:
		http.Error(w, "GET, PUT or DELETE only", 405)
	}
}

func (s *Server) deviceBinding(id string) (*DeviceEventBinding, error) {
	b := &DeviceEventBinding{}
	err := s.store.dbQueryRow("SELECT id,device_id,agent_id,project_id,capability,kind,thread_id,instruction,enabled,cooldown_seconds,max_age_seconds FROM device_event_bindings WHERE id=?", id).
		Scan(&b.ID, &b.DeviceID, &b.AgentID, &b.ProjectID, &b.Capability, &b.Kind, &b.ThreadID, &b.Instruction, &b.Enabled, &b.CooldownSeconds, &b.MaxAgeSeconds)
	return b, err
}

func (s *Server) handleDeviceEventBindings(w http.ResponseWriter, r *http.Request, d *AccountDevice, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		rows, err := s.store.dbQueryContext(r.Context(), "SELECT id FROM device_event_bindings WHERE device_id=? ORDER BY id", d.ID)
		if err != nil {
			http.Error(w, "subscriptions unavailable", 503)
			return
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if rows.Scan(&id) != nil {
				rows.Close()
				http.Error(w, "subscriptions unavailable", 503)
				return
			}
			ids = append(ids, id)
		}
		rows.Close()
		out := []DeviceEventBinding{}
		for _, id := range ids {
			b, e := s.deviceBinding(id)
			if e != nil {
				http.Error(w, "subscriptions unavailable", 503)
				return
			}
			out = append(out, *b)
		}
		writeJSON(w, map[string]any{"bindings": out})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		b, err := s.deviceBinding(parts[0])
		if err != nil || b.DeviceID != d.ID {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !deviceJSON(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			http.Error(w, "enabled is required", 400)
			return
		}
		if *body.Enabled {
			a, ok := s.requireAgentAccess(w, r, b.AgentID, ProjectEditor)
			if !ok {
				return
			}
			if !s.canUseDeviceAgent(d, a, b.ProjectID) {
				http.Error(w, "device or project access revoked", 403)
				return
			}
		}
		if _, err = s.store.dbExecContext(r.Context(), "UPDATE device_event_bindings SET enabled=? WHERE id=? AND device_id=?", *body.Enabled, b.ID, d.ID); err != nil {
			http.Error(w, "subscription update failed", 503)
			return
		}
		if !*body.Enabled {
			_, _ = s.store.dbExecContext(r.Context(), "UPDATE device_event_outbox SET status='canceled',last_error='subscription disabled' WHERE binding_id=? AND status='pending'", b.ID)
		}
		b.Enabled = *body.Enabled
		writeJSON(w, b)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		result, err := s.store.dbExecContext(r.Context(), "DELETE FROM device_event_bindings WHERE id=? AND device_id=?", parts[0], d.ID)
		if err != nil {
			http.Error(w, "subscription removal failed", 503)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			http.NotFound(w, r)
			return
		}
		_, _ = s.store.dbExecContext(r.Context(), "UPDATE device_event_outbox SET status='canceled',last_error='subscription removed' WHERE binding_id=? AND status='pending'", parts[0])
		writeJSON(w, map[string]bool{"deleted": true})
		return
	}
	if len(parts) != 0 || r.Method != http.MethodPost {
		http.Error(w, "GET, POST, PATCH or DELETE only", 405)
		return
	}
	var b DeviceEventBinding
	if !deviceJSON(w, r, &b) {
		return
	}
	a, ok := s.requireAgentAccess(w, r, b.AgentID, ProjectEditor)
	if !ok {
		return
	}
	if !s.canUseDeviceAgent(d, a, a.ProjectID) {
		http.Error(w, "device or agent unavailable", 403)
		return
	}
	if b.ThreadID == "" {
		b.ThreadID = "main"
	}
	if b.MaxAgeSeconds == 0 {
		b.MaxAgeSeconds = 300
	}
	if b.CooldownSeconds < 0 || b.CooldownSeconds > 86400 || b.MaxAgeSeconds < 1 || b.MaxAgeSeconds > 86400 || len(b.Instruction) > 4000 || len(b.ThreadID) > 256 || !validDeviceThreadID(b.ThreadID) {
		http.Error(w, "invalid subscription options", 400)
		return
	}
	var events []struct {
		Capability string `json:"capability"`
		Kind       string `json:"kind"`
	}
	_ = json.Unmarshal(d.Events, &events)
	known := false
	for _, e := range events {
		known = known || (e.Capability == b.Capability && e.Kind == b.Kind)
	}
	if !known {
		http.Error(w, "event is not declared by this device", 400)
		return
	}
	b.ID = "node_event_" + generateToken(12)
	b.DeviceID = d.ID
	b.ProjectID = a.ProjectID
	_, err := s.store.dbExecContext(r.Context(), "INSERT INTO device_event_bindings(id,device_id,agent_id,project_id,capability,kind,thread_id,instruction,enabled,cooldown_seconds,max_age_seconds) VALUES(?,?,?,?,?,?,?,?,?,?,?)", b.ID, d.ID, a.ID, a.ProjectID, b.Capability, b.Kind, b.ThreadID, b.Instruction, b.Enabled, b.CooldownSeconds, b.MaxAgeSeconds)
	if err != nil {
		http.Error(w, "subscription save failed", 503)
		return
	}
	writeJSON(w, b)
}
