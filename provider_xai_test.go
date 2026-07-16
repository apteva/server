package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func installTestXAIModelCatalog(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"id":                                   "grok-4.5",
					"aliases":                              []string{"grok-4.5-latest"},
					"context_length":                       500_000,
					"input_modalities":                     []string{"text", "image"},
					"output_modalities":                    []string{"text"},
					"prompt_text_token_price":              20_000,
					"cached_prompt_text_token_price":       2_000,
					"completion_text_token_price":          60_000,
					"long_context_threshold":               200_000,
					"prompt_text_token_price_long_context": 40_000,
				},
				{
					"id":                             "grok-4.3",
					"aliases":                        []string{"grok-latest"},
					"context_length":                 1_000_000,
					"input_modalities":               []string{"text", "image"},
					"output_modalities":              []string{"text"},
					"prompt_text_token_price":        12_500,
					"cached_prompt_text_token_price": 2_000,
					"completion_text_token_price":    25_000,
				},
				{
					"id":                "grok-imagine-image",
					"context_length":    1_024,
					"input_modalities":  []string{"text"},
					"output_modalities": []string{"image"},
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	previous := xAILanguageModelsURL
	xAILanguageModelsURL = server.URL
	t.Cleanup(func() { xAILanguageModelsURL = previous })
}

func TestXAIProviderTypeEnvironmentAndPool(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()

	providerType, err := s.store.GetProviderType(17)
	if err != nil {
		t.Fatalf("GetProviderType: %v", err)
	}
	if providerType.Name != "xAI" || providerType.Type != "llm" || providerType.AuthProvider != "xai" || providerType.RuntimeStatus != "available" {
		t.Fatalf("provider type = %#v", providerType)
	}
	if len(providerType.Fields) != 1 || providerType.Fields[0] != "XAI_API_KEY" {
		t.Fatalf("provider fields = %#v", providerType.Fields)
	}
	for _, capability := range []string{"llm", "streaming", "native_tools", "reasoning", "vision"} {
		if !containsString(providerType.Capabilities, capability) {
			t.Fatalf("provider capabilities = %#v, missing %q", providerType.Capabilities, capability)
		}
	}

	state := map[string]any{
		"XAI_API_KEY": "xai-secret",
		"model_large": "grok-4.5", "model_medium": "grok-4.3", "model_small": "grok-4.3",
		"model_capabilities": map[string]any{
			"grok-4.5": map[string]any{"context_window": 500_000, "supported_reasoning_levels": []map[string]any{{"effort": "low"}, {"effort": "high"}}},
			"grok-4.3": map[string]any{"context_window": 1_000_000},
		},
	}
	raw, _ := json.Marshal(state)
	encrypted, _ := Encrypt(s.secret, string(raw))
	if _, err := s.store.CreateProvider(1, 17, "llm", "xAI", encrypted); err != nil {
		t.Fatal(err)
	}

	env, err := s.store.GetAllProviderEnvVars(1, s.secret)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	if env["XAI_API_KEY"] != "xai-secret" {
		t.Fatalf("XAI_API_KEY = %q", env["XAI_API_KEY"])
	}
	pool := s.GetProviderPool(1)
	if len(pool) != 1 || pool[0].Type != "xai" {
		t.Fatalf("provider pool = %#v", pool)
	}
	if pool[0].ModelLarge != "grok-4.5" || pool[0].ModelMedium != "grok-4.3" || pool[0].ModelSmall != "grok-4.3" {
		t.Fatalf("pool models = %#v", pool[0])
	}
	if pool[0].ModelCapabilities["grok-4.5"].ContextWindow != 500_000 {
		t.Fatalf("pool capabilities = %#v", pool[0].ModelCapabilities)
	}
}

