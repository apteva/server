package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Preferences follow credential ownership: each user can set an account default
// and override it for a project. They are applied only when creating an agent.
func newAgentProviderSettingKey(userID int64, projectID string) string {
	return fmt.Sprintf("new_agent_provider:%d:%s", userID, projectID)
}

type newAgentProviderSettings struct {
	Provider           string   `json:"provider"`
	EffectiveProvider  string   `json:"effective_provider"`
	InheritedProvider  string   `json:"inherited_provider"`
	AvailableProviders []string `json:"available_providers"`
}

func (s *Server) newAgentProviderSettings(userID int64, projectID string) newAgentProviderSettings {
	pool := eligibleProviderPool(s.GetProviderPool(userID, projectID))
	result := newAgentProviderSettings{
		Provider:           s.store.GetSetting(newAgentProviderSettingKey(userID, projectID)),
		AvailableProviders: []string{},
	}
	for _, p := range pool {
		if !isRealtimeProviderType(p.Type) {
			result.AvailableProviders = append(result.AvailableProviders, p.Type)
		}
	}
	if projectID != "" {
		result.InheritedProvider = s.store.GetSetting(newAgentProviderSettingKey(userID, ""))
	}
	// An unavailable project preference falls back to the account preference,
	// then the existing deterministic pool order.
	result.EffectiveProvider = effectiveProviderDefault(pool, result.InheritedProvider)
	for _, name := range result.AvailableProviders {
		if name == result.Provider {
			result.EffectiveProvider = name
			break
		}
	}
	return result
}

func (s *Server) handleNewAgentProviderSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	if projectID != "" {
		need := ProjectViewer
		if r.Method == http.MethodPut {
			need = ProjectEditor
		}
		if _, _, ok := s.requireProjectAccess(w, r, projectID, need); !ok {
			return
		}
	}
	settings := s.newAgentProviderSettings(userID, projectID)
	if r.Method == http.MethodPut {
		var body struct {
			Provider *string `json:"provider"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Provider == nil {
			http.Error(w, "provider is required (empty clears the override)", http.StatusBadRequest)
			return
		}
		provider := strings.TrimSpace(*body.Provider)
		available := provider == ""
		for _, name := range settings.AvailableProviders {
			if provider == name {
				available = true
				break
			}
		}
		if !available {
			http.Error(w, "Provider is not available in this scope", http.StatusBadRequest)
			return
		}
		if err := s.store.SetSetting(newAgentProviderSettingKey(userID, projectID), provider); err != nil {
			http.Error(w, "Could not save default provider", http.StatusInternalServerError)
			return
		}
		settings = s.newAgentProviderSettings(userID, projectID)
	}
	writeJSON(w, settings)
}

func (s *Server) applyNewAgentProviderDefault(userID int64, projectID, configJSON string) string {
	if configuredAgentDefaultProvider(configJSON) != "" {
		return configJSON
	}
	selected := s.newAgentProviderSettings(userID, projectID).EffectiveProvider
	if selected == "" {
		return configJSON
	}
	var config map[string]any
	if json.Unmarshal([]byte(configJSON), &config) != nil || config == nil {
		return configJSON
	}
	config["default_provider"] = selected
	raw, err := json.Marshal(config)
	if err != nil {
		return configJSON
	}
	return string(raw)
}
