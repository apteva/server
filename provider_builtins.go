package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProviderBuiltin struct {
	Enabled bool           `json:"enabled"`
	Options map[string]any `json:"options,omitempty"`
}

// A non-nil empty options object explicitly replaces inherited options.
func (c ProviderBuiltin) MarshalJSON() ([]byte, error) {
	var options *map[string]any
	if c.Options != nil {
		options = &c.Options
	}
	return json.Marshal(struct {
		Enabled bool            `json:"enabled"`
		Options *map[string]any `json:"options,omitempty"`
	}{c.Enabled, options})
}

type ProviderBuiltins map[string]ProviderBuiltin
type AgentBuiltinOverrides map[string]map[string]*ProviderBuiltin

type BuiltinDescriptor struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Experimental bool           `json:"experimental,omitempty"`
	OptionNames  []string       `json:"option_names,omitempty"`
	Options      map[string]any `json:"options,omitempty"`
}
type builtinCatalog struct {
	Version   int                            `json:"version"`
	Providers map[string][]BuiltinDescriptor `json:"providers"`
}

var builtinDiscoveryCache = struct {
	sync.Mutex
	entries map[string]struct {
		catalog builtinCatalog
		err     error
		expires time.Time
	}
}{entries: make(map[string]struct {
	catalog builtinCatalog
	err     error
	expires time.Time
})}

// --version is understood by older Core binaries too; a discovery probe can
// never accidentally launch a runtime, read credentials or create an agent.
func (s *Server) builtinCommand(ctx context.Context, flag string, input any) ([]byte, error) {
	if s.agents == nil || s.agents.coreCmd == "" {
		return nil, fmt.Errorf("Core is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.agents.coreCmd, "--version", flag)
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		cmd.Stdin = bytes.NewReader(raw)
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Core capability discovery unavailable: %w", err)
	}
	return output, nil
}
func (s *Server) providerBuiltinCatalog() (builtinCatalog, error) {
	if s.agents == nil {
		return builtinCatalog{}, fmt.Errorf("Core unavailable")
	}
	path := s.agents.coreCmd
	if resolved, err := exec.LookPath(path); err == nil {
		path = resolved
	}
	key := path
	if info, err := os.Stat(path); err == nil {
		key = fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
	}
	builtinDiscoveryCache.Lock()
	defer builtinDiscoveryCache.Unlock()
	if entry, ok := builtinDiscoveryCache.entries[key]; ok && time.Now().Before(entry.expires) {
		return entry.catalog, entry.err
	}
	var catalog builtinCatalog
	raw, err := s.builtinCommand(context.Background(), "--builtin-capabilities", nil)
	if err == nil {
		err = json.Unmarshal(raw, &catalog)
	}
	if err != nil || catalog.Version != 1 {
		err = fmt.Errorf("Update Core to configure built-in model capabilities")
	}
	builtinDiscoveryCache.entries[key] = struct {
		catalog builtinCatalog
		err     error
		expires time.Time
	}{catalog, err, time.Now().Add(time.Minute)}
	return catalog, err
}
func (s *Server) builtinDescriptors(provider string) ([]BuiltinDescriptor, string) {
	catalog, err := s.providerBuiltinCatalog()
	if err != nil {
		return nil, err.Error()
	}
	return catalog.Providers[provider], ""
}
func (s *Server) validateProviderBuiltins(ctx context.Context, provider string, configs ProviderBuiltins) (ProviderBuiltins, error) {
	if len(configs) == 0 {
		return configs, nil
	}
	raw, err := s.builtinCommand(ctx, "--validate-builtins", map[string]any{"provider": provider, "builtins": configs})
	if err != nil {
		return nil, err
	}
	var result struct {
		Builtins ProviderBuiltins `json:"builtins"`
		Error    string           `json:"error"`
	}
	if json.Unmarshal(raw, &result) != nil || (result.Builtins == nil && result.Error == "") {
		return nil, fmt.Errorf("update Core to configure built-in model capabilities")
	}
	if result.Error != "" {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return result.Builtins, nil
}
func runtimeBuiltins(state map[string]any) ProviderBuiltins {
	out := ProviderBuiltins{}
	raw, _ := json.Marshal(state["builtins"])
	_ = json.Unmarshal(raw, &out)
	return out
}
func canonicalProviderBuiltin(name string) string {
	switch name {
	case "code_interpreter":
		return "code_execution"
	case "google_search", "web_search_preview":
		return "web_search"
	}
	return name
}
func decodeBuiltin(value any) (*ProviderBuiltin, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("capability must be an object")
	}
	if _, ok := obj["enabled"].(bool); !ok {
		return nil, fmt.Errorf("capability.enabled must be a boolean")
	}
	for key := range obj {
		if key != "enabled" && key != "options" {
			return nil, fmt.Errorf("unknown capability field %q", key)
		}
	}
	if opts, ok := obj["options"]; ok {
		if _, valid := opts.(map[string]any); !valid {
			return nil, fmt.Errorf("capability.options must be an object")
		}
	}
	raw, _ := json.Marshal(obj)
	var cfg ProviderBuiltin
	err := json.Unmarshal(raw, &cfg)
	return &cfg, err
}

// Missing capability keys preserve other settings; null deletes one entry.
// Explicit options replace the entire options object; omission inherits it.
func mergeBuiltinPatch(saved map[string]*ProviderBuiltin, value any) error {
	patch, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("builtins must be an object")
	}
	seen := map[string]bool{}
	for key, value := range patch {
		name := canonicalProviderBuiltin(key)
		if seen[name] {
			return fmt.Errorf("duplicate builtin aliases for %s", name)
		}
		seen[name] = true
		if value == nil {
			delete(saved, name)
			continue
		}
		cfg, err := decodeBuiltin(value)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		saved[name] = cfg
	}
	return nil
}
func savedAgentBuiltins(config string) AgentBuiltinOverrides {
	var cfg struct {
		Overrides AgentBuiltinOverrides `json:"builtin_overrides"`
		Providers []map[string]any      `json:"providers"`
	}
	_ = json.Unmarshal([]byte(config), &cfg)
	out := AgentBuiltinOverrides{}
	// Existing direct provider configuration remains an explicit agent choice.
	for _, p := range cfg.Providers {
		name, _ := p["name"].(string)
		builtins := runtimeBuiltins(p)
		if len(builtins) > 0 {
			out[name] = map[string]*ProviderBuiltin{}
			for k, v := range builtins {
				copy := v
				out[name][k] = &copy
			}
		}
	}
	for provider, entries := range cfg.Overrides {
		if out[provider] == nil {
			out[provider] = map[string]*ProviderBuiltin{}
		}
		for name, value := range entries {
			out[provider][name] = value
		}
	}
	return out
}

