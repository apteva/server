package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedSecurityAppInstall(t *testing.T, s *Server) int64 {
	t.Helper()
	ensureTestAdmin(t, s)
	res, err := s.store.db.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES('security-app','builtin','{"name":"security-app"}')`)
	if err != nil {
		t.Fatalf("insert app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,installed_by) VALUES(?,'','running',1)`, appID)
	if err != nil {
		t.Fatalf("insert install: %v", err)
	}
	installID, _ := res.LastInsertId()
	return installID
}

func TestAppInstallTokensAreRandomStableAndDataPlaneOnly(t *testing.T) {
	s := newTestServer(t)
	installID := seedSecurityAppInstall(t, s)
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "app_") || len(token) != len("app_")+64 || token == "dev-"+itoa(installID) {
		t.Fatalf("token is not an opaque app capability: %q", token)
	}
	again, err := s.appInstallToken(installID)
	if err != nil || again != token {
		t.Fatalf("app token was not stable: %q err=%v", again, err)
	}

	called := false
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/apps/security-app/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("app data-plane auth failed: called=%v status=%d", called, rec.Code)
	}

	called = false
	req = httptest.NewRequest(http.MethodDelete, "/apps/installs/"+itoa(installID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("app token reached management plane: called=%v status=%d", called, rec.Code)
	}
}

func TestAppInstallTokensCanSubscribeOnlyWithinEventScope(t *testing.T) {
	s := newTestServer(t)
	seedProject(t, s, "project-a")
	seedProject(t, s, "project-b")

	globalID := seedInstall(t, s, "routes-auth-test", "")
	globalToken, err := s.appInstallToken(globalID)
	if err != nil {
		t.Fatal(err)
	}
	projectID := seedInstall(t, s, "storage-auth-test", "project-a")
	projectToken, err := s.appInstallToken(projectID)
	if err != nil {
		t.Fatal(err)
	}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Apteva-App-Install-ID") == "" {
			http.Error(w, "missing install principal", http.StatusUnauthorized)
			return
		}
		projectID := r.URL.Query().Get("project_id")
		var accessErr error
		if projectID == "" {
			accessErr = s.checkFirehoseAccess(r)
		} else {
			accessErr = s.checkProjectAccess(r, projectID)
		}
		if accessErr != nil {
			http.Error(w, accessErr.Error(), http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	request := func(token, path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	if got := request(globalToken, "/app-events/routes-auth-test"); got != http.StatusNoContent {
		t.Fatalf("global app event firehose status=%d, want %d", got, http.StatusNoContent)
	}
	if got := request(projectToken, "/app-events/storage-auth-test?project_id=project-a"); got != http.StatusNoContent {
		t.Fatalf("same-project event stream status=%d, want %d", got, http.StatusNoContent)
	}
	if got := request(projectToken, "/app-events/storage-auth-test?project_id=project-b"); got != http.StatusForbidden {
		t.Fatalf("cross-project event stream status=%d, want %d", got, http.StatusForbidden)
	}
	if got := request(projectToken, "/app-events/storage-auth-test"); got != http.StatusForbidden {
		t.Fatalf("project install firehose status=%d, want %d", got, http.StatusForbidden)
	}
	if got := request(globalToken, "/app-events/routes-auth-test/nested"); got != http.StatusUnauthorized {
		t.Fatalf("nested event route status=%d, want %d", got, http.StatusUnauthorized)
	}
}

func TestLegacyAppTokensAreRejected(t *testing.T) {
	s := newTestServer(t)
	installID := seedSecurityAppInstall(t, s)
	legacy := "dev-" + itoa(installID)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	remote := httptest.NewRequest(http.MethodPost, "/apps/security-app/mcp", nil)
	remote.RemoteAddr = "203.0.113.8:1234"
	remote.Header.Set("Authorization", "Bearer "+legacy)
	remoteRec := httptest.NewRecorder()
	handler(remoteRec, remote)
	if remoteRec.Code != http.StatusUnauthorized {
		t.Fatalf("remote legacy token status=%d", remoteRec.Code)
	}

	local := httptest.NewRequest(http.MethodPost, "/apps/security-app/mcp", nil)
	local.RemoteAddr = "127.0.0.1:1234"
	local.Header.Set("Authorization", "Bearer "+legacy)
	localRec := httptest.NewRecorder()
	handler(localRec, local)
	if localRec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback transition token status=%d", localRec.Code)
	}
}

func TestInternalMCPAndEnvironmentEndpointsRejectRemoteCallers(t *testing.T) {
	s := newTestServer(t)
	for name, endpoint := range map[string]struct {
		handler http.HandlerFunc
		path    string
	}{
		"integration mcp":         {s.handleMCPEndpoint, "/mcp/1"},
		"environment mcp":         {s.handleEnvironmentMCP, "/environment-mcp"},
		"environment app gateway": {s.handleEnvironmentAppGateway, "/environment-app-gateway/e/a/health"},
	} {
		req := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(`{}`))
		req.RemoteAddr = "203.0.113.9:4321"
		rec := httptest.NewRecorder()
		endpoint.handler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d to remote caller", name, rec.Code)
		}
	}
}

