package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelegatedUserKeyMintAndAppMCPPrincipalForwarding(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	user, err := s.store.CreateUser("issuer@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	project, err := s.store.CreateProject(user.ID, "Cloud", "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	authInstallID := seedAppInstall(t, s, user.ID, project.ID, "auth")

	body := `{"subject_id":"auth-user-1","subject_email":"user@example.com","organization_id":"org-1","organization_slug":"default","scopes":[{"type":"app_user","app":"catalog","actions":["catalog_prices_list"]}],"expires_in":3600}`
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/delegated-keys/mint", strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa64(authInstallID))
	w := httptest.NewRecorder()
	s.handleCallbackDelegatedKeys(w, req, []string{"mint"})
	if w.Code != http.StatusOK {
		t.Fatalf("mint expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var mint struct {
		AccessToken string `json:"access_token"`
		KeyPrefix   string `json:"key_prefix"`
		ProjectID   string `json:"project_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &mint); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if !strings.HasPrefix(mint.AccessToken, "uk_") {
		t.Fatalf("expected uk_ token, got prefix %q", mint.AccessToken[:3])
	}
	if mint.ProjectID != project.ID {
		t.Fatalf("expected project %q, got %q", project.ID, mint.ProjectID)
	}

	var seenSubjectID, seenSubjectEmail, seenOrgSlug, seenAuth string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSubjectID = r.Header.Get("X-Apteva-Subject-ID")
		seenSubjectEmail = r.Header.Get("X-Apteva-Subject-Email")
		seenOrgSlug = r.Header.Get("X-Apteva-Organization-Slug")
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`))
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 701, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "catalog-install-token"})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	call := httptest.NewRequest(http.MethodPost, "/apps/catalog/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"catalog_prices_list","arguments":{}}}`))
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Authorization", "Bearer "+mint.AccessToken)
	rec := httptest.NewRecorder()
	apiMux.ServeHTTP(rec, call)
	if rec.Code != http.StatusOK {
		t.Fatalf("delegated app call expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if seenAuth != "Bearer catalog-install-token" {
		t.Fatalf("expected sidecar install auth, got %q", seenAuth)
	}
	if seenSubjectID != "auth-user-1" || seenSubjectEmail != "user@example.com" || seenOrgSlug != "default" {
		t.Fatalf("principal headers not forwarded: subject=%q email=%q org=%q", seenSubjectID, seenSubjectEmail, seenOrgSlug)
	}
}

func TestDelegatedUserKeyBlocksUnscopedMCPAction(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	user, err := s.store.CreateUser("issuer-block@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	project, err := s.store.CreateProject(user.ID, "Cloud", "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	raw := "uk_test_delegated_scope"
	scopes, _ := json.Marshal([]publicClientScope{{Type: "app_user", App: "catalog", Actions: []string{"catalog_prices_list"}}})
	if _, err := s.store.CreateAPIKey(user.ID, "delegated", HashAPIKey(raw), "uk_test", APIKeyCreateOptions{
		Kind:             "delegated_user",
		ProjectID:        project.ID,
		Scopes:           string(scopes),
		ExpiresAt:        time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		IssuerApp:        "auth",
		IssuerInstallID:  1,
		SubjectType:      "user",
		SubjectID:        "auth-user-1",
		SubjectEmail:     "user@example.com",
		OrganizationSlug: "default",
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	var calls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 701, AppName: "catalog", ProjectID: project.ID, SidecarURL: sidecar.URL, Token: "catalog-install-token"})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	call := httptest.NewRequest(http.MethodPost, "/apps/catalog/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"catalog_admin_delete","arguments":{}}}`))
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	apiMux.ServeHTTP(rec, call)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("sidecar should not be called for unscoped action")
	}
}

func seedAppInstall(t *testing.T, s *Server, userID int64, projectID, appName string) int64 {
	t.Helper()
	res, err := s.store.db.Exec(
		`INSERT INTO apps(name, source, manifest_json) VALUES (?, 'test', '{}')`,
		appName,
	)
	if err != nil {
		t.Fatalf("insert app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(
		`INSERT INTO app_installs(app_id, project_id, status, installed_by, permissions_json) VALUES (?, ?, 'running', ?, '[]')`,
		appID, projectID, userID,
	)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	return installID
}
