package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testPresetDefinition() ProjectSetupPresetDefinition {
	return ProjectSetupPresetDefinition{
		Category:        "work",
		Agents:          []ProjectPresetAgent{{Key: "operator", Name: "Operator", Directive: "Operate the project.", Mode: "cautious"}},
		DashboardLayout: []dashboardWidgetInstance{{ID: "captured:1", Component: "native:usage", Size: "full", Settings: map[string]any{"window": "7d"}}},
	}
}

func TestGenericPresetCRUDAndSystemReadOnly(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)

	create := authedRequest(t, http.MethodPost, "/presets", "", map[string]any{
		"name": "Operations", "description": "Reusable operations setup", "scope": "personal",
		"definition": testPresetDefinition(),
	})
	w := httptest.NewRecorder()
	s.handlePresets(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created Preset
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != projectSetupPresetKind || created.SchemaVersion != 2 || created.Source != "user" || created.OwnerID != 1 {
		t.Fatalf("created preset metadata=%+v", created)
	}

	list := authedRequest(t, http.MethodGet, "/presets", "", nil)
	w = httptest.NewRecorder()
	s.handlePresets(w, list)
	var catalog struct {
		Presets []Preset `json:"presets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presets) != 20 {
		t.Fatalf("catalog has %d presets, want 19 system + 1 personal", len(catalog.Presets))
	}

	patch := authedRequest(t, http.MethodPatch, "/presets/"+created.ID, "", map[string]any{"description": ""})
	w = httptest.NewRecorder()
	s.handlePresetByID(w, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", w.Code, w.Body.String())
	}
	var updated Preset
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Description != "" {
		t.Fatalf("description was not cleared: %q", updated.Description)
	}

	systemGet := authedRequest(t, http.MethodGet, "/presets/personal-assistant", "", nil)
	w = httptest.NewRecorder()
	s.handlePresetByID(w, systemGet)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"system"`) {
		t.Fatalf("system GET status=%d body=%s", w.Code, w.Body.String())
	}
	systemDelete := authedRequest(t, http.MethodDelete, "/presets/personal-assistant", "", nil)
	w = httptest.NewRecorder()
	s.handlePresetByID(w, systemDelete)
	if w.Code != http.StatusForbidden {
		t.Fatalf("system DELETE status=%d, want 403", w.Code)
	}

	remove := authedRequest(t, http.MethodDelete, "/presets/"+created.ID, "", nil)
	w = httptest.NewRecorder()
	s.handlePresetByID(w, remove)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPresetPersonalVisibilityAndSharedAdminGate(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES(2,'member@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	definition, _ := json.Marshal(testPresetDefinition())
	if _, err := s.store.insertPreset(storedPreset{ID: "usr-1-private", UserID: 1, Kind: projectSetupPresetKind, Scope: "personal", SchemaVersion: 2, Name: "Private", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.insertPreset(storedPreset{ID: "shared-company", UserID: 1, Kind: projectSetupPresetKind, Scope: "shared", SchemaVersion: 2, Name: "Company", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	catalog, err := s.genericPresetCatalog(2)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, preset := range catalog {
		seen[preset.ID] = true
	}
	if seen["usr-1-private"] || !seen["shared-company"] {
		t.Fatalf("member visibility=%v", seen)
	}

	request := authedRequest(t, http.MethodPost, "/presets", "", map[string]any{
		"name": "Unauthorized shared", "scope": "shared", "definition": testPresetDefinition(),
	})
	request.Header.Set("X-User-ID", "2")
	w := httptest.NewRecorder()
	s.handlePresets(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin shared create status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCaptureProjectPresetIsPortableAndExcludesRuntimeData(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "capture-project")
	result, err := s.store.db.Exec(`INSERT INTO agents(user_id,name,directive,mode,config,status,project_id,kind)
		VALUES(1,'Research Agent','Research carefully.','cautious',?,'running','capture-project','user')`,
		`{"unconscious":true,"api_key":"must-not-leak","threads":[{"id":"runtime-thread"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := result.LastInsertId()
	appResult, err := s.store.db.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES('notes','builtin','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := appResult.LastInsertId()
	installResult, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status) VALUES(?,'capture-project','running')`, appID)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := installResult.LastInsertId()
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, agentID); err != nil {
		t.Fatal(err)
	}
	layout := json.RawMessage(`{"projects":{"capture-project":{"slots":{"dashboard.home":[{"id":"mine","component":"native:usage","size":"full","settings":{"window":"30d"}}]}}}}`)
	if err := s.store.SetUserUILayout(1, layout); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPost, "/presets/capture", "", map[string]any{
		"project_id": "capture-project", "name": "Captured research", "description": "Portable setup", "category": "work", "scope": "personal",
	})
	w := httptest.NewRecorder()
	s.handlePresetCapture(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "must-not-leak") || strings.Contains(w.Body.String(), "runtime-thread") || strings.Contains(w.Body.String(), "agent_id") || strings.Contains(w.Body.String(), "install_id") {
		t.Fatalf("captured runtime data leaked: %s", w.Body.String())
	}
	var preset Preset
	if err := json.Unmarshal(w.Body.Bytes(), &preset); err != nil {
		t.Fatal(err)
	}
	if len(preset.Definition.Agents) != 1 || !preset.Definition.Agents[0].Unconscious || !containsString(preset.Definition.Agents[0].Apps, "notes") {
		t.Fatalf("captured agent=%+v", preset.Definition.Agents)
	}
	if len(preset.Definition.DashboardLayout) != 1 || preset.Definition.DashboardLayout[0].Settings["window"] != "30d" {
		t.Fatalf("captured layout=%+v", preset.Definition.DashboardLayout)
	}

	projectPreset := projectPresetFromEnvelope(preset)
	compiled, warnings := s.compileProjectPresetDashboardLayout("capture-project", projectPreset)
	if len(warnings) != 0 || len(compiled) != 1 || !strings.HasPrefix(compiled[0].ID, "preset:"+preset.ID+":") {
		t.Fatalf("compiled layout=%+v warnings=%v", compiled, warnings)
	}
	// Applying back to the source project recognizes the equivalent manual
	// widget instead of adding a duplicate. Applying to an empty target adds it
	// once, and reapplication is revision-idempotent.
	_, sourceRevision := s.store.GetUserUILayoutWithRevision(1)
	if err := s.mergeProjectPresetDashboardLayout(1, "capture-project", compiled); err != nil {
		t.Fatal(err)
	}
	_, afterSourceRevision := s.store.GetUserUILayoutWithRevision(1)
	if afterSourceRevision != sourceRevision {
		t.Fatalf("source layout duplicated; revision %d -> %d", sourceRevision, afterSourceRevision)
	}
	seedPresetProject(t, s, "capture-target")
	if err := s.mergeProjectPresetDashboardLayout(1, "capture-target", compiled); err != nil {
		t.Fatal(err)
	}
	_, targetRevision := s.store.GetUserUILayoutWithRevision(1)
	if err := s.mergeProjectPresetDashboardLayout(1, "capture-target", compiled); err != nil {
		t.Fatal(err)
	}
	_, targetRevisionAfter := s.store.GetUserUILayoutWithRevision(1)
	if targetRevisionAfter != targetRevision {
		t.Fatalf("reapply changed revision %d -> %d", targetRevision, targetRevisionAfter)
	}
}

func TestProjectTemplatesAreProjectOwnedAndOnboardingIsSystemOnly(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedPresetProject(t, s, "template-owner")

	capture := authedRequest(t, http.MethodPost, "/projects/template-owner/templates/capture", "", map[string]any{
		"name": "Client delivery", "description": "Project-owned setup", "category": "business",
	})
	w := httptest.NewRecorder()
	s.handleProjectTemplates(w, capture, "template-owner", "templates/capture")
	if w.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", w.Code, w.Body.String())
	}
	var created Preset
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Scope != "project" || created.OwnerProjectID != "template-owner" || created.Revision != 1 {
		t.Fatalf("template ownership=%+v", created)
	}
	if _, err := s.store.db.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES(2,'viewer@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	blocked := authedRequest(t, http.MethodGet, "/templates/"+created.ID, "", nil)
	blocked.Header.Set("X-User-ID", "2")
	w = httptest.NewRecorder()
	s.handlePresetByID(w, blocked)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-member read status=%d", w.Code)
	}
	if _, err := s.store.db.Exec(`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES('template-owner',2,'viewer',1)`); err != nil {
		t.Fatal(err)
	}
	viewerEdit := authedRequest(t, http.MethodPatch, "/templates/"+created.ID, "", map[string]any{"name": "Nope"})
	viewerEdit.Header.Set("X-User-ID", "2")
	w = httptest.NewRecorder()
	s.handlePresetByID(w, viewerEdit)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer edit status=%d body=%s", w.Code, w.Body.String())
	}

	list := authedRequest(t, http.MethodGet, "/projects/template-owner/templates", "", nil)
	w = httptest.NewRecorder()
	s.handleProjectTemplates(w, list, "template-owner", "templates")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ID) || !strings.Contains(w.Body.String(), `"source":"system"`) {
		t.Fatalf("project catalog status=%d body=%s", w.Code, w.Body.String())
	}

	onboarding := authedRequest(t, http.MethodGet, "/templates?system_only=true", "", nil)
	w = httptest.NewRecorder()
	s.handlePresets(w, onboarding)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), created.ID) || !strings.Contains(w.Body.String(), `"templates"`) {
		t.Fatalf("onboarding catalog status=%d body=%s", w.Code, w.Body.String())
	}

	globalCreate := authedRequest(t, http.MethodPost, "/templates", "", map[string]any{
		"name": "Unowned", "definition": testPresetDefinition(),
	})
	w = httptest.NewRecorder()
	s.handlePresets(w, globalCreate)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unowned create status=%d body=%s", w.Code, w.Body.String())
	}
}
