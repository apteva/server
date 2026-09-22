package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeGoFallbackCatalogIncludesCurrentModels(t *testing.T) {
	models := openCodeGoModels()
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	for _, want := range []string{"glm-5.2", "kimi-k2.7-code", "qwen3.7-max", "qwen3.7-plus", "hy3-preview"} {
		if !ids[want] {
			t.Fatalf("fallback catalog missing %q", want)
		}
	}
}

func TestEnrichOpenCodeGoModel(t *testing.T) {
	model := enrichOpenCodeGoModel(ModelInfo{ID: "glm-5.2"})
	if model.Name != "GLM-5.2" {
		t.Fatalf("name = %q, want GLM-5.2", model.Name)
	}
	if model.ContextSize != 128_000 {
		t.Fatalf("context size = %d, want 128000", model.ContextSize)
	}

	unknown := enrichOpenCodeGoModel(ModelInfo{ID: "new-model"})
	if unknown.Name != "New Model" {
		t.Fatalf("unknown name = %q, want New Model", unknown.Name)
	}
}

func TestParseFireworksInferenceModelsIsAuthoritative(t *testing.T) {
	models, err := parseFireworksInferenceModels([]byte(`{"data":[
		{"id":"accounts/fireworks/models/deepseek-v4p1-flash"},
		{"id":"accounts/fireworks/models/kimi-k2p6","name":"Kimi K2.6"},
		{"id":"accounts/fireworks/models/deepseek-v4p1-flash"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models=%v", models)
	}
	if models[0].ID != "accounts/fireworks/models/deepseek-v4p1-flash" || models[0].Name != "deepseek-v4p1-flash" {
		t.Fatalf("unexpected first model: %+v", models[0])
	}
	if len(models[0].Methods) != 1 || models[0].Methods[0] != "chat_completion" || models[0].SupportedAPI == nil || !*models[0].SupportedAPI {
		t.Fatalf("inference metadata missing: %+v", models[0])
	}
	if _, err := parseFireworksInferenceModels([]byte(`{bad`)); err == nil {
		t.Fatal("malformed catalog accepted")
	}
}

func TestFetchFireworksModelsDoesNotAdmitNativeOnlyModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing authorization header")
		}
		switch r.URL.Path {
		case "/inference/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"accounts/fireworks/models/deepseek-v4p1-flash"}]}`))
		case "/v1/accounts/fireworks/models":
			_, _ = w.Write([]byte(`{"models":[
				{"name":"accounts/fireworks/models/chronos-hermes-13b-v2","displayName":"Chronos","state":"READY","contextLength":4096},
				{"name":"accounts/fireworks/models/deepseek-v4p1-flash","displayName":"DeepSeek V4.1 Flash","state":"READY","contextLength":131072}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldInference, oldNative := fireworksInferenceModelsURL, fireworksNativeModelsURL
	fireworksInferenceModelsURL = server.URL + "/inference/v1/models"
	fireworksNativeModelsURL = server.URL + "/v1/accounts/fireworks/models"
	t.Cleanup(func() {
		fireworksInferenceModelsURL = oldInference
		fireworksNativeModelsURL = oldNative
	})

	models, err := fetchFireworksModels("test-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "accounts/fireworks/models/deepseek-v4p1-flash" {
		t.Fatalf("native-only model leaked into eligible catalog: %+v", models)
	}
	if models[0].Name != "DeepSeek V4.1 Flash" || models[0].ContextSize != 131072 {
		t.Fatalf("native metadata not applied: %+v", models[0])
	}
}

func TestFireworksPolicyRejectsNonInferenceReadyAndRanksPreferredModel(t *testing.T) {
	app := fireworksPolicyFixture(t)
	policy := app.Runtime.ModelPolicy
	models := []ModelInfo{
		{ID: "accounts/fireworks/models/chronos-hermes-13b-v2", Methods: []string{"chat_completion"}},
		{ID: "accounts/fireworks/models/kimi-k2p6", Methods: []string{"chat_completion"}},
		{ID: "accounts/fireworks/models/deepseek-v4p1-flash", Methods: []string{"chat_completion"}},
		{ID: "accounts/fireworks/models/deepseek-v4p2-flash"},
	}
	eligible := policy.filter(models)
	if len(eligible) != 2 {
		t.Fatalf("eligible=%v", eligible)
	}
	for _, tier := range runtimeModelTiers {
		if got := policy.selectTier(eligible, tier); got != "accounts/fireworks/models/kimi-k2p6" {
			t.Fatalf("%s=%q", tier, got)
		}
	}
	state := map[string]any{
		"model_large":  "accounts/fireworks/models/chronos-hermes-13b-v2",
		"model_medium": "accounts/fireworks/models/chronos-hermes-13b-v2",
		"model_small":  "accounts/fireworks/models/chronos-hermes-13b-v2",
	}
	reconcileRuntimeModels(policy, state, eligible, nil)
	for _, tier := range runtimeModelTiers {
		key := "model_" + tier
		if state[key] != "accounts/fireworks/models/kimi-k2p6" || stateObject(state, "model_selection_sources")[key] != "automatic" {
			t.Fatalf("%s not repaired: %v", key, state)
		}
	}
}
