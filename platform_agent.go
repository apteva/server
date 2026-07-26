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
	"sort"
	"strings"
	"time"
)

const helperGlobalMCPServerIDsKey = "helper_global_mcp_server_ids"

var reservedPlatformHelperMCPNames = map[string]bool{
	"apteva-server":   true,
	"channels":        true,
	"apteva-channels": true,
	"agent-output":    true,
	"environments":    true,
	"worlds":          true,
}

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

func helperConfigMap(helper *Agent) map[string]any {
	cfg := map[string]any{}
	if helper != nil && strings.TrimSpace(helper.Config) != "" {
		_ = json.Unmarshal([]byte(helper.Config), &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg
}

func helperSelectedGlobalMCPServerIDs(helper *Agent) []int64 {
	cfg := helperConfigMap(helper)
	raw, _ := cfg[helperGlobalMCPServerIDsKey].([]any)
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(raw))
	for _, value := range raw {
		var id int64
		switch typed := value.(type) {
		case float64:
			id = int64(typed)
		case int64:
			id = typed
		case int:
			id = int64(typed)
		}
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func setHelperSelectedGlobalMCPServerIDs(helper *Agent, ids []int64) {
	cfg := helperConfigMap(helper)
	seen := map[int64]bool{}
	normalized := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	cfg[helperGlobalMCPServerIDsKey] = normalized
	if out, err := json.Marshal(cfg); err == nil {
		helper.Config = string(out)
	}
}

func (s *Server) platformHelperMCPConfig(record *MCPServerRecord) (map[string]any, error) {
	if record == nil {
		return nil, errors.New("MCP server not found")
	}
	if strings.TrimSpace(record.ProjectID) != "" {
		return nil, fmt.Errorf("MCP server %d is project-scoped; Helper accepts global apps and integrations only", record.ID)
	}
	name := strings.TrimSpace(record.Name)
	if name == "" || reservedPlatformHelperMCPNames[name] {
		return nil, fmt.Errorf("MCP server %d has a reserved or empty name", record.ID)
	}
	switch record.Source {
	case "app":
		if strings.TrimSpace(record.URL) == "" {
			return nil, fmt.Errorf("global app MCP server %d has no URL", record.ID)
		}
		return map[string]any{
			"name":      name,
			"transport": "http",
			"url":       record.URL,
		}, nil
	case "local":
		if record.ConnectionID <= 0 {
			return nil, fmt.Errorf("global integration MCP server %d has no connection", record.ID)
		}
		return map[string]any{
			"name":      name,
			"transport": "http",
			"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", s.port, record.ID),
		}, nil
	case "remote":
		if strings.TrimSpace(record.URL) == "" {
			return nil, fmt.Errorf("global integration MCP server %d has no URL", record.ID)
		}
		return map[string]any{
			"name":      name,
			"transport": "http",
			"url":       record.URL,
		}, nil
	default:
		return nil, fmt.Errorf("MCP server %d is not a global app or integration", record.ID)
	}
}

// resolvePlatformHelperMCPs turns persisted MCP row IDs into the canonical
// Core config entries. strict=true is used for operator writes and rejects the
// whole request on invalid input. Startup reconciliation is non-strict so a
// deleted/uninstalled MCP is pruned without preventing Helper from starting.
func (s *Server) resolvePlatformHelperMCPs(userID int64, ids []int64, strict bool) ([]int64, []any, error) {
	validIDs := make([]int64, 0, len(ids))
	servers := make([]any, 0, len(ids))
	seenIDs := map[int64]bool{}
	seenNames := map[string]bool{}
	for _, id := range ids {
		if id <= 0 || seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		record, _, err := s.store.GetMCPServer(userID, id)
		if err != nil {
			if strict {
				return nil, nil, fmt.Errorf("MCP server %d was not found", id)
			}
			continue
		}
		config, err := s.platformHelperMCPConfig(record)
		if err != nil {
			if strict {
				return nil, nil, err
			}
			continue
		}
		name, _ := config["name"].(string)
		if seenNames[name] {
			if strict {
				return nil, nil, fmt.Errorf("multiple selected MCP servers use the name %q", name)
			}
			continue
		}
		seenNames[name] = true
		validIDs = append(validIDs, id)
		servers = append(servers, config)
	}
	sort.Slice(validIDs, func(i, j int) bool { return validIDs[i] < validIDs[j] })
	sort.Slice(servers, func(i, j int) bool {
		left, _ := servers[i].(map[string]any)["name"].(string)
		right, _ := servers[j].(map[string]any)["name"].(string)
		return left < right
	})
	return validIDs, servers, nil
}

// ensurePlatformHelperRuntimeConfig is the authoritative capability compiler
// for the hidden Helper. It deliberately rebuilds the optional MCP list from
// server-owned global row IDs, so a stale/project-scoped config cannot survive
// a restart or a deleted integration.
func (s *Server) ensurePlatformHelperRuntimeConfig(helper *Agent) (bool, error) {
	before := helper.Config
	ids := helperSelectedGlobalMCPServerIDs(helper)
	validIDs, selected, err := s.resolvePlatformHelperMCPs(helper.UserID, ids, false)
	if err != nil {
		return false, err
	}
	cfg := helperConfigMap(helper)
	cfg[helperGlobalMCPServerIDsKey] = validIDs
	cfg["mcp_servers"] = selected
	if out, err := json.Marshal(cfg); err == nil {
		helper.Config = string(out)
	} else {
		return false, err
	}
	s.ensureEnvironmentMCPOnHelper(helper)
	return helper.Config != before, nil
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
	wasRunning := s.agents.IsRunning(helper.ID)
	needsRestart := wasRunning && !helperHasRequiredRuntimeConfig(helper)
	runtimeChanged, err := s.ensurePlatformHelperRuntimeConfig(helper)
	if err != nil {
		return nil, fmt.Errorf("configure platform helper capabilities: %w", err)
	}
	if runtimeChanged {
		if err := s.store.UpdateAgent(helper); err != nil {
			return nil, fmt.Errorf("persist platform helper capabilities: %w", err)
		}
	}
	// Already running? Done.
	if wasRunning {
		if needsRestart {
			log.Printf("[PLATFORM-HELPER] restarting helper agent=%d to apply required runtime config", helper.ID)
			s.agents.Stop(helper.ID)
		} else {
			if runtimeChanged {
				if err := s.applyPlatformHelperMCPConfig(helper); err != nil {
					return nil, fmt.Errorf("apply platform helper capabilities: %w", err)
				}
				if _, err := s.resetPlatformHelperConversationThreads(helper); err != nil {
					return nil, fmt.Errorf("refresh platform helper conversation capabilities: %w", err)
				}
			}
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
	if err := s.syncStoppedPlatformHelperMCPConfig(helper); err != nil {
		return nil, fmt.Errorf("persist platform helper runtime config: %w", err)
	}
	if _, err := s.startManagedAgent(helper, providerEnv, pool); err != nil {
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

func helperConfiguredMCPServers(helper *Agent) []any {
	cfg := helperConfigMap(helper)
	servers, _ := cfg["mcp_servers"].([]any)
	return servers
}

func (s *Server) syncStoppedPlatformHelperMCPConfig(helper *Agent) error {
	if s.agents == nil {
		return errors.New("agent manager unavailable")
	}
	servers := helperConfiguredMCPServers(helper)
	return s.writeStoppedConfigAtomic(helper.ID, func(cfg map[string]any) error {
		cfg["mcp_servers"] = servers
		return nil
	})
}

// applyPlatformHelperMCPConfig reconciles optional Helper capabilities without
// routing through the public agent-config handler. It preserves the live,
// dynamically-addressed system MCP entries and replaces everything else with
// the server-validated global app/integration list plus environments.
func (s *Server) applyPlatformHelperMCPConfig(helper *Agent) error {
	port := s.agents.GetPort(helper.ID)
	if port == 0 {
		return s.syncStoppedPlatformHelperMCPConfig(helper)
	}
	coreKey := s.agents.GetCoreAPIKey(helper.ID)
	configURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
	req, err := http.NewRequest(http.MethodGet, configURL, nil)
	if err != nil {
		return err
	}
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("read helper config: HTTP %d", resp.StatusCode)
	}
	var live map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
		return err
	}
	next := make([]any, 0, len(helperConfiguredMCPServers(helper))+2)
	if existing, _ := live["mcp_servers"].([]any); len(existing) > 0 {
		for _, raw := range existing {
			entry, _ := raw.(map[string]any)
			name, _ := entry["name"].(string)
			if name == "apteva-server" || isServerOwnedOutputMCP(name) {
				next = append(next, entry)
			}
		}
	}
	next = append(next, helperConfiguredMCPServers(helper)...)
	body, _ := json.Marshal(map[string]any{"mcp_servers": next})
	req, err = http.NewRequest(http.MethodPut, configURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err = (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("update helper config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (s *Server) resetPlatformHelperConversationThreads(helper *Agent) (int, error) {
	resolver := &serverResolver{srv: s}
	inst, err := resolver.OwnedInstance(helper.UserID, helper.ID)
	if err != nil {
		return 0, err
	}
	threadIDs, err := resolver.ListThreadIDs(inst)
	if err != nil {
		return 0, err
	}
	reset := 0
	for _, threadID := range threadIDs {
		if !strings.HasPrefix(threadID, "chat-conv-") {
			continue
		}
		if err := resolver.KillThread(inst, threadID); err != nil {
			return reset, err
		}
		reset++
	}
	return reset, nil
}

type platformHelperCapabilitiesResponse struct {
	SelectedMCPServerIDs []int64 `json:"selected_mcp_server_ids"`
	Applied              bool    `json:"applied"`
	ResetThreads         int     `json:"reset_threads,omitempty"`
}

func (s *Server) platformHelperCapabilitiesApplied(helper *Agent, validIDs []int64) bool {
	if !s.agents.IsRunning(helper.ID) {
		// The DB selection is authoritative while stopped and is compiled into
		// config.json immediately before the next Helper start.
		return true
	}
	expectedHelper := *helper
	setHelperSelectedGlobalMCPServerIDs(&expectedHelper, validIDs)
	if _, err := s.ensurePlatformHelperRuntimeConfig(&expectedHelper); err != nil {
		return false
	}
	expected := map[string]bool{}
	for _, raw := range helperConfiguredMCPServers(&expectedHelper) {
		entry, _ := raw.(map[string]any)
		if name, _ := entry["name"].(string); name != "" {
			expected[name] = true
		}
	}
	resolver := &serverResolver{srv: s}
	inst, err := resolver.OwnedInstance(helper.UserID, helper.ID)
	if err != nil {
		return false
	}
	names, err := resolver.ListMCPNames(inst)
	if err != nil {
		return false
	}
	actual := map[string]bool{}
	for _, name := range names {
		switch name {
		case "apteva-server", "channels", "apteva-channels", "agent-output", "apteva-agent-output":
			continue
		}
		actual[name] = true
	}
	if len(actual) != len(expected) {
		return false
	}
	for name := range expected {
		if !actual[name] {
			return false
		}
	}
	return true
}

// GET/PUT /api/platform/helper/capabilities
//
// This is intentionally narrower than the generic agent-config endpoint:
// callers select canonical MCP row IDs and the server accepts global
// app/integration rows only. Mandatory Helper system MCPs are never writable.
func (s *Server) handlePlatformHelperCapabilities(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	helper, err := s.store.GetOrCreatePlatformHelper(userID, platformHelperSystemPrompt)
	if err != nil {
		http.Error(w, "platform helper: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		validIDs, _, err := s.resolvePlatformHelperMCPs(
			userID,
			helperSelectedGlobalMCPServerIDs(helper),
			false,
		)
		if err != nil {
			http.Error(w, "platform helper capabilities: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, platformHelperCapabilitiesResponse{
			SelectedMCPServerIDs: validIDs,
			Applied:              s.platformHelperCapabilitiesApplied(helper, validIDs),
		})
	case http.MethodPut:
		var body struct {
			MCPServerIDs []int64 `json:"mcp_server_ids"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		validIDs, _, err := s.resolvePlatformHelperMCPs(userID, body.MCPServerIDs, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		setHelperSelectedGlobalMCPServerIDs(helper, validIDs)
		if _, err := s.ensurePlatformHelperRuntimeConfig(helper); err != nil {
			http.Error(w, "configure platform helper capabilities: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.UpdateAgent(helper); err != nil {
			http.Error(w, "persist platform helper capabilities", http.StatusInternalServerError)
			return
		}
		applied := true
		resetThreads := 0
		if s.agents.IsRunning(helper.ID) {
			if err := s.applyPlatformHelperMCPConfig(helper); err != nil {
				log.Printf("[PLATFORM-HELPER] apply capabilities helper=%d: %v", helper.ID, err)
				applied = false
			} else if count, err := s.resetPlatformHelperConversationThreads(helper); err != nil {
				log.Printf("[PLATFORM-HELPER] reset conversation threads helper=%d: %v", helper.ID, err)
				applied = false
			} else {
				resetThreads = count
			}
		} else if err := s.syncStoppedPlatformHelperMCPConfig(helper); err != nil {
			log.Printf("[PLATFORM-HELPER] persist stopped capabilities helper=%d: %v", helper.ID, err)
			applied = false
		}
		writeJSON(w, platformHelperCapabilitiesResponse{
			SelectedMCPServerIDs: helperSelectedGlobalMCPServerIDs(helper),
			Applied:              applied,
			ResetThreads:         resetThreads,
		})
	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
	}
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

Help the operator understand the current page, design agents, create and manage agents, choose apps, integrations, and MCP servers, and inspect recent agent activity. Be concise and practical. User-facing dashboard conversations have their own durable reply capability and perform available control-plane mutations directly. Main has no internal chat-reply tool; when main receives an action-required request from a conversation, perform the durable work and return its result with the core send tool to that originating conversation. When the operator asks you to create or manage agents, ask briefly for missing details, then use the apteva-server MCP tools such as agents_create, agents_list, agents_start, agents_stop, agents_delete, agents_update, mcp_servers_list, and agent_list_activity when appropriate.`

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
