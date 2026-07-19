package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func createProviderSelectionFixture(t *testing.T, s *Server, providerTypeID int64, name, projectID string, state map[string]any) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, providerTypeID, "llm", name, encrypted, projectID); err != nil {
		t.Fatal(err)
	}
}

func providerConfigByName(t *testing.T, providers []any, name string) map[string]any {
	t.Helper()
	for _, raw := range providers {
		provider, _ := raw.(map[string]any)
		if provider["name"] == name {
			return provider
		}
	}
	t.Fatalf("provider %q missing from %#v", name, providers)
	return nil
}

func TestGetProviderPoolUsesProjectProviderAndStableCodexOrder(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()

	createProviderSelectionFixture(t, s, 2, "OpenAI", "", map[string]any{
		"OPENAI_API_KEY": "global-key", "model_large": "global-openai",
	})
	createProviderSelectionFixture(t, s, 15, "OpenAI Codex", "", map[string]any{
		"credentials": map[string]any{"access_token": "codex-token"},
		"model_large": "gpt-5.6-terra", "model_medium": "gpt-5.6-terra", "model_small": "gpt-5.6-terra",
		"model_capabilities": map[string]any{"gpt-5.6-terra": map[string]any{"context_window": 272000}},
	})
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{
		"OPENAI_API_KEY": "project-key", "model_large": "project-openai",
	})

	pool := s.GetProviderPool(1, "project-a")
	if len(pool) != 3 { // openai, codex, and openai-realtime
		t.Fatalf("pool=%#v", pool)
	}
	if pool[0].Type != "openai" || pool[0].ModelLarge != "project-openai" {
		t.Fatalf("project provider did not win deterministically: %#v", pool)
	}
	if pool[1].Type != "openai-codex" || pool[2].Type != "openai-realtime" {
		t.Fatalf("provider order=%#v, want openai, openai-codex, openai-realtime", pool)
	}
}

func TestEnsureAgentDefaultProviderPersistsEffectiveSelection(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent, err := s.store.CreateAgent(1, "provider-default", "directive", "autonomous", `{"include_channels":true}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	pool := []ProviderInfo{
		{Type: "openai", ModelLarge: "gpt-5.4-mini"},
		{Type: "openai-codex", ModelLarge: "gpt-5.6-terra"},
	}

	selected, err := s.ensureAgentDefaultProvider(agent, pool)
	if err != nil {
		t.Fatal(err)
	}
	if selected != "openai" {
		t.Fatalf("selected=%q, want deterministic first provider", selected)
	}
	persisted, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentDefaultProvider(persisted.Config); got != "openai" {
		t.Fatalf("persisted default=%q config=%s", got, persisted.Config)
	}
}

func TestUpdateConfigHydratesAndPersistsCodexSelection(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{
		"OPENAI_API_KEY": "openai-key",
		"model_large":    "gpt-5.4-mini", "model_medium": "gpt-5.4-mini", "model_small": "gpt-5.4-mini",
	})
	createProviderSelectionFixture(t, s, 15, "OpenAI Codex", "project-a", map[string]any{
		"credentials": map[string]any{"access_token": "codex-token"},
		"model_large": "gpt-5.6-terra", "model_medium": "gpt-5.6-terra", "model_small": "gpt-5.6-terra",
		"model_capabilities": map[string]any{
			"gpt-5.6-terra": map[string]any{"context_window": 272000, "supports_parallel_tool_calls": true},
		},
	})
	agent, err := s.store.CreateAgent(1, "codex-selection", "directive", "autonomous", `{"include_channels":true}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{
		"providers": []map[string]any{
			{"name": "openai", "default": false},
			{"name": "openai-codex", "default": true},
		},
	})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	persisted, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentDefaultProvider(persisted.Config); got != "openai-codex" {
		t.Fatalf("persisted default=%q config=%s", got, persisted.Config)
	}

	raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	providers, _ := config["providers"].([]any)
	codex := providerConfigByName(t, providers, "openai-codex")
	if codex["default"] != true {
		t.Fatalf("codex default=%v", codex["default"])
	}
	models, _ := codex["models"].(map[string]any)
	if models["large"] != "gpt-5.6-terra" || models["small"] != "gpt-5.6-terra" {
		t.Fatalf("codex models=%#v", models)
	}
	capabilities, _ := codex["model_capabilities"].(map[string]any)
	if _, ok := capabilities["gpt-5.6-terra"]; !ok {
		t.Fatalf("codex capabilities=%#v", capabilities)
	}
	openAI := providerConfigByName(t, providers, "openai")
	if openAI["default"] != false {
		t.Fatalf("openai default=%v", openAI["default"])
	}
	providerConfigByName(t, providers, "openai-realtime")
}

