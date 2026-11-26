package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// handleAgentMCPServers exposes an additive attachment API over core's
// replace-all mcp_servers config field.
//
// POST /instances/:id/mcp-servers
// {"action":"add"|"remove"|"set","mcp_server_ids":[1,2],"bound_app_install_ids":[3],"bound_connection_ids":[4]}
//
// The actual read/modify/write happens inside handleUpdateConfig while holding
// the per-agent config lock. Rewriting the request here keeps every config
// mutation on the same validation, persistence, hot-refresh, and binding-sync
// path.
func (s *Server) handleAgentMCPServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/mcp-servers")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	var body struct {
		Action        string  `json:"action"`
		MCPServerIDs  []int64 `json:"mcp_server_ids"`
		AppInstallIDs []int64 `json:"bound_app_install_ids"`
		ConnectionIDs []int64 `json:"bound_connection_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Action == "" {
		body.Action = "add"
	}
	switch body.Action {
	case "add", "remove", "set":
	default:
		http.Error(w, "action must be add, remove, or set", http.StatusBadRequest)
		return
	}
	encoded, _ := json.Marshal(map[string]any{
		"_mcp_action":      body.Action,
		"_mcp_server_ids":  body.MCPServerIDs,
		"_app_install_ids": body.AppInstallIDs,
		"_connection_ids":  body.ConnectionIDs,
	})
	r.Method = http.MethodPut
	r.URL.Path = "/instances/" + strconv.FormatInt(instanceID, 10) + "/config"
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	r.Header.Set("Content-Type", "application/json")
	s.handleUpdateConfig(w, r)
}

// connectionMCPServer returns the canonical MCP server that exposes a
// connection's tools. With create, a connection that has none yet gets one,
// as when it is attached to a new agent. A nil record without error means the
// connection has no MCP server and create was false. A non-empty projectID
// refuses a connection that belongs to another project before anything is
// created for it.
func (s *Server) connectionMCPServer(userID, connectionID int64, projectID string, create bool) (*MCPServerRecord, error) {
	conn, _, err := s.store.GetConnection(userID, connectionID)
	if err != nil || conn == nil {
		return nil, fmt.Errorf("connection %d not found", connectionID)
	}
	if projectID != "" && conn.ProjectID != "" && conn.ProjectID != projectID {
		return nil, fmt.Errorf("connection %d belongs to project %q, agent belongs to project %q", connectionID, conn.ProjectID, projectID)
	}
	record, err := s.store.FindCanonicalMCPServerByConnection(conn.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve MCP server for connection %d: %w", connectionID, err)
	}
	if record != nil || !create {
		return record, nil
	}
	toolCount := 0
	if s.catalog != nil {
		if app := s.catalog.Get(conn.AppSlug); app != nil {
			toolCount = len(app.Tools)
		}
	}
	mcpID, err := s.store.CreateMCPServerFromConnection(userID, conn, toolCount)
	if err != nil {
		return nil, fmt.Errorf("create MCP server for connection %d: %w", connectionID, err)
	}
	record, err = s.store.GetMCPServerByIDUnscoped(mcpID)
	if err != nil || record == nil {
		return nil, fmt.Errorf("reload MCP server %d for connection %d: %v", mcpID, connectionID, err)
	}
	return record, nil
}

func (s *Server) lockAgentConfig(agentID int64) func() {
	value, _ := s.agentConfigLocks.LoadOrStore(agentID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// currentAgentMCPServers reads the canonical desired MCP set. A running core
// owns config.json, so read it through Core; a stopped agent is read from disk.
// The DB config is only a brand-new-agent fallback before config.json exists.
func (s *Server) currentAgentMCPServers(inst *Agent, port int) ([]map[string]any, error) {
	var cfg map[string]any
	if port > 0 {
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		resp, err := s.coreDoWithBootWait(inst.ID, http.MethodGet, targetURL, nil, s.agents.GetCoreAPIKey(inst.ID))
		if err != nil {
			return nil, fmt.Errorf("read current core config: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("read current core config: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode current core config: %w", err)
		}
	} else {
		configPath := filepath.Join(s.agents.instanceDir(inst.ID), "config.json")
		data, err := os.ReadFile(configPath)
		if err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("decode saved config: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read saved config: %w", err)
		} else if strings.TrimSpace(inst.Config) != "" {
			_ = json.Unmarshal([]byte(inst.Config), &cfg)
		}
	}
	if cfg == nil {
		return []map[string]any{}, nil
	}
	return mcpMaps(cfg["mcp_servers"]), nil
}

func mcpMaps(value any) []map[string]any {
	switch list := value.(type) {
	case []map[string]any:
		return append([]map[string]any(nil), list...)
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if entry, ok := item.(map[string]any); ok {
				out = append(out, entry)
			}
		}
		return out
	default:
		return []map[string]any{}
	}
}

func mcpMapsAsAny(list []map[string]any) []any {
	out := make([]any, 0, len(list))
	for _, entry := range list {
		out = append(out, entry)
	}
	return out
}

func (s *Server) resolveAgentMCPConfigs(userID int64, inst *Agent, ids []int64, removing ...bool) ([]map[string]any, error) {
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	selected := make([]map[string]any, 0, len(ids))
	seenNames := map[string]bool{}
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("invalid mcp server id %d", id)
		}
		record, _, err := s.store.GetMCPServer(userID, id)
		if err != nil {
			record, err = s.store.GetMCPServerByIDUnscoped(id)
			if err != nil || record == nil {
				return nil, fmt.Errorf("mcp server %d not found", id)
			}
			// App rows can be platform-owned and managed rows can be shared
			// within their project. Do not expose another user's arbitrary
			// global custom/remote server merely because its id was guessed.
			if record.Source != "app" && record.Source != deviceMCPSource && record.Source != managedMCPSource &&
				(record.ProjectID == "" || record.ProjectID != inst.ProjectID) {
				return nil, fmt.Errorf("mcp server %d not found", id)
			}
		}
		if record.Source == deviceMCPSource {
			cfg, err := s.deviceMCPSelection(userID, inst, record, len(removing) > 0 && removing[0])
			if err != nil {
				return nil, err
			}
			selected = append(selected, cfg)
			continue
		}
		if err := attachmentProjectError("mcp server", id, record.ProjectID, inst.ProjectID); err != nil {
			return nil, err
		}
		cfg, err := gatewayMCPConfigFromRecord(*record, inst.ProjectID, port, "")
		if err != nil {
			return nil, fmt.Errorf("mcp server %d cannot be attached: %w", id, err)
		}
		name, _ := cfg["name"].(string)
		if name == "" || seenNames[name] {
			continue
		}
		seenNames[name] = true
		s.authorizeFileReferenceMCPConfig(inst, cfg)
		selected = append(selected, cfg)
	}
	return selected, nil
}

// attachmentProjectError is the one project rule for everything an agent can
// attach: an item owned by a project attaches only to agents of that project;
// an item with no project attaches anywhere.
func attachmentProjectError(kind string, id int64, ownerProject, agentProject string) error {
	if ownerProject == "" || ownerProject == agentProject {
		return nil
	}
	return fmt.Errorf("%s %d belongs to project %q, agent belongs to project %q",
		kind, id, ownerProject, agentProject)
}

// connectionMCPConfigsForCreate resolves the connections picked when creating
// an agent to MCP entries, by the same rule as attaching MCP servers later.
// Every connection is checked before an MCP server row is made for any of
// them. Connections that cannot be loaded are skipped, as before.
func (s *Server) connectionMCPConfigsForCreate(userID int64, inst *Agent, connectionIDs []int64) ([]map[string]any, error) {
	conns := make([]*Connection, 0, len(connectionIDs))
	seen := map[int64]bool{}
	for _, id := range connectionIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		conn, _, err := s.store.GetConnection(userID, id)
		if err != nil || conn == nil {
			log.Printf("[CREATE] skip bound connection id=%d: %v", id, err)
			continue
		}
		if err := attachmentProjectError("connection", conn.ID, conn.ProjectID, inst.ProjectID); err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	serverIDs := make([]int64, 0, len(conns))
	for _, conn := range conns {
		record, err := s.store.FindCanonicalMCPServerByConnection(conn.ID)
		if err != nil {
			log.Printf("[CREATE] resolve MCP for bound connection id=%d: %v", conn.ID, err)
			continue
		}
		if record != nil {
			serverIDs = append(serverIDs, record.ID)
			continue
		}
		toolCount := 0
		if s.catalog != nil {
			if app := s.catalog.Get(conn.AppSlug); app != nil {
				toolCount = len(app.Tools)
			}
		}
		mcpID, err := s.store.CreateMCPServerFromConnection(userID, conn, toolCount)
		if err != nil {
			log.Printf("[CREATE] create MCP for bound connection id=%d: %v", conn.ID, err)
			continue
		}
		serverIDs = append(serverIDs, mcpID)
	}
	configs, err := s.resolveAgentMCPConfigs(userID, inst, serverIDs)
	if err != nil {
		return nil, err
	}
	// Core reaches connection MCPs through this server's loopback route,
	// which needs the route capability.
	for _, cfg := range configs {
		if raw, ok := cfg["url"].(string); ok {
			cfg["url"] = authorizeMCPURL(raw, s.instanceSecret)
		}
	}
	return configs, nil
}

// seedSavedAgentMCPServers copies the MCP entries of a new agent's DB config
// into its saved config.json, where a stopped agent's MCP set is read from.
func (s *Server) seedSavedAgentMCPServers(inst *Agent) error {
	var cfg map[string]any
	if strings.TrimSpace(inst.Config) != "" {
		if err := json.Unmarshal([]byte(inst.Config), &cfg); err != nil {
			return fmt.Errorf("decode agent config: %w", err)
		}
	}
	servers := mcpMaps(cfg["mcp_servers"])
	if len(servers) == 0 {
		return nil
	}
	unlock := s.lockAgentConfig(inst.ID)
	defer unlock()
	return s.writeStoppedConfigAtomic(inst.ID, func(saved map[string]any) error {
		saved["mcp_servers"] = mcpMapsAsAny(mutateMCPServers(mcpMaps(saved["mcp_servers"]), servers, "add"))
		return nil
	})
}

func mutateMCPServers(current, selected []map[string]any, action string) []map[string]any {
	switch action {
	case "set":
		return append(filterSystemMCPConfigs(current), selected...)
	case "remove":
		return removeMatchingMCPConfigs(current, selected)
	default:
		return append(removeMatchingMCPConfigs(current, selected), selected...)
	}
}

// refreshAgentAppMCPConfigs replaces stale embedded app credentials and names
// with the current registry config before a core starts or is reattached.
// The install_id in the URL is the durable identity; app tokens and display
// names are allowed to change without disconnecting the agent from the app.
func (s *Server) refreshAgentAppMCPConfigs(inst *Agent) error {
	if inst == nil {
		return nil
	}
	current, err := s.currentAgentMCPServers(inst, 0)
	if err != nil {
		return err
	}
	port := s.port
	if port == "" {
		port = localServerPort()
	}
	refreshed := make([]map[string]any, 0, len(current))
	changed := false
	for _, entry := range current {
		if serverID, ok := localMCPServerIDFromConfig(entry); ok {
			var count int
			if err := s.store.dbQueryRow(`
				SELECT COUNT(*) FROM mcp_servers
				WHERE id=? AND (COALESCE(project_id,'')='' OR project_id=?)`,
				serverID, inst.ProjectID,
			).Scan(&count); err == nil && count == 0 {
				// Numeric gateway URLs are references to persisted registry rows.
				// If the row was removed by an uninstall or migration, retaining
				// the config leaves Core attached to a permanently dead endpoint.
				changed = true
				continue
			}
		}
		installID := appInstallIDFromMCPConfig(entry)
		if installID <= 0 {
			refreshed = append(refreshed, entry)
			continue
		}
		var serverID int64
		err := s.store.dbQueryRow(
			`SELECT id FROM mcp_servers
			 WHERE source='app' AND upstream_id=?
			   AND (COALESCE(project_id,'')='' OR project_id=?)`,
			appMCPUpstreamID(installID), inst.ProjectID,
		).Scan(&serverID)
		if err != nil {
			if inst.Kind != "platform_helper" && s.appInstallDisabled(installID) {
				// A disabled app rejects every connection, so Core would
				// carry a dead entry that fails each later update. The
				// binding stays and re-attaches the app when it is enabled.
				changed = true
				continue
			}
			// An uninstalled or temporarily unavailable app must not cause us
			// to destructively drop the user's desired attachment.
			refreshed = append(refreshed, entry)
			continue
		}
		record, err := s.store.GetMCPServerByIDUnscoped(serverID)
		if err != nil {
			return err
		}
		replacement, err := gatewayMCPConfigFromRecord(*record, inst.ProjectID, port, "")
		if err != nil {
			refreshed = append(refreshed, entry)
			continue
		}
		s.authorizeFileReferenceMCPConfig(inst, replacement)
		if !mcpConfigsEqual(entry, replacement) {
			changed = true
		}
		refreshed = append(refreshed, replacement)
	}

	if changed {
		if err := s.saveStoppedAgentMCPServers(inst, refreshed); err != nil {
			return err
		}
		log.Printf("[MCP-ATTACH] refreshed stale app configs agent=%d", inst.ID)
	}
	return s.syncAppBindingsFromMCPServers(inst.ID, inst.ProjectID, mcpMapsAsAny(refreshed))
}

// saveStoppedAgentMCPServers writes a stopped agent's MCP set where its start
// will read it: config.json once the agent has one, otherwise the agent's
// initial configuration. The caller holds the agent config lock.
func (s *Server) saveStoppedAgentMCPServers(inst *Agent, servers []map[string]any) error {
	configPath := filepath.Join(s.agents.instanceDir(inst.ID), "config.json")
	if _, err := os.Stat(configPath); err == nil {
		return s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
			cfg["mcp_servers"] = mcpMapsAsAny(servers)
			return nil
		})
	} else if !os.IsNotExist(err) {
		return err
	}
	var cfg map[string]any
	if strings.TrimSpace(inst.Config) != "" {
		if err := json.Unmarshal([]byte(inst.Config), &cfg); err != nil {
			return err
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["mcp_servers"] = mcpMapsAsAny(servers)
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	inst.Config = string(encoded)
	return s.store.UpdateAgent(inst)
}

func (s *Server) appInstallDisabled(installID int64) bool {
	var status string
	err := s.store.dbQueryRow(`SELECT status FROM app_installs WHERE id=?`, installID).Scan(&status)
	return err == nil && status == "disabled"
}

// boundAppAgentIDs lists the agents with an enabled binding to an install.
func (s *Server) boundAppAgentIDs(installID int64) ([]int64, error) {
	rows, err := s.store.dbQuery(
		`SELECT agent_id FROM app_agent_bindings WHERE install_id=? AND enabled=1 ORDER BY agent_id`,
		installID,
	)
	if err != nil {
		return nil, fmt.Errorf("list bound agents for install %d: %w", installID, err)
	}
	defer rows.Close()
	var agentIDs []int64
	for rows.Next() {
		var agentID int64
		if err := rows.Scan(&agentID); err != nil {
			return nil, err
		}
		agentIDs = append(agentIDs, agentID)
	}
	return agentIDs, rows.Err()
}

// detachDisabledAppTools takes a disabled app out of the MCP configuration of
// every ordinary agent bound to it, live and stopped. The bindings stay: they
// are what re-attaches the app when it is enabled again. It runs after the
// bridge row is removed, so syncing bindings cannot drop them. Best-effort:
// an agent that cannot be updated now drops the entry at its next start.
func (s *Server) detachDisabledAppTools(installID int64) {
	agentIDs, err := s.boundAppAgentIDs(installID)
	if err != nil {
		log.Printf("[APPS-MCP] disable detach install=%d: %v", installID, err)
		return
	}
	for _, agentID := range agentIDs {
		agent, err := s.store.GetAgentByID(agentID)
		if err != nil || agent == nil || agent.Kind == "platform_helper" {
			continue
		}
		if err := s.detachDisabledAppFromAgent(agent, installID); err != nil {
			log.Printf("[APPS-MCP] disable detach install=%d agent=%d: %v", installID, agentID, err)
		}
	}
}

func (s *Server) detachDisabledAppFromAgent(agent *Agent, installID int64) error {
	unlock := s.lockAgentConfig(agent.ID)
	defer unlock()
	port := s.agents.GetPort(agent.ID)
	current, err := s.currentAgentMCPServers(agent, port)
	if err != nil {
		return err
	}
	next := removeAppInstallMCPConfig(current, installID)
	if mcpConfigListsEqual(current, next) {
		return nil
	}
	if port > 0 {
		_, _, _, err = s.applyOrdinaryAgentMCPServersLocked(agent, port, current, next)
		return err
	}
	if err := s.saveStoppedAgentMCPServers(agent, next); err != nil {
		return err
	}
	return s.syncAppBindingsFromMCPServers(agent.ID, agent.ProjectID, mcpMapsAsAny(next))
}

// attachAppMCPToStoppedBoundAgents puts an app's entry back into the saved
// configuration of stopped ordinary agents still bound to it, such as after
// the app is enabled again. Running agents get it from the live reconcile.
func (s *Server) attachAppMCPToStoppedBoundAgents(installID int64) error {
	agentIDs, err := s.boundAppAgentIDs(installID)
	if err != nil {
		return err
	}
	var errs []error
	for _, agentID := range agentIDs {
		if s.agents.IsRunning(agentID) {
			continue
		}
		if err := s.attachAppMCPToStoppedAgent(agentID, installID); err != nil {
			errs = append(errs, fmt.Errorf("attach install %d to stopped agent %d: %w", installID, agentID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) attachAppMCPToStoppedAgent(agentID, installID int64) error {
	unlock := s.lockAgentConfig(agentID)
	defer unlock()
	if s.agents.IsRunning(agentID) {
		return nil
	}
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return err
	}
	if agent == nil || agent.Kind == "platform_helper" {
		return nil
	}
	var serverID int64
	err = s.store.dbQueryRow(`
		SELECT id FROM mcp_servers
		WHERE source='app' AND upstream_id=?
		  AND (COALESCE(project_id,'')='' OR project_id=?)`,
		appMCPUpstreamID(installID), agent.ProjectID,
	).Scan(&serverID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	record, err := s.store.GetMCPServerByIDUnscoped(serverID)
	if err != nil {
		return err
	}
	serverPort := s.port
	if serverPort == "" {
		serverPort = localServerPort()
	}
	entry, err := gatewayMCPConfigFromRecord(*record, agent.ProjectID, serverPort, "")
	if err != nil {
		return err
	}
	s.authorizeFileReferenceMCPConfig(agent, entry)
	current, err := s.currentAgentMCPServers(agent, 0)
	if err != nil {
		return err
	}
	next := mutateMCPServers(current, []map[string]any{entry}, "add")
	if mcpConfigListsEqual(current, next) {
		return nil
	}
	if err := s.saveStoppedAgentMCPServers(agent, next); err != nil {
		return err
	}
	return s.syncAppBindingsFromMCPServers(agent.ID, agent.ProjectID, mcpMapsAsAny(next))
}

// reconcileRunningAgentAppMCP replaces one app attachment through Core's
// existing transactional MCP reconciliation. The app process already switched
// behind the stable server proxy; only an agent-visible capability revision
// reaches this path. A failed replacement leaves Core's previous connection
// intact, allowing the caller to use the restart fallback safely.
func (s *Server) reconcileRunningAgentAppMCP(ctx context.Context, agentID, installID int64) error {
	unlock := s.lockAgentConfig(agentID)
	defer unlock()

	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	port := s.agents.GetPort(agentID)
	if port == 0 {
		return fmt.Errorf("agent is not running")
	}
	current, err := s.currentAgentMCPServers(inst, port)
	if err != nil {
		return err
	}

	var serverID int64
	bridgeErr := s.store.dbQueryRow(`
		SELECT id FROM mcp_servers
		WHERE source='app' AND upstream_id=?
		  AND (COALESCE(project_id,'')='' OR project_id=?)`,
		appMCPUpstreamID(installID), inst.ProjectID,
	).Scan(&serverID)
	var desired []map[string]any
	if bridgeErr == sql.ErrNoRows {
		// The upgraded app no longer exposes agent-visible MCP tools. Detach
		// only this install; keep the app binding itself because it may still
		// provide skills, UI, workers, or other non-MCP capabilities.
		desired = removeAppInstallMCPConfig(current, installID)
	} else if bridgeErr != nil {
		return fmt.Errorf("resolve app MCP bridge: %w", bridgeErr)
	} else {
		record, err := s.store.GetMCPServerByIDUnscoped(serverID)
		if err != nil {
			return fmt.Errorf("load app MCP bridge: %w", err)
		}
		serverPort := s.port
		if serverPort == "" {
			serverPort = localServerPort()
		}
		replacement, err := gatewayMCPConfigFromRecord(*record, inst.ProjectID, serverPort, "")
		if err != nil {
			return err
		}
		s.authorizeFileReferenceMCPConfig(inst, replacement)
		desired = mutateMCPServers(current, []map[string]any{replacement}, "add")
	}
	if mcpConfigListsEqual(current, desired) {
		return nil
	}

	body, err := json.Marshal(map[string]any{"mcp_servers": mcpMapsAsAny(desired)})
	if err != nil {
		return err
	}
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
	resp, err := s.coreDoWithBootWaitContext(
		ctx, agentID, http.MethodPut, targetURL, body, s.agents.GetCoreAPIKey(agentID),
		http.Header{"Content-Type": []string{"application/json"}},
	)
	if err != nil {
		return fmt.Errorf("apply live app MCP config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return newCoreMCPApplyError(resp.StatusCode, detail, desired, installID)
	}
	if err := s.syncAppBindingsFromMCPServers(agentID, inst.ProjectID, mcpMapsAsAny(desired)); err != nil {
		return fmt.Errorf("sync app attachment metadata: %w", err)
	}
	return nil
}

// coreMCPApplyError is a live MCP configuration Core rejected. Core names the
// connections it could not make when it can; AppFailed reports whether one of
// them belongs to the app being applied.
type coreMCPApplyError struct {
	Status    int
	Detail    string
	Failed    []string
	AppFailed bool
}

func (e *coreMCPApplyError) Error() string {
	return fmt.Sprintf("apply live app MCP config: status %d: %s", e.Status, e.Detail)
}

func newCoreMCPApplyError(status int, body []byte, desired []map[string]any, installID int64) *coreMCPApplyError {
	out := &coreMCPApplyError{Status: status, Detail: strings.TrimSpace(string(body))}
	var reported struct {
		Error  string   `json:"error"`
		Failed []string `json:"failed_mcp_servers"`
	}
	if json.Unmarshal(body, &reported) != nil {
		return out
	}
	if reported.Error != "" {
		out.Detail = reported.Error
	}
	appNames := map[string]bool{}
	for _, entry := range desired {
		if name, _ := entry["name"].(string); name != "" && appInstallIDFromMCPConfig(entry) == installID {
			appNames[name] = true
		}
	}
	for _, name := range reported.Failed {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		out.Failed = append(out.Failed, name)
		if appNames[name] {
			out.AppFailed = true
		}
	}
	return out
}

func removeAppInstallMCPConfig(current []map[string]any, installID int64) []map[string]any {
	out := make([]map[string]any, 0, len(current))
	for _, entry := range current {
		if appInstallIDFromMCPConfig(entry) == installID {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func localMCPServerIDFromConfig(config map[string]any) (int64, bool) {
	rawURL, _ := config["url"].(string)
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return 0, false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return 0, false
	}
	path := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] != "mcp" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return id, err == nil && id > 0
}

func appInstallIDFromMCPConfig(config map[string]any) int64 {
	rawURL, _ := config["url"].(string)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(parsed.Query().Get("install_id"), 10, 64)
	return id
}

func mcpConfigsEqual(a, b map[string]any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// syncAppBindingsFromMCPServers makes app_agent_bindings derived metadata for
// MCP-capable apps. Bindings for UI/worker-only apps are preserved because
// those apps intentionally have no mcp_servers row.
func (s *Server) syncAppBindingsFromMCPServers(agentID int64, projectID string, value any) error {
	wanted := map[int64]bool{}
	for _, entry := range mcpMaps(value) {
		rawURL, _ := entry["url"].(string)
		parsed, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		installID, err := strconv.ParseInt(parsed.Query().Get("install_id"), 10, 64)
		if err != nil || installID <= 0 {
			continue
		}
		var count int
		if err := s.store.dbQueryRow(`
			SELECT COUNT(*) FROM mcp_servers
			WHERE source='app' AND upstream_id=? AND (COALESCE(project_id,'')='' OR project_id=?)`,
			appMCPUpstreamID(installID), projectID).Scan(&count); err == nil && count > 0 {
			wanted[installID] = true
		}
	}

	tx, err := s.store.dbBegin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		DELETE FROM app_agent_bindings
		WHERE agent_id=? AND install_id IN (
			SELECT CAST(SUBSTR(upstream_id, 5) AS INTEGER)
			FROM mcp_servers
			WHERE source='app' AND upstream_id LIKE 'app:%'
			  AND (COALESCE(project_id,'')='' OR project_id=?)
		)`, agentID, projectID); err != nil {
		return err
	}
	for installID := range wanted {
		if _, err := tx.Exec(`
			INSERT INTO app_agent_bindings(install_id, agent_id, enabled)
			VALUES(?, ?, 1)
			ON CONFLICT(install_id, agent_id) DO UPDATE SET enabled=1`,
			installID, agentID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return fmt.Errorf("load agent for app skill reconciliation: %w", err)
	}
	if _, err := s.reconcileAgentAppSkills(inst); err != nil {
		return fmt.Errorf("reconcile app skills: %w", err)
	}
	return nil
}

