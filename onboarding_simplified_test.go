package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOnboardingPreparationDoesNotChooseInterfaceOrComplete(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	user, _ := s.store.CreateUser("prepare-first@test.local", "hash")
	s.store.SetUserInterfaceLevel(user.ID, "developer")
	for range 2 {
		r := helperLifecycleRequest(http.MethodPost, "/auth/onboarding/prepare", user.ID, `{"mode":"ai"}`)
		w := httptest.NewRecorder()
		s.handlePrepareOnboarding(w, r)
		if w.Code != 200 {
			t.Fatalf("prepare: %d %s", w.Code, w.Body.String())
		}
	}
	user, _ = s.store.GetUserByID(user.ID)
	if user.OnboardedAt != nil || s.store.GetUserInterfaceLevel(user.ID) != "developer" {
		t.Fatal("preparation changed onboarding/preferences")
	}
	var count int
	s.store.db.QueryRow("SELECT COUNT(*) FROM app_installs").Scan(&count)
	if count != 1 {
		t.Fatalf("repeated preparation created %d installs", count)
	}
}

func TestOnboardingInterfaceCompletionIsCallerOnlyAndPreservesLaterPreferences(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	user, _ := s.store.CreateUser("finish-first@test.local", "hash")
	other, _ := s.store.CreateUser("finish-other@test.local", "hash")
	s.store.SetUserInterfaceLevel(other.ID, "developer")
	call := func(level string) *httptest.ResponseRecorder {
		r := helperLifecycleRequest(http.MethodPost, "/auth/onboarding/complete", user.ID, `{"interface_level":"`+level+`"}`)
		w := httptest.NewRecorder()
		s.handleCompleteOnboarding(w, r)
		return w
	}
	if w := call("invalid"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	before, _ := s.store.GetUserByID(user.ID)
	if before.OnboardedAt != nil {
		t.Fatal("invalid choice completed onboarding")
	}
	if w := call("personal"); w.Code != 200 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if s.store.GetUserInterfaceLevel(user.ID) != "personal" {
		t.Fatal("selected interface not applied")
	}
	if s.store.GetUserInterfaceLevel(other.ID) != "developer" {
		t.Fatal("another member was changed")
	}
	// A later explicit preference and replay of completion must stay intact.
	s.store.SetUserInterfaceLevel(user.ID, "developer")
	if w := call("business"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if s.store.GetUserInterfaceLevel(user.ID) != "developer" {
		t.Fatal("completion retry replaced existing preference")
	}
}

func TestOnboardingCompletionRollsBackStampOnPreferenceFailure(t *testing.T) {
	s := newTestServer(t)
	user, _ := s.store.CreateUser("rollback-interface@test.local", "hash")
	_, err := s.store.db.Exec(`CREATE TRIGGER fail_onboarding_interface BEFORE UPDATE OF interface_level ON user_preferences BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CompleteOnboardingWithInterface(user.ID, "personal"); err == nil {
		t.Fatal("expected preference failure")
	}
	user, _ = s.store.GetUserByID(user.ID)
	if user.OnboardedAt != nil {
		t.Fatal("failed preference committed completion")
	}
}

func TestProjectPresetInterfaceRecommendationIsExplicitAndReadOnly(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "interface-preview")
	catalog, err := s.projectPresetCatalog(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, preset := range catalog.Presets {
		if !validInterfaceLevel(preset.InterfaceLevel) {
			t.Fatalf("preset %s lacks recommendation", preset.ID)
		}
	}
	before := s.store.GetUserInterfaceLevel(1)
	preview, err := s.compileProjectPresetPreview(context.Background(), 1, "interface-preview", ProjectPresetPreviewRequest{PresetID: "personal-assistant", Description: "Plan my week", InterfaceLevel: "business"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Preset.InterfaceLevel != "personal" || preview.InterfaceLevel != "business" {
		t.Fatal("category/default/override conflated")
	}
	if s.store.GetUserInterfaceLevel(1) != before {
		t.Fatal("preview changed account")
	}
	_, err = s.compileProjectPresetPreview(context.Background(), 1, "interface-preview", ProjectPresetPreviewRequest{PresetID: "personal-assistant", Description: "Plan my week", InterfaceLevel: "invalid"})
	if err == nil {
		t.Fatal("invalid interface accepted")
	}
	legacy := catalog.ByID["personal-assistant"]
	legacy.InterfaceLevel = ""
	if err := validateProjectPreset(legacy); err != nil {
		t.Fatalf("older preset rejected: %v", err)
	}
	envelope := systemPresetEnvelope(catalog.ByID["personal-assistant"])
	if projectPresetFromEnvelope(envelope).InterfaceLevel != "personal" {
		t.Fatal("envelope lost recommendation")
	}
}

func TestWorkspaceSetupHelperRecommendationDoesNotChangePreferences(t *testing.T) {
	s := newTestServer(t)
	user, _ := s.store.CreateUser("helper-interface@test.local", "hash")
	project, _ := s.store.CreateProject(user.ID, "Workspace", "", "")
	before := s.store.GetUserInterfaceLevel(user.ID)
	preview := &ProjectPresetPreview{Preset: ProjectPreset{ID: "personal-assistant"}, InterfaceLevel: "personal"}
	if err := s.rememberOnboardingPreset(user.ID, project.ID, preview); err != nil {
		t.Fatal(err)
	}
	r := helperLifecycleRequest(http.MethodGet, "/projects/"+project.ID+"/setup/session", user.ID, "")
	w := httptest.NewRecorder()
	s.handleProject(w, r)
	var draft workspaceSetupDraft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.InterfaceLevel != "personal" || draft.PresetID != "personal-assistant" || draft.Mode != "ai" {
		t.Fatal(w.Body.String())
	}
	if s.store.GetUserInterfaceLevel(user.ID) != before {
		t.Fatal("applying preset changed account preference")
	}
	draft.InterfaceLevel = "developer"
	raw, _ := json.Marshal(draft)
	put := helperLifecycleRequest(http.MethodPut, r.URL.Path, user.ID, string(raw))
	w = httptest.NewRecorder()
	s.handleProject(w, put)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := s.rememberOnboardingPreset(user.ID, project.ID, preview); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleProject(w, r)
	if !strings.Contains(w.Body.String(), `"interface_level":"developer"`) {
		t.Fatal("Helper overwrote user's selection")
	}
	s.store.MarkUserOnboarded(user.ID)
	preview.Preset.ID = "business-lead-generation"
	if err := s.rememberOnboardingPreset(user.ID, project.ID, preview); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleProject(w, r)
	if !strings.Contains(w.Body.String(), `"preset_id":"personal-assistant"`) {
		t.Fatal("later setup replaced onboarding draft")
	}
}

func TestManualOnboardingDoesNotProvisionConversations(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s) // A working installer must remain unused.
	w := postJSON(t, s.handleRegister, map[string]string{"email": "manual-dev@test.local", "password": "test-password-123"})
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var account struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", `{}`, `{"mode":"manual"}`} {
		r := helperLifecycleRequest(http.MethodPost, "/auth/onboarding/prepare", account.ID, body)
		w = httptest.NewRecorder()
		s.handlePrepareOnboarding(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("manual prepare: %d %s", w.Code, w.Body.String())
		}
	}
	for _, level := range []string{"personal", "business", "developer"} {
		w = chooseInterface(t, s, account.ID, level)
		if w.Code != http.StatusOK {
			t.Fatalf("preferences: %d %s", w.Code, w.Body.String())
		}
	}
	r := helperLifecycleRequest(http.MethodPost, "/auth/onboarding/complete", account.ID, `{"interface_level":"developer"}`)
	w = httptest.NewRecorder()
	s.handleCompleteOnboarding(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	// Registration previously spawned a background installer. Observe long enough
	// for that asynchronous regression to appear against this local static fixture.
	deadline := time.Now().Add(150 * time.Millisecond)
	for {
		var apps, helpers int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&apps); err != nil {
			t.Fatal(err)
		}
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE kind='platform_helper'`).Scan(&helpers); err != nil {
			t.Fatal(err)
		}
		if apps != 0 || helpers != 0 {
			t.Fatalf("manual setup provisioned apps=%d helpers=%d", apps, helpers)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	user, err := s.store.GetUserByID(account.ID)
	if err != nil || user.OnboardedAt == nil {
		t.Fatalf("manual onboarding incomplete: %v", err)
	}
	if s.interfacePreparationStatus()["status"] != "not_installed" {
		t.Fatal("absent app reported as preparing")
	}
}

func TestOnboardingPreparationRejectsUnknownMode(t *testing.T) {
	s := newTestServer(t)
	user, _ := s.store.CreateUser("mode@test.local", "hash")
	for _, body := range []string{`{"mode":"automatic"}`, `{broken`} {
		w := httptest.NewRecorder()
		s.handlePrepareOnboarding(w, helperLifecycleRequest(http.MethodPost, "/auth/onboarding/prepare", user.ID, body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
