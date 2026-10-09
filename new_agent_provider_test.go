package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newAgentProviderFixture(t *testing.T) *Server {
	t.Helper()
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "openai", "openai", map[string]string{"OPENAI_API_KEY": "{{credentials.api_key}}"})
	registerRuntimeApp(s, "anthropic", "anthropic", map[string]string{"ANTHROPIC_API_KEY": "{{credentials.api_key}}"})
	addConnection(t, s, "openai", "OpenAI", "", map[string]string{"api_key": "test"})
	addConnection(t, s, "anthropic", "Anthropic", "project-a", map[string]string{"api_key": "test"})
	return s
}

func providerPreferenceRequest(t *testing.T, s *Server, method, project string, body any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleNewAgentProviderSettings(rec, authedRequest(t, method, "/settings/new-agent-provider?project_id="+project, "", body))
	return rec
}

func TestNewAgentProviderPreferenceScopeAndFallback(t *testing.T) {
	s := newAgentProviderFixture(t)
	for _, tc := range []struct{ project, provider string }{{"", "anthropic"}, {"project-a", "openai"}} {
		rec := providerPreferenceRequest(t, s, http.MethodPut, tc.project, map[string]string{"provider": tc.provider})
		if rec.Code != 200 {
			t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct{ project, expected string }{{"", "anthropic"}, {"project-a", "openai"}, {"project-b", "openai"}} {
		rec := providerPreferenceRequest(t, s, http.MethodGet, tc.project, nil)
		var got newAgentProviderSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.EffectiveProvider != tc.expected {
			t.Fatalf("%s: %#v", tc.project, got)
		}
		for _, p := range got.AvailableProviders {
			if isRealtimeProviderType(p) {
				t.Fatal("realtime offered as default")
			}
		}
	}
	rec := providerPreferenceRequest(t, s, http.MethodPut, "project-a", map[string]string{"provider": ""})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if got := s.newAgentProviderSettings(1, "project-a"); got.Provider != "" || got.EffectiveProvider != "anthropic" {
		t.Fatalf("clear did not inherit: %#v", got)
	}
	// User preferences must never leak into another user's account.
	if got := s.newAgentProviderSettings(2, "project-a"); got.Provider != "" || got.InheritedProvider != "" {
		t.Fatalf("cross-user preference: %#v", got)
	}
}

func TestNewAgentProviderPreferenceRejectsUnavailableAndUnauthorized(t *testing.T) {
	s := newAgentProviderFixture(t)
	for _, provider := range []string{"unknown", "openai-realtime", "anthropic"} {
		rec := providerPreferenceRequest(t, s, http.MethodPut, "project-b", map[string]string{"provider": provider})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", provider, rec.Code, rec.Body.String())
		}
	}
	rec := providerPreferenceRequest(t, s, http.MethodPut, "", map[string]string{})
	if rec.Code != 400 {
		t.Fatalf("missing field status=%d", rec.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := authedRequest(t, method, "/settings/new-agent-provider?project_id=project-a", "", map[string]string{"provider": "openai"})
		req.Header.Set("X-User-ID", "2")
		rec := httptest.NewRecorder()
		s.handleNewAgentProviderSettings(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unauthorized status=%d", rec.Code)
		}
	}
}

func TestNewAgentCreationPinsPreferenceAndPreservesExistingAgents(t *testing.T) {
	s := newAgentProviderFixture(t)
	save := func(provider string) {
		t.Helper()
		rec := providerPreferenceRequest(t, s, http.MethodPut, "project-a", map[string]string{"provider": provider})
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
	}
	create := func(name, config string) Agent {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleCreateInstance(rec, authedRequest(t, "POST", "/instances", "", map[string]any{
			"name": name, "project_id": "project-a", "start": false, "config": config, "bound_app_install_ids": []int64{},
		}))
		if rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		var agent Agent
		if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil {
			t.Fatal(err)
		}
		return agent
	}
	save("openai")
	first := create("first", `{"unconscious":true}`)
	save("anthropic")
	second := create("second", `{}`)
	explicit := create("explicit", `{"default_provider":"openai"}`)
	persisted, err := s.store.GetAgentByID(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		agent    *Agent
		expected string
	}{{persisted, "openai"}, {&second, "anthropic"}, {&explicit, "openai"}} {
		if got := configuredAgentDefaultProvider(tc.agent.Config); got != tc.expected {
			t.Fatalf("agent %s provider=%s want=%s", tc.agent.Name, got, tc.expected)
		}
	}
	// Selecting a creation default must not reorder the runtime pool for existing agents.
	pool := s.GetProviderPool(1, "project-a")
	if pool[0].Type != "anthropic" {
		t.Fatalf("pool order changed: %#v", pool)
	}
}

func TestAgentConfigPatchPreservesCreationProvider(t *testing.T) {
	for _, tc := range []struct {
		name, patch, expected string
	}{
		{"partial", `{"unconscious":true}`, "openai-codex"},
		{"empty", `{}`, "openai-codex"},
		{"explicit", `{"default_provider":"venice"}`, "venice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			registerAndLogin(t, s)
			s.secret = testSecret()
			createProviderSelectionFixture(t, s, 14, "Venice", "project-a", map[string]any{
				"VENICE_API_KEY": "test", "model_large": "venice-model", "model_medium": "venice-model", "model_small": "venice-model",
			})
			createProviderSelectionFixture(t, s, 15, "OpenAI Codex", "project-a", map[string]any{
				"credentials": map[string]any{"access_token": "test"}, "model_capabilities": map[string]any{},
				"model_large": "gpt-6.1-sol", "model_medium": "gpt-6.1-sol", "model_small": "gpt-6.1-sol",
			})
			if err := s.store.SetSetting(newAgentProviderSettingKey(1, "project-a"), "openai-codex"); err != nil {
				t.Fatal(err)
			}
			config := s.applyNewAgentProviderDefault(1, "project-a", `{"include_channels":false,"unconscious":false}`)
			agent, err := s.store.CreateAgent(1, "config-patch", "Idle", "cautious", config, "project-a")
			if err != nil {
				t.Fatal(err)
			}
			if got := effectiveProviderDefault(s.GetProviderPool(1, "project-a"), ""); got != "venice" {
				t.Fatalf("fixture fallback=%q, want a different provider from the creation preference", got)
			}
			rec := httptest.NewRecorder()
			s.handleUpdateConfig(rec, authedRequest(t, http.MethodPut, "/instances/1/config", "", map[string]any{"config": tc.patch}))
			if rec.Code != http.StatusOK {
				t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
			}
			persisted, err := s.store.GetAgentByID(agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := configuredAgentDefaultProvider(persisted.Config); got != tc.expected {
				t.Fatalf("provider=%q want=%q config=%s", got, tc.expected, persisted.Config)
			}
			var saved map[string]any
			if err := json.Unmarshal([]byte(persisted.Config), &saved); err != nil {
				t.Fatal(err)
			}
			if tc.name == "partial" && saved["unconscious"] != true {
				t.Fatalf("patch not applied: %s", persisted.Config)
			}
			providers := buildAgentCoreProviderConfigs(s.GetProviderPool(1, "project-a"), persisted.Config)
			if providerConfigByName(t, mapsToAny(providers), tc.expected)["default"] != true {
				t.Fatalf("startup providers lost selection: %#v", providers)
			}
			raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var disk struct {
				Providers []any `json:"providers"`
			}
			if err := json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			if providerConfigByName(t, disk.Providers, tc.expected)["default"] != true {
				t.Fatalf("persisted core config lost selection: %s", raw)
			}
		})
	}
}
