package main

import (
	"os"
	"testing"
	"time"
)

func TestSessionRoundtrip(t *testing.T) {
	// Create temp DB
	f, _ := os.CreateTemp("", "test-*.db")
	f.Close()
	defer os.Remove(f.Name())

	store, err := NewStore(f.Name())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// Create a user first
	user, err := store.CreateUser("testuser", "hashedpw")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create session
	token := "test-token-abc123"
	expires := time.Now().Add(24 * time.Hour)
	err = store.CreateSession(token, user.ID, expires)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Get session back immediately
	userID, err := store.GetSession(token)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if userID != user.ID {
		t.Fatalf("expected user ID %d, got %d", user.ID, userID)
	}

	// Get session again — should still work (not deleted)
	userID2, err := store.GetSession(token)
	if err != nil {
		t.Fatalf("GetSession second call failed: %v", err)
	}
	if userID2 != user.ID {
		t.Fatalf("expected user ID %d on second call, got %d", user.ID, userID2)
	}
}

func TestSessionExpired(t *testing.T) {
	f, _ := os.CreateTemp("", "test-*.db")
	f.Close()
	defer os.Remove(f.Name())

	store, err := NewStore(f.Name())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	user, _ := store.CreateUser("testuser", "hashedpw")

	// Create already-expired session
	token := "expired-token"
	expires := time.Now().Add(-1 * time.Hour)
	store.CreateSession(token, user.ID, expires)

	// Should fail
	_, err = store.GetSession(token)
	if err == nil {
		t.Fatal("expected error for expired session, got nil")
	}
}
