package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeModelsAPI serves the provider, model and config routes the gateway
// reads, and records every write.
type fakeModelsAPI struct {
	t        *testing.T
	project  string
	restart  map[string]any
	mu       sync.Mutex
	requests []string
	writes   []map[string]any
}

func (f *fakeModelsAPI) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Method == http.MethodGet && (r.URL.Path == "/settings/new-agent-provider" || r.URL.Path == "/connections/runtime") {
		if got := r.URL.Query().Get("project_id"); got != f.project {
			f.t.Errorf("%s project_id=%q, want %q", r.URL.Path, got, f.project)
		}
	}
	switch {
	case r.URL.Path == "/settings/new-agent-provider":
		writeJSON(w, map[string]any{"effective_provider": "claude-subscription", "available_providers": []string{"claude-subscription", "openai"}})
	case r.URL.Path == "/connections/runtime":
		writeJSON(w, []map[string]any{
			{"id": 4, "app_name": "OpenAI realtime", "provider_key": "openai", "role": "realtime"},
			{"id": 153, "app_name": "Claude subscription", "provider_key": "claude-subscription", "role": "llm", "runtime_config": map[string]any{"model_large": "opus"}},
			{"id": 9, "app_name": "OpenAI", "provider_key": "openai", "role": "llm"},
			{"id": 10, "app_name": "OpenAI (second key)", "provider_key": "openai", "role": "llm"},
		})
	case r.URL.Path == "/connections/153/models":
		writeJSON(w, []map[string]any{{"id": "opus", "name": "Claude Opus", "context_size": 200000}, {"id": "sonnet", "name": "Claude Sonnet"}, {"id": "haiku", "name": "Claude Haiku"}})
	case r.URL.Path == "/connections/9/models":
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/config"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.writes = append(f.writes, body)
		f.mu.Unlock()
		reply := map[string]any{"directive": "x"}
		for key, value := range f.restart {
			reply[key] = value
		}
		writeJSON(w, reply)
	case r.Method == http.MethodPut:
		f.mu.Lock()
		f.writes = append(f.writes, map[string]any{"path": r.URL.Path})
		f.mu.Unlock()
		writeJSON(w, map[string]any{})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeModelsAPI) fetched(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.HasSuffix(request, " "+path) {
			return true
		}
	}
	return false
}

func newFakeModelsAPI(t *testing.T, project string) (*fakeModelsAPI, gatewayAPIClient) {
	t.Helper()
	fake := &fakeModelsAPI{t: t, project: project}
	ts := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(ts.Close)
	return fake, gatewayAPIClient{baseURL: ts.URL, userID: 1, instanceSecret: "fixture"}
}

