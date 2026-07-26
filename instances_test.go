package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: register + login (creates user, session cookie set as side effect)
func registerAndLogin(t *testing.T, s *Server) {
	t.Helper()
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
}

func authedRequest(t *testing.T, method, path, token string, body any) *http.Request {
	t.Helper()
	var req *http.Request
	if body != nil {
		data, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(data))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Set user ID (normally done by middleware)
	req.Header.Set("X-User-ID", "1")
	return req
}

func TestListInstances_Empty(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)

	req := authedRequest(t, "GET", "/instances", "", nil)
	w := httptest.NewRecorder()
	s.handleListInstances(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var instances []any
	json.Unmarshal(w.Body.Bytes(), &instances)
	if len(instances) != 0 {
		t.Errorf("expected 0, got %d", len(instances))
	}
}

func TestCreateInstance_NoStart(t *testing.T) {
	// Test that instance is created in DB even if core binary doesn't exist
	s := newTestServer(t)
	registerAndLogin(t, s)

	// Create instance — core won't start (binary is "echo") but DB entry should exist
	req := authedRequest(t, "POST", "/instances", "", map[string]string{
		"name": "test-agent", "directive": "do stuff",
	})
	w := httptest.NewRecorder()
	s.handleCreateInstance(w, req)

	// May fail to start core, but instance should be in DB
	instances, _ := s.store.ListAgents(1, "")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance in DB, got %d", len(instances))
	}
	if instances[0].Name != "test-agent" {
		t.Errorf("expected test-agent, got %s", instances[0].Name)
	}
}

