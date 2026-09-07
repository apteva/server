package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func setupInterfaceRegistry(t *testing.T, s *Server) string {
	t.Helper()
	ensureTestAdmin(t, s)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.staticMounts = newStaticAppMounts()
	dir := filepath.Join(t.TempDir(), "ui")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("conversations"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `schema: apteva-app/v1
name: conversations
display_name: Conversations
version: 0.9.0
scopes: [global]
provides:
  mcp_tools:
    - { name: send, description: Send a conversation message. }
  ui_components:
    - name: agent-conversations
      label: Conversations
      entry: /ui/Chat.mjs
      slots: [dashboard.build]
      visibility: attached
      supported_sizes: [full]
      default_size: full
runtime:
  kind: static
  static_dir: %s
`, dir)
	}))
	t.Cleanup(manifest.Close)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"apps": []map[string]string{{"name": "conversations", "manifest_url": manifest.URL}}})
	}))
	t.Cleanup(registry.Close)
	t.Setenv("APTEVA_APP_REGISTRY_URL", registry.URL)
	registryCacheMu.Lock()
	registryCache = registryCacheEntry{}
	registryCacheMu.Unlock()
	t.Cleanup(func() { registryCacheMu.Lock(); registryCache = registryCacheEntry{}; registryCacheMu.Unlock() })
	return dir
}

func chooseInterface(t *testing.T, s *Server, userID int64, level string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/auth/preferences", strings.NewReader(fmt.Sprintf(`{"interface_level":%q}`, level)))
	r.Header.Set("X-User-ID", itoa(userID))
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, r)
	return w
}

func TestInterfaceAppsInstallForPersonalAndBusinessMembers(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	user, err := s.store.CreateUser("member@interface.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"personal", "business", "personal"} {
		w := chooseInterface(t, s, user.ID, level)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", level, w.Code, w.Body.String())
		}
		if got := s.store.GetUserInterfaceLevel(user.ID); got != level {
			t.Fatalf("level=%s want %s", got, level)
		}
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("install count=%d err=%v", count, err)
	}
	var scope, status string
	var owner int64
	if err := s.store.db.QueryRow(`SELECT project_id,status,installed_by FROM app_installs`).Scan(&scope, &status, &owner); err != nil {
		t.Fatal(err)
	}
	if scope != "" || status != "running" || owner != 1 {
		t.Fatalf("scope=%s status=%s owner=%d", scope, status, owner)
	}
	if s.store.GetPlatformRole(user.ID) == PlatformAdmin {
		t.Fatal("member was promoted")
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE kind='platform_helper'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected Helper count=%d err=%v", count, err)
	}
}

func TestInterfaceAppsReuseRunningInstallWithoutRegistry(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	id := seedAppWithTools(t, s, defaultConversationsApp, "", []string{"send"})
	t.Setenv("APTEVA_APP_REGISTRY_URL", "http://127.0.0.1:1/unavailable")
	w := chooseInterface(t, s, 1, "personal")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(id)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("bridge count=%d err=%v", n, err)
	}
}

func TestInterfaceAppsFailurePreservesChoiceAndCanRetry(t *testing.T) {
	s := newTestServer(t)
	dir := setupInterfaceRegistry(t, s)
	if err := s.store.SetUserInterfaceLevel(1, "developer"); err != nil {
		t.Fatal(err)
	}
	// The manifest is valid but activation fails after the install row exists.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	w := chooseInterface(t, s, 1, "personal")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if s.store.GetUserInterfaceLevel(1) != "developer" {
		t.Fatal("failed preparation changed interface")
	}
	var originalID int64
	if err := s.store.db.QueryRow(`SELECT id FROM app_installs`).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ready"), 0644); err != nil {
		t.Fatal(err)
	}
	w = chooseInterface(t, s, 1, "personal")
	if w.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", w.Code, w.Body.String())
	}
	var n int
	var id int64
	if err := s.store.db.QueryRow(`SELECT COUNT(*), MIN(id) FROM app_installs`).Scan(&n, &id); err != nil || n != 1 || id != originalID {
		t.Fatalf("retry replaced install: count=%d id=%d original=%d err=%v", n, id, originalID, err)
	}
}

func TestInterfaceAppsDoNotReusePrivateProjectInstall(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	privateID := seedAppWithTools(t, s, defaultConversationsApp, "private-project", []string{"send"})
	w := chooseInterface(t, s, 1, "business")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	var scope string
	if err := s.store.db.QueryRow(`SELECT project_id FROM app_installs WHERE id=?`, privateID).Scan(&scope); err != nil || scope != "private-project" {
		t.Fatalf("private install changed scope=%s err=%v", scope, err)
	}
}

func TestInterfaceAppsConcurrentSelectionsShareOneInstall(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Go(func() { results <- s.ensureInterfaceConversations(1) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestInterfaceAppsDeveloperAndUnrelatedPreferencesDoNotInstall(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	// No supervisor: any attempted preparation would fail.
	for _, level := range []string{"developer", "invalid"} {
		w := chooseInterface(t, s, 1, level)
		want := http.StatusOK
		if level == "invalid" {
			want = http.StatusBadRequest
		}
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d", level, w.Code, want)
		}
	}
	r := httptest.NewRequest(http.MethodPut, "/auth/preferences", strings.NewReader(`{"language":"fr"}`))
	r.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleAuthPreferences(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("language status=%d", w.Code)
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestInterfaceAppsOnboardingCompletionPreparesDefaultBusiness(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	user, err := s.store.CreateUser("new@interface.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/onboarding/complete", nil)
	r.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleCompleteOnboarding(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	user, err = s.store.GetUserByID(user.ID)
	if err != nil || user.OnboardedAt == nil {
		t.Fatalf("onboarding not complete err=%v", err)
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE status='running'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestInterfaceAppsFailedCompletionStaysInOnboarding(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("waiting@interface.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/onboarding/complete", nil)
	r.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleCompleteOnboarding(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	user, err = s.store.GetUserByID(user.ID)
	if err != nil || user.OnboardedAt != nil {
		t.Fatalf("failed preparation completed onboarding: user=%+v err=%v", user, err)
	}
}

func TestInterfaceAppsRespectOperatorAllowlist(t *testing.T) {
	s := newTestServer(t)
	setupInterfaceRegistry(t, s)
	user, err := s.store.CreateUser("restricted@interface.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	policy := defaultAccessPolicy("open")
	policy.Capabilities.AllowedApps = []string{"tasks"}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSetting(accessPolicySettingKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetUserInterfaceLevel(user.ID, "developer"); err != nil {
		t.Fatal(err)
	}
	w := chooseInterface(t, s, user.ID, "personal")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if s.store.GetUserInterfaceLevel(user.ID) != "developer" {
		t.Fatal("restricted selection changed interface")
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("restricted request installed apps: count=%d err=%v", n, err)
	}
}
