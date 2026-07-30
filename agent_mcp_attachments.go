package main

import (
	"bytes"
	"encoding/json"
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
// {"action":"add"|"remove"|"set","mcp_server_ids":[1,2]}
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
		Action       string  `json:"action"`
		MCPServerIDs []int64 `json:"mcp_server_ids"`
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
		"_mcp_action":     body.Action,
		"_mcp_server_ids": body.MCPServerIDs,
	})
	r.Method = http.MethodPut
	r.URL.Path = "/instances/" + strconv.FormatInt(instanceID, 10) + "/config"
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	r.Header.Set("Content-Type", "application/json")
	s.handleUpdateConfig(w, r)
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

func (s *Server) resolveAgentMCPConfigs(userID int64, inst *Agent, ids []int64) ([]map[string]any, error) {
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
			if record.Source != "app" && record.Source != managedMCPSource &&
				(record.ProjectID == "" || record.ProjectID != inst.ProjectID) {
				return nil, fmt.Errorf("mcp server %d not found", id)
			}
		}
		if record.ProjectID != "" && record.ProjectID != inst.ProjectID {
			return nil, fmt.Errorf("mcp server %d belongs to project %q, agent belongs to project %q",
				id, record.ProjectID, inst.ProjectID)
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
		selected = append(selected, cfg)
	}
	return selected, nil
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
		installID := appInstallIDFromMCPConfig(entry)
		if installID <= 0 {
			refreshed = append(refreshed, entry)
			continue
		}
		var serverID int64
		err := s.store.db.QueryRow(
			`SELECT id FROM mcp_servers
			 WHERE source='app' AND upstream_id=?
			   AND (COALESCE(project_id,'')='' OR project_id=?)`,
			appMCPUpstreamID(installID), inst.ProjectID,
		).Scan(&serverID)
		if err != nil {
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
		if !mcpConfigsEqual(entry, replacement) {
			changed = true
		}
		refreshed = append(refreshed, replacement)
	}

	configPath := filepath.Join(s.agents.instanceDir(inst.ID), "config.json")
	if changed {
		if _, err := os.Stat(configPath); err == nil {
			if err := s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
				cfg["mcp_servers"] = mcpMapsAsAny(refreshed)
				return nil
			}); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		} else {
			var cfg map[string]any
			if strings.TrimSpace(inst.Config) != "" {
				if err := json.Unmarshal([]byte(inst.Config), &cfg); err != nil {
					return err
				}
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			cfg["mcp_servers"] = mcpMapsAsAny(refreshed)
			encoded, err := json.Marshal(cfg)
			if err != nil {
				return err
			}
			inst.Config = string(encoded)
			if err := s.store.UpdateAgent(inst); err != nil {
				return err
			}
		}
		log.Printf("[MCP-ATTACH] refreshed stale app configs agent=%d", inst.ID)
	}
	return s.syncAppBindingsFromMCPServers(inst.ID, inst.ProjectID, mcpMapsAsAny(refreshed))
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
		if err := s.store.db.QueryRow(`
			SELECT COUNT(*) FROM mcp_servers
			WHERE source='app' AND upstream_id=? AND (COALESCE(project_id,'')='' OR project_id=?)`,
			appMCPUpstreamID(installID), projectID).Scan(&count); err == nil && count > 0 {
			wanted[installID] = true
		}
	}

	tx, err := s.store.db.Begin()
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
	return tx.Commit()
}

// reconcileAllAgentAppBindings repairs historical split-brain rows at boot.
// Core writes its desired MCP set to each agent's config.json, so this can run
// before live cores are reattached. A missing/malformed config is skipped
// rather than interpreted as an empty set.
func (s *Server) reconcileAllAgentAppBindings() {
	rows, err := s.store.db.Query(`SELECT id FROM agents ORDER BY id`)
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
func (s *Server) appMCPConfigsForInstallIDs(userID int64, inst *Agent, installIDs []int64) ([]int64, []map[string]any) {
	validIDs := make([]int64, 0, len(installIDs))
	serverIDs := make([]int64, 0, len(installIDs))
	for _, installID := range installIDs {
		var installProject string
		var serverID int64
		err := s.store.db.QueryRow(`
			SELECT COALESCE(i.project_id,''), COALESCE(m.id,0)
			FROM app_installs i
			LEFT JOIN mcp_servers m ON m.upstream_id=?
			WHERE i.id=? AND i.status='running'`,
			appMCPUpstreamID(installID), installID).Scan(&installProject, &serverID)
		if err != nil || (installProject != "" && installProject != inst.ProjectID) {
			continue
		}
		validIDs = append(validIDs, installID)
		if serverID > 0 {
			serverIDs = append(serverIDs, serverID)
		}
	}
	configs, err := s.resolveAgentMCPConfigs(userID, inst, serverIDs)
	if err != nil {
		return validIDs, nil
	}
	return validIDs, configs
}
