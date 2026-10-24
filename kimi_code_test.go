package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func kimiCodeFixture(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/kimi-code.json")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := os.ReadFile("../integrations/src/apps/kimi-code.json")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err == nil && string(raw) != string(canonical) {
		t.Fatal("embedded Kimi Code definition differs from integration source")
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func mockKimiCodeCatalog(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	oldURL, oldCache := kimiCodeModelsURL, globalModelCache
	kimiCodeModelsURL = server.URL + "/coding/v1/models"
	globalModelCache = &modelCache{entries: map[string]modelCacheEntry{}}
	t.Cleanup(func() { kimiCodeModelsURL, globalModelCache = oldURL, oldCache })
}

func TestKimiCodeAccountCatalogCapabilities(t *testing.T) {
	models, err := parseKimiCodeModels([]byte(`{"data":[
		{"id":"k3.1-pro","display_name":"K3.1 Pro","context_length":262144,"supports_reasoning":true,"supports_image_in":true,"supports_video_in":true},
		{"id":"k3-256k","context_length":262144,"supports_reasoning":true,"supports_image_in":true},
		{"id":"kimi-for-coding-highspeed","context_length":262144,"supports_reasoning":true},
		{"id":"unknown","context_length":0},
		{"id":"k3.1-pro","context_length":1048576}, {"id":""}
	]}`))
	if err != nil || len(models) != 4 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	pro := models[0]
	if pro.Name != "K3.1 Pro" || pro.ContextSize != 262144 || pro.Capabilities.ContextWindow != 262144 {
		t.Fatalf("account context/name ignored: %+v", pro)
	}
	if !reflect.DeepEqual(pro.Capabilities.InputModalities, []string{"text", "image", "video"}) || pro.Capabilities.DefaultReasoningLevel != "max" {
		t.Fatalf("incorrect Pro capabilities: %+v", pro.Capabilities)
	}
	if got := reasoningEfforts(pro.Capabilities.SupportedReasoningLevels); strings.Join(got, ",") != "low,high,max" {
		t.Fatalf("efforts=%v", got)
	}
	if models[1].Capabilities.DefaultReasoningLevel != "high" || len(models[1].Capabilities.InputModalities) != 2 {
		t.Fatalf("incorrect K3 capabilities: %+v", models[1])
	}
	if len(models[2].Capabilities.SupportedReasoningLevels) != 0 || models[3].ContextSize != 0 {
		t.Fatal("invented capabilities or subscription limits")
	}
	for _, model := range models {
		if model.InputCost != 0 || model.CachedInputCost != 0 || model.OutputCost != 0 {
			t.Fatal("subscription received API token prices")
		}
	}
	policy := kimiCodeFixture(t).Runtime.ModelPolicy
	eligible := policy.filter(models)
	if len(eligible) != 3 {
		t.Fatalf("eligible=%+v", eligible)
	}
	for _, tier := range runtimeModelTiers {
		if policy.selectTier(eligible, tier) != "k3-256k" {
			t.Fatal("policy invented an unavailable default")
		}
	}
	for _, raw := range []string{`{`, `{}`, `null`, `{"data":null}`, `{"data":{}}`} {
		if _, err := parseKimiCodeModels([]byte(raw)); err == nil {
			t.Errorf("malformed catalog accepted: %s", raw)
		}
	}
	if empty, err := parseKimiCodeModels([]byte(`{"data":[]}`)); err != nil || len(empty) != 0 {
		t.Fatalf("empty account catalog=%v err=%v", empty, err)
	}
}

func TestKimiCodeDiscoveryAuthenticationCacheAndRefresh(t *testing.T) {
	calls := 0
	mockKimiCodeCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/coding/v1/models" || r.Header.Get("User-Agent") != "Apteva" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
		}
		context := 262144
		if r.Header.Get("Authorization") == "Bearer membership-key-two" {
			context = 1048576
		} else if r.Header.Get("Authorization") != "Bearer membership-key-one" {
			t.Error("missing membership credential")
		}
		fmt.Fprintf(w, `{"data":[{"id":"kimi-for-coding","context_length":%d}]}`, context)
	})
	for _, tc := range []struct {
		key                    string
		refresh                bool
		wantContext, wantCalls int
	}{
		{"membership-key-one", false, 262144, 1},
		{"membership-key-one", false, 262144, 1},
		{"membership-key-two", false, 1048576, 2},
		{"membership-key-one", true, 262144, 3},
	} {
		models, err := FetchModels("kimi-code", tc.key, tc.refresh)
		if err != nil || len(models) != 1 || models[0].ContextSize != tc.wantContext || calls != tc.wantCalls {
			t.Fatalf("discovery/cache mismatch: models=%v calls=%d err=%v", models, calls, err)
		}
	}
}

