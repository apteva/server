package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// codexConnection creates an active OpenAI Codex connection whose token
// is far from expiry, so the runtime-token handler serves it without
// attempting an upstream refresh.
func codexConnection(t *testing.T, s *Server, legacyProviderID int64) *Connection {
	t.Helper()
	registerRuntimeApp(s, "openai-codex", "openai-codex", map[string]string{
		"OPENAI_CODEX_ACCESS_TOKEN": "{{credentials.access_token}}",
		"OPENAI_CODEX_ACCOUNT_ID":   "{{credentials.account_id}}",
		"OPENAI_CODEX_PROVIDER_ID":  "{{connection.provider_ref}}",
	})
	conn := addConnection(t, s, "openai-codex", "Codex", "", map[string]string{
		"access_token":     "tok-live",
		"account_id":       "acct-42",
		"token_expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if legacyProviderID != 0 {
		if _, err := s.store.db.Exec(
			`UPDATE connections SET legacy_provider_id = ? WHERE id = ?`,
			legacyProviderID, conn.ID); err != nil {
			t.Fatalf("set legacy_provider_id: %v", err)
		}
	}
	return conn
}

func getRuntimeToken(t *testing.T, s *Server, id int64) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/providers/%d/auth/runtime-token", id)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleRuntimeToken(rec, req)
	return rec
}

// TestRuntimeTokenResolvesLegacyProviderID is the migration's sharpest
// edge: a core spawned before the fusion holds the OLD providers.id in
// its environment for the life of the process and keeps calling
// /api/providers/<that id>/auth/runtime-token. After the provider row is
// deleted, that id has to resolve to the migrated connection or the
// agent silently loses token refresh.
func TestRuntimeTokenResolvesLegacyProviderID(t *testing.T) {
	s := runtimeTestServer(t)
	const oldProviderID = 7
	codexConnection(t, s, oldProviderID)

	rec := getRuntimeToken(t, s, oldProviderID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.AccessToken != "tok-live" {
		t.Errorf("access_token = %q, want tok-live", payload.AccessToken)
	}
	// core reads exactly these two fields.
	if payload.AccountID != "acct-42" {
		t.Errorf("account_id = %q, want acct-42", payload.AccountID)
	}
	if payload.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", payload.TokenType)
	}
}

func TestRuntimeTokenResolvesConnectionID(t *testing.T) {
	s := runtimeTestServer(t)
	conn := codexConnection(t, s, 0)

	rec := getRuntimeToken(t, s, conn.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestRuntimeTokenAcceptsOwningRunningCoreKey(t *testing.T) {
	s := runtimeTestServer(t)
	conn := codexConnection(t, s, 0)
	user, err := s.store.CreateUser("runtime-core@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "runtime core", "wait", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	// The connection helper creates its fixture for the default test user.
	// Move it to the agent owner so the handler's normal ownership check is
	// exercised after core-key authentication succeeds.
	if _, err := s.store.db.Exec(`UPDATE connections SET user_id=? WHERE id=?`, user.ID, conn.ID); err != nil {
		t.Fatal(err)
	}
	const coreKey = "core_runtime_token_end_to_end"
	if _, err := s.store.db.Exec(`UPDATE agents SET status='running', core_api_key=?, pid=77, port=7777 WHERE id=?`, coreKey, agent.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/providers/%d/auth/runtime-token", conn.ID), nil)
	req.RemoteAddr = "127.0.0.1:43777"
	req.Header.Set("Authorization", "Bearer "+coreKey)
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleRuntimeToken)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken != "tok-live" {
		t.Fatalf("access_token = %q, want tok-live", payload.AccessToken)
	}
}

// TestRuntimeTokenPrefersProviderRow pins the dual-read precedence for
// this endpoint: while a provider row still exists the behavior must be
// bit-identical to before the fusion, so the connection path can be
// shipped without a flag day.
func TestRuntimeTokenPrefersProviderRow(t *testing.T) {
	s := runtimeTestServer(t)
	conn := codexConnection(t, s, 0)

	// A provider row whose id collides with the connection id, holding a
	// different (non-Codex) auth provider. The handler must route to the
	// provider path — which rejects it — rather than quietly serving the
	// connection's token.
	blob, _ := json.Marshal(map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})
	encrypted, _ := Encrypt(s.secret, string(blob))
	provider, err := s.store.CreateProvider(1, 3, "llm", "Anthropic", encrypted)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if provider.ID != conn.ID {
		t.Skipf("needed colliding ids for this check (provider=%d connection=%d)", provider.ID, conn.ID)
	}

	rec := getRuntimeToken(t, s, provider.ID)
	if rec.Code == http.StatusOK {
		t.Fatal("provider row should have taken precedence and rejected the request")
	}
}

func TestRuntimeTokenUnknownIDIs404(t *testing.T) {
	s := runtimeTestServer(t)
	codexConnection(t, s, 7)

	if rec := getRuntimeToken(t, s, 999); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestRuntimeTokenIgnoresNonRuntimeConnections stops an ordinary
// integration from being addressable as a runtime credential endpoint.
func TestRuntimeTokenIgnoresNonRuntimeConnections(t *testing.T) {
	s := runtimeTestServer(t)
	s.catalog.Register(&AppTemplate{Slug: "stripe", Name: "Stripe"})
	conn := addConnection(t, s, "stripe", "Stripe", "", map[string]string{"token": "sk-stripe"})

	if rec := getRuntimeToken(t, s, conn.ID); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-runtime connection", rec.Code)
	}
}
