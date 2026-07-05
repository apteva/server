package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSecret() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestProviderCRUD(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	// Create user
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create provider
	data := map[string]string{"FIREWORKS_API_KEY": "sk-test123", "model": "llama3"}
	dataJSON, _ := json.Marshal(data)
	encrypted, err := Encrypt(s.secret, string(dataJSON))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	provider, err := s.store.CreateProvider(1, 0, "llm", "Fireworks", encrypted)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if provider.Name != "Fireworks" {
		t.Errorf("expected Fireworks, got %s", provider.Name)
	}

	// List providers
	list, err := s.store.ListProviders(1)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(list))
	}

	// Get provider
	p, encData, err := s.store.GetProvider(1, provider.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if p.Name != "Fireworks" {
		t.Errorf("expected Fireworks, got %s", p.Name)
	}

	// Decrypt and verify
	plain, err := Decrypt(s.secret, encData)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	var got map[string]string
	json.Unmarshal([]byte(plain), &got)
	if got["FIREWORKS_API_KEY"] != "sk-test123" {
		t.Errorf("expected sk-test123, got %s", got["FIREWORKS_API_KEY"])
	}

	// Delete
	s.store.DeleteProvider(1, provider.ID)
	list2, _ := s.store.ListProviders(1)
	if len(list2) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(list2))
	}
}

func TestGetAllProviderEnvVars(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Add two providers
	data1, _ := json.Marshal(map[string]string{"FIREWORKS_API_KEY": "fw-key", "model": "llama3"})
	enc1, _ := Encrypt(s.secret, string(data1))
	s.store.CreateProvider(1, 0, "llm", "Fireworks", enc1)

	data2, _ := json.Marshal(map[string]string{
		"OLLAMA_HOST":        "http://localhost:11434",
		"OLLAMA_MODEL":       "llama3.2",
		"OLLAMA_EMBED_MODEL": "qwen3-embedding:0.6b",
		"OLLAMA_EMBED_DIM":   "1024",
	})
	enc2, _ := Encrypt(s.secret, string(data2))
	s.store.CreateProvider(1, 0, "llm", "Ollama", enc2)

	// Get env vars
	envVars, err := s.store.GetAllProviderEnvVars(1, s.secret)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}

	// Should have uppercase provider fields, but NOT lowercase "model".
	if envVars["FIREWORKS_API_KEY"] != "fw-key" {
		t.Errorf("expected fw-key, got %s", envVars["FIREWORKS_API_KEY"])
	}
	if envVars["OLLAMA_HOST"] != "http://localhost:11434" {
		t.Errorf("expected http://localhost:11434, got %s", envVars["OLLAMA_HOST"])
	}
	if envVars["OLLAMA_MODEL"] != "llama3.2" {
		t.Errorf("expected llama3.2, got %s", envVars["OLLAMA_MODEL"])
	}
	if envVars["OLLAMA_EMBED_MODEL"] != "qwen3-embedding:0.6b" {
		t.Errorf("expected qwen3-embedding:0.6b, got %s", envVars["OLLAMA_EMBED_MODEL"])
	}
	if envVars["OLLAMA_EMBED_DIM"] != "1024" {
		t.Errorf("expected 1024, got %s", envVars["OLLAMA_EMBED_DIM"])
	}
	if _, ok := envVars["model"]; ok {
		t.Error("lowercase 'model' should not be in env vars")
	}
}

func TestGetAllProviderEnvVars_CodexRefreshFailureBlocksExpiredToken(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	state := map[string]any{
		"auth": map[string]any{
			"provider": openAICodexAuthProvider,
		},
		"credentials": map[string]any{
			"access_token":  "expired-access-token",
			"refresh_token": "reused-refresh-token",
			"expires_at":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
	raw, _ := json.Marshal(state)
	enc, _ := Encrypt(s.secret, string(raw))
	if _, err := s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", enc); err != nil {
		t.Fatal(err)
	}

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_reused","message":"Please sign in again"}}`))
	}))
	defer tokenServer.Close()
	oldEndpoint := openAICodexTokenEndpoint
	openAICodexTokenEndpoint = tokenServer.URL
	defer func() { openAICodexTokenEndpoint = oldEndpoint }()

	envVars, err := s.store.GetAllProviderEnvVars(1, s.secret)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	if envVars != nil {
		t.Fatalf("env vars = %#v, want nil on refresh failure", envVars)
	}
	if !strings.Contains(err.Error(), "refresh_token_reused") || !strings.Contains(err.Error(), "Please sign in again") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestGetAllProviderEnvVars_CodexRefreshSuccessExportsFreshToken(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	state := map[string]any{
		"auth": map[string]any{
			"provider": openAICodexAuthProvider,
		},
		"credentials": map[string]any{
			"access_token":  "expired-access-token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
	raw, _ := json.Marshal(state)
	enc, _ := Encrypt(s.secret, string(raw))
	provider, err := s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", enc)
	if err != nil {
		t.Fatal(err)
	}
	freshToken := testJWTWithExpiry(time.Now().Add(time.Hour))

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-token" {
			t.Fatalf("unexpected token refresh form: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": freshToken,
		})
	}))
	defer tokenServer.Close()
	oldEndpoint := openAICodexTokenEndpoint
	openAICodexTokenEndpoint = tokenServer.URL
	defer func() { openAICodexTokenEndpoint = oldEndpoint }()

	envVars, err := s.store.GetAllProviderEnvVars(1, s.secret)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}
	if envVars["OPENAI_CODEX_ACCESS_TOKEN"] != freshToken {
		t.Fatalf("OPENAI_CODEX_ACCESS_TOKEN not refreshed")
	}
	if envVars["OPENAI_CODEX_PROVIDER_ID"] != fmt.Sprint(provider.ID) {
		t.Fatalf("OPENAI_CODEX_PROVIDER_ID = %q, want %d", envVars["OPENAI_CODEX_PROVIDER_ID"], provider.ID)
	}
}