func TestKimiCodeDiscoveryFailureIsSafeAndNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		status int
		hint   string
	}{
		{401, "membership key rejected"}, {403, "plan and quota"}, {429, "rate or quota"}, {500, "model discovery failed"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			mockKimiCodeCatalog(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"error":"membership-secret-echo"}`)
			})
			for range 2 {
				models, err := FetchModels("kimi-code", "membership-secret-echo")
				if len(models) != 0 || err == nil || !strings.Contains(err.Error(), tc.hint) || !strings.Contains(err.Error(), "retry after 17 seconds") || strings.Contains(err.Error(), "membership-secret-echo") {
					t.Fatalf("unsafe/fallback response: models=%v err=%v", models, err)
				}
			}
			if calls != 2 {
				t.Fatalf("failed discovery cached or retried: %d calls", calls)
			}
		})
	}
	if _, err := fetchKimiCodeModels(""); err == nil {
		t.Fatal("empty membership key accepted")
	}
}

func TestKimiCodeDiscoveryRefusesRedirectAndOversizeCatalog(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		mockKimiCodeCatalog(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/coding/v1/models" {
				t.Error("redirect followed with membership credentials")
			}
			http.Redirect(w, r, "/other", http.StatusFound)
		})
		if _, err := fetchKimiCodeModels("test-key"); err == nil {
			t.Fatal("redirect accepted")
		}
	})
	t.Run("oversize", func(t *testing.T) {
		mockKimiCodeCatalog(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat(" ", (1<<20)+1)) })
		if _, err := fetchKimiCodeModels("test-key"); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("unbounded catalog: %v", err)
		}
	})
}

func TestKimiCodeConnectionDiscoveryAndConfigWithoutCoreActivation(t *testing.T) {
	s := runtimeTestServer(t)
	app := kimiCodeFixture(t)
	s.catalog.Register(app)
	conn := addConnection(t, s, app.Slug, "Coding membership", "", map[string]string{"api_key": "test-key"})
	mockKimiCodeCatalog(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"kimi-for-coding","context_length":262144,"supports_reasoning":true}]}`)
	})
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/connections/%d/models", conn.ID), nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleConnectionModels(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"context_size":262144`) {
		t.Fatalf("discovery %d: %s", rec.Code, rec.Body.String())
	}
	patch := map[string]any{"model_large": "kimi-for-coding", "model_medium": "kimi-for-coding", "model_small": "kimi-for-coding"}
	rec = patchJSON(t, s, s.handleConnectionRuntimeConfig, fmt.Sprintf("/connections/%d/runtime-config", conn.ID), patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("config %d: %s", rec.Code, rec.Body.String())
	}
	bad := patchJSON(t, s, s.handleConnectionRuntimeConfig, fmt.Sprintf("/connections/%d/runtime-config", conn.ID), map[string]any{"model_large": "k3.1-pro"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unavailable model accepted: %d %s", bad.Code, bad.Body.String())
	}
	if app.Runtime.Env["KIMI_CODE_API_KEY"] != "{{credentials.api_key}}" || isLLMKey("kimi-code") {
		t.Fatal("Core contract changed before adapter support")
	}
	if len(s.GetProviderPool(1)) != 0 {
		t.Fatal("unsupported provider reached agent pool")
	}
	env, err := s.runtimeEnvFromConnections(1, nil)
	if err != nil || len(env) != 0 {
		t.Fatalf("membership credential exported before Core support: %v %v", env, err)
	}
	if len(s.runtimeCatalogForRole("llm")) != 0 || s.catalog.Get("kimi-code") == nil {
		t.Fatal("provider selection must be gated while ordinary integration remains accessible")
	}
	req = httptest.NewRequest(http.MethodGet, "/connections/runtime", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleListRuntimeConnections(rec, req)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("unsupported runtime offered in provider settings: %s", rec.Body.String())
	}
	config, err := s.store.GetConnectionRuntimeConfig(1, conn.ID)
	if err != nil || config["model_large"] != "kimi-for-coding" {
		t.Fatalf("staged config was lost: %v %v", config, err)
	}
}

