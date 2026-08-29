package main

// Tests for the integration-binding + app-call authorization checks.
// The actual integration-execute downstream (decrypt + HTTP call to
// upstream) is covered by the existing /connections/:id/execute
// tests; here we only exercise the new auth surface.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// seedInstallWithBindings inserts an apps row + an app_installs row
// with the given manifest and bindings. Returns the install id.
func seedInstallWithBindings(t *testing.T, s *Server, appName string, manifest sdk.Manifest, bindings map[string]any) int64 {
	t.Helper()
	mj, _ := json.Marshal(manifest)
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		appName, string(mj),
	); err != nil {
		t.Fatalf("insert apps: %v", err)
	}
	var appID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name=?`, appName).Scan(&appID)
	bj, _ := json.Marshal(bindings)
	permsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings, permissions_json)
		 VALUES (?, ?, 'running', 1, ?, ?)`,
		appID, "proj-1", string(bj), string(permsJSON),
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	return id
}

func TestCallbackAgentForInstallEnforcesOwnerAndProject(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (2, 'other@test.local', 'x')`); err != nil {
		t.Fatal(err)
	}
	inProject, err := s.store.CreateAgent(1, "in-project", "answer calls", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := s.store.CreateAgent(1, "other-project", "answer calls", "autonomous", "{}", "proj-2")
	if err != nil {
		t.Fatal(err)
	}
	otherOwner, err := s.store.CreateAgent(2, "other-owner", "answer calls", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "telephony-auth-test"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermInstancesRead}
	installID := seedInstallWithBindings(t, s, "telephony-auth-test", manifest, nil)
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/spawn-realtime", nil)
	req.Header.Set("X-User-ID", "1")

	if _, err := s.callbackAgentForInstall(req, installID, inProject.ID); err != nil {
		t.Fatalf("matching owner and project rejected: %v", err)
	}
	if _, err := s.callbackAgentForInstall(req, installID, otherProject.ID); err == nil {
		t.Fatal("agent from another project accepted")
	}
	if _, err := s.callbackAgentForInstall(req, installID, otherOwner.ID); err == nil {
		t.Fatal("agent from another owner accepted")
	}

	runtimeManager := NewEnvironmentManager(t.TempDir())
	runtime := &Environment{
		ID: "runtime-1",
		installs: map[string]*localInstall{
			"telephony-auth-test": {InstallID: installID},
		},
		agents: map[int64]*EnvironmentAgent{
			otherProject.ID: {AgentID: otherProject.ID, Alias: "main"},
		},
		agentAliases: map[string]int64{"main": otherProject.ID},
	}
	runtimeManager.environments[runtime.ID] = runtime
	s.environments = runtimeManager
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id=?`, runtime.ID, installID); err != nil {
		t.Fatal(err)
	}
	projected, err := s.callbackAgentForInstall(req, installID, otherProject.ID)
	if err != nil {
		t.Fatalf("agent and install in the same runtime rejected: %v", err)
	}
	if projected.ID != otherProject.ID || projected.Name != "main" || projected.ProjectID != runtime.ID || projected.Mode != otherProject.Mode || projected.Status != otherProject.Status {
		t.Fatalf("runtime projection=%+v source=%+v", projected, otherProject)
	}

	metadataReq := httptest.NewRequest(http.MethodGet, "/apps/callback/agents/"+itoa(otherProject.ID), nil)
	metadataReq.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	metadataReq.Header.Set("X-User-ID", "1")
	metadataRec := httptest.NewRecorder()
	s.handleAppCallback(metadataRec, metadataReq)
	if metadataRec.Code != http.StatusOK {
		t.Fatalf("runtime metadata status=%d body=%s", metadataRec.Code, metadataRec.Body.String())
	}
	var metadata sdk.PlatformInstance
	if err := json.Unmarshal(metadataRec.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ID != otherProject.ID || metadata.Name != "main" || metadata.ProjectID != runtime.ID || metadata.Mode != otherProject.Mode || metadata.Status != otherProject.Status || !metadata.AttachedToCaller {
		t.Fatalf("runtime metadata=%+v", metadata)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/apps/callback/agents", nil)
	listReq.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	listReq.Header.Set("X-User-ID", "1")
	listRec := httptest.NewRecorder()
	s.handleAppCallback(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("runtime list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []sdk.PlatformInstance
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != metadata {
		t.Fatalf("runtime list=%+v metadata=%+v", listed, metadata)
	}
	if _, err := s.callbackAgentForInstall(req, installID, inProject.ID); err == nil {
		t.Fatal("agent outside the install runtime accepted")
	}

	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.callbackAgentForInstall(req, installID, otherProject.ID); err != nil {
		t.Fatalf("global install rejected an agent owned by its user: %v", err)
	}
}

func TestCallbackDeliveryTargetRequiresHelperBinding(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	helper, err := s.store.CreateAgent(1, "callback-helper", "help", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	helper.Kind = "platform_helper"
	if _, err := s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, helper.ID); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "helper-binding-test"}
	installID := seedInstallWithBindings(t, s, "helper-binding-test", manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	starts := 0
	s.platformHelperStarter = func(userID int64) (*Agent, error) {
		starts++
		return helper, nil
	}

	if _, err := s.ensureCallbackDeliveryTargetRunning(installID, helper); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("unbound helper wake error=%v, want attachment rejection", err)
	}
	if starts != 0 {
		t.Fatalf("unbound helper invoked starter %d time(s)", starts)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO app_agent_bindings (install_id,agent_id,enabled) VALUES (?,?,1)`,
		installID, helper.ID,
	); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ensureCallbackDeliveryTargetRunning(installID, helper); err != nil || got.ID != helper.ID {
		t.Fatalf("bound helper wake got=%+v err=%v", got, err)
	}
	if starts != 1 {
		t.Fatalf("bound helper starter calls=%d, want 1", starts)
	}

	ordinary, err := s.store.CreateAgent(1, "ordinary", "work", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ensureCallbackDeliveryTargetRunning(installID, ordinary); err != nil || got.ID != ordinary.ID {
		t.Fatalf("ordinary target got=%+v err=%v", got, err)
	}
	if starts != 1 {
		t.Fatalf("ordinary target unexpectedly invoked Helper starter; calls=%d", starts)
	}
}

func TestCallbackDeliveryLazilyStartsBoundHelper(t *testing.T) {
	eventID := "conversation:helper:message:1:agent:1"
	threadCalls, eventCalls := 0, 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/threads/chat-helper":
			threadCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "created",
				"events": map[string]any{"accepted": []string{eventID}, "duplicates": []string{}},
			})
		case "/event":
			eventCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"queued": true})
		default:
			http.Error(w, "unexpected core path", http.StatusNotFound)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	helper, err := s.store.CreateAgent(1, "delivery-helper", "help", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	helper.Kind = "platform_helper"
	if _, err := s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, helper.ID); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "helper-delivery-test",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermThreadsWrite}},
	}
	installID := seedInstallWithBindings(t, s, "helper-delivery-test", manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(
		`INSERT INTO app_agent_bindings (install_id,agent_id,enabled) VALUES (?,?,1)`,
		installID, helper.ID,
	); err != nil {
		t.Fatal(err)
	}
	starts := 0
	s.platformHelperStarter = func(userID int64) (*Agent, error) {
		starts++
		s.agents.processes[helper.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
		return helper, nil
	}

	// Metadata lookup must remain read-only.
	metadataReq := httptest.NewRequest(http.MethodGet, "/apps/callback/agents/"+itoa(helper.ID), nil)
	metadataReq.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	metadataReq.Header.Set("X-User-ID", "1")
	metadataRec := httptest.NewRecorder()
	s.handleAppCallback(metadataRec, metadataReq)
	if metadataRec.Code != http.StatusOK || starts != 0 {
		t.Fatalf("metadata status=%d starts=%d body=%s", metadataRec.Code, starts, metadataRec.Body.String())
	}

	spawnBody, _ := json.Marshal(sdk.ThreadSpawnRequest{
		AgentID: helper.ID, ThreadID: "chat-helper", ProjectID: "proj-thread", MCP: []string{"conversations"},
		Events: []sdk.ThreadEvent{{ID: eventID, Message: "Build this"}},
	})
	spawnReq := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/spawn", strings.NewReader(string(spawnBody)))
	spawnReq.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	spawnReq.Header.Set("X-User-ID", "1")
	spawnRec := httptest.NewRecorder()
	s.handleAppCallback(spawnRec, spawnReq)
	if spawnRec.Code != http.StatusOK || starts != 1 || threadCalls != 1 {
		t.Fatalf("spawn status=%d starts=%d thread_calls=%d body=%s", spawnRec.Code, starts, threadCalls, spawnRec.Body.String())
	}
	if projectID, err := s.store.AgentThreadProjectForUser(1, helper.ID, "chat-helper"); err != nil || projectID != "proj-thread" {
		t.Fatalf("persisted thread project=%q err=%v", projectID, err)
	}

	// Simulate a later Helper stop. Event delivery wakes it once, while the
	// following event reuses the running process without another start.
	delete(s.agents.processes, helper.ID)
	sendEvent := func(message string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/apps/callback/agents/"+itoa(helper.ID)+"/event",
			strings.NewReader(`{"message":`+strconv.Quote(message)+`,"thread_id":"chat-helper"}`))
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		return rec
	}
	if rec := sendEvent("continue"); rec.Code != http.StatusOK {
		t.Fatalf("cold event status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := sendEvent("continue again"); rec.Code != http.StatusOK {
		t.Fatalf("warm event status=%d body=%s", rec.Code, rec.Body.String())
	}
	if starts != 2 || eventCalls != 2 {
		t.Fatalf("after events starts=%d event_calls=%d, want 2 and 2", starts, eventCalls)
	}
}

func TestCallbackOpaqueThreadSpawnRequiresPermissionAndProjectScope(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "opaque-target", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateAgent(1, "other-project", "directive", "autonomous", "{}", "proj-2")
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "opaque-owner"}
	installID := seedInstallWithBindings(t, s, "opaque-owner", manifest, nil)

	call := func(agentID int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(sdk.ThreadSpawnRequest{AgentID: agentID, ThreadID: "opaque-run-17"})
		req := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/spawn", strings.NewReader(string(body)))
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		return rec
	}

	if rec := call(agent.ID); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(sdk.PermThreadsWrite)) {
		t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
	}
	permissions, _ := json.Marshal([]sdk.Permission{sdk.PermThreadsWrite})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, permissions, installID); err != nil {
		t.Fatal(err)
	}
	if rec := call(other.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The target is authorized now; this stopped test agent has no Core port,
	// so reaching the resolver produces 502 rather than an auth rejection.
	if rec := call(agent.ID); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "agent is not running") {
		t.Fatalf("authorized spawn status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCallbackThreadSpawnForwardsInitialEventsAtomically(t *testing.T) {
	var coreCalls int
	var coreBody map[string]any
	eventID := "conversation:conv-42:message:99:agent:7"
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coreCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/threads/chat-conv-42" {
			t.Fatalf("core request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&coreBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "created",
			"events": map[string]any{"accepted": []string{eventID}, "duplicates": []string{}},
		})
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "atomic-target", "directive", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "atomic-owner",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermThreadsWrite}},
	}
	installID := seedInstallWithBindings(t, s, "atomic-owner", manifest, nil)

	body, _ := json.Marshal(sdk.ThreadSpawnRequest{
		AgentID: agent.ID, ThreadID: "chat-conv-42", MCP: []string{"conversations"},
		Events: []sdk.ThreadEvent{{ID: eventID, Message: "Hello"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/spawn", strings.NewReader(string(body)))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if coreCalls != 1 {
		t.Fatalf("core calls=%d, want exactly one", coreCalls)
	}
	events, ok := coreBody["events"].([]any)
	if !ok || len(events) != 1 || events[0].(map[string]any)["id"] != eventID {
		t.Fatalf("core events=%v", coreBody["events"])
	}
	var result sdk.ThreadSpawnResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Events.Accepted) != 1 || result.Events.Accepted[0] != eventID {
		t.Fatalf("result=%+v", result)
	}
}

func TestCallbackThreadEnsureReconcilesExistingProfileAndEvents(t *testing.T) {
	eventID := "conversation:conv-42:message:100:agent:7"
	var calls []string
	var updateBody map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "main", "directive": "main"},
				{"id": "chat-conv-42", "directive": "main old", "mcp_names": []string{"conversations"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"directive": "main"})
		case r.Method == http.MethodPut && r.URL.Path == "/threads/chat-conv-42":
			if err := json.NewDecoder(r.Body).Decode(&updateBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "updated", "events": map[string]any{"accepted": []string{eventID}},
			})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer core.Close()
	parsed, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "ensure-target", "main", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "ensure-owner",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermThreadsWrite}}}
	installID := seedInstallWithBindings(t, s, "ensure-owner", manifest, nil)
	body, _ := json.Marshal(sdk.ThreadEnsureRequest{
		ThreadSpawnRequest: sdk.ThreadSpawnRequest{
			AgentID: agent.ID, ThreadID: "chat-conv-42", DirectiveSuffix: " conversation",
			MCP: []string{"conversations"}, Events: []sdk.ThreadEvent{{ID: eventID, Message: "Hello again"}},
		},
		ProfileHash: "profile-v2",
	})
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/ensure", strings.NewReader(string(body)))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Join(calls, ",") != "GET /threads,GET /config,PUT /threads/chat-conv-42" {
		t.Fatalf("core calls=%v", calls)
	}
	if got := updateBody["mcp"].([]any); len(got) != 1 || got[0] != "conversations" {
		t.Fatalf("updated MCP=%v", updateBody["mcp"])
	}
	if got := updateBody["events"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != eventID {
		t.Fatalf("updated events=%v", updateBody["events"])
	}
	var result sdk.ThreadEnsureResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "updated" || !result.Reconciled || result.ProfileHash != "profile-v2" ||
		len(result.Events.Accepted) != 1 || result.Events.Accepted[0] != eventID {
		t.Fatalf("result=%+v", result)
	}
}

func TestValidateThreadSpawnEventsRejectsInvalidEvents(t *testing.T) {
	for _, events := range [][]sdk.ThreadEvent{
		{{ID: "", Message: "hello"}},
		{{ID: " stable-id ", Message: "hello"}},
		{{ID: "stable-id", Message: nil}},
		{{ID: "stable-id", Message: "  "}},
		{{ID: "stable-id", Message: []any{}}},
		{{ID: "bad\nseparator", Message: "hello"}},
	} {
		if err := validateThreadSpawnEvents(events); err == nil {
			t.Fatalf("events=%+v accepted", events)
		}
	}
}

func TestNormalizeCallbackEventMessageBridgesStructuredAppsToCore(t *testing.T) {
	structured, err := normalizeCallbackEventMessage(json.RawMessage(`{"type":"work.ready","task_id":"task-7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if structured != `{"task_id":"task-7","type":"work.ready"}` {
		t.Fatalf("structured=%q", structured)
	}
	parts, err := normalizeCallbackEventMessage(json.RawMessage(`[{"type":"text","text":"hello"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parts.([]any); !ok {
		t.Fatalf("content parts lost native array shape: %#v", parts)
	}
	text, err := normalizeCallbackEventMessage(json.RawMessage(`"plain"`))
	if err != nil || text != "plain" {
		t.Fatalf("text=%#v err=%v", text, err)
	}
}

// --- /callback/projects ---------------------------------------------

// Project-scoped install — singleton listing of the install's own
// project.
func TestCallback_Projects_ProjectScopedSingleton(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-1', 1, 'p1', 'Project one description')`)
	installID := seedInstall(t, s, "media", "proj-1")
	s.store.db.Exec(`UPDATE app_installs SET installed_by=1 WHERE id=?`, installID)

	req := httptest.NewRequest("GET", "/apps/callback/projects", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected singleton list, got %d", len(out))
	}
	if out[0]["id"] != "proj-1" {
		t.Errorf("got id %v, want proj-1", out[0]["id"])
	}
	if out[0]["description"] != "Project one description" {
		t.Errorf("got description %v, want project description", out[0]["description"])
	}
}

// Global install — every project the install's owner has.
//
// Locks in the column-name fix: the handler must read installed_by,
// not user_id (which doesn't exist on app_installs). Before the fix
// this returned 404 "install not found" in prod because SELECT user_id
// errored on the missing column.
func TestCallback_Projects_GlobalInstallListsOwnerProjects(t *testing.T) {
	s := newTestServer(t)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-A', 1, 'a', 'alpha context')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name, description) VALUES ('proj-B', 1, 'b', 'beta context')`)
	// Different user's project — must NOT leak.
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (2, 'c@d.e', 'x')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name) VALUES ('not-mine', 2, 'theirs')`)

	installID := seedInstall(t, s, "media", "") // global
	s.store.db.Exec(`UPDATE app_installs SET installed_by=1 WHERE id=?`, installID)

	req := httptest.NewRequest("GET", "/apps/callback/projects", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotIDs := map[string]bool{}
	gotDescriptions := map[string]string{}
	for _, p := range out {
		id := p["id"].(string)
		gotIDs[id] = true
		gotDescriptions[id], _ = p["description"].(string)
	}
	if !gotIDs["proj-A"] || !gotIDs["proj-B"] {
		t.Errorf("missing owner projects: got %v", gotIDs)
	}
	if gotDescriptions["proj-A"] != "alpha context" || gotDescriptions["proj-B"] != "beta context" {
		t.Errorf("missing project descriptions: got %v", gotDescriptions)
	}
	if gotIDs["not-mine"] {
		t.Errorf("leaked foreign project")
	}
}

// --- /integrations/:connID/execute auth checks ----------------------

func TestCallback_IntegrationExecute_RequiresInstallToken(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/apps/callback/integrations/42/execute",
		strings.NewReader(`{"tool":"x","input":{}}`))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCallback_IntegrationExecute_RejectsUnboundConnection(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "x",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsExecute},
			Integrations: []sdk.IntegrationDep{
				{Role: "provider", Kind: "integration", CompatibleSlugs: []string{"openai-api"}},
			},
		},
	}
	// Bound to connection 99, NOT the conn we'll request.
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"provider": 99})
	// A different conn that's app_install-owned by a different install
	// (so neither bound, nor owned by us, nor operator-installed).
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "openai-api", AppName: "OpenAI", Name: "x",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"generate_image","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unbound conn, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not reachable") {
		t.Errorf("expected 'not reachable' message, got: %s", rec.Body.String())
	}
}

// Operator-installed connections (created_via='integration') are
// reachable by ANY install with platform.connections.execute
// permission. This is the path Social uses to call list_pages on a
// Facebook integration the operator installed in Settings →
// Integrations — without it, the page picker would 403 and disappear.
func TestCallback_IntegrationExecute_AllowsOperatorInstalledConnection(t *testing.T) {
	s := newTestServer(t)
	// Stub the catalog so the handler can find the upstream tool.
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "facebook-api", Name: "Facebook",
		Tools: []AppToolDef{{Name: "list_pages"}},
	})
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	// No bindings — social doesn't pre-declare facebook-api.
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	// Operator-installed integration connection.
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", AppName: "Facebook", Name: "Facebook Pages",
		ProjectID: "proj-1", CreatedVia: "integration",
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	// We don't have a real Facebook to call out to — the auth check
	// should pass and we'll fail later in resolveConnectionContext or
	// the actual upstream HTTP. Anything other than 403/404 means the
	// auth gate let us through, which is what this test asserts.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("operator connection rejected by auth: %s", rec.Body.String())
	}
}

// App-owned connections (owner_app_install_id == calling install) are
// reachable by their owner. Mirrors social's "I created this via
// platform.oauth.start" flow.
func TestCallback_IntegrationExecute_AllowsOwnedConnection(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "facebook-api", Tools: []AppToolDef{{Name: "list_pages"}},
	})
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", Name: "fb",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: installID,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("owned connection rejected: %s", rec.Body.String())
	}
}

// App-owned connection but owner is a DIFFERENT install — must be
// rejected (otherwise apps could read each other's private OAuth
// tokens just by knowing the connection id).
func TestCallback_IntegrationExecute_RejectsCrossAppOwnedConnection(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema:   sdk.SchemaCurrent,
		Name:     "social",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}},
	}
	installID := seedInstallWithBindings(t, s, "social", manifest, map[string]any{})
	// Owned by a DIFFERENT install (id 999).
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "facebook-api", Name: "fb",
		ProjectID: "proj-1", CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"list_pages","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-app owned conn, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCallback_IntegrationExecute_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	// Manifest declares the dep but NOT the permission.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "x",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "provider", Kind: "integration", CompatibleSlugs: []string{"openai-api"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"provider": 42})
	req := httptest.NewRequest("POST", "/apps/callback/integrations/42/execute",
		strings.NewReader(`{"tool":"generate_image","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing permission, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "platform.connections.execute") {
		t.Errorf("error message should name the missing permission: %s", rec.Body.String())
	}
}

func TestCallback_IntegrationExecute_StripsProjectRoutingInput(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_test"}`))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug:    "anthropic",
		BaseURL: upstream.URL,
		Auth: AppAuthConfig{
			Types:   []string{"api_key"},
			Headers: map[string]string{"x-api-key": "{{api_key}}"},
		},
		Tools: []AppToolDef{{Name: "messages_create", Method: "POST", Path: "/v1/messages"}},
	})

	manifest := integrationOfficialManifest("functions", true)
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermConnectionsExecute}
	installID := seedRunningInstall(t, s, "functions", "", manifest, nil)

	plain, _ := json.Marshal(map[string]string{"api_key": "test-key"})
	encrypted, err := Encrypt(s.secret, string(plain))
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: "anthropic", AppName: "Anthropic", Name: "Anthropic",
		AuthType: "api_key", EncryptedCreds: encrypted, ProjectID: "proj-1",
		CreatedVia: "app_install", OwnerAppInstallID: 999,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	req := httptest.NewRequest("POST", "/apps/callback/integrations/"+itoa(conn.ID)+"/execute",
		strings.NewReader(`{"tool":"messages_create","input":{"_project_id":"proj-1","model":"claude-test","messages":[{"role":"user","content":"hello"}]}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamBody["model"] != "claude-test" {
		t.Fatalf("upstream body missing provider input: %#v", upstreamBody)
	}
	if _, leaked := upstreamBody["_project_id"]; leaked {
		t.Fatalf("routing metadata leaked upstream: %#v", upstreamBody)
	}
}

func TestSanitizeIntegrationCallbackInputDoesNotMutateCaller(t *testing.T) {
	input := map[string]any{"_project_id": " proj-1 ", "model": "claude-test"}
	projectID, clean := sanitizeIntegrationCallbackInput(input)
	if projectID != "proj-1" {
		t.Fatalf("projectID=%q", projectID)
	}
	if _, present := clean["_project_id"]; present {
		t.Fatalf("clean input contains routing field: %#v", clean)
	}
	if input["_project_id"] != " proj-1 " {
		t.Fatalf("caller input mutated: %#v", input)
	}
}

// --- /apps/:appName/call auth checks --------------------------------

func TestCallback_AppCall_RejectsUnboundApp(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "image-studio",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Integrations: []sdk.IntegrationDep{
				{Role: "storage", Kind: "app", CompatibleAppNames: []string{"storage"}},
			},
		},
	}
	// No binding for "storage" — operator declined.
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{})

	req := httptest.NewRequest("POST", "/apps/callback/apps/storage/call",
		strings.NewReader(`{"tool":"files_upload","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unbound app, got %d", rec.Code)
	}
}

func TestCallback_AppCall_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "image-studio",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "storage", Kind: "app", CompatibleAppNames: []string{"storage"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "image-studio", manifest, map[string]any{"storage": 17})
	req := httptest.NewRequest("POST", "/apps/callback/apps/storage/call",
		strings.NewReader(`{"tool":"files_upload","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 missing permission, got %d", rec.Code)
	}
}

func TestCallback_AppCall_GlobalCallerPreservesValidatedProject(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id,email,password_hash,role) VALUES (1,'caller@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(1, "Delegated", "", "")
	if err != nil {
		t.Fatal(err)
	}

	var gotArguments map[string]any
	var gotBoundCaller, gotBoundCallerName string
	targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBoundCaller = r.Header.Get(sdk.HeaderBoundCallerInstallID)
		gotBoundCallerName = r.Header.Get(sdk.HeaderBoundCallerAppName)
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Errorf("decode target request: %v", err)
		}
		gotArguments = rpc.Params.Arguments
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
	}))
	defer targetHTTP.Close()

	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "crm-project-context-test"}
	targetID := seedInstallWithBindings(t, s, "crm-project-context-test", targetManifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, targetID); err != nil {
		t.Fatal(err)
	}
	target := &InstalledApp{
		InstallID: targetID, AppName: "crm-project-context-test", ProjectID: "",
		Manifest: targetManifest, SidecarURL: targetHTTP.URL, Token: "target-token",
	}
	s.installedApps.Add(target)

	callerManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "messaging-project-context-test",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "crm-project-context-test"}},
		},
	}
	callerID := seedInstallWithBindings(t, s, "messaging-project-context-test", callerManifest,
		map[string]any{"crm-project-context-test": targetID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, callerID); err != nil {
		t.Fatal(err)
	}

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/crm-project-context-test/call", strings.NewReader(body))
		req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		return rec
	}

	rec := call(`{"tool":"messaging_inbound_receive","input":{"_project_id":"` + project.ID + `","message_id":1062}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delegated call status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotArguments["_project_id"] != project.ID {
		t.Fatalf("delegated _project_id=%v, want %s", gotArguments["_project_id"], project.ID)
	}
	if gotBoundCaller != itoa(callerID) {
		t.Fatalf("bound caller header=%q, want %d", gotBoundCaller, callerID)
	}
	if gotBoundCallerName != "messaging-project-context-test" {
		t.Fatalf("bound caller app name=%q, want messaging-project-context-test", gotBoundCallerName)
	}

	// Preserve compatibility with global callers bound directly to a
	// project-scoped target, even when an older SDK omits _project_id.
	target.ProjectID = project.ID
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id=?`, project.ID, targetID); err != nil {
		t.Fatal(err)
	}
	gotArguments = nil
	rec = call(`{"tool":"messaging_inbound_receive","input":{"message_id":1063}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("inferred call status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotArguments["_project_id"] != project.ID {
		t.Fatalf("inferred _project_id=%v, want %s", gotArguments["_project_id"], project.ID)
	}
}

func TestCallback_AppCall_RejectsSpoofedOrUnauthorizedProject(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id,email,password_hash,role) VALUES (1,'caller@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "project-auth-test",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}},
	}
	callerID := seedInstallWithBindings(t, s, "project-auth-test", manifest, nil)

	// A project-scoped install cannot replace its pinned project.
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/crm/call",
		strings.NewReader(`{"tool":"x","input":{"_project_id":"proj-2"}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "scoped to another project") {
		t.Fatalf("scoped spoof status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A global install can delegate only to projects visible to its owner.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, callerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id,email,password_hash,role) VALUES (2,'owner@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	foreignProject, err := s.store.CreateProject(2, "Foreign", "", "")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/apps/callback/apps/crm/call",
		strings.NewReader(`{"tool":"x","input":{"_project_id":"`+foreignProject.ID+`"}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "insufficient role") {
		t.Fatalf("foreign project status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCallback_AppProxy_StreamsThroughExactBinding(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	ensureTestAdmin(t, s)
	var gotPath, gotQuery, gotAuth, gotTargetID, gotCallerID, gotCallerName, gotUserID, gotRange, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("project_id")
		gotAuth = r.Header.Get("Authorization")
		gotTargetID = r.Header.Get("X-Apteva-App-Install-ID")
		gotCallerID = r.Header.Get("X-Apteva-Bound-Caller-Install-ID")
		gotCallerName = r.Header.Get(sdk.HeaderBoundCallerAppName)
		gotUserID = r.Header.Get("X-User-ID")
		gotRange = r.Header.Get("Range")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "video-bytes")
	}))
	defer upstream.Close()

	storageManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "storage"}
	storageID := seedInstallWithBindings(t, s, "storage", storageManifest, nil)
	s.installedApps.Add(&InstalledApp{
		InstallID: storageID, AppName: "storage", ProjectID: "proj-1",
		Manifest: storageManifest, SidecarURL: upstream.URL, Token: "storage-token",
	})
	mediaManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "media",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "storage"}},
		},
	}
	mediaID := seedInstallWithBindings(t, s, "media", mediaManifest, map[string]any{"storage": storageID})
	mediaToken, err := s.appInstallToken(mediaID)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/apps/callback/apps/storage/proxy/files/5300/content?project_id=proj-1", nil)
	req.Header.Set("Authorization", "Bearer "+mediaToken)
	req.Header.Set("Range", "bytes=0-1023")
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(rec, req)

	if rec.Code != http.StatusPartialContent || rec.Body.String() != "video-bytes" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if gotPath != "/files/5300/content" || gotQuery != "proj-1" {
		t.Fatalf("upstream route = %q project=%q", gotPath, gotQuery)
	}
	if gotAuth != "Bearer storage-token" {
		t.Fatalf("target credential not swapped: %q", gotAuth)
	}
	if gotTargetID != itoa(storageID) || gotCallerID != itoa(mediaID) {
		t.Fatalf("install headers target=%q caller=%q", gotTargetID, gotCallerID)
	}
	if gotCallerName != "media" {
		t.Fatalf("caller app name=%q, want media", gotCallerName)
	}
	if gotUserID != "1" {
		t.Fatalf("trusted caller user missing: %q", gotUserID)
	}
	if gotRange != "bytes=0-1023" {
		t.Fatalf("Range header lost: %q", gotRange)
	}

	req = httptest.NewRequest(http.MethodPost,
		"/apps/callback/apps/storage/proxy/uploads?project_id=proj-1", strings.NewReader("upload-bytes"))
	req.Header.Set("Authorization", "Bearer "+mediaToken)
	rec = httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(rec, req)
	if rec.Code != http.StatusCreated || gotPath != "/uploads" || gotBody != "upload-bytes" {
		t.Fatalf("streamed upload = status %d path %q body %q", rec.Code, gotPath, gotBody)
	}
}

