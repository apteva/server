package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

type dashboardWidgetInstance struct {
	AgentID   int64                     `json:"agent_id,omitempty"`
	Setup     []ProjectPresetConnection `json:"setup,omitempty"`
	ID        string                    `json:"id"`
	Component string                    `json:"component"`
	Size      string                    `json:"size"`
	Settings  map[string]any            `json:"settings,omitempty"`
}

type dashboardWidgetNativeRenderer struct {
	Schema string `json:"schema"`
	Entry  string `json:"entry"`
}

type dashboardWidgetDefinition struct {
	Component       string                         `json:"component"`
	Kind            string                         `json:"kind"`
	App             string                         `json:"app,omitempty"`
	InstallID       int64                          `json:"install_id,omitempty"`
	Label           string                         `json:"label"`
	Description     string                         `json:"description,omitempty"`
	Icon            string                         `json:"icon,omitempty"`
	IconStyle       string                         `json:"icon_style,omitempty"`
	SupportedSizes  []string                       `json:"supported_sizes"`
	DefaultSize     string                         `json:"default_size"`
	DefaultSettings map[string]any                 `json:"default_settings,omitempty"`
	SettingsSchema  map[string]any                 `json:"settings_schema,omitempty"`
	RefreshTopics   []string                       `json:"refresh_topics,omitempty"`
	Suggested       bool                           `json:"suggested,omitempty"`
	Native          *dashboardWidgetNativeRenderer `json:"native,omitempty"`
	ProjectSurfaces []sdk.UISurface                `json:"project_surfaces,omitempty"`
}

var dashboardHomeBuiltins = []dashboardWidgetDefinition{
	{Component: "native:helper", Kind: "builtin", Label: "Apteva Helper", Description: "Build through the existing Conversations chat widget.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:system-map", Kind: "builtin", Label: "System map", Description: "Explore agents and their configured capabilities.", SupportedSizes: []string{"half", "full"}, DefaultSize: "full"},
	{Component: "native:result-preview", Kind: "builtin", Label: "Result / preview", Description: "Inspect selected activity and app outputs.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:workspace-summary", Kind: "builtin", Label: "Workspace overview", Description: "A compact view of the resources and state in this workspace.", SupportedSizes: []string{"half", "full"}, DefaultSize: "full"},
	{Component: "native:quick-actions", Kind: "builtin", Label: "Quick actions", Description: "Start common workspace actions without leaving the page.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:context-inspector", Kind: "builtin", Label: "Inspector", Description: "Inspect and act on the item selected in another widget.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:readiness", Kind: "builtin", Label: "Needs attention", Description: "Reported resource issues in this scope.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:agents", Kind: "builtin", Label: "Agents", Description: "Your agents and their current state.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half", DefaultSettings: map[string]any{"max_rows": 6}, SettingsSchema: map[string]any{"type": "object", "properties": map[string]any{"max_rows": map[string]any{"type": "integer", "title": "Rows", "minimum": 3, "maximum": 12, "default": 6}}}},
	{Component: "native:apps", Kind: "builtin", Label: "Apps", Description: "Installed apps available in this workspace.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half", DefaultSettings: map[string]any{"max_rows": 6}, SettingsSchema: map[string]any{"type": "object", "properties": map[string]any{"max_rows": map[string]any{"type": "integer", "title": "Rows", "minimum": 3, "maximum": 12, "default": 6}}}},
	{Component: "native:integrations", Kind: "builtin", Label: "Integrations", Description: "Connected services and the tools they provide.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half", DefaultSettings: map[string]any{"max_rows": 6}, SettingsSchema: map[string]any{"type": "object", "properties": map[string]any{"max_rows": map[string]any{"type": "integer", "title": "Rows", "minimum": 3, "maximum": 12, "default": 6}}}},
	{Component: "native:skills", Kind: "builtin", Label: "Skills", Description: "Reusable instructions available to your agents.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half", DefaultSettings: map[string]any{"max_rows": 6}, SettingsSchema: map[string]any{"type": "object", "properties": map[string]any{"max_rows": map[string]any{"type": "integer", "title": "Rows", "minimum": 3, "maximum": 12, "default": 6}}}},
	{Component: "native:agent-activity", Kind: "builtin", Label: "Agent activity", Description: "Current work, progress, blockers, and next steps across your agents.", SupportedSizes: []string{"half", "full"}, DefaultSize: "full"},
	{Component: "native:usage", Kind: "builtin", Label: "Usage summary", Description: "Agents, calls, tokens, errors, and cost for the last 24 hours.", SupportedSizes: []string{"full"}, DefaultSize: "full"},
	{Component: "native:activity", Kind: "builtin", Label: "Live activity", Description: "Thoughts, tools, events, and results across this scope.", SupportedSizes: []string{"half", "full"}, DefaultSize: "full"},
}

