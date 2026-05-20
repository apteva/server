package main

// world_agent.go — spawn a copy of an agent INTO a World.
//
// This is the "agent copy evolving in a virtual world" piece. It mirrors
// the eval runner's core-spawn wiring (eval_runner.go) but, instead of a
// mock gateway, points the agent's mcp_servers at the World's REAL in-world
// sidecars (so it sees real tools with real schemas) and routes the core's
// egress through the World edge (so its outbound HTTP is virtualised by the
// same cassette/mock policy as the sidecars). No apteva-core changes: the
// agent runs the exact binary that ships, just pointed at the world.
//
// The transient agent row (kind='world_agent') is cloned from a source
// agent's directive/mode/config and deleted on World teardown.

import (
	"encoding/json"
	"fmt"
	"time"
)

// WorldAgentSpec configures spawning an agent copy into a World.
type WorldAgentSpec struct {
	UserID            int64
	Source            *Agent // clone directive/mode/config/project from this live agent
	DirectiveOverride string // optional: run with a modified directive
}

// WorldAgent is a running agent core living inside a World.
type WorldAgent struct {
	AgentID int64  `json:"agent_id"`
	Port    int    `json:"port"`
	APIKey  string `json:"-"` // core API key — never serialised to clients
	cleanup func()
}

// Stop tears the world-agent down (stops the core, deletes the row).
func (wa *WorldAgent) Stop() {
	if wa != nil && wa.cleanup != nil {
		wa.cleanup()
	}
}

// SpawnAgentInWorld clones the source agent into the given World and boots a
// real apteva-core for it. The caller drives the core via its HTTP API
// (Port + APIKey) — or through the /api/worlds/<id>/agent/* proxy.
func (s *Server) SpawnAgentInWorld(world *World, spec WorldAgentSpec) (*WorldAgent, error) {
	if world == nil || spec.Source == nil {
		return nil, fmt.Errorf("world and source agent required")
	}
	userID := spec.UserID
	src := spec.Source

	// Provider preflight — same fail-fast as the eval runner.
	pool := s.GetProviderPool(userID, src.ProjectID)
	if len(pool) == 0 {
		return nil, fmt.Errorf("no LLM provider configured — add one in Settings → Providers")
	}

	directive := src.Directive
	if spec.DirectiveOverride != "" {
		directive = spec.DirectiveOverride
	}

	// Transient world-agent row cloned from the source.
	row, err := s.store.CreateAgent(userID,
		fmt.Sprintf("__world_%s_%d__", world.ID, time.Now().UnixNano()),
		directive, src.Mode, src.Config, src.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("create world agent: %w", err)
	}
	_, _ = s.store.db.Exec(`UPDATE agents SET kind = 'world_agent' WHERE id = ?`, row.ID)
	wAgent, err := s.store.GetAgentByID(row.ID)
	if err != nil {
		s.store.DeleteAgent(userID, row.ID)
		return nil, fmt.Errorf("reload world agent: %w", err)
	}
	teardown := func() {
		s.agents.Stop(wAgent.ID)
		s.store.DeleteAgent(userID, wAgent.ID)
	}

	// Point mcp_servers at the World's real in-world sidecars.
	mcpServers := make([]any, 0, len(world.Apps()))
	for name, inst := range world.Apps() {
		mcpServers = append(mcpServers, map[string]any{
			"name":      name,
			"url":       inst.MCPURL,
			"transport": "http",
			"no_spawn":  true,
		})
	}
	cfg := map[string]any{
		"directive":             directive,
		"mode":                  wAgent.Mode,
		"mcp_servers":           mcpServers,
		"include_apteva_server": false,
		"include_channels":      false,
	}
	cfgJSON, _ := json.Marshal(cfg)
	wAgent.Directive = directive
	wAgent.Config = string(cfgJSON)
	_ = s.store.UpdateAgent(wAgent)

	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, src.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	// Route the core's own egress through the World edge so any direct
	// outbound HTTP it makes lands on the same cassette/mock policy as the
	// sidecars. LLM hosts are in the edge's default allowlist, so the
	// provider call still reaches its API.
	providerEnv["HTTP_PROXY"] = world.ProxyURL()
	providerEnv["HTTPS_PROXY"] = world.ProxyURL()

	if err := s.agents.PreSeedConfig(wAgent.ID, wAgent.Config); err != nil {
		teardown()
		return nil, fmt.Errorf("seed world agent config: %w", err)
	}
	if err := s.agents.Start(wAgent, providerEnv, s.port, pool, s.instanceSecret,
		s.getBrowserConfig(userID, defaultProviderForInstance(wAgent), src.ProjectID)); err != nil {
		teardown()
		return nil, fmt.Errorf("spawn world core: %w", err)
	}
	if !waitForCoreListening(s.agents.GetPort(wAgent.ID), 10*time.Second) {
		teardown()
		return nil, fmt.Errorf("world core never started listening")
	}

	wa := &WorldAgent{
		AgentID: wAgent.ID,
		Port:    s.agents.GetPort(wAgent.ID),
		APIKey:  s.agents.GetCoreAPIKey(wAgent.ID),
		cleanup: teardown,
	}
	world.AttachAgent(wa)
	return wa, nil
}
