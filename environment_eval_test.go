package main

import "testing"

func TestSubscribeEnvironmentAgentToAppEvents(t *testing.T) {
	s := newTestServer(t)
	s.appBus = NewAppEventBus()
	s.appEventDispatcher = NewAppEventDispatcher(s)
	user, err := s.store.CreateUser("environment-sub@example.com", "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	environment := &Environment{ID: "environment-1", ProjectID: "source-project"}
	wa := &EnvironmentAgent{AgentID: 42}
	err = s.subscribeEnvironmentAgentToAppEvents(user.ID, environment, wa, []RunAppEventSubscription{
		{App: "media", Topic: "media.completed", ThreadID: "main"},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	subs, err := s.store.ListAllAppEventSubscriptions()
	if err != nil {
		t.Fatalf("list app-event subs: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 app-event subscription, got %d", len(subs))
	}
	if subs[0].AgentID != wa.AgentID || subs[0].Slug != "media:media.completed" || subs[0].ProjectID != environment.ID {
		t.Fatalf("subscription row wrong: %+v", subs[0])
	}
}
