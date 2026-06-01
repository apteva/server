package main

// environment_seed.go — AI-seeding: populate a environment's starting state by driving the
// apps' REAL tools (never hand-written DB rows).
//
// Split in two, on purpose:
//   - The meta-agent PROPOSES a seed plan — a list of {app, tool, input}
//     calls — from a plain-English instruction + the environment's advertised tools.
//     (LLM; the proposer lives in platform_agent.go and is gated on a
//     provider, like the judge.)
//   - ExecuteSeedPlan EXECUTES the plan deterministically against the
//     in-environment app tools, authenticated with each install's token. This half
//     is app-agnostic (it forwards whatever the plan says) and needs no LLM,
//     so it's the verifiable core.
//
// Running execution server-side (rather than an LLM "seeder agent" inside the
// environment) sidesteps the agent→token-protected-install-MCP auth wiring and keeps
// the seed reproducible: the same plan produces the same state.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SeedCall is one tool invocation in a environment's seed plan.
type SeedCall struct {
	App   string         `json:"app"`
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
	File  string         `json:"file,omitempty"`
}

// ExecuteSeedPlan runs each seed call against its in-environment app's MCP endpoint
// (authenticated with the install's dev token). Returns each call's raw result
// and stops at the first error. App-agnostic: no per-app knowledge.
func (s *Server) ExecuteSeedPlan(environment *Environment, plan []SeedCall) ([]json.RawMessage, error) {
	return s.ExecuteSeedPlanWithBaseDir(environment, plan, "")
}

// ExecuteSeedPlanWithBaseDir is ExecuteSeedPlan plus local fixture support.
// If a call sets file (or input.file), the path is resolved under baseDir,
// read, and injected as content_base64 before the app tool is called.
func (s *Server) ExecuteSeedPlanWithBaseDir(environment *Environment, plan []SeedCall, baseDir string) ([]json.RawMessage, error) {
	results := make([]json.RawMessage, 0, len(plan))
	for i, call := range plan {
		inst, ok := environment.Install(call.App)
		if !ok {
			return results, fmt.Errorf("seed call %d: app %q not in environment", i, call.App)
		}
		input, err := prepareSeedInput(environment, call, baseDir)
		if err != nil {
			return results, fmt.Errorf("seed call %d (%s.%s): %w", i, call.App, call.Tool, err)
		}
		res, err := callAppMCPTool(inst.SidecarURL+"/mcp", fmt.Sprintf("dev-%d", inst.InstallID), call.Tool, input)
		if err != nil {
			return results, fmt.Errorf("seed call %d (%s.%s): %w", i, call.App, call.Tool, err)
		}
		results = append(results, res)
	}
	return results, nil
}

func prepareSeedInput(environment *Environment, call SeedCall, baseDir string) (map[string]any, error) {
	input := map[string]any{}
	for k, v := range call.Input {
		input[k] = v
	}
	file := strings.TrimSpace(call.File)
	if file == "" {
		if v, ok := input["file"].(string); ok {
			file = strings.TrimSpace(v)
			delete(input, "file")
		}
	}
	if file == "" {
		return input, nil
	}
	path, err := resolveSeedFixturePath(baseDir, file)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture %s: %w", path, err)
	}
	if _, ok := input["name"]; !ok {
		input["name"] = filepath.Base(path)
	}
	input["content_base64"] = base64.StdEncoding.EncodeToString(body)
	if environment != nil && environment.ID != "" {
		if _, ok := input["_project_id"]; !ok {
			input["_project_id"] = environment.ID
		}
	}
	return input, nil
}

func resolveSeedFixturePath(baseDir, file string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("seed file %q requires seed_base_dir", file)
	}
	file = strings.TrimPrefix(file, "file://")
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve seed_base_dir: %w", err)
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseAbs, path)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve seed file %q: %w", file, err)
	}
	rel, err := filepath.Rel(baseAbs, pathAbs)
	if err != nil {
		return "", fmt.Errorf("resolve seed file %q: %w", file, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("seed file %q escapes seed_base_dir %s", file, baseAbs)
	}
	return pathAbs, nil
}

// callAppMCPTool POSTs a single tools/call to an app's MCP endpoint and
// returns the raw JSON-RPC result (or the tool's error). Used by seeding and
// reusable by any server-side caller that needs to drive an in-environment app.
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

func listAppMCPTools(mcpURL, token string) ([]installMCPToolInfo, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	req, err := http.NewRequest("POST", mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Result struct {
			Tools []installMCPToolInfo `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bad MCP response: %s", string(raw))
	}
	if env.Error != nil {
		return nil, fmt.Errorf("tool list error: %s", env.Error.Message)
	}
	if env.Result.Tools == nil {
		return []installMCPToolInfo{}, nil
	}
	return env.Result.Tools, nil
}
