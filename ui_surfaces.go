package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

type dashboardWidgetInstance struct {
	ID        string         `json:"id"`
	Component string         `json:"component"`
	Size      string         `json:"size"`
	Settings  map[string]any `json:"settings,omitempty"`
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

var dashboardHomeDefaults = []dashboardWidgetInstance{
	{ID: "native:usage", Component: "native:usage", Size: "full"},
	{ID: "native:inbox", Component: "native:inbox", Size: "half"},
	{ID: "native:activity", Component: "native:activity", Size: "full"},
}

var dashboardHomeBuiltins = []dashboardWidgetDefinition{
	{Component: "native:usage", Kind: "builtin", Label: "Usage summary", Description: "Agents, calls, tokens, errors, and cost for the last 24 hours.", SupportedSizes: []string{"full"}, DefaultSize: "full"},
	{Component: "native:inbox", Kind: "builtin", Label: "Inbox", Description: "Approvals, reports, and alerts from your agents.", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:activity", Kind: "builtin", Label: "Recent activity", Description: "Significant agent actions and tool events.", SupportedSizes: []string{"half", "full"}, DefaultSize: "full"},
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
			if component.Native == nil || !containsString(component.Slots, sdk.UIComponentSlotDashboardHome) {
				continue
			}
			sizes := normalizedDashboardWidgetSizes(component)
			label := component.Label
			if label == "" {
				label = component.Name
			}
			definitions = append(definitions, dashboardWidgetDefinition{
				Component: app.AppName + ":" + component.Name,
				Kind:      "app", App: app.AppName, InstallID: app.InstallID,
				Label: label, Description: component.Description,
				Icon:           resolveInstalledAppIcon(app.AppName, app.Manifest.Icon, app.Manifest.Version, app.InstallID, projectID),
				IconStyle:      app.Manifest.IconStyle,
				SupportedSizes: sizes, DefaultSize: normalizedDashboardWidgetDefaultSize(component, sizes),
				DefaultSettings: dashboardWidgetDefaultSettings(component.SettingsSchema),
				SettingsSchema:  component.SettingsSchema,
				RefreshTopics:   component.RefreshTopics, Suggested: component.Suggested,
				Native:          &dashboardWidgetNativeRenderer{Schema: component.Native.Schema, Entry: component.Native.Entry},
				ProjectSurfaces: app.Manifest.Provides.UISurfaces,
			})
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
	var parsed struct {
		Projects map[string]struct {
			Slots map[string]json.RawMessage `json:"slots"`
		} `json:"projects"`
	}
	if json.Unmarshal(document, &parsed) != nil {
		return cloneDashboardWidgetInstances(dashboardHomeDefaults)
	}
	project, ok := parsed.Projects[projectID]
	if !ok {
		return cloneDashboardWidgetInstances(dashboardHomeDefaults)
	}
	raw, explicit := project.Slots[sdk.UIComponentSlotDashboardHome]
	if !explicit {
		return cloneDashboardWidgetInstances(dashboardHomeDefaults)
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return []dashboardWidgetInstance{}
	}
	layout := make([]dashboardWidgetInstance, 0, len(entries))
	for index, entry := range entries {
		var legacy string
		if json.Unmarshal(entry, &legacy) == nil && legacy != "" {
			layout = append(layout, dashboardWidgetInstance{ID: "legacy:" + itoa(int64(index)) + ":" + legacy, Component: legacy, Size: "half"})
			continue
		}
		var widget dashboardWidgetInstance
		if json.Unmarshal(entry, &widget) != nil || widget.Component == "" {
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
	if len(layout) > 0 {
		hasBuiltin := false
		for _, widget := range layout {
			if strings.HasPrefix(widget.Component, "native:") {
				hasBuiltin = true
				break
			}
		}
		if !hasBuiltin {
			merged := cloneDashboardWidgetInstances(dashboardHomeDefaults[:len(dashboardHomeDefaults)-1])
			merged = append(merged, layout...)
			merged = append(merged, dashboardHomeDefaults[len(dashboardHomeDefaults)-1])
			layout = merged
		}
	}
	return layout
}

func cloneDashboardWidgetInstances(source []dashboardWidgetInstance) []dashboardWidgetInstance {
	result := make([]dashboardWidgetInstance, len(source))
	copy(result, source)
	return result
}
