package main

// platform_agent.go owns the Apteva dashboard helper.
//
// The helper is a real apteva-core process spawned by the same
// AgentManager that handles user agents. It lives as an agents row
// with kind='platform_helper', filtered out of the operator's
// dashboard listings.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// isCoreListening dials the agent's allocated port to confirm the
// spawned core is accepting connections before we dispatch.
func isCoreListening(port int) bool {
	if port == 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// bootMetaAgents brings up the meta-agent for every user with at
// least one LLM provider configured. It is opt-in at server boot via
// APTEVA_BOOT_META_AGENTS=1; default behavior is lazy start from the
// helper chat path so normal restarts don't wake platform helpers just
// to keep them warm. Failures are logged but never fatal.
//
// Users without a provider configured at boot time get their
// helper spawned lazily on their first helper request once they add a provider.
func (s *Server) bootMetaAgents() {
	// Brief delay so the HTTP listener is definitely accepting
	// connections before we start spawning cores (the spawned cores
	// connect back to apteva-server for telemetry).
	time.Sleep(500 * time.Millisecond)
	users, err := s.store.ListUsers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[boot] list users for meta-agent boot: %v\n", err)
		return
	}
	for _, u := range users {
		pool := s.GetProviderPool(u.ID, "")
		if len(pool) == 0 {
			// Skip — lazy start handles this user when they add a
			// provider and request the helper.
			continue
		}
		if _, err := s.ensureMetaAgentRunning(u.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[boot] meta-agent for user=%d: %v\n", u.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[boot] meta-agent up for user=%d\n", u.ID)
	}
}

// ensureEnvironmentMCPOnHelper keeps the platform helper's required MCP
// surfaces enabled. The apteva-server and channel MCPs are system MCPs
// injected by AgentManager.Start; the environment MCP is an explicit HTTP
// MCP entry. Idempotent. Mutates helper.Config in place; the caller persists
// it (UpdateAgent) and Start merges it into the core's config.
func (s *Server) ensureEnvironmentMCPOnHelper(helper *Agent) {
	var cfg map[string]any
	if helper.Config != "" {
		_ = json.Unmarshal([]byte(helper.Config), &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	servers, _ := cfg["mcp_servers"].([]any)
	environmentURL := fmt.Sprintf("http://127.0.0.1:%s/api/environment-mcp", s.port)
	cleaned := make([]any, 0, len(servers)+1)
	hasEnvironment := false
	for _, e := range servers {
		if m, ok := e.(map[string]any); ok {
			name, _ := m["name"].(string)
			url, _ := m["url"].(string)
			if name == "worlds" || strings.Contains(url, "/api/world-mcp") {
				continue
			}
			if name == "environments" || url == environmentURL {
				hasEnvironment = true
			}
		}
		cleaned = append(cleaned, e)
	}
	if !hasEnvironment {
		cleaned = append(cleaned, map[string]any{
			"name":      "environments",
			"url":       environmentURL,
			"transport": "http",
			"no_spawn":  true,
		})
	}
	cfg["include_apteva_server"] = true
	cfg["include_channels"] = true
	cfg["execution_control"] = map[string]any{
		"mode":        "paused",
		"breakpoints": []string{"iteration.start"},
	}
	cfg["mcp_servers"] = cleaned
	if out, err := json.Marshal(cfg); err == nil {
		helper.Config = string(out)
	}
}

func helperHasRequiredSystemMCPs(helper *Agent) bool {
	if helper == nil {
		return false
	}
	var cfg map[string]any
	if helper.Config != "" {
		_ = json.Unmarshal([]byte(helper.Config), &cfg)
	}
	if cfg == nil {
		return false
	}
	includeGateway, okGateway := cfg["include_apteva_server"].(bool)
	includeChannels, okChannels := cfg["include_channels"].(bool)
	// Ordinary agents default to no platform gateway; the helper must
	// carry an explicit true written by ensurePlatformAgentRuntime.
	if !okGateway {
		includeGateway = false
	}
	if !okChannels {
		includeChannels = true
	}
	return includeGateway && includeChannels
}

func helperHasRequiredRuntimeConfig(helper *Agent) bool {
	if !helperHasRequiredSystemMCPs(helper) {
		return false
	}
	var cfg map[string]any
	if helper.Config != "" {
		_ = json.Unmarshal([]byte(helper.Config), &cfg)
	}
	if cfg == nil {
		return false
	}
	execControl, _ := cfg["execution_control"].(map[string]any)
	mode, _ := execControl["mode"].(string)
	hasIterationStart := false
	switch breakpoints := execControl["breakpoints"].(type) {
	case []any:
		for _, bp := range breakpoints {
			if s, _ := bp.(string); s == "iteration.start" {
				hasIterationStart = true
				break
			}
		}
	case []string:
		for _, bp := range breakpoints {
			if bp == "iteration.start" {
				hasIterationStart = true
				break
			}
		}
	}
	return mode == "paused" && hasIterationStart
}

// ensureMetaAgentRunning makes sure the user's platform_helper
// agent exists in the DB and its core process is running. Returns
// the agent struct ready to dispatch to.
//
// First-call latency: ~2-3s (spawn + listener wait). Subsequent
// calls in the same server lifetime are no-ops.
func (s *Server) ensureMetaAgentRunning(userID int64) (*Agent, error) {
	helper, err := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("ensure platform helper row: %w", err)
	}
	// Already running? Done.
	if s.agents.IsRunning(helper.ID) {
		needsRestart := !helperHasRequiredRuntimeConfig(helper)
		s.ensureEnvironmentMCPOnHelper(helper)
		_ = s.store.UpdateAgent(helper)
		if needsRestart {
			log.Printf("[PLATFORM-HELPER] restarting helper agent=%d to apply required runtime config", helper.ID)
			s.agents.Stop(helper.ID)
		} else {
			_ = s.refreshPlatformHelperDirective(helper)
			return helper, nil
		}
	}
	if s.agents.IsRunning(helper.ID) {
		_ = s.refreshPlatformHelperDirective(helper)
		return helper, nil
	}
	// Cold start. Needs the user's LLM provider pool to make calls.
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, "")
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID, "")
	if len(pool) == 0 {
		return nil, errors.New("no LLM provider configured - add one in Settings > Providers to enable the helper")
	}
	// Give the meta-agent the Apteva server gateway, channels, and
	// Environment control tools before Start so core merges them into
	// config.json.
	s.ensureEnvironmentMCPOnHelper(helper)
	if err := s.agents.Start(helper, providerEnv, s.port, pool, s.instanceSecret); err != nil {
		return nil, fmt.Errorf("start meta-agent: %w", err)
	}
	// Persist new port + pid + status so future restarts pick it up.
	s.store.UpdateAgent(helper)

	// Wait for the core to be listening on its allocated port. Reuse
	// the dial pattern from AgentManager.Start's healthcheck goroutine,
	// but synchronously so we don't dispatch into a not-yet-ready
	// process.
	port := s.agents.GetPort(helper.ID)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if isCoreListening(port) {
			return helper, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, errors.New("meta-agent core failed to listen within 8s — check apteva-server logs")
}

