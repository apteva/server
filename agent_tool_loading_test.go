package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateAutomaticToolLoading(t *testing.T) {
	valid := []string{
		`{"enabled": true}`,
		`{"enabled": false}`,
		`{"enabled": true, "include_memory": false, "max_tools": 8, "max_schema_tokens": 6000, "workflow_tools": ["crm_contacts_get_context", "processes_step-claim"]}`,
		`{"enabled": true, "max_tools": 0, "max_schema_tokens": 0}`,
	}
	for _, raw := range valid {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		if err := validateAutomaticToolLoading(value); err != nil {
			t.Errorf("%s: unexpected error %v", raw, err)
		}
	}
	invalid := map[string]string{
		`true`:                                "must be an object",
		`null`:                                "must be an object",
		`{"max_schema_tokens": 6000}`:         "enabled is required",
		`{"enabled": "yes"}`:                  "enabled must be true or false",
		`{"enabled": true, "max_tools": 21}`:  "max_tools",
		`{"enabled": true, "max_tools": 2.5}`: "max_tools",
		`{"enabled": true, "max_schema_tokens": -1}`:              "max_schema_tokens",
		`{"enabled": true, "max_schema_tokens": 16001}`:           "max_schema_tokens",
		`{"enabled": true, "workflow_tools": "crm_contacts_get"}`: "workflow_tools",
		`{"enabled": true, "workflow_tools": ["crm contacts"]}`:   "workflow_tools",
		`{"enabled": true, "max_schema_token": 6000}`:             "is not a setting",
	}
	for raw, want := range invalid {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		err := validateAutomaticToolLoading(value)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", raw, err, want)
		}
	}
	tooMany := map[string]any{"enabled": true, "workflow_tools": make([]any, 21)}
	for i := range tooMany["workflow_tools"].([]any) {
		tooMany["workflow_tools"].([]any)[i] = "tool_name"
	}
	if err := validateAutomaticToolLoading(tooMany); err == nil {
		t.Error("21 workflow tools accepted")
	}
}

func TestRestoreAutomaticToolLoadingFillsOnlyMissingValue(t *testing.T) {
	saved := `{"automatic_tool_loading": {"enabled": true, "max_schema_tokens": 6000}}`

	empty := map[string]any{}
	restoreAutomaticToolLoading(1, empty, saved)
	got, _ := empty[automaticToolLoadingKey].(map[string]any)
	if got["max_schema_tokens"] != float64(6000) {
		t.Fatalf("missing value not restored: %#v", empty)
	}

	// Core's own file wins: a running Core can be changed through its /config.
	coreOwned := map[string]any{automaticToolLoadingKey: map[string]any{"enabled": false}}
	restoreAutomaticToolLoading(1, coreOwned, saved)
	if !reflect.DeepEqual(coreOwned[automaticToolLoadingKey], map[string]any{"enabled": false}) {
		t.Fatalf("Core's value overwritten: %#v", coreOwned)
	}

	// A malformed saved value is never handed to Core.
	malformed := map[string]any{}
	restoreAutomaticToolLoading(1, malformed, `{"automatic_tool_loading": true}`)
	if _, ok := malformed[automaticToolLoadingKey]; ok {
		t.Fatalf("malformed value restored: %#v", malformed)
	}
}

// toolLoadingAgent creates an agent in a project with a provider, which any
// config patch needs.
func toolLoadingAgent(t *testing.T, s *Server) *Agent {
	t.Helper()
	s.secret = testSecret()
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{
		"OPENAI_API_KEY": "openai-key",
		"model_large":    "gpt-5.4-mini", "model_medium": "gpt-5.4-mini", "model_small": "gpt-5.4-mini",
	})
	agent, err := s.store.CreateAgent(1, "tool-loading", "directive", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// fakeRunningCore registers a fake Core for agentID and records each /config PUT.
func fakeRunningCore(t *testing.T, s *Server, agentID int64) *[]map[string]any {
	t.Helper()
	var puts []map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"mcp_servers": []any{}, "builtin_capabilities": map[string]any{}})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		puts = append(puts, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	t.Cleanup(core.Close)
	port := core.Listener.Addr().(*net.TCPAddr).Port
	s.agents.mu.Lock()
	s.agents.processes[agentID] = &runningAgent{port: port, pid: os.Getpid(), coreAPIKey: "core-test", reattached: true}
	s.agents.mu.Unlock()
	t.Cleanup(func() {
		s.agents.mu.Lock()
		delete(s.agents.processes, agentID)
		s.agents.mu.Unlock()
	})
	return &puts
}

