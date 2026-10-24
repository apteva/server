package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"reflect"
)

// Only new/changed settings of known widgets are validated. This preserves
// unavailable widgets and legacy settings during unrelated reorders/resizes.
func (s *Server) validateChangedWidgetSettings(userID int64, projectID, surface string, raw json.RawMessage, global bool) error {
	type entry struct {
		ID        string          `json:"id"`
		Component string          `json:"component"`
		Settings  json.RawMessage `json:"settings"`
	}
	var encoded []json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return fmt.Errorf("invalid widget layout")
	}
	incoming := make([]entry, 0, len(encoded))
	for _, item := range encoded {
		// Legacy string identities and non-widget preference entries remain portable.
		var legacy string
		if json.Unmarshal(item, &legacy) == nil {
			continue
		}
		var widget entry
		if err := json.Unmarshal(item, &widget); err != nil {
			return fmt.Errorf("invalid widget layout")
		}
		incoming = append(incoming, widget)
	}
	document, _ := s.store.GetUserUILayoutWithRevision(userID)
	var stored struct {
		Projects map[string]struct {
			Slots map[string]json.RawMessage `json:"slots"`
		} `json:"projects"`
		Global struct {
			Slots map[string]json.RawMessage `json:"slots"`
		} `json:"global"`
	}
	_ = json.Unmarshal(document, &stored)
	oldRaw := stored.Projects[projectID].Slots[surface]
	if global {
		oldRaw = stored.Global.Slots[surface]
	}
	var previous []entry
	var previousItems []json.RawMessage
	_ = json.Unmarshal(oldRaw, &previousItems)
	for _, item := range previousItems {
		var old entry
		if json.Unmarshal(item, &old) == nil {
			previous = append(previous, old)
		}
	}
	definitions := map[string]map[string]any{}
	for _, d := range dashboardHomeBuiltins {
		definitions[d.Component] = d.SettingsSchema
	}
	if s.installedApps != nil {
		visible := map[string]*InstalledApp{}
		for _, app := range s.installedApps.ListForProject(projectID) {
			if global && app.ProjectID != "" {
				continue
			}
			if old := visible[app.AppName]; old == nil || (old.ProjectID == "" && app.ProjectID == projectID) {
				visible[app.AppName] = app
			}
		}
		for _, app := range visible {
			for _, component := range app.Manifest.Provides.UIComponents {
				definitions[app.AppName+":"+component.Name] = component.SettingsSchema
			}
		}
	}
	for _, widget := range incoming {
		schema, known := definitions[widget.Component]
		if !known || len(schema) == 0 {
			continue
		}
		var settings map[string]any
		if len(widget.Settings) > 0 {
			if err := json.Unmarshal(widget.Settings, &settings); err != nil {
				return fmt.Errorf("%s settings must be an object", widget.Component)
			}
		}
		unchanged := false
		for _, old := range previous {
			if old.ID == widget.ID && old.Component == widget.Component {
				var oldSettings map[string]any
				if json.Unmarshal(old.Settings, &oldSettings) == nil && reflect.DeepEqual(oldSettings, settings) {
					unchanged = true
					break
				}
			}
		}
		if unchanged {
			continue
		}
		if err := sdk.ValidateWidgetSettings(settings, schema); err != nil {
			return fmt.Errorf("%s: %w", widget.Component, err)
		}
	}
	return nil
}