// reconcileAllAgentAppBindings repairs historical split-brain rows at boot.
// Core writes its desired MCP set to each agent's config.json, so this can run
// before live cores are reattached. A missing/malformed config is skipped
// rather than interpreted as an empty set.
func (s *Server) reconcileAllAgentAppBindings() {
	rows, err := s.store.dbQuery(`SELECT id FROM agents ORDER BY id`)
	if err != nil {
		log.Printf("[MCP-BIND] list agents for startup reconciliation: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		inst, err := s.store.GetAgentByID(id)
		if err != nil {
			continue
		}
		configPath := filepath.Join(s.agents.instanceDir(id), "config.json")
		if _, err := os.Stat(configPath); err != nil {
			if os.IsNotExist(err) {
				// Never started: inst.Config is the initial canonical source.
				if strings.TrimSpace(inst.Config) == "" {
					continue
				}
			} else {
				log.Printf("[MCP-BIND] inspect agent=%d config: %v", id, err)
				continue
			}
		}
		servers, err := s.currentAgentMCPServers(inst, 0)
		if err != nil {
			log.Printf("[MCP-BIND] read agent=%d config: %v", id, err)
			continue
		}
		if err := s.syncAppBindingsFromMCPServers(id, inst.ProjectID, mcpMapsAsAny(servers)); err != nil {
			log.Printf("[MCP-BIND] reconcile agent=%d: %v", id, err)
		}
	}
}

// appMCPConfigsForInstallIDs resolves wizard app selections to the same MCP
// inventory records used by the detail-page picker.
func (s *Server) appMCPConfigsForInstallIDs(userID int64, inst *Agent, installIDs []int64) ([]int64, []map[string]any, error) {
	validIDs := make([]int64, 0, len(installIDs))
	serverIDs := make([]int64, 0, len(installIDs))
	seenIDs := map[int64]bool{}
	for _, installID := range installIDs {
		if installID <= 0 || seenIDs[installID] {
			if installID <= 0 {
				return nil, nil, fmt.Errorf("invalid app install id %d", installID)
			}
			continue
		}
		seenIDs[installID] = true
		var installProject string
		var serverID int64
		var skillCount int
		err := s.store.dbQueryRow(`
			SELECT COALESCE(i.project_id,''), COALESCE(m.id,0),
			       (SELECT COUNT(*) FROM skills sk WHERE sk.install_id=i.id AND sk.enabled=1)
			FROM app_installs i
			LEFT JOIN mcp_servers m ON m.upstream_id=?
			WHERE i.id=? AND i.status='running' AND i.private=0`,
			appMCPUpstreamID(installID), installID).Scan(&installProject, &serverID, &skillCount)
		if err != nil {
			return nil, nil, fmt.Errorf("app install %d is not running or does not exist", installID)
		}
		if err := attachmentProjectError("app install", installID, installProject, inst.ProjectID); err != nil {
			return nil, nil, err
		}
		if serverID == 0 && skillCount == 0 {
			return nil, nil, fmt.Errorf("app install %d has no agent tools or skills", installID)
		}
		validIDs = append(validIDs, installID)
		if serverID > 0 {
			serverIDs = append(serverIDs, serverID)
		}
	}
	configs, err := s.resolveAgentMCPConfigs(userID, inst, serverIDs)
	if err != nil {
		return nil, nil, err
	}
	return validIDs, configs, nil
}
