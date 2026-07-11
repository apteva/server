package main

import (
	"reflect"
	"testing"
)

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