// GET /api/ui/surfaces/dashboard.home resolves the user's portable layout and
// every native-capable definition visible to a project. Layout entries whose
// definition is web-only remain in the response so a native edit can round-trip
// them without deleting another host's configuration.
func (s *Server) handleUISurfaceResolution(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	surface := strings.TrimPrefix(r.URL.Path, "/ui/surfaces/")
	projectID := r.URL.Query().Get("project_id")
	if surface != sdk.UIComponentSlotDashboardHome || projectID == "" {
		http.Error(w, "only dashboard.home with project_id is supported", http.StatusBadRequest)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	layoutDocument, revision := s.store.GetUserUILayoutWithRevision(userID)
	layout := resolvedDashboardHomeLayout(layoutDocument, projectID)
	definitions := append([]dashboardWidgetDefinition(nil), dashboardHomeBuiltins...)
	definitions = append(definitions, s.nativeDashboardWidgetDefinitions(projectID)...)

	w.Header().Set("ETag", `"`+itoa(revision)+`"`)
	writeJSON(w, map[string]any{
		"surface":     surface,
		"project_id":  projectID,
		"revision":    revision,
		"layout":      layout,
		"definitions": definitions,
	})
}

func (s *Server) nativeDashboardWidgetDefinitions(projectID string) []dashboardWidgetDefinition {
	return s.installedDashboardWidgetDefinitions(projectID, true)
}

// installedDashboardWidgetDefinitions resolves installed app contributions
// for dashboard.home. Preset compilation needs the full web catalog, while
// the native surface endpoint requests only definitions with a declarative
// native renderer.
func (s *Server) installedDashboardWidgetDefinitions(projectID string, nativeOnly bool) []dashboardWidgetDefinition {
	return s.installedWidgetDefinitionsForSlot(projectID, sdk.UIComponentSlotDashboardHome, nativeOnly)
}

func (s *Server) installedWidgetDefinitionsForSlot(projectID, slot string, nativeOnly bool) []dashboardWidgetDefinition {
	if s.installedApps == nil {
		return nil
	}
	// A project-local install shadows a global install with the same name.
	visible := map[string]*InstalledApp{}
	for _, app := range s.installedApps.ListForProject(projectID) {
		current := visible[app.AppName]
		if current == nil || (current.ProjectID == "" && app.ProjectID == projectID) {
			visible[app.AppName] = app
		}
	}
	apps := make([]*InstalledApp, 0, len(visible))
	for _, app := range visible {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].AppName < apps[j].AppName })

	definitions := make([]dashboardWidgetDefinition, 0)
	for _, app := range apps {
		for _, component := range app.Manifest.Provides.UIComponents {
			if !containsString(component.Slots, slot) || (nativeOnly && component.Native == nil) {
				continue
			}
			sizes := normalizedDashboardWidgetSizes(component)
			label := component.Label
			if label == "" {
				label = component.Name
			}
			definition := dashboardWidgetDefinition{
				Component: app.AppName + ":" + component.Name,
				Kind:      "app", App: app.AppName, InstallID: app.InstallID,
				Label: label, Description: component.Description,
				Icon:           resolveInstalledAppIcon(app.AppName, app.Manifest.Icon, app.Manifest.Version, app.InstallID, projectID),
				IconStyle:      app.Manifest.IconStyle,
				SupportedSizes: sizes, DefaultSize: normalizedDashboardWidgetDefaultSize(component, sizes),
				DefaultSettings: dashboardWidgetDefaultSettings(component.SettingsSchema),
				SettingsSchema:  component.SettingsSchema,
				RefreshTopics:   component.RefreshTopics, Suggested: component.Suggested,
				ProjectSurfaces: app.Manifest.Provides.UISurfaces,
			}
			if component.Native != nil {
				definition.Native = &dashboardWidgetNativeRenderer{Schema: component.Native.Schema, Entry: component.Native.Entry}
			}
			definitions = append(definitions, definition)
		}
	}
	return definitions
}

