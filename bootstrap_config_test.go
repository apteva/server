package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func testBootstrapStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "apteva.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestLoadAptevaConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	passwordPath := filepath.Join(dir, "admin-password")
	if err := os.WriteFile(passwordPath, []byte("secret123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`
server:
  public_url: https://agent.example.com
  registration: locked
managed:
  controller_url: https://control.example.com/
  enrollment_token_file: /run/secrets/apteva-enrollment
  interval_seconds: 45
bootstrap:
  enabled: true
  mark_onboarded: true
  admin:
    email: admin@example.com
    password_file: `+passwordPath+`
  project:
    name: Customer Workspace
    description: Production workspace
    color: "#ff6600"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APTEVA_CONFIG", configPath)
	cfg, loadedPath, err := loadAptevaConfig("")
	if err != nil {
		t.Fatalf("loadAptevaConfig: %v", err)
	}
	if loadedPath != configPath {
		t.Fatalf("loadedPath=%q, want %q", loadedPath, configPath)
	}
	if cfg.Server.PublicURL != "https://agent.example.com" || cfg.Server.Registration != "locked" {
		t.Fatalf("server config not loaded: %+v", cfg.Server)
	}
	if !cfg.Bootstrap.Enabled || !cfg.Bootstrap.MarkOnboarded || cfg.Bootstrap.Admin.PasswordFile != passwordPath {
		t.Fatalf("bootstrap config not loaded: %+v", cfg.Bootstrap)
	}
	if cfg.Managed.ControllerURL != "https://control.example.com" || cfg.Managed.EnrollmentTokenFile != "/run/secrets/apteva-enrollment" || cfg.Managed.IntervalSeconds != 45 {
		t.Fatalf("managed config not loaded: %+v", cfg.Managed)
	}
}

func TestLoadAptevaConfigAutoDetectsDataDir(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "apteva.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  public_url: https://auto.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APTEVA_CONFIG", "")
	cfg, loadedPath, err := loadAptevaConfig(dir)
	if err != nil {
		t.Fatalf("loadAptevaConfig: %v", err)
	}
	if loadedPath != configPath {
		t.Fatalf("loadedPath=%q, want %q", loadedPath, configPath)
	}
	if cfg.Server.PublicURL != "https://auto.example.com" {
		t.Fatalf("public_url=%q", cfg.Server.PublicURL)
	}
}

func TestApplyAptevaBootstrapCreatesAdminProjectAndSkipsOnboarding(t *testing.T) {
	store := testBootstrapStore(t)
	cfg := &aptevaConfig{Bootstrap: aptevaBootstrapConfig{
		Enabled:       true,
		MarkOnboarded: true,
		Admin: aptevaBootstrapAdminConfig{
			Email:    "admin@example.com",
			Password: "secret123",
		},
		Project: aptevaBootstrapProject{
			Name:        "Customer Workspace",
			Description: "Production workspace",
			Color:       "#ff6600",
		},
	}}
	user, err := applyAptevaBootstrap(store, cfg)
	if err != nil {
		t.Fatalf("applyAptevaBootstrap: %v", err)
	}
	if user == nil {
		t.Fatal("expected bootstrapped user")
	}
	got, err := store.GetUserByEmail("admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("secret123")); err != nil {
		t.Fatalf("password hash does not match: %v", err)
	}
	if got.OnboardedAt == nil {
		t.Fatal("expected user to be marked onboarded")
	}
	if role := store.GetPlatformRole(got.ID); role != PlatformAdmin {
		t.Fatalf("role=%q, want admin", role)
	}
	projects, err := store.ListProjects(got.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "Customer Workspace" || projects[0].Color != "#ff6600" {
		t.Fatalf("projects=%+v", projects)
	}
	memberRole, err := store.GetProjectRole(projects[0].ID, got.ID)
	if err != nil {
		t.Fatalf("GetProjectRole: %v", err)
	}
	if memberRole != ProjectOwner {
		t.Fatalf("memberRole=%q, want owner", memberRole)
	}
	if fp := store.GetSetting("bootstrap_fingerprint"); fp == "" {
		t.Fatal("expected bootstrap_fingerprint setting")
	}
}

func TestApplyAptevaBootstrapIsNoopWhenUsersExist(t *testing.T) {
	store := testBootstrapStore(t)
	existing, err := store.CreateUser("existing@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	user, err := applyAptevaBootstrap(store, &aptevaConfig{Bootstrap: aptevaBootstrapConfig{
		Enabled: true,
		Admin:   aptevaBootstrapAdminConfig{Email: "admin@example.com", Password: "secret123"},
	}})
	if err != nil {
		t.Fatalf("applyAptevaBootstrap: %v", err)
	}
	if user != nil {
		t.Fatalf("expected noop, got %+v", user)
	}
	if got, err := store.GetUserByID(existing.ID); err != nil || got.Email != "existing@example.com" {
		t.Fatalf("existing user changed: %+v err=%v", got, err)
	}
}

func TestApplyAptevaBootstrapReadsPasswordFile(t *testing.T) {
	store := testBootstrapStore(t)
	passwordPath := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordPath, []byte("from-file-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := applyAptevaBootstrap(store, &aptevaConfig{Bootstrap: aptevaBootstrapConfig{
		Enabled: true,
		Admin:   aptevaBootstrapAdminConfig{Email: "admin@example.com", PasswordFile: passwordPath},
	}})
	if err != nil {
		t.Fatalf("applyAptevaBootstrap: %v", err)
	}
	got, err := store.GetUserByEmail("admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("from-file-123")); err != nil {
		t.Fatalf("password hash does not match: %v", err)
	}
}
