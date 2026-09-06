package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The OpenAI path used to hardcode api.openai.com everywhere, so an
// operator pointing at an OpenAI-compatible gateway still got probed,
// model-listed, and inferenced against the real API — a guaranteed 401
// that blocked the save entirely. These tests pin the base URL override
// through the health check, the model fetch, and the environment edge
// allowlist.

// openAITestApp mirrors the shape of integrations-catalog/openai-api.json
// closely enough to exercise runHealthCheck: bearer auth, a list_models
// tool, a health_check that reuses it, and an LLM runtime block.
func openAITestApp() *AppTemplate {
	return &AppTemplate{
		Slug:    "openai-api",
		Name:    "OpenAI",
		BaseURL: "https://api.openai.com/v1",
		Auth: AppAuthConfig{
			Types:   []string{"bearer"},
			Headers: map[string]string{"Authorization": "Bearer {{token}}"},
		},
		Tools: []AppToolDef{{
			Name:        "list_models",
			Description: "List models",
			Method:      "GET",
			Path:        "/models",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		HealthCheck: &AppHealthCheck{Tool: "list_models"},
		Runtime: &AppRuntimeConfig{
			Role:        "llm",
			ProviderKey: "openai",
			Env: map[string]string{
				"OPENAI_API_KEY":  "{{credentials.token}}",
				"OPENAI_BASE_URL": "{{config.base_url}}",
			},
		},
	}
}

func encryptCreds(t *testing.T, s *Server, creds string) string {
	t.Helper()
	encrypted, err := Encrypt(s.secret, creds)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

// A gateway key is only valid at the gateway. The probe must fire at the
// configured base URL, with the connection's bearer token, not at the
// catalog's api.openai.com default.
func TestRunHealthCheckUsesBaseURLOverride(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gw-model-a"},{"id":"gw-model-b"}]}`))
	}))
	defer srv.Close()

	s := newTestServer(t)
	enc := encryptCreds(t, s, `{"token":"sk-gateway"}`)

	res := s.runHealthCheck(openAITestApp(), enc, srv.URL+"/v1")
	if !res.OK {
		t.Fatalf("health check failed against configured base URL: %+v", res)
	}
	if gotPath != "/v1/models" {
		t.Errorf("probe requested %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-gateway" {
		t.Errorf("probe sent auth %q, want the connection's bearer token", gotAuth)
	}
}

// No override must leave the catalog base URL untouched — stock OpenAI
// connections keep probing the real API.
func TestRunHealthCheckWithoutOverrideKeepsCatalogBaseURL(t *testing.T) {
	s := newTestServer(t)
	enc := encryptCreds(t, s, `{"token":"sk-real"}`)

	app := openAITestApp()
	// Point the catalog base URL at a local stub standing in for the
	// real API, so the assertion is on routing rather than on a live
	// api.openai.com round-trip.
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	app.BaseURL = srv.URL + "/v1"

	if res := s.runHealthCheck(app, enc, ""); !res.OK {
		t.Fatalf("health check failed: %+v", res)
	}
	if !hit {
		t.Error("probe never reached the catalog base URL")
	}
}

// FetchModels must honour the base URL for provider key "openai" and
// keep gateway catalogs cache-separated from the real API's.
func TestFetchModelsHonoursOpenAIBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gw-model-a"},{"id":"gw-model-b"}]}`))
	}))
	defer srv.Close()

	models, err := FetchModels("openai", "sk-fetch-models-gw", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("model fetch requested %q, want /v1/models", gotPath)
	}
	if len(models) != 2 || models[0].ID != "gw-model-a" {
		t.Errorf("models = %+v, want the gateway's two models", models)
	}

	// Same key, different endpoint → must not serve the gateway's
	// cached list.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"other-model"}]}`))
	}))
	defer srv2.Close()

	models2, err := FetchModels("openai", "sk-fetch-models-gw", srv2.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchModels (second endpoint): %v", err)
	}
	if len(models2) != 1 || models2[0].ID != "other-model" {
		t.Errorf("second endpoint served %+v — cache key ignores the base URL", models2)
	}
}

