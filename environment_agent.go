package main

// environment_agent.go — spawn a copy of an agent INTO a Environment.
//
// This is the "agent copy evolving in a virtual environment" piece. It mirrors
// the eval runner's core-spawn wiring (eval_runner.go) but, instead of a
// mock gateway, points the agent's mcp_servers at the Environment's REAL in-environment
// sidecars (so it sees real tools with real schemas) and routes the core's
// egress through the Environment edge (so its outbound HTTP is virtualised by the
// same cassette/mock policy as the sidecars). No apteva-core changes: the
// agent runs the exact binary that ships, just pointed at the environment.
//
// The transient agent row (kind='environment_agent') is cloned from a source
// agent's directive/mode/config and deleted on Environment teardown.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EnvironmentAgentSpec configures spawning an agent copy into a Environment.
type EnvironmentAgentSpec struct {
	UserID            int64
	Source            *Agent // clone directive/mode/config/project from this live agent
	DirectiveOverride string // optional: run with a modified directive
	Alias             string // stable environment-local label; defaults to "main"
	ProviderPool      []ProviderInfo
	// StartPaused gates the cloned core at main's first iteration.start. This
	// lets eval runners inject the opening event into main before the initial
	// autonomous no-event loop can race ahead.
	StartPaused bool
}

// EnvironmentAgent is a running agent core living inside a Environment.
type EnvironmentAgent struct {
	AgentID       int64     `json:"agent_id"`
	SourceAgentID int64     `json:"source_agent_id"`
	SourceName    string    `json:"source_name"`
	Alias         string    `json:"alias"`
	Port          int       `json:"port"`
	CreatedAt     time.Time `json:"created_at"`
	APIKey        string    `json:"-"` // core API key — never serialised to clients
	cleanup       func()
}

// Stop tears the environment-agent down (stops the core, deletes the row).
func (wa *EnvironmentAgent) Stop() {
	if wa != nil && wa.cleanup != nil {
		wa.cleanup()
	}
}

