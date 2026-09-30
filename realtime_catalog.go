package main

import (
	"encoding/json"
	"fmt"
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
		if !isRealtimeProviderType(info.Type) {
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
	}
	if chosen.Realtime != nil {
		for _, option := range chosen.Realtime.Models {
			if option.ID != model {
				continue
			}
			if !option.Available {
				return "", "", fmt.Errorf("realtime model %q requires newer Core support", model)
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
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		if !isRealtimeProviderType(name) {
			continue
		}
		selected := providerKeyFromName(name) == selectedProvider
		provider["default"] = selected
		if selected {
			provider["models"] = map[string]string{
				"large": selectedModel, "medium": selectedModel, "small": selectedModel,
			}
		}
	}
}

func validateRealtimeProviderModelMaps(pool []ProviderInfo, providers []map[string]any) error {
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		if !isRealtimeProviderType(name) {
			continue
		}
		var models map[string]string
		encoded, err := json.Marshal(provider["models"])
		if err != nil || json.Unmarshal(encoded, &models) != nil {
			return fmt.Errorf("invalid realtime models for %s", name)
		}
		for _, tier := range []string{"large", "medium", "small"} {
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
	for _, info := range pool {
		if providerKeyFromName(info.Type) != providerName || info.Realtime == nil || len(info.Realtime.Voices) == 0 {
			continue
		}
		for _, allowed := range info.Realtime.Voices {
			if voice == allowed {
				return nil
			}
		}
		return fmt.Errorf("voice %q is not available for %s", voice, providerName)
	}
	return nil
}
