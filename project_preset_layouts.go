package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var presetAgentWidgets = []dashboardWidgetDefinition{
	{Component: "native:agent-activity", Label: "Recent activity", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
	{Component: "native:agent-results", Label: "Recent responses", SupportedSizes: []string{"half", "full"}, DefaultSize: "half"},
}

func portablePresetWidgets(widgets []dashboardWidgetInstance, agentKeys map[int64]string) []ProjectPresetWidget {
	result := []ProjectPresetWidget{}
	for index, widget := range widgets {
		if widget.AgentID != 0 && agentKeys[widget.AgentID] == "" {
			continue
		}
		key := agentKeys[widget.AgentID]
		widget.ID = fmt.Sprintf("captured-%d", index+1)
		widget.AgentID = 0
		widget.Setup = nil
		widget.Settings = portablePresetSettings(widget.Settings)
		result = append(result, ProjectPresetWidget{dashboardWidgetInstance: widget, AgentKey: key})
	}
	return result
}

// Capture presentation choices, never account/resource IDs, credentials, or
// conversation state. Apps resolve their own data in the destination project.
func portablePresetSettings(settings map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range settings {
		lower := strings.ToLower(key)
		forbidden := lower == "id" || strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, "_ids")
		for _, part := range []string{"token", "secret", "password", "credential", "api_key", "apikey", "authorization", "conversation", "thread", "message", "history"} {
			if strings.Contains(lower, part) {
				forbidden = true
			}
		}
		if forbidden {
			continue
		}
		switch value := value.(type) {
		case bool, float64, string:
			result[key] = value
		case map[string]any:
			result[key] = portablePresetSettings(value)
		}
	}
	return result
}

