package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installTestCodexCatalog(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	oldBaseURL := codexModelCatalogBaseURL
	oldClient := codexModelCatalogClient
	oldFreshTTL := codexModelCatalogFreshTTL
	oldStaleTTL := codexModelCatalogStaleTTL
	codexModelCatalogBaseURL = server.URL
	codexModelCatalogClient = server.Client()
	codexModelCatalogFreshTTL = 15 * time.Minute
	codexModelCatalogStaleTTL = 24 * time.Hour
	globalCodexCatalogCache = &codexCatalogCacheStore{entries: map[string]codexCatalogCacheEntry{}}
	t.Cleanup(func() {
		server.Close()
		codexModelCatalogBaseURL = oldBaseURL
		codexModelCatalogClient = oldClient
		codexModelCatalogFreshTTL = oldFreshTTL
		codexModelCatalogStaleTTL = oldStaleTTL
		globalCodexCatalogCache = &codexCatalogCacheStore{entries: map[string]codexCatalogCacheEntry{}}
	})
}

func writeTestCodexCatalog(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	parallel := true
	summaries := true
	_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
		{
			"slug": "gpt-5.6-luna", "display_name": "GPT-5.6 Luna", "visibility": "list", "priority": 30,
			"supported_in_api": true, "context_window": 272000, "max_context_window": 272000,
			"effective_context_window_percent": 90, "default_reasoning_level": "low",
			"supported_reasoning_levels": []map[string]string{{"effort": "low"}, {"effort": "medium"}},
			"input_modalities":           []string{"text", "image"}, "supports_parallel_tool_calls": parallel,
		},
		{
			"slug": "gpt-5.6-sol", "display_name": "GPT-5.6 Sol", "visibility": "list", "priority": 10,
			"context_window": 400000, "effective_context_window_percent": 95,
			"supported_reasoning_levels":   []map[string]string{{"effort": "high"}, {"effort": "xhigh"}},
			"supports_reasoning_summaries": summaries,
		},
		{
			"slug": "gpt-5.6-terra", "display_name": "GPT-5.6 Terra", "visibility": "list", "priority": 20,
			"context_window": 400000, "effective_context_window_percent": 95,
		},
		{"slug": "internal-model", "visibility": "hidden", "priority": 1},
	}})
}

func TestFetchCodexModelCatalogFiltersAndCachesPerAccount(t *testing.T) {
	var calls atomic.Int32
	installTestCodexCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.URL.Query().Get("client_version"); got != codexModelCatalogClientVersion {
			t.Errorf("client_version = %q, want %q", got, codexModelCatalogClientVersion)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-a" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-a" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		w.Header().Set("ETag", `"catalog-v1"`)
		writeTestCodexCatalog(t, w)
	}))

	models, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-a", false)
	if err != nil {
		t.Fatalf("fetchCodexModelCatalog: %v", err)
	}
	if len(models) != 3 || models[0].ID != "gpt-5.6-sol" || models[1].ID != "gpt-5.6-terra" || models[2].ID != "gpt-5.6-luna" {
		t.Fatalf("models = %#v", models)
	}
	if models[2].SupportedAPI == nil || !*models[2].SupportedAPI || models[2].Capabilities.ContextWindow != 272000 {
		t.Fatalf("Luna capabilities = %#v", models[2])
	}
	if _, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-a", false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("catalog calls = %d, want 1 cached call", calls.Load())
	}
}

func TestFetchCodexModelCatalogRevalidatesETagAndIsolatesAccounts(t *testing.T) {
	var calls atomic.Int32
	installTestCodexCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"catalog-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"catalog-v1"`)
		writeTestCodexCatalog(t, w)
	}))

	if _, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-b", false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("catalog calls = %d, want forced revalidation plus isolated account", calls.Load())
	}
}

func TestFetchCodexModelCatalogCoalescesConcurrentAccountRequests(t *testing.T) {
	var calls atomic.Int32
	installTestCodexCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		writeTestCodexCatalog(t, w)
	}))

	const callers = 12
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, err := fetchCodexModelCatalog(context.Background(), "token-a", "account-a", false)
			if err == nil && len(models) != 3 {
				err = &unexpectedModelCountError{got: len(models), want: 3}
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("catalog calls = %d, want one coalesced request", calls.Load())
	}
}

type unexpectedModelCountError struct {
	got  int
	want int
}

func (e *unexpectedModelCountError) Error() string {
	return "model count mismatch: got " + jsonNumber(int64(e.got)) + ", want " + jsonNumber(int64(e.want))
}

func TestApplyCodexCatalogToStatePreservesAvailableSelections(t *testing.T) {
	models := []ModelInfo{
		{ID: "gpt-5.6-sol", Capabilities: ProviderModelCapabilities{ContextWindow: 400000}},
		{ID: "gpt-5.6-terra", Capabilities: ProviderModelCapabilities{ContextWindow: 400000}},
		{ID: "gpt-5.6-luna", Capabilities: ProviderModelCapabilities{ContextWindow: 272000}},
	}
	state := map[string]any{"model_large": "gpt-5.6-luna", "model_medium": "gpt-5.6-sol", "model_small": "removed-model"}
	applyCodexCatalogToState(state, models)
	if state["model_large"] != "gpt-5.6-luna" || state["model_medium"] != "gpt-5.6-sol" || state["model_small"] != "gpt-5.6-terra" {
		t.Fatalf("selected models = %#v", state)
	}
	caps, ok := state["model_capabilities"].(map[string]ProviderModelCapabilities)
	if !ok || len(caps) != 3 || caps["gpt-5.6-luna"].ContextWindow != 272000 {
		t.Fatalf("model capabilities = %#v", state["model_capabilities"])
	}
}