// Present legacy agent image settings as explicit choices in the editor.
// Boot keeps the legacy wire format until the user edits capabilities.
func editableAgentBuiltins(config string) AgentBuiltinOverrides {
	out := savedAgentBuiltins(config)
	var saved struct {
		Providers []map[string]any `json:"providers"`
	}
	_ = json.Unmarshal([]byte(config), &saved)
	for _, p := range saved.Providers {
		name, _ := p["name"].(string)
		legacy, ok := p["image_generation"].(map[string]any)
		if !ok {
			continue
		}
		if out[name] == nil {
			out[name] = map[string]*ProviderBuiltin{}
		}
		if _, exists := out[name]["image_generation"]; exists {
			continue
		}
		enabled, _ := legacy["enabled"].(bool)
		options := map[string]any{}
		for key, value := range legacy {
			if key != "enabled" {
				options[key] = value
			}
		}
		out[name]["image_generation"] = &ProviderBuiltin{Enabled: enabled, Options: options}
	}
	return out
}
func removeLegacyAgentImages(cfg map[string]any) {
	if providers, ok := cfg["providers"].([]any); ok {
		for _, p := range providers {
			if obj, ok := p.(map[string]any); ok {
				delete(obj, "image_generation")
			}
		}
	}
}

func effectiveProviderBuiltins(base ProviderBuiltins, overrides map[string]*ProviderBuiltin) ProviderBuiltins {
	out := ProviderBuiltins{}
	for k, v := range base {
		out[k] = v
	}
	for name, cfg := range overrides {
		if cfg == nil {
			continue
		}
		resolved := *cfg
		if resolved.Options == nil {
			resolved.Options = out[name].Options
		}
		out[name] = resolved
	}
	return out
}
func applyAgentBuiltins(providers []map[string]any, overrides AgentBuiltinOverrides) {
	for _, p := range providers {
		name, _ := p["name"].(string)
		cfg := effectiveProviderBuiltins(runtimeBuiltins(p), overrides[name])
		if len(cfg) > 0 {
			p["builtins"] = cfg
		}
	}
}
func (s *Server) mergeAgentBuiltins(ctx context.Context, saved AgentBuiltinOverrides, patch any, pool []ProviderInfo) error {
	providers, ok := patch.(map[string]any)
	if !ok {
		return fmt.Errorf("builtin_overrides must be an object")
	}
	for key, value := range providers {
		name := providerKeyFromName(key)
		if value == nil {
			delete(saved, name)
			continue
		}
		var base *ProviderInfo
		for i := range pool {
			if providerKeyFromName(pool[i].Type) == name {
				base = &pool[i]
				break
			}
		}
		if base == nil {
			return fmt.Errorf("provider %q is not configured for this agent", key)
		}
		if saved[name] == nil {
			saved[name] = map[string]*ProviderBuiltin{}
		}
		if err := mergeBuiltinPatch(saved[name], value); err != nil {
			return err
		}
		if _, err := s.validateProviderBuiltins(ctx, name, effectiveProviderBuiltins(base.Builtins, saved[name])); err != nil {
			return err
		}
	}
	return nil
}

