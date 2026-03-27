package main

import (
	"encoding/json"
	"testing"
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

	data2, _ := json.Marshal(map[string]string{"OLLAMA_HOST": "http://localhost:11434"})
	enc2, _ := Encrypt(s.secret, string(data2))
	s.store.CreateProvider(1, 0, "llm", "Ollama", enc2)

	// Get env vars
	envVars, err := s.store.GetAllProviderEnvVars(1, s.secret)
	if err != nil {
		t.Fatalf("GetAllProviderEnvVars: %v", err)
	}

	// Should have FIREWORKS_API_KEY and OLLAMA_HOST, but NOT "model"
	if envVars["FIREWORKS_API_KEY"] != "fw-key" {
		t.Errorf("expected fw-key, got %s", envVars["FIREWORKS_API_KEY"])
	}
	if envVars["OLLAMA_HOST"] != "http://localhost:11434" {
		t.Errorf("expected http://localhost:11434, got %s", envVars["OLLAMA_HOST"])
	}
	if _, ok := envVars["model"]; ok {
		t.Error("lowercase 'model' should not be in env vars")
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
