package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func liveProviderPool() []ProviderInfo {
	catalog := &RuntimeRealtimeCatalog{ProviderKey: "openai-realtime", DefaultModel: "gpt-realtime-2.1", Models: []RuntimeRealtimeModel{
		{ID: "gpt-live-1", Available: true, Voices: []string{"brise", "sillage"}, DefaultVoice: "brise"},
		{ID: "gpt-realtime-2.1", Available: true},
	}, Voices: []string{"marin"}}
	return []ProviderInfo{{Type: "openai", ModelLarge: "gpt-6.1-sol", ModelMedium: "gpt-6.1-sol", ModelSmall: "gpt-6.1-sol", Realtime: catalog},
		{Type: "openai-realtime", ModelLarge: "gpt-realtime-2.1", ModelMedium: "gpt-realtime-2.1", ModelSmall: "gpt-realtime-2.1", Realtime: catalog}}
}

func TestRealtimeSharedIntegrationPreservesTextAndRestarts(t *testing.T) {
	pool := liveProviderPool()
	saved := `{"default_provider":"openai","realtime_provider":"openai","realtime_model":"gpt-live-1","providers":[{"name":"openai","realtime_models":{"large":"gpt-live-1"},"realtime_voice":"sillage","realtime_execution":{"mode":"provider","provider":"openai","model":"medium"}}]}`
	providers, _, err := hydrateCoreProviderConfigs(pool, "openai", mergeSavedRealtimeProviders(saved, []map[string]any{{"name": "openai", "default": true}}))
	if err != nil {
		t.Fatal(err)
	}
	applyRealtimeSelection(providers, "openai", "gpt-live-1")
	restart := buildAgentCoreProviderConfigs(pool, saved)
	for _, configs := range [][]map[string]any{providers, restart} {
		if configs[1]["default"] != true {
			t.Fatal("shared alias lost realtime default selection")
		}
		first := configs[0]
		if first["default"] != true {
			t.Fatal("voice changed text default")
		}
		models, _ := json.Marshal(first["models"])
		if string(models) != `{"large":"gpt-6.1-sol","medium":"gpt-6.1-sol","small":"gpt-6.1-sol"}` {
			t.Fatalf("text models changed: %s", models)
		}
		if first["realtime_execution"] == nil || first["realtime_models"] == nil || first["realtime_voice"] != "sillage" {
			t.Fatalf("lost voice configuration: %#v", first)
		}
	}
	// Switching back from the shared alias must replace the shared voice map,
	// which Core prioritizes over its legacy transport companion.
	applyRealtimeSelection(providers, "openai-realtime", "gpt-realtime-2.1")
	if providers[0]["realtime_models"].(map[string]string)["large"] != "gpt-realtime-2.1" {
		t.Fatal("stale shared voice override")
	}
	if err := validateRealtimeModelVoice(pool, "openai", "gpt-live-1", "sillage"); err != nil {
		t.Fatal(err)
	}
	if err := validateRealtimeModelVoice(pool, "openai", "gpt-live-1", "marin"); err == nil {
		t.Fatal("allowed incompatible model voice")
	}
	providers[0]["realtime_execution"] = map[string]any{"mode": "unknown"}
	if err := validateRealtimeProviderModelMaps(pool, providers); err == nil {
		t.Fatal("accepted unknown execution mode")
	}
}

func TestRealtimeStoppedEditRetainsOptionsThroughUnrelatedUpdate(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()
	if err := s.catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatal(err)
	}
	createProviderSelectionFixture(t, s, 2, "OpenAI", "project-a", map[string]any{"OPENAI_API_KEY": "fixture", "model_large": "gpt-5.4-mini", "model_medium": "gpt-5.4-mini", "model_small": "gpt-5.4-mini"})
	agent, err := s.store.CreateAgent(1, "voice-persistence", "directive", "cautious", `{"default_provider":"openai"}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(body map[string]any) {
		t.Helper()
		req := authedRequest(t, http.MethodPut, "/instances/1/config", "", body)
		rec := httptest.NewRecorder()
		s.handleUpdateConfig(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
		}
	}
	apply(map[string]any{"providers": []map[string]any{{"name": "openai", "default": true, "realtime_models": map[string]string{"large": "gpt-realtime-2.1"}, "realtime_execution": map[string]string{"mode": "thread", "model": "medium"}}}})
	apply(map[string]any{"providers": []map[string]any{{"name": "openai", "default": true}}, "directive": "edited"})
	stored, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := buildAgentCoreProviderConfigs(s.GetProviderPool(1, "project-a"), stored.Config)
	if rebuilt[0]["realtime_execution"] == nil || rebuilt[0]["realtime_models"] == nil {
		t.Fatalf("restart lost options: %#v", rebuilt[0])
	}
	raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	diskProvider := providerConfigByName(t, disk["providers"].([]any), "openai")
	if diskProvider["realtime_execution"].(map[string]any)["model"] != "medium" {
		t.Fatalf("disk lost override: %s", raw)
	}
	// Upgrade old installations whose only saved options live on disk.
	imported := buildAgentCoreProviderConfigs(s.GetProviderPool(1, "project-a"), realtimeConfigFromDisk(`{"default_provider":"openai"}`, disk))
	if imported[0]["realtime_execution"] == nil {
		t.Fatal("disk fallback not imported")
	}
}
