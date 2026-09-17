package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestUIContributionsAllowsOnlyOwnActiveHelperAcrossProjectContext(t *testing.T) {
	s := newTestServer(t)
	owner, _ := s.store.CreateUser("helper-ui-owner@test.local", "hash")
	other, _ := s.store.CreateUser("helper-ui-other@test.local", "hash")
	project, _ := s.store.CreateProject(owner.ID, "Workspace", "", "")
	helper, _ := s.store.GetOrCreatePlatformHelper(owner.ID, platformHelperSystemPrompt)
	otherHelper, _ := s.store.GetOrCreatePlatformHelper(other.ID, platformHelperSystemPrompt)
	ordinary, _ := s.store.CreateAgent(owner.ID, "Unscoped", "", "cautious", "{}", "")
	call := func(id int64) int {
		query := url.Values{"project_id": {project.ID}, "surface": {"dashboard.build"}, "agent_id": {itoa(id)}}
		r := helperLifecycleRequest(http.MethodGet, "/ui/contributions?"+query.Encode(), owner.ID, "")
		w := httptest.NewRecorder()
		s.handleUIContributions(w, r)
		return w.Code
	}
	if code := call(helper.ID); code != 200 {
		t.Fatalf("own Helper: %d", code)
	}
	if code := call(otherHelper.ID); code != 404 {
		t.Fatalf("other Helper: %d", code)
	}
	if code := call(ordinary.ID); code != 404 {
		t.Fatalf("unscoped ordinary agent: %d", code)
	}
	setPlatformHelperActivated(helper, false)
	s.store.UpdateAgent(helper)
	if code := call(helper.ID); code != 404 {
		t.Fatalf("inactive Helper: %d", code)
	}
}

func TestUIContributionEligibilityUsesAgentAttachmentWithoutThreadKinds(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("contributions@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(user.ID, "Context", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "Agent", "", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}

	install := func(name, visibility string) int64 {
		t.Helper()
		manifest, _ := json.Marshal(sdk.Manifest{
			Name: name, DisplayName: name, Version: "1.0.0",
			Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{
				Name: "summary", Entry: "/ui/Summary.mjs",
				Slots:      []string{sdk.UIComponentSlotDashboardThreadSidebar},
				Visibility: visibility,
			}}},
		})
		result, err := s.store.db.Exec(
			`INSERT INTO apps(name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
			name, string(manifest),
		)
		if err != nil {
			t.Fatal(err)
		}
		appID, _ := result.LastInsertId()
		result, err = s.store.db.Exec(
			`INSERT INTO app_installs(app_id, project_id, status, version, permissions_json, manifest_json)
			 VALUES (?, ?, 'running', '1.0.0', '[]', ?)`, appID, project.ID, string(manifest),
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		return id
	}

	attachedInstall := install("attached-app", "")
	install("unattached-app", sdk.UIComponentVisibilityAttached)
	install("project-app", sdk.UIComponentVisibilityProject)
	if _, err := s.store.db.Exec(
		`INSERT INTO app_agent_bindings(install_id, agent_id, enabled) VALUES (?, ?, 1)`,
		attachedInstall, agent.ID,
	); err != nil {
		t.Fatal(err)
	}

	query := url.Values{
		"project_id": {project.ID},
		"surface":    {sdk.UIComponentSlotDashboardThreadSidebar},
		"agent_id":   {itoa(agent.ID)},
		"thread_id":  {"opaque-thread-123"},
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/contributions?"+query.Encode(), nil)
	req.Header.Set("X-User-ID", itoa(user.ID))
	rec := httptest.NewRecorder()
	s.handleUIContributions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ThreadID      string                      `json:"thread_id"`
		Contributions []uiContributionEligibility `json:"contributions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ThreadID != "opaque-thread-123" {
		t.Fatalf("thread id=%q", response.ThreadID)
	}
	eligible := map[string]bool{}
	for _, contribution := range response.Contributions {
		eligible[contribution.App] = contribution.Eligible
	}
	if !eligible["attached-app"] || eligible["unattached-app"] || !eligible["project-app"] {
		t.Fatalf("eligibility=%+v response=%s", eligible, rec.Body.String())
	}
}
