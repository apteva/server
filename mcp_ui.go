package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

var workspacePageToolNames = map[string]bool{
	"pages_list": true, "page_get": true, "widgets_list": true,
	"page_create": true, "page_update": true,
	"widget_add": true, "widget_configure": true, "widget_move": true, "widget_remove": true,
}

func workspacePageToolDefinitions() []toolDef {
	var tools []toolDef
	definitions := []struct {
		name, description string
		required          []string
	}{
		{"pages_list", "List your Pages in the current project, including Home. Returns the layout revision.", nil},
		{"page_get", "Read one Page's widget instances and revision. Use widget IDs from this result for edits.", nil},
		{"page_create", "Create a personal Page in the current scope. Returns page.id and revision for adding widgets.", []string{"title"}},
		{"page_update", "Edit a Page's title, description, layout or pinned state. Omitted fields are preserved, including all widgets. Read page_get first.", []string{"page_id", "revision"}},
		{"widgets_list", "List or search installed widgets available for this Page, including their settings. Omit query to list all available widgets.", nil},
		{"widget_add", "Add a widget to your Page. Reuse the same widget_id on retry to prevent duplicates. Select a component from widgets_list. Read page_get first.", []string{"widget_id", "component", "revision"}},
		{"widget_configure", "Configure one widget. Settings merge into its current settings. Supply the revision from page_get.", []string{"widget_id", "revision"}},
		{"widget_move", "Move a widget to a zero-based position or workspace placement. Other pages and widget settings are preserved.", []string{"widget_id", "revision"}},
		{"widget_remove", "Remove one widget instance from your Page, preserving other widgets and pages.", []string{"widget_id", "revision"}},
	}
	for _, d := range definitions {
		properties := map[string]toolParam{
			"project_id": {Type: "string", Description: "Defaults to the server-bound conversation project."},
			"scope":      {Type: "string", Description: "project or global. Project conversations are restricted to project scope."},
		}
		if d.name != "pages_list" && d.name != "page_create" {
			properties["page_id"] = toolParam{Type: "string", Description: "Page ID from pages_list. Omit for dashboard.home."}
		}
		if d.name == "page_create" || d.name == "page_update" {
			properties["title"] = toolParam{Type: "string", Description: "Page title, up to 160 bytes. Required when creating."}
			properties["description"] = toolParam{Type: "string", Description: "Page description, up to 2000 bytes. Empty clears it."}
			properties["layout"] = toolParam{Type: "string", Description: "grid or workspace. New Pages default to grid."}
			properties["pinned"] = toolParam{Type: "boolean", Description: "Whether the Page is pinned. Defaults to false for new Pages."}
		}
		if d.name == "page_update" {
			properties["page_id"] = toolParam{Type: "string", Description: "Custom Page ID from pages_list or page_create. Home cannot be renamed."}
			properties["revision"] = toolParam{Type: "integer", Description: "Current revision from page_get. Conflicts require a fresh read."}
		}
		if strings.HasPrefix(d.name, "widget_") {
			properties["widget_id"] = toolParam{Type: "string", Description: "Stable widget instance ID, unique within this Page. Choose once for add; use page_get for existing instances."}
			properties["revision"] = toolParam{Type: "integer", Description: "Required layout revision from page_get. Conflicts require a fresh read."}
		}
		if d.name == "widgets_list" {
			properties["query"] = toolParam{Type: "string", Description: "Optional text search across widget name, label, description and app name; case-insensitive."}
			properties["install_id"] = toolParam{Type: "integer", Description: "Optional. Inspect widgets from a particular installed version. Omit for defaults."}
		}
		if d.name == "widget_add" {
			properties["component"] = toolParam{Type: "string", Description: "Exact component identity from widgets_list."}
			properties["install_id"] = toolParam{Type: "integer", Description: "Optional. Select a particular app installation or version instead of its default."}
		}
		if d.name == "widget_add" || d.name == "widget_configure" {
			properties["settings"] = toolParam{Type: "object", Description: "Widget settings matching the discovered settings_schema. False and zero are preserved."}
			properties["size"] = toolParam{Type: "string", Description: "A supported size, half or full."}
		}
		if d.name == "widget_add" || d.name == "widget_move" {
			properties["position"] = toolParam{Type: "integer", Description: "Zero-based insertion position. Add defaults to the end."}
		}
		if d.name == "widget_add" || d.name == "widget_configure" || d.name == "widget_move" {
			properties["placement"] = toolParam{Type: "string", Description: "Workspace area: assistant, main, activity or details. Empty removes the area override."}
		}
		tools = append(tools, toolDef{Name: d.name, Description: d.description, InputSchema: toolSchema{Type: "object", Properties: properties, Required: d.required}})
	}
	return tools
}

