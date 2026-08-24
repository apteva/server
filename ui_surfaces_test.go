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
	if len(response.Layout) != 1 || response.Layout[0].Component != "tasks:overview" {
		t.Fatalf("layout=%+v", response.Layout)
	}
	builtins := map[string]bool{}
	var tasks *dashboardWidgetDefinition
	for index := range response.Definitions {
		if response.Definitions[index].Kind == "builtin" {
			builtins[response.Definitions[index].Component] = true
		}
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
	if !builtins["native:usage"] || !builtins["native:activity"] || builtins["native:inbox"] {
		t.Fatalf("builtins=%+v", builtins)
	}
}

func TestDashboardHomeSurfaceDefaultsToEmpty(t *testing.T) {
	for name, document := range map[string]json.RawMessage{
		"empty document":   json.RawMessage(`{}`),
		"missing project":  json.RawMessage(`{"projects":{"other":{}}}`),
		"missing slot":     json.RawMessage(`{"projects":{"project-a":{"slots":{}}}}`),
		"invalid document": json.RawMessage(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			if got := resolvedDashboardHomeLayout(document, "project-a"); len(got) != 0 {
				t.Fatalf("layout=%+v", got)
			}
		})
	}
}

func TestDashboardHomeSurfaceKeepsExplicitEmptyLayout(t *testing.T) {
	projectID := "project-a"
	document := json.RawMessage(`{"projects":{"project-a":{"slots":{"dashboard.home":[]}}}}`)
	if got := resolvedDashboardHomeLayout(document, projectID); len(got) != 0 {
		t.Fatalf("layout=%+v", got)
	}
}

func TestDashboardHomeSurfacePreservesExplicitLayoutWithoutMergingDefaults(t *testing.T) {
	document := json.RawMessage(`{"projects":{"project-a":{"slots":{"dashboard.home":["native:inbox",{"id":"tasks","component":"tasks:overview","size":"full"}]}}}}`)
	got := resolvedDashboardHomeLayout(document, "project-a")
	if len(got) != 1 || got[0].Component != "tasks:overview" || got[0].Size != "full" {
		t.Fatalf("layout=%+v", got)
	}
}
