package main

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthReauthProviderErrorPreservesActiveConnection(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.CreateUser("oauth-reauth@test.local", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conn, err := store.CreateConnectionExt(ConnectionInput{
		UserID:   1,
		AppSlug:  "google-sheets",
		AppName:  "Google Sheets",
		Name:     "Google Sheets",
		AuthType: "oauth2",
		Status:   "active",
	})
	if err != nil {
		t.Fatalf("CreateConnectionExt: %v", err)
	}
	state, err := store.mintOAuthState(1, conn.ID, conn.AppSlug, "", time.Minute, 0, "", oauthStatePurposeReauth)
	if err != nil {
		t.Fatalf("mintOAuthState: %v", err)
	}
	s := &Server{store: store}

	req := httptest.NewRequest("GET", "/oauth/local/callback?state="+state+"&error=access_denied", nil)
	rec := httptest.NewRecorder()
	s.handleLocalOAuthCallback(rec, req)

	got, _, err := store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active connection to survive failed re-auth, got %q", got.Status)
	}
}

func TestOAuthConnectProviderErrorMarksPendingConnectionFailed(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.CreateUser("oauth-connect@test.local", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conn, err := store.CreateConnectionExt(ConnectionInput{
		UserID:   1,
		AppSlug:  "google-sheets",
		AppName:  "Google Sheets",
		Name:     "Google Sheets",
		AuthType: "oauth2",
		Status:   "pending",
	})
	if err != nil {
		t.Fatalf("CreateConnectionExt: %v", err)
	}
	state, err := store.mintOAuthState(1, conn.ID, conn.AppSlug, "", time.Minute, 0, "", oauthStatePurposeConnect)
	if err != nil {
		t.Fatalf("mintOAuthState: %v", err)
	}
	s := &Server{store: store}

	req := httptest.NewRequest("GET", "/oauth/local/callback?state="+state+"&error=access_denied", nil)
	rec := httptest.NewRecorder()
	s.handleLocalOAuthCallback(rec, req)

	got, _, err := store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Status != "failed" {
		t.Fatalf("expected pending connection to be failed after connect error, got %q", got.Status)
	}
}
