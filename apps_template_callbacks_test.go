package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedTemplateCallbackInstall(t *testing.T, s *Server, name, projectID string, permissions ...sdk.Permission) int64 {
	t.Helper()
	manifest := sdk.Manifest{Name: name, Requires: sdk.Requires{Permissions: permissions}}
	installID := seedInstallWithBindings(t, s, name, manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=?,installed_by=1 WHERE id=?`, projectID, installID); err != nil {
		t.Fatal(err)
	}
	return installID
}

func seedProjectTemplateForCallback(t *testing.T, s *Server, projectID string) {
	t.Helper()
	definition := json.RawMessage(`{"category":"business","agents":[{"key":"advisor","name":"Advisor","directive":"Advise the client","mode":"autonomous","apps":["crm"]}],"dashboard":["crm:overview"]}`)
	if _, err := s.store.insertPreset(storedPreset{
		ID: "consulting", UserID: 1, ProjectID: projectID, Kind: projectSetupPresetKind,
		Scope: "project", SchemaVersion: 2, Name: "Consulting", Description: "Client consulting team", Definition: definition,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCallbackProjectTemplatesReadProjection(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "template-project")
	seedProjectTemplateForCallback(t, s, "template-project")
	installID := seedTemplateCallbackInstall(t, s, "template-reader", "template-project", sdk.PermTemplatesRead)

	rec := runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/templates?project_id=template-project", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "owner_id") {
		t.Fatalf("app projection leaked owner user id: %s", rec.Body.String())
	}
	var listed struct {
		Templates []sdk.ProjectTemplate `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Templates) != 1 || listed.Templates[0].ID != "consulting" || listed.Templates[0].Source != "user" {
		t.Fatalf("templates=%+v", listed.Templates)
	}
	if listed.Templates[0].OwnerProjectID != "template-project" || listed.Templates[0].Revision != 1 {
		t.Fatalf("project metadata=%+v", listed.Templates[0])
	}
	definition, err := listed.Templates[0].DecodeProjectSetup()
	if err != nil || len(definition.Agents) != 1 || definition.Agents[0].Apps[0] != "crm" {
		t.Fatalf("definition=%+v err=%v", definition, err)
	}

	rec = runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/templates?project_id=template-project&include_system=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("system list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	foundSystem := false
	for _, template := range listed.Templates {
		foundSystem = foundSystem || template.Source == "system"
	}
	if !foundSystem || len(listed.Templates) <= 1 {
		t.Fatalf("include_system did not add bundled templates: %+v", listed.Templates)
	}

	rec = runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/templates/consulting?project_id=template-project", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got sdk.ProjectTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "consulting" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
}

func TestCallbackProjectTemplatesAuthorizationAndReadOnly(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "template-project")
	seedPresetProject(t, s, "sibling-project")
	seedProjectTemplateForCallback(t, s, "template-project")
	reader := seedTemplateCallbackInstall(t, s, "scoped-template-reader", "template-project", sdk.PermTemplatesRead)
	noPermission := seedTemplateCallbackInstall(t, s, "unprivileged-template-reader", "template-project")

	tests := []struct {
		name      string
		installID int64
		method    string
		path      string
		status    int
	}{
		{"permission required", noPermission, http.MethodGet, "/apps/callback/templates?project_id=template-project", http.StatusForbidden},
		{"project scope enforced", reader, http.MethodGet, "/apps/callback/templates?project_id=sibling-project", http.StatusForbidden},
		{"project required", reader, http.MethodGet, "/apps/callback/templates", http.StatusBadRequest},
		{"read only", reader, http.MethodPost, "/apps/callback/templates?project_id=template-project", http.StatusMethodNotAllowed},
		{"unknown template", reader, http.MethodGet, "/apps/callback/templates/missing?project_id=template-project", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := runtimeAPIRequest(t, s, tt.installID, tt.method, tt.path, nil)
			if rec.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestCallbackProjectTemplatesGlobalInstallUsesOwnedProjects(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "owned-project")
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users(id,email,password_hash) VALUES(2,'other-template-owner@test.local','x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name) VALUES('foreign-project',2,'Foreign')`); err != nil {
		t.Fatal(err)
	}
	global := seedTemplateCallbackInstall(t, s, "global-template-reader", "", sdk.PermTemplatesRead)

	owned := runtimeAPIRequest(t, s, global, http.MethodGet, "/apps/callback/templates?project_id=owned-project", nil)
	if owned.Code != http.StatusOK {
		t.Fatalf("owned status=%d body=%s", owned.Code, owned.Body.String())
	}
	foreign := runtimeAPIRequest(t, s, global, http.MethodGet, "/apps/callback/templates?project_id=foreign-project", nil)
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign status=%d body=%s", foreign.Code, foreign.Body.String())
	}
}
