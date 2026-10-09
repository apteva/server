package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentSourceAgentPolicyPreservesRealtimeSelection(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"realtime_enabled": true,
		"realtime_provider": "google-realtime",
		"realtime_model": "gemini-3.8-live",
		"realtime_voice": "Kore",
		"realtime_voice_mcp": ["flexylead-bookings"],
		"mcp_servers": [
			{
				"name": "flexylead-bookings",
				"url": "http://source.test/mcp",
				"tool_loading": {"default": "always"}
			},
			{"name": "crm", "url": "http://source.test/crm"}
		]
	}`)

	bookings := policy.mcpConfig("flexylead-bookings", "http://runtime.test/bookings")
	if bookings["no_spawn"] != false {
		t.Fatalf("bookings no_spawn=%#v, want false", bookings["no_spawn"])
	}
	if got := bookings["tool_loading"]; !reflect.DeepEqual(got, map[string]any{"default": "always"}) {
		t.Fatalf("bookings tool_loading=%#v", got)
	}
	if crm := policy.mcpConfig("crm", "http://runtime.test/crm"); crm["no_spawn"] != true {
		t.Fatalf("crm no_spawn=%#v, want true", crm["no_spawn"])
	}
	if unknown := policy.mcpConfig("runtime-only", "http://runtime.test/other"); unknown["no_spawn"] != true {
		t.Fatalf("runtime-only no_spawn=%#v, want true", unknown["no_spawn"])
	}

	config := map[string]any{}
	policy.copyRealtimeConfig(config)
	if config["realtime_enabled"] != true || config["realtime_voice"] != "Kore" {
		t.Fatalf("realtime config=%#v", config)
	}
	if config["realtime_provider"] != "google-realtime" || config["realtime_model"] != "gemini-3.8-live" {
		t.Fatalf("realtime selection was lost: %#v", config)
	}
	if got := config["realtime_voice_mcp"]; !reflect.DeepEqual(got, []any{"flexylead-bookings"}) {
		t.Fatalf("realtime_voice_mcp=%#v", got)
	}
	pool := environmentRealtimeTestPool()
	if err := policy.validateRealtimeSelection(pool); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	providers := buildAgentCoreProviderConfigs(pool, string(encoded))
	for _, provider := range providers {
		if provider["name"] != "google-realtime" {
			continue
		}
		models := provider["models"].(map[string]string)
		if provider["default"] != true || models["large"] != "gemini-3.8-live" || models["medium"] != "gemini-3.8-live" || models["small"] != "gemini-3.8-live" {
			t.Fatalf("Core did not receive the pinned realtime model: %#v", provider)
		}
		return
	}
	t.Fatal("Core realtime provider configuration is missing")
}

func environmentRealtimeTestPool() []ProviderInfo {
	return append(localRuntimeProviderPool(), ProviderInfo{
		Type: "google-realtime", ModelLarge: "gemini-3.1-flash-live-preview",
		ModelMedium: "gemini-3.1-flash-live-preview", ModelSmall: "gemini-3.1-flash-live-preview",
		Realtime: &RuntimeRealtimeCatalog{Models: []RuntimeRealtimeModel{
			{ID: "gemini-3.1-flash-live-preview", Available: true},
			{ID: "gemini-3.8-live", Available: true},
			{ID: "gemini-3.8-live-extended-thinking", Available: false},
		}},
	})
}

func TestEnvironmentRealtimeInvalidSelectionFailsBeforeCreatingClone(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"missing_provider", `{"realtime_provider":"xai-realtime","realtime_model":"grok-voice-latest"}`, "not configured"},
		{"unknown_model", `{"realtime_provider":"google-realtime","realtime_model":"unknown-live"}`, "not listed"},
		{"unavailable_model", `{"realtime_provider":"google-realtime","realtime_model":"gemini-3.8-live-extended-thinking"}`, "unavailable"},
		{"model_only", `{"realtime_model":"unknown-live"}`, "not listed"},
		{"malformed_provider", `{"realtime_provider":42}`, "realtime_provider must be a string"},
		{"malformed_model", `{"realtime_model":true}`, "realtime_model must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, runtime, source := runtimeStartupFixture(t)
			source.Config = tc.config
			_, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: environmentRealtimeTestPool(), StartPaused: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "environment realtime") {
				t.Fatalf("invalid selection did not fail clearly: %v", err)
			}
			var count int
			if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE kind='environment_agent'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("temporary agent created for invalid selection: count=%d err=%v", count, err)
			}
			if len(runtime.pendingAgents) != 0 || len(runtime.Agents()) != 0 {
				t.Fatal("invalid selection reserved a runtime agent")
			}
			if source.Config != tc.config {
				t.Fatal("source agent configuration was changed")
			}
		})
	}
}

func TestEnvironmentRealtimeSelectionIsGenericAndKeepsUnpinnedDefaults(t *testing.T) {
	pool := append(environmentRealtimeTestPool(), ProviderInfo{Type: "openai-realtime", ModelLarge: "gpt-realtime-2.1", ModelMedium: "gpt-realtime-2.1-mini", ModelSmall: "gpt-realtime-2.1-mini"})
	for _, config := range []string{
		`{}`, `{"realtime_enabled":false}`, `{"realtime_model":""}`,
		`{"realtime_provider":"google-realtime"}`, `{"realtime_model":"gemini-3.8-live"}`,
		`{"realtime_provider":"openai-realtime","realtime_model":"gpt-realtime-2.1-mini"}`,
	} {
		if err := parseEnvironmentSourceAgentPolicy(config).validateRealtimeSelection(pool); err != nil {
			t.Fatalf("valid selection %s rejected: %v", config, err)
		}
	}
}

func TestEnvironmentSourceAgentPolicyFallsBackToSourceNoSpawn(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"mcp_servers": [
			{"name": "bookings", "url": "http://source.test/bookings"},
			{"name": "private", "url": "http://source.test/private", "no_spawn": true},
			{"name": "malformed", "url": "http://source.test/malformed", "no_spawn": "false"}
		]
	}`)

	if got := policy.mcpConfig("bookings", "http://runtime.test/bookings")["no_spawn"]; got != false {
		t.Fatalf("bookings no_spawn=%#v, want false", got)
	}
	if got := policy.mcpConfig("private", "http://runtime.test/private")["no_spawn"]; got != true {
		t.Fatalf("private no_spawn=%#v, want true", got)
	}
	if got := policy.mcpConfig("malformed", "http://runtime.test/malformed")["no_spawn"]; got != true {
		t.Fatalf("malformed no_spawn=%#v, want true", got)
	}
	if got := policy.mcpConfig("runtime-only", "http://runtime.test/other")["no_spawn"]; got != true {
		t.Fatalf("runtime-only no_spawn=%#v, want true", got)
	}
}

func TestEnvironmentSourceAgentPolicyExplicitEmptyAllowlistDeniesAll(t *testing.T) {
	policy := parseEnvironmentSourceAgentPolicy(`{
		"realtime_voice_mcp": [],
		"mcp_servers": [{"name": "bookings", "url": "http://source.test/bookings"}]
	}`)
	if got := policy.mcpConfig("bookings", "http://runtime.test/bookings")["no_spawn"]; got != true {
		t.Fatalf("bookings no_spawn=%#v, want true", got)
	}
}