func TestCallback_AppProxy_RejectsUnboundAndCrossProject(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	ensureTestAdmin(t, s)
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "media",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "storage"}},
		},
	}
	mediaID := seedInstallWithBindings(t, s, "media", manifest, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/apps/callback/apps/storage/proxy/files/1?project_id=proj-1", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(mediaID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unbound: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	storageManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "storage"}
	storageID := seedInstallWithBindings(t, s, "storage", storageManifest, nil)
	s.installedApps.Add(&InstalledApp{
		InstallID: storageID, AppName: "storage", ProjectID: "proj-1",
		Manifest: storageManifest, SidecarURL: "http://127.0.0.1:1", Token: "storage-token",
	})
	bindingJSON, _ := json.Marshal(map[string]any{"storage": storageID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, bindingJSON, mediaID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet,
		"/apps/callback/apps/storage/proxy/files/1?project_id=other-project", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(mediaID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "scoped to another project") {
		t.Fatalf("cross-project: expected project 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// /whoami carries the platform's public_url so apps mint shareable
// URLs without re-reading APTEVA_PUBLIC_URL env (which is frozen at
// sidecar spawn time). Setting changes propagate via the SDK's
// sub-second WhoAmI cache.
func TestCallback_Whoami_ReturnsPublicURL(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.SetSetting("public_url", "https://agents.example.com"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, nil)
	req := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out sdk.InstallIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PublicURL != "https://agents.example.com" {
		t.Errorf("public_url = %q, want https://agents.example.com", out.PublicURL)
	}

	// Live-fresh: change the setting, next whoami call reflects it.
	if err := s.store.SetSetting("public_url", "https://updated.example.com"); err != nil {
		t.Fatalf("update: %v", err)
	}
	req2 := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req2.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req2.Header.Set("X-User-ID", "1")
	rec2 := httptest.NewRecorder()
	s.handleAppCallback(rec2, req2)
	var out2 sdk.InstallIdentity
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if out2.PublicURL != "https://updated.example.com" {
		t.Errorf("after setting change: public_url = %q, want updated", out2.PublicURL)
	}
}

// --- /whoami includes bindings -------------------------------------

func TestCallback_Whoami_ReturnsBindings(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, map[string]any{
		"provider": float64(42),
		"storage":  float64(17),
	})
	req := httptest.NewRequest("GET", "/apps/callback/whoami", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out sdk.InstallIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.InstallID != installID {
		t.Errorf("install_id = %d, want %d", out.InstallID, installID)
	}
	if got := out.Bindings["provider"]; got == nil {
		t.Errorf("bindings.provider missing")
	}
}

// --- /connections/:id/credentials auth + happy-path -----------------

// seedCredsConnection creates a connection with encrypted credentials
// and returns its id. The slug + creds shape are configurable so
// tests can exercise R2 / S3 / generic.
func seedCredsConnection(t *testing.T, s *Server, slug string, creds map[string]string) int64 {
	t.Helper()
	ensureTestAdmin(t, s)
	if len(s.secret) == 0 {
		// Encrypt requires a 32-byte AES key. newTestServer doesn't
		// populate s.secret, so seed a deterministic test key here
		// rather than threading a secret through every callsite.
		s.secret = []byte("0123456789abcdef0123456789abcdef")
	}
	credsJSON, _ := json.Marshal(creds)
	enc, err := Encrypt(s.secret, string(credsJSON))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: slug, AppName: slug, Name: "test",
		AuthType: "aws_sigv4", EncryptedCreds: enc,
		ProjectID: "proj-1", CreatedVia: "integration",
	})
	if err != nil {
		t.Fatalf("create conn: %v", err)
	}
	return conn.ID
}

func TestCallback_GetCredentials_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{"access_key_id": "AKIA"})
	// Manifest declares the role + binding but NOT the permission.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "platform.connections.read_credentials") {
		t.Errorf("expected error to name missing permission, got: %s", rec.Body.String())
	}
}

