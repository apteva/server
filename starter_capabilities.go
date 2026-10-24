package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentCapability declares an explicit built-in MCP grant using a portable name.
// Absence keeps legacy attachment behavior. Grants are reviewed with preset
// contents and scoped to the newly created agent, never the shared MCP row.
type AgentCapability struct {
	Name         string   `json:"name"`
	AllowedTools []string `json:"allowed_tools"`
}

func validateAgentCapabilities(caps []AgentCapability) error {
	seen := map[string]bool{}
	for _, cap := range caps {
		if cap.Name != platformMCPName || seen[cap.Name] {
			return fmt.Errorf("unknown or duplicate capability %q", cap.Name)
		}
		seen[cap.Name] = true
		if cap.AllowedTools == nil {
			return fmt.Errorf("capability %q requires explicit allowed_tools", cap.Name)
		}
		tools := map[string]bool{}
		for _, tool := range cap.AllowedTools {
			if !projectConversationGatewayTools[tool] || tools[tool] {
				return fmt.Errorf("invalid or duplicate capability tool %q", tool)
			}
			tools[tool] = true
		}
	}
	return nil
}

func (s *Server) configWithAgentCapabilities(agent *Agent, caps []AgentCapability) (string, error) {
	if err := validateAgentCapabilities(caps); err != nil {
		return "", err
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(agent.Config), &cfg); err != nil || cfg == nil {
		return "", fmt.Errorf("config must be a JSON object")
	}
	for _, cap := range caps {
		if err := s.authorizePlatformMCPAgent(agent); err != nil {
			return "", err
		}
		if err := s.store.ensurePlatformMCPInventory(agent.UserID); err != nil {
			return "", err
		}
		cfg["mcp_servers"] = mutateMCPServers(mcpMaps(cfg["mcp_servers"]), []map[string]any{managementGatewayConfig(agent, "", s.port)}, "add")
		cfg["include_apteva_server"] = true
		cfg["platform_mcp_tools"] = cap.AllowedTools
	}
	raw, err := json.Marshal(cfg)
	return string(raw), err
}

// A missing policy preserves existing agents; an invalid policy fails closed.
func platformToolPolicy(agent *Agent) (map[string]bool, bool) {
	if agent == nil {
		return map[string]bool{}, true
	}
	if strings.TrimSpace(agent.Config) == "" {
		return nil, false
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal([]byte(agent.Config), &cfg) != nil {
		return map[string]bool{}, true
	}
	raw, present := cfg["platform_mcp_tools"]
	if !present {
		return nil, false
	}
	var tools []string
	if json.Unmarshal(raw, &tools) != nil {
		return map[string]bool{}, true
	}
	allowed := map[string]bool{}
	for _, name := range tools {
		if projectConversationGatewayTools[name] {
			allowed[name] = true
		}
	}
	return allowed, true
}
func platformToolAllowed(agent *Agent, name string) bool {
	allowed, restricted := platformToolPolicy(agent)
	return !restricted || allowed[name]
}

func (s *Server) checkPlatformToolGrant(agent *Agent, body []byte) error {
	allowed, restricted := platformToolPolicy(agent)
	if !restricted {
		return nil
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Method != "tools/call" {
		return nil
	}
	name, args := rpc.Params.Name, rpc.Params.Arguments
	if !allowed[name] {
		return fmt.Errorf("tool %q is not granted to this agent", name)
	}
	if name != "agents_create" && name != "agents_update" {
		return nil
	}
	// Restricted callers may configure ordinary specialists, but may not mint
	// or alter management grants (including their own or a privileged teammate).
	if name == "agents_update" {
		id, _ := parseIntArg(args["id"])
		target, err := s.store.GetAgentByID(id)
		if err != nil || platformMCPAttached(target) {
			return fmt.Errorf("management-enabled agents must be configured by the operator")
		}
	}
	for _, key := range []string{"capabilities", "include_apteva_server", "platform_mcp_tools", "mcp_servers", "_mcp_server_ids"} {
		if _, ok := args[key]; ok {
			return fmt.Errorf("management grants must be configured by the operator")
		}
	}
	if raw, ok := args["config"]; ok {
		cfg, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("config must be an object")
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if err := validateAgentAttachmentSettings(string(encoded)); err != nil {
			return err
		}
		for _, key := range []string{"include_apteva_server", "platform_mcp_tools", "capabilities", "_mcp_server_ids"} {
			if _, ok := cfg[key]; ok {
				return fmt.Errorf("management grants must be configured by the operator")
			}
		}
	}
	if raw, ok := args["mcp_server_ids"]; ok {
		ids, err := positiveAttachmentIDs(raw)
		if err != nil {
			return err
		}
		for _, id := range ids {
			row, err := s.store.GetMCPServerByIDUnscoped(id)
			if err != nil || row == nil {
				return fmt.Errorf("MCP server not found")
			}
			if row.Source == platformMCPSource {
				return fmt.Errorf("built-in management grants must be configured by the operator")
			}
		}
	}
	return nil
}
