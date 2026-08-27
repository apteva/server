package main

import (
	"strings"
	"testing"
)

func TestAgentThreadScopeCannotCrossProjectsOrOwners(t *testing.T) {
	s := newTestServer(t)
	owner, err := s.store.CreateUser("thread-owner@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateUser("thread-other@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(owner.ID, "helper", "help", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.store.BindAgentThreadScope(agent.ID, "chat-one", "project-a", 10)
	if err != nil || !created {
		t.Fatalf("initial bind created=%v err=%v", created, err)
	}
	created, err = s.store.BindAgentThreadScope(agent.ID, "chat-one", "project-a", 11)
	if err != nil || created {
		t.Fatalf("idempotent bind created=%v err=%v", created, err)
	}
	if _, err := s.store.BindAgentThreadScope(agent.ID, "chat-one", "project-b", 10); err == nil || !strings.Contains(err.Error(), "already scoped") {
		t.Fatalf("cross-project rebind err=%v", err)
	}
	if got, err := s.store.AgentThreadProjectForUser(owner.ID, agent.ID, "chat-one"); err != nil || got != "project-a" {
		t.Fatalf("owner lookup=%q err=%v", got, err)
	}
	if got, err := s.store.AgentThreadProjectForUser(other.ID, agent.ID, "chat-one"); err != nil || got != "" {
		t.Fatalf("foreign lookup=%q err=%v", got, err)
	}
	if err := s.store.DeleteAgentThreadScope(agent.ID, "chat-one"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.store.AgentThreadProjectForUser(owner.ID, agent.ID, "chat-one"); err != nil || got != "" {
		t.Fatalf("deleted lookup=%q err=%v", got, err)
	}
}