func TestUpdateConfigSendsToolLoadingFromConfigPatchToRunningCore(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent := toolLoadingAgent(t, s)
	puts := fakeRunningCore(t, s, agent.ID)

	setting := map[string]any{"enabled": true, "include_memory": false, "max_schema_tokens": float64(6000), "workflow_tools": []any{"processes_step_claim"}}
	patch, _ := json.Marshal(map[string]any{automaticToolLoadingKey: setting})
	req := authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{"config": string(patch)})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*puts) != 1 || !reflect.DeepEqual((*puts)[0][automaticToolLoadingKey], setting) {
		t.Fatalf("Core did not receive the setting: %#v", *puts)
	}
	saved, _ := s.store.GetAgentByID(agent.ID)
	var cfg map[string]any
	_ = json.Unmarshal([]byte(saved.Config), &cfg)
	if !reflect.DeepEqual(cfg[automaticToolLoadingKey], setting) {
		t.Fatalf("saved config=%s", saved.Config)
	}

	// An unrelated settings edit merges the saved config, but must not re-send
	// the saved value over the one Core holds.
	req = authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{"config": `{"onboarding_staged": true}`})
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := (*puts)[1][automaticToolLoadingKey]; ok {
		t.Fatalf("unrelated edit re-sent automatic_tool_loading: %#v", (*puts)[1])
	}
}

func TestUpdateConfigRejectsMalformedToolLoading(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent := toolLoadingAgent(t, s)
	puts := fakeRunningCore(t, s, agent.ID)
	for _, body := range []map[string]any{
		{"config": `{"automatic_tool_loading": true}`},
		{"config": `{"automatic_tool_loading": {"max_schema_tokens": 6000}}`},
		{automaticToolLoadingKey: map[string]any{"enabled": true, "max_tools": 99}},
		{automaticToolLoadingKey: map[string]any{"enabled": true}, "config": `{"automatic_tool_loading": {"enabled": false}}`},
	} {
		req := authedRequest(t, http.MethodPut, "/instances/1/config", "", body)
		rec := httptest.NewRecorder()
		s.handleUpdateConfig(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if len(*puts) != 0 {
		t.Fatalf("rejected update reached Core: %#v", *puts)
	}
	saved, _ := s.store.GetAgentByID(agent.ID)
	if strings.Contains(saved.Config, automaticToolLoadingKey) {
		t.Fatalf("rejected value saved: %s", saved.Config)
	}
}

func TestUpdateStoppedConfigWritesToolLoadingForNextStart(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent := toolLoadingAgent(t, s)
	setting := map[string]any{"enabled": true, "max_schema_tokens": float64(6000)}
	req := authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{"config": `{"automatic_tool_loading": {"enabled": true, "max_schema_tokens": 6000}}`})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk[automaticToolLoadingKey], setting) {
		t.Fatalf("Core config file=%s", data)
	}

	req = authedRequest(t, http.MethodGet, "/instances/1/config", "", nil)
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(out[automaticToolLoadingKey], setting) {
		t.Fatalf("stopped GET /config=%s", rec.Body.String())
	}
}

func TestCreateInstanceRejectsMalformedToolLoading(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	req := authedRequest(t, http.MethodPost, "/instances", "", map[string]any{
		"name": "tool-loading", "directive": "do stuff", "start": false,
		"config": `{"automatic_tool_loading": true}`,
	})
	rec := httptest.NewRecorder()
	s.handleCreateInstance(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "automatic_tool_loading") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if agents, _ := s.store.ListAgents(1, ""); len(agents) != 0 {
		t.Fatalf("agent created with malformed setting: %d", len(agents))
	}
}
