package main

import (
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func onboardingStatus(t *testing.T, s *Server, userID int64) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/onboarding/status", nil)
	r.Header.Set("X-User-ID", itoa(userID))
	w := httptest.NewRecorder()
	s.handleOnboardingStatus(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOnboardingStatusUsesOwnedWorkspaceAndExistingProvider(t *testing.T) {
	s := newTestServer(t)
	admin := createTestAdmin(t, s)
	other, _ := s.store.CreateUser("other@onboarding.test", "hash")
	if _, err := s.store.CreateProject(other.ID, "Other workspace", "", ""); err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(admin.ID, "My workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before := onboardingStatus(t, s, admin.ID)
	if before["project_id"] != project.ID || before["provider_configured"] != false || before["can_manage_provider"] != true {
		t.Fatalf("unexpected: %+v", before)
	}
	createProviderSelectionFixture(t, s, 2, "OpenAI", project.ID, map[string]any{"OPENAI_API_KEY": "private-test-key"})
	after := onboardingStatus(t, s, admin.ID)
	if after["provider_configured"] != true {
		t.Fatalf("existing provider missing: %+v", after)
	}
	raw, _ := json.Marshal(after)
	if strings.Contains(string(raw), "private-test-key") || len(after) != 5 {
		t.Fatalf("unexpected credential data: %s", raw)
	}
}

func TestOnboardingStatusRecognizesManagedAccessForRestrictedMember(t *testing.T) {
	s := newTestServer(t)
	admin := createTestAdmin(t, s)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{Slug: "onboarding-llm", Name: "Test LLM", BaseURL: "https://example.com", Auth: AppAuthConfig{Headers: map[string]string{"Authorization": "Bearer {{token}}"}}, Runtime: &AppRuntimeConfig{Role: "llm", ProviderKey: "managed"}})
	encrypted, err := Encrypt(s.secret, `{"token":"private-managed-key"}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{UserID: admin.ID, AppSlug: "onboarding-llm", AppName: "Test LLM", Name: "hosted", AuthType: "bearer", EncryptedCreds: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	policy := restrictedTestPolicy()
	if _, err := s.saveAccessPolicy(admin.ID, policy); err != nil {
		t.Fatal(err)
	}
	user, _ := s.store.CreateUser("member@onboarding.test", "hash")
	project, err := s.store.CreateProject(user.ID, "Private", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before := onboardingStatus(t, s, user.ID)
	if before["can_manage_provider"] != false || before["provider_configured"] != false {
		t.Fatalf("unexpected: %+v", before)
	}
	policy.ManagedLLM = ManagedLLMPolicy{ConnectionID: conn.ID, Path: "/chat/completions", Models: []string{"model-a"}}
	if _, err := s.saveAccessPolicy(admin.ID, policy); err != nil {
		t.Fatal(err)
	}
	after := onboardingStatus(t, s, user.ID)
	if after["project_id"] != project.ID || after["provider_configured"] != true || after["can_manage_provider"] != false {
		t.Fatalf("unexpected: %+v", after)
	}
}

func TestOnboardingStatusRejectsMissingWorkspaceAndInvalidRequests(t *testing.T) {
	s := newTestServer(t)
	user := createTestAdmin(t, s)
	for _, tc := range []struct {
		method string
		userID int64
		status int
	}{{http.MethodGet, 0, http.StatusUnauthorized}, {http.MethodPost, user.ID, http.StatusMethodNotAllowed}, {http.MethodGet, user.ID, http.StatusConflict}} {
		r := httptest.NewRequest(tc.method, "/auth/onboarding/status", nil)
		r.Header.Set("X-User-ID", itoa(tc.userID))
		w := httptest.NewRecorder()
		s.handleOnboardingStatus(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s user %d: status=%d body=%s", tc.method, tc.userID, w.Code, w.Body.String())
		}
	}
}

func TestOnboardingStatusFindsStarterForQuotaSafeRetry(t *testing.T) {
	s := newTestServer(t)
	admin := createTestAdmin(t, s)
	user, _ := s.store.CreateUser("retry@onboarding.test", "hash")
	project, err := s.store.CreateProject(user.ID, "Private", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.saveAccessPolicy(admin.ID, restrictedTestPolicy()); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.store.CreateAgentIdempotent(user.ID, "Assistant", "", "learn", "{}", project.ID, "onboarding-starter:"+itoa(user.ID))
	if err != nil {
		t.Fatal(err)
	}
	result := onboardingStatus(t, s, user.ID)
	if result["starter_agent_id"] != float64(agent.ID) {
		t.Fatalf("starter missing at quota: %+v", result)
	}
}

func TestOnboardingPreparationStatusHidesLogs(t *testing.T) {
	s := newTestServer(t)
	m := sdk.Manifest{Name: defaultConversationsApp, Version: "1.0.0"}
	id := seedRunningInstall(t, s, m.Name, "", m, nil)
	for _, tc := range []struct{ status, log, want string }{
		{"pending", "Downloading prebuilt app…", "Downloading Conversations…"},
		{"pending", "Cloning private-repo-with-credentials…", "Fetching Conversations…"},
		{"error", "secret internal failure", "Workspace preparation needs a retry"},
		{"running", "", "Your workspace is ready"},
	} {
		if _, err := s.store.db.Exec("UPDATE app_installs SET status=?, status_message=? WHERE id=?", tc.status, tc.log, id); err != nil {
			t.Fatal(err)
		}
		if got := s.interfacePreparationStatus(); got["message"] != tc.want {
			t.Fatalf("%+v", got)
		}
	}
}