func TestCallback_GetCredentials_RejectsUnboundConnection(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{"access_key_id": "AKIA"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	// Bind to a DIFFERENT connection id than the one we'll request.
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(99)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCallback_GetCredentials_RejectsIncompatibleSlug(t *testing.T) {
	s := newTestServer(t)
	// Slug stored on the connection is openai-api, manifest only allows cloudflare-r2.
	connID := seedCredsConnection(t, s, "openai-api", map[string]string{"api_key": "sk-1"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "compatible_slugs") {
		t.Errorf("expected error mentioning compatible_slugs, got: %s", rec.Body.String())
	}
}

func TestCallback_GetCredentials_HappyPath(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "cloudflare-r2", map[string]string{
		"account_id":        "abc123",
		"access_key_id":     "AKIATEST",
		"secret_access_key": "shhh",
		"region":            "auto",
	})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "storage",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{
				{Role: "backend", Kind: "integration", CompatibleSlugs: []string{"cloudflare-r2"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "storage", manifest, map[string]any{"backend": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out sdk.ConnectionCredentials
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ConnectionID != connID {
		t.Errorf("ConnectionID = %d, want %d", out.ConnectionID, connID)
	}
	if out.Slug != "cloudflare-r2" {
		t.Errorf("Slug = %q, want cloudflare-r2", out.Slug)
	}
	if out.Fields["account_id"] != "abc123" || out.Fields["access_key_id"] != "AKIATEST" {
		t.Errorf("Fields missing expected values: %+v", out.Fields)
	}
}

func TestCallback_GetPublicConfig_ReturnsOnlyCatalogPublicFields(t *testing.T) {
	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	if err := s.catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatal(err)
	}
	connID := seedCredsConnection(t, s, "stripe", map[string]string{
		"token":          "sk_test_secret",
		"publishableKey": "pk_test_browser",
		"webhookSecret":  "whsec_secret",
	})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "billing",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermConnectionsReadPublicConfig},
			Integrations: []sdk.IntegrationDep{
				{Role: "payment_processor", Kind: "integration", CompatibleSlugs: []string{"stripe"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "billing", manifest, map[string]any{"payment_processor": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/public-config", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out sdk.ConnectionPublicConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Fields["publishableKey"] != "pk_test_browser" {
		t.Fatalf("public key missing: %#v", out.Fields)
	}
	if _, ok := out.Fields["token"]; ok {
		t.Fatalf("secret token leaked: %#v", out.Fields)
	}
	if _, ok := out.Fields["webhookSecret"]; ok {
		t.Fatalf("webhook secret leaked: %#v", out.Fields)
	}
}

func TestCallback_GetPublicConfig_RejectsMissingPermission(t *testing.T) {
	s := newTestServer(t)
	connID := seedCredsConnection(t, s, "stripe", map[string]string{"publishableKey": "pk_test_browser"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "billing",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "payment_processor", Kind: "integration", CompatibleSlugs: []string{"stripe"}},
			},
		},
	}
	installID := seedInstallWithBindings(t, s, "billing", manifest, map[string]any{"payment_processor": float64(connID)})

	req := httptest.NewRequest("GET", "/apps/callback/connections/"+itoa(connID)+"/public-config", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- helpers --------------------------------------------------------

// installBoundConnection / installBoundApp / etc. are exercised
// indirectly by the auth-failure tests above. A direct helper test:

func TestInstallBoundConnection_Match(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "x"}
	installID := seedInstallWithBindings(t, s, "x", manifest, map[string]any{"provider": float64(42)})
	role, ok := installBoundConnection(s, installID, 42)
	if !ok || role != "provider" {
		t.Fatalf("expected role=provider, got role=%q ok=%v", role, ok)
	}
	_, ok = installBoundConnection(s, installID, 999)
	if ok {
		t.Fatal("expected miss for unbound connection id")
	}
}

// --- Project-aware app routing -------------------------------------

func TestRegistry_GetByNameAndProject_PrefersProjectMatch(t *testing.T) {
	r := NewInstalledAppsRegistry()
	r.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "alpha"})
	r.Add(&InstalledApp{InstallID: 2, AppName: "storage", ProjectID: "beta"})
	r.Add(&InstalledApp{InstallID: 3, AppName: "storage", ProjectID: ""})

	if got := r.GetByNameAndProject("storage", "alpha"); got == nil || got.InstallID != 1 {
		t.Errorf("alpha → install_id=1, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", "beta"); got == nil || got.InstallID != 2 {
		t.Errorf("beta → install_id=2, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", "gamma"); got == nil || got.InstallID != 3 {
		t.Errorf("gamma (no match) → global install_id=3, got %+v", got)
	}
	if got := r.GetByNameAndProject("storage", ""); got == nil || got.InstallID != 3 {
		t.Errorf("empty project → global install_id=3, got %+v", got)
	}
	if got := r.GetByNameAndProject("missing", "alpha"); got != nil {
		t.Errorf("unknown app → nil, got %+v", got)
	}
}

func TestRegistry_GetByNameAndProject_NoGlobalFallback(t *testing.T) {
	r := NewInstalledAppsRegistry()
	r.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "alpha"})
	// No global install. A request scoped to "beta" must NOT silently
	// route to alpha — that's the bug we're fixing.
	if got := r.GetByNameAndProject("storage", "beta"); got != nil {
		t.Errorf("beta with no global → nil, got install_id=%d", got.InstallID)
	}
}

func TestInstallBoundAppID_ResolvesBoundTarget(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	// Seed a "storage" app + two installs under different projects.
	s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('storage','git','','','{}')`)
	var storageAppID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name='storage'`).Scan(&storageAppID)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1,'a@b.c','x')`)
	res1, _ := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-alpha', 'running', 1)`, storageAppID)
	storageInstall1, _ := res1.LastInsertId()
	res2, _ := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-beta', 'running', 1)`, storageAppID)
	storageInstall2, _ := res2.LastInsertId()
	s.installedApps.Add(&InstalledApp{InstallID: storageInstall1, AppName: "storage", ProjectID: "proj-alpha"})
	s.installedApps.Add(&InstalledApp{InstallID: storageInstall2, AppName: "storage", ProjectID: "proj-beta"})

	// Caller (media) bound to storageInstall2 specifically.
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "media",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "storage"}},
		},
	}
	mediaInstallID := seedInstallWithBindings(t, s, "media", manifest, map[string]any{
		"storage": float64(storageInstall2),
	})

	got := installBoundAppID(s, mediaInstallID, "storage")
	if got != storageInstall2 {
		t.Errorf("expected bound install_id=%d, got %d", storageInstall2, got)
	}
}