func TestDefaultCORSDoesNotReflectOrigins(t *testing.T) {
	cfg := newCORSConfig("")
	h := cfg.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("default CORS reflected untrusted origin: %q", got)
	}
}

func TestLogoutRevokesServerSession(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	token := "session-to-revoke"
	if err := s.store.CreateSession(token, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	rec := httptest.NewRecorder()
	s.handleLogout(rec, req)
	if _, err := s.store.GetSession(token); err == nil {
		t.Fatal("logout left server-side session valid")
	}
}

func TestStoreEnforcesForeignKeys(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.store.CreateAgent(9999, "orphan", "", "autonomous", "{}", ""); err == nil {
		t.Fatal("agent with nonexistent user was accepted")
	}
}

func TestAppProxyRequiresMembershipAndNeverFallsBackAcrossProjects(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	user, err := s.store.CreateUser("viewer@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.store.CreateProject(1, "Private", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: 99, AppName: "private-app", ProjectID: project.ID, SidecarURL: "http://127.0.0.1:1"})

	req := httptest.NewRequest(http.MethodGet, "/apps/private-app/data?project_id="+project.ID, nil)
	req.Header.Set("X-User-ID", itoa(user.ID))
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-member project proxy status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/apps/private-app/data", nil)
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unscoped request selected project install: status=%d", rec.Code)
	}
}

func TestWebhookFailsClosedWhenHMACSecretCannotDecrypt(t *testing.T) {
	s := newTestServer(t)
	s.secret = []byte("0123456789abcdef0123456789abcdef")
	sub := &Subscription{ID: "sub-1", Enabled: true, AgentID: 1}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/token", strings.NewReader(`{"ok":true}`))
	rec := httptest.NewRecorder()
	s.handleSubscriptionWebhook(rec, req, sub, "not-valid-ciphertext")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("decrypt failure status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestForwardedClientIPRequiresTrustedProxy(t *testing.T) {
	t.Setenv("APTEVA_TRUST_PROXY_HEADERS", "")
	remote := httptest.NewRequest(http.MethodGet, "/", nil)
	remote.RemoteAddr = "203.0.113.10:1234"
	remote.Header.Set("X-Forwarded-For", "198.51.100.5")
	if got := clientIP(remote); got != "203.0.113.10" {
		t.Fatalf("untrusted proxy spoofed client ip: %q", got)
	}
	local := httptest.NewRequest(http.MethodGet, "/", nil)
	local.RemoteAddr = "127.0.0.1:1234"
	local.Header.Set("X-Forwarded-For", "198.51.100.5")
	if got := clientIP(local); got != "198.51.100.5" {
		t.Fatalf("trusted local proxy client ip=%q", got)
	}
}

func TestTelemetryRetentionConfiguration(t *testing.T) {
	t.Setenv("TELEMETRY_RETENTION", "7d")
	if got := telemetryRetentionFromEnv(); got != 7*24*time.Hour {
		t.Fatalf("retention=%s", got)
	}
	t.Setenv("TELEMETRY_RETENTION", "off")
	if got := telemetryRetentionFromEnv(); got != 0 {
		t.Fatalf("disabled retention=%s", got)
	}
}

func TestRunningAgentWaitIsSingleOwner(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	running := &runningAgent{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- running.wait()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent wait returned %v", err)
		}
	}
}

func TestInstalledAppsRemoveKeepsOtherProjectInstallIndexed(t *testing.T) {
	registry := NewInstalledAppsRegistry()
	registry.Add(&InstalledApp{InstallID: 1, AppName: "storage", ProjectID: "one"})
	registry.Add(&InstalledApp{InstallID: 2, AppName: "storage", ProjectID: "two"})
	registry.Remove(2)
	if got := registry.GetByName("storage"); got == nil || got.InstallID != 1 {
		t.Fatalf("remaining install was lost from name index: %#v", got)
	}
}

func TestAPIRequestBodyLimitRejectsOversizedManagementRequest(t *testing.T) {
	called := false
	handler := limitAPIRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/apps/install", strings.NewReader("{}"))
	req.ContentLength = defaultAPIRequestLimit + 1
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request called=%v status=%d", called, rec.Code)
	}
}