func TestGatewayAgentModelsListsProjectProvidersAndModels(t *testing.T) {
	fake, client := newFakeModelsAPI(t, "p1")
	out, err := handleGatewayAgentTool("agents_models", map[string]any{}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var got struct {
		ProjectID string `json:"project_id"`
		Providers []struct {
			Provider     string               `json:"provider"`
			Name         string               `json:"name"`
			NewAgents    bool                 `json:"default_for_new_agents"`
			DefaultModel string               `json:"default_model"`
			Models       []gatewayModelChoice `json:"models"`
			ModelsTotal  int                  `json:"models_total"`
			ModelsError  string               `json:"models_error"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != "p1" || len(got.Providers) != 2 {
		t.Fatalf("providers: %s", raw)
	}
	claude, openai := got.Providers[0], got.Providers[1]
	if claude.Provider != "claude-subscription" || claude.Name != "Claude subscription" || !claude.NewAgents || claude.DefaultModel != "opus" {
		t.Fatalf("claude row: %s", raw)
	}
	if len(claude.Models) != 3 || claude.Models[1] != (gatewayModelChoice{ID: "sonnet", Name: "Claude Sonnet"}) || claude.ModelsTotal != 3 {
		t.Fatalf("claude models: %s", raw)
	}
	if strings.Contains(string(raw), "context_size") {
		t.Fatalf("model rows are not compact: %s", raw)
	}
	// The first LLM connection is the one agents run on; realtime rows are skipped.
	if openai.Provider != "openai" || openai.Name != "OpenAI" || openai.NewAgents || openai.Models != nil {
		t.Fatalf("openai row: %s", raw)
	}
	if !strings.Contains(openai.ModelsError, "502") || !strings.Contains(openai.ModelsError, "upstream unavailable") {
		t.Fatalf("model list failure not reported: %s", raw)
	}
	if fake.fetched("/connections/10/models") || fake.fetched("/connections/4/models") {
		t.Fatal("models fetched from a connection agents do not use")
	}

	fake, client = newFakeModelsAPI(t, "p1")
	out, err = handleGatewayAgentTool("agents_models", map[string]any{"provider": "Claude Subscription", "query": "SON"}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), `"models":[{"id":"sonnet","name":"Claude Sonnet"}]`) || strings.Contains(string(raw), `"provider":"openai"`) {
		t.Fatalf("provider filter and query: %s", raw)
	}
	if fake.fetched("/connections/9/models") {
		t.Fatal("filtered call fetched another provider's models")
	}

	_, err = handleGatewayAgentTool("agents_models", map[string]any{"provider": "mistral"}, "p1", client, nil, "")
	if err == nil || !strings.Contains(err.Error(), "available providers: claude-subscription, openai") {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestGatewayAgentsUpdateSetsCheckedProviderAndModel(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "Support", "Help.", "cautious", `{"default_provider":"claude-subscription"}`, "p1")
	if err != nil {
		t.Fatal(err)
	}
	unpinned, err := s.store.CreateAgent(1, "Unpinned", "Help.", "cautious", `{}`, "p1")
	if err != nil {
		t.Fatal(err)
	}
	update := func(fake *fakeModelsAPI, client gatewayAPIClient, args map[string]any) (map[string]any, error) {
		if _, ok := args["id"]; !ok {
			args["id"] = float64(agent.ID)
		}
		out, err := handleGatewayAgentTool("agents_update", args, "p1", client, s.store, "")
		if err != nil {
			return nil, err
		}
		return out.(map[string]any), nil
	}

	// Model alone uses the agent's saved provider; a running agent reports its restart.
	fake, client := newFakeModelsAPI(t, "p1")
	fake.restart = map[string]any{"restarted": true}
	got, err := update(fake, client, map[string]any{"model": "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes: %v", fake.writes)
	}
	body, _ := json.Marshal(fake.writes[0])
	if string(body) != `{"model_override":"sonnet","providers":[{"default":true,"name":"claude-subscription"}]}` {
		t.Fatalf("config body: %s", body)
	}
	if got["provider"] != "claude-subscription" || got["model"] != "sonnet" || got["model_checked"] != true || got["restarted"] != true {
		t.Fatalf("receipt: %v", got)
	}
	if changed := fmt.Sprint(got["changed"]); changed != "[provider model]" {
		t.Fatalf("changed: %s", changed)
	}

	// A failed restart is passed on.
	fake, client = newFakeModelsAPI(t, "p1")
	fake.restart = map[string]any{"restarted": false, "restart_error": "core did not come back"}
	got, err = update(fake, client, map[string]any{"provider": "claude-subscription", "model": "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	if got["restarted"] != false || got["restart_error"] != "core did not come back" {
		t.Fatalf("restart failure: %v", got)
	}

	// Rejected before anything is written, the name included.
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"name": "Renamed", "model": "Claude Sonnet"}, `Did you mean sonnet?`},
		{map[string]any{"name": "Renamed", "model": "gpt-9"}, `model "gpt-9" is not offered by provider "claude-subscription"`},
		{map[string]any{"name": "Renamed", "provider": "mistral", "model": "large"}, "available providers: claude-subscription, openai"},
		{map[string]any{"name": "Renamed", "provider": ""}, "provider must be a provider name"},
		{map[string]any{"name": "Renamed", "model": 4}, "model must be a model id string"},
		{map[string]any{"name": "Renamed", "config": map[string]any{"default_provider": "openai"}}, "config.default_provider is not accepted"},
		{map[string]any{"name": "Renamed", "config": `{"model_override":{"provider":"openai","model":"gpt"}}`}, "config.model_override is not accepted"},
		{map[string]any{"id": float64(unpinned.ID), "name": "Renamed", "model": "sonnet"}, "provider is required with model"},
	} {
		fake, client = newFakeModelsAPI(t, "p1")
		_, err := update(fake, client, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err=%v, want %q", tc.args, err, tc.want)
		}
		if len(fake.writes) != 0 {
			t.Fatalf("%v: wrote %v before rejecting", tc.args, fake.writes)
		}
	}

	// An unreadable model list leaves the check to the server, and says so.
	fake, client = newFakeModelsAPI(t, "p1")
	got, err = update(fake, client, map[string]any{"provider": "openai", "model": "gpt-x"})
	if err != nil {
		t.Fatal(err)
	}
	if got["model_checked"] != false || !strings.Contains(fmt.Sprint(got["warning"]), "was not checked") {
		t.Fatalf("unchecked model: %v", got)
	}
	// The agent is stopped in the store, so the change applies on its next start.
	if got["applies"] != "on next start" {
		t.Fatalf("stopped agent: %v", got)
	}

	// An empty model returns to the provider's default model; a provider
	// alone keeps the model and fetches no model list.
	fake, client = newFakeModelsAPI(t, "p1")
	if _, err := update(fake, client, map[string]any{"model": ""}); err != nil {
		t.Fatal(err)
	}
	if body, _ := json.Marshal(fake.writes[0]); string(body) != `{"model_override":"","providers":[{"default":true,"name":"claude-subscription"}]}` {
		t.Fatalf("clear: %s", body)
	}
	fake, client = newFakeModelsAPI(t, "p1")
	if _, err := update(fake, client, map[string]any{"id": float64(unpinned.ID), "provider": "openai"}); err != nil {
		t.Fatal(err)
	}
	if body, _ := json.Marshal(fake.writes[0]); string(body) != `{"providers":[{"default":true,"name":"openai"}]}` {
		t.Fatalf("provider only: %s", body)
	}
	if fake.fetched("/connections/9/models") {
		t.Fatal("provider-only change fetched a model list")
	}
}

// mockGeminiCatalog answers Gemini's model list and lets loopback calls to
// the test API through.
func mockGeminiCatalog(t *testing.T, response string) {
	t.Helper()
	old := http.DefaultTransport
	oldCache := globalModelCache
	globalModelCache = &modelCache{entries: make(map[string]modelCacheEntry)}
	http.DefaultTransport = policyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "generativelanguage.googleapis.com" {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
		}
		host := r.URL.Hostname()
		if host != "127.0.0.1" && host != "localhost" {
			return nil, fmt.Errorf("unexpected test request: %s", r.URL.Host)
		}
		return old.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = old; globalModelCache = oldCache })
}

func TestGatewayAgentModelsAgainstServerRoutes(t *testing.T) {
	mockGeminiCatalog(t, `{"models":[
 {"name":"models/gemini-2.5-flash-image","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-3.1-pro-preview","displayName":"Gemini 3.1 Pro","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-3.7-flash","displayName":"Gemini 3.7 Flash","supportedGenerationMethods":["generateContent"]}]}`)
	s := runtimeTestServer(t)
	s.catalog.Register(geminiPolicyFixture(t))
	addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "test-key"})
	if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name,description) VALUES('p1',1,'p1','')`); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "Gemini agent", "Help.", "cautious", `{"default_provider":"google"}`, "p1")
	if err != nil {
		t.Fatal(err)
	}
	ts := newGatewayAgentAPITestServer(s)
	t.Cleanup(ts.Close)
	client := gatewayAPIClient{baseURL: ts.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}

	listed, err := handleGatewayAgentTool("agents_models", map[string]any{}, "p1", client, s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(listed)
	if !strings.Contains(string(raw), `"provider":"google"`) || !strings.Contains(string(raw), `{"id":"gemini-3.7-flash","name":"Gemini 3.7 Flash"}`) {
		t.Fatalf("agents_models: %s", raw)
	}
	if strings.Contains(string(raw), "flash-image") {
		t.Fatalf("listed a model the policy excludes: %s", raw)
	}

	if _, err := handleGatewayAgentTool("agents_update", map[string]any{"id": float64(agent.ID), "model": "gemini-2.5-flash-image"}, "p1", client, s.store, ""); err == nil || !strings.Contains(err.Error(), "is not offered") {
		t.Fatalf("excluded model accepted: %v", err)
	}
	out, err := handleGatewayAgentTool("agents_update", map[string]any{"id": float64(agent.ID), "model": "gemini-3.7-flash"}, "p1", client, s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt := out.(map[string]any); receipt["model_checked"] != true || receipt["applies"] != "on next start" {
		t.Fatalf("receipt: %v", receipt)
	}
	saved, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if configuredAgentDefaultProvider(saved.Config) != "google" || configuredAgentModelOverride(saved.Config, "google") != "gemini-3.7-flash" {
		t.Fatalf("saved config: %s", saved.Config)
	}

	got, err := handleGatewayAgentTool("agents_get", map[string]any{"id": float64(agent.ID)}, "p1", client, s.store, "")
	if err != nil {
		t.Fatal(err)
	}
	if row := got.(map[string]any); row["provider"] != "google" || row["model"] != "gemini-3.7-flash" {
		t.Fatalf("agents_get: %v", row)
	}
}

func TestGatewayRoutesEveryAgentTool(t *testing.T) {
	for _, name := range []string{"agents_models", "agents_update", "agent_list_activity", "agent_templates_list", "agent_templates_save"} {
		if !isGatewayAgentTool(name) {
			t.Fatalf("%s is not routed to the agent tool handler", name)
		}
	}
	if isGatewayAgentTool("list_providers") {
		t.Fatal("list_providers routed to the agent tool handler")
	}
}

func TestPlatformMCPHelperToolsNameProjectConversationLimits(t *testing.T) {
	s := newTestServer(t)
	s.instanceSecret = "platform-mcp-fixture-secret"
	user, err := s.store.CreateUser("platform-mcp-models@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPlatformRole(user.ID, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.BindAgentThreadScope(helper.ID, "chat-models", "project-models", 91); err != nil {
		t.Fatal(err)
	}
	var executed []string
	s.platformGatewayExec = func(_ context.Context, _ int64, _ string, request []byte) ([]byte, error) {
		var rpc struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(request, &rpc)
		executed = append(executed, rpc.Method+" "+rpc.Params.Name+" "+fmt.Sprint(rpc.Params.Arguments["project_id"]))
		if rpc.Method == "tools/list" {
			return []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"list_providers","description":"List compact active provider summaries."},
				{"name":"activate_provider","description":"Activate a provider."},
				{"name":"agents_models","description":"List the LLM providers."},
				{"name":"agents_update","description":"Update agent."}]}}`), nil
		}
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`), nil
	}
	call := func(body string) string {
		req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, helper)), strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:43212"
		req.Header.Set("X-Apteva-Caller-Agent", itoa64(helper.ID))
		rec := httptest.NewRecorder()
		s.handlePlatformMCP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	var listed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &listed); err != nil {
		t.Fatal(err)
	}
	descriptions := map[string]string{}
	for _, tool := range listed.Result.Tools {
		descriptions[tool.Name] = tool.Description
	}
	if descriptions["list_providers"] != "List compact active provider summaries. Not available in project conversations. Use agents_models there." {
		t.Fatalf("list_providers: %q", descriptions["list_providers"])
	}
	if descriptions["activate_provider"] != "Activate a provider. Not available in project conversations." {
		t.Fatalf("activate_provider: %q", descriptions["activate_provider"])
	}
	for _, name := range []string{"agents_models", "agents_update"} {
		if strings.Contains(descriptions[name], "Not available") {
			t.Fatalf("%s labelled blocked: %q", name, descriptions[name])
		}
	}

	blocked := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_providers","arguments":{"_apteva_caller_thread":"chat-models"}}}`)
	if !strings.Contains(blocked, `not available in a project conversation; use agents_models instead`) {
		t.Fatalf("blocked call: %s", blocked)
	}
	call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agents_models","arguments":{"project_id":"forged","_apteva_caller_thread":"chat-models"}}}`)
	if last := executed[len(executed)-1]; last != "tools/call agents_models project-models" {
		t.Fatalf("agents_models in a project conversation: %v", executed)
	}
}
