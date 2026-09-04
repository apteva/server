package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedPresetProject(t *testing.T, s *Server, id string) {
	t.Helper()
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO projects(id,user_id,name,description,color) VALUES(?,1,'Default','','#6366f1')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members(project_id,user_id,role,added_by) VALUES(?,1,'owner',1)`, id); err != nil {
		t.Fatal(err)
	}
}

func TestProjectPresetCatalogIsVersionedAndContainsFourCategories(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presets) != 19 {
		t.Fatalf("got %d presets, want 19", len(catalog.Presets))
	}
	counts := map[string]int{}
	knownApps := map[string]bool{
		"conversations": true,
		"tasks":         true, "todo": true, "notes": true, "calendar": true,
		"pantry": true, "recipes": true, "health": true, "content": true,
		"social": true, "media-studio": true, "storage": true, "analytics": true,
		"docs": true, "crm": true, "web": true, "email-checker": true,
		"campaigns": true, "bookings": true, "tickets": true, "tables": true,
		"code": true, "environments": true, "deploy": true, "instances": true,
		"containers": true, "fleet": true, "backup": true, "computer": true,
		"screenshots": true, "signatures": true, "billing": true, "commerce": true,
		"catalog": true, "inventory": true, "orders": true, "messaging": true,
		"webinars": true, "builder": true, "workspaces": true, "a2a": true,
		"media-downloader": true, "media": true,
	}
	requiredDomainApps := map[string][]string{
		"personal-assistant":             {"todo", "notes", "calendar"},
		"personal-household":             {"todo", "pantry", "recipes"},
		"personal-wellbeing":             {"health", "notes", "calendar"},
		"personal-creator":               {"content", "social", "media-studio", "analytics"},
		"work-executive":                 {"notes", "calendar", "docs"},
		"work-sales":                     {"crm", "web", "email-checker", "campaigns", "bookings"},
		"work-support":                   {"tickets", "crm", "storage"},
		"work-research":                  {"web", "notes", "tables"},
		"work-youtube-to-blog":           {"media-downloader", "media", "storage", "content"},
		"development-software":           {"code", "environments", "deploy"},
		"development-engineering-team":   {"builder", "tasks", "code", "workspaces", "a2a", "deploy"},
		"development-devops":             {"deploy", "instances", "containers", "backup"},
		"development-qa":                 {"code", "environments", "computer", "screenshots"},
		"development-data":               {"tables", "analytics", "storage"},
		"business-lead-generation":       {"crm", "web", "email-checker", "campaigns", "analytics", "bookings"},
		"business-professional-services": {"crm", "bookings", "docs", "signatures", "billing"},
		"business-ecommerce":             {"commerce", "catalog", "inventory", "orders", "analytics"},
		"business-local-services":        {"crm", "bookings", "messaging", "billing"},
		"business-webinars":              {"webinars", "crm", "messaging", "campaigns"},
	}
	for _, preset := range catalog.Presets {
		counts[preset.Category]++
		if len(preset.Highlights) < 2 {
			t.Fatalf("preset %s must describe what it enables: %v", preset.ID, preset.Highlights)
		}
		if len(preset.Agents) == 0 {
			t.Fatalf("preset %s has no starter agent", preset.ID)
		}
		presetApps := projectPresetApps(preset)
		if !containsString(presetApps, defaultConversationsApp) {
			t.Fatalf("preset %s is missing the Conversations app: %v", preset.ID, presetApps)
		}
		for _, agent := range preset.Agents {
			if !containsString(agent.Apps, defaultConversationsApp) {
				t.Fatalf("preset %s agent %s is missing Conversations: %v", preset.ID, agent.Key, agent.Apps)
			}
		}
		for _, app := range presetApps {
			if !knownApps[app] {
				t.Fatalf("preset %s assigns unknown app %q", preset.ID, app)
			}
		}
		for _, required := range requiredDomainApps[preset.ID] {
			if !containsString(presetApps, required) {
				t.Fatalf("preset %s must assign its domain app %q: %v", preset.ID, required, presetApps)
			}
		}
		conversationWidgets := 0
		for _, component := range preset.Dashboard {
			if component == "native:inbox" {
				t.Fatalf("preset %s still exposes the retired native inbox", preset.ID)
			}
			if component == defaultConversationsWidget {
				conversationWidgets++
			}
			if strings.HasPrefix(component, "tasks:") && component != "tasks:task-overview" {
				t.Fatalf("preset %s uses an unknown Tasks widget %q", preset.ID, component)
			}
		}
		if conversationWidgets != 1 {
			t.Fatalf("preset %s has %d Conversations widgets, want 1: %v", preset.ID, conversationWidgets, preset.Dashboard)
		}
	}
	// Work, development, and business each include a specialist preset.
	wantCounts := map[string]int{"personal": 4, "work": 5, "development": 5, "business": 5}
	for category, want := range wantCounts {
		if counts[category] != want {
			t.Fatalf("category %s has %d presets, want %d", category, counts[category], want)
		}
	}
	youtube := catalog.ByID["work-youtube-to-blog"]
	if len(youtube.Agents) != 1 || youtube.Agents[0].Unconscious || !strings.Contains(youtube.Agents[0].Directive, "Never publish") {
		t.Fatalf("youtube-to-blog safety contract is incomplete: %+v", youtube)
	}
}

func TestProjectPresetDeterministicPlannerClassifiesEngineeringTeam(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	preset, confidence := deterministicProjectPreset("development", "I want an engineering team to build software with agents", catalog.Presets)
	if preset.ID != "development-engineering-team" {
		t.Fatalf("selected %q, want development-engineering-team", preset.ID)
	}
	if confidence <= 0.5 {
		t.Fatalf("confidence=%v, want a meaningful match", confidence)
	}
}

func TestProjectPresetDeterministicPlannerClassifiesDoctorLeadGeneration(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	preset, confidence := deterministicProjectPreset("business", "I run a doctor lead-generation company", catalog.Presets)
	if preset.ID != "business-lead-generation" {
		t.Fatalf("selected %q, want business-lead-generation", preset.ID)
	}
	if confidence <= 0.5 {
		t.Fatalf("confidence=%v, want a meaningful match", confidence)
	}
}

func TestProjectPresetDeterministicPlannerClassifiesWebinars(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	preset, confidence := deterministicProjectPreset("business", "I want a space to host webinars and online workshops for my course", catalog.Presets)
	if preset.ID != "business-webinars" {
		t.Fatalf("selected %q, want business-webinars", preset.ID)
	}
	if confidence <= 0.5 {
		t.Fatalf("confidence=%v, want a meaningful match", confidence)
	}
}

func TestProjectPresetPreviewUsesConstrainedPlannerAndResolvesProjectApps(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "preset-project")
	crmInstallID := seedAppWithTools(t, s, "crm", "preset-project", []string{"contacts_list"})
	if err := s.registerAppMCP(crmInstallID); err != nil {
		t.Fatal(err)
	}
	s.projectPresetPlanner = func(_ context.Context, _ int64, _, _ string, _ []ProjectPreset) (projectPresetPlanChoice, error) {
		return projectPresetPlanChoice{PresetID: "business-lead-generation", Confidence: 0.91}, nil
	}
	preview, err := s.compileProjectPresetPreview(context.Background(), 1, "preset-project", ProjectPresetPreviewRequest{
		Description: "A company helping medical practices find qualified patients",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Planner != "meta" || preview.Preset.ID != "business-lead-generation" {
		t.Fatalf("unexpected plan: planner=%s preset=%s", preview.Planner, preview.Preset.ID)
	}
	if len(preview.Agents) != 1 {
		t.Fatalf("agents=%d, want 1", len(preview.Agents))
	}
	if !containsInt64(preview.Agents[0].AppInstallIDs, crmInstallID) {
		t.Fatalf("CRM install %d not attached: %+v", crmInstallID, preview.Agents[0])
	}
	if strings.Contains(preview.Agents[0].Directive, "{{") {
		t.Fatalf("directive still contains an unresolved placeholder: %q", preview.Agents[0].Directive)
	}
	if !strings.Contains(preview.Agents[0].Directive, "medical practices find qualified patients") {
		t.Fatalf("project description was not placed in the agent directive: %q", preview.Agents[0].Directive)
	}
	foundMissingTasks := false
	for _, warning := range preview.Warnings {
		foundMissingTasks = foundMissingTasks || strings.Contains(warning, "tasks is assigned by the preset")
	}
	if !foundMissingTasks {
		t.Fatalf("missing Tasks recommendation should be explicit: %v", preview.Warnings)
	}
}

func TestProjectPresetApplyUsesNormalAgentContractAndIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "apply-project")
	crmInstallID := seedAppWithTools(t, s, "crm", "apply-project", []string{"contacts_list"})
	if err := s.registerAppMCP(crmInstallID); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"preset_id":   "business-lead-generation",
		"description": "Find and qualify leads for independent clinics; never send outreach without approval",
	}

	apply := func() map[string]any {
		req := authedRequest(t, http.MethodPost, "/projects/apply-project/setup/apply", "", body)
		rec := httptest.NewRecorder()
		s.handleProject(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := apply()
	if got := len(first["created_agents"].([]any)); got != 1 {
		t.Fatalf("created_agents=%d, want 1: %#v", got, first)
	}
	foundStoppedWarning := false
	for _, raw := range first["warnings"].([]any) {
		foundStoppedWarning = foundStoppedWarning || strings.Contains(raw.(string), "no LLM provider configured")
	}
	if !foundStoppedWarning {
		t.Fatalf("stopped-agent warning was lost: %#v", first["warnings"])
	}
	agents, err := s.store.ListAgentsInProject("apply-project")
	if err != nil || len(agents) != 1 {
		t.Fatalf("project agents=%d err=%v", len(agents), err)
	}
	fresh, err := s.store.GetAgentByID(agents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(fresh.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if !hasMCPName(mcpMaps(cfg["mcp_servers"]), "crm") {
		t.Fatalf("preset agent did not use normal app attachment contract: %s", fresh.Config)
	}
	project, err := s.store.GetProjectAny("apply-project")
	if err != nil || project.Name != "Default" || project.Description != body["description"] {
		t.Fatalf("project=%+v err=%v", project, err)
	}
	layout := string(s.store.GetUserUILayout(1))
	if !strings.Contains(layout, `"dashboard.home"`) || !strings.Contains(layout, `"native:usage"`) || !strings.Contains(layout, `"native:activity"`) {
		t.Fatalf("preset dashboard widgets were not applied: %s", layout)
	}
	if strings.Contains(layout, `"native:inbox"`) {
		t.Fatalf("retired inbox widget was applied: %s", layout)
	}
	_, firstLayoutRevision := s.store.GetUserUILayoutWithRevision(1)
	second := apply()
	if got := len(second["created_agents"].([]any)); got != 0 {
		t.Fatalf("second apply created %d duplicate agents: %#v", got, second)
	}
	if got := len(second["existing_agents"].([]any)); got != 1 {
		t.Fatalf("second apply existing_agents=%d, want 1", got)
	}
	_, secondLayoutRevision := s.store.GetUserUILayoutWithRevision(1)
	if secondLayoutRevision != firstLayoutRevision {
		t.Fatalf("idempotent reapply changed layout revision from %d to %d", firstLayoutRevision, secondLayoutRevision)
	}
}

func TestMergeProjectPresetDashboardLayoutPreservesExistingWidgets(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "merge-layout-project")
	existing := json.RawMessage(`{"projects":{"merge-layout-project":{"slots":{"dashboard.home":[{"id":"custom","component":"custom:overview","size":"half","settings":{"mode":"mine"}},{"id":"usage","component":"native:usage","size":"full"}]}}}}`)
	if err := s.store.SetUserUILayout(1, existing); err != nil {
		t.Fatal(err)
	}
	preset := []dashboardWidgetInstance{
		{ID: "preset:native:usage", Component: "native:usage", Size: "full"},
		{ID: "custom", Component: "native:activity", Size: "full"},
		{ID: "preset:native:inbox", Component: "native:inbox", Size: "half"},
	}
	if err := s.mergeProjectPresetDashboardLayout(1, "merge-layout-project", preset); err != nil {
		t.Fatal(err)
	}
	document, revision := s.store.GetUserUILayoutWithRevision(1)
	got := resolvedDashboardHomeLayout(document, "merge-layout-project")
	if len(got) != 3 {
		t.Fatalf("layout=%+v", got)
	}
	if got[0].Component != "custom:overview" || got[0].Settings["mode"] != "mine" || got[1].Component != "native:usage" || got[2].Component != "native:activity" {
		t.Fatalf("layout order or settings changed: %+v", got)
	}
	if got[2].ID == "custom" {
		t.Fatalf("preset widget reused an existing id: %+v", got)
	}
	if err := s.mergeProjectPresetDashboardLayout(1, "merge-layout-project", preset); err != nil {
		t.Fatal(err)
	}
	_, secondRevision := s.store.GetUserUILayoutWithRevision(1)
	if secondRevision != revision {
		t.Fatalf("idempotent merge changed revision from %d to %d", revision, secondRevision)
	}
}

func TestProjectPresetPlannerRejectsInventedShape(t *testing.T) {
	if _, err := parseProjectPresetPlanChoice(`{"preset_id":"business-lead-generation","run_tool":"install"}`); err == nil {
		t.Fatal("unknown planner fields must be rejected")
	}
	choice, err := parseProjectPresetPlanChoice("```json\n{\"preset_id\":\"business-lead-generation\",\"confidence\":0.8}\n```")
	if err != nil || choice.PresetID != "business-lead-generation" {
		t.Fatalf("choice=%+v err=%v", choice, err)
	}
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestProjectPresetApplyAutoInstallsMissingRegistryApps is the one-click
// contract: an admin applying a preset gets the preset's apps installed
// from the registry, not a pile of "not installed" warnings. The
// registry and manifest are stubbed; the app is kind=static with an
// absolute static_dir so the install completes synchronously without a
// clone or build.
func TestProjectPresetApplyAutoInstallsMissingRegistryApps(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "webinar-project")
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.staticMounts = newStaticAppMounts()

	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `schema: apteva-app/v1
name: webinars
display_name: Webinars
version: 0.1.1
scopes: [project, global]
provides:
  mcp_tools:
    - { name: webinar_list, description: List webinars. }
runtime:
  kind: static
  static_dir: %s
`, staticDir)
	}))
	defer manifest.Close()
	conversationsManifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `schema: apteva-app/v1
name: conversations
display_name: Conversations
version: 0.8.0
scopes: [global]
provides:
  mcp_tools:
    - { name: send, description: Send a conversation message. }
  ui_components:
    - name: inbox-overview
      label: Inbox
      entry: /ui/InboxWidget.mjs
      slots: [dashboard.home]
      supported_sizes: [half, full]
      default_size: half
runtime:
  kind: static
  static_dir: %s
`, staticDir)
	}))
	defer conversationsManifest.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apps": []map[string]any{
				{"name": "webinars", "manifest_url": manifest.URL},
				{"name": "conversations", "manifest_url": conversationsManifest.URL},
			},
		})
	}))
	defer registry.Close()
	t.Setenv("APTEVA_APP_REGISTRY_URL", registry.URL)

	body := map[string]any{
		"preset_id":   "business-webinars",
		"description": "Host webinars for my course business",
	}
	req := authedRequest(t, http.MethodPost, "/projects/webinar-project/setup/apply", "", body)
	rec := httptest.NewRecorder()
	s.handleProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The webinars app must now be a running install in this project.
	var installID int64
	var status string
	if err := s.store.db.QueryRow(`
		SELECT i.id, i.status FROM app_installs i
		JOIN apps a ON a.id=i.app_id
		WHERE a.name='webinars' AND i.project_id='webinar-project'`).
		Scan(&installID, &status); err != nil {
		t.Fatalf("webinars was not installed: %v", err)
	}
	if status != "running" {
		t.Fatalf("install status = %q, want running", status)
	}
	var conversationsInstallID int64
	if err := s.store.db.QueryRow(`
		SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE a.name='conversations' AND COALESCE(i.project_id,'')='' AND i.status='running'`).
		Scan(&conversationsInstallID); err != nil {
		t.Fatalf("Conversations was not installed globally: %v", err)
	}
	var conversationsBindings int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND enabled=1`, conversationsInstallID).Scan(&conversationsBindings); err != nil || conversationsBindings != 1 {
		t.Fatalf("Conversations bindings=%d err=%v", conversationsBindings, err)
	}
	if layout := string(s.store.GetUserUILayout(1)); !strings.Contains(layout, defaultConversationsWidget) {
		t.Fatalf("Conversations widget was not added: %s", layout)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// Apps the registry does not carry (crm, messaging, …) degrade to
	// the informative warning instead of failing the apply — the exact
	// pre-auto-install behavior, minus the apps we could fix.
	warnings, _ := result["warnings"].([]any)
	sawRegistryMiss, sawWebinarsWarning, sawConversationsWarning := false, false, false
	for _, raw := range warnings {
		text, _ := raw.(string)
		if strings.Contains(text, "could not be installed from the registry") {
			sawRegistryMiss = true
		}
		if strings.Contains(text, "webinars") {
			sawWebinarsWarning = true
		}
		if strings.Contains(text, "conversations") || strings.Contains(text, "Conversations") {
			sawConversationsWarning = true
		}
	}
	if !sawRegistryMiss {
		t.Fatalf("expected registry-miss warnings for uninstallable apps: %#v", warnings)
	}
	if sawWebinarsWarning {
		t.Fatalf("webinars installed fine and must not be warned about: %#v", warnings)
	}
	if sawConversationsWarning {
		t.Fatalf("Conversations installed fine and must not be warned about: %#v", warnings)
	}
}

// A non-admin project editor keeps the old warn-and-continue behavior:
// applying a preset never becomes a way to install apps without the
// platform-admin capability handleInstallApp requires.
func TestProjectPresetApplyDoesNotInstallForNonAdmins(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "editor-project")
	s.localApps = NewLocalSupervisor(t.TempDir())

	editor, err := s.store.CreateUser("preset-editor@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES('editor-project',?,'editor',1)`,
		editor.ID); err != nil {
		t.Fatal(err)
	}

	// A registry that fails the test if anyone dials it.
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("non-admin apply must not consult the registry")
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer registry.Close()
	t.Setenv("APTEVA_APP_REGISTRY_URL", registry.URL)

	body := map[string]any{
		"preset_id":   "business-webinars",
		"description": "Host webinars for my course business",
	}
	req := authedRequest(t, http.MethodPost, "/projects/editor-project/setup/apply", "", body)
	req.Header.Set("X-User-ID", fmt.Sprint(editor.ID))
	rec := httptest.NewRecorder()
	s.handleProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := s.store.db.QueryRow(`SELECT count(*) FROM app_installs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("non-admin apply installed %d apps, want 0", count)
	}
}
