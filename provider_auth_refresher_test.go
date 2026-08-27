package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderAuthRefreshDefaultsEnabled(t *testing.T) {
	t.Setenv("APTEVA_PROVIDER_AUTH_REFRESH", "")
	if !providerAuthRefreshEnvEnabled() {
		t.Fatal("provider auth refresh should be enabled by default")
	}

	t.Setenv("APTEVA_PROVIDER_AUTH_REFRESH", "off")
	if providerAuthRefreshEnvEnabled() {
		t.Fatal("provider auth refresh should honor explicit off")
	}

	t.Setenv("APTEVA_CODEX_REFRESH_DISABLE_REATTACH", "")
	if !disableCoreReattachForCodexRefresh() {
		t.Fatal("Codex refresh should disable core reattach by default")
	}

	t.Setenv("APTEVA_CODEX_REFRESH_DISABLE_REATTACH", "0")
	if disableCoreReattachForCodexRefresh() {
		t.Fatal("Codex refresh reattach policy should honor explicit 0")
	}
}

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

func TestRunningAgentsUseCodexProviderRecognizesConnection(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("codex-connection-refresh@test.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "agent", "directive", "autonomous", "{}", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	conn := addConnection(t, s, integrationOpenAICodexSlug, "Codex", "project-b", map[string]string{
		"access_token": "access", "refresh_token": "refresh",
		"token_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if _, err := s.store.db.Exec(`UPDATE connections SET auth_type=? WHERE id=?`, connectionAuthTypeDeviceCode, conn.ID); err != nil {
		t.Fatal(err)
	}
	if s.store.RunningAgentsUseCodexProvider() {
		t.Fatal("connection scoped to another project should not match")
	}
	if _, err := s.store.db.Exec(`UPDATE connections SET project_id='' WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	if !s.store.RunningAgentsUseCodexProvider() {
		t.Fatal("global Codex connection should match the running agent")
	}
}

func TestConnectionBackedCodexRefreshPersistsRotatedToken(t *testing.T) {
	s := runtimeTestServer(t)
	conn := addConnection(t, s, integrationOpenAICodexSlug, "Codex", "", map[string]string{
		"access_token": "expired-access", "refresh_token": "refresh-one",
		"token_expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
	})
	if _, err := s.store.db.Exec(`UPDATE connections SET auth_type=? WHERE id=?`, connectionAuthTypeDeviceCode, conn.ID); err != nil {
		t.Fatal(err)
	}

	expiry := time.Now().Add(24 * time.Hour).Unix()
	payload, _ := json.Marshal(map[string]any{"exp": expiry})
	accessToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	var calls atomic.Int32
	handlerErr := make(chan error, 1)
	reportHandlerError := func(err error) {
		select {
		case handlerErr <- err:
		default:
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			reportHandlerError(err)
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-one" {
			reportHandlerError(fmt.Errorf("unexpected refresh form: %v", r.Form))
			http.Error(w, "unexpected form", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-two",
			"account_id":    "account-two",
		})
	}))
	defer upstream.Close()
	previousTokenURL := integrationOpenAICodexTokenURL
	integrationOpenAICodexTokenURL = upstream.URL
	t.Cleanup(func() { integrationOpenAICodexTokenURL = previousTokenURL })
	codexProviderRefreshInFlight.Store(false)

	result := s.refreshExpiringCodexProviders(context.Background(), time.Hour, false)
	select {
	case err := <-handlerErr:
		t.Fatal(err)
	default:
	}
	if result.ProvidersScanned != 0 || result.ConnectionsScanned != 1 || result.ConnectionsRefreshed != 1 || result.ConnectionsFailed != 0 {
		t.Fatalf("unexpected refresh result: %+v", result)
	}
	_, encrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		t.Fatal(err)
	}
	if credentials["access_token"] != accessToken || credentials["refresh_token"] != "refresh-two" || credentials["account_id"] != "account-two" {
		t.Fatalf("rotated credential metadata was not persisted")
	}

	second := s.refreshExpiringCodexProviders(context.Background(), time.Hour, false)
	if second.ConnectionsRefreshed != 0 || calls.Load() != 1 {
		t.Fatalf("fresh connection refreshed again: result=%+v calls=%d", second, calls.Load())
	}
}

func TestConnectionBackedCodexRefreshDoesNotOverwriteInteractiveReauth(t *testing.T) {
	s := runtimeTestServer(t)
	conn := addConnection(t, s, integrationOpenAICodexSlug, "Codex", "", map[string]string{
		"access_token": "expired-access", "refresh_token": "background-refresh",
		"token_expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
	})
	if _, err := s.store.db.Exec(`UPDATE connections SET auth_type=? WHERE id=?`, connectionAuthTypeDeviceCode, conn.ID); err != nil {
		t.Fatal(err)
	}

	interactive := map[string]string{
		"access_token": "interactive-access", "refresh_token": "interactive-refresh",
		"token_expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	interactiveJSON, _ := json.Marshal(interactive)
	interactiveEncrypted, _ := Encrypt(s.secret, string(interactiveJSON))
	handlerErr := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.UpdateConnectionCredentials(conn.ID, interactiveEncrypted); err != nil {
			handlerErr <- err
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "background-access",
			"refresh_token": "background-next",
		})
	}))
	defer upstream.Close()
	previousTokenURL := integrationOpenAICodexTokenURL
	integrationOpenAICodexTokenURL = upstream.URL
	t.Cleanup(func() { integrationOpenAICodexTokenURL = previousTokenURL })

	_, expectedEncrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.refreshOneCodexConnection(conn.ID, expectedEncrypted, time.Hour)
	select {
	case handlerErr := <-handlerErr:
		t.Fatal(handlerErr)
	default:
	}
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("background refresh claimed to overwrite concurrent re-auth")
	}
	_, currentEncrypted, err := s.store.GetConnection(1, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(s.secret, currentEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	current := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &current); err != nil {
		t.Fatal(err)
	}
	if current["access_token"] != "interactive-access" {
		t.Fatalf("interactive re-auth was overwritten")
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
