package main

// environment_agent.go — spawn a copy of an agent INTO a Environment.
//
// This is the "agent copy evolving in a virtual environment" piece. It points
// the agent's mcp_servers at the Environment's real in-environment
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func waitForCoreListening(port int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if isCoreListening(port) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// EnvironmentAgentSpec configures spawning an agent copy into a Environment.
type EnvironmentAgentSpec struct {
	UserID            int64
	Source            *Agent // clone directive/mode/config/project from this live agent
	DirectiveOverride string // optional: run with a modified directive
	Alias             string // stable environment-local label; defaults to "main"
	ProviderPool      []ProviderInfo
	Provider          string
	Model             string
	// StartPaused gates the cloned core at main's first iteration.start. This
	// lets callers inject the opening event into main before the initial
	// autonomous no-event loop can race ahead.
	StartPaused bool
}

// EnvironmentAgent is a running agent core living inside a Environment.
type EnvironmentAgent struct {
	AgentID       int64     `json:"agent_id"`
	SourceAgentID int64     `json:"source_agent_id"`
	SourceName    string    `json:"source_name"`
	Alias         string    `json:"alias"`
	Provider      string    `json:"provider,omitempty"`
	Model         string    `json:"model,omitempty"`
	Port          int       `json:"port"`
	CreatedAt     time.Time `json:"created_at"`
	APIKey        string    `json:"-"` // core API key — never serialised to clients
	cleanup       func()
}

type environmentSourceAgentPolicy struct {
	config                  map[string]any
	mcpServers              map[string]map[string]any
	realtimeMCPs            map[string]bool
	hasRealtimeMCPAllowlist bool
}

func parseEnvironmentSourceAgentPolicy(raw string) environmentSourceAgentPolicy {
	policy := environmentSourceAgentPolicy{
		config:       map[string]any{},
		mcpServers:   map[string]map[string]any{},
		realtimeMCPs: map[string]bool{},
	}
	if json.Unmarshal([]byte(raw), &policy.config) != nil {
		return policy
	}
	if entries, ok := policy.config["mcp_servers"].([]any); ok {
		for _, entry := range entries {
			server, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			name, _ := server["name"].(string)
			name = strings.TrimSpace(name)
			if name != "" {
				policy.mcpServers[name] = server
			}
		}
	}
	rawAllowlist, exists := policy.config["realtime_voice_mcp"]
	policy.hasRealtimeMCPAllowlist = exists
	if entries, ok := rawAllowlist.([]any); ok {
		for _, entry := range entries {
			name, _ := entry.(string)
			name = strings.TrimSpace(name)
			if name != "" {
				policy.realtimeMCPs[name] = true
			}
		}
	}
	return policy
}

func (p environmentSourceAgentPolicy) mcpConfig(name, endpoint string) map[string]any {
	name = strings.TrimSpace(name)
	config := map[string]any{
		"name":      name,
		"url":       endpoint,
		"transport": "http",
		"no_spawn":  true,
	}
	source, attachedToSource := p.mcpServers[name]
	if toolLoading, ok := source["tool_loading"]; ok {
		config["tool_loading"] = toolLoading
	}
	if p.hasRealtimeMCPAllowlist {
		config["no_spawn"] = !p.realtimeMCPs[name]
	} else if attachedToSource {
		noSpawn := false
		if rawNoSpawn, exists := source["no_spawn"]; exists {
			var valid bool
			noSpawn, valid = rawNoSpawn.(bool)
			if !valid {
				noSpawn = true
			}
		}
		config["no_spawn"] = noSpawn
	}
	return config
}

func (p environmentSourceAgentPolicy) copyRealtimeConfig(target map[string]any) {
	for _, key := range []string{"realtime_enabled", "realtime_voice", "realtime_voice_mcp"} {
		if value, ok := p.config[key]; ok {
			target[key] = value
		}
	}
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

	// Fail before spawning when no provider can run the agent.
	pool := spec.ProviderPool
	if len(pool) == 0 {
		pool = s.GetProviderPool(userID, src.ProjectID)
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no LLM provider configured — add one in Settings → Providers")
	}
	requestedProvider, requestedModel := environmentAgentProviderSelection(pool, src.Config, spec.Provider, spec.Model)
	pool, selectedProvider, selectedModel, err := runtimeProviderPool(pool, requestedProvider, requestedModel)
	if err != nil {
		return nil, err
	}

	directive := src.Directive
	if spec.DirectiveOverride != "" {
		directive = spec.DirectiveOverride
	}
	sourcePolicy := parseEnvironmentSourceAgentPolicy(src.Config)

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
			// Per-agent URL so the gateway attributes calls and the
			// sidecar receives an sdk.Caller (a2a etc. require it).
			mcpServers = append(mcpServers, sourcePolicy.mcpConfig(name, s.environmentAgentAppMCPURL(environment.ID, wAgent.ID, name)))
			continue
		}
		if inst, ok := legacyApps[name]; ok && inst != nil {
			mcpServers = append(mcpServers, sourcePolicy.mcpConfig(name, inst.MCPURL))
		}
	}
	for _, cid := range environment.ConnectionIDs() {
		conn, _, err := s.store.GetConnection(userID, cid)
		if err != nil || conn == nil {
			continue
		}
		mcpServers = append(mcpServers, sourcePolicy.mcpConfig(
			conn.AppSlug,
			authorizeMCPURL(fmt.Sprintf("http://127.0.0.1:%s/mcp/connection/%d?environment_id=%s", s.port, cid, environment.ID), s.instanceSecret),
		))
	}
	for _, mcp := range environment.ManagedMCPs() {
		mcpServers = append(mcpServers, sourcePolicy.mcpConfig(mcp.Name, s.runtimeManagedMCPURL(environment.ID, mcp.Token)))
	}
	// Runtime-owned MCP attachments are private endpoints exposed by the
	// orchestrating app (for example a dynamic mock session). The
	// core reaches them only through the server's capability-token gateway.
	for _, attachment := range environment.MCPAttachments() {
		mcpServers = append(mcpServers, sourcePolicy.mcpConfig(attachment.Name, s.runtimeMCPAttachmentURL(environment.ID, attachment.Token)))
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
					if !isEnvironmentConnectionMCPURL(url) {
						continue
					}
					sep := "?"
					if strings.Contains(url, "?") {
						sep = "&"
					}
					name, _ := m["name"].(string)
					mcpServers = append(mcpServers, sourcePolicy.mcpConfig(name, url+sep+"environment_id="+environment.ID))
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
	sourcePolicy.copyRealtimeConfig(cfg)
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

	providerEnv, err := s.environmentAgentProviderEnv(userID, src.ProjectID, environment.ProxyURL())
	if err != nil {
		teardown()
		return nil, err
	}
	// The agent's OPENAI_BASE_URL (from a connection's
	// runtime_config.base_url or a legacy provider blob) names a host the
	// edge has never heard of — its allowlist was built at edge start,
	// before any agent's provider env existed. Admit exactly that host,
	// read from the same rendered env the core boots with, so inference
	// against a configured OpenAI-compatible gateway isn't egress-blocked.
	if host := llmGatewayHost(providerEnv["OPENAI_BASE_URL"]); host != "" {
		environment.Edge().AllowHost(host)
	}

	if err := s.agents.PreSeedConfig(wAgent.ID, wAgent.Config); err != nil {
		teardown()
		return nil, fmt.Errorf("seed environment agent config: %w", err)
	}
	if _, err := s.startManagedAgent(wAgent, providerEnv, pool); err != nil {
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
		Provider:      selectedProvider,
		Model:         selectedModel,
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

// environmentAgentProviderSelection gives explicit spawn parameters highest
// precedence, then inherits the source agent's persisted provider/model pins.
// Stale source provider pins follow ordinary-agent behavior and fall back to
// the first configured text provider; an explicit invalid provider still
// reaches runtimeProviderPool and fails closed.
func environmentAgentProviderSelection(pool []ProviderInfo, sourceConfig, explicitProvider, explicitModel string) (string, string) {
	provider := strings.TrimSpace(explicitProvider)
	if provider == "" {
		provider = effectiveProviderDefault(pool, configuredAgentDefaultProvider(sourceConfig))
	}
	model := strings.TrimSpace(explicitModel)
	if model == "" {
		model = configuredAgentModelOverride(sourceConfig, provider)
	}
	return provider, model
}

// environmentAgentProviderEnv resolves providers through the same dual-read
// path used by ordinary agents. In particular, this includes runtime-backed
// connections after credentials have migrated out of the legacy providers
// table. The environment proxy is additive: it must never replace or hide the
// credentials required to boot the cloned core.
func (s *Server) environmentAgentProviderEnv(userID int64, projectID, proxyURL string) (map[string]string, error) {
	providerEnv, err := s.GetAllProviderEnvVars(userID, projectID)
	if err != nil {
		return nil, fmt.Errorf("load environment agent providers: %w", err)
	}
	providerEnv["HTTP_PROXY"] = proxyURL
	providerEnv["HTTPS_PROXY"] = proxyURL
	return providerEnv, nil
}

func isEnvironmentConnectionMCPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "mcp" {
		return false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && id > 0
}

func runtimeProviderPool(pool []ProviderInfo, provider, model string) ([]ProviderInfo, string, string, error) {
	provider = providerKeyFromName(provider)
	model = strings.TrimSpace(model)
	selected := ProviderInfo{}
	for _, candidate := range pool {
		if !isRealtimeProviderType(candidate.Type) {
			selected = candidate
			break
		}
	}
	if selected.Type == "" {
		return nil, "", "", fmt.Errorf("no text LLM provider configured for this project")
	}
	if provider != "" {
		found := false
		for _, candidate := range pool {
			if providerKeyFromName(candidate.Type) == provider && !isRealtimeProviderType(candidate.Type) {
				selected = candidate
				found = true
				break
			}
		}
		if !found {
			return nil, "", "", fmt.Errorf("LLM provider %q is not configured for this project", provider)
		}
	}
	if model != "" {
		selected.ModelLarge = model
		selected.ModelMedium = model
		selected.ModelSmall = model
	}
	selectedModel := model
	if selectedModel == "" {
		selectedModel = selected.ModelLarge
	}
	selectedPool := []ProviderInfo{selected}
	for _, candidate := range pool {
		if isRealtimeProviderType(candidate.Type) {
			selectedPool = append(selectedPool, candidate)
		}
	}
	return selectedPool, selected.Type, selectedModel, nil
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