func TestKimiCodeIntegrationHealthAndToolForwarding(t *testing.T) {
	app := kimiCodeFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer member-test-key" || r.Header.Get("User-Agent") != "Apteva" {
			t.Error("missing membership auth or honest client identity")
		}
		switch r.URL.Path {
		case "/models":
			if r.Method != http.MethodGet || r.ContentLength > 0 {
				t.Error("health check was not read-only")
			}
			fmt.Fprint(w, `{"data":[{"id":"kimi-for-coding"}]}`)
		case "/chat/completions":
			var body struct {
				Model    string `json:"model"`
				Messages []struct {
					Reasoning string `json:"reasoning_content"`
					CallID    string `json:"tool_call_id"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "kimi-for-coding" || len(body.Messages) != 2 || body.Messages[0].Reasoning != "check build" || body.Messages[1].CallID != "call-1" {
				t.Errorf("tool continuation was changed: %+v err=%v", body, err)
			}
			w.Header().Set("Retry-After", "23")
			w.Header().Set("Set-Cookie", "private=value")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"type":"insufficient_quota"}}`)
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	app.BaseURL = upstream.URL
	s := runtimeTestServer(t)
	credentials := map[string]string{"api_key": "member-test-key"}
	raw, _ := json.Marshal(credentials)
	encrypted, _ := Encrypt(s.secret, string(raw))
	if health := s.runHealthCheck(app, encrypted); !health.OK || health.Skipped {
		t.Fatalf("membership health=%+v", health)
	}
	result, err := executeIntegrationToolWithRefresh(app, &app.Tools[0], credentials, map[string]any{
		"model": "kimi-for-coding",
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "check build", "tool_calls": []any{}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "passed"},
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.Status != http.StatusForbidden || result.Headers["Retry-After"] != "23" || result.Headers["Set-Cookie"] != "" || calls != 2 {
		t.Fatalf("quota result lost or retried: %+v calls=%d", result, calls)
	}
}

func TestRuntimeSupportedUsesSharedCoreAllowList(t *testing.T) {
	for _, tc := range []struct {
		runtime *AppRuntimeConfig
		want    bool
	}{
		{nil, false},
		{&AppRuntimeConfig{Role: "llm", ProviderKey: "future-llm"}, false},
		{&AppRuntimeConfig{Role: "LLM", ProviderKey: "kimi-code"}, false},
		{&AppRuntimeConfig{Role: "llm", ProviderKey: "openai"}, true},
		{&AppRuntimeConfig{Role: "embeddings", ProviderKey: "voyage"}, true},
		{&AppRuntimeConfig{Role: "tts", ProviderKey: "elevenlabs"}, true},
	} {
		if got := runtimeSupported(tc.runtime); got != tc.want {
			t.Errorf("runtimeSupported(%+v)=%v want=%v", tc.runtime, got, tc.want)
		}
	}
}
