package main

import (
	"reflect"
	"testing"
)

func TestEnvironmentAgentProviderEnvIncludesRuntimeConnections(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, "anthropic-api", "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": "{{credentials.api_key}}",
	})
	addConnection(t, s, "anthropic-api", "Anthropic", "project-a", map[string]string{
		"api_key": "connection-backed-key",
	})

	env, err := s.environmentAgentProviderEnv(1, "project-a", "http://environment-proxy.test")
	if err != nil {
		t.Fatalf("environmentAgentProviderEnv: %v", err)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "connection-backed-key" {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want connection-backed credential", got)
	}
	if got := env["HTTP_PROXY"]; got != "http://environment-proxy.test" {
		t.Errorf("HTTP_PROXY = %q", got)
	}
	if got := env["HTTPS_PROXY"]; got != "http://environment-proxy.test" {
		t.Errorf("HTTPS_PROXY = %q", got)
	}
}

func testEnvironmentWithInstalls(names ...string) *Environment {
	installs := map[string]*localInstall{}
	for _, name := range names {
		installs[name] = &localInstall{AppName: name}
	}
	return &Environment{
		ID:       "test-env",
		installs: installs,
		apps:     map[string]*SandboxAppInstance{},
	}
}

func TestEnvironmentAgentAppMCPNames_UsesSourceAgentBindings(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "media-agent", "process media", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO agents(id,user_id,name,project_id,status) VALUES(9999,1,'other','proj-1','stopped')`); err != nil {
		t.Fatalf("create decoy agent: %v", err)
	}
	seedBoundApp(t, s, "media", "proj-1", agent.ID)
	seedBoundApp(t, s, "storage", "proj-1", 9999)

	got := s.environmentAgentAppMCPNames(testEnvironmentWithInstalls("media", "storage"), agent)
	want := []string{"media"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentAgentAppMCPNames = %#v, want %#v", got, want)
	}
}

func TestEnvironmentAgentAppMCPNames_UsesExplicitSourceAppMCPs(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent := &Agent{
		ID: 1,
		Config: `{
			"mcp_servers": [
				{"name": "media", "url": "http://127.0.0.1:5280/api/apps/media/mcp", "transport": "http"},
				{"name": "storage", "url": "http://127.0.0.1:5280/mcp/12", "transport": "http"}
			]
		}`,
	}

	got := s.environmentAgentAppMCPNames(testEnvironmentWithInstalls("media", "storage"), agent)
	want := []string{"media"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentAgentAppMCPNames = %#v, want %#v", got, want)
	}
}

func TestEnvironmentAgentAppMCPNames_UnboundAgentGetsExplicitEnvironmentApps(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "unbound-agent", "do work", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	got := s.environmentAgentAppMCPNames(testEnvironmentWithInstalls("media", "storage"), agent)
	want := []string{"media", "storage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environmentAgentAppMCPNames = %#v, want %#v", got, want)
	}
}
