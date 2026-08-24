package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
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
	ID              string                    `json:"id"`
	Kind            string                    `json:"kind,omitempty"`
	Scope           string                    `json:"scope,omitempty"`
	Source          string                    `json:"source,omitempty"`
	SchemaVersion   int                       `json:"schema_version,omitempty"`
	OwnerID         int64                     `json:"owner_id,omitempty"`
	Category        string                    `json:"category"`
	Name            string                    `json:"name"`
	Description     string                    `json:"description"`
	Match           []string                  `json:"match,omitempty"`
	Agents          []ProjectPresetAgent      `json:"agents"`
	Dashboard       []string                  `json:"dashboard,omitempty"`
	DashboardLayout []dashboardWidgetInstance `json:"dashboard_layout,omitempty"`
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

const (
	defaultConversationsApp    = "conversations"
	defaultConversationsWidget = "conversations:inbox-overview"
)

// applyBundledPresetDefaults gives every server-shipped setup the platform's
// durable conversation surface. Keep this normalization limited to embedded
// presets: personal/shared presets remain an exact representation of what the
// operator authored or captured.
func applyBundledPresetDefaults(preset ProjectPreset) ProjectPreset {
	for index := range preset.Agents {
		if !containsString(preset.Agents[index].Apps, defaultConversationsApp) {
			preset.Agents[index].Apps = append(preset.Agents[index].Apps, defaultConversationsApp)
		}
	}

	dashboard := make([]string, 0, len(preset.Dashboard)+1)
	hasConversations := false
	for _, component := range preset.Dashboard {
		if component == "native:inbox" {
			component = defaultConversationsWidget
		}
		if component == defaultConversationsWidget {
			if hasConversations {
				continue
			}
			hasConversations = true
		}
		dashboard = append(dashboard, component)
	}
	if !hasConversations {
		dashboard = append(dashboard, defaultConversationsWidget)
	}
	preset.Dashboard = dashboard
	return preset
}

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
				preset = applyBundledPresetDefaults(preset)
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
		if !validPresetIdentifier(agent.Key) || seenAgents[agent.Key] || agent.Name == "" || agent.Directive == "" || len(agent.Name) > 160 || len(agent.Directive) > 32000 {
			return fmt.Errorf("preset %q has invalid agent %q", preset.ID, agent.Key)
		}
		if agent.Mode != "autonomous" && agent.Mode != "cautious" && agent.Mode != "learn" {
			return fmt.Errorf("preset %q agent %q has invalid mode %q", preset.ID, agent.Key, agent.Mode)
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
	catalog, err := s.projectPresetCatalog(getUserID(r))
	if err != nil {
		http.Error(w, "load project presets", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"schema_version": 2, "presets": catalog.Presets})
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
	catalog, err := s.projectPresetCatalog(userID)
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

	layout, layoutWarnings := s.compileProjectPresetDashboardLayout(projectID, preset)
	warnings = append(warnings, layoutWarnings...)
	return &ProjectPresetPreview{
		Preset: preset, Planner: planner, Confidence: confidence,
		Project: map[string]string{"name": project.Name, "description": description, "color": project.Color},
		Apps:    appPreviews, Agents: agents, Layout: layout, Warnings: warnings,
		NextSteps: []string{"Review app access before enabling external actions.", "Preset Home widgets are added automatically and remain editable.", "Create durable tasks only when real work is requested."},
	}, nil
}