func TestGetAllProviderEnvVars_CodexRefreshSerializesConcurrentCallers(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()
	state := map[string]any{
		"auth": map[string]any{
			"provider": openAICodexAuthProvider,
		},
		"credentials": map[string]any{
			"access_token":  "expired-access-token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
	raw, _ := json.Marshal(state)
	enc, _ := Encrypt(s.secret, string(raw))
	if _, err := s.store.CreateProvider(1, 15, "llm", "OpenAI Codex", enc); err != nil {
		t.Fatal(err)
	}
	freshToken := testJWTWithExpiry(time.Now().Add(time.Hour))
	var calls atomic.Int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-token" {
			http.Error(w, "stale or unexpected refresh token", http.StatusUnauthorized)
			return
		}
		time.Sleep(50 * time.Millisecond)
		if call > 1 {
			http.Error(w, `{"error":{"code":"refresh_token_reused"}}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  freshToken,
			"refresh_token": "next-refresh-token",
		})
	}))
	defer tokenServer.Close()
	oldEndpoint := openAICodexTokenEndpoint
	openAICodexTokenEndpoint = tokenServer.URL
	defer func() { openAICodexTokenEndpoint = oldEndpoint }()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			envVars, err := s.store.GetAllProviderEnvVars(1, s.secret)
			if err != nil {
				errs <- err
				return
			}
			if envVars["OPENAI_CODEX_ACCESS_TOKEN"] != freshToken {
				errs <- fmt.Errorf("OPENAI_CODEX_ACCESS_TOKEN = %q, want fresh token", envVars["OPENAI_CODEX_ACCESS_TOKEN"])
				return
			}
			errs <- nil
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}
}

func testJWTWithExpiry(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "sub": "test-user"})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + "."
}

func TestOllamaProviderTypeIncludesEmbeddingFields(t *testing.T) {
	s := newTestServer(t)

	pt, err := s.store.GetProviderType(4)
	if err != nil {
		t.Fatalf("GetProviderType(4): %v", err)
	}
	got := map[string]bool{}
	for _, field := range pt.Fields {
		got[field] = true
	}
	for _, field := range []string{"OLLAMA_HOST", "OLLAMA_MODEL", "OLLAMA_EMBED_MODEL", "OLLAMA_EMBED_DIM"} {
		if !got[field] {
			t.Fatalf("Ollama provider fields missing %s: %+v", field, pt.Fields)
		}
	}
}

func TestGoogleProviderTypeSeeded(t *testing.T) {
	s := newTestServer(t)

	pt, err := s.store.GetProviderType(16)
	if err != nil {
		t.Fatalf("GetProviderType(16): %v", err)
	}
	if pt.Type != "llm" {
		t.Fatalf("Type=%q, want llm", pt.Type)
	}
	if pt.Name != "Google" {
		t.Fatalf("Name=%q, want Google", pt.Name)
	}
	if pt.AuthType != "api_key" {
		t.Fatalf("AuthType=%q, want api_key", pt.AuthType)
	}
	if pt.AuthProvider != "google" {
		t.Fatalf("AuthProvider=%q, want google", pt.AuthProvider)
	}
	if pt.RuntimeStatus != "available" {
		t.Fatalf("RuntimeStatus=%q, want available", pt.RuntimeStatus)
	}

	got := map[string]bool{}
	for _, field := range pt.Fields {
		got[field] = true
	}
	if !got["GOOGLE_API_KEY"] {
		t.Fatalf("Google provider fields missing GOOGLE_API_KEY: %+v", pt.Fields)
	}
}

func TestLegacyBrowserProviderTypesUnsupported(t *testing.T) {
	s := newTestServer(t)

	types, err := s.store.ListProviderTypes()
	if err != nil {
		t.Fatalf("ListProviderTypes: %v", err)
	}

	statusByName := map[string]string{}
	for _, pt := range types {
		statusByName[pt.Name] = pt.RuntimeStatus
	}
	for _, name := range []string{"Browserbase", "Steel", "Browser Engine"} {
		if statusByName[name] != "unsupported" {
			t.Fatalf("%s runtime_status=%q, want unsupported", name, statusByName[name])
		}
	}
}

func TestIsEnvVar(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{"FIREWORKS_API_KEY", true},
		{"OLLAMA_HOST", true},
		{"API_PORT", true},
		{"model", false},
		{"base_url", false},
		{"OpenAI", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isEnvVar(c.input); got != c.expected {
			t.Errorf("isEnvVar(%q) = %v, want %v", c.input, got, c.expected)
		}
	}
}

func TestProviderIsolation(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	postJSON(t, s.handleRegister, map[string]string{
		"email": "bob@test.com", "password": "password123",
	})

	data, _ := json.Marshal(map[string]string{"KEY": "alice-secret"})
	enc, _ := Encrypt(s.secret, string(data))
	s.store.CreateProvider(1, 0, "llm", "Alice Provider", enc)

	// Bob should see nothing
	bobProviders, _ := s.store.ListProviders(2)
	if len(bobProviders) != 0 {
		t.Errorf("bob should see 0 providers, got %d", len(bobProviders))
	}

	// Bob can't access Alice's provider
	_, _, err := s.store.GetProvider(2, 1)
	if err == nil {
		t.Error("bob should not access alice's provider")
	}
}

func TestProviderUpdateMerge(t *testing.T) {
	s := newTestServer(t)
	s.secret = testSecret()

	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})

	// Create provider with API key + model
	origData := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-real-key-12345",
		"model_large":       "claude-opus-4-6",
		"model_medium":      "claude-sonnet-4-6",
		"model_small":       "claude-haiku-4-5-20251001",
	}
	dataJSON, _ := json.Marshal(origData)
	enc, _ := Encrypt(s.secret, string(dataJSON))
	provider, _ := s.store.CreateProvider(1, 0, "anthropic", "anthropic", enc)

	// Simulate GET (returns masked data)
	getReq := httptest.NewRequest("GET", fmt.Sprintf("/providers/%d", provider.ID), nil)
	getReq.Header.Set("X-User-ID", "1")
	getW := httptest.NewRecorder()
	s.handleGetProvider(getW, getReq)

	var getResult struct {
		Type string            `json:"type"`
		Name string            `json:"name"`
		Data map[string]string `json:"data"`
	}
	json.Unmarshal(getW.Body.Bytes(), &getResult)

	// Verify API key is masked
	if !strings.Contains(getResult.Data["ANTHROPIC_API_KEY"], "...") {
		t.Errorf("API key should be masked, got: %s", getResult.Data["ANTHROPIC_API_KEY"])
	}

	// Update just the model_large — send back masked API key
	getResult.Data["model_large"] = "claude-sonnet-4-6"
	putBody, _ := json.Marshal(getResult)
	putReq := httptest.NewRequest("PUT", fmt.Sprintf("/providers/%d", provider.ID), bytes.NewReader(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putReq.Header.Set("X-User-ID", "1")
	putW := httptest.NewRecorder()
	s.handleUpdateProvider(putW, putReq)

	if putW.Code != 200 {
		t.Fatalf("PUT failed: %d %s", putW.Code, putW.Body.String())
	}

	// Verify API key is preserved (not replaced with masked value)
	_, encAfter, err := s.store.GetProvider(1, provider.ID)
	if err != nil {
		t.Fatalf("GetProvider after update: %v", err)
	}
	plain, _ := Decrypt(s.secret, encAfter)
	var afterData map[string]string
	json.Unmarshal([]byte(plain), &afterData)

	if afterData["ANTHROPIC_API_KEY"] != "sk-ant-real-key-12345" {
		t.Errorf("API key was wiped! got: %q", afterData["ANTHROPIC_API_KEY"])
	}
	if afterData["model_large"] != "claude-sonnet-4-6" {
		t.Errorf("model_large not updated, got: %q", afterData["model_large"])
	}
	if afterData["model_medium"] != "claude-sonnet-4-6" {
		t.Errorf("model_medium should be preserved, got: %q", afterData["model_medium"])
	}
}