func TestUpdateConfigForwardsHydratedCodexSelectionToRunningCore(t *testing.T) {
	var forwarded map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" || r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&forwarded); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	defer core.Close()

	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{
		"OPENAI_API_KEY": "openai-key",
		"model_large":    "gpt-5.4-mini", "model_medium": "gpt-5.4-mini", "model_small": "gpt-5.4-mini",
	})
	createProviderSelectionFixture(t, s, 15, "OpenAI Codex", "project-a", map[string]any{
		"credentials": map[string]any{"access_token": "codex-token"},
		"model_large": "gpt-5.6-terra", "model_medium": "gpt-5.6-terra", "model_small": "gpt-5.6-terra",
		"model_capabilities": map[string]any{
			"gpt-5.6-terra": map[string]any{"context_window": 272000},
		},
	})
	agent, err := s.store.CreateAgent(1, "running-codex-selection", "directive", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	port := core.Listener.Addr().(*net.TCPAddr).Port
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{
		port: port, pid: os.Getpid(), coreAPIKey: "core-test", reattached: true,
	}
	s.agents.mu.Unlock()
	t.Cleanup(func() {
		s.agents.mu.Lock()
		delete(s.agents.processes, agent.ID)
		s.agents.mu.Unlock()
	})

	req := authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{
		"providers": []map[string]any{
			{"name": "openai", "default": false},
			{"name": "openai-codex", "default": true},
		},
	})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	providers, _ := forwarded["providers"].([]any)
	codex := providerConfigByName(t, providers, "openai-codex")
	if codex["default"] != true {
		t.Fatalf("forwarded codex default=%v", codex["default"])
	}
	models, _ := codex["models"].(map[string]any)
	if models["large"] != "gpt-5.6-terra" {
		t.Fatalf("forwarded codex models=%#v", models)
	}
	if _, ok := codex["model_capabilities"]; !ok {
		t.Fatalf("forwarded codex capabilities missing: %#v", codex)
	}
	providerConfigByName(t, providers, "openai-realtime")
}

func TestBuildAgentCoreProviderConfigsAppliesOnlyMatchingModelOverride(t *testing.T) {
	pool := []ProviderInfo{
		{Type: "openai", ModelLarge: "openai-large", ModelMedium: "openai-medium", ModelSmall: "openai-small"},
		{Type: "openai-codex", ModelLarge: "codex-large", ModelMedium: "codex-medium", ModelSmall: "codex-small"},
	}
	config := `{"default_provider":"openai-codex","model_override":{"provider":"openai-codex","model":"gpt-5.6-sol"}}`
	providers := buildAgentCoreProviderConfigs(pool, config)
	codex := providerConfigByName(t, mapsToAny(providers), "openai-codex")
	models, _ := codex["models"].(map[string]string)
	if models["large"] != "gpt-5.6-sol" || models["medium"] != "gpt-5.6-sol" || models["small"] != "gpt-5.6-sol" {
		t.Fatalf("agent model override not applied to all tiers: %#v", models)
	}
	openAI := providerConfigByName(t, mapsToAny(providers), "openai")
	openAIModels, _ := openAI["models"].(map[string]string)
	if openAIModels["large"] != "openai-large" {
		t.Fatalf("non-selected provider changed: %#v", openAIModels)
	}
}

func mapsToAny(values []map[string]any) []any {
	out := make([]any, len(values))
	for i := range values {
		out[i] = values[i]
	}
	return out
}

func TestUpdateConfigPersistsAgentModelOverrideWithoutChangingProviderDefaults(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{
		"OPENAI_API_KEY": "openai-key",
		"model_large":    "provider-large", "model_medium": "provider-medium", "model_small": "provider-small",
	})
	agent, err := s.store.CreateAgent(1, "model-override", "directive", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{
		"providers":      []map[string]any{{"name": "openai", "default": true}},
		"model_override": "gpt-helper-specific",
	})
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	persisted, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentModelOverride(persisted.Config, "openai"); got != "gpt-helper-specific" {
		t.Fatalf("persisted model override=%q config=%s", got, persisted.Config)
	}
	raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	providerRows, _ := config["providers"].([]any)
	openAI := providerConfigByName(t, providerRows, "openai")
	models, _ := openAI["models"].(map[string]any)
	if models["large"] != "gpt-helper-specific" || models["medium"] != "gpt-helper-specific" || models["small"] != "gpt-helper-specific" {
		t.Fatalf("stopped core config models=%#v", models)
	}

	providerRowsFromStore := s.GetProviderPool(1, "project-a")
	if providerRowsFromStore[0].ModelLarge != "provider-large" || providerRowsFromStore[0].ModelMedium != "provider-medium" {
		t.Fatalf("provider-wide defaults changed: %#v", providerRowsFromStore[0])
	}
}
