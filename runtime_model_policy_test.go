package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func geminiPolicyFixture(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := os.ReadFile("integrations-catalog/gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	if app.Runtime.ModelPolicy == nil {
		t.Fatal("Gemini policy missing")
	}
	return &app
}

func policyModels(ids ...string) []ModelInfo {
	out := []ModelInfo{}
	for _, id := range ids {
		out = append(out, ModelInfo{ID: id, Methods: []string{"generateContent"}})
	}
	return out
}

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockPolicyCatalog(t *testing.T, response func(*http.Request) string) {
	t.Helper()
	old := http.DefaultTransport
	oldCache := globalModelCache
	globalModelCache = &modelCache{entries: make(map[string]modelCacheEntry)}
	http.DefaultTransport = policyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "generativelanguage.googleapis.com" {
			return nil, fmt.Errorf("unexpected test request: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response(r)))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old; globalModelCache = oldCache })
}

func TestGeminiRuntimeModelEligibility(t *testing.T) {
	policy := geminiPolicyFixture(t).Runtime.ModelPolicy
	good := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-3.1-pro-preview", "gemini-3.7-flash", "gemini-flash-latest"}
	bad := []string{"antigravity-preview-05-2026", "gemini-2.5-flash-image", "gemini-3-pro-image-preview", "gemini-2.5-flash-preview-tts", "gemini-2.5-flash-native-audio-preview", "gemini-3.1-flash-live-preview", "imagen-4.0-generate-001", "veo-3.0-generate-001", "gemini-embedding-001", "gemini-future-specialized"}
	models := policyModels(append(good, bad...)...)
	models = append(models, ModelInfo{ID: "gemini-9-pro", Methods: []string{"countTokens"}})
	got := policy.filter(models)
	ids := []string{}
	for _, m := range got {
		ids = append(ids, m.ID)
		if !reflect.DeepEqual(m.Purposes, []string{"agent"}) {
			t.Fatal("purpose metadata missing")
		}
	}
	if !reflect.DeepEqual(ids, good) {
		t.Fatalf("eligible=%v want=%v", ids, good)
	}
	if models[0].Purposes != nil {
		t.Fatal("filter modified raw cache")
	}
	var noPolicy *RuntimeModelPolicy
	if len(noPolicy.filter(models)) != len(models) {
		t.Fatal("opt-out behavior changed")
	}
}

func TestGeminiRuntimeTierPreferencesAndRepair(t *testing.T) {
	p := geminiPolicyFixture(t).Runtime.ModelPolicy
	models := p.filter(policyModels("antigravity-preview-05-2026", "gemini-3.9-pro", "gemini-3.10-pro", "gemini-4-pro-preview", "gemini-3.7-flash", "gemini-2.5-flash-lite"))
	state := map[string]any{"model_large": "antigravity-preview-05-2026", "model_medium": "antigravity-preview-05-2026", "model_small": "antigravity-preview-05-2026"}
	reconcileRuntimeModels(p, state, models, nil)
	for tier, want := range map[string]string{"large": "gemini-3.10-pro", "medium": "gemini-3.7-flash", "small": "gemini-2.5-flash-lite"} {
		key := "model_" + tier
		if state[key] != want {
			t.Fatalf("%s=%v want=%s", key, state[key], want)
		}
		if stateObject(state, "model_selection_sources")[key] != "automatic" {
			t.Fatal("automatic origin missing")
		}
		if stateObject(state, "model_selection_previous")[key] != "antigravity-preview-05-2026" {
			t.Fatal("legacy selection not archived")
		}
	}
	if state["model_selection_errors"] != nil {
		t.Fatal(state)
	}
	legacy := map[string]any{"model_large": "gemini-3.7-flash", "model_medium": "gemini-3.7-flash", "model_small": "gemini-3.7-flash"}
	reconcileRuntimeModels(p, legacy, models, nil)
	if legacy["model_large"] != "gemini-3.7-flash" || stateObject(legacy, "model_selection_sources")["model_large"] != "legacy" {
		t.Fatal("valid legacy choice was overwritten")
	}
	// An intentionally smaller valid model is preserved, even for the large tier.
	state["model_large"] = "gemini-3.7-flash"
	stateObject(state, "model_selection_sources")["model_large"] = "explicit"
	reconcileRuntimeModels(p, state, models, nil)
	if state["model_large"] != "gemini-3.7-flash" {
		t.Fatal("explicit pin changed")
	}
	state["model_large"] = "gemini-2.5-flash-image"
	reconcileRuntimeModels(p, state, models, nil)
	if state["model_large"] != "gemini-2.5-flash-image" || state["model_selection_errors"] == nil {
		t.Fatal("invalid explicit pin silently replaced")
	}
	// No available candidates must never produce a made-up default.
	empty := map[string]any{}
	reconcileRuntimeModels(p, empty, nil, nil)
	if empty["model_large"] != nil || empty["model_selection_errors"] == nil {
		t.Fatal(empty)
	}
	// Catalog outage preserves compatible saved models, blocks incompatible ones.
	state["model_large"] = "gemini-3.7-flash"
	reconcileRuntimeModels(p, state, nil, fmt.Errorf("offline"))
	if state["model_selection_errors"] != nil {
		t.Fatal("outage rejected valid saved selections")
	}
	state["model_large"] = "antigravity-preview-05-2026"
	reconcileRuntimeModels(p, state, nil, fmt.Errorf("offline"))
	if state["model_selection_errors"] == nil {
		t.Fatal("outage allowed incompatible fallback")
	}
}

func TestGeminiModelDiscoveryPaginationCacheAndRefresh(t *testing.T) {
	calls := 0
	mockPolicyCatalog(t, func(r *http.Request) string {
		calls++
		if r.URL.Query().Get("key") != "" || r.Header.Get("x-goog-api-key") == "" {
			t.Error("credential should be in header")
		}
		if r.URL.Query().Get("pageToken") == "next" {
			return `{"models":[{"name":"models/gemini-3.7-flash","supportedGenerationMethods":["generateContent"]}]}`
		}
		return `{"models":[{"name":"models/antigravity-preview-05-2026","supportedGenerationMethods":["generateContent"]}],"nextPageToken":"next"}`
	})
	app := geminiPolicyFixture(t)
	models, err := fetchRuntimeModels(app.Runtime, "same-key-A")
	if err != nil || len(models) != 1 || models[0].ID != "gemini-3.7-flash" {
		t.Fatalf("%v %v", models, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	_, _ = fetchRuntimeModels(app.Runtime, "same-key-A")
	if calls != 2 {
		t.Fatal("cache miss")
	}
	_, _ = fetchRuntimeModels(app.Runtime, "same-key-B")
	if calls != 4 {
		t.Fatal("connections with identical key prefixes shared cache")
	}
	_, _ = fetchRuntimeModels(app.Runtime, "same-key-A", true)
	if calls != 6 {
		t.Fatal("refresh ignored")
	}
	// Policy is reapplied after caching; catalog edits take effect immediately.
	app.Runtime.ModelPolicy.AllowedIDPatterns = []string{"^nothing$"}
	models, err = fetchRuntimeModels(app.Runtime, "same-key-A")
	if err != nil || len(models) != 0 || calls != 6 {
		t.Fatal("cached models bypassed updated policy")
	}
}

func TestGeminiModelDiscoveryRejectsMalformedAndRepeatedPages(t *testing.T) {
	for _, response := range []string{`{bad`, `{"nextPageToken":"same"}`} {
		t.Run(response, func(t *testing.T) {
			mockPolicyCatalog(t, func(*http.Request) string { return response })
			if _, err := fetchGoogleModels("test"); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestGeminiRuntimePolicyEndpointsAndExports(t *testing.T) {
	mockPolicyCatalog(t, func(*http.Request) string {
		return `{"models":[
 {"name":"models/antigravity-preview-05-2026","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-2.5-flash-image","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-3.1-pro-preview","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-3.7-flash","supportedGenerationMethods":["generateContent"]},
 {"name":"models/gemini-2.5-flash-lite","supportedGenerationMethods":["generateContent"]}]}`
	})
	s := runtimeTestServer(t)
	app := geminiPolicyFixture(t)
	s.catalog.Register(app)
	conn := addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "test-key"})
	old := `{"model_large":"antigravity-preview-05-2026","model_medium":"antigravity-preview-05-2026","model_small":"antigravity-preview-05-2026"}`
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID, old); err != nil {
		t.Fatal(err)
	}
	pool := s.GetProviderPool(1)
	providers := buildAgentCoreProviderConfigs(pool, `{"default_provider":"google"}`)
	if len(providers) == 0 {
		t.Fatal("missing provider")
	}
	encoded, _ := json.Marshal(providers)
	if strings.Contains(string(encoded), "antigravity") || strings.Contains(string(encoded), "flash-image") {
		t.Fatalf("invalid export: %s", encoded)
	}
	state, _ := s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if state["model_large"] != "gemini-3.1-pro-preview" || state["model_medium"] != "gemini-3.7-flash" || state["model_small"] != "gemini-2.5-flash-lite" {
		t.Fatal(state)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/connections/%d/models", conn.ID), nil)
	req.Header.Set("X-User-ID", "1")
	s.handleConnectionModels(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "antigravity") || strings.Contains(rec.Body.String(), "flash-image") {
		t.Fatalf("picker: %d %s", rec.Code, rec.Body.String())
	}
	path := fmt.Sprintf("/connections/%d/runtime-config", conn.ID)
	for _, patch := range []map[string]any{
		{"model_large": "gemini-2.5-flash-image"}, {"model_large": "gemini-99-pro"}, {"model_large": 42},
		{"model_selection_sources": map[string]any{"model_large": "automatic"}},
		{"model_large": "gemini-3.7-flash", "model_small": "antigravity-preview-05-2026"},
	} {
		rec := patchJSON(t, s, s.handleConnectionRuntimeConfig, path, patch)
		if rec.Code != 400 {
			t.Fatalf("patch accepted: %v => %d %s", patch, rec.Code, rec.Body.String())
		}
	}
	state, _ = s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if state["model_large"] != "gemini-3.1-pro-preview" {
		t.Fatal("failed patch partially persisted")
	}
	rec = patchJSON(t, s, s.handleConnectionRuntimeConfig, path, map[string]any{"model_large": "gemini-3.7-flash"})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	state, _ = s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if stateObject(state, "model_selection_sources")["model_large"] != "explicit" {
		t.Fatal("pin source missing")
	}
	// The stale state captured before a PATCH cannot override a newer pin.
	stale := map[string]any{"model_large": "antigravity-preview-05-2026"}
	_, encrypted, _ := s.store.GetConnection(1, conn.ID)
	rc := runtimeConnection{ID: conn.ID, AppSlug: "gemini", EncryptedCreds: encrypted}
	src, _ := buildRuntimeSources(rc, s.secret)
	s.hydrateRuntimeModels(rc, app, src, stale)
	if stale["model_large"] != "gemini-3.7-flash" {
		t.Fatal("hydration overwrote current pin")
	}
	rec = patchJSON(t, s, s.handleConnectionRuntimeConfig, path, map[string]any{"model_large": nil})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	pool = s.GetProviderPool(1)
	state, _ = s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if state["model_large"] != "gemini-3.1-pro-preview" || stateObject(state, "model_selection_sources")["model_large"] != "automatic" {
		t.Fatal("reset did not restore ranked automatic choice")
	}
	for _, model := range []string{"gemini-2.5-flash-image", "gemini-99-pro"} {
		if _, _, _, err := runtimeProviderPool(pool, "google", model); err == nil {
			t.Fatalf("environment runtime accepted %s", model)
		}

		if err := validateProviderModel(pool, "google", model); err == nil {
			t.Fatalf("agent override accepted %s", model)
		}
		_, _, err := hydrateCoreProviderConfigs(pool, "google", []map[string]any{{"name": "google", "models": map[string]string{"large": model, "medium": model, "small": model}}})
		if err == nil {
			t.Fatalf("raw provider config accepted %s", model)
		}
	}
	// Explicit incompatible selections block export, with a visible settings error.
	state["model_large"] = "gemini-2.5-flash-image"
	stateObject(state, "model_selection_sources")["model_large"] = "explicit"
	raw, _ := json.Marshal(state)
	s.store.UpdateConnectionRuntimeConfig(conn.ID, string(raw))
	pool = s.GetProviderPool(1)
	if configs := buildCoreProviderConfigs(pool, "google"); len(configs) != 0 {
		t.Fatalf("bad explicit provider exported: %v", configs)
	}
	state, _ = s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if state["model_selection_errors"] == nil {
		t.Fatal("no visible settings error")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/connections/runtime", nil)
	req.Header.Set("X-User-ID", "1")
	s.handleListRuntimeConnections(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "model_selection_errors") {
		t.Fatalf("settings error not exposed: %d %s", rec.Code, rec.Body.String())
	}

}

func TestGeminiPolicyBlocksStaleDiskStartupAndAgentEdits(t *testing.T) {
	mockPolicyCatalog(t, func(*http.Request) string {
		return `{"models":[{"name":"models/gemini-3.7-flash","supportedGenerationMethods":["generateContent"]}]}`
	})
	s := runtimeTestServer(t)
	app := geminiPolicyFixture(t)
	s.catalog.Register(app)
	addConnection(t, s, "gemini", "Gemini", "", map[string]string{"api_key": "test-key"})
	agent, err := s.store.CreateAgent(1, "Gemini policy", "directive", "cautious", `{"default_provider":"google"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	badPool := []ProviderInfo{{Type: "google", ModelPolicy: app.Runtime.ModelPolicy, ModelLarge: "antigravity-preview-05-2026", ModelMedium: "antigravity-preview-05-2026", ModelSmall: "antigravity-preview-05-2026"}}
	if err := s.agents.Start(agent, nil, "5280", badPool, ""); err == nil || !strings.Contains(err.Error(), "no compatible agent models") {
		t.Fatalf("startup did not block incompatible defaults: %v", err)
	}
	for _, body := range []map[string]any{
		{"model_override": "gemini-2.5-flash-image"},
		{"providers": []map[string]any{{"name": "google", "default": true}}, "model_override": "gemini-99-pro"},
		{"config": `{"default_provider":"google","model_override":{"provider":"google","model":"gemini-2.5-flash-image"}}`},
	} {
		req := authedRequest(t, http.MethodPut, fmt.Sprintf("/instances/%d/config", agent.ID), "", body)
		rec := httptest.NewRecorder()
		s.handleUpdateConfig(rec, req)
		if rec.Code != 400 {
			t.Fatalf("invalid agent model accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestRuntimeModelPolicyIsProviderIndependent(t *testing.T) {
	p := &RuntimeModelPolicy{Purpose: "agent", RequiredMethods: []string{"respond", "tools"}, AllowedIDPatterns: []string{`^acme-reasoner-[0-9]+$`}, TierPreferences: map[string][]string{"large": {`^acme-reasoner-2$`}}}
	models := p.filter([]ModelInfo{
		{ID: "acme-image-1", Methods: []string{"respond", "tools"}},
		{ID: "acme-reasoner-3", Methods: []string{"respond"}},
		{ID: "acme-reasoner-1", Methods: []string{"respond", "tools"}},
		{ID: "acme-reasoner-2", Methods: []string{"respond", "tools"}},
	})
	if len(models) != 2 || p.selectTier(models, "large") != "acme-reasoner-2" {
		t.Fatalf("generic contract failed: %v", models)
	}
}
