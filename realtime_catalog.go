package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) realtimeCatalogForProvider(providerKey string) *RuntimeRealtimeCatalog {
	if s.catalog == nil {
		return nil
	}
	for _, summary := range s.catalog.List() {
		app := s.catalog.Get(summary.Slug)
		if app != nil && app.Runtime != nil && app.Runtime.ProviderKey == providerKey && app.Runtime.Realtime != nil {
			return app.Runtime.Realtime
		}
	}
	return nil
}

func configuredAgentRealtimeSelection(configJSON string) (string, string) {
	var config struct {
		Provider string `json:"realtime_provider"`
		Model    string `json:"realtime_model"`
	}
	_ = json.Unmarshal([]byte(configJSON), &config)
	return strings.TrimSpace(config.Provider), strings.TrimSpace(config.Model)
}

func resolveRealtimeSelection(pool []ProviderInfo, providerName, model string) (string, string, error) {
	providerName = providerKeyFromName(providerName)
	model = strings.TrimSpace(model)
	var chosen *ProviderInfo
	for i := range pool {
		info := &pool[i]
		if !voiceCapableProvider(*info) {
			continue
		}
		if providerName == "" || providerName == providerKeyFromName(info.Type) {
			chosen = info
			break
		}
	}
	if chosen == nil {
		return "", "", fmt.Errorf("realtime provider %q is not configured", providerName)
	}
	if model == "" {
		model = chosen.ModelLarge
		if !isRealtimeProviderType(chosen.Type) && chosen.Realtime != nil {
			model = chosen.Realtime.DefaultModel
		}
	}
	if chosen.Realtime != nil {
		for _, option := range chosen.Realtime.Models {
			if option.ID != model {
				continue
			}
			if !option.Available {
				return "", "", fmt.Errorf("realtime model %q is unavailable in the integration catalog", model)
			}
			return providerKeyFromName(chosen.Type), model, nil
		}
		return "", "", fmt.Errorf("realtime model %q is not listed for %s", model, chosen.Type)
	}
	if model != chosen.ModelLarge && model != chosen.ModelMedium && model != chosen.ModelSmall {
		return "", "", fmt.Errorf("realtime model %q is not configured for %s", model, chosen.Type)
	}
	return providerKeyFromName(chosen.Type), model, nil
}

func applyRealtimeSelection(providers []map[string]any, selectedProvider, selectedModel string) {
	integration := strings.TrimSuffix(selectedProvider, "-realtime")
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		models := map[string]string{"large": selectedModel, "medium": selectedModel, "small": selectedModel}
		if isRealtimeProviderType(name) {
			// Shared integration aliases select the same internal transport.
			selected := name == selectedProvider || name == selectedProvider+"-realtime"
			provider["default"] = selected
			if selected {
				provider["models"] = models
			}
		} else if name == integration {
			// Keep text models and the text provider's default independent.
			provider["realtime_models"] = models
		}
	}
}