func TestApplyCodexCatalogToStateKeepsFallbackWhenTerraUnavailable(t *testing.T) {
	models := []ModelInfo{
		{ID: "gpt-5.5", Capabilities: ProviderModelCapabilities{ContextWindow: 400000}},
		{ID: "gpt-5.4-mini", Capabilities: ProviderModelCapabilities{ContextWindow: 200000}},
	}
	state := map[string]any{}
	applyCodexCatalogToState(state, models)
	if state["model_large"] != "gpt-5.5" || state["model_medium"] != "gpt-5.5" || state["model_small"] != "gpt-5.4-mini" {
		t.Fatalf("selected models = %#v", state)
	}
}

func TestMergeOpenAICodexProviderStatePreservesSettingsAndRotatesCredentials(t *testing.T) {
	previous := map[string]any{
		"credentials": map[string]any{"access_token": "old", "refresh_token": "old-refresh"},
		"account":     map[string]any{"chatgpt_account_id": "account-a", "email": "old@example.test"},
		"model_large": "gpt-5.6-sol", "model_medium": "gpt-5.6-terra", "custom_setting": "keep",
	}
	refreshed := map[string]any{
		"credentials": map[string]any{"access_token": "new", "refresh_token": ""},
		"account":     map[string]any{},
		"model_large": "fallback-should-not-win", "runtime": map[string]any{"base_url": "new"},
	}
	merged := mergeOpenAICodexProviderState(previous, refreshed)
	if stringFromNested(merged, "credentials", "access_token") != "new" ||
		stringFromNested(merged, "credentials", "refresh_token") != "old-refresh" ||
		codexAccountIDFromState(merged) != "account-a" || merged["model_large"] != "gpt-5.6-sol" || merged["custom_setting"] != "keep" {
		t.Fatalf("merged state = %#v", merged)
	}
}

func TestGetProviderPoolPreservesConfiguredCodexModels(t *testing.T) {
	installTestCodexCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestCodexCatalog(t, w)
	}))
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	state := map[string]any{
		"auth":        map[string]any{"provider": openAICodexAuthProvider},
		"credentials": map[string]any{"access_token": "access-secret"},
		"account":     map[string]any{"chatgpt_account_id": "account-a"},
		"model_large": "gpt-5.6-sol", "model_medium": "gpt-5.6-terra", "model_small": "gpt-5.6-terra",
		"model_capabilities": map[string]any{
			"gpt-5.6-sol":   map[string]any{"context_window": 372000},
			"gpt-5.6-terra": map[string]any{"context_window": 372000},
		},
	}
	raw, _ := json.Marshal(state)
	encrypted, _ := Encrypt(s.secret, string(raw))
	if _, err := s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", encrypted); err != nil {
		t.Fatal(err)
	}

	pool := s.GetProviderPool(1)
	if len(pool) != 1 {
		t.Fatalf("pool = %#v", pool)
	}
	if pool[0].ModelLarge != "gpt-5.6-sol" || pool[0].ModelMedium != "gpt-5.6-terra" || pool[0].ModelSmall != "gpt-5.6-terra" {
		t.Fatalf("models = %#v", pool[0])
	}
	if pool[0].ModelCapabilities["gpt-5.6-sol"].ContextWindow != 372000 || pool[0].ModelCapabilities["gpt-5.6-terra"].ContextWindow != 372000 {
		t.Fatalf("model capabilities = %#v", pool[0].ModelCapabilities)
	}
}

func TestOpenAICodexIntegrationPassesExplicitCatalogModel(t *testing.T) {
	oldTransport := http.DefaultTransport
	oldCatalogClient := codexModelCatalogClient
	oldBaseURL := codexModelCatalogBaseURL
	globalCodexCatalogCache = &codexCatalogCacheStore{entries: map[string]codexCatalogCacheEntry{}}
	var responseModel string
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/models" {
			var body bytes.Buffer
			recorder := httptest.NewRecorder()
			writeTestCodexCatalog(t, recorder)
			body.Write(recorder.Body.Bytes())
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(&body), Request: r}, nil
		}
		if r.URL.Path == "/backend-api/codex/responses" {
			if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-a" {
				t.Errorf("ChatGPT-Account-ID = %q", got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode response request: %v", err)
			}
			responseModel, _ = body["model"].(string)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"output_text":"ok"}`)), Request: r}, nil
		}
		return nil, &unexpectedRequestError{method: r.Method, url: r.URL.String()}
	})
	http.DefaultTransport = transport
	codexModelCatalogClient = &http.Client{Transport: transport}
	codexModelCatalogBaseURL = "https://catalog.example.test"
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		codexModelCatalogClient = oldCatalogClient
		codexModelCatalogBaseURL = oldBaseURL
		globalCodexCatalogCache = &codexCatalogCacheStore{entries: map[string]codexCatalogCacheEntry{}}
	})

	result, err := executeOpenAICodexIntegrationTool(nil, &AppToolDef{Name: "responses_create"}, map[string]string{
		"access_token": "access-secret", "account_id": "account-a",
	}, map[string]any{"input": "hello", "model": "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || responseModel != "gpt-5.6-luna" {
		t.Fatalf("result = %#v, response model = %q", result, responseModel)
	}
}

type unexpectedRequestError struct {
	method string
	url    string
}

func (e *unexpectedRequestError) Error() string { return e.method + " " + e.url }

func TestOpenAICodexSmokeRequestSendsAccountHeader(t *testing.T) {
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-a" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"output_text":"ok"}`)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	if _, err := runOpenAICodexSmokeRequest(context.Background(), "token-a", "account-a", map[string]any{"model": "gpt-5.6-terra"}); err != nil {
		t.Fatal(err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func jsonNumber(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
