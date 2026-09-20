package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
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

type workspaceSetupResultAgent struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Existing bool   `json:"existing,omitempty"`
}

type workspaceSetupResult struct {
	Agents   []workspaceSetupResultAgent `json:"agents"`
	Warnings []string                    `json:"warnings,omitempty"`
}

// workspaceSetupProposal is the authoritative bridge between Helper and the
// onboarding UI. The conversation remains prose; setup_preview writes this
// typed snapshot so the dashboard never has to scrape messages to render the
// workspace being proposed.
type workspaceSetupProposal struct {
	Revision  int64                 `json:"revision"`
	Status    string                `json:"status"`
	UpdatedAt string                `json:"updated_at,omitempty"`
	Preview   *ProjectPresetPreview `json:"preview,omitempty"`
	Result    *workspaceSetupResult `json:"result,omitempty"`
}

func workspaceSetupProposalKey(userID int64, projectID string) string {
	return fmt.Sprintf("workspace_setup_proposal:%d:%s", userID, projectID)
}

func (s *Server) getWorkspaceSetupProposal(userID int64, projectID string) (workspaceSetupProposal, error) {
	proposal := workspaceSetupProposal{Status: "empty"}
	var raw string
	err := s.store.db.QueryRow("SELECT value FROM server_settings WHERE key=?", workspaceSetupProposalKey(userID, projectID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return proposal, nil
	}
	if err != nil {
		return proposal, err
	}
	if err := json.Unmarshal([]byte(raw), &proposal); err != nil {
		return workspaceSetupProposal{}, err
	}
	return proposal, nil
}

func (s *Server) saveWorkspaceSetupProposal(userID int64, projectID, status string, preview *ProjectPresetPreview, result *workspaceSetupResult) error {
	proposal, err := s.getWorkspaceSetupProposal(userID, projectID)
	if err != nil {
		return err
	}
	proposal.Revision = time.Now().UTC().UnixMicro()
	proposal.Status = status
	proposal.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	proposal.Preview = preview
	proposal.Result = result
	raw, err := json.Marshal(proposal)
	if err != nil {
		return err
	}
	return s.store.SetSetting(workspaceSetupProposalKey(userID, projectID), string(raw))
}

func (s *Server) handleWorkspaceSetupProposal(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	proposal, err := s.getWorkspaceSetupProposal(getUserID(r), projectID)
	if err != nil {
		http.Error(w, "Could not load workspace proposal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, proposal)
}

func workspaceSetupStagedAgent(agent Agent) bool {
	var config map[string]any
	if json.Unmarshal([]byte(agent.Config), &config) != nil {
		return false
	}
	staged, _ := config["onboarding_staged"].(bool)
	return staged
}

// handleWorkspaceSetupConfirm activates agents Helper created during the
// onboarding conversation. The agents already exist and are visible in the
// live sidebar; this endpoint only performs the explicit activation step.
func (s *Server) handleWorkspaceSetupConfirm(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectEditor); !ok {
		return
	}
	agents, err := s.store.ListAgentsInProject(projectID)
	if err != nil {
		http.Error(w, "Could not load staged agents", http.StatusInternalServerError)
		return
	}
	staged := make([]Agent, 0, len(agents))
	for _, agent := range agents {
		full, getErr := s.store.GetAgentByID(agent.ID)
		if getErr == nil && workspaceSetupStagedAgent(*full) {
			staged = append(staged, *full)
		}
	}
	warnings := []string{}
	confirmed := make([]Agent, 0, len(staged))
	for _, agent := range staged {
		if agent.Status == "running" || s.agents.IsRunning(agent.ID) {
			current, getErr := s.store.GetAgentByID(agent.ID)
			if getErr == nil {
				var config map[string]any
				if json.Unmarshal([]byte(current.Config), &config) != nil || config == nil {
					config = map[string]any{}
				}
				config["onboarding_staged"] = false
				config["onboarding_confirmed"] = true
				if raw, marshalErr := json.Marshal(config); marshalErr == nil {
					current.Config = string(raw)
					_ = s.store.UpdateAgent(current)
				}
				confirmed = append(confirmed, *current)
			}
			continue
		}
		path := "/instances/" + itoa64(agent.ID) + "/start"
		startRequest := httptest.NewRequest(http.MethodPost, path, nil)
		startRequest.Header.Set("X-User-ID", r.Header.Get("X-User-ID"))
		startRequest.Header.Set("X-User-Name", r.Header.Get("X-User-Name"))
		startResponse := httptest.NewRecorder()
		s.handleStartInstance(startResponse, startRequest)
		if startResponse.Code < 200 || startResponse.Code >= 300 {
			warnings = append(warnings, fmt.Sprintf("%s could not be started: %s", agent.Name, strings.TrimSpace(startResponse.Body.String())))
			continue
		}
		current, getErr := s.store.GetAgentByID(agent.ID)
		if getErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s started but its status could not be read", agent.Name))
			continue
		}
		var config map[string]any
		if json.Unmarshal([]byte(current.Config), &config) != nil || config == nil {
			config = map[string]any{}
		}
		config["onboarding_staged"] = false
		config["onboarding_confirmed"] = true
		if raw, marshalErr := json.Marshal(config); marshalErr == nil {
			current.Config = string(raw)
			if updateErr := s.store.UpdateAgent(current); updateErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s started but could not be finalized: %v", current.Name, updateErr))
				continue
			}
		}
		confirmed = append(confirmed, *current)
	}
	status := "confirmed"
	if len(warnings) > 0 {
		status = "needs_attention"
	}
	writeJSON(w, map[string]any{"status": status, "project_id": projectID, "agents": confirmed, "warnings": warnings})
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
