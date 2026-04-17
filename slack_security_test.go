package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// These tests lock in the two security fixes in slack.go:
//
//  1. handleChannelDisconnect scopes channel lookup to the authenticated
//     user so one tenant cannot delete another tenant's channel by
//     guessing ids (IDOR).
//
//  2. handleSlackConfigure rejects project_ids the caller does not own,
//     so one tenant cannot overwrite another tenant's Slack app tokens
//     (which would both DoS them and let the attacker intercept their
//     inbound messages via a substituted app).

// helper: create a user and return its id.
func mkUser(t *testing.T, s *Server, email string) int64 {
	t.Helper()
	u, err := s.store.CreateUser(email, "hash")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func TestChannelDisconnect_CannotDeleteOtherUsersChannel(t *testing.T) {
	s := newTestServer(t)

	aliceID := mkUser(t, s, "alice@test")
	bobID := mkUser(t, s, "bob@test")

	// Alice creates a channel (belongs to her).
	ch, err := s.store.CreateChannel(aliceID, 0, "slack", "#alice-ops", "enc", "proj_alice")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Bob tries to delete Alice's channel by id.
	req := httptest.NewRequest("DELETE", "/channels/disconnect/"+itoa64(ch.ID), nil)
	req.Header.Set("X-User-ID", itoa64(bobID))
	w := httptest.NewRecorder()
	s.handleChannelDisconnect(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404 for cross-user delete, got %d body=%s", w.Code, w.Body.String())
	}

	// The channel should still exist (the deletion was blocked).
	records, err := s.store.ListChannels(0)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	found := false
	for _, r := range records {
		if r.ID == ch.ID {
			found = true
			break
		}
	}
	if !found {
		// ListChannels(0) returns all channels with instance_id=0 — sanity
		// check it still matches the row we inserted.
		t.Fatal("Alice's channel was deleted by Bob's request — IDOR still open")
	}
}

func TestChannelDisconnect_OwnerCanDeleteOwnChannel(t *testing.T) {
	s := newTestServer(t)
	aliceID := mkUser(t, s, "alice@test")

	ch, err := s.store.CreateChannel(aliceID, 0, "slack", "#alice-ops", "enc", "proj_alice")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Alice deletes her own channel — should succeed.
	req := httptest.NewRequest("DELETE", "/channels/disconnect/"+itoa64(ch.ID), nil)
	req.Header.Set("X-User-ID", itoa64(aliceID))
	w := httptest.NewRecorder()
	s.handleChannelDisconnect(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for self-delete, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestSlackConfigure_RequiresProjectOwnership(t *testing.T) {
	s := newTestServer(t)

	aliceID := mkUser(t, s, "alice@test")
	bobID := mkUser(t, s, "bob@test")

	// Alice owns a project.
	proj, err := s.store.CreateProject(aliceID, "Alice Project", "", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Bob tries to configure Slack for Alice's project.
	body, _ := json.Marshal(map[string]string{
		"project_id": proj.ID,
		"bot_token":  "xoxb-bobs-evil-token",
		"app_token":  "xapp-bobs-evil-token",
	})
	req := httptest.NewRequest("POST", "/api/slack/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", itoa64(bobID))
	w := httptest.NewRecorder()
	s.handleSlackConfigure(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404 for cross-tenant configure, got %d body=%s", w.Code, w.Body.String())
	}

	// No slack_app channel should exist for Alice's project.
	existing, err := s.store.ListChannelsByProject(proj.ID, "slack_app")
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	if len(existing) > 0 {
		t.Fatalf("Bob's tokens were persisted against Alice's project: %+v", existing)
	}

	// And no gateway should be running for that project.
	if gw := getSlackGateway(proj.ID); gw != nil {
		t.Fatal("gateway was started for a project Bob doesn't own")
	}
}

func TestSlackConfigure_MissingProjectIDRejected(t *testing.T) {
	s := newTestServer(t)
	aliceID := mkUser(t, s, "alice@test")

	// Empty project_id — should 400, not crash or fall through.
	body, _ := json.Marshal(map[string]string{
		"project_id": "",
		"bot_token":  "xoxb-x",
		"app_token":  "xapp-x",
	})
	req := httptest.NewRequest("POST", "/api/slack/configure", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", itoa64(aliceID))
	w := httptest.NewRecorder()
	s.handleSlackConfigure(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for missing project_id, got %d", w.Code)
	}
}
