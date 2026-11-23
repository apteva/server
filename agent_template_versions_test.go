package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func templateTestCreate(t *testing.T, s *Server, name, directive, mode string) AgentTemplate {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCreateAgentTemplate(w, authedRequest(t, http.MethodPost, "/agent-templates", "", map[string]any{
		"name": name, "directive": directive, "mode": mode,
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("create template status=%d body=%s", w.Code, w.Body.String())
	}
	var created AgentTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func templateTestPut(t *testing.T, s *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAgentTemplateByID(w, authedRequest(t, http.MethodPut, "/agent-templates/"+id, "", body))
	return w
}

func templateTestNewVersion(t *testing.T, s *Server, id, directive, mode string, expected int) {
	t.Helper()
	current, err := s.store.GetAgentTemplate(1, id)
	if err != nil {
		t.Fatal(err)
	}
	w := templateTestPut(t, s, id, map[string]any{"name": current.Name, "directive": directive, "mode": mode, "expected_version": expected})
	if w.Code != http.StatusOK {
		t.Fatalf("save version status=%d body=%s", w.Code, w.Body.String())
	}
}

func templateTestCreateAgent(t *testing.T, s *Server, payload map[string]any) (*Agent, *httptest.ResponseRecorder) {
	t.Helper()
	payload["start"] = false
	w := httptest.NewRecorder()
	s.handleCreateInstance(w, authedRequest(t, http.MethodPost, "/agents", "", payload))
	if w.Code != http.StatusOK {
		return nil, w
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("create agent body=%s", w.Body.String())
	}
	agent, err := s.store.GetAgentByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return agent, w
}

func templateTestMustCreateAgent(t *testing.T, s *Server, payload map[string]any) *Agent {
	t.Helper()
	agent, w := templateTestCreateAgent(t, s, payload)
	if agent == nil {
		t.Fatalf("create agent status=%d body=%s", w.Code, w.Body.String())
	}
	return agent
}

func templateTestLink(t *testing.T, s *Server, agentID int64) *agentTemplateLink {
	t.Helper()
	link, err := s.store.agentTemplateLink(agentID)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func templateTestAgent(t *testing.T, s *Server, id int64) *Agent {
	t.Helper()
	agent, err := s.store.GetAgentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func templateTestLinkRequest(t *testing.T, s *Server, agentID int64, method string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAgentTemplateLink(w, authedRequest(t, method, fmt.Sprintf("/instances/%d/template", agentID), "", body), agentID)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// Only instructions, mode, background memory and requirements are versioned;
// catalog edits keep the version, and stale edits are refused.
func TestAgentTemplateSavesVersionsForBehaviorChangesOnly(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Support", "Answer support tickets v1.", "cautious")
	if tpl.Version != 1 {
		t.Fatalf("new template version=%d", tpl.Version)
	}
	w := templateTestPut(t, s, tpl.ID, map[string]any{"name": "Support desk", "directive": "Answer support tickets v1.", "mode": "cautious", "expected_version": 1})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":1`) {
		t.Fatalf("rename created a version: %d %s", w.Code, w.Body.String())
	}
	templateTestNewVersion(t, s, tpl.ID, "Answer support tickets v2.", "cautious", 1)
	if w := templateTestPut(t, s, tpl.ID, map[string]any{"name": "Support", "directive": "Stale edit.", "mode": "cautious", "expected_version": 1}); w.Code != http.StatusConflict {
		t.Fatalf("stale edit status=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleAgentTemplateByID(w, authedRequest(t, http.MethodGet, "/agent-templates/"+tpl.ID, "", nil))
	var detail struct {
		Version  int                    `json:"version"`
		Versions []AgentTemplateVersion `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Version != 2 || len(detail.Versions) != 2 || detail.Versions[0].Version != 2 {
		t.Fatalf("detail=%s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleAgentTemplateByID(w, authedRequest(t, http.MethodGet, "/agent-templates/"+tpl.ID+"/versions/1", "", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Answer support tickets v1.") {
		t.Fatalf("version 1 status=%d body=%s", w.Code, w.Body.String())
	}
}

// A template saved before versioning gets version 1 from its stored content
// when first used, so an older definition is never lost.
func TestAgentTemplateSavedBeforeVersioningKeepsItsContentAsVersionOne(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT INTO agent_templates(id,user_id,source,name,directive,mode) VALUES('usr-1:legacy',1,'user','Legacy','Legacy instructions.','learn')`); err != nil {
		t.Fatal(err)
	}
	templateTestNewVersion(t, s, "usr-1:legacy", "New instructions.", "learn", 1)
	first, err := s.store.agentTemplateVersionBody("usr-1:legacy", 1)
	if err != nil || first.Directive != "Legacy instructions." {
		t.Fatalf("version 1=%+v err=%v", first, err)
	}
	second, err := s.store.agentTemplateVersionBody("usr-1:legacy", 2)
	if err != nil || second.Directive != "New instructions." {
		t.Fatalf("version 2=%+v err=%v", second, err)
	}
}

// Built-in templates are refreshed from the presets at every boot; only a
// changed preset produces a new version.
func TestBuiltinAgentTemplateVersionsFollowPresetChanges(t *testing.T) {
	s := newTestServer(t)
	var rows, versions int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_templates WHERE source='builtin'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_template_versions WHERE source='builtin' AND version=1`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if rows == 0 || versions != rows {
		t.Fatalf("builtin rows=%d versions=%d", rows, versions)
	}
	if err := s.store.recordBuiltinTemplateVersions(); err != nil {
		t.Fatal(err)
	}
	const id = "preset:development-software:engineer"
	if latest, _ := s.store.latestAgentTemplateVersion(id); latest != 1 {
		t.Fatalf("unchanged boot added a version: %d", latest)
	}
	if _, err := s.store.db.Exec(`UPDATE agent_templates SET directive=directive||' Release note.' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.store.recordBuiltinTemplateVersions(); err != nil {
		t.Fatal(err)
	}
	if latest, _ := s.store.latestAgentTemplateVersion(id); latest != 2 {
		t.Fatalf("changed preset version=%d", latest)
	}
}

func TestAgentCreatedFromTemplateIsLinked(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Support", "Answer support tickets v1.", "cautious")

	agent := templateTestMustCreateAgent(t, s, map[string]any{"name": "Support A", "template_id": tpl.ID})
	if agent.Directive != "Answer support tickets v1." || agent.Mode != "cautious" {
		t.Fatalf("agent did not start from the template: %q %q", agent.Directive, agent.Mode)
	}
	if link := templateTestLink(t, s, agent.ID); link == nil || !link.FollowLatest || link.Version != 1 || link.TemplateID != tpl.ID {
		t.Fatalf("own template link=%+v", link)
	}

	// Instructions edited before creation count as local changes.
	edited := templateTestMustCreateAgent(t, s, map[string]any{"name": "Support B", "template_id": tpl.ID, "directive": "My own instructions."})
	if st, _ := s.agentTemplateStatusFor(context.Background(), edited, true); !st.ChangedLocally || st.UpdateAvailable {
		t.Fatalf("edited at creation status=%+v", st)
	}

	builtinID := "preset:development-software:engineer"
	builtin := templateTestMustCreateAgent(t, s, map[string]any{"name": "Engineer", "template_id": builtinID})
	if link := templateTestLink(t, s, builtin.ID); link == nil || link.FollowLatest {
		t.Fatalf("built-in template agents must stay pinned by default: %+v", link)
	}

	templateTestNewVersion(t, s, tpl.ID, "Answer support tickets v2.", "cautious", 1)
	pinned := templateTestMustCreateAgent(t, s, map[string]any{"name": "Pinned", "template_id": tpl.ID, "template_version": 1})
	if link := templateTestLink(t, s, pinned.ID); link == nil || link.FollowLatest || link.Version != 1 || pinned.Directive != "Answer support tickets v1." {
		t.Fatalf("pinned link=%+v directive=%q", link, pinned.Directive)
	}
	for _, payload := range []map[string]any{
		{"name": "Bad follow", "template_id": tpl.ID, "template_version": 1, "follow_template": true},
		{"name": "Bad version", "template_id": tpl.ID, "template_version": 3},
		{"name": "Missing", "template_id": "usr-1:missing"},
	} {
		if agent, w := templateTestCreateAgent(t, s, payload); agent != nil || w.Code != http.StatusBadRequest {
			t.Fatalf("%v status=%d body=%s", payload["name"], w.Code, w.Body.String())
		}
	}
	scratch := templateTestMustCreateAgent(t, s, map[string]any{"name": "Scratch", "template_id": "empty", "directive": "From scratch."})
	if link := templateTestLink(t, s, scratch.ID); link != nil {
		t.Fatalf("start-from-scratch agent was linked: %+v", link)
	}
}

// A new version reaches following agents whose instructions are unchanged
// since their last template version, and never overwrites local edits.
func TestAgentTemplateRolloutLeavesLocalChanges(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Support", "Answer support tickets v1.", "cautious")
	a := templateTestMustCreateAgent(t, s, map[string]any{"name": "A", "template_id": tpl.ID})
	b := templateTestMustCreateAgent(t, s, map[string]any{"name": "B", "template_id": tpl.ID})
	if _, err := s.store.db.Exec(`UPDATE agents SET directive='Answer support tickets v1. Escalate refunds.' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}

	templateTestNewVersion(t, s, tpl.ID, "Answer support tickets v2.", "learn", 1)
	s.rolloutAgentTemplatesOnce(context.Background())

	a = templateTestAgent(t, s, a.ID)
	if a.Directive != "Answer support tickets v2." || a.Mode != "learn" {
		t.Fatalf("following agent not updated: %q %q", a.Directive, a.Mode)
	}
	if link := templateTestLink(t, s, a.ID); link.Version != 2 || link.CheckedVersion != 2 || link.LastError != "" {
		t.Fatalf("updated link=%+v", link)
	}
	var reason string
	if err := s.store.db.QueryRow(`SELECT reason FROM agent_change_history WHERE agent_id=? AND field='directive'`, a.ID).Scan(&reason); err != nil || reason != `Template "Support" version 2` {
		t.Fatalf("history reason=%q err=%v", reason, err)
	}
	saved := readBehaviorDisk(t, s, a.ID)
	if directive, _ := saved["directive"].(string); !strings.HasPrefix(directive, "Answer support tickets v2.") {
		t.Fatalf("stopped agent's saved config not updated: %q", directive)
	}

	b = templateTestAgent(t, s, b.ID)
	if b.Directive != "Answer support tickets v1. Escalate refunds." || b.Mode != "cautious" {
		t.Fatalf("local changes overwritten: %q %q", b.Directive, b.Mode)
	}
	if link := templateTestLink(t, s, b.ID); link.Version != 1 || link.CheckedVersion != 2 {
		t.Fatalf("changed agent link=%+v", link)
	}
	st, err := s.agentTemplateStatusFor(context.Background(), b, true)
	if err != nil || !st.ChangedLocally || !st.UpdateAvailable || st.LatestVersion != 2 {
		t.Fatalf("changed agent status=%+v err=%v", st, err)
	}

	// Evaluated versions are not retried, so the edited agent stays as it is.
	s.rolloutAgentTemplatesOnce(context.Background())
	if b2 := templateTestAgent(t, s, b.ID); b2.Directive != b.Directive {
		t.Fatalf("second sweep changed the edited agent: %q", b2.Directive)
	}
}

// Running agents are judged by Core's own instructions, which the agent's
// learning may have changed without the server's copy knowing.
func TestAgentTemplateRolloutRunningAgentsUseLiveInstructions(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Queue", "Watch the queue v1.", "autonomous")
	synced := templateTestMustCreateAgent(t, s, map[string]any{"name": "Synced", "template_id": tpl.ID})
	evolved := templateTestMustCreateAgent(t, s, map[string]any{"name": "Evolved", "template_id": tpl.ID})
	syncedCore := attachBehaviorTestCore(t, s, synced)
	syncedCore.mu.Lock()
	syncedCore.cfg["directive"] = compileAgentDirective("Watch the queue v1.", "autonomous", synced.Proactivity)
	syncedCore.mu.Unlock()
	evolvedCore := attachBehaviorTestCore(t, s, evolved) // Core holds "live evolved instructions"

	templateTestNewVersion(t, s, tpl.ID, "Watch the queue v2.", "autonomous", 1)
	s.rolloutAgentTemplatesOnce(context.Background())

	syncedCore.mu.Lock()
	live, _ := syncedCore.cfg["directive"].(string)
	syncedCore.mu.Unlock()
	if !strings.HasPrefix(live, "Watch the queue v2.") || !strings.Contains(live, behaviorStart) {
		t.Fatalf("running agent did not receive the new version: %q", live)
	}
	if link := templateTestLink(t, s, synced.ID); link.Version != 2 {
		t.Fatalf("synced link=%+v", link)
	}

	evolvedCore.mu.Lock()
	live, _ = evolvedCore.cfg["directive"].(string)
	puts := evolvedCore.puts
	evolvedCore.mu.Unlock()
	if !strings.HasPrefix(live, "live evolved instructions") || puts != 0 {
		t.Fatalf("learned instructions were overwritten: %q puts=%d", live, puts)
	}
	if link := templateTestLink(t, s, evolved.ID); link.Version != 1 || link.CheckedVersion != 2 {
		t.Fatalf("evolved link=%+v", link)
	}
	if row := templateTestAgent(t, s, evolved.ID); row.Directive != "Watch the queue v1." {
		t.Fatalf("server copy changed: %q", row.Directive)
	}
}

func TestAgentTemplateLinkActions(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Drafts", "Draft replies v1.", "cautious")
	agent := templateTestMustCreateAgent(t, s, map[string]any{"name": "Drafter", "template_id": tpl.ID})
	templateTestNewVersion(t, s, tpl.ID, "Draft replies v2.", "cautious", 1)
	s.rolloutAgentTemplatesOnce(context.Background())
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v2." {
		t.Fatalf("rollout directive=%q", got)
	}

	// Choosing a version pins the agent to it.
	w, out := templateTestLinkRequest(t, s, agent.ID, http.MethodPut, map[string]any{"version": 1})
	if w.Code != http.StatusOK || out["outcome"] != agentTemplateApplied || out["follow_latest"] != false || out["version"] != float64(1) {
		t.Fatalf("pin status=%d body=%s", w.Code, w.Body.String())
	}
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v1." {
		t.Fatalf("pinned directive=%q", got)
	}
	s.rolloutAgentTemplatesOnce(context.Background())
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v1." {
		t.Fatalf("rollout moved a pinned agent: %q", got)
	}

	// Following again does not replace local edits without consent.
	if _, err := s.store.db.Exec(`UPDATE agents SET directive='Draft replies v1. Sign as support.' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	w, out = templateTestLinkRequest(t, s, agent.ID, http.MethodPut, map[string]any{"follow_latest": true})
	if w.Code != http.StatusConflict || out["status"].(map[string]any)["follow_latest"] != true {
		t.Fatalf("follow over local edits status=%d body=%s", w.Code, w.Body.String())
	}
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v1. Sign as support." {
		t.Fatalf("local edit replaced without consent: %q", got)
	}
	w, out = templateTestLinkRequest(t, s, agent.ID, http.MethodPut, map[string]any{"follow_latest": true, "overwrite_local_changes": true})
	if w.Code != http.StatusOK || out["outcome"] != agentTemplateApplied || out["changed_locally"] != false {
		t.Fatalf("overwrite status=%d body=%s", w.Code, w.Body.String())
	}
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v2." {
		t.Fatalf("overwrite directive=%q", got)
	}
	if w, _ := templateTestLinkRequest(t, s, agent.ID, http.MethodPut, map[string]any{"version": 3}); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown version status=%d", w.Code)
	}

	// Unlinking keeps the agent as it is.
	if w, _ := templateTestLinkRequest(t, s, agent.ID, http.MethodDelete, nil); w.Code != http.StatusOK {
		t.Fatalf("unlink status=%d", w.Code)
	}
	if w, out := templateTestLinkRequest(t, s, agent.ID, http.MethodGet, nil); w.Code != http.StatusOK || out["linked"] != false {
		t.Fatalf("after unlink=%s", w.Body.String())
	}
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Draft replies v2." {
		t.Fatalf("unlink changed the agent: %q", got)
	}
}

func TestSaveAgentAsTemplateLinksTheAgent(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent := templateTestMustCreateAgent(t, s, map[string]any{"name": "Planner", "directive": "Plan sprints.", "mode": "cautious"})
	w := httptest.NewRecorder()
	s.handleSaveAgentAsTemplate(w, authedRequest(t, http.MethodPost, fmt.Sprintf("/instances/%d/template/save", agent.ID), "", map[string]any{"name": "Sprint planner"}), agent.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("save as template status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Template AgentTemplate       `json:"template"`
		Status   agentTemplateStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Template.Directive != "Plan sprints." || out.Template.Mode != "cautious" || out.Template.Version != 1 {
		t.Fatalf("template=%+v", out.Template)
	}
	if !out.Status.Linked || !out.Status.FollowLatest || out.Status.ChangedLocally {
		t.Fatalf("status=%+v", out.Status)
	}
	templateTestNewVersion(t, s, out.Template.ID, "Plan sprints and reviews.", "cautious", 1)
	s.rolloutAgentTemplatesOnce(context.Background())
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Plan sprints and reviews." {
		t.Fatalf("saved agent did not follow its template: %q", got)
	}
}

func TestDeletingTemplateOrAgentRemovesLinks(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Temp", "Temporary role.", "cautious")
	kept := templateTestMustCreateAgent(t, s, map[string]any{"name": "Kept", "template_id": tpl.ID})
	removed := templateTestMustCreateAgent(t, s, map[string]any{"name": "Removed", "template_id": tpl.ID})
	if err := s.store.DeleteAgent(1, removed.ID); err != nil {
		t.Fatal(err)
	}
	if link := templateTestLink(t, s, removed.ID); link != nil {
		t.Fatalf("deleted agent kept a link: %+v", link)
	}
	if err := s.store.DeleteAgentTemplate(1, tpl.ID); err != nil {
		t.Fatal(err)
	}
	var versions int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_template_versions WHERE template_id=?`, tpl.ID).Scan(&versions)
	if link := templateTestLink(t, s, kept.ID); link != nil || versions != 0 {
		t.Fatalf("template delete left link=%+v versions=%d", link, versions)
	}
	if got := templateTestAgent(t, s, kept.ID).Directive; got != "Temporary role." {
		t.Fatalf("template delete changed the agent: %q", got)
	}
}

func TestGatewayAgentTemplateTools(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/agents", s.authMiddleware(s.handleCreateInstance))
	apiMux.HandleFunc("/agent-templates", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleCreateAgentTemplate(w, r)
			return
		}
		s.handleListAgentTemplates(w, r)
	}))
	apiMux.HandleFunc("/agent-templates/", s.authMiddleware(s.handleAgentTemplateByID))
	root := http.NewServeMux()
	root.Handle("/api/", http.StripPrefix("/api", apiMux))
	ts := httptest.NewServer(root)
	defer ts.Close()
	client := gatewayAPIClient{baseURL: ts.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}

	created, err := handleGatewayAgentTool("agent_templates_save", map[string]any{"name": "Triage", "directive": "Triage new issues v1.", "mode": "cautious"}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := created.(map[string]any)["id"].(string)
	if id == "" || created.(map[string]any)["version"] != 1 {
		t.Fatalf("created=%v", created)
	}
	agentResult, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Triage bot", "template_id": id, "start": false}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := parseIntArg(agentResult.(map[string]any)["id"])
	if agent := templateTestAgent(t, s, agentID); agent.Directive != "Triage new issues v1." {
		t.Fatalf("agents_create ignored the template: %q", agent.Directive)
	}

	if _, err := handleGatewayAgentTool("agent_templates_save", map[string]any{"id": id, "directive": "Triage v2."}, "", client, s.store, "/tmp/apteva-server"); err == nil {
		t.Fatal("update without expected_version accepted")
	}
	if _, err := handleGatewayAgentTool("agent_templates_save", map[string]any{"id": id, "directive": "Triage v2.", "expected_version": 5}, "", client, s.store, "/tmp/apteva-server"); err == nil {
		t.Fatal("stale expected_version accepted")
	}
	saved, err := handleGatewayAgentTool("agent_templates_save", map[string]any{"id": id, "directive": "Triage new issues v2.", "expected_version": 1}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatal(err)
	}
	if row := saved.(map[string]any); row["version"] != 2 || row["name"] != "Triage" || row["following_agents"] != 1 {
		t.Fatalf("saved=%v", saved)
	}
	listed, err := handleGatewayAgentTool("agent_templates_list", map[string]any{"query": "triage"}, "", client, s.store, "/tmp/apteva-server")
	if err != nil {
		t.Fatal(err)
	}
	templates := listed.(map[string]any)["templates"].([]map[string]any)
	if len(templates) != 1 || templates[0]["version"] != 2 {
		t.Fatalf("listed=%v", listed)
	}
}

// A sweep lists due agents before applying; an agent pinned in between must
// not be moved.
func TestAgentTemplateRolloutSkipsAgentPinnedAfterListing(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	tpl := templateTestCreate(t, s, "Pinned", "Pinned role v1.", "cautious")
	agent := templateTestMustCreateAgent(t, s, map[string]any{"name": "Pinned agent", "template_id": tpl.ID})
	templateTestNewVersion(t, s, tpl.ID, "Pinned role v2.", "cautious", 1)
	if _, err := s.store.db.Exec(`UPDATE agent_template_links SET follow_latest=0 WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.applyAgentTemplateVersion(context.Background(), agent.ID, 2, agentTemplateApplyOptions{Rollout: true})
	if err != nil || outcome != agentTemplateUpToDate {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if got := templateTestAgent(t, s, agent.ID).Directive; got != "Pinned role v1." {
		t.Fatalf("pinned agent moved: %q", got)
	}
}

// Agents created by a workspace preset are linked to their built-in roles,
// pinned, and a later role version is rendered with the project's own
// description, exactly as preset setup rendered the first one.
func TestPresetAgentsAreLinkedToTheirRoles(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "apply-project")
	sidecar, _ := presetEvalsFixture(t)
	seedPresetEvalsApp(t, s, "apply-project", sidecar.URL)
	crmInstallID := seedAppWithTools(t, s, "crm", "apply-project", []string{"contacts_list"})
	if err := s.registerAppMCP(crmInstallID); err != nil {
		t.Fatal(err)
	}
	const description = "Find and qualify leads for independent clinics"
	req := authedRequest(t, http.MethodPost, "/projects/apply-project/setup/apply", "", map[string]any{
		"preset_id": "business-lead-generation", "description": description,
	})
	req.Header.Set("X-Apteva-Operator-ID", "1")
	rec := httptest.NewRecorder()
	s.handleProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
	}
	agents, err := s.store.ListAgentsInProject("apply-project")
	if err != nil || len(agents) == 0 {
		t.Fatalf("agents=%d err=%v", len(agents), err)
	}
	for _, agent := range agents {
		link := templateTestLink(t, s, agent.ID)
		if link == nil || !strings.HasPrefix(link.TemplateID, "preset:business-lead-generation:") || link.FollowLatest || link.PresetValues["description"] != description {
			t.Fatalf("agent %q link=%+v", agent.Name, link)
		}
		row := templateTestAgent(t, s, agent.ID)
		if st, err := s.agentTemplateStatusFor(context.Background(), row, true); err != nil || st.ChangedLocally || st.UpdateAvailable {
			t.Fatalf("agent %q status=%+v err=%v", agent.Name, st, err)
		}
	}

	// A release changes one role's preset text.
	agent := agents[0]
	link := templateTestLink(t, s, agent.ID)
	role, err := s.store.GetAgentTemplate(1, link.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := s.store.agentTemplateVersionBody(link.TemplateID, link.Version)
	role.PresetDirective = current.PresetDirective + " Confirm every lead's opt-in before outreach."
	tx, err := s.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	version, created, err := recordAgentTemplateVersion(tx, *role, 0)
	if err != nil || !created {
		t.Fatalf("record version=%d created=%v err=%v", version, created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	row := templateTestAgent(t, s, agent.ID)
	if st, _ := s.agentTemplateStatusFor(context.Background(), row, true); !st.UpdateAvailable || st.ChangedLocally {
		t.Fatalf("after release status=%+v", st)
	}
	s.rolloutAgentTemplatesOnce(context.Background())
	if got := templateTestAgent(t, s, agent.ID).Directive; got != row.Directive {
		t.Fatal("pinned preset agent was updated by the rollout")
	}
	w, out := templateTestLinkRequest(t, s, agent.ID, http.MethodPut, map[string]any{"version": version})
	if w.Code != http.StatusOK || out["outcome"] != agentTemplateApplied {
		t.Fatalf("take version status=%d body=%s", w.Code, w.Body.String())
	}
	got := templateTestAgent(t, s, agent.ID).Directive
	if got != expandPresetTemplate(role.PresetDirective, map[string]string{"description": description}) || !strings.Contains(got, description) || !strings.Contains(got, "Confirm every lead's opt-in") {
		t.Fatalf("new version was not rendered for the project: %q", got)
	}
}

// A preset whose role text differs from the shipped role is not linked.
func TestPresetAgentWithDifferentRoleTextIsNotLinked(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	preset := catalog.ByID["development-software"]
	spec := preset.Agents[0]
	values := map[string]string{"description": "A billing API"}
	agent, err := s.store.CreateAgent(1, spec.Name, expandPresetTemplate(spec.Directive, values), spec.Mode, `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	spec.Directive += " Customized by this installation."
	if err := s.linkPresetAgentToTemplate(1, agent.ID, preset.ID, spec, values); err != nil {
		t.Fatal(err)
	}
	if link := templateTestLink(t, s, agent.ID); link != nil {
		t.Fatalf("customized preset role was linked: %+v", link)
	}
}
