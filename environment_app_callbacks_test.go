package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func seedEnvironmentInstall(t *testing.T, s *Server, name, environmentID string, owner int64, manifest sdk.Manifest, bindings map[string]any) int64 {
	t.Helper()
	manifestJSON, _ := json.Marshal(manifest)
	var appID int64
	err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name=?`, name).Scan(&appID)
	if err != nil {
		res, insertErr := s.store.db.Exec(
			`INSERT INTO apps(name,source,manifest_json) VALUES(?,'environment',?)`,
			name, string(manifestJSON),
		)
		if insertErr != nil {
			t.Fatalf("insert app %s: %v", name, insertErr)
		}
		appID, _ = res.LastInsertId()
	}
	bindingsJSON, _ := json.Marshal(bindings)
	permissionsJSON, _ := json.Marshal(manifest.Requires.Permissions)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs
		 (app_id,project_id,status,version,manifest_json,source,installed_by,integration_bindings,permissions_json)
		 VALUES(?,?,'running',?,?,'environment',?,?,?)`,
		appID, environmentID, manifest.Version, string(manifestJSON), owner, string(bindingsJSON), string(permissionsJSON),
	)
	if err != nil {
		t.Fatalf("insert environment install %s: %v", name, err)
	}
	installID, _ := res.LastInsertId()
	return installID
}

func callEnvironmentApp(t *testing.T, s *Server, installID int64, target string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatalf("create install token: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/"+target+"/call",
		strings.NewReader(`{"tool":"workspace_get","input":{}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleAppCallback)(rec, req)
	return rec
}

func TestEnvironmentClonedAppCallsStayBoundAndIsolated(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.environments = NewEnvironmentManager(t.TempDir())
	s.environments.server = s
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users(id,email,password_hash,role) VALUES(2,'environment-owner@test.local','x','user')`); err != nil {
		t.Fatal(err)
	}
	s.environments.creatorUserIDs["env-a"] = 2
	s.environments.creatorUserIDs["env-b"] = 2

	var environmentCalls, productionCalls int
	environmentTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		environmentCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"scope\":\"environment\"}"}]}}`))
	}))
	defer environmentTarget.Close()
	productionTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		productionCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"scope\":\"production\"}"}]}}`))
	}))
	defer productionTarget.Close()

	workspacesManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "workspaces", Version: "1.0.0"}
	productionID := seedEnvironmentInstall(t, s, "workspaces", "source-project", 2, workspacesManifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET source='git' WHERE id=?`, productionID); err != nil {
		t.Fatal(err)
	}
	environmentID := seedEnvironmentInstall(t, s, "workspaces", "env-a", 2, workspacesManifest, nil)
	foreignID := seedEnvironmentInstall(t, s, "workspaces", "env-b", 2, workspacesManifest, nil)
	s.installedApps.Add(&InstalledApp{InstallID: productionID, AppName: "workspaces", ProjectID: "source-project", Manifest: workspacesManifest, SidecarURL: productionTarget.URL, Token: "production-token"})
	s.installedApps.Add(&InstalledApp{InstallID: environmentID, AppName: "workspaces", ProjectID: "env-a", Manifest: workspacesManifest, SidecarURL: environmentTarget.URL, Token: "environment-token"})
	s.installedApps.Add(&InstalledApp{InstallID: foreignID, AppName: "workspaces", ProjectID: "env-b", Manifest: workspacesManifest, SidecarURL: environmentTarget.URL, Token: "foreign-token"})

	codeManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "environment-code", Version: "1.0.0",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "workspaces", Optional: true}},
		},
	}
	callerID := seedEnvironmentInstall(t, s, "environment-code", "env-a", 2, codeManifest, map[string]any{"workspaces": environmentID})

	rec := callEnvironmentApp(t, s, callerID, "workspaces")
	if rec.Code != http.StatusOK {
		t.Fatalf("bound environment call status=%d body=%s", rec.Code, rec.Body.String())
	}
	if environmentCalls != 1 || productionCalls != 0 {
		t.Fatalf("dispatch environment=%d production=%d, want exact environment clone", environmentCalls, productionCalls)
	}

	bindings, _ := json.Marshal(map[string]any{"workspaces": foreignID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, string(bindings), callerID); err != nil {
		t.Fatal(err)
	}
	rec = callEnvironmentApp(t, s, callerID, "workspaces")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "project_id does not match target app install") {
		t.Fatalf("cross-environment call status=%d body=%s", rec.Code, rec.Body.String())
	}

	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings='{}' WHERE id=?`, callerID); err != nil {
		t.Fatal(err)
	}
	rec = callEnvironmentApp(t, s, callerID, "workspaces")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "app not bound: workspaces") || strings.Contains(rec.Body.String(), "install not found") {
		t.Fatalf("missing optional dependency status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeCallerProjectAcceptsOnlyRecordedEnvironmentCreator(t *testing.T) {
	s := newTestServer(t)
	s.environments = NewEnvironmentManager(t.TempDir())
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users(id,email,password_hash,role) VALUES(2,'environment-runtime@test.local','x','user')`); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "environment-runtime-caller", Version: "1.0.0"}
	installID := seedEnvironmentInstall(t, s, manifest.Name, "env-runtime", 2, manifest, nil)
	s.environments.creatorUserIDs["env-runtime"] = 2

	req := httptest.NewRequest(http.MethodGet, "/apps/callback/runtimes", nil)
	rec := httptest.NewRecorder()
	userID, projectID, ok := s.runtimeCallerProject(rec, req, installID, "", ProjectViewer)
	if !ok || userID != 2 || projectID != "env-runtime" {
		t.Fatalf("environment runtime principal ok=%v user=%d project=%q status=%d body=%s", ok, userID, projectID, rec.Code, rec.Body.String())
	}

	delete(s.environments.creatorUserIDs, "env-runtime")
	rec = httptest.NewRecorder()
	if _, _, ok := s.runtimeCallerProject(rec, req, installID, "", ProjectViewer); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("unrecorded environment scope accepted: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInstallBackedEnvironmentRequiresAuthenticatedCreator(t *testing.T) {
	manager := NewEnvironmentManager(t.TempDir())
	_, err := manager.Create(EnvironmentSpec{ID: "ownerless", AppSrcDirs: map[string]string{"code": t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "authenticated creator required") {
		t.Fatalf("ownerless environment error=%v", err)
	}
}
