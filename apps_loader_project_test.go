package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func newAppProxyProjectUser(t *testing.T, role ProjectRole) (*Server, int64, string) {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	user, err := s.store.CreateUser("app-proxy-project@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(1, "App Proxy Project", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddProjectMember(project.ID, user.ID, role, 1); err != nil {
		t.Fatal(err)
	}
	s.installedApps = NewInstalledAppsRegistry()
	return s, user.ID, project.ID
}

func TestGlobalAppProxyUsesEffectiveProjectAuthorizationAndHeader(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectViewer)

	var seenProjectID string
	var calls int
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seenProjectID = r.Header.Get("X-Apteva-Project-ID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID:  701,
		AppName:    "global-project-app",
		ProjectID:  "",
		SidecarURL: sidecar.URL,
	})

	request := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/apps/global-project-app/resource?project_id="+projectID, nil)
		req.Header.Set("X-User-ID", itoa64(userID))
		// Ordinary client input must never reach the sidecar. The proxy sets
		// the validated query project after authorization instead.
		req.Header.Set("X-Apteva-Project-ID", "spoofed-project")
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		return rec
	}

	if rec := request(http.MethodGet); rec.Code != http.StatusNoContent {
		t.Fatalf("viewer GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenProjectID != projectID {
		t.Fatalf("sidecar project header=%q want %q", seenProjectID, projectID)
	}
	if calls != 1 {
		t.Fatalf("sidecar calls=%d want 1", calls)
	}

	if rec := request(http.MethodPost); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("viewer mutation reached sidecar: calls=%d", calls)
	}

	if err := s.store.AddProjectMember(projectID, userID, ProjectEditor, 1); err != nil {
		t.Fatal(err)
	}
	if rec := request(http.MethodPost); rec.Code != http.StatusNoContent {
		t.Fatalf("editor POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("editor mutation sidecar calls=%d want 2", calls)
	}
}

func TestGlobalAppProxyKeepsUnscopedAdminFallback(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectViewer)

	var seenProjectID string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenProjectID = r.Header.Get("X-Apteva-Project-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID:  702,
		AppName:    "global-admin-app",
		SidecarURL: sidecar.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/apps/global-admin-app/resource", nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unscoped non-admin status=%d body=%s", rec.Code, rec.Body.String())
	}

	outsider, err := s.store.CreateUser("app-proxy-outsider@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/apps/global-admin-app/resource?project_id="+projectID, nil)
	req.Header.Set("X-User-ID", itoa64(outsider.ID))
	rec = httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("project non-member status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/global-admin-app/resource", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unscoped admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenProjectID != "" {
		t.Fatalf("unscoped global request forwarded project header %q", seenProjectID)
	}
}

