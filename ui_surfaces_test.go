package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestDashboardHomeSurfaceResolvesNativeAppsAndPortableLayout(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("native-widgets@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(user.ID, "Native widgets", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{
		InstallID: 42, AppName: "tasks", ProjectID: project.ID,
		Manifest: sdk.Manifest{Name: "tasks", DisplayName: "Tasks", Version: "1.0.0", Icon: "/ui/icon.svg", Provides: sdk.Provides{
			UISurfaces: []sdk.UISurface{{ID: "tasks", Label: "Tasks", Icon: "list-checks", Schema: sdk.NativeSurfaceSchemaCurrent, Entry: "/ui/surfaces/tasks.json", Slots: []string{sdk.UISurfaceSlotMobileProjectApp}}},
			UIComponents: []sdk.UIComponent{{
				Name: "overview", Entry: "/ui/Overview.mjs", Slots: []string{sdk.UIComponentSlotDashboardHome},
				Label: "Tasks", SupportedSizes: []string{"half", "full"}, DefaultSize: "half",
				RefreshTopics:  []string{"task.updated"},
				SettingsSchema: map[string]any{"properties": map[string]any{"show_recent": map[string]any{"type": "boolean", "default": true}}},
				Native:         &sdk.NativeUIRenderer{Schema: sdk.NativeSurfaceSchemaCurrent, Entry: "/ui/surfaces/overview.json"},
			}},
		}},
	})
	if err := s.store.SetUserUILayout(user.ID, json.RawMessage(`{"projects":{"`+project.ID+`":{"slots":{"dashboard.home":[{"id":"tasks-one","component":"tasks:overview","size":"full","settings":{"show_recent":false}}]}}}}`)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/surfaces/dashboard.home?project_id="+project.ID, nil)
	req.Header.Set("X-User-ID", itoa(user.ID))
	rec := httptest.NewRecorder()
	s.handleUISurfaceResolution(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Revision    int64                       `json:"revision"`
		Layout      []dashboardWidgetInstance   `json:"layout"`
		Definitions []dashboardWidgetDefinition `json:"definitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Revision != 1 {
		t.Fatalf("revision=%d", response.Revision)
	}
	if got := []string{response.Layout[0].Component, response.Layout[1].Component, response.Layout[2].Component, response.Layout[3].Component}; got[0] != "native:usage" || got[1] != "native:inbox" || got[2] != "tasks:overview" || got[3] != "native:activity" {
		t.Fatalf("layout=%+v", response.Layout)
	}
	var tasks *dashboardWidgetDefinition
	for index := range response.Definitions {
		if response.Definitions[index].Component == "tasks:overview" {
			tasks = &response.Definitions[index]
		}
	}
	if tasks == nil || tasks.Native == nil || tasks.Native.Entry != "/ui/surfaces/overview.json" || tasks.DefaultSettings["show_recent"] != true {
		t.Fatalf("tasks definition=%+v", tasks)
	}
	if len(tasks.ProjectSurfaces) != 1 || tasks.ProjectSurfaces[0].ID != "tasks" {
		t.Fatalf("tasks project surfaces=%+v", tasks.ProjectSurfaces)
	}
}

func TestDashboardHomeSurfaceKeepsExplicitEmptyLayout(t *testing.T) {
	projectID := "project-a"
	document := json.RawMessage(`{"projects":{"project-a":{"slots":{"dashboard.home":[]}}}}`)
	if got := resolvedDashboardHomeLayout(document, projectID); len(got) != 0 {
		t.Fatalf("layout=%+v", got)
	}
}
