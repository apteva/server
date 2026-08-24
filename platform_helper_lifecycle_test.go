package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func helperLifecycleRequest(method, path string, userID int64, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-User-ID", fmt.Sprint(userID))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestPlatformHelperReadsDoNotCreateAnInactiveHelper(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("helper-inactive@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}

	statusRec := httptest.NewRecorder()
	s.handlePlatformHelperStatus(statusRec, helperLifecycleRequest(http.MethodGet, "/platform/helper/status", user.ID, ""))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var status platformHelperStatusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Activated || status.State != "inactive" || status.Agent != nil {
		t.Fatalf("unexpected inactive status: %+v", status)
	}

	getRec := httptest.NewRecorder()
	s.handlePlatformHelper(getRec, helperLifecycleRequest(http.MethodGet, "/platform/helper", user.ID, ""))
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("GET helper status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if _, err := s.store.GetPlatformHelper(user.ID); err != sql.ErrNoRows {
		t.Fatalf("read created a Helper row: %v", err)
	}

	capabilitiesRec := httptest.NewRecorder()
	s.handlePlatformHelperCapabilities(capabilitiesRec, helperLifecycleRequest(http.MethodGet, "/platform/helper/capabilities", user.ID, ""))
	if capabilitiesRec.Code != http.StatusConflict {
		t.Fatalf("capabilities status=%d body=%s", capabilitiesRec.Code, capabilitiesRec.Body.String())
	}
	if _, err := s.store.GetPlatformHelper(user.ID); err != sql.ErrNoRows {
		t.Fatalf("capability read created a Helper row: %v", err)
	}
}

func TestPlatformHelperActivationChecksRequirementsBeforeCreatingRow(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("helper-requirements@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}

	withoutProvider := httptest.NewRecorder()
	s.handlePlatformHelperActivate(withoutProvider, helperLifecycleRequest(http.MethodPost, "/platform/helper/activate", user.ID, `{}`))
	if withoutProvider.Code != http.StatusConflict || !strings.Contains(withoutProvider.Body.String(), "provider_required") {
		t.Fatalf("without provider status=%d body=%s", withoutProvider.Code, withoutProvider.Body.String())
	}
	if _, err := s.store.GetPlatformHelper(user.ID); err != sql.ErrNoRows {
		t.Fatalf("failed provider check created a Helper row: %v", err)
	}

	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(user.ID, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	withoutConversations := httptest.NewRecorder()
	s.handlePlatformHelperActivate(withoutConversations, helperLifecycleRequest(http.MethodPost, "/platform/helper/activate", user.ID, `{}`))
	if withoutConversations.Code != http.StatusConflict || !strings.Contains(withoutConversations.Body.String(), "conversations_required") {
		t.Fatalf("without Conversations status=%d body=%s", withoutConversations.Code, withoutConversations.Body.String())
	}
	if _, err := s.store.GetPlatformHelper(user.ID); err != sql.ErrNoRows {
		t.Fatalf("failed Conversations check created a Helper row: %v", err)
	}
	installAttempt := httptest.NewRecorder()
	s.handlePlatformHelperActivate(installAttempt, helperLifecycleRequest(http.MethodPost, "/platform/helper/activate", user.ID, `{"install_conversations":true}`))
	if installAttempt.Code != http.StatusForbidden || !strings.Contains(installAttempt.Body.String(), "admin_required") {
		t.Fatalf("non-admin install status=%d body=%s", installAttempt.Code, installAttempt.Body.String())
	}
	if _, err := s.store.GetPlatformHelper(user.ID); err != sql.ErrNoRows {
		t.Fatalf("failed admin check created a Helper row: %v", err)
	}
}

func TestPlatformHelperDeactivatePreservesRowAndDisablesLazyStart(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("helper-deactivate@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handlePlatformHelperDeactivate(rec, helperLifecycleRequest(http.MethodPost, "/platform/helper/deactivate", user.ID, `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate status=%d body=%s", rec.Code, rec.Body.String())
	}
	persisted, err := s.store.GetPlatformHelper(user.ID)
	if err != nil {
		t.Fatalf("deactivation removed Helper row: %v", err)
	}
	if persisted.ID != helper.ID || platformHelperActivated(persisted) {
		t.Fatalf("deactivated Helper=%+v config=%s", persisted, persisted.Config)
	}
	if _, err := s.ensureMetaAgentRunning(user.ID); err == nil || !strings.Contains(err.Error(), "deactivated") {
		t.Fatalf("deactivated Helper lazy-start err=%v", err)
	}
}

func TestPlatformHelperActivationBindsConversationsAndIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(userID, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	installID := seedAppWithTools(t, s, defaultConversationsApp, "", []string{"send", "list"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	s.platformHelperStarter = func(id int64) (*Agent, error) {
		return s.store.GetPlatformHelper(id)
	}

	activate := func() platformHelperStatusResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handlePlatformHelperActivate(rec, helperLifecycleRequest(http.MethodPost, "/platform/helper/activate", userID, `{}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("activate status=%d body=%s", rec.Code, rec.Body.String())
		}
		var status platformHelperStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	first := activate()
	if !first.Activated || first.ConversationsInstallID != installID || first.Agent == nil {
		t.Fatalf("activation status=%+v", first)
	}
	helper, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	var mcpServerID int64
	if err := s.store.db.QueryRow(`SELECT id FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(installID)).Scan(&mcpServerID); err != nil {
		t.Fatal(err)
	}
	if !containsInt64(helperSelectedGlobalMCPServerIDs(helper), mcpServerID) {
		t.Fatalf("Helper selected MCPs=%v want Conversations=%d", helperSelectedGlobalMCPServerIDs(helper), mcpServerID)
	}
	second := activate()
	if second.Agent == nil || second.Agent.ID != helper.ID {
		t.Fatalf("second activation created another Helper: first=%d second=%+v", helper.ID, second.Agent)
	}
	var bindings int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`, installID, helper.ID).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("bindings=%d err=%v", bindings, err)
	}
}