func validatePresetLayouts(preset ProjectPreset) error {
	if len(preset.Connections) > 50 {
		return errors.New("too many connection setup steps")
	}
	for _, step := range preset.Connections {
		if !validPresetIdentifier(step.App) || strings.TrimSpace(step.Title) == "" || len(step.Title) > 160 || strings.TrimSpace(step.Description) == "" || len(step.Description) > 2000 {
			return errors.New("invalid preset connection setup step")
		}
	}
	if preset.Layouts == nil {
		return nil
	}
	agents := map[string]ProjectPresetAgent{}
	for _, agent := range preset.Agents {
		agents[agent.Key] = agent
	}
	validate := func(widgets []ProjectPresetWidget, agentKey string) error {
		if len(widgets) > 50 {
			return errors.New("a preset surface may contain at most 50 widgets")
		}
		ids := map[string]bool{}
		for _, widget := range widgets {
			app, name, ok := strings.Cut(widget.Component, ":")
			if !ok || !validPresetIdentifier(app) || !validPresetIdentifier(name) || widget.ID == "" || len(widget.ID) > 160 || ids[widget.ID] || (widget.Size != "half" && widget.Size != "full") {
				return fmt.Errorf("invalid preset widget %q", widget.Component)
			}
			ids[widget.ID] = true
			if widget.AgentID != 0 || len(widget.Setup) > 0 {
				return errors.New("preset widgets must use portable agent_key and top-level connections")
			}
			if widget.AgentKey != "" {
				if _, ok := agents[widget.AgentKey]; !ok {
					return fmt.Errorf("unknown widget agent_key %q", widget.AgentKey)
				}
				if app != "native" && !containsString(agents[widget.AgentKey].Apps, app) {
					return fmt.Errorf("widget %s requires app %s attached to agent %s", widget.Component, app, widget.AgentKey)
				}
			}
			if agentKey != "" {
				if widget.AgentKey != "" && widget.AgentKey != agentKey {
					return errors.New("agent overview widgets cannot target another agent")
				}
				if app != "native" && !containsString(agents[agentKey].Apps, app) {
					return fmt.Errorf("widget %s requires app %s attached to agent %s", widget.Component, app, agentKey)
				}
			}
			if app == "native" {
				definitions := dashboardHomeBuiltins
				if agentKey != "" {
					definitions = presetAgentWidgets
				}
				found := false
				for _, definition := range definitions {
					if definition.Component == widget.Component {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("widget %s does not support this surface", widget.Component)
				}
			}
		}
		return nil
	}
	if err := validate(preset.Layouts.Home, ""); err != nil {
		return err
	}
	for key, widgets := range preset.Layouts.AgentOverview {
		if _, ok := agents[key]; !ok {
			return fmt.Errorf("unknown agent overview key %q", key)
		}
		if err := validate(widgets, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) compilePresetWidgets(projectID string, preset ProjectPreset, slot string, widgets []ProjectPresetWidget) ([]dashboardWidgetInstance, []string) {
	definitions := dashboardHomeBuiltins
	if slot == "dashboard.agent_detail" {
		definitions = presetAgentWidgets
	}
	available := map[string]dashboardWidgetDefinition{}
	for _, definition := range append(append([]dashboardWidgetDefinition(nil), definitions...), s.installedWidgetDefinitionsForSlot(projectID, slot, false)...) {
		available[definition.Component] = definition
	}
	layout := []dashboardWidgetInstance{}
	warnings := []string{}
	for _, spec := range widgets {
		widget := spec.dashboardWidgetInstance
		widget.ID = "preset:" + preset.ID + ":" + spec.ID
		if definition, ok := available[widget.Component]; ok {
			if !containsString(definition.SupportedSizes, widget.Size) {
				widget.Size = definition.DefaultSize
				warnings = append(warnings, fmt.Sprintf("%s uses its supported %s width", widget.Component, widget.Size))
			}
			settings := map[string]any{}
			for key, value := range definition.DefaultSettings {
				settings[key] = value
			}
			for key, value := range widget.Settings {
				settings[key] = value
			}
			if err := validatePresetWidgetSettings(settings, definition.SettingsSchema); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s settings need review: %s; using widget defaults", widget.Component, err))
				settings = definition.DefaultSettings
			}
			widget.Settings = settings
		} else {
			// Keep the requested place in the layout. The canvas shows a recoverable
			// placeholder until the app supplies an eligible widget on this surface.
			warnings = append(warnings, fmt.Sprintf("%s is unavailable on %s; install or update its app", widget.Component, slot))
		}
		app, _, _ := strings.Cut(widget.Component, ":")
		for _, step := range preset.Connections {
			if step.App == app {
				widget.Setup = append(widget.Setup, step)
			}
		}
		layout = append(layout, widget)
	}
	return layout, warnings
}

// Validate the primitive controls supported by the shared widget settings UI.
func validatePresetWidgetSettings(settings map[string]any, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	// Manifest YAML and captured JSON can use different Go numeric types.
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var normalizedSettings, normalizedSchema map[string]any
	if err := json.Unmarshal(settingsJSON, &normalizedSettings); err != nil {
		return err
	}
	if err := json.Unmarshal(schemaJSON, &normalizedSchema); err != nil {
		return err
	}
	settings, schema = normalizedSettings, normalizedSchema
	props, _ := schema["properties"].(map[string]any)
	for key, value := range settings {
		property, ok := props[key].(map[string]any)
		if !ok {
			if schema["additionalProperties"] == false {
				return fmt.Errorf("unknown setting %s", key)
			}
			continue
		}
		kind, _ := property["type"].(string)
		valid := true
		switch kind {
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number", "integer":
			number, ok := value.(float64)
			valid = ok
			if valid && kind == "integer" {
				valid = number == float64(int64(number))
			}
			if min, ok := property["minimum"].(float64); ok && number < min {
				valid = false
			}
			if max, ok := property["maximum"].(float64); ok && number > max {
				valid = false
			}
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		}
		if !valid {
			return fmt.Errorf("invalid value for %s", key)
		}
		if choices, ok := property["enum"].([]any); ok {
			found := false
			for _, choice := range choices {
				if reflect.DeepEqual(choice, value) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("invalid choice for %s", key)
			}
		}
	}
	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			key, _ := raw.(string)
			if _, ok := settings[key]; !ok {
				return fmt.Errorf("missing setting %s", key)
			}
		}
	}
	return nil
}

func (s *Server) compilePresetAgentLayouts(projectID string, preset ProjectPreset) (map[string][]dashboardWidgetInstance, []string) {
	result := map[string][]dashboardWidgetInstance{}
	warnings := []string{}
	if preset.Layouts == nil {
		return result, warnings
	}
	for key, widgets := range preset.Layouts.AgentOverview {
		hasActivity := false
		for _, widget := range widgets {
			if widget.Component == "native:agent-activity" {
				hasActivity = true
			}
		}
		if !hasActivity {
			widgets = append([]ProjectPresetWidget{{dashboardWidgetInstance: dashboardWidgetInstance{ID: "activity", Component: "native:agent-activity", Size: "half"}}}, widgets...)
		}
		layout, notices := s.compilePresetWidgets(projectID, preset, "dashboard.agent_detail", widgets)
		result[key] = layout
		warnings = append(warnings, notices...)
	}
	return result, warnings
}