func validateRealtimeProviderModelMaps(pool []ProviderInfo, providers []map[string]any) error {
	for _, provider := range providers {
		value := provider["realtime_execution"]
		if value == nil {
			continue
		}
		var execution struct {
			Mode     string `json:"mode"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		raw, err := json.Marshal(value)
		if err != nil || json.Unmarshal(raw, &execution) != nil {
			return fmt.Errorf("invalid realtime_execution")
		}
		if execution.Mode != "" && execution.Mode != "thread" && execution.Mode != "provider" {
			return fmt.Errorf("invalid realtime execution mode %q", execution.Mode)
		}
		if execution.Model != "" && execution.Model != "large" && execution.Model != "medium" && execution.Model != "small" {
			return fmt.Errorf("realtime execution model must be a Core model tier")
		}
		if execution.Provider != "" {
			found := false
			for _, info := range pool {
				if info.Type == execution.Provider && !isRealtimeProviderType(info.Type) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("execution provider %q is not configured", execution.Provider)
			}
			name, _ := provider["name"].(string)
			if execution.Mode == "provider" && execution.Provider != strings.TrimSuffix(name, "-realtime") {
				return fmt.Errorf("provider execution requires the same API integration")
			}
		}
	}
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		field := "models"
		if !isRealtimeProviderType(name) {
			if _, exists := provider["realtime_models"]; !exists {
				continue
			}
			field = "realtime_models"
		}
		var models map[string]string
		encoded, err := json.Marshal(provider[field])
		if err != nil || json.Unmarshal(encoded, &models) != nil {
			return fmt.Errorf("invalid realtime models for %s", name)
		}
		for _, tier := range []string{"large", "medium", "small"} {
			if field == "realtime_models" && tier != "large" && models[tier] == "" {
				continue
			}
			if strings.TrimSpace(models[tier]) == "" {
				return fmt.Errorf("missing %s realtime model for %s", tier, name)
			}
			if _, _, err := resolveRealtimeSelection(pool, name, models[tier]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRealtimeVoice(pool []ProviderInfo, providerName, voice string) error {
	return validateRealtimeModelVoice(pool, providerName, "", voice)
}

func validateRealtimeModelVoice(pool []ProviderInfo, providerName, model, voice string) error {
	for _, info := range pool {
		if providerKeyFromName(info.Type) != providerName || info.Realtime == nil {
			continue
		}
		voices := info.Realtime.Voices
		for _, option := range info.Realtime.Models {
			if option.ID == model && len(option.Voices) > 0 {
				voices = option.Voices
			}
		}
		if len(voices) == 0 {
			return nil
		}
		for _, allowed := range voices {
			if voice == allowed {
				return nil
			}
		}
		return fmt.Errorf("voice %q is not available for %s", voice, providerName)
	}
	return nil
}

func voiceCapableProvider(info ProviderInfo) bool {
	return isRealtimeProviderType(info.Type) || info.Realtime != nil
}

func realtimeDefaultVoice(info ProviderInfo, model string) string {
	if info.Realtime != nil {
		for _, option := range info.Realtime.Models {
			if option.ID == model && option.DefaultVoice != "" {
				return option.DefaultVoice
			}
		}
	}
	return info.RealtimeVoice
}

// Narrow dashboard edits must not erase independently configured voice options.
func mergeSavedRealtimeProviders(configJSON string, requested []map[string]any) []map[string]any {
	var saved struct {
		Providers []map[string]any `json:"providers"`
	}
	_ = json.Unmarshal([]byte(configJSON), &saved)
	merged := make(map[string]map[string]any)
	var names []string
	for _, provider := range saved.Providers {
		name, _ := provider["name"].(string)
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name}
		for _, field := range []string{"realtime_models", "realtime_execution", "realtime_voice"} {
			if value, exists := provider[field]; exists {
				entry[field] = value
			}
		}
		merged[name] = entry
		names = append(names, name)
	}
	for _, provider := range requested {
		name, _ := provider["name"].(string)
		if merged[name] == nil {
			merged[name] = map[string]any{}
			names = append(names, name)
		}
		for key, value := range provider {
			merged[name][key] = value
		}
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, merged[name])
	}
	return out
}

// Old installations kept provider options only in Core's config.json. Import
// those as a fallback; explicit Server-owned options take precedence.
func realtimeConfigFromDisk(configJSON string, disk map[string]any) string {
	var config map[string]any
	_ = json.Unmarshal([]byte(configJSON), &config)
	if config == nil {
		config = map[string]any{}
	}
	if _, exists := config["realtime_voice"]; !exists && disk["realtime_voice"] != nil {
		config["realtime_voice"] = disk["realtime_voice"]
	}
	raw, _ := json.Marshal(map[string]any{"providers": disk["providers"]})
	var saved struct {
		Providers []map[string]any `json:"providers"`
	}
	encoded, _ := json.Marshal(config["providers"])
	_ = json.Unmarshal(encoded, &saved.Providers)
	config["providers"] = mergeSavedRealtimeProviders(string(raw), saved.Providers)
	out, _ := json.Marshal(config)
	return string(out)
}
func (s *Server) savedRealtimeConfig(inst *Agent) string {
	var disk map[string]any
	raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(inst.ID), "config.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &disk)
	}
	return realtimeConfigFromDisk(inst.Config, disk)
}

func applyRealtimeVoice(providers []map[string]any, selectedProvider, voice string) {
	integration := strings.TrimSuffix(selectedProvider, "-realtime")
	for _, provider := range providers {
		if provider["name"] == selectedProvider || provider["name"] == integration {
			provider["realtime_voice"] = voice
		}
	}
}
