package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed project-presets/*.json
var projectPresetFiles embed.FS

type ProjectPresetAgent struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Directive   string   `json:"directive"`
	Mode        string   `json:"mode"`
	Unconscious bool     `json:"unconscious,omitempty"`
	Apps        []string `json:"apps,omitempty"`
}

type ProjectPreset struct {
	ID          string               `json:"id"`
	Category    string               `json:"category"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Match       []string             `json:"match,omitempty"`
	Agents      []ProjectPresetAgent `json:"agents"`
	Dashboard   []string             `json:"dashboard,omitempty"`
}

type projectPresetFile struct {
	SchemaVersion int             `json:"schema_version"`
	Presets       []ProjectPreset `json:"presets"`
}

type projectPresetCatalog struct {
	Presets []ProjectPreset
	ByID    map[string]ProjectPreset
}

var (
	projectPresetCatalogOnce sync.Once
	loadedProjectPresets     projectPresetCatalog
	loadedProjectPresetsErr  error
)

func loadProjectPresetCatalog() (projectPresetCatalog, error) {
	projectPresetCatalogOnce.Do(func() {
		loadedProjectPresets.ByID = map[string]ProjectPreset{}
		entries, err := fs.Glob(projectPresetFiles, "project-presets/*.json")
		if err != nil {
			loadedProjectPresetsErr = err
			return
		}
		sort.Strings(entries)
		for _, name := range entries {
			raw, err := projectPresetFiles.ReadFile(name)
			if err != nil {
				loadedProjectPresetsErr = err
				return
			}
			var file projectPresetFile
			if err := json.Unmarshal(raw, &file); err != nil {
				loadedProjectPresetsErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			if file.SchemaVersion != 1 {
				loadedProjectPresetsErr = fmt.Errorf("%s: unsupported schema_version %d", name, file.SchemaVersion)
				return
			}
			for _, preset := range file.Presets {
				if err := validateProjectPreset(preset); err != nil {
					loadedProjectPresetsErr = fmt.Errorf("%s: %w", name, err)
					return
				}
				if _, exists := loadedProjectPresets.ByID[preset.ID]; exists {
					loadedProjectPresetsErr = fmt.Errorf("duplicate preset id %q", preset.ID)
					return
				}
				loadedProjectPresets.ByID[preset.ID] = preset
				loadedProjectPresets.Presets = append(loadedProjectPresets.Presets, preset)
			}
		}
		sort.Slice(loadedProjectPresets.Presets, func(i, j int) bool {
			if loadedProjectPresets.Presets[i].Category == loadedProjectPresets.Presets[j].Category {
				return loadedProjectPresets.Presets[i].Name < loadedProjectPresets.Presets[j].Name
			}
			return loadedProjectPresets.Presets[i].Category < loadedProjectPresets.Presets[j].Category
		})
	})
	return loadedProjectPresets, loadedProjectPresetsErr
}

func validateProjectPreset(preset ProjectPreset) error {
	if !validPresetIdentifier(preset.ID) || preset.Name == "" {
		return fmt.Errorf("invalid preset id or name %q", preset.ID)
	}
	switch preset.Category {
	case "personal", "work", "development", "business":
	default:
		return fmt.Errorf("preset %q has invalid category %q", preset.ID, preset.Category)
	}
	seenAgents := map[string]bool{}
	for _, agent := range preset.Agents {
		if !validPresetIdentifier(agent.Key) || seenAgents[agent.Key] || agent.Name == "" || agent.Directive == "" {
			return fmt.Errorf("preset %q has invalid agent %q", preset.ID, agent.Key)
		}
		seenAgents[agent.Key] = true
		seenApps := map[string]bool{}
		for _, app := range agent.Apps {
			if !validPresetIdentifier(app) || seenApps[app] {
				return fmt.Errorf("preset %q agent %q has invalid or duplicate app %q", preset.ID, agent.Key, app)
			}
			seenApps[app] = true
		}
	}
	return nil
}

func validPresetIdentifier(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func (s *Server) handleProjectPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		http.Error(w, "load project presets", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"schema_version": 1, "presets": catalog.Presets})
}

type ProjectPresetPreviewRequest struct {
	PresetID    string `json:"preset_id,omitempty"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
}

type ProjectPresetAppPreview struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	InstallID int64  `json:"install_id,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

type ProjectPresetAgentPreview struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	Directive     string   `json:"directive"`
	Mode          string   `json:"mode"`
	Unconscious   bool     `json:"unconscious"`
	Apps          []string `json:"apps,omitempty"`
	AppInstallIDs []int64  `json:"app_install_ids,omitempty"`
}

type ProjectPresetPreview struct {
	Preset     ProjectPreset               `json:"preset"`
	Planner    string                      `json:"planner"`
	Confidence float64                     `json:"confidence"`
	Project    map[string]string           `json:"project"`
	Apps       []ProjectPresetAppPreview   `json:"apps"`
	Agents     []ProjectPresetAgentPreview `json:"agents"`
	Layout     []dashboardWidgetInstance   `json:"layout"`
	Warnings   []string                    `json:"warnings"`
	NextSteps  []string                    `json:"next_steps,omitempty"`
}

type projectPresetPlanChoice struct {
	PresetID   string  `json:"preset_id"`
	Confidence float64 `json:"confidence"`
}

type projectPresetPlannerFunc func(context.Context, int64, string, string, []ProjectPreset) (projectPresetPlanChoice, error)

func (s *Server) handleProjectPresetPreview(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectEditor); !ok {
		return
	}
	var body ProjectPresetPreviewRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	preview, err := s.compileProjectPresetPreview(r.Context(), getUserID(r), projectID, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, preview)
}

func (s *Server) compileProjectPresetPreview(ctx context.Context, userID int64, projectID string, request ProjectPresetPreviewRequest) (*ProjectPresetPreview, error) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		return nil, err
	}
	description := cleanPresetText(request.Description, 4000)
	preset, planner, confidence, err := s.selectProjectPreset(ctx, userID, projectID, request.PresetID, request.Category, description, catalog)
	if err != nil {
		return nil, err
	}
	if description == "" {
		return nil, errors.New("description is required")
	}
	project, err := s.store.GetProjectAny(projectID)
	if err != nil {
		return nil, errors.New("project not found")
	}
	templateValues := map[string]string{"description": description}

	visibleApps, err := s.visibleProjectPresetApps(projectID)
	if err != nil {
		return nil, err
	}
	presetAppNames := projectPresetApps(preset)
	appPreviews := make([]ProjectPresetAppPreview, 0, len(presetAppNames))
	warnings := []string{}
	for _, name := range presetAppNames {
		app := visibleApps[name]
		entry := ProjectPresetAppPreview{Name: name}
		if app.InstallID > 0 {
			entry.Installed, entry.InstallID, entry.Scope = true, app.InstallID, app.Scope
		} else {
			warnings = append(warnings, fmt.Sprintf("%s is assigned by the preset but not installed for this project", name))
		}
		appPreviews = append(appPreviews, entry)
	}

	agents := make([]ProjectPresetAgentPreview, 0, len(preset.Agents))
	for _, spec := range preset.Agents {
		agent := ProjectPresetAgentPreview{
			Key: spec.Key, Name: expandPresetTemplate(spec.Name, templateValues),
			Directive: expandPresetTemplate(spec.Directive, templateValues),
			Mode:      spec.Mode, Unconscious: spec.Unconscious,
		}
		for _, name := range spec.Apps {
			if app := visibleApps[name]; app.InstallID > 0 {
				agent.Apps = append(agent.Apps, name)
				agent.AppInstallIDs = append(agent.AppInstallIDs, app.InstallID)
			}
		}
		agents = append(agents, agent)
	}

	layout, layoutWarnings := s.compileProjectPresetLayout(projectID, preset.Dashboard)
	warnings = append(warnings, layoutWarnings...)
	return &ProjectPresetPreview{
		Preset: preset, Planner: planner, Confidence: confidence,
		Project: map[string]string{"name": project.Name, "description": description, "color": project.Color},
		Apps:    appPreviews, Agents: agents, Layout: layout, Warnings: warnings,
		NextSteps: []string{"Review app access before enabling external actions.", "Add or remove dashboard widgets at any time.", "Create durable tasks only when real work is requested."},
	}, nil
}

func projectPresetApps(preset ProjectPreset) []string {
	seen := map[string]bool{}
	apps := []string{}
	for _, agent := range preset.Agents {
		for _, app := range agent.Apps {
			if seen[app] {
				continue
			}
			seen[app] = true
			apps = append(apps, app)
		}
	}
	return apps
}

func (s *Server) selectProjectPreset(ctx context.Context, userID int64, projectID, requested, category, purpose string, catalog projectPresetCatalog) (ProjectPreset, string, float64, error) {
	if requested != "" {
		preset, ok := catalog.ByID[requested]
		if !ok {
			return ProjectPreset{}, "", 0, fmt.Errorf("unknown preset %q", requested)
		}
		return preset, "selected", 1, nil
	}
	candidates := catalog.Presets
	if category != "" {
		if category != "personal" && category != "work" && category != "development" && category != "business" {
			return ProjectPreset{}, "", 0, fmt.Errorf("unknown preset category %q", category)
		}
		candidates = nil
		for _, preset := range catalog.Presets {
			if preset.Category == category {
				candidates = append(candidates, preset)
			}
		}
	}
	if purpose != "" {
		planner := s.projectPresetPlanner
		if planner == nil {
			planner = s.planProjectPresetWithProvider
		}
		planCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		choice, err := planner(planCtx, userID, projectID, purpose, candidates)
		cancel()
		if err == nil {
			if preset, ok := catalog.ByID[choice.PresetID]; ok && (category == "" || preset.Category == category) {
				return preset, "meta", clampPresetConfidence(choice.Confidence), nil
			}
		}
	}
	preset, confidence := deterministicProjectPreset(category, purpose, candidates)
	return preset, "deterministic", confidence, nil
}

func deterministicProjectPreset(category, purpose string, presets []ProjectPreset) (ProjectPreset, float64) {
	text := normalizedPresetMatchText(category + " " + purpose)
	bestScore := -1
	best := ProjectPreset{}
	for _, preset := range presets {
		score := 0
		if category != "" && category == preset.Category {
			score += 3
		}
		for _, term := range append([]string{preset.Name}, preset.Match...) {
			term = normalizedPresetMatchText(term)
			if term != "" && strings.Contains(text, term) {
				score += 4 + strings.Count(term, " ")
			}
		}
		if score > bestScore {
			bestScore, best = score, preset
		}
	}
	if best.ID == "" {
		for _, preset := range presets {
			if preset.ID == "personal-assistant" {
				return preset, 0.2
			}
		}
	}
	confidence := 0.35 + float64(bestScore)*0.06
	return best, clampPresetConfidence(confidence)
}

func normalizedPresetMatchText(value string) string {
	value = strings.ToLower(value)
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func clampPresetConfidence(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

type visibleProjectPresetApp struct {
	InstallID int64
	Scope     string
}

func (s *Server) visibleProjectPresetApps(projectID string) (map[string]visibleProjectPresetApp, error) {
	rows, err := s.store.db.Query(`
		SELECT a.name, i.id, COALESCE(i.project_id,'')
		FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE i.status='running' AND (COALESCE(i.project_id,'')='' OR i.project_id=?)
		ORDER BY a.name, CASE WHEN i.project_id=? THEN 0 ELSE 1 END, i.id`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]visibleProjectPresetApp{}
	for rows.Next() {
		var name, installProject string
		var installID int64
		if err := rows.Scan(&name, &installID, &installProject); err != nil {
			return nil, err
		}
		if _, exists := result[name]; exists {
			continue
		}
		scope := "global"
		if installProject != "" {
			scope = "project"
		}
		result[name] = visibleProjectPresetApp{InstallID: installID, Scope: scope}
	}
	return result, rows.Err()
}

func (s *Server) compileProjectPresetLayout(projectID string, requested []string) ([]dashboardWidgetInstance, []string) {
	definitions := append([]dashboardWidgetDefinition(nil), dashboardHomeBuiltins...)
	definitions = append(definitions, s.nativeDashboardWidgetDefinitions(projectID)...)
	byComponent := map[string]dashboardWidgetDefinition{}
	for _, definition := range definitions {
		byComponent[definition.Component] = definition
	}
	layout := []dashboardWidgetInstance{}
	warnings := []string{}
	seen := map[string]bool{}
	for _, component := range requested {
		if seen[component] {
			continue
		}
		definition, ok := byComponent[component]
		if !ok {
			if !strings.HasPrefix(component, "native:") {
				warnings = append(warnings, fmt.Sprintf("dashboard widget %s is unavailable until its app is installed", component))
			}
			continue
		}
		seen[component] = true
		layout = append(layout, dashboardWidgetInstance{
			ID: "preset:" + component, Component: component,
			Size: definition.DefaultSize, Settings: definition.DefaultSettings,
		})
	}
	return layout, warnings
}

type ProjectPresetApplyRequest struct {
	PresetID    string `json:"preset_id"`
	Description string `json:"description"`
}

func (s *Server) handleProjectPresetApply(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectEditor); !ok {
		return
	}
	var body ProjectPresetApplyRequest
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PresetID == "" {
		http.Error(w, "preset_id is required", http.StatusBadRequest)
		return
	}
	preview, err := s.compileProjectPresetPreview(r.Context(), getUserID(r), projectID, ProjectPresetPreviewRequest{
		PresetID: body.PresetID, Description: body.Description,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProjectAny(projectID)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err := s.store.UpdateProjectAny(projectID, preview.Project["name"], preview.Project["description"], project.Color); err != nil {
		http.Error(w, "update project", http.StatusInternalServerError)
		return
	}
	warnings := append([]string(nil), preview.Warnings...)
	raw, _ := json.Marshal(preview.Layout)
	if _, _, err := s.store.PatchUserUILayoutSurface(getUserID(r), projectID, "dashboard.home", raw, nil); err != nil {
		warnings = append(warnings, "dashboard layout was not applied: "+err.Error())
	}

	created := []Agent{}
	existing := []Agent{}
	for _, agent := range preview.Agents {
		if current, ok := s.findPresetAgentByName(projectID, agent.Name); ok {
			existing = append(existing, current)
			continue
		}
		payload := map[string]any{
			"name": agent.Name, "directive": agent.Directive, "mode": agent.Mode,
			"project_id": projectID, "start": true, "unconscious": agent.Unconscious,
			"bound_app_install_ids": agent.AppInstallIDs,
		}
		raw, _ := json.Marshal(payload)
		request := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-User-ID", r.Header.Get("X-User-ID"))
		request.Header.Set("X-User-Name", r.Header.Get("X-User-Name"))
		recorder := httptest.NewRecorder()
		s.handleCreateInstance(recorder, request)
		if recorder.Code < 200 || recorder.Code >= 300 {
			warnings = append(warnings, fmt.Sprintf("%s was not created: %s", agent.Name, strings.TrimSpace(recorder.Body.String())))
			continue
		}
		var result Agent
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.ID == 0 {
			warnings = append(warnings, fmt.Sprintf("%s was created but its result could not be read", agent.Name))
			continue
		}
		var creationNotice struct {
			Warning string `json:"warning"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &creationNotice) == nil && creationNotice.Warning != "" {
			warnings = append(warnings, agent.Name+": "+creationNotice.Warning)
		}
		created = append(created, result)
	}
	writeJSON(w, map[string]any{
		"status": "applied", "project_id": projectID, "preset_id": body.PresetID,
		"created_agents": created, "existing_agents": existing, "warnings": warnings,
	})
}

func (s *Server) findPresetAgentByName(projectID, name string) (Agent, bool) {
	var agent Agent
	var createdAt string
	err := s.store.db.QueryRow(`
		SELECT id,user_id,name,directive,mode,config,port,pid,core_api_key,status,project_id,kind,created_at
		FROM agents WHERE project_id=? AND name=? AND kind='user' ORDER BY id LIMIT 1`, projectID, name).
		Scan(&agent.ID, &agent.UserID, &agent.Name, &agent.Directive, &agent.Mode, &agent.Config,
			&agent.Port, &agent.Pid, &agent.CoreAPIKey, &agent.Status, &agent.ProjectID, &agent.Kind, &createdAt)
	if err != nil {
		return Agent{}, false
	}
	agent.CreatedAt, _ = parseTime(createdAt)
	return agent, true
}

func expandPresetTemplate(template string, answers map[string]string) string {
	result := template
	for key, value := range answers {
		result = strings.ReplaceAll(result, "{{"+key+"}}", strings.TrimSpace(value))
	}
	for {
		start := strings.Index(result, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(result[start+2:], "}}")
		if end < 0 {
			break
		}
		result = result[:start] + "not specified" + result[start+2+end+2:]
	}
	return strings.TrimSpace(strings.Join(strings.Fields(result), " "))
}

func cleanPresetText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}