func resolvePresetWidgetAgents(layout []dashboardWidgetInstance, specs []ProjectPresetWidget, ids map[string]int64, presetID string) ([]dashboardWidgetInstance, []string) {
	bindings := map[string]string{}
	for _, spec := range specs {
		if spec.AgentKey != "" {
			bindings["preset:"+presetID+":"+spec.ID] = spec.AgentKey
		}
	}
	result := append([]dashboardWidgetInstance(nil), layout...)
	warnings := []string{}
	for i := range result {
		if key := bindings[result[i].ID]; key != "" {
			result[i].AgentID = ids[key]
			if ids[key] == 0 {
				warnings = append(warnings, "Widget "+result[i].Component+" is waiting for agent "+key)
				result[i].AgentID = -1
			}
		}
	}
	return result, warnings
}

func (s *Server) applyPresetLayouts(userID int64, projectID string, preview *ProjectPresetPreview, ids map[string]int64) []string {
	warnings := []string{}
	home := preview.Layout
	if preview.Preset.Layouts != nil {
		var notices []string
		home, notices = resolvePresetWidgetAgents(home, preview.Preset.Layouts.Home, ids, preview.Preset.ID)
		warnings = append(warnings, notices...)
	}
	mergeHome := s.mergeProjectPresetDashboardLayout
	if preview.Preset.Layouts != nil {
		mergeHome = func(userID int64, projectID string, widgets []dashboardWidgetInstance) error {
			return s.mergePresetSurface(userID, projectID, "dashboard.home", widgets)
		}
	}
	if err := mergeHome(userID, projectID, home); err != nil {
		warnings = append(warnings, "Home layout: "+err.Error())
	}
	for key, layout := range preview.AgentLayouts {
		id := ids[key]
		if id == 0 {
			continue
		}
		// All interface modes start with the preset; each remains independently editable.
		for _, audience := range []string{"personal", "business", "developer"} {
			slot := fmt.Sprintf("agent.%d.%s.overview", id, audience)
			if err := s.mergePresetSurface(userID, projectID, slot, layout); err != nil {
				warnings = append(warnings, key+" overview: "+err.Error())
			}
		}
	}
	return warnings
}

func (s *Server) mergePresetSurface(userID int64, projectID, slot string, widgets []dashboardWidgetInstance) error {
	if len(widgets) == 0 {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		document, revision := s.store.GetUserUILayoutWithRevision(userID)
		current := resolvedWidgetLayout(document, projectID, slot)
		if strings.HasPrefix(slot, "agent.") {
			var stored struct {
				Projects map[string]struct {
					Slots map[string]json.RawMessage `json:"slots"`
				} `json:"projects"`
			}
			if json.Unmarshal(document, &stored) == nil {
				if _, explicit := stored.Projects[projectID].Slots[slot]; !explicit {
					current = resolvedWidgetLayout(document, projectID, "dashboard.agent_detail")
				}
			}
		}
		merged := append([]dashboardWidgetInstance(nil), current...)
		changed := false
		ids := map[string]bool{}
		shapes := map[string]bool{}
		for _, widget := range current {
			ids[widget.ID] = true
			shapes[dashboardWidgetFingerprint(widget)] = true
		}
		for _, widget := range widgets {
			// Recover a binding whose agent creation failed on the previous try,
			// preserving the user's position, size, and settings.
			if ids[widget.ID] && widget.AgentID > 0 {
				for i := range merged {
					if merged[i].ID == widget.ID && merged[i].AgentID == -1 {
						merged[i].AgentID = widget.AgentID
						changed = true
					}
				}
			}
			if ids[widget.ID] || shapes[dashboardWidgetFingerprint(widget)] {
				continue
			}
			// Activity is mandatory and unique, even if a user already moved/resized it.
			duplicateActivity := false
			if widget.Component == "native:agent-activity" && slot != "dashboard.home" {
				for _, old := range current {
					if old.Component == widget.Component {
						duplicateActivity = true
					}
				}
			}
			if duplicateActivity {
				continue
			}
			merged = append(merged, widget)
			ids[widget.ID] = true
			shapes[dashboardWidgetFingerprint(widget)] = true
		}
		if !changed && len(merged) == len(current) {
			return nil
		}
		raw, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if _, _, err = s.store.PatchUserUILayoutSurface(userID, projectID, slot, raw, &revision); err == nil {
			return nil
		} else if !errors.Is(err, errUILayoutConflict) {
			return err
		}
	}
	return errUILayoutConflict
}
