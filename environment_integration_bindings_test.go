package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestBindEnvironmentIntegrationMocksCreatesAppOwnedConnection(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	user, err := store.CreateUser("env-mock-bindings@example.com", "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	s := &Server{
		store:         store,
		secret:        []byte("0123456789abcdef0123456789abcdef"),
		installedApps: NewInstalledAppsRegistry(),
	}
	manifest := sdk.Manifest{
		Name: "messaging",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{
				{Role: "email_provider", CompatibleSlugs: []string{"aws-ses"}, Required: true},
			},
		},
	}
	installID := seedRunningInstall(t, s, "messaging", "eval-1", manifest, nil)
	environment := &Environment{
		ID:        "eval-1",
		ProjectID: "eval-1",
		installs: map[string]*localInstall{
			"messaging": {InstallID: installID, AppName: "messaging", ProjectID: "eval-1"},
		},
	}

	if err := s.bindEnvironmentIntegrationMocks(user.ID, environment, []RunIntegrationBinding{{
		App:      "messaging",
		Role:     "email_provider",
		Slug:     "aws-ses",
		AppName:  "AWS SES",
		Name:     "Benchmark Mock SES",
		AuthType: "api_key",
	}}); err != nil {
		t.Fatalf("bind mocks: %v", err)
	}

	bindings := readBindings(t, s, installID)
	connID, ok := asInt64(bindings["email_provider"])
	if !ok || connID <= 0 {
		t.Fatalf("email_provider binding = %#v, want connection id", bindings["email_provider"])
	}
	var (
		projectID         string
		appSlug           string
		createdVia        string
		ownerAppInstallID int64
		autoMCP           int
		encryptedCreds    string
	)
	if err := store.db.QueryRow(
		`SELECT project_id, app_slug, created_via, owner_app_install_id, auto_mcp, encrypted_credentials FROM connections WHERE id = ?`,
		connID,
	).Scan(&projectID, &appSlug, &createdVia, &ownerAppInstallID, &autoMCP, &encryptedCreds); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if projectID != "eval-1" || appSlug != "aws-ses" || createdVia != "app_install" || ownerAppInstallID != installID || autoMCP != 0 {
		t.Fatalf("unexpected connection scope: project=%q slug=%q created_via=%q owner=%d auto_mcp=%d", projectID, appSlug, createdVia, ownerAppInstallID, autoMCP)
	}
	plain, err := Decrypt(s.secret, encryptedCreds)
	if err != nil {
		t.Fatalf("decrypt credentials: %v", err)
	}
	var credentials map[string]string
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	for _, key := range []string{"region", "access_key_id", "secret_access_key"} {
		if credentials[key] == "" {
			t.Fatalf("missing mock aws credential %q in %#v", key, credentials)
		}
	}
}
