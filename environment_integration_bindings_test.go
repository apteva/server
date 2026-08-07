package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedEnvironmentConnection(t *testing.T, s *Server, userID int64, projectID, slug, status string, credentials map[string]string) int64 {
	t.Helper()
	raw, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{UserID: userID, AppSlug: slug, AppName: slug, Name: slug + "-" + status, AuthType: "api_key", EncryptedCreds: encrypted, Status: status, ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	return conn.ID
}

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

	if err := s.bindEnvironmentIntegrationMocks(user.ID, environment, []RuntimeIntegrationBinding{{
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

func TestBindEnvironmentIntegrationMocksExposesCatalogConnectionToAgents(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("env-agent-mock@example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewAppCatalog()
	catalog.Register(&AppTemplate{Slug: "facebook", Name: "Facebook", Tools: []AppToolDef{{Name: "pages_list"}}})
	s := &Server{store: store, secret: []byte("0123456789abcdef0123456789abcdef"), installedApps: NewInstalledAppsRegistry(), catalog: catalog}
	ownerID := seedRunningInstall(t, s, "environments", "proj-1", sdk.Manifest{Name: "environments"}, nil)
	environment := &Environment{ID: "runtime-1", ProjectID: "proj-1", ownerInstallID: ownerID, installs: map[string]*localInstall{}}

	if err := s.bindEnvironmentIntegrationMocks(user.ID, environment, []RuntimeIntegrationBinding{{
		Slug: "facebook", Name: "Mock Facebook", ExposeToAgents: true,
	}}); err != nil {
		t.Fatalf("bind agent mock: %v", err)
	}
	ids := environment.ConnectionIDs()
	if len(ids) != 1 || ids[0] <= 0 {
		t.Fatalf("connection ids = %#v", ids)
	}
	var projectID, slug string
	var owner int64
	if err := store.db.QueryRow(`SELECT project_id, app_slug, owner_app_install_id FROM connections WHERE id=?`, ids[0]).Scan(&projectID, &slug, &owner); err != nil {
		t.Fatal(err)
	}
	if projectID != environment.ID || slug != "facebook" || owner != ownerID {
		t.Fatalf("project=%q slug=%q owner=%d", projectID, slug, owner)
	}
}

func TestBindEnvironmentConnectionsUsesExistingCredentialWithoutMutatingSource(t *testing.T) {
	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	manifest := sdk.Manifest{
		Name: "computer",
		Requires: sdk.Requires{
			Permissions:  []sdk.Permission{sdk.PermConnectionsReadCredentials},
			Integrations: []sdk.IntegrationDep{{Role: "browserbase", Kind: "integration", CompatibleSlugs: []string{"browserbase"}}},
		},
	}
	sourceInstallID := seedRunningInstall(t, s, "computer-source", "proj-1", manifest, nil)
	cloneInstallID := seedRunningInstall(t, s, "computer-clone", "runtime-1", manifest, nil)
	connectionID := seedEnvironmentConnection(t, s, 1, "proj-1", "browserbase", "active", map[string]string{"api_key": "secret-key", "project_id": "bb-project"})
	sourceBindings, _ := json.Marshal(map[string]any{"browserbase": connectionID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, string(sourceBindings), sourceInstallID); err != nil {
		t.Fatal(err)
	}
	environment := &Environment{
		ID:        "runtime-1",
		ProjectID: "proj-1",
		server:    s,
		installs: map[string]*localInstall{
			"computer": {InstallID: cloneInstallID, AppName: "computer", ProjectID: "runtime-1"},
		},
		apps:         map[string]*SandboxAppInstance{},
		agents:       map[int64]*EnvironmentAgent{},
		agentAliases: map[string]int64{},
		managedMCPs:  map[string]*RuntimeManagedMCP{},
	}

	if err := s.bindEnvironmentConnections(1, "proj-1", environment, []sdk.RuntimeConnectionBinding{{App: "computer", Role: "browserbase", ConnectionID: connectionID}}); err != nil {
		t.Fatalf("bind existing connection: %v", err)
	}
	cloneBindings := readBindings(t, s, cloneInstallID)
	if got, ok := asInt64(cloneBindings["browserbase"]); !ok || got != connectionID {
		t.Fatalf("clone browserbase binding = %#v", cloneBindings["browserbase"])
	}
	if got, ok := asInt64(readBindings(t, s, sourceInstallID)["browserbase"]); !ok || got != connectionID {
		t.Fatalf("source install binding changed: %#v", readBindings(t, s, sourceInstallID))
	}

	req := httptest.NewRequest("GET", "/api/apps/callback/connections/"+strconv.FormatInt(connectionID, 10)+"/credentials", nil)
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(cloneInstallID, 10))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleCallbackConnectionCredentials(rec, req, strconv.FormatInt(connectionID, 10))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"api_key":"secret-key"`) {
		t.Fatalf("credential callback status=%d body=%s", rec.Code, rec.Body.String())
	}

	environment.Stop()
	var remaining int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE id=?`, connectionID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("source connection removed during teardown: count=%d err=%v", remaining, err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE id=?`, sourceInstallID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("source install removed during teardown: count=%d err=%v", remaining, err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE id=?`, cloneInstallID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("clone install survived teardown: count=%d err=%v", remaining, err)
	}
}

func TestBindEnvironmentConnectionsRejectsInvalidBindings(t *testing.T) {
	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	s.installedApps = NewInstalledAppsRegistry()
	manifest := sdk.Manifest{Name: "computer", Requires: sdk.Requires{Integrations: []sdk.IntegrationDep{{Role: "browserbase", Kind: "integration", CompatibleSlugs: []string{"browserbase"}}}}}
	cloneInstallID := seedRunningInstall(t, s, "computer-invalid-bindings", "runtime-1", manifest, nil)
	environment := &Environment{ID: "runtime-1", ProjectID: "proj-1", installs: map[string]*localInstall{"computer": {InstallID: cloneInstallID, AppName: "computer", ProjectID: "runtime-1"}}}
	active := seedEnvironmentConnection(t, s, 1, "proj-1", "browserbase", "active", nil)
	disabled := seedEnvironmentConnection(t, s, 1, "proj-1", "browserbase", "disabled", nil)
	foreignProject := seedEnvironmentConnection(t, s, 1, "proj-2", "browserbase", "active", nil)
	incompatible := seedEnvironmentConnection(t, s, 1, "proj-1", "steel", "active", nil)
	if _, err := s.store.CreateUser("other-bindings@example.com", "x"); err != nil {
		t.Fatal(err)
	}
	foreignUser := seedEnvironmentConnection(t, s, 2, "proj-1", "browserbase", "active", nil)

	tests := []struct {
		name    string
		binding sdk.RuntimeConnectionBinding
		want    string
	}{
		{name: "missing app", binding: sdk.RuntimeConnectionBinding{App: "missing", Role: "browserbase", ConnectionID: active}, want: "not installed"},
		{name: "undeclared role", binding: sdk.RuntimeConnectionBinding{App: "computer", Role: "steel", ConnectionID: active}, want: "does not declare"},
		{name: "disabled", binding: sdk.RuntimeConnectionBinding{App: "computer", Role: "browserbase", ConnectionID: disabled}, want: "not active"},
		{name: "foreign project", binding: sdk.RuntimeConnectionBinding{App: "computer", Role: "browserbase", ConnectionID: foreignProject}, want: "another project"},
		{name: "foreign user", binding: sdk.RuntimeConnectionBinding{App: "computer", Role: "browserbase", ConnectionID: foreignUser}, want: "not found"},
		{name: "incompatible", binding: sdk.RuntimeConnectionBinding{App: "computer", Role: "browserbase", ConnectionID: incompatible}, want: "not compatible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.bindEnvironmentConnections(1, "proj-1", environment, []sdk.RuntimeConnectionBinding{tc.binding})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want containing %q", err, tc.want)
			}
			if bindings := readBindings(t, s, cloneInstallID); len(bindings) != 0 {
				t.Fatalf("failed binding mutated clone: %#v", bindings)
			}
		})
	}
}

func TestValidateRuntimeBindingOverlapRejectsRealAndFakeRole(t *testing.T) {
	err := validateRuntimeBindingOverlap(
		[]sdk.RuntimeConnectionBinding{{App: "computer", Role: "browserbase", ConnectionID: 6}},
		[]RuntimeIntegrationBinding{{App: "computer", Role: "browserbase", Slug: "browserbase"}},
	)
	if err == nil || !strings.Contains(err.Error(), "both a real and fake connection") {
		t.Fatalf("overlap error=%v", err)
	}
}
