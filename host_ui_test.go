package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestHostUIPreservedInExistingAppAndContributionResponses(t *testing.T) {
	s := newTestServer(t)
	const projectID = "host-ui-project"
	seedPresetProject(t, s, projectID)
	id := seedAppWithTools(t, s, "host-ui-app", projectID, nil)
	policy := &sdk.HostUI{ChatAssistant: &sdk.HostUIChatAssistant{Launcher: sdk.ChatAssistantLauncherHiddenOnCompact}}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "host-ui-app", Version: "1.0.0", Provides: sdk.Provides{
		UIPanels:     []sdk.UIPanel{{Slot: "project.page", Entry: "/ui/Demo.mjs", HostUI: policy}},
		UIComponents: []sdk.UIComponent{{Name: "demo", Entry: "/ui/Demo.mjs", Slots: []string{sdk.UIComponentSlotDashboardHome}, HostUI: policy}},
		UISurfaces:   []sdk.UISurface{{ID: "demo", Label: "Demo", Icon: "chat", Schema: sdk.NativeSurfaceSchemaCurrent, Entry: "/ui/demo.json", Slots: []string{sdk.UISurfaceSlotMobileProjectApp}, HostUI: policy}},
	}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE apps SET manifest_json=? WHERE name='host-ui-app'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps?project_id="+projectID, nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListApps(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list status=%d: %s", rec.Code, rec.Body.String())
	}
	var rows []AppRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0].UIPanels[0].HostUI, policy) || !reflect.DeepEqual(rows[0].UIComponents[0].HostUI, policy) || !reflect.DeepEqual(rows[0].UISurfaces[0].HostUI, policy) {
		t.Fatalf("policy lost from app metadata: %s", rec.Body.String())
	}
	// Reload from persisted manifest JSON, just as mounting after restart does.
	var restored sdk.Manifest
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: id, AppName: "host-ui-app", ProjectID: projectID, Manifest: restored})
	defs := s.installedDashboardWidgetDefinitions(projectID, false)
	if len(defs) != 1 || !reflect.DeepEqual(defs[0].HostUI, policy) {
		t.Fatal("widget projection lost policy")
	}
	req = httptest.NewRequest(http.MethodGet, "/ui/contributions?project_id="+projectID+"&surface=dashboard.home", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleUIContributions(rec, req)
	var contributions struct {
		Contributions []uiContributionEligibility `json:"contributions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &contributions); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(contributions.Contributions) != 1 || !reflect.DeepEqual(contributions.Contributions[0].HostUI, policy) {
		t.Fatalf("contribution metadata lost policy: %s", rec.Body.String())
	}
}