func TestAppProxyRejectsConflictingTrustedOrInstallProject(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	s.installedApps.Add(&InstalledApp{
		InstallID:  703,
		AppName:    "global-conflict-app",
		SidecarURL: "http://127.0.0.1:1",
	})
	s.installedApps.Add(&InstalledApp{
		InstallID:  704,
		AppName:    "scoped-conflict-app",
		ProjectID:  projectID,
		SidecarURL: "http://127.0.0.1:1",
	})

	req := httptest.NewRequest(http.MethodGet, "/apps/global-conflict-app/resource?project_id="+projectID, nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	req.Header.Set("X-Apteva-Subject-Type", "user")
	req.Header.Set("X-Apteva-Project-ID", "delegated-other-project")
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegated project conflict status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/scoped-conflict-app/resource?install_id=704&project_id=other-project", nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec = httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("selected install project conflict status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAppProxyDerivesEffectiveProjectFromInstallAndDelegatedKey(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectViewer)

	var seenProjectID string
	var seenQueryProjectID string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenProjectID = r.Header.Get("X-Apteva-Project-ID")
		seenQueryProjectID = r.URL.Query().Get("project_id")
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID:  706,
		AppName:    "scoped-header-app",
		ProjectID:  projectID,
		SidecarURL: sidecar.URL,
	})
	s.installedApps.Add(&InstalledApp{
		InstallID:  707,
		AppName:    "delegated-header-app",
		SidecarURL: sidecar.URL,
	})

	// An explicitly selected project install is sufficient project context;
	// callers do not also need to duplicate it in the query string.
	req := httptest.NewRequest(http.MethodGet, "/apps/scoped-header-app/resource?install_id=706", nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("selected project install status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenProjectID != projectID {
		t.Fatalf("selected install project header=%q want %q", seenProjectID, projectID)
	}
	if seenQueryProjectID != "" {
		t.Fatalf("selected install unexpectedly rewrote query project=%q", seenQueryProjectID)
	}

	// Delegated keys carry their validated project as server-owned identity.
	// Preserve that context for callers that do not repeat project_id.
	req = httptest.NewRequest(http.MethodGet, "/apps/delegated-header-app/resource", nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	req.Header.Set("X-Apteva-Subject-Type", "user")
	req.Header.Set("X-Apteva-Project-ID", projectID)
	rec = httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delegated project fallback status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenProjectID != projectID || seenQueryProjectID != projectID {
		t.Fatalf("delegated project header=%q query=%q want %q", seenProjectID, seenQueryProjectID, projectID)
	}
}

func TestAppProxyInstallPathRoutesPublicWebSocketWithoutQuery(t *testing.T) {
	s, _, projectID := newAppProxyProjectUser(t, ProjectViewer)
	var seenPath, seenProject string
	var sidecarCalls int
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sidecarCalls++
		seenPath = r.URL.Path
		seenProject = r.Header.Get("X-Apteva-Project-ID")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID: 708, AppName: "telephony", ProjectID: projectID, SidecarURL: sidecar.URL,
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{Prefix: "/media/", NoAuth: true}}}},
	})
	s.installedApps.Add(&InstalledApp{
		InstallID: 709, AppName: "other-app", ProjectID: projectID, SidecarURL: sidecar.URL,
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{Prefix: "/media/", NoAuth: true}}}},
	})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	req := httptest.NewRequest(http.MethodGet, "/apps/telephony/_install/708/media/twilio/call/token", nil)
	rec := httptest.NewRecorder()
	apiMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenPath != "/media/twilio/call/token" || seenProject != projectID {
		t.Fatalf("sidecar path=%q project=%q", seenPath, seenProject)
	}
	if sidecarCalls != 1 {
		t.Fatalf("sidecar calls=%d want 1", sidecarCalls)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/telephony/_install/708/media/twilio/call/token?install_id=709", nil)
	rec = httptest.NewRecorder()
	apiMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflicting selector status=%d body=%s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{
		"/apps/telephony/_install/not-a-number/media/twilio/call/token",
		"/apps/telephony/_install/999/media/twilio/call/token",
		"/apps/telephony/_install/709/media/twilio/call/token",
		"/apps/telephony/_install/708/private/call/token",
	} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		rec = httptest.NewRecorder()
		apiMux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("path=%q status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if sidecarCalls != 1 {
		t.Fatalf("rejected selectors reached sidecar: calls=%d", sidecarCalls)
	}
}

func TestGlobalAppProxyInjectsEffectiveProjectIntoMCP(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)

	var seenHeader string
	var seenProjectArg string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Apteva-Project-ID")
		var rpc struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Errorf("decode proxied MCP body: %v", err)
		}
		seenProjectArg, _ = rpc.Params.Arguments["_project_id"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{
		InstallID:  705,
		AppName:    "global-mcp-app",
		SidecarURL: sidecar.URL,
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"example","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/apps/global-mcp-app/mcp?project_id="+projectID, strings.NewReader(body))
	req.Header.Set("X-User-ID", itoa64(userID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("global MCP status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenHeader != projectID {
		t.Fatalf("MCP sidecar project header=%q want %q", seenHeader, projectID)
	}
	if seenProjectArg != projectID {
		t.Fatalf("MCP _project_id=%q want %q", seenProjectArg, projectID)
	}
}