// Clear legacy inline builtins once migrated so resetting an override cannot
// resurrect an older value after restart.
func persistAgentBuiltins(cfg map[string]any, overrides AgentBuiltinOverrides) {
	cfg["builtin_overrides"] = overrides
	if providers, ok := cfg["providers"].([]any); ok {
		for _, p := range providers {
			if obj, ok := p.(map[string]any); ok {
				delete(obj, "builtins")
			}
		}
	}
}
func (s *Server) prepareAgentBuiltins(ctx context.Context, config string, pool []ProviderInfo) (string, error) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("invalid agent config")
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	overrides := savedAgentBuiltins(config)
	if providers, ok := cfg["providers"].([]any); ok {
		for _, raw := range providers {
			if provider, ok := raw.(map[string]any); ok {
				if builtins, exists := provider["builtins"]; exists {
					name, _ := provider["name"].(string)
					if err := s.mergeAgentBuiltins(ctx, overrides, map[string]any{name: builtins}, pool); err != nil {
						return "", err
					}
				}
			}
		}
	}
	if raw, exists := cfg["builtin_overrides"]; exists {
		if err := s.mergeAgentBuiltins(ctx, overrides, raw, pool); err != nil {
			return "", err
		}
	}
	for name, entries := range overrides {
		base := ProviderBuiltins{}
		found := false
		for _, p := range pool {
			if providerKeyFromName(p.Type) == name {
				base = p.Builtins
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("provider %q is not configured", name)
		}
		if _, err := s.validateProviderBuiltins(ctx, name, effectiveProviderBuiltins(base, entries)); err != nil {
			return "", err
		}
	}
	if len(overrides) > 0 {
		persistAgentBuiltins(cfg, overrides)
	}
	raw, _ := json.Marshal(cfg)
	return string(raw), nil
}

// Check the live process as well as the installed binary: a rollout may leave
// an older Core running. It must not silently ignore the new configuration.
func (s *Server) liveBuiltinConfig(ctx context.Context, inst *Agent) (map[string]any, error) {
	var current map[string]any
	if err := s.behaviorCoreJSON(ctx, inst, http.MethodGet, "/config", nil, &current); err != nil {
		return nil, err
	}
	if _, ok := current["builtin_capabilities"]; !ok {
		return nil, fmt.Errorf("restart this agent with the updated Core to apply model capabilities")
	}
	return current, nil
}

