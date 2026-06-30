package main

import "testing"

func TestRunningAgentsUseCodexProviderRequiresVisibleProvider(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("codex-refresh@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "agent", "directive", "autonomous", "{}", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	agent.Status = "running"
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatal(err)
	}

	if s.store.RunningAgentsUseCodexProvider() {
		t.Fatal("expected false with no OpenAI Codex provider")
	}
	if _, err := s.store.CreateProvider(user.ID, 15, "llm", "OpenAI Codex", "opaque", "project-b"); err != nil {
		t.Fatal(err)
	}
	if s.store.RunningAgentsUseCodexProvider() {
		t.Fatal("expected false when Codex provider is scoped to another project")
	}
	if _, err := s.store.CreateProvider(user.ID, 15, "llm", "OpenAI Codex", "opaque", "project-a"); err != nil {
		t.Fatal(err)
	}
	if !s.store.RunningAgentsUseCodexProvider() {
		t.Fatal("expected true when a running user agent can see an OpenAI Codex provider")
	}
}

func TestAgentUsesRefreshedCodexProviderProjectVisibility(t *testing.T) {
	agent := &Agent{ID: 10, UserID: 1, ProjectID: "project-a"}
	if !agentUsesRefreshedCodexProvider(agent, []codexProviderRefresh{{UserID: 1, ProjectID: ""}}) {
		t.Fatal("global provider should apply to project agent")
	}
	if !agentUsesRefreshedCodexProvider(agent, []codexProviderRefresh{{UserID: 1, ProjectID: "project-a"}}) {
		t.Fatal("project-scoped provider should apply to matching project agent")
	}
	if agentUsesRefreshedCodexProvider(agent, []codexProviderRefresh{{UserID: 1, ProjectID: "project-b"}}) {
		t.Fatal("project-scoped provider should not apply to a different project")
	}
	if agentUsesRefreshedCodexProvider(agent, []codexProviderRefresh{{UserID: 2, ProjectID: ""}}) {
		t.Fatal("provider for another user should not apply")
	}
}