func TestInstallBoundAppID_DoesNotDependOnBootRegistry(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()

	targetManifest := sdk.Manifest{Name: "instances"}
	targetID := seedRunningInstall(t, s, "instances", "", targetManifest, nil)
	callerManifest := sdk.Manifest{
		Name: "fleet",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{{
				Role: "host_provider", Kind: "app", CompatibleAppNames: []string{"instances"},
			}},
		},
	}
	callerID := seedRunningInstall(t, s, "fleet", "", callerManifest, map[string]any{
		"host_provider": targetID,
	})

	if got := installBoundAppID(s, callerID, "instances"); got != targetID {
		t.Fatalf("bound install=%d, want %d while runtime registry is still empty", got, targetID)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, targetID); err != nil {
		t.Fatal(err)
	}
	if got := installBoundAppID(s, callerID, "instances"); got != 0 {
		t.Fatalf("non-running target authorized as install %d", got)
	}
}

// GET /apps/callback/agents — the SDK's optional AgentDirectoryClient.
// Requires platform.instances.read; project-scoped installs are pinned
// to their own project regardless of ?project_id; platform helpers and
// foreign users' agents never appear.
func TestCallback_AgentList_PermissionAndScope(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (2, 'other@test.local', 'x')`)
	a1, err := s.store.CreateAgent(1, "alpha", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.store.CreateAgent(1, "beta", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	a3, err := s.store.CreateAgent(1, "gamma", "d", "autonomous", "{}", "proj-2")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.store.CreateAgent(2, "foreign", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.CreateAgent(1, "helper", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, helper.ID)

	withPerm := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "a2a-list-test"}
	withPerm.Requires.Permissions = []sdk.Permission{sdk.PermInstancesRead}
	installID := seedInstallWithBindings(t, s, "a2a-list-test", withPerm, nil)

	list := func(installID int64, query string) ([]sdk.PlatformInstance, int, string) {
		req := httptest.NewRequest(http.MethodGet, "/apps/callback/agents"+query, nil)
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		var out []sdk.PlatformInstance
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out, rec.Code, rec.Body.String()
	}
	ids := func(agents []sdk.PlatformInstance) map[int64]bool {
		m := map[int64]bool{}
		for _, a := range agents {
			m[a.ID] = true
		}
		return m
	}

	// Project-scoped install: pinned to proj-1 even when asking for proj-2.
	got, code, body := list(installID, "?project_id=proj-2")
	if code != http.StatusOK {
		t.Fatalf("scoped list: %d %s", code, body)
	}
	m := ids(got)
	if !m[a1.ID] || !m[a2.ID] || m[a3.ID] || m[foreign.ID] || m[helper.ID] {
		t.Fatalf("scoped list wrong agents: %v", m)
	}

	// Global install: all owner agents; project filter honored.
	s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID)
	got, code, _ = list(installID, "")
	if code != http.StatusOK {
		t.Fatalf("global list: %d", code)
	}
	m = ids(got)
	if !m[a1.ID] || !m[a2.ID] || !m[a3.ID] || m[foreign.ID] || m[helper.ID] {
		t.Fatalf("global list wrong agents: %v", m)
	}
	got, _, _ = list(installID, "?project_id=proj-2")
	m = ids(got)
	if m[a1.ID] || !m[a3.ID] {
		t.Fatalf("global filtered list wrong agents: %v", m)
	}

	// Install without the permission: 403.
	noPerm := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "a2a-noperm-test"}
	noPermID := seedInstallWithBindings(t, s, "a2a-noperm-test", noPerm, nil)
	if _, code, _ := list(noPermID, ""); code != http.StatusForbidden {
		t.Fatalf("missing permission: got %d, want 403", code)
	}
}

// The agent list annotates attached_to_caller from app_agent_bindings
// so capability-aware apps (a2a) can scope discovery to agents that
// actually hold their tools.
func TestCallback_AgentList_AnnotatesAttachment(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	bound, err := s.store.CreateAgent(1, "bound", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := s.store.CreateAgent(1, "unbound", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := s.store.CreateAgent(1, "disabled", "d", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "a2a-attach-test"}
	manifest.Requires.Permissions = []sdk.Permission{sdk.PermInstancesRead}
	installID := seedInstallWithBindings(t, s, "a2a-attach-test", manifest, nil)
	s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, bound.ID)
	s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 0)`, installID, disabled.ID)

	req := httptest.NewRequest(http.MethodGet, "/apps/callback/agents", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var out []sdk.PlatformInstance
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	attached := map[int64]bool{}
	for _, a := range out {
		attached[a.ID] = a.AttachedToCaller
	}
	if !attached[bound.ID] || attached[unbound.ID] || attached[disabled.ID] {
		t.Fatalf("attachment annotation wrong: %v", attached)
	}
}
