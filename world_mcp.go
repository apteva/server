package main

// world_mcp.go — the World control surface, exposed as MCP tools.
//
// Worlds are driven the same way as everything else in apteva: by tool calls.
// This is an MCP-over-HTTP endpoint (same shape as the eval-mock-gateway) that
// the meta-agent gets in its mcp_servers, so it creates + seeds + tears down
// Worlds by calling tools — no bespoke "plan" codepath.
//
// It lives on the MAIN server (not the --mcp-gateway subprocess) because the
// WorldManager is in-memory here; the subprocess only has a DB handle and
// couldn't see live worlds. Loopback-only; the meta-agent is a trusted
// platform component.
//
// Tools:
//   world_create_for_agent(agent_id)            → {world_id, apps}
//   world_call_app(world_id, app, tool, input)  → the tool's result (seeding)
//   world_list()                                → live worlds
//   world_destroy(world_id)

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (s *Server) handleWorldMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	respond := func(result any, errMsg string) {
		resp := map[string]any{"jsonrpc": "2.0"}
		if len(req.ID) > 0 {
			resp["id"] = req.ID
		}
		if errMsg != "" {
			resp["error"] = map[string]any{"code": -32603, "message": errMsg}
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}

	switch req.Method {
	case "initialize":
		respond(map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "apteva-worlds", "version": "1.0.0"},
		}, "")
	case "tools/list":
		respond(map[string]any{"tools": worldMCPTools()}, "")
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			respond(nil, "invalid tools/call params")
			return
		}
		var args map[string]any
		_ = json.Unmarshal(p.Arguments, &args)
		result, err := s.worldMCPCall(p.Name, args)
		if err != nil {
			respond(nil, err.Error())
			return
		}
		// Conventional MCP content envelope: one text part with the JSON.
		b, _ := json.Marshal(result)
		respond(map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}, "")
	default:
		respond(nil, "method not found: "+req.Method)
	}
}

func worldMCPTools() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	str := map[string]any{"type": "string"}
	return []map[string]any{
		{
			"name":        "world_create_for_agent",
			"description": "Create an isolated test World for an agent, with the agent's bound apps installed (real, isolated). Returns {world_id, apps}.",
			"inputSchema": obj(map[string]any{"agent_id": map[string]any{"type": "integer", "description": "Agent to build the world for"}}, "agent_id"),
		},
		{
			"name":        "world_call_app",
			"description": "Call a tool on an app inside a World — use this to seed starting state (create files, contacts, …) by driving the app's real tools. Returns the tool's result.",
			"inputSchema": obj(map[string]any{
				"world_id": str,
				"app":      str,
				"tool":     str,
				"input":    map[string]any{"type": "object", "additionalProperties": true},
			}, "world_id", "app", "tool"),
		},
		{
			"name":        "world_list",
			"description": "List live test Worlds.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "world_destroy",
			"description": "Tear down a test World and free its resources.",
			"inputSchema": obj(map[string]any{"world_id": str}, "world_id"),
		},
	}
}

func (s *Server) worldMCPCall(name string, args map[string]any) (any, error) {
	switch name {
	case "world_create_for_agent":
		agentID := int64(toFloat(args["agent_id"]))
		if agentID == 0 {
			return nil, fmt.Errorf("agent_id required")
		}
		agent, err := s.store.GetAgentByID(agentID)
		if err != nil || agent == nil {
			return nil, fmt.Errorf("agent %d not found", agentID)
		}
		worldID := fmt.Sprintf("eval-%d-%d", agentID, time.Now().UnixNano())
		world, err := s.CreateWorldForAgent(agent, worldID)
		if err != nil {
			return nil, err
		}
		apps := world.InstallNames()
		for n := range world.Apps() {
			apps = append(apps, n)
		}
		return map[string]any{"world_id": worldID, "apps": apps}, nil

	case "world_call_app":
		worldID, _ := args["world_id"].(string)
		app, _ := args["app"].(string)
		tool, _ := args["tool"].(string)
		input, _ := args["input"].(map[string]any)
		world, ok := s.worlds.Get(worldID)
		if !ok {
			return nil, fmt.Errorf("world %q not found", worldID)
		}
		results, err := s.ExecuteSeedPlan(world, []SeedCall{{App: app, Tool: tool, Input: input}})
		if err != nil {
			return nil, err
		}
		var out any
		if len(results) > 0 {
			_ = json.Unmarshal(results[0], &out)
		}
		return out, nil

	case "world_list":
		out := []map[string]any{}
		for _, wd := range s.worlds.List() {
			out = append(out, map[string]any{"world_id": wd.ID, "project_id": wd.ProjectID})
		}
		return map[string]any{"worlds": out}, nil

	case "world_destroy":
		worldID, _ := args["world_id"].(string)
		s.worlds.Destroy(worldID)
		return map[string]any{"destroyed": worldID}, nil

	default:
		return nil, fmt.Errorf("unknown world tool: %s", name)
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
