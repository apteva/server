package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// providerSwitchCore stands in for a running core: it takes PUT /config and,
// like Core, reports a chosen default it has no credentials for.
type providerSwitchCore struct {
	mu       sync.Mutex
	canRun   map[string]bool
	defaults []string
}

func attachProviderSwitchCore(t *testing.T, s *Server, agentID int64, canRun ...string) *providerSwitchCore {
	t.Helper()
	c := &providerSwitchCore{canRun: map[string]bool{}}
	for _, name := range canRun {
		c.canRun[name] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/config" {
			writeJSON(w, map[string]any{"directive": "directive", "mcp_servers": []any{}})
			return
		}
		if r.Method != http.MethodPut || r.URL.Path != "/config" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Providers []struct {
				Name    string `json:"name"`
				Default bool   `json:"default"`
			} `json:"providers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		chosen := ""
		for _, p := range body.Providers {
			// Realtime companions keep their own default, as in Core.
			if p.Default && !strings.HasSuffix(p.Name, "-realtime") {
				chosen = p.Name
			}
		}
		c.mu.Lock()
		c.defaults = append(c.defaults, chosen)
		c.mu.Unlock()
		out := map[string]any{"status": "updated"}
		if chosen != "" && !c.canRun[chosen] {
			out["provider_unavailable"] = map[string]string{"name": chosen, "reason": "its key was not set when the agent started"}
		}
		writeJSON(w, out)
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	s.agents.mu.Lock()
	s.agents.processes[agentID] = &runningAgent{port: port, coreAPIKey: "test-core", reattached: true}
	s.agents.mu.Unlock()
	return c
}

func providerSwitchServer(t *testing.T) (*Server, *Agent) {
	t.Helper()
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.secret = testSecret()
	createProviderSelectionFixture(t, s, 2, "OpenAI", "", map[string]any{"OPENAI_API_KEY": "openai-key"})
	createProviderSelectionFixture(t, s, 3, "Anthropic", "", map[string]any{"ANTHROPIC_API_KEY": "anthropic-key"})
	agent, err := s.store.CreateAgent(1, "switcher", "directive", "autonomous", `{"default_provider":"openai"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, agent
}

func putAgentProviders(t *testing.T, s *Server, id int64, providers []map[string]any) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleUpdateConfig(rec, authedRequest(t, http.MethodPut, fmt.Sprintf("/instances/%d/config", id), "", map[string]any{"providers": providers}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT config status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func TestProviderSwitchRestartsOnlyWhenCoreCannotRunTheChoice(t *testing.T) {
	s, agent := providerSwitchServer(t)
	core := attachProviderSwitchCore(t, s, agent.ID, "openai")
	var restarts []int64
	s.providerSwitchRestarter = func(_ context.Context, id int64) error {
		// The handler must have let go of the agent's config lock: the real
		// restart takes it.
		acquired := make(chan struct{})
		go func() { s.lockAgentConfig(id)(); close(acquired) }()
		select {
		case <-acquired:
		case <-time.After(2 * time.Second):
			t.Error("config lock still held during the restart")
		}
		restarts = append(restarts, id)
		return nil
	}

	// A provider the core already runs applies live.
	out := putAgentProviders(t, s, agent.ID, []map[string]any{{"name": "openai", "default": true}, {"name": "anthropic"}})
	if len(restarts) != 0 || out["restarted"] != nil {
		t.Fatalf("restarted for a provider the core can run: %v core saw %v", restarts, core.defaults)
	}

	// One it has no credentials for restarts the agent so it gets them.
	out = putAgentProviders(t, s, agent.ID, []map[string]any{{"name": "openai"}, {"name": "anthropic", "default": true}})
	if !reflect.DeepEqual(restarts, []int64{agent.ID}) {
		t.Fatalf("restarts = %v", restarts)
	}
	if out["restarted"] != true || out["restart_reason"] != restartReasonProviderUnavailable || out["provider_unavailable"] != nil {
		t.Fatalf("response = %#v", out)
	}
	saved, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentDefaultProvider(saved.Config); got != "anthropic" {
		t.Fatalf("saved default = %q", got)
	}
	if !reflect.DeepEqual(core.defaults, []string{"openai", "anthropic"}) {
		t.Fatalf("core saw defaults %v", core.defaults)
	}
}

func TestProviderSwitchReportsAFailedRestart(t *testing.T) {
	s, agent := providerSwitchServer(t)
	attachProviderSwitchCore(t, s, agent.ID, "openai")
	s.providerSwitchRestarter = func(context.Context, int64) error { return errors.New("core did not come back") }

	out := putAgentProviders(t, s, agent.ID, []map[string]any{{"name": "openai"}, {"name": "anthropic", "default": true}})
	if out["restarted"] != false || out["restart_error"] != "core did not come back" {
		t.Fatalf("response = %#v", out)
	}
	// The running core still can't use it, so the agent says so.
	if unavailable, _ := out["provider_unavailable"].(map[string]any); unavailable["name"] != "anthropic" {
		t.Fatalf("provider_unavailable = %#v", out["provider_unavailable"])
	}
}

func TestStaleProviderEnvComparesWhatTheCoreStartedWith(t *testing.T) {
	started := providerEnvFingerprint(map[string]string{
		"ANTHROPIC_API_KEY":         "key-1",
		"OPENAI_CODEX_ACCESS_TOKEN": "token-1",
		"OPENAI_CODEX_PROVIDER_ID":  "7",
	})
	for name, tc := range map[string]struct {
		current map[string]string
		want    []string
	}{
		"unchanged":                {map[string]string{"ANTHROPIC_API_KEY": "key-1", "OPENAI_CODEX_ACCESS_TOKEN": "token-1", "OPENAI_CODEX_PROVIDER_ID": "7"}, nil},
		"refreshed token":          {map[string]string{"ANTHROPIC_API_KEY": "key-1", "OPENAI_CODEX_ACCESS_TOKEN": "token-2", "OPENAI_CODEX_PROVIDER_ID": "7"}, nil},
		"new provider":             {map[string]string{"ANTHROPIC_API_KEY": "key-1", "OPENAI_CODEX_ACCESS_TOKEN": "token-1", "OPENAI_CODEX_PROVIDER_ID": "7", "XAI_API_KEY": "x"}, nil},
		"replaced key":             {map[string]string{"ANTHROPIC_API_KEY": "key-2", "OPENAI_CODEX_ACCESS_TOKEN": "token-1", "OPENAI_CODEX_PROVIDER_ID": "7"}, []string{"ANTHROPIC_API_KEY"}},
		"removed connection":       {map[string]string{"ANTHROPIC_API_KEY": "key-1"}, []string{"OPENAI_CODEX_ACCESS_TOKEN", "OPENAI_CODEX_PROVIDER_ID"}},
		"signed in to another one": {map[string]string{"ANTHROPIC_API_KEY": "key-1", "OPENAI_CODEX_ACCESS_TOKEN": "token-9", "OPENAI_CODEX_PROVIDER_ID": "8"}, []string{"OPENAI_CODEX_PROVIDER_ID"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := staleProviderEnv(started, providerEnvFingerprint(tc.current)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stale = %v, want %v", got, tc.want)
			}
		})
	}
	for _, fingerprint := range started {
		if fingerprint == "key-1" || fingerprint == "token-1" {
			t.Fatal("fingerprint holds a credential")
		}
	}
}

func getAgentJSON(t *testing.T, s *Server, id int64) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleInstance(rec, authedRequest(t, http.MethodGet, fmt.Sprintf("/instances/%d", id), "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET agent status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAgentNeedsRestartWhenItsCredentialsChange(t *testing.T) {
	ttl := providerCredentialCheckTTL
	providerCredentialCheckTTL = 0
	t.Cleanup(func() { providerCredentialCheckTTL = ttl })
	s, agent := providerSwitchServer(t)
	env, err := s.GetAllProviderEnvVars(agent.UserID, agent.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	ri := &runningAgent{port: 1, reattached: true, providerEnv: providerEnvFingerprint(env)}
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = ri
	s.agents.mu.Unlock()

	if got := getAgentJSON(t, s, agent.ID); got["needs_restart"] != nil {
		t.Fatalf("fresh agent = %#v", got)
	}
	// A provider connected after the start isn't something this agent uses yet.
	createProviderSelectionFixture(t, s, 10, "NVIDIA", "", map[string]any{"NVIDIA_API_KEY": "nvidia-key"})
	if got := getAgentJSON(t, s, agent.ID); got["needs_restart"] != nil {
		t.Fatalf("after a new provider = %#v", got)
	}

	// Replacing a key the core holds leaves it on the old one.
	providers, err := s.store.ListProviders(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.Name != "Anthropic" {
			continue
		}
		encrypted, err := Encrypt(s.secret, `{"ANTHROPIC_API_KEY":"rotated-key"}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.UpdateProvider(1, p.ID, p.Type, p.Name, encrypted); err != nil {
			t.Fatal(err)
		}
	}
	got := getAgentJSON(t, s, agent.ID)
	if got["needs_restart"] != true || got["restart_reason"] != restartReasonCredentialsChanged {
		t.Fatalf("after a key change = %#v", got)
	}

	// A core adopted from an earlier server process has no record to compare.
	ri.providerEnv = nil
	if got := getAgentJSON(t, s, agent.ID); got["needs_restart"] != nil {
		t.Fatalf("adopted core = %#v", got)
	}
}

func TestAgentNeedsRestartWhenCoreReportsItsProviderCannotRun(t *testing.T) {
	s, agent := providerSwitchServer(t)
	ri := &runningAgent{port: 1, reattached: true}
	ri.runtimeInfo = coreRuntimeInfo{Version: "dev", ProviderUnavailable: &coreProviderUnavailable{Name: "anthropic", Reason: "ANTHROPIC_API_KEY was not set when the agent started"}}
	ri.runtimeAt = time.Now()
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = ri
	s.agents.mu.Unlock()

	got := getAgentJSON(t, s, agent.ID)
	if got["needs_restart"] != true || got["restart_reason"] != restartReasonProviderUnavailable {
		t.Fatalf("agent = %#v", got)
	}
	if unavailable, _ := got["provider_unavailable"].(map[string]any); unavailable["name"] != "anthropic" {
		t.Fatalf("provider_unavailable = %#v", got["provider_unavailable"])
	}

	// The list carries it too, from the same cached core status.
	rec := httptest.NewRecorder()
	s.handleListInstances(rec, authedRequest(t, http.MethodGet, "/instances", "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		var wrapped struct {
			Agents []map[string]any `json:"agents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wrapped); err != nil {
			t.Fatalf("decode list: %s", rec.Body.String())
		}
		list = wrapped.Agents
	}
	if len(list) != 1 || list[0]["needs_restart"] != true {
		t.Fatalf("list = %s", rec.Body.String())
	}
}