func (s *Server) refreshPlatformHelperDirective(helper *Agent) error {
	port := s.agents.GetPort(helper.ID)
	if port == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"directive": platformHelperSystemPrompt})
	url := fmt.Sprintf("http://127.0.0.1:%d/config", port)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := s.agents.GetCoreAPIKey(helper.ID); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("refresh helper directive http %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// handlePlatformHelper exposes the current user's platform helper as a
// sanitized chat target. It does not include the helper in normal agent
// listings; callers opt in via this endpoint.
func (s *Server) handlePlatformHelper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	helper, err := s.ensureMetaAgentRunning(getUserID(r))
	if err != nil {
		http.Error(w, "platform helper: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if s.agents.IsRunning(helper.ID) {
		helper.Status = "running"
	} else {
		helper.Status = "stopped"
	}
	helper.Name = "Apteva Helper"
	helper.Directive = "Platform assistant for dashboard help, agent design, and quick agent creation."
	helper.Kind = "platform_helper"
	writeJSON(w, helper)
}

const platformHelperSystemPrompt = `You are Apteva Helper, the platform assistant for the Apteva dashboard.

Help the operator understand the current page, design agents, create and manage agents, choose apps, integrations, and MCP servers, and inspect recent agent activity. Be concise and practical. When an answer is for the Apteva operator channel, plain assistant text and thoughts are not visible to the user; call channels_send with channel="current" or channel="apteva" and complete text for visible messages. If you promised tool work, continue after the send result and then send another channels_send message with the outcome. When the operator asks you to create or manage agents, ask briefly for missing details, then use the apteva-server MCP tools such as agents_create, agents_list, agents_start, agents_stop, agents_delete, agents_update, mcp_servers_list, and agent_list_activity when appropriate.`

// ─── Core-process HTTP helpers ─────────────────────────────────────

// postCoreEvent POSTs a user message to an apteva-core's /event
// endpoint. threadID can be a fresh id; core lazy-spawns the thread.
// The core auth uses APTEVA_API_KEY (the per-instance token set at
// spawn time) via the Authorization header.
func postCoreEvent(ctx context.Context, port int, apiKey, threadID, message string) error {
	body, _ := json.Marshal(map[string]any{
		"thread_id": threadID,
		"message":   message,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("core /event http %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}
