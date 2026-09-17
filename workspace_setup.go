package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Setup is a resumable workspace operation, also available after onboarding.
// Drafts belong to the caller, never to every member of the project.
type workspaceSetupDraft struct {
	InterfaceLevel string                       `json:"interface_level,omitempty"`
	AgentOverrides []ProjectPresetAgentOverride `json:"agent_overrides,omitempty"`
	Category       string                       `json:"category"`
	PresetID       string                       `json:"preset_id"`
	Description    string                       `json:"description"`
	Mode           string                       `json:"mode"`
}

func (s *Server) handleWorkspaceSetupSession(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectEditor); !ok {
		return
	}
	key := fmt.Sprintf("workspace_setup:%d:%s", getUserID(r), projectID)
	draft := workspaceSetupDraft{Mode: "choice"}
	if r.Method == http.MethodGet {
		var raw string
		err := s.store.db.QueryRow("SELECT value FROM server_settings WHERE key=?", key).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Could not load workspace setup", http.StatusInternalServerError)
			return
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &draft); err != nil {
				http.Error(w, "Could not load workspace setup", http.StatusInternalServerError)
				return
			}
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
			http.Error(w, "Invalid setup draft", http.StatusBadRequest)
			return
		}
		if draft.Mode != "choice" && draft.Mode != "browse" && draft.Mode != "manual" && draft.Mode != "ai" && draft.Mode != "scratch" {
			http.Error(w, "Invalid setup mode", http.StatusBadRequest)
			return
		}
		if draft.Category != "" && draft.Category != "personal" && draft.Category != "business" && draft.Category != "work" && draft.Category != "development" {
			http.Error(w, "Invalid setup category", http.StatusBadRequest)
			return
		}
		if draft.InterfaceLevel != "" && !validInterfaceLevel(draft.InterfaceLevel) {
			http.Error(w, "Invalid interface level", http.StatusBadRequest)
			return
		}
		if len(draft.Description) > 12000 {
			http.Error(w, "Description is too long", http.StatusBadRequest)
			return
		}
		if draft.PresetID != "" {
			catalog, err := s.projectPresetCatalog(getUserID(r))
			if err != nil {
				http.Error(w, "Could not load presets", http.StatusInternalServerError)
				return
			}
			if _, ok := catalog.ByID[draft.PresetID]; !ok {
				http.Error(w, "Preset is not available", http.StatusBadRequest)
				return
			}
		}
		raw, _ := json.Marshal(draft)
		if err := s.store.SetSetting(key, string(raw)); err != nil {
			http.Error(w, "Could not save workspace setup", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, draft)
}

// Helper uses exactly the same authorization and provisioning endpoints as UI.
func handleGatewaySetupTool(name string, args map[string]any, defaultProjectID string, api gatewayAPIClient) (any, error) {
	var result any
	if name == "setup_presets_list" {
		err := api.do(http.MethodGet, "/templates", nil, &result)
		return result, err
	}
	projectID, _ := args["project_id"].(string)
	if projectID == "" {
		projectID = defaultProjectID
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	suffix := "preview"
	if name == "setup_apply" {
		suffix = "apply"
	} else if name != "setup_preview" {
		return nil, fmt.Errorf("unknown setup tool %q", name)
	}
	body := map[string]any{}
	for _, key := range []string{"preset_id", "description", "category", "interface_level"} {
		if value, ok := args[key].(string); ok {
			body[key] = value
		}
	}
	err := api.do(http.MethodPost, "/projects/"+url.PathEscape(projectID)+"/setup/"+suffix, body, &result)
	return result, err
}

func applySetupAgentOverrides(agents []ProjectPresetAgentPreview, overrides []ProjectPresetAgentOverride) error {
	seen := map[string]bool{}
	for _, override := range overrides {
		if seen[override.Key] {
			return fmt.Errorf("duplicate agent override %q", override.Key)
		}
		seen[override.Key] = true
		index := -1
		for i := range agents {
			if agents[i].Key == override.Key {
				index = i
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("unknown preset agent %q", override.Key)
		}
		if strings.TrimSpace(override.Name) == "" || len(override.Name) > 200 || strings.TrimSpace(override.Directive) == "" || len(override.Directive) > 16000 || !validAgentMode(override.Mode) {
			return fmt.Errorf("invalid configuration for agent %q", override.Key)
		}
		agents[index].Name = strings.TrimSpace(override.Name)
		agents[index].Directive = override.Directive
		agents[index].Mode = override.Mode
	}
	names := map[string]bool{}
	for _, agent := range agents {
		if names[agent.Name] {
			return fmt.Errorf("agent names must be distinct")
		}
		names[agent.Name] = true
	}
	return nil
}

// Keep Helper's selected preset available for the onboarding review. This is a
// recommendation in the caller's draft, never a change to account preferences.
func (s *Server) rememberOnboardingPreset(userID int64, projectID string, preview *ProjectPresetPreview) error {
	user, err := s.store.GetUserByID(userID)
	if err != nil {
		return err
	}
	if user.OnboardedAt != nil {
		return nil
	}
	key := fmt.Sprintf("workspace_setup:%d:%s", userID, projectID)
	draft := workspaceSetupDraft{Mode: "ai"}
	var raw string
	err = s.store.db.QueryRow("SELECT value FROM server_settings WHERE key=?", key).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &draft); err != nil {
			return err
		}
	}
	draft.PresetID = preview.Preset.ID
	if draft.InterfaceLevel == "" {
		draft.InterfaceLevel = preview.InterfaceLevel
	}
	encoded, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	return s.store.SetSetting(key, string(encoded))
}
