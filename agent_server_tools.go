package main

// agent_server_tools.go lets an operator see and change which Apteva Server
// tools an agent may use. An agent made from a preset that grants a limited
// set keeps that set in its saved config as platform_mcp_tools, and every
// call and tool listing is checked against it (platformToolPolicy). Without
// the key the agent may use all of them. Nothing after creation used to show
// the set or change it, so an operator, or the Helper acting for one, could
// not widen it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

const serverToolsAll = "all"

// serverToolNames lists every Apteva Server tool a project agent can be given.
func serverToolNames() []string {
	names := make([]string, 0, len(projectConversationGatewayTools))
	for name := range projectConversationGatewayTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// agentServerTools reports the tools the agent may use: "all", or the list it
// was given, in its saved order.
func agentServerTools(agent *Agent) any {
	allowed, restricted := platformToolPolicy(agent)
	if !restricted {
		return serverToolsAll
	}
	var cfg map[string]json.RawMessage
	var saved []string
	if json.Unmarshal([]byte(agent.Config), &cfg) == nil {
		_ = json.Unmarshal(cfg["platform_mcp_tools"], &saved)
	}
	tools := []string{}
	for _, name := range saved {
		if allowed[name] && !containsExact(tools, name) {
			tools = append(tools, name)
		}
	}
	return tools
}

// parseServerTools reads "all" (or ["all"]) or a list of tool names. It
// reports nil tools for "all".
func parseServerTools(value any) ([]string, error) {
	const shape = `apteva_server_tools must be "all" or a list of Apteva Server tool names`
	if text, ok := value.(string); ok {
		if strings.EqualFold(strings.TrimSpace(text), serverToolsAll) {
			return nil, nil
		}
		return nil, errors.New(shape)
	}
	list, ok := value.([]any)
	if !ok {
		if names, isNames := value.([]string); isNames {
			for _, name := range names {
				list = append(list, name)
			}
		} else {
			return nil, errors.New(shape)
		}
	}
	if len(list) == 1 {
		if text, _ := list[0].(string); strings.EqualFold(strings.TrimSpace(text), serverToolsAll) {
			return nil, nil
		}
	}
	if len(list) == 0 {
		return nil, errors.New(`apteva_server_tools needs at least one tool; use "all" to remove the limit`)
	}
	tools := []string{}
	var unknown []string
	for _, raw := range list {
		name, ok := raw.(string)
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, errors.New(shape)
		}
		if strings.EqualFold(name, serverToolsAll) {
			return nil, errors.New(`"all" cannot be combined with tool names`)
		}
		if !projectConversationGatewayTools[name] {
			unknown = append(unknown, name)
			continue
		}
		if !containsExact(tools, name) {
			tools = append(tools, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown Apteva Server tools: %s. Choose from: %s", strings.Join(unknown, ", "), strings.Join(serverToolNames(), ", "))
	}
	return tools, nil
}

// setAgentServerTools saves the agent's tools; nil tools removes the limit.
// The caller reconnects a running agent afterwards so its tool list follows.
func (s *Server) setAgentServerTools(agentID int64, tools []string) (*Agent, error) {
	unlock := s.lockAgentConfig(agentID)
	defer unlock()
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return nil, err
	}
	if agent.Kind == "platform_helper" {
		return nil, errors.New("the Helper's Apteva Server tools are managed by the server")
	}
	if tools != nil && !platformMCPAttached(agent) {
		return nil, errors.New("the agent has no Apteva Server tools attached; attach apteva-server first")
	}
	cfg := map[string]any{}
	if strings.TrimSpace(agent.Config) != "" {
		if err := json.Unmarshal([]byte(agent.Config), &cfg); err != nil {
			return nil, fmt.Errorf("decode agent config: %w", err)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	if tools == nil {
		delete(cfg, "platform_mcp_tools")
	} else {
		cfg["platform_mcp_tools"] = tools
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	agent.Config = string(encoded)
	if err := s.store.UpdateAgent(agent); err != nil {
		return nil, err
	}
	return agent, nil
}

// checkServerToolsPatch refuses a config patch that changes the tool limit;
// that change goes through setAgentServerTools. Sending back the saved value
// unchanged is allowed.
func checkServerToolsPatch(saved, patch map[string]any) error {
	value, ok := patch["platform_mcp_tools"]
	if !ok {
		return nil
	}
	current, exists := saved["platform_mcp_tools"]
	if exists && reflect.DeepEqual(normalizeJSONValue(current), normalizeJSONValue(value)) {
		return nil
	}
	return errors.New("change an agent's Apteva Server tools with apteva_server_tools in agents_update or PUT /agents/:id/server-tools, not in config")
}

func normalizeJSONValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	_ = json.Unmarshal(encoded, &out)
	return out
}

func serverToolsReply(agent *Agent) map[string]any {
	return map[string]any{
		"agent_id":  agent.ID,
		"attached":  platformMCPAttached(agent),
		"tools":     agentServerTools(agent),
		"available": serverToolNames(),
	}
}

// handleAgentServerTools serves /agents/:id/server-tools.
//
// GET reports the agent's Apteva Server tools and every tool it could have.
// PUT {"tools": "all" | ["agents_list", ...]} sets them. A running agent
// reconnects its Apteva Server tools so its tool list changes at once.
func (s *Server) handleAgentServerTools(w http.ResponseWriter, r *http.Request, agentID int64) {
	switch r.Method {
	case http.MethodGet:
		agent, err := s.store.GetAgentByIDContext(r.Context(), agentID)
		if err != nil {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		writeJSON(w, serverToolsReply(agent))
	case http.MethodPut:
		var body struct {
			Tools any `json:"tools"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		tools, err := parseServerTools(body.Tools)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		agent, err := s.setAgentServerTools(agentID, tools)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[SERVER-TOOLS] agent=%d tools=%v by=%d", agentID, agentServerTools(agent), getUserID(r))
		reply := serverToolsReply(agent)
		reply["applies"] = s.reloadAgentServerTools(r.Context(), agent)
		writeJSON(w, reply)
	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
	}
}

// reloadAgentServerTools makes a running agent list its Apteva Server tools
// again and says when the change applies.
func (s *Server) reloadAgentServerTools(ctx context.Context, agent *Agent) string {
	if s.agents.GetPort(agent.ID) == 0 {
		return "on next start"
	}
	if err := s.reloadGrantedAttachment(ctx, agent.ID, platformMCPName); err != nil {
		log.Printf("[SERVER-TOOLS] reload agent=%d: %v", agent.ID, err)
		return "calls are checked now; the agent's tool list updates after a restart"
	}
	return "now"
}