func TestFetchXAIModelsFiltersGenerationModelsAndParsesCapabilitiesAndPricing(t *testing.T) {
	installTestXAIModelCatalog(t)
	models, err := fetchXAIModels("catalog-key")
	if err != nil {
		t.Fatalf("fetchXAIModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "grok-4.3" || models[1].ID != "grok-4.5" {
		t.Fatalf("models = %#v", models)
	}
	grok43 := models[0]
	if grok43.ContextSize != 1_000_000 || grok43.InputCost != 1.25 || grok43.CachedInputCost != 0.2 || grok43.OutputCost != 2.5 {
		t.Fatalf("grok-4.3 catalog data = %#v", grok43)
	}
	if !containsStringFold(grok43.Capabilities.InputModalities, "image") || grok43.Capabilities.SupportsParallelToolCalls == nil || !*grok43.Capabilities.SupportsParallelToolCalls {
		t.Fatalf("grok-4.3 capabilities = %#v", grok43.Capabilities)
	}
	if got := reasoningEfforts(grok43.Capabilities.SupportedReasoningLevels); strings.Join(got, ",") != "none,low,medium,high" {
		t.Fatalf("grok-4.3 reasoning = %#v", got)
	}
	grok45 := models[1]
	if grok45.Capabilities.DefaultReasoningLevel != "high" || grok45.Capabilities.SupportsReasoningSummaries == nil || !*grok45.Capabilities.SupportsReasoningSummaries {
		t.Fatalf("grok-4.5 capabilities = %#v", grok45.Capabilities)
	}
	if grok45.LongContextThreshold != 200_000 || grok45.LongContextInputCost != 4 {
		t.Fatalf("grok-4.5 long-context pricing = %#v", grok45)
	}
	if got := reasoningEfforts(grok45.Capabilities.SupportedReasoningLevels); strings.Join(got, ",") != "low,medium,high" {
		t.Fatalf("grok-4.5 reasoning = %#v", got)
	}
}

func TestSaveXAIModelsPersistsSelectedCapabilities(t *testing.T) {
	installTestXAIModelCatalog(t)
	globalModelCache = &modelCache{entries: make(map[string]modelCacheEntry)}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	state := map[string]any{"XAI_API_KEY": "save-key"}
	raw, _ := json.Marshal(state)
	encrypted, _ := Encrypt(s.secret, string(raw))
	provider, err := s.store.CreateProvider(1, 17, "llm", "xAI", encrypted)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"large": "grok-4.5", "medium": "grok-4.3", "small": "grok-4.3"})
	req := httptest.NewRequest(http.MethodPut, "/providers/"+jsonNumber(provider.ID)+"/models", bytes.NewReader(body))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleProviderModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	_, savedEncrypted, err := s.store.GetProvider(1, provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _ := Decrypt(s.secret, savedEncrypted)
	var saved map[string]any
	if err := json.Unmarshal([]byte(plaintext), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["model_large"] != "grok-4.5" || saved["model_medium"] != "grok-4.3" || saved["model_small"] != "grok-4.3" || saved["XAI_API_KEY"] != "save-key" {
		t.Fatalf("saved state = %#v", saved)
	}
	caps := stateMap(saved, "model_capabilities")
	if len(caps) != 2 {
		t.Fatalf("saved capabilities = %#v", saved["model_capabilities"])
	}
}

func TestXAIProviderCredentialProbe(t *testing.T) {
	probe, ok := providerProbes["xAI"]
	if !ok {
		t.Fatal("xAI credential probe missing")
	}
	if probe.url != "https://api.x.ai/v1/language-models" || probe.headers["Authorization"] != "Bearer {XAI_API_KEY}" || probe.modelCountPath != "models" {
		t.Fatalf("xAI probe = %#v", probe)
	}
}

func TestXAIModelPricingFallbacks(t *testing.T) {
	for model, want := range map[string][3]float64{
		"grok-4.5":       {2.00, 0.50, 6.00},
		"grok-4.3":       {1.25, 0.20, 2.50},
		"grok-build-0.1": {1.00, 0.20, 2.00},
	} {
		input, cached, output, ok := LookupModelPricing(model)
		if !ok || input != want[0] || cached != want[1] || output != want[2] {
			t.Fatalf("pricing %s = (%v, %v, %v, %v), want %#v", model, input, cached, output, ok, want)
		}
	}
}

func reasoningEfforts(levels []ModelReasoningLevel) []string {
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		out = append(out, level.Effort)
	}
	return out
}
