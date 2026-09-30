package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Empty means provider default; omission/null in the agent patch means inherit.
func validateServiceTier(provider string, supported []string, value any) (string, error) {
	tier, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("service_tier must be a string")
	}
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		return "", nil
	}
	for _, allowed := range supported {
		if tier == allowed {
			return tier, nil
		}
	}
	return "", fmt.Errorf("provider %q does not support service_tier %q", provider, tier)
}

func (s *Server) serviceTiersForProvider(provider string) []string {
	if s.catalog == nil {
		return nil
	}
	for _, item := range s.catalog.List() {
		app := s.catalog.Get(item.Slug)
		if app != nil && app.Runtime != nil && app.Runtime.ProviderKey == provider {
			return app.Runtime.ServiceTiers
		}
	}
	return nil
}

func savedAgentServiceTiers(config string) map[string]string {
	var cfg struct {
		Overrides map[string]string `json:"service_tier_overrides"`
	}
	_ = json.Unmarshal([]byte(config), &cfg)
	if cfg.Overrides == nil {
		return map[string]string{}
	}
	return cfg.Overrides
}

func mergeAgentServiceTiers(saved map[string]string, patch map[string]any, pool []ProviderInfo) error {
	for key, value := range patch {
		name := providerKeyFromName(key)
		if value == nil {
			delete(saved, name)
			continue
		}
		var found *ProviderInfo
		for i := range pool {
			if providerKeyFromName(pool[i].Type) == name {
				found = &pool[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("LLM provider %q is not configured for this agent", key)
		}
		if len(found.ServiceTiers) == 0 {
			return fmt.Errorf("provider %q does not support service tiers", name)
		}
		tier, err := validateServiceTier(name, found.ServiceTiers, value)
		if err != nil {
			return err
		}
		saved[name] = tier
	}
	return nil
}

func applyAgentServiceTiers(providers []map[string]any, pool []ProviderInfo, overrides map[string]string) {
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		value, exists := overrides[name]
		if !exists {
			continue
		}
		for _, info := range pool {
			if providerKeyFromName(info.Type) != name {
				continue
			}
			tier, err := validateServiceTier(name, info.ServiceTiers, value)
			if err != nil {
				continue
			} // Stale saved choices must not prevent agent startup.
			delete(provider, "service_tier")
			if tier != "" {
				provider["service_tier"] = tier
			}
			break
		}
	}
}