// SpawnAgentInEnvironment clones the source agent into the given Environment and boots a
// real apteva-core for it. The caller drives the core via its HTTP API
// (Port + APIKey) — or through the /api/environments/<id>/agent/* proxy.
func (s *Server) SpawnAgentInEnvironment(environment *Environment, spec EnvironmentAgentSpec) (*EnvironmentAgent, error) {
	if environment == nil || spec.Source == nil {
		return nil, fmt.Errorf("environment and source agent required")
	}
	userID := spec.UserID
	src := spec.Source
	alias := strings.TrimSpace(spec.Alias)
	if alias == "" {
		alias = "main"
	}
	if existing := environment.AgentByAlias(alias); existing != nil {
		return nil, fmt.Errorf("environment already has an agent with alias %q", alias)
	}

	// Provider preflight — same fail-fast as the eval runner.
	pool := spec.ProviderPool
	if len(pool) == 0 {
		pool = s.GetProviderPool(userID, src.ProjectID)
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no LLM provider configured — add one in Settings → Providers")
	}

	directive := src.Directive
	if spec.DirectiveOverride != "" {
		directive = spec.DirectiveOverride
	}

	// Transient environment-agent row cloned from the source.
	row, err := s.store.CreateAgent(userID,
		fmt.Sprintf("__environment_%s_%d__", environment.ID, time.Now().UnixNano()),
		directive, src.Mode, src.Config, src.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("create environment agent: %w", err)
	}
	_, _ = s.store.db.Exec(`UPDATE agents SET kind = 'environment_agent' WHERE id = ?`, row.ID)
	wAgent, err := s.store.GetAgentByID(row.ID)
	if err != nil {
		s.store.DeleteAgent(userID, row.ID)
		return nil, fmt.Errorf("reload environment agent: %w", err)
	}
	teardown := func() {
		s.agents.Stop(wAgent.ID)
		s.store.DeleteAgent(userID, wAgent.ID)
	}

	// Point mcp_servers at the Environment apps the source agent can actually
	// use. Real (install-backed) apps are token-protected, so route them through
	// the environment-app gateway which brokers the install token; legacy sandbox
	// apps run tokenless (dev mode) so the agent reaches their MCP directly.
	mcpServers := []any{}
	legacyApps := environment.Apps()
	for _, name := range s.environmentAgentAppMCPNames(environment, src) {
		if _, ok := environment.Install(name); ok {
			mcpServers = append(mcpServers, map[string]any{
				"name":      name,
				"url":       s.environmentAppMCPURL(environment.ID, name),
				"transport": "http",
				"no_spawn":  true,
			})
			continue
		}
		if inst, ok := legacyApps[name]; ok && inst != nil {
			mcpServers = append(mcpServers, map[string]any{
				"name":      name,
				"url":       inst.MCPURL,
				"transport": "http",
				"no_spawn":  true,
			})
		}
	}
	for _, cid := range environment.ConnectionIDs() {
		conn, _, err := s.store.GetConnection(userID, cid)
		if err != nil || conn == nil {
			continue
		}
		mcpServers = append(mcpServers, map[string]any{
			"name":      conn.AppSlug,
			"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d?environment_id=%s", s.port, cid, environment.ID),
			"transport": "http",
			"no_spawn":  true,
		})
	}
	// Carry over the agent's OWN integration connections (its config's
	// /mcp/<id> entries), tagging each URL with ?environment_id so the executor
	// mocks them instead of hitting the real API. The connection is the
	// agent's real one; the environment changes only the executor's behavior.
	if src.Config != "" {
		var srcCfg map[string]any
		if json.Unmarshal([]byte(src.Config), &srcCfg) == nil {
			if existing, ok := srcCfg["mcp_servers"].([]any); ok {
				for _, e := range existing {
					m, ok := e.(map[string]any)
					if !ok {
						continue
					}
					url, _ := m["url"].(string)
					if !strings.Contains(url, "/mcp/") { // connection entries only
						continue
					}
					sep := "?"
					if strings.Contains(url, "?") {
						sep = "&"
					}
					mcpServers = append(mcpServers, map[string]any{
						"name":      m["name"],
						"url":       url + sep + "environment_id=" + environment.ID,
						"transport": "http",
						"no_spawn":  true,
					})
				}
			}
		}
	}
	cfg := map[string]any{
		"directive":             directive,
		"mode":                  wAgent.Mode,
		"mcp_servers":           mcpServers,
		"include_apteva_server": false,
		"include_channels":      false,
	}
	if spec.StartPaused {
		cfg["execution_control"] = map[string]any{
			"mode":        "paused",
			"breakpoints": []string{"iteration.start"},
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	wAgent.Directive = directive
	wAgent.Config = string(cfgJSON)
	_ = s.store.UpdateAgent(wAgent)

	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, src.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	// Route the core's own egress through the Environment edge so any direct
	// outbound HTTP it makes lands on the same cassette/mock policy as the
	// sidecars. LLM hosts are in the edge's default allowlist, so the
	// provider call still reaches its API.
	providerEnv["HTTP_PROXY"] = environment.ProxyURL()
	providerEnv["HTTPS_PROXY"] = environment.ProxyURL()

	if err := s.agents.PreSeedConfig(wAgent.ID, wAgent.Config); err != nil {
		teardown()
		return nil, fmt.Errorf("seed environment agent config: %w", err)
	}
	if err := s.agents.Start(wAgent, providerEnv, s.port, pool, s.instanceSecret); err != nil {
		teardown()
		return nil, fmt.Errorf("spawn environment core: %w", err)
	}
	if !waitForCoreListening(s.agents.GetPort(wAgent.ID), 10*time.Second) {
		teardown()
		return nil, fmt.Errorf("environment core never started listening")
	}
	if err := s.store.UpdateAgent(wAgent); err != nil {
		teardown()
		return nil, fmt.Errorf("persist environment agent runtime state: %w", err)
	}

	wa := &EnvironmentAgent{
		AgentID:       wAgent.ID,
		SourceAgentID: src.ID,
		SourceName:    src.Name,
		Alias:         alias,
		Port:          s.agents.GetPort(wAgent.ID),
		CreatedAt:     time.Now(),
		APIKey:        s.agents.GetCoreAPIKey(wAgent.ID),
		cleanup:       teardown,
	}
	if err := environment.AttachAgent(wa); err != nil {
		wa.Stop()
		return nil, err
	}
	if err := s.installEnvironmentSubscriptionsForAgent(userID, environment, wa); err != nil {
		environment.StopAgent(wa.AgentID)
		return nil, fmt.Errorf("install environment subscriptions: %w", err)
	}
	return wa, nil
}

func (s *Server) environmentAgentAppMCPNames(environment *Environment, src *Agent) []string {
	if environment == nil {
		return nil
	}
	available := map[string]bool{}
	for _, name := range environment.InstallNames() {
		if strings.TrimSpace(name) != "" {
			available[name] = true
		}
	}
	for name := range environment.Apps() {
		if strings.TrimSpace(name) != "" {
			available[name] = true
		}
	}
	if len(available) == 0 {
		return nil
	}

	selected := map[string]bool{}
	if s != nil && s.store != nil && src != nil && src.ID != 0 {
		if names, err := s.AppNamesForAgent(src.ID); err == nil {
			for _, name := range names {
				if available[name] {
					selected[name] = true
				}
			}
		}
	}
	for _, name := range appMCPNamesFromAgentConfig(src, available) {
		selected[name] = true
	}
	if len(selected) == 0 {
		for name := range available {
			selected[name] = true
		}
	}
	return sortedMapKeys(selected)
}

func appMCPNamesFromAgentConfig(agent *Agent, available map[string]bool) []string {
	if agent == nil || strings.TrimSpace(agent.Config) == "" || len(available) == 0 {
		return nil
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(agent.Config), &cfg) != nil {
		return nil
	}
	servers, ok := cfg["mcp_servers"].([]any)
	if !ok {
		return nil
	}
	selected := map[string]bool{}
	for _, entry := range servers {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		url, _ := m["url"].(string)
		name = strings.TrimSpace(name)
		if name == "" || !available[name] {
			continue
		}
		// /mcp/<id> entries are integration connections, not environment app
		// installs. Those are carried over separately below.
		if strings.Contains(url, "/mcp/") {
			continue
		}
		selected[name] = true
	}
	return sortedMapKeys(selected)
}

func sortedMapKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key, ok := range values {
		if ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