// installMissingPresetApps installs a preset's not-yet-installed apps
// from the registry, so applying a preset is genuinely one click instead
// of "install eight apps by hand first, then apply".
//
// It reuses the requires.apps dependency cascade wholesale by wrapping
// the missing names in a synthetic manifest: the preset's app list IS a
// dependency list, and the cascade already does everything the job
// needs — registry name→manifest resolution, transitive deps in topo
// order (webinars pulls streaming by itself), cycle detection, and
// skip-if-installed.
//
// Every ref is marked Optional with an explicit opt-in binding. That
// combination makes each app resolve independently: one app missing
// from the registry (or failing its build) degrades to a warning while
// the rest still install, instead of the hard-fail a required ref
// produces. A preset must apply with whatever subset exists — that was
// the old behavior, and auto-install must only ever improve on it.
//
// Installs run synchronously (clone/build/health-check inside the
// call), so on return the new installs are status='running' and visible
// to the preview compiler.
func (s *Server) installMissingPresetApps(
	userID int64, projectID string, appNames []string,
	visible map[string]visibleProjectPresetApp,
) []string {
	// No local supervisor means no way to run a sidecar — nothing to
	// gain from resolving the registry, and the resolution itself is a
	// network call. Headless variants and the unit-test fixture both
	// land here.
	if s.localApps == nil {
		return nil
	}
	var missing []string
	for _, name := range appNames {
		if visible[name].InstallID == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	refs := make([]sdk.RequiredAppRef, 0, len(missing))
	bindings := map[string]any{}
	for _, name := range missing {
		refs = append(refs, sdk.RequiredAppRef{Name: name, Optional: true})
		bindings[name] = true
	}
	synthetic := &sdk.Manifest{}
	synthetic.Requires.Apps = refs

	warnings := []string{}
	resolved, err := s.installDependencies(userID, synthetic, projectID, bindings)
	if err != nil {
		// Registry unreachable — nothing installed; every app keeps the
		// classic not-installed warning below.
		log.Printf("[PRESET-INSTALL] project=%s registry resolution failed: %v", projectID, err)
		warnings = append(warnings, "app auto-install skipped: "+err.Error())
	}
	for _, name := range missing {
		if id, ok := resolved[name]; ok && id != 0 {
			log.Printf("[PRESET-INSTALL] project=%s installed %s (install=%d)", projectID, name, id)
			continue
		}
		warnings = append(warnings,
			fmt.Sprintf("%s is assigned by the preset but is not installed and could not be installed from the registry", name))
	}
	return warnings
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
	definitions = append(definitions, s.installedDashboardWidgetDefinitions(projectID, false)...)
	byComponent := map[string]dashboardWidgetDefinition{}
	for _, definition := range definitions {
		byComponent[definition.Component] = definition
	}
	layout := []dashboardWidgetInstance{}
	warnings := []string{}
	seen := map[string]bool{}
	for _, component := range requested {
		if retiredDashboardHomeWidget(component) {
			continue
		}
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

// compileProjectPresetDashboardLayout preserves schema-v2 captured widget
// order, size, and settings. Bundled schema-v1 presets continue through the
// compact component-list compiler above.
func (s *Server) compileProjectPresetDashboardLayout(projectID string, preset ProjectPreset) ([]dashboardWidgetInstance, []string) {
	if len(preset.DashboardLayout) == 0 {
		return s.compileProjectPresetLayout(projectID, preset.Dashboard)
	}
	definitions := append([]dashboardWidgetDefinition(nil), dashboardHomeBuiltins...)
	definitions = append(definitions, s.installedDashboardWidgetDefinitions(projectID, false)...)
	available := map[string]dashboardWidgetDefinition{}
	for _, definition := range definitions {
		available[definition.Component] = definition
	}
	layout := make([]dashboardWidgetInstance, 0, len(preset.DashboardLayout))
	warnings := []string{}
	for index, captured := range preset.DashboardLayout {
		if retiredDashboardHomeWidget(captured.Component) {
			continue
		}
		definition, ok := available[captured.Component]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("dashboard widget %s is unavailable until its app is installed", captured.Component))
			continue
		}
		if captured.Size != "full" {
			captured.Size = "half"
		}
		if captured.ID == "" {
			captured.ID = "captured:" + itoa(int64(index+1))
		}
		// A stable preset-owned id makes reapplication idempotent while still
		// allowing a captured layout to contain multiple instances of the same
		// component with different settings.
		captured.ID = "preset:" + preset.ID + ":" + captured.ID
		if captured.Settings == nil {
			captured.Settings = definition.DefaultSettings
		}
		layout = append(layout, captured)
	}
	return layout, warnings
}

// mergeProjectPresetDashboardLayout adds the preset's available widgets to
// the user's existing Home layout without replacing user choices. The
// revision check prevents a concurrent browser edit from being overwritten;
// conflicts are re-read and merged again.
func (s *Server) mergeProjectPresetDashboardLayout(userID int64, projectID string, preset []dashboardWidgetInstance) error {
	if len(preset) == 0 {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		document, revision := s.store.GetUserUILayoutWithRevision(userID)
		current := resolvedDashboardHomeLayout(document, projectID)
		merged, changed := mergeDashboardWidgetLayouts(current, preset)
		if !changed {
			return nil
		}
		raw, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if _, _, err = s.store.PatchUserUILayoutSurface(userID, projectID, sdk.UIComponentSlotDashboardHome, raw, &revision); err == nil {
			return nil
		} else if !errors.Is(err, errUILayoutConflict) {
			return err
		}
	}
	return errUILayoutConflict
}

func mergeDashboardWidgetLayouts(current, preset []dashboardWidgetInstance) ([]dashboardWidgetInstance, bool) {
	merged := append([]dashboardWidgetInstance(nil), current...)
	components := make(map[string]bool, len(current)+len(preset))
	ids := make(map[string]bool, len(current)+len(preset))
	existingCapturedShapes := make(map[string]int, len(current))
	for _, widget := range current {
		components[widget.Component] = true
		ids[widget.ID] = true
		if !strings.HasPrefix(widget.ID, "preset:") {
			existingCapturedShapes[dashboardWidgetFingerprint(widget)]++
		}
	}
	changed := false
	for _, widget := range preset {
		captured := strings.HasPrefix(widget.ID, "preset:usr-") || strings.HasPrefix(widget.ID, "preset:shared-")
		if widget.Component == "" || retiredDashboardHomeWidget(widget.Component) || (captured && ids[widget.ID]) || (!captured && components[widget.Component]) {
			continue
		}
		if captured {
			fingerprint := dashboardWidgetFingerprint(widget)
			if existingCapturedShapes[fingerprint] > 0 {
				existingCapturedShapes[fingerprint]--
				continue
			}
		}
		widget.ID = availablePresetWidgetID(widget.ID, widget.Component, ids)
		merged = append(merged, widget)
		components[widget.Component] = true
		ids[widget.ID] = true
		changed = true
	}
	return merged, changed
}

func dashboardWidgetFingerprint(widget dashboardWidgetInstance) string {
	settings, _ := json.Marshal(widget.Settings)
	return widget.Component + "\x00" + widget.Size + "\x00" + string(settings)
}

func availablePresetWidgetID(preferred, component string, used map[string]bool) string {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = "preset:" + component
	}
	if !used[base] {
		return base
	}
	for suffix := int64(2); ; suffix++ {
		candidate := base + ":" + itoa(suffix)
		if !used[candidate] {
			return candidate
		}
	}
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

	// Auto-install the preset's missing apps before compiling the
	// preview, so the agents bind the full set instead of whatever
	// happened to be installed. Preview (the read-only endpoint) never
	// does this — installing is a side effect that belongs to apply.
	//
	// Admin-gated because installing apps is an admin capability
	// everywhere else (handleInstallApp requires platform admin); a
	// project editor applying a preset gets the old warn-and-continue
	// behavior rather than a privilege escalation.
	installWarnings := []string{}
	if userID := getUserID(r); s.store.GetPlatformRole(userID) == PlatformAdmin {
		if catalog, err := s.projectPresetCatalog(userID); err == nil {
			if preset, ok := catalog.ByID[body.PresetID]; ok {
				if visible, err := s.visibleProjectPresetApps(projectID); err == nil {
					installWarnings = s.installMissingPresetApps(
						userID, projectID, projectPresetApps(preset), visible)
				}
			}
		}
	}

	preview, err := s.compileProjectPresetPreview(r.Context(), getUserID(r), projectID, ProjectPresetPreviewRequest{
		PresetID: body.PresetID, Description: body.Description,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The preview re-warns generically about anything still missing;
	// the install pass already explains those with the richer "could
	// not be installed from the registry" message, so drop the
	// duplicates and keep the informative one.
	if len(installWarnings) > 0 {
		kept := preview.Warnings[:0]
		for _, warning := range preview.Warnings {
			if !strings.HasSuffix(warning, "not installed for this project") {
				kept = append(kept, warning)
			}
		}
		preview.Warnings = append(kept, installWarnings...)
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
	if err := s.mergeProjectPresetDashboardLayout(getUserID(r), projectID, preview.Layout); err != nil {
		warnings = append(warnings, "dashboard widgets were not applied: "+err.Error())
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
