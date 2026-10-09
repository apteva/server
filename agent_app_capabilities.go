package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const gatewayAppAttachmentGuidance = "Installing an app does not attach its tools to agents. Agents directly using its tools need that capability attached to themselves, including coordination and domain apps. Use agents_update with top-level bound_app_install_ids and mcp_action=add; verify capabilities in the receipt or agents_get. Some apps ensure attachments at dispatch. Helper's operator app_tool_search/app_tool_call broker requires no permanent attachment."

// Attachment selectors are API operations, not runtime settings. Reject them
// rather than persisting a setting that cannot attach any tools.
func validateAgentAttachmentSettings(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || cfg == nil {
		return fmt.Errorf("config must be a JSON object")
	}
	for _, field := range []string{"bound_app_install_ids", "bound_connection_ids", "mcp_server_ids", "mcp_action", "mcp_servers"} {
		if _, exists := cfg[field]; exists {
			return fmt.Errorf("config.%s cannot attach capabilities; use top-level bound_app_install_ids or mcp_server_ids with mcp_action in agents_update, or POST /agents/:id/mcp-servers", field)
		}
	}
	return nil
}

func positiveAttachmentIDs(value any) ([]int64, error) {
	if list, ok := value.([]any); ok {
		for _, item := range list {
			if n, ok := item.(float64); ok && (n <= 0 || n != float64(int64(n))) {
				return nil, fmt.Errorf("IDs must be positive integers")
			}
		}
	}
	ids, err := parseIntListArg(value)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("IDs must be positive integers")
		}
	}
	return ids, nil
}

// Resolve installed apps into the existing registry attachment path. All IDs
// are resolved before any mutation; app bindings and skills remain derived
// from the real MCP set by the shared config handler.
func (s *Server) resolveAgentAppMCPIDs(inst *Agent, installIDs []int64) ([]int64, error) {
	ids := []int64{}
	for _, installID := range installIDs {
		if installID <= 0 {
			return nil, fmt.Errorf("bound_app_install_ids must contain positive integers")
		}
		var project string
		if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&project); err != nil {
			return nil, fmt.Errorf("app installation %d not found", installID)
		}
		if project != "" && project != inst.ProjectID {
			return nil, fmt.Errorf("app installation %d is not in the agent's project", installID)
		}
		var mcpID int64
		if err := s.store.db.QueryRow(`SELECT id FROM mcp_servers WHERE source='app' AND upstream_id=? AND COALESCE(project_id,'')=? ORDER BY id LIMIT 1`, appMCPUpstreamID(installID), project).Scan(&mcpID); err != nil {
			return nil, fmt.Errorf("app installation %d has no registered MCP tools to attach", installID)
		}
		ids = append(ids, mcpID)
	}
	return ids, nil
}

// Return only identities derived from actual live/saved MCP configuration.
// Never copy config URLs, headers, commands, arguments or credentials.
func gatewayAgentCapabilities(agentID int64, api gatewayAPIClient, store *Store) (map[string]any, error) {
	if store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	var cfg struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := api.do(http.MethodGet, fmt.Sprintf("/agents/%d/config", agentID), nil, &cfg); err != nil {
		return nil, fmt.Errorf("verify agent capabilities: %w", err)
	}
	agent, err := store.GetAgentByID(agentID)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.Query(`SELECT id FROM mcp_servers WHERE (COALESCE(project_id,'')='' OR project_id=?) AND (user_id=? OR source IN ('app', 'builtin') OR (project_id<>'' AND project_id=?)) ORDER BY id`, agent.ProjectID, api.userID, agent.ProjectID)
	if err != nil {
		return nil, err
	}
	var registryIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		registryIDs = append(registryIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var records []MCPServerRecord
	for _, id := range registryIDs {
		row, err := store.GetMCPServerByIDUnscoped(id)
		if err != nil {
			return nil, err
		}
		if row != nil {
			records = append(records, *row)
		}
	}
	base, _ := url.Parse(api.baseURL)
	servers := []map[string]any{}
	apps := []map[string]any{}
	appSeen := map[int64]bool{}
	actualIDs := []int64{}
	for _, entry := range cfg.MCPServers {
		name, _ := entry["name"].(string)
		if strings.TrimSpace(name) == "" || gatewayMCPConfigIsSystem(entry) {
			continue
		}
		safe := map[string]any{"name": name}
		raw, _ := entry["url"].(string)
		actual, _ := url.Parse(raw)
		for _, record := range records {
			if record.Name != name || actual == nil || base == nil {
				continue
			}
			expectedCfg, err := gatewayMCPConfigFromRecord(record, agent.ProjectID, base.Port(), "")
			if err != nil {
				continue
			}
			expectedRaw, _ := expectedCfg["url"].(string)
			expected, err := url.Parse(expectedRaw)
			if err != nil || !sameGatewayCapabilityEndpoint(actual, expected, record.Source) {
				continue
			}
			safe["id"], safe["source"] = record.ID, record.Source
			actualIDs = append(actualIDs, record.ID)
			if record.ConnectionID > 0 {
				safe["connection_id"] = record.ConnectionID
			}
			if record.Source == "app" {
				installID, _ := strconv.ParseInt(strings.TrimPrefix(record.UpstreamID, "app:"), 10, 64)
				if installID > 0 {
					safe["install_id"] = installID
					if !appSeen[installID] {
						var appName, manifest, status string
						if err := store.db.QueryRow(`SELECT a.name,a.manifest_json,i.status FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID).Scan(&appName, &manifest, &status); err != nil {
							return nil, err
						}
						var metadata struct {
							DisplayName string `json:"display_name"`
						}
						_ = json.Unmarshal([]byte(manifest), &metadata)
						apps = append(apps, map[string]any{"install_id": installID, "name": appName, "display_name": metadata.DisplayName, "mcp_server_id": record.ID, "status": status})
						appSeen[installID] = true
					}
				}
			}
			break
		}
		servers = append(servers, safe)
	}
	return map[string]any{"mcp_servers": servers, "apps": apps, "mcp_server_ids": actualIDs, "count": len(servers)}, nil
}

func sameGatewayCapabilityEndpoint(actual, expected *url.URL, source string) bool {
	if source == "remote" {
		return actual.String() == expected.String()
	}
	host := func(u *url.URL) string {
		if u.Hostname() == "localhost" {
			return "127.0.0.1"
		}
		return u.Hostname()
	}
	if actual.Scheme != expected.Scheme || host(actual) != host(expected) || actual.Port() != expected.Port() || actual.Path != expected.Path {
		return false
	}
	if source == "app" {
		return actual.Query().Get("install_id") == expected.Query().Get("install_id")
	}
	return true
}