func normalizedDashboardWidgetSizes(component sdk.UIComponent) []string {
	seen := map[string]bool{}
	var sizes []string
	for _, size := range component.SupportedSizes {
		if (size == "half" || size == "full") && !seen[size] {
			seen[size] = true
			sizes = append(sizes, size)
		}
	}
	if len(sizes) == 0 {
		if component.DefaultWidth == 2 {
			return []string{"full"}
		}
		return []string{"half"}
	}
	return sizes
}

func normalizedDashboardWidgetDefaultSize(component sdk.UIComponent, sizes []string) string {
	for _, size := range sizes {
		if size == component.DefaultSize {
			return size
		}
	}
	return sizes[0]
}

func dashboardWidgetDefaultSettings(schema map[string]any) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	defaults := map[string]any{}
	for name, raw := range properties {
		property, _ := raw.(map[string]any)
		if value, ok := property["default"]; ok {
			defaults[name] = value
		}
	}
	if len(defaults) == 0 {
		return nil
	}
	return defaults
}

func resolvedDashboardHomeLayout(document json.RawMessage, projectID string) []dashboardWidgetInstance {
	return resolvedWidgetLayout(document, projectID, sdk.UIComponentSlotDashboardHome)
}

func resolvedWidgetLayout(document json.RawMessage, projectID, slot string) []dashboardWidgetInstance {
	var parsed struct {
		Projects map[string]struct {
			Slots map[string]json.RawMessage `json:"slots"`
		} `json:"projects"`
	}
	if json.Unmarshal(document, &parsed) != nil {
		return []dashboardWidgetInstance{}
	}
	project, ok := parsed.Projects[projectID]
	if !ok {
		return []dashboardWidgetInstance{}
	}
	raw, explicit := project.Slots[slot]
	if !explicit {
		return []dashboardWidgetInstance{}
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return []dashboardWidgetInstance{}
	}
	layout := make([]dashboardWidgetInstance, 0, len(entries))
	for index, entry := range entries {
		var legacy string
		if json.Unmarshal(entry, &legacy) == nil && legacy != "" {
			if retiredDashboardHomeWidget(legacy) {
				continue
			}
			layout = append(layout, dashboardWidgetInstance{ID: "legacy:" + itoa(int64(index)) + ":" + legacy, Component: legacy, Size: "half"})
			continue
		}
		var widget dashboardWidgetInstance
		if json.Unmarshal(entry, &widget) != nil || widget.Component == "" {
			continue
		}
		if retiredDashboardHomeWidget(widget.Component) {
			continue
		}
		if widget.ID == "" {
			widget.ID = "widget:" + itoa(int64(index)) + ":" + widget.Component
		}
		if widget.Size != "full" {
			widget.Size = "half"
		}
		layout = append(layout, widget)
	}
	return migrateStarterWidgets(layout)
}

// Retired starter cards now use the app-owned Tasks overview. Existing Tasks
// widgets take precedence, preserving their configuration and position.
func migrateStarterWidgets(widgets []dashboardWidgetInstance) []dashboardWidgetInstance {
	hasTasks := false
	for _, widget := range widgets {
		hasTasks = hasTasks || widget.Component == "tasks:task-overview"
	}
	result := make([]dashboardWidgetInstance, 0, len(widgets))
	for _, widget := range widgets {
		if widget.Component == "native:starter-assignments" {
			if hasTasks {
				continue
			}
			widget.Component = "tasks:task-overview"
			hasTasks = true
		}
		result = append(result, widget)
	}
	return result
}

func retiredDashboardHomeWidget(component string) bool {
	return component == "native:inbox"
}
