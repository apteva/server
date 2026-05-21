package main

// world_seed.go — AI-seeding: populate a world's starting state by driving the
// apps' REAL tools (never hand-written DB rows).
//
// Split in two, on purpose:
//   - The meta-agent PROPOSES a seed plan — a list of {app, tool, input}
//     calls — from a plain-English instruction + the world's advertised tools.
//     (LLM; the proposer lives in platform_agent.go and is gated on a
//     provider, like the judge.)
//   - ExecuteSeedPlan EXECUTES the plan deterministically against the
//     in-world app tools, authenticated with each install's token. This half
//     is app-agnostic (it forwards whatever the plan says) and needs no LLM,
//     so it's the verifiable core.
//
// Running execution server-side (rather than an LLM "seeder agent" inside the
// world) sidesteps the agent→token-protected-install-MCP auth wiring and keeps
// the seed reproducible: the same plan produces the same state.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SeedCall is one tool invocation in a world's seed plan.
type SeedCall struct {
	App   string         `json:"app"`
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

// ExecuteSeedPlan runs each seed call against its in-world app's MCP endpoint
// (authenticated with the install's dev token). Returns each call's raw result
// and stops at the first error. App-agnostic: no per-app knowledge.
func (s *Server) ExecuteSeedPlan(world *World, plan []SeedCall) ([]json.RawMessage, error) {
	results := make([]json.RawMessage, 0, len(plan))
	for i, call := range plan {
		inst, ok := world.Install(call.App)
		if !ok {
			return results, fmt.Errorf("seed call %d: app %q not in world", i, call.App)
		}
		input := call.Input
		if input == nil {
			input = map[string]any{}
		}
		res, err := callAppMCPTool(inst.SidecarURL+"/mcp", fmt.Sprintf("dev-%d", inst.InstallID), call.Tool, input)
		if err != nil {
			return results, fmt.Errorf("seed call %d (%s.%s): %w", i, call.App, call.Tool, err)
		}
		results = append(results, res)
	}
	return results, nil
}

// callAppMCPTool POSTs a single tools/call to an app's MCP endpoint and
// returns the raw JSON-RPC result (or the tool's error). Used by seeding and
// reusable by any server-side caller that needs to drive an in-world app.
func callAppMCPTool(mcpURL, token, tool string, input map[string]any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": input},
	})
	req, err := http.NewRequest("POST", mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bad MCP response: %s", string(raw))
	}
	if env.Error != nil {
		return nil, fmt.Errorf("tool error: %s", env.Error.Message)
	}
	return env.Result, nil
}
