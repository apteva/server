package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspaceSetupDraftResumesAndEnforcesProjectAccess(t *testing.T) {
	s := newTestServer(t)
	owner, _ := s.store.CreateUser("setup-owner@test.local", "hash")
	other, _ := s.store.CreateUser("setup-other@test.local", "hash")
	project, _ := s.store.CreateProject(owner.ID, "Workspace", "", "")
	s.store.SetUserInterfaceLevel(owner.ID, "business")
	call := func(method string, userID int64, body string) *httptest.ResponseRecorder {
		r := helperLifecycleRequest(method, "/projects/"+project.ID+"/setup/session", userID, body)
		w := httptest.NewRecorder()
		s.handleProject(w, r)
		return w
	}
	initial := call(http.MethodGet, owner.ID, "")
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), `"category":""`) || !strings.Contains(initial.Body.String(), `"mode":"choice"`) {
		t.Fatalf("initial: %d %s", initial.Code, initial.Body.String())
	}
	for _, mode := range []string{"choice", "browse", "scratch"} {
		body := `{"category":"business","mode":"` + mode + `"}`
		if w := call(http.MethodPut, owner.ID, body); w.Code != http.StatusOK {
			t.Fatalf("mode %s: %d %s", mode, w.Code, w.Body.String())
		}
	}
	body := `{"category":"business","preset_id":"business-lead-generation","description":"Find clinic leads","mode":"ai"}`
	if w := call(http.MethodPut, owner.ID, body); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	w := call(http.MethodGet, owner.ID, "")
	if !strings.Contains(w.Body.String(), "Find clinic leads") || !strings.Contains(w.Body.String(), `"mode":"ai"`) {
		t.Fatal(w.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if w := call(method, other.ID, body); w.Code == 200 {
			t.Fatalf("other user could %s draft", method)
		}
	}
	if w := call(http.MethodPut, owner.ID, `{"category":"business","preset_id":"missing","mode":"ai"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(http.MethodPut, owner.ID, `{"category":"business","mode":"invalid"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	agents, _ := s.store.ListAgentsInProject(project.ID)
	if len(agents) != 0 {
		t.Fatal("saving a draft created agents")
	}
	if _, err := s.store.GetPlatformHelper(owner.ID); err == nil {
		t.Fatal("browsing activated Helper")
	}
}

func TestWorkspaceSetupOverridesAndRetryPreserveRenamedAgent(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "setup-project")
	catalog, _ := s.projectPresetCatalog(1)
	preset := catalog.ByID["business-lead-generation"]
	key := preset.Agents[0].Key
	body := map[string]any{"preset_id": preset.ID, "description": "Qualify clinic leads", "agent_overrides": []ProjectPresetAgentOverride{{Key: key, Name: "My lead assistant", Directive: "Research leads. Ask before sending outreach.", Mode: "cautious"}}}
	call := func(suffix string) *httptest.ResponseRecorder {
		r := authedRequest(t, http.MethodPost, "/projects/setup-project/setup/"+suffix, "", body)
		w := httptest.NewRecorder()
		s.handleProject(w, r)
		return w
	}
	w := call("preview")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "My lead assistant") {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
	agents, _ := s.store.ListAgentsInProject("setup-project")
	if len(agents) != 0 {
		t.Fatal("preview created agents")
	}
	w = call("apply")
	if w.Code != 200 {
		t.Fatalf("apply %d %s", w.Code, w.Body.String())
	}
	agents, _ = s.store.ListAgentsInProject("setup-project")
	if len(agents) != 1 || agents[0].Name != "My lead assistant" || agents[0].Mode != "cautious" {
		t.Fatalf("agents %+v", agents)
	}
	s.store.RenameAgent(agents[0].ID, "Renamed later")
	w = call("apply")
	var result struct {
		Created  []Agent `json:"created_agents"`
		Existing []Agent `json:"existing_agents"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Created) != 0 || len(result.Existing) != 1 || result.Existing[0].Name != "Renamed later" {
		t.Fatalf("retry: %s", w.Body.String())
	}
	body["agent_overrides"] = []ProjectPresetAgentOverride{{Key: "unknown", Name: "Invalid", Directive: "Do work", Mode: "cautious"}}
	if w := call("apply"); w.Code != 400 {
		t.Fatalf("invalid override: %d %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceSetupHelperUsesSharedAuthenticatedEndpoints(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Secret") != "fixture" || r.Header.Get("X-Apteva-MCP-User-ID") != "7" {
			t.Error("missing caller identity")
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/denied/") {
			http.Error(w, "not a project editor", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"warnings":["connection needed"]}`))
	}))
	defer ts.Close()
	api := gatewayAPIClient{baseURL: ts.URL, userID: 7, instanceSecret: "fixture"}
	for _, name := range []string{"setup_presets_list", "setup_preview", "setup_apply"} {
		result, err := handleGatewaySetupTool(name, map[string]any{"project_id": "p", "preset_id": "preset", "description": "My goal"}, "", api)
		if err != nil || result == nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if strings.Join(paths, ",") != "GET /templates,POST /projects/p/setup/preview,POST /projects/p/setup/apply" {
		t.Fatal(paths)
	}
	if _, err := handleGatewaySetupTool("setup_apply", map[string]any{}, "", api); err == nil {
		t.Fatal("accepted absent project")
	}
	if _, err := handleGatewaySetupTool("setup_apply", map[string]any{"project_id": "denied"}, "", api); err == nil {
		t.Fatal("ignored authorization failure")
	}
}

func TestWorkspaceSetupToolsUseTrustedConversationProject(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"setup_presets_list", "setup_preview", "setup_apply"} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{"project_id": "model-chosen-project", "preset_id": "personal-assistant", "description": "Plan my week"}}})
		scoped, err := s.scopeProjectGatewayRequest(raw, "trusted-project")
		if err != nil {
			t.Fatalf("%s blocked in Helper conversation: %v", name, err)
		}
		var call struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scoped, &call); err != nil {
			t.Fatal(err)
		}
		if call.Params.Arguments["project_id"] != "trusted-project" {
			t.Fatalf("%s escaped project scope", name)
		}
	}
}
