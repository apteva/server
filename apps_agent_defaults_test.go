package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seedAgentDefaultsProject(t *testing.T, s *Server, projectID string) {
	t.Helper()
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO projects(id,user_id,name,description) VALUES(?,1,?,'')`,
		projectID, projectID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO project_members(project_id,user_id,role,added_by) VALUES(?,1,'owner',1)`,
		projectID,
	); err != nil {
		t.Fatal(err)
	}
}

func createdAgentMCPNames(t *testing.T, s *Server, rec *httptest.ResponseRecorder) (map[string]bool, int64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.store.GetAgentByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(fresh.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, server := range mcpMaps(cfg["mcp_servers"]) {
		if name, _ := server["name"].(string); name != "" {
			names[name] = true
		}
	}
	return names, created.ID
}

func TestAgentDefaultsOmittedEmptyAndExplicitSelections(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedAgentDefaultsProject(t, s, "project-a")
	seedAgentDefaultsProject(t, s, "project-b")

	globalDefault := seedAppWithTools(t, s, "global-default", "", []string{"global_get"})
	projectDefault := seedAppWithTools(t, s, "project-default", "project-a", []string{"project_get"})
	otherProjectDefault := seedAppWithTools(t, s, "other-project-default", "project-b", []string{"other_get"})
	explicitOnly := seedAppWithTools(t, s, "explicit-only", "project-a", []string{"explicit_get"})
	for _, id := range []int64{globalDefault, projectDefault, otherProjectDefault, explicitOnly} {
		if err := s.registerAppMCP(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET default_for_new_agents=1 WHERE id IN (?,?,?)`,
		globalDefault, projectDefault, otherProjectDefault,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`
		INSERT INTO skills (slug, name, description, body, source, install_id, project_id, enabled)
		VALUES ('project-default:guide', 'guide', 'Default app guide', 'Use the project default app.', 'app', ?, 'project-a', 1)`,
		projectDefault,
	); err != nil {
		t.Fatal(err)
	}

	// Omission asks the server to resolve global + project defaults.
	req := authedRequest(t, http.MethodPost, "/instances", "", map[string]any{
		"name": "defaults", "directive": "test", "project_id": "project-a", "start": false,
	})
	rec := httptest.NewRecorder()
	s.handleCreateInstance(rec, req)
	names, defaultAgentID := createdAgentMCPNames(t, s, rec)
	if !names["global-default"] || !names["project-default"] || names["other-project-default"] || names["explicit-only"] {
		t.Fatalf("omitted selection resolved wrong apps: %#v", names)
	}
	var bindings int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=? AND enabled=1`, defaultAgentID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 2 {
		t.Fatalf("default apps did not create synchronized bindings: got %d want 2", bindings)
	}
	activeSkills, err := journalActiveSkillRecords(s.agents.instanceDir(defaultAgentID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := activeSkills["project-default:guide"]; !ok {
		t.Fatalf("default app skill was not attached before startup: %#v", activeSkills)
	}

	// An explicit empty array is the creation-wizard opt-out.
	req = authedRequest(t, http.MethodPost, "/instances", "", map[string]any{
		"name": "no defaults", "directive": "test", "project_id": "project-a", "start": false,
		"bound_app_install_ids": []int64{},
	})
	rec = httptest.NewRecorder()
	s.handleCreateInstance(rec, req)
	names, _ = createdAgentMCPNames(t, s, rec)
	if names["global-default"] || names["project-default"] {
		t.Fatalf("explicit empty selection attached defaults: %#v", names)
	}

	// A present non-empty array is exact; defaults are not merged back in.
	req = authedRequest(t, http.MethodPost, "/instances", "", map[string]any{
		"name": "explicit", "directive": "test", "project_id": "project-a", "start": false,
		"bound_app_install_ids": []int64{explicitOnly},
	})
	rec = httptest.NewRecorder()
	s.handleCreateInstance(rec, req)
	names, _ = createdAgentMCPNames(t, s, rec)
	if !names["explicit-only"] || names["global-default"] || names["project-default"] {
		t.Fatalf("explicit selection was not authoritative: %#v", names)
	}
}

func TestAgentDefaultPolicyEndpointAndEligibility(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedAgentDefaultsProject(t, s, "project-a")
	eligible := seedAppWithTools(t, s, "eligible-default", "project-a", []string{"read"})
	if err := s.registerAppMCP(eligible); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPatch, "/apps/installs/"+itoa64(eligible)+"/agent-default", "", map[string]any{
		"default_for_new_agents": true,
	})
	rec := httptest.NewRecorder()
	s.handleSetInstallAgentDefault(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable default status=%d body=%s", rec.Code, rec.Body.String())
	}
	var enabled int
	if err := s.store.db.QueryRow(`SELECT default_for_new_agents FROM app_installs WHERE id=?`, eligible).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("default policy not stored: enabled=%d err=%v", enabled, err)
	}

	uiOnly := seedAppWithTools(t, s, "ui-only-default", "project-a", nil)
	req = authedRequest(t, http.MethodPatch, "/apps/installs/"+itoa64(uiOnly)+"/agent-default", "", map[string]any{
		"default_for_new_agents": true,
	})
	rec = httptest.NewRecorder()
	s.handleSetInstallAgentDefault(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("UI-only app default status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestChangingDefaultDoesNotMutateExistingAgent(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedAgentDefaultsProject(t, s, "project-a")
	installID := seedAppWithTools(t, s, "future-default", "project-a", []string{"read"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "existing", "test", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET default_for_new_agents=1 WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Config != `{}` {
		t.Fatalf("default policy rewrote existing agent config: %s", fresh.Config)
	}
	var bindings int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=?`, agent.ID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("default policy attached to existing agent: %d bindings", bindings)
	}
}

func TestProjectDefaultWinsOverGlobalInstallOfSameApp(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedAgentDefaultsProject(t, s, "project-a")
	globalID := seedAppWithTools(t, s, "shared-default", "", []string{"read"})
	var appID int64
	if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, globalID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'project-a', 'running', 1)`,
		appID,
	)
	if err != nil {
		t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	for _, id := range []int64{globalID, projectID} {
		if err := s.registerAppMCP(id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.db.Exec(`UPDATE app_installs SET default_for_new_agents=1 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.defaultAppInstallIDsForProject("project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != projectID {
		t.Fatalf("defaults=%v, want project install %d", ids, projectID)
	}
}
