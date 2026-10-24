package main

import (
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func widgetSettingsFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	seedPresetProject(t, s, "p")
	seedPresetProject(t, s, "other")
	id := seedAppWithTools(t, s, "metrics", "p", nil)
	ui := &sdk.WidgetSettingsUI{Order: []string{"count"}, Advanced: []string{"enabled"}, Preview: true, Fields: map[string]sdk.WidgetSettingsField{"count": {Discovery: &sdk.WidgetSettingsDiscovery{ConfigFieldDiscovery: sdk.ConfigFieldDiscovery{Route: "/widget-options", ResponsePath: "options", ValueField: "id", LabelField: "name"}}}}}
	m := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "metrics", Version: "1", Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{Name: "metric", Entry: "/ui/Metric.mjs", Slots: []string{"dashboard.home"}, DashboardScopes: []string{"project", "global"}, SettingsUI: ui, SettingsSchema: map[string]any{"type": "object", "required": []string{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 0, "maximum": 10}, "enabled": map[string]any{"type": "boolean"}}}}}}}
	raw, _ := json.Marshal(m)
	s.store.db.Exec(`UPDATE apps SET manifest_json=? WHERE name='metrics'`, string(raw))
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, string(raw), id)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: id, AppName: "metrics", ProjectID: "p", Manifest: m})
	return s, id
}
func TestWidgetSettingsPassthroughAndStoredManifest(t *testing.T) {
	s, id := widgetSettingsFixture(t)
	req := httptest.NewRequest("GET", "/apps?project_id=p", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListApps(rec, req)
	var apps []AppRow
	if err := json.Unmarshal(rec.Body.Bytes(), &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].UIComponents[0].SettingsUI == nil || !apps[0].UIComponents[0].SettingsUI.Preview {
		t.Fatal("app response lost settings_ui")
	}
	defs := s.installedWidgetDefinitionsForSlot("p", "dashboard.home", false)
	if len(defs) != 1 || defs[0].InstallID != id || !reflect.DeepEqual(defs[0].SettingsUI, apps[0].UIComponents[0].SettingsUI) {
		t.Fatal("widget projection lost settings_ui")
	}
	req = httptest.NewRequest("GET", "/ui/contributions?project_id=p&surface=dashboard.home", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleUIContributions(rec, req)
	if !strings.Contains(rec.Body.String(), `"settings_ui"`) {
		t.Fatal("eligibility response lost metadata")
	}
}
func TestWidgetSettingsNormalSavesValidateOnlyChangedKnownSettings(t *testing.T) {
	s, _ := widgetSettingsFixture(t)
	patch := func(project string, settings any, revision string) int {
		req := authedRequest(t, "PATCH", "/ui-layout/projects/"+project+"/surfaces/dashboard.home", "", map[string]any{"value": []any{map[string]any{"id": "one", "component": "metrics:metric", "size": "half", "settings": settings}}})
		if revision != "" {
			req.Header.Set("If-Match", revision)
		}
		rec := httptest.NewRecorder()
		s.handleUILayoutSurface(rec, req)
		return rec.Code
	}
	if got := patch("p", map[string]any{"count": 0, "enabled": false}, ""); got != 200 {
		t.Fatalf("valid typed settings status=%d", got)
	}
	before, _ := s.store.GetUserUILayoutWithRevision(1)
	for _, settings := range []any{map[string]any{"count": "2"}, map[string]any{"count": 11}, map[string]any{"count": 1.5}, map[string]any{}, "invalid", []any{}} {
		if got := patch("p", settings, ""); got != 400 {
			t.Fatalf("invalid settings accepted: %v status=%d", settings, got)
		}
	}
	after, _ := s.store.GetUserUILayoutWithRevision(1)
	if string(before) != string(after) {
		t.Fatal("failed save changed layout")
	}
	if got := patch("p", map[string]any{"count": 2}, "0"); got != 409 {
		t.Fatalf("stale revision status=%d", got)
	}
	// Unavailable components in another project remain stored without trying to
	// validate them against a different installation's private schema.
	if got := patch("other", map[string]any{"count": "legacy"}, ""); got != 200 {
		t.Fatalf("unavailable widget lost: %d", got)
	}
	// Legacy invalid values may be resized or reordered without being erased.
	raw := json.RawMessage(`["legacy:component",{"id":"one","component":"metrics:metric","size":"half","settings":{"count":"legacy"}}]`)
	if _, _, err := s.store.PatchUserUILayoutSurface(1, "p", "dashboard.home", raw, nil); err != nil {
		t.Fatal(err)
	}
	if got := patch("p", map[string]any{"count": "legacy"}, ""); got != 200 {
		t.Fatal("unchanged legacy settings blocked")
	}
}
func TestWidgetSettingsGlobalValidationUsesGlobalInstallation(t *testing.T) {
	s, id := widgetSettingsFixture(t)
	app := s.installedApps.Get(id)
	app.ProjectID = ""
	s.installedApps.Add(app)
	for _, tc := range []struct {
		count any
		want  int
	}{{2, 200}, {"2", 400}} {
		req := authedRequest(t, "PATCH", "/ui-layout/global/surfaces/dashboard.home", "", map[string]any{"value": []any{map[string]any{"id": "g", "component": "metrics:metric", "settings": map[string]any{"count": tc.count}}}})
		rec := httptest.NewRecorder()
		s.handleUILayoutGlobalSurface(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("global count=%v status=%d body=%s", tc.count, rec.Code, rec.Body.String())
		}
	}
}
