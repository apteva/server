package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func (f *fakeModelsAPI) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeModelsAPI) lastCreate(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.creates) == 0 {
		t.Fatal("no agent was created")
	}
	return f.creates[len(f.creates)-1]
}

func createdConfig(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := body["config"].(string)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config %q: %v", raw, err)
	}
	return cfg
}

func TestGatewayAgentsCreateRequiresProjectOrExplicitGlobal(t *testing.T) {
	fake, client := newFakeModelsAPI(t, "p1")

	// No project in the conversation and none named: refuse before any call.
	_, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work."}, "", client, nil, "")
	if err == nil || !strings.Contains(err.Error(), "project_id is required") || !strings.Contains(err.Error(), "global=true") {
		t.Fatalf("unscoped create: %v", err)
	}
	if _, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work.", "global": false}, "", client, nil, ""); err == nil {
		t.Fatal("global=false without a project was accepted")
	}
	if _, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work.", "global": "yes"}, "", client, nil, ""); err == nil || !strings.Contains(err.Error(), "global must be true or false") {
		t.Fatalf("bad global: %v", err)
	}
	if fake.createCount() != 0 {
		t.Fatalf("created %d agents without a project", fake.createCount())
	}

	// Explicit global: created with no project, and the receipt says so.
	out, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work.", "global": true}, "", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if body := fake.lastCreate(t); body["project_id"] != "" {
		t.Fatalf("global create sent project_id=%v", body["project_id"])
	}
	if receipt := out.(map[string]any); receipt["scope"] != "global" || receipt["project_id"] != "" {
		t.Fatalf("global receipt: %v", receipt)
	}

	// The conversation's project is the default, and the receipt names it.
	out, err = handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work."}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if body := fake.lastCreate(t); body["project_id"] != "p1" {
		t.Fatalf("project create sent project_id=%v", body["project_id"])
	}
	if receipt := out.(map[string]any); receipt["scope"] != "project" || receipt["project_id"] != "p1" {
		t.Fatalf("project receipt: %v", receipt)
	}

	// A named project works without a conversation project.
	out, err = handleGatewayAgentTool("agents_create", map[string]any{"name": "Worker", "directive": "Work.", "project_id": "p2"}, "", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt := out.(map[string]any); receipt["scope"] != "project" || receipt["project_id"] != "p2" {
		t.Fatalf("named project receipt: %v", receipt)
	}
}

func TestGatewayAgentsCreateSetsCheckedProviderAndModel(t *testing.T) {
	fake, client := newFakeModelsAPI(t, "p1")
	out, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name": "Writer", "directive": "Write.", "provider": "Claude Subscription", "model": "sonnet",
		"config": map[string]any{"onboarding_staged": true},
	}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := createdConfig(t, fake.lastCreate(t))
	override, _ := cfg["model_override"].(map[string]any)
	if cfg["default_provider"] != "claude-subscription" || override["provider"] != "claude-subscription" || override["model"] != "sonnet" || cfg["onboarding_staged"] != true {
		t.Fatalf("created config: %v", cfg)
	}
	receipt := out.(map[string]any)
	if receipt["provider"] != "claude-subscription" || receipt["model"] != "sonnet" || receipt["model_checked"] != true || receipt["scope"] != "project" {
		t.Fatalf("receipt: %v", receipt)
	}

	// Provider alone picks the provider and leaves its default model.
	if _, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Writer", "directive": "Write.", "provider": "claude-subscription"}, "p1", client, nil, ""); err != nil {
		t.Fatal(err)
	}
	cfg = createdConfig(t, fake.lastCreate(t))
	if _, pinned := cfg["model_override"]; pinned || cfg["default_provider"] != "claude-subscription" {
		t.Fatalf("provider-only config: %v", cfg)
	}

	// A provider whose model list is unavailable is accepted with a warning.
	out, err = handleGatewayAgentTool("agents_create", map[string]any{"name": "Writer", "directive": "Write.", "provider": "openai", "model": "gpt-5"}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt := out.(map[string]any); receipt["model_checked"] != false || !strings.Contains(receipt["warning"].(string), "not checked") {
		t.Fatalf("unchecked receipt: %v", receipt)
	}

	// Config without a provider is passed through untouched.
	if _, err := handleGatewayAgentTool("agents_create", map[string]any{"name": "Writer", "directive": "Write.", "config": map[string]any{"onboarding_staged": true}}, "p1", client, nil, ""); err != nil {
		t.Fatal(err)
	}
	if body := fake.lastCreate(t); body["config"] != `{"onboarding_staged":true}` {
		t.Fatalf("passthrough config: %v", body["config"])
	}
}