func TestGetInstance(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateAgent(1, "agent", "directive", "autonomous", "{}", "")

	req := authedRequest(t, "GET", "/instances/1", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	if body["name"] != "agent" {
		t.Errorf("expected agent, got %v", body["name"])
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)

	req := authedRequest(t, "GET", "/instances/999", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteInstance(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateAgent(1, "agent", "dir", "autonomous", "{}", "")

	req := authedRequest(t, "DELETE", "/instances/1", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	instances, _ := s.store.ListAgents(1, "")
	if len(instances) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(instances))
	}
}

func TestUpdateConfig(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateAgent(1, "agent", "old directive", "autonomous", "{}", "")

	req := authedRequest(t, "PUT", "/instances/1/config", "", map[string]string{
		"directive": "new directive",
	})
	w := httptest.NewRecorder()
	s.handleUpdateConfig(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	inst, _ := s.store.GetAgent(1, 1)
	if inst.Directive != "new directive" {
		t.Errorf("expected new directive, got %s", inst.Directive)
	}
}

func TestBackgroundMemoryTogglePersistsAndPreservesMemory(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent, err := s.store.CreateAgent(1, "memory-agent", "directive", "autonomous", `{"unconscious":true}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["unconscious"] = true
		cfg["threads"] = []any{
			map[string]any{"id": "unconscious", "system": true},
			map[string]any{"id": "worker"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(s.agents.instanceDir(agent.ID), "memory.jsonl")
	if err := os.WriteFile(memoryPath, []byte("preserve me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	req := authedRequest(t, http.MethodPut, "/instances/1/background-memory", "", map[string]any{
		"enabled": false,
		"restart": false,
	})
	w := httptest.NewRecorder()
	s.handleBackgroundMemory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle status=%d body=%s", w.Code, w.Body.String())
	}

	configData, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(configData, &cfg); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := cfg["unconscious"].(bool); enabled {
		t.Fatalf("unconscious still enabled: %s", configData)
	}
	threads, _ := cfg["threads"].([]any)
	if len(threads) != 1 || threads[0].(map[string]any)["id"] != "worker" {
		t.Fatalf("threads=%#v, want worker only", threads)
	}
	if got, err := os.ReadFile(memoryPath); err != nil || string(got) != "preserve me\n" {
		t.Fatalf("memory changed: %q err=%v", got, err)
	}
	stored, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.backgroundMemoryEnabled(stored) {
		t.Fatal("background memory still enabled after toggle")
	}

	req = authedRequest(t, http.MethodGet, "/instances/1/background-memory", "", nil)
	w = httptest.NewRecorder()
	s.handleBackgroundMemory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}
	var state struct {
		Enabled bool `json:"enabled"`
		Running bool `json:"running"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Enabled || state.Running {
		t.Fatalf("state=%+v, want disabled and stopped", state)
	}
}

func TestAgentBootResumeMode(t *testing.T) {
	s := newTestServer(t)

	t.Setenv("APTEVA_AGENT_BOOT_RESUME", "")
	if got := s.agentBootResumeMode(); got != "staggered" {
		t.Fatalf("default mode=%q, want staggered", got)
	}

	if err := s.store.SetSetting("agent_boot_resume", "manual"); err != nil {
		t.Fatal(err)
	}
	if got := s.agentBootResumeMode(); got != "manual" {
		t.Fatalf("setting mode=%q, want manual", got)
	}

	t.Setenv("APTEVA_AGENT_BOOT_RESUME", "staggered")
	if got := s.agentBootResumeMode(); got != "staggered" {
		t.Fatalf("env mode=%q, want staggered", got)
	}

	t.Setenv("APTEVA_AGENT_BOOT_RESUME", "auto")
	if got := s.agentBootResumeMode(); got != "auto" {
		t.Fatalf("env mode=%q, want auto", got)
	}
}

func TestAgentShutdownPolicy(t *testing.T) {
	s := newTestServer(t)

	t.Setenv("APTEVA_AGENT_SHUTDOWN_POLICY", "")
	if got := s.agentShutdownPolicy(); got != "stop" {
		t.Fatalf("default policy=%q, want stop", got)
	}

	if err := s.store.SetSetting("agent_shutdown_policy", "stop"); err != nil {
		t.Fatal(err)
	}
	if got := s.agentShutdownPolicy(); got != "stop" {
		t.Fatalf("setting policy=%q, want stop", got)
	}

	t.Setenv("APTEVA_AGENT_SHUTDOWN_POLICY", "detach")
	if got := s.agentShutdownPolicy(); got != "detach" {
		t.Fatalf("env policy=%q, want detach", got)
	}
}

func assertRoleSplitOutputEntries(t *testing.T, servers []any) {
	t.Helper()
	found := map[string]bool{}
	for _, raw := range servers {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		if name != "channels" && name != agentOutputMCPName {
			continue
		}
		found[name] = true
		if entry["no_spawn"] != true {
			t.Fatalf("%s no_spawn = %#v, want true", name, entry["no_spawn"])
		}
		loading, _ := entry["tool_loading"].(map[string]any)
		wantLoading := "deferred"
		if name == agentOutputMCPName {
			wantLoading = "always"
		}
		if loading["default"] != wantLoading {
			t.Fatalf("%s tool_loading = %#v, want default=%s", name, entry["tool_loading"], wantLoading)
		}
	}
	for _, name := range []string{"channels", agentOutputMCPName} {
		if !found[name] {
			t.Fatalf("%s entry missing from %#v", name, servers)
		}
	}
}

func TestChannelsMCPConfigsSplitConversationFromMainOutput(t *testing.T) {
	conversation := channelsMCPConfig("http://127.0.0.1:9999")
	output := agentOutputMCPConfig("http://127.0.0.1:9998")
	assertRoleSplitOutputEntries(t, []any{conversation, output})
	if conversation["url"] != "http://127.0.0.1:9999" || conversation["transport"] != "http" {
		t.Fatalf("conversation channels connection fields = %#v", conversation)
	}
	if output["url"] != "http://127.0.0.1:9998" || output["transport"] != "http" {
		t.Fatalf("agent output connection fields = %#v", output)
	}
}

func TestAgentManagerStartInjectsRoleSplitOutputMCPs(t *testing.T) {
	dir := t.TempDir()
	core := filepath.Join(dir, "fake-core")
	script := `#!/bin/sh
trap 'exit 0' TERM INT
while :; do sleep 1 & wait $!; done
`
	if err := os.WriteFile(core, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	im := NewAgentManager(filepath.Join(dir, "agents"), core)
	inst := &Agent{
		ID: 78, UserID: 1, Name: "channels-policy", Mode: "autonomous",
		Config: "{}", Kind: "user",
	}
	if err := im.Start(inst, nil, "5280", nil, "agent-secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { im.Stop(inst.ID) })

	raw, err := os.ReadFile(filepath.Join(im.InstanceDir(inst.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers, _ := cfg["mcp_servers"].([]any)
	assertRoleSplitOutputEntries(t, servers)
}

func TestAgentManagerStartPassesCodexRuntimeRefreshEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "core-env.txt")
	core := filepath.Join(dir, "fake-core")
	script := `#!/bin/sh
trap 'exit 0' TERM INT
env | sort > "$APTEVA_ENV_CAPTURE"
while :; do sleep 1 & wait $!; done
`
	if err := os.WriteFile(core, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake core: %v", err)
	}
	t.Setenv("APTEVA_ENV_CAPTURE", envFile)

	im := NewAgentManager(filepath.Join(dir, "agents"), core)
	inst := &Agent{
		ID:        77,
		UserID:    1,
		Name:      "codex-env",
		Mode:      "autonomous",
		Config:    `{"include_channels":false}`,
		ProjectID: "project-a",
		Kind:      "user",
	}
	providerEnv := map[string]string{
		"OPENAI_CODEX_ACCESS_TOKEN": "access-token",
		"OPENAI_CODEX_PROVIDER_ID":  "42",
		"OPENAI_CODEX_ACCOUNT_ID":   "account-a",
	}
	parallel := true
	providerPool := []ProviderInfo{{
		Type:        "openai-codex",
		ModelLarge:  "gpt-5.6-sol",
		ModelMedium: "gpt-5.6-terra",
		ModelSmall:  "gpt-5.6-terra",
		ModelCapabilities: map[string]ProviderModelCapabilities{
			"gpt-5.6-terra": {
				ContextWindow:                 400000,
				EffectiveContextWindowPercent: 95,
				SupportsParallelToolCalls:     &parallel,
			},
		},
	}}
	if err := im.Start(inst, providerEnv, "5280", providerPool, "agent-secret"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { im.Stop(inst.ID) })

	var lines []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(envFile)
		if err == nil && len(data) > 0 {
			lines = strings.Split(strings.TrimSpace(string(data)), "\n")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) == 0 {
		t.Fatalf("fake core did not write env file")
	}

	assertEnvValue(t, lines, "SERVER_URL", "http://127.0.0.1:5280")
	assertEnvValue(t, lines, "APTEVA_API_KEY", inst.CoreAPIKey)
	assertEnvValue(t, lines, "OPENAI_CODEX_ACCESS_TOKEN", "access-token")
	assertEnvValue(t, lines, "OPENAI_CODEX_PROVIDER_ID", "42")
	assertEnvValue(t, lines, "OPENAI_CODEX_ACCOUNT_ID", "account-a")
	if !strings.HasPrefix(inst.CoreAPIKey, "core_") {
		t.Fatalf("CoreAPIKey=%q, want generated core_ key", inst.CoreAPIKey)
	}

	rawConfig, err := os.ReadFile(filepath.Join(im.InstanceDir(inst.ID), "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		t.Fatalf("decode config.json: %v", err)
	}
	providers, _ := config["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers = %#v", config["providers"])
	}
	provider, _ := providers[0].(map[string]any)
	models, _ := provider["models"].(map[string]any)
	if models["large"] != "gpt-5.6-sol" || models["medium"] != "gpt-5.6-terra" || models["small"] != "gpt-5.6-terra" {
		t.Fatalf("models = %#v", models)
	}
	caps, _ := provider["model_capabilities"].(map[string]any)
	terra, _ := caps["gpt-5.6-terra"].(map[string]any)
	if terra["context_window"] != float64(400000) || terra["supports_parallel_tool_calls"] != true {
		t.Fatalf("model_capabilities = %#v", caps)
	}
}

func TestAgentManagerReattachRefreshesChannelsConfig(t *testing.T) {
	var sawAuth string
	var sawConfig map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/config":
			if r.Method != http.MethodPut {
				http.Error(w, "PUT only", http.StatusMethodNotAllowed)
				return
			}
			sawAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&sawConfig); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	port := core.Listener.Addr().(*net.TCPAddr).Port

	im := NewAgentManager(t.TempDir(), "missing-core")
	inst := &Agent{
		ID:         42,
		UserID:     1,
		Name:       "reattach",
		Mode:       "autonomous",
		Config:     "{}",
		Port:       port,
		Pid:        os.Getpid(),
		CoreAPIKey: "core_test",
		Status:     "running",
		Kind:       "user",
	}
	dir := im.InstanceDir(inst.ID)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mcp_servers":[{"name":"custom","url":"http://example.test","transport":"http"},{"name":"channels","url":"http://127.0.0.1:1","transport":"http"}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := im.Reattach(inst, "5280"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		im.mu.Lock()
		ri := im.processes[inst.ID]
		delete(im.processes, inst.ID)
		im.mu.Unlock()
		if ri != nil && ri.channels != nil {
			ri.channels.Stop()
		}
	})

	if got := im.GetPort(inst.ID); got != port {
		t.Fatalf("reattached port=%d, want %d", got, port)
	}
	if got := im.GetCoreAPIKey(inst.ID); got != "core_test" {
		t.Fatalf("core key=%q, want persisted key", got)
	}
	if sawAuth != "Bearer core_test" {
		t.Fatalf("Authorization=%q, want persisted bearer key", sawAuth)
	}
	servers, _ := sawConfig["mcp_servers"].([]any)
	if len(servers) != 3 {
		t.Fatalf("mcp_servers=%v, want agent-output + channels + custom", servers)
	}
	names := map[string]bool{}
	for _, raw := range servers {
		m, _ := raw.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, name := range []string{agentOutputMCPName, "channels", "custom"} {
		if !names[name] {
			t.Fatalf("missing %s from refreshed config: %#v", name, sawConfig)
		}
	}
	assertRoleSplitOutputEntries(t, servers)
}

func TestResumeRunningInstancesManualModeSkipsSpawn(t *testing.T) {
	s := newTestServer(t)
	s.agents.coreCmd = "/definitely/missing/apteva-core"
	if err := s.store.SetSetting("agent_boot_resume", "manual"); err != nil {
		t.Fatal(err)
	}
	user, _ := s.store.CreateUser("resume@test.com", "hash")
	inst, _ := s.store.CreateAgent(user.ID, "agent", "dir", "autonomous", "{}", "")
	inst.Status = "running"
	inst.Port = 3210
	inst.Pid = 123
	if err := s.store.UpdateAgent(inst); err != nil {
		t.Fatal(err)
	}

	s.ResumeRunningInstances()

	got, _ := s.store.GetAgent(user.ID, inst.ID)
	if got.Status != "running" || got.Port != 3210 || got.Pid != 123 {
		t.Fatalf("manual mode should leave row untouched, got %+v", got)
	}
	if s.agents.IsRunning(inst.ID) {
		t.Fatal("manual mode should not spawn a process")
	}
}

func TestInstanceManager_PortTracking(t *testing.T) {
	im := NewAgentManager(t.TempDir(), "sleep")

	// Not running initially
	if im.IsRunning(1) {
		t.Error("should not be running")
	}
	if im.GetPort(1) != 0 {
		t.Error("port should be 0 when not running")
	}

	// Simulate a running instance by directly inserting into the map
	cmd := exec.Command("sleep", "60")
	cmd.Start()
	defer cmd.Process.Kill()

	im.mu.Lock()
	im.processes[1] = &runningAgent{cmd: cmd, port: 5001}
	im.mu.Unlock()

	if !im.IsRunning(1) {
		t.Error("should be running")
	}
	if im.GetPort(1) != 5001 {
		t.Errorf("expected port 5001, got %d", im.GetPort(1))
	}

	// Stop should clear the process
	im.Stop(1)
	if im.IsRunning(1) {
		t.Error("should not be running after stop")
	}
	if im.GetPort(1) != 0 {
		t.Error("port should be 0 after stop")
	}
}

func TestInstanceManager_StopNotRunning(t *testing.T) {
	im := NewAgentManager(t.TempDir(), "sleep")
	// Should not panic
	im.Stop(999)
}

func TestInstanceIsolation(t *testing.T) {
	s := newTestServer(t)

	// Create two users
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	postJSON(t, s.handleRegister, map[string]string{
		"email": "bob@test.com", "password": "password123",
	})

	// Alice creates an instance
	s.store.CreateAgent(1, "alice-agent", "alice stuff", "autonomous", "{}", "")

	// Bob creates an instance
	s.store.CreateAgent(2, "bob-agent", "bob stuff", "autonomous", "{}", "")

	// Alice should see only her instance
	aliceInstances, _ := s.store.ListAgents(1, "")
	if len(aliceInstances) != 1 || aliceInstances[0].Name != "alice-agent" {
		t.Errorf("alice should see only alice-agent, got %v", aliceInstances)
	}

	// Bob should see only his
	bobInstances, _ := s.store.ListAgents(2, "")
	if len(bobInstances) != 1 || bobInstances[0].Name != "bob-agent" {
		t.Errorf("bob should see only bob-agent, got %v", bobInstances)
	}

	// Alice can't access Bob's instance
	_, err := s.store.GetAgent(1, 2)
	if err == nil {
		t.Error("alice should not see bob's instance")
	}
}