// Preserve legacy per-agent images when a provider/model edit rehydrates the
// pool. A supplied legacy value still takes precedence; generic builtins win
// over both in Core.
func preserveAgentLegacyImages(providers, requested []map[string]any, config string) {
	var saved struct {
		Providers []map[string]any `json:"providers"`
	}
	_ = json.Unmarshal([]byte(config), &saved)
	for _, p := range providers {
		explicit := false
		for _, r := range requested {
			if r["name"] == p["name"] {
				_, explicit = r["image_generation"]
				break
			}
		}
		if explicit {
			continue
		}
		for _, old := range saved.Providers {
			if old["name"] == p["name"] {
				if value, ok := old["image_generation"]; ok {
					p["image_generation"] = value
				}
			}
		}
	}
}

// Defaults are persisted first. Every running agent, including Helper, uses
// the same resolver as boot; failures are reported so retry/restart is clear.
func (s *Server) reconcileBuiltinDefaults(ctx context.Context, userID int64, projectID, provider string) []string {
	agents, err := s.store.ListAgents(userID, projectID)
	if err != nil {
		return []string{"Could not list affected agents"}
	}
	if helper, err := s.store.GetPlatformHelper(userID); err == nil && (projectID == "" || helper.ProjectID == projectID) {
		agents = append(agents, *helper)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var failures []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, row := range agents {
		if s.agents.GetPort(row.ID) == 0 {
			continue
		}
		wg.Add(1)
		go func(row Agent) {
			defer wg.Done()
			err := func() error {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return ctx.Err()
				}
				unlock := s.lockAgentConfig(row.ID)
				defer unlock()
				if err := ctx.Err(); err != nil {
					return err
				}
				inst, err := s.store.GetAgent(userID, row.ID)
				if err != nil {
					return err
				}
				configs := buildAgentCoreProviderConfigs(s.GetProviderPool(userID, inst.ProjectID), inst.Config)
				found := false
				for _, p := range configs {
					if p["name"] == provider {
						found = true
					}
				}
				if !found || s.agents.GetPort(inst.ID) == 0 {
					return nil
				}
				current, err := s.liveBuiltinConfig(ctx, inst)
				if err != nil {
					return err
				}
				raw, _ := json.Marshal(map[string]any{"providers": configs, "mcp_servers": current["mcp_servers"]})
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				resp, err := s.coreDoWithBootWaitContext(callCtx, inst.ID, http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/config", s.agents.GetPort(inst.ID)), raw, s.agents.GetCoreAPIKey(inst.ID), http.Header{"Content-Type": []string{"application/json"}})
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
				if readErr != nil {
					return readErr
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return fmt.Errorf("Core rejected update (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
				}
				return nil
			}()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", row.Name, err))
				mu.Unlock()
			}
		}(row)
	}
	wg.Wait()
	sort.Strings(failures)
	return failures
}

type AgentBuiltinSummary struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func populateAgentBuiltinSummary(inst *Agent, pool []ProviderInfo) {
	for _, p := range buildAgentCoreProviderConfigs(pool, inst.Config) {
		name, _ := p["name"].(string)
		if strings.HasSuffix(name, "-realtime") {
			continue
		}
		effective := runtimeBuiltins(p)
		if legacy, ok := p["builtin_tools"].([]string); ok {
			for _, tool := range legacy {
				k := canonicalProviderBuiltin(tool)
				if _, exists := effective[k]; !exists && k != "image_generation" {
					effective[k] = ProviderBuiltin{Enabled: true}
				}
			}
		}
		if legacy, ok := p["image_generation"].(map[string]any); ok {
			if _, exists := effective["image_generation"]; !exists {
				enabled, _ := legacy["enabled"].(bool)
				effective["image_generation"] = ProviderBuiltin{Enabled: enabled}
			}
		}
		for k, v := range effective {
			if v.Enabled {
				inst.Builtins = append(inst.Builtins, AgentBuiltinSummary{Name: k, Provider: name})
			}
		}
	}
	sort.Slice(inst.Builtins, func(i, j int) bool {
		return inst.Builtins[i].Provider+inst.Builtins[i].Name < inst.Builtins[j].Provider+inst.Builtins[j].Name
	})
}