func TestGatewayAgentsCreateRejectsBadModelChoiceBeforeCreating(t *testing.T) {
	fake, client := newFakeModelsAPI(t, "p1")
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"model": "sonnet"}, "provider is required with model"},
		{map[string]any{"provider": "claude-subscription", "model": "gpt-9"}, "is not offered"},
		{map[string]any{"provider": "Claude Subscription", "model": "Claude Sonnet"}, "Did you mean sonnet"},
		{map[string]any{"provider": "mistral"}, "available providers: claude-subscription, openai"},
		{map[string]any{"provider": ""}, "provider must be"},
		{map[string]any{"config": map[string]any{"default_provider": "openai"}}, "config.default_provider is not accepted"},
		{map[string]any{"config": map[string]any{"model_override": map[string]any{"provider": "openai", "model": "x"}}}, "config.model_override is not accepted"},
		{map[string]any{"config": `["not","an","object"]`}, "config must be a JSON object"},
	}
	for _, tc := range cases {
		args := map[string]any{"name": "Writer", "directive": "Write."}
		for key, value := range tc.args {
			args[key] = value
		}
		_, err := handleGatewayAgentTool("agents_create", args, "p1", client, nil, "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: err=%v, want %q", tc.args, err, tc.want)
		}
	}
	if fake.createCount() != 0 {
		t.Fatalf("created %d agents from rejected requests", fake.createCount())
	}
}

func TestGatewayAgentsCreateReplayDoesNotClaimProviderChange(t *testing.T) {
	_, client := newFakeModelsAPI(t, "p1")
	out, err := handleGatewayAgentTool("agents_create", map[string]any{
		"name": "Existing", "directive": "Work.", "idempotency_key": "existing", "provider": "claude-subscription", "model": "haiku",
	}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt := out.(map[string]any)
	if receipt["idempotent_replay"] != true || receipt["scope"] != "project" || receipt["project_id"] != "p1" {
		t.Fatalf("replay receipt: %v", receipt)
	}
	if _, claimed := receipt["model"]; claimed || !strings.Contains(receipt["warning"].(string), "agents_update") {
		t.Fatalf("replay receipt claims a model change: %v", receipt)
	}
}

func TestGatewayAgentModelsHintsAtQueryWhenTruncated(t *testing.T) {
	fake, client := newFakeModelsAPI(t, "p1")
	fake.extra = gatewayModelsPerProvider + 20
	out, err := handleGatewayAgentTool("agents_models", map[string]any{"provider": "claude-subscription"}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	row := out.(map[string]any)["providers"].([]map[string]any)[0]
	hint, _ := row["models_hint"].(string)
	if row["models_truncated"] != true || row["models_total"] != gatewayModelsPerProvider+23 || !strings.Contains(hint, "pass query") || !strings.Contains(hint, "123") {
		t.Fatalf("truncated row: total=%v truncated=%v hint=%q", row["models_total"], row["models_truncated"], hint)
	}

	out, err = handleGatewayAgentTool("agents_models", map[string]any{"provider": "claude-subscription", "query": "extra"}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	row = out.(map[string]any)["providers"].([]map[string]any)[0]
	if hint, _ := row["models_hint"].(string); !strings.Contains(hint, "more specific query") {
		t.Fatalf("truncated query hint: %q", hint)
	}

	out, err = handleGatewayAgentTool("agents_models", map[string]any{"provider": "claude-subscription", "query": "son"}, "p1", client, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	row = out.(map[string]any)["providers"].([]map[string]any)[0]
	if _, hinted := row["models_hint"]; hinted || row["models_truncated"] != nil {
		t.Fatalf("short list flagged as truncated: %v", row)
	}
}