// Blank base URL must resolve to the real API.
func TestOpenAIBaseURLOrDefaults(t *testing.T) {
	if got := openAIBaseURLOr(""); got != defaultOpenAIBaseURL {
		t.Fatalf("openAIBaseURLOr(\"\") = %q, want %q", got, defaultOpenAIBaseURL)
	}
	if got := openAIBaseURLOr("  "); got != defaultOpenAIBaseURL {
		t.Fatalf("openAIBaseURLOr(blank) = %q, want %q", got, defaultOpenAIBaseURL)
	}
	if got := openAIBaseURLOr("https://gw.internal/v1/"); got != "https://gw.internal/v1" {
		t.Fatalf("openAIBaseURLOr trailing slash = %q", got)
	}
}

// runtimeBaseURLFor reads the same runtime_config.base_url key the
// provider migration writes and {{config.base_url}} renders.
func TestRuntimeBaseURLFor(t *testing.T) {
	src := runtimeTemplateSources{config: map[string]any{"base_url": " https://gw.internal/v1 "}}
	if got := runtimeBaseURLFor(src); got != "https://gw.internal/v1" {
		t.Errorf("runtimeBaseURLFor = %q, want trimmed configured value", got)
	}
	if got := runtimeBaseURLFor(runtimeTemplateSources{config: map[string]any{}}); got != "" {
		t.Errorf("runtimeBaseURLFor with no base_url = %q, want empty", got)
	}
}

// The environment edge allowlist must admit the configured gateway host
// dynamically — and only that host, not a widened static suffix list.
func TestLLMAllowSuffixesIncludesConfiguredGateway(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://gw.internal:8443/v1")
	if !hostMatchesSuffix("gw.internal", llmAllowSuffixes()) {
		t.Error("configured OPENAI_BASE_URL host is not allowlisted by the environment edge")
	}
	if !hostMatchesSuffix("api.openai.com", llmAllowSuffixes()) {
		t.Error("default LLM hosts must stay allowlisted")
	}

	t.Setenv("OPENAI_BASE_URL", "")
	if got, want := len(llmAllowSuffixes()), len(defaultAllowSuffixes); got != want {
		t.Errorf("blank base URL changed the allowlist: got %d entries, want %d", got, want)
	}
}

func TestLLMGatewayHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://gw.internal:8443/v1", "gw.internal"},
		{"http://fakegw:8899/v1", "fakegw"},
		{" https://gw.internal/v1 ", "gw.internal"},
		{"", ""},
		{"   ", ""},
		{"://not-a-url", ""},
	} {
		if got := llmGatewayHost(tc.in); got != tc.want {
			t.Errorf("llmGatewayHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A gateway configured per-connection (runtime_config.base_url) is only
// known at agent attach time, after the edge has started. AllowHost must
// admit exactly that host — additively, idempotently, and while the edge
// is serving.
func TestEdgeAllowHostAdmitsConnectionGateway(t *testing.T) {
	edge, err := startEnvironmentEdge(SandboxPolicy{}, EdgeBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer edge.Stop()

	if edge.allowedHost("gw.internal") {
		t.Fatal("gateway host allowed before AllowHost — allowlist is too broad")
	}
	if !edge.allowedHost("api.openai.com") {
		t.Fatal("default LLM hosts must be allowlisted at edge start")
	}

	edge.AllowHost("gw.internal")
	edge.AllowHost("gw.internal") // idempotent
	if !edge.allowedHost("gw.internal") {
		t.Error("AllowHost did not admit the connection's gateway host")
	}
	if edge.allowedHost("evil.example") {
		t.Error("unrelated hosts must stay blocked")
	}

	edge.policyMu.RLock()
	count := 0
	for _, hostname := range edge.policy.AllowHostSuffixes {
		if hostname == "gw.internal" {
			count++
		}
	}
	edge.policyMu.RUnlock()
	if count != 1 {
		t.Errorf("AllowHost added %d entries for one host, want 1", count)
	}
}