func handleGatewayPageTool(name string, args map[string]any, projectID string, api gatewayAPIClient) (any, error) {
	body := make(map[string]any, len(args)+1)
	for key, value := range args {
		body[key] = value
	}
	delete(body, "_project_id") // Reserved transport metadata; projectID is server-bound.
	body["action"] = name
	if projectID != "" {
		if scope, ok := body["scope"]; ok && scope != "project" {
			return nil, fmt.Errorf("project conversations can only access project pages")
		}
		body["project_id"] = projectID
		body["scope"] = "project"
	}
	var out json.RawMessage
	if err := api.do(http.MethodPost, "/ui-layout/tools", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type workspacePageRequest struct {
	Action      string          `json:"action"`
	ProjectID   string          `json:"project_id"`
	Scope       string          `json:"scope"`
	PageID      string          `json:"page_id"`
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	Layout      *string         `json:"layout"`
	Pinned      *bool           `json:"pinned"`
	Query       string          `json:"query"`
	WidgetID    string          `json:"widget_id"`
	Component   string          `json:"component"`
	InstallID   int64           `json:"install_id"`
	Revision    *int64          `json:"revision"`
	Settings    json.RawMessage `json:"settings"`
	Size        *string         `json:"size"`
	Placement   *string         `json:"placement"`
	Position    *int            `json:"position"`
}

type workspaceLayoutScope struct {
	Pages []workspacePage            `json:"pages"`
	Slots map[string]json.RawMessage `json:"slots"`
}

type workspacePageState struct {
	Page      workspacePage     `json:"page"`
	ProjectID string            `json:"project_id,omitempty"`
	Scope     string            `json:"scope"`
	Revision  int64             `json:"revision"`
	Widgets   []json.RawMessage `json:"widgets"`
	Explicit  bool              `json:"explicit"`
	Changed   bool              `json:"changed"`
}

func (s *Server) handleWorkspacePageTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	userID := getUserID(r)
	if userID <= 0 {
		http.Error(w, "authenticated user required", 401)
		return
	}
	var req workspacePageRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || !workspacePageToolNames[req.Action] {
		http.Error(w, "invalid Page tool request", 400)
		return
	}
	if req.Scope == "" {
		req.Scope = "project"
		if req.ProjectID == "" {
			req.Scope = "global"
		}
	}
	if req.Scope != "project" && req.Scope != "global" {
		http.Error(w, "scope must be project or global", 400)
		return
	}
	if req.Scope == "global" && req.ProjectID != "" {
		http.Error(w, "global pages cannot specify project_id", 400)
		return
	}
	if req.Scope == "project" {
		if req.ProjectID == "" {
			http.Error(w, "project_id required", 400)
			return
		}
		if _, _, ok := s.requireProjectAccess(w, r, req.ProjectID, ProjectViewer); !ok {
			return
		}
	}
	document, revision := s.store.GetUserUILayoutWithRevision(userID)
	var layout struct {
		Projects map[string]workspaceLayoutScope `json:"projects"`
		Global   workspaceLayoutScope            `json:"global"`
	}
	if err := json.Unmarshal(document, &layout); err != nil {
		http.Error(w, "saved layout could not be read", 500)
		return
	}
	scope := layout.Global
	if req.Scope == "project" {
		scope = layout.Projects[req.ProjectID]
	}
	home := workspacePage{ID: "dashboard.home", Title: "Home", Scope: req.Scope, Kind: "system"}
	if req.Action == "pages_list" {
		pages := append([]workspacePage{home}, scope.Pages...)
		writeJSON(w, map[string]any{"pages": pages, "scope": req.Scope, "project_id": req.ProjectID, "revision": revision})
		return
	}
	if req.Action == "page_create" {
		if req.PageID != "" {
			http.Error(w, "page_id is assigned by the server when creating a Page", 400)
			return
		}
		page, err := applyPageMetadata(workspacePage{Scope: req.Scope, Kind: "custom", Layout: "grid", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			http.Error(w, "could not create page", 500)
			return
		}
		page.ID = hex.EncodeToString(id[:])
		_, revision, err = s.store.mutateUIPage(userID, req.ProjectID, page, http.MethodPost)
		if err != nil {
			writePageMutationError(w, err, revision)
			return
		}
		writeJSON(w, workspacePageState{Page: page, Scope: req.Scope, ProjectID: req.ProjectID, Revision: revision, Widgets: []json.RawMessage{}, Changed: true})
		return
	}
	page := home
	surface := "dashboard.home"
	if req.PageID != "" && req.PageID != "dashboard.home" {
		found := false
		for _, candidate := range scope.Pages {
			if candidate.ID == req.PageID {
				page = candidate
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "Page not found in this scope", 404)
			return
		}
		surface = "page." + page.ID
	}
	if !validUILayoutSurface(surface) {
		http.Error(w, "invalid Page surface", 400)
		return
	}
	raw, explicit := scope.Slots[surface]
	widgets := []json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &widgets) != nil {
		http.Error(w, "saved widget layout could not be read", 500)
		return
	}
	if widgets == nil {
		widgets = []json.RawMessage{}
	}
	state := workspacePageState{Page: page, Scope: req.Scope, ProjectID: req.ProjectID, Revision: revision, Widgets: widgets, Explicit: explicit}
	if req.Action == "page_update" {
		if req.PageID == "" || page.Kind == "system" {
			http.Error(w, "only custom Pages can be edited", 400)
			return
		}
		if req.Revision == nil || *req.Revision < 0 {
			http.Error(w, "a nonnegative revision from page_get is required", 400)
			return
		}
		if *req.Revision != revision {
			writePageMutationError(w, errUILayoutConflict, revision)
			return
		}
		updated, err := applyPageMetadata(page, req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		state.Widgets = normalizePageWidgetIDs(widgets, surface)
		if updated != page {
			_, revision, err = s.store.mutateUIPage(userID, req.ProjectID, updated, http.MethodPatch, req.Revision)
			if err != nil {
				writePageMutationError(w, err, revision)
				return
			}
			state.Page, state.Revision, state.Changed = updated, revision, true
		}
		writeJSON(w, state)
		return
	}
	definitions := s.pageWidgetDefinitions(req.ProjectID, surface, req.Scope == "global")
	if req.Action == "page_get" {
		state.Widgets = normalizePageWidgetIDs(state.Widgets, surface)
		writeJSON(w, state)
		return
	}
	if req.Action == "widgets_list" {
		available := []dashboardWidgetDefinition{}
		found := req.InstallID == 0
		for _, definition := range definitions {
			if definition.Kind == "app" {
				if req.InstallID > 0 {
					if definition.InstallID != req.InstallID {
						continue
					}
					found = true
				} else {
					app := s.installedApps.GetByNameAndProject(definition.App, req.ProjectID)
					if app == nil || app.InstallID != definition.InstallID {
						continue
					}
				}
				definition.Component = definition.App + ":" + strings.SplitN(definition.Component, ":", 2)[1]
			} else if req.InstallID > 0 {
				continue
			}
			if gatewayQueryMatch(req.Query, definition.Component, definition.App, definition.Label, definition.Description) {
				available = append(available, definition)
			}
		}
		if !found || req.InstallID < 0 {
			http.Error(w, "installation has no widgets available in this Page", 404)
			return
		}
		writeJSON(w, map[string]any{"widgets": available, "scope": req.Scope, "project_id": req.ProjectID, "page_id": page.ID, "revision": revision})
		return
	}
	if req.Revision == nil || *req.Revision < 0 {
		http.Error(w, "a nonnegative revision from page_get is required", 400)
		return
	}
	if req.WidgetID == "" || len(req.WidgetID) > 240 || strings.ContainsAny(req.WidgetID, "\r\n\x00") {
		http.Error(w, "a stable widget_id is required", 400)
		return
	}
	if req.Action != "widget_add" && *req.Revision != revision {
		writeJSONStatus(w, 409, map[string]any{"error": "layout changed; read page_get again", "revision": revision})
		return
	}
	if req.Action == "widget_add" {
		if err := s.resolvePageWidgetInstallation(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	updated, changed, err := mutatePageWidgets(req, state.Widgets, surface, definitions)
	if err != nil {
		status := 400
		if errors.Is(err, errUILayoutConflict) {
			status = 409
		}
		writeJSONStatus(w, status, map[string]any{"error": err.Error(), "revision": revision})
		return
	}
	// An identical add is an idempotent retry, even if another widget changed.
	if !changed && req.Action == "widget_add" {
		state.Widgets = normalizePageWidgetIDs(state.Widgets, surface)
		writeJSON(w, state)
		return
	}
	if *req.Revision != revision {
		writeJSONStatus(w, 409, map[string]any{"error": "layout changed; read page_get again", "revision": revision})
		return
	}
	if !changed {
		state.Widgets = normalizePageWidgetIDs(state.Widgets, surface)
		writeJSON(w, state)
		return
	}
	value, err := json.Marshal(updated)
	if err != nil {
		http.Error(w, "encode widget layout", 500)
		return
	}
	if req.Scope == "global" {
		_, revision, err = s.store.PatchUserUILayoutGlobalSurface(userID, surface, value, req.Revision)
	} else {
		_, revision, err = s.store.PatchUserUILayoutSurface(userID, req.ProjectID, surface, value, req.Revision)
	}
	if errors.Is(err, errUILayoutConflict) {
		writeJSONStatus(w, 409, map[string]any{"error": "layout changed; read page_get again", "revision": revision})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	state.Widgets = normalizePageWidgetIDs(updated, surface)
	state.Revision = revision
	state.Explicit = true
	state.Changed = true
	writeJSON(w, state)
}

func applyPageMetadata(page workspacePage, req workspacePageRequest) (workspacePage, error) {
	if req.Title != nil {
		page.Title = strings.TrimSpace(*req.Title)
	}
	if req.Description != nil {
		page.Description = strings.TrimSpace(*req.Description)
	}
	if req.Pinned != nil {
		page.Pinned = *req.Pinned
	}
	if req.Layout != nil {
		if *req.Layout != "grid" && *req.Layout != "workspace" {
			return page, fmt.Errorf("layout must be grid or workspace")
		}
		page.Layout = *req.Layout
	}
	if page.Title == "" || len(page.Title) > 160 || len(page.Description) > 2000 {
		return page, fmt.Errorf("title is required (160 bytes max); description must be at most 2000 bytes")
	}
	return page, nil
}

func writePageMutationError(w http.ResponseWriter, err error, revision int64) {
	status := http.StatusBadRequest
	message := err.Error()
	if errors.Is(err, errUILayoutConflict) {
		status = http.StatusConflict
		message = "layout changed; read page_get again"
	} else if errors.Is(err, errPageMissing) {
		status = http.StatusNotFound
	}
	writeJSONStatus(w, status, map[string]any{"error": message, "revision": revision})
}

func pageWidgetObject(raw json.RawMessage, surface string, index int) map[string]json.RawMessage {
	var legacy string
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &legacy) == nil {
		fields = map[string]json.RawMessage{}
		fields["component"], _ = json.Marshal(legacy)
		fields["id"], _ = json.Marshal(fmt.Sprintf("legacy:%d:%s", index, legacy))
		fields["size"] = json.RawMessage(`"half"`)
		return fields
	}
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	var id, component string
	_ = json.Unmarshal(fields["id"], &id)
	_ = json.Unmarshal(fields["component"], &component)
	if id == "" && component != "" {
		fields["id"], _ = json.Marshal(fmt.Sprintf("legacy:%d:%s", index, component))
	}
	return fields
}

func normalizePageWidgetIDs(widgets []json.RawMessage, surface string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(widgets))
	for index, raw := range widgets {
		fields := pageWidgetObject(raw, surface, index)
		if fields == nil {
			out = append(out, raw)
			continue
		}
		value, _ := json.Marshal(fields)
		out = append(out, value)
	}
	return out
}

func mutatePageWidgets(req workspacePageRequest, widgets []json.RawMessage, surface string, definitions []dashboardWidgetDefinition) ([]json.RawMessage, bool, error) {
	index := -1
	var current map[string]json.RawMessage
	for i, raw := range widgets {
		fields := pageWidgetObject(raw, surface, i)
		var id string
		_ = json.Unmarshal(fields["id"], &id)
		if id == req.WidgetID {
			if index >= 0 {
				return nil, false, fmt.Errorf("duplicate widget ID in saved layout")
			}
			index = i
			current = fields
		}
	}
	if req.Action == "widget_remove" {
		if index < 0 {
			return widgets, false, nil
		}
		return append(widgets[:index:index], widgets[index+1:]...), true, nil
	}
	if req.Action != "widget_add" && index < 0 {
		return nil, false, fmt.Errorf("widget not found in this Page")
	}
	component := req.Component
	var installID int64
	if req.Action != "widget_add" {
		_ = json.Unmarshal(current["component"], &component)
		_ = json.Unmarshal(current["install_id"], &installID)
	} else {
		installID = req.InstallID
	}
	var selected *dashboardWidgetDefinition
	for i := range definitions {
		definition := &definitions[i]
		if definition.Component == component && (installID == 0 || definition.InstallID == installID) {
			selected = definition
			break
		}
	}
	if selected == nil {
		return nil, false, fmt.Errorf("component is not installed or embeddable in this Page")
	}

	if installID != 0 && selected.InstallID != installID {
		return nil, false, fmt.Errorf("widget installation mismatch")
	}
	target := map[string]json.RawMessage{}
	if req.Action != "widget_add" {
		for key, value := range current {
			target[key] = value
		}
	}
	target["id"], _ = json.Marshal(req.WidgetID)
	target["component"], _ = json.Marshal(component)
	if selected.InstallID > 0 {
		target["install_id"], _ = json.Marshal(selected.InstallID)
	}
	size := selected.DefaultSize
	if req.Action != "widget_add" {
		_ = json.Unmarshal(target["size"], &size)
	}
	if req.Size != nil {
		size = *req.Size
	}
	if !containsString(selected.SupportedSizes, size) {
		return nil, false, fmt.Errorf("unsupported widget size")
	}
	target["size"], _ = json.Marshal(size)
	if req.Placement != nil {
		if *req.Placement != "" && !containsString([]string{"assistant", "main", "activity", "details"}, *req.Placement) {
			return nil, false, fmt.Errorf("invalid workspace placement")
		}
		if *req.Placement == "" {
			delete(target, "placement")
		} else {
			target["placement"], _ = json.Marshal(*req.Placement)
		}
	}
	if req.Action == "widget_add" || req.Action == "widget_configure" {
		settings := map[string]any{}
		for key, value := range selected.DefaultSettings {
			settings[key] = value
		}
		if req.Action != "widget_add" && len(target["settings"]) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(target["settings"]))
			decoder.UseNumber()
			if decoder.Decode(&settings) != nil || settings == nil {
				return nil, false, fmt.Errorf("saved settings must be an object")
			}
		}
		if len(req.Settings) > 0 {
			var patch map[string]any
			decoder := json.NewDecoder(bytes.NewReader(req.Settings))
			decoder.UseNumber()
			if decoder.Decode(&patch) != nil || patch == nil {
				return nil, false, fmt.Errorf("settings must be an object")
			}
			if len(patch) > 0 && len(selected.SettingsSchema) == 0 {
				return nil, false, fmt.Errorf("component does not declare configurable settings")
			}
			for key, value := range patch {
				settings[key] = value
			}
		}
		if err := sdk.ValidateWidgetSettings(settings, selected.SettingsSchema); err != nil {
			return nil, false, err
		}
		target["settings"], _ = json.Marshal(settings)
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return nil, false, err
	}
	if req.Action == "widget_add" && index >= 0 {
		// Compare only fields owned by add so later host metadata is preserved.
		same := true
		for key, value := range target {
			if key == "settings" && len(current[key]) == 0 && string(value) == "{}" {
				continue
			}
			if !equalWidgetJSON(current[key], value) {
				same = false
				break
			}
		}
		if same {
			return widgets, false, nil
		}
		return nil, false, fmt.Errorf("%w: widget_id already exists with different configuration", errUILayoutConflict)
	}
	if req.Action == "widget_configure" {
		if equalWidgetJSON(widgets[index], encoded) {
			return widgets, false, nil
		}
		out := append([]json.RawMessage(nil), widgets...)
		out[index] = encoded
		return out, true, nil
	}
	if req.Action == "widget_move" && req.Position == nil && req.Placement == nil {
		return nil, false, fmt.Errorf("position or placement is required")
	}
	out := append([]json.RawMessage{}, widgets...)
	position := len(out)
	if req.Action == "widget_move" {
		out = append(out[:index:index], out[index+1:]...)
		position = index
	}
	if req.Position != nil {
		position = *req.Position
	}
	if position < 0 || position > len(out) {
		return nil, false, fmt.Errorf("position is outside the Page layout")
	}
	out = append(out, nil)
	copy(out[position+1:], out[position:])
	out[position] = encoded
	return out, !equalWidgetJSON(mustWidgetArray(widgets), mustWidgetArray(out)), nil
}

func mustWidgetArray(widgets []json.RawMessage) json.RawMessage {
	value, _ := json.Marshal(widgets)
	return value
}
func equalWidgetJSON(a, b json.RawMessage) bool {
	var left, right any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	return da.Decode(&left) == nil && db.Decode(&right) == nil && reflect.DeepEqual(left, right)
}

func (s *Server) resolvePageWidgetInstallation(req *workspacePageRequest) error {
	if req.InstallID < 0 {
		return fmt.Errorf("invalid installation selector")
	}
	parts := strings.SplitN(req.Component, ":", 2)
	if len(parts) != 2 || parts[0] == "native" {
		return nil
	}
	if s.installedApps == nil {
		return fmt.Errorf("app is not installed")
	}
	var app *InstalledApp
	if req.InstallID > 0 {
		app = s.installedApps.Get(req.InstallID)
		if app == nil || (app.ProjectID != "" && app.ProjectID != req.ProjectID) || (parts[0] != app.AppName && parts[0] != widgetAppNamespace(app)) {
			return fmt.Errorf("installation is not available for this component")
		}
	} else {
		app = s.installedApps.GetByNameAndProject(parts[0], req.ProjectID)
	}
	if app != nil {
		req.Component = widgetAppNamespace(app) + ":" + parts[1]
		req.InstallID = app.InstallID
	}
	return nil
}
