package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppCallbackUserSession(t *testing.T) {
	s := newTestServer(t)
	installID := seedAppWithTools(t, s, "session-app", "", nil)
	user, err := s.store.CreateUser("app-session-member@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []struct {
		name   string
		expiry time.Time
	}{
		{"active", time.Now().Add(time.Hour)}, {"expired", time.Now().Add(-time.Hour)},
	} {
		if err := s.store.CreateSession(session.name, user.ID, session.expiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreatePendingMFASession("pending", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, session, path string
		status                     int
		user                       int64
	}{
		{"request user", token, "active", "/apps/callback/agents", 200, user.ID},
		{"legacy service", token, "", "/apps/callback/agents", 200, 1},
		{"invalid session", token, "invalid", "/apps/callback/agents", 401, 0},
		{"expired session", token, "expired", "/apps/callback/agents", 401, 0},
		{"pending mfa", token, "pending", "/apps/callback/agents", 401, 0},
		{"session alone", "", "active", "/apps/callback/agents", 401, 0},
		{"invalid app", "bad-app", "active", "/apps/callback/agents", 401, 0},
		{"management forbidden", token, "active", "/agents", 401, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			req.Header.Set("X-Apteva-User-Session", tc.session)
			req.Header.Set("X-User-ID", "999")
			rec := httptest.NewRecorder()
			s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
				if getUserID(r) != tc.user {
					t.Errorf("user=%d want=%d", getUserID(r), tc.user)
				}
				if r.Header.Get("X-Apteva-App-Install-ID") != fmt.Sprint(installID) {
					t.Error("lost app identity")
				}
				if r.Header.Get("X-Apteva-User-Session") != "" {
					t.Error("credential retained downstream")
				}
				w.WriteHeader(200)
			})(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
	// Session delegation does not bypass declared app permissions.
	req := httptest.NewRequest("GET", "/apps/callback/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-User-Session", "active")
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleCallbackAgentList)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing app permission status=%d", rec.Code)
	}
	// Ownership still excludes the installer's private Helper.
	adminHelper, err := s.store.GetOrCreatePlatformHelper(1, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	memberHelper, err := s.store.GetOrCreatePlatformHelper(user.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/apps/callback/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-User-Session", "active")
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.callbackAgentForInstall(r, installID, adminHelper.ID); err == nil {
			t.Error("member accessed admin Helper")
		}
		if _, err := s.callbackAgentForInstall(r, installID, memberHelper.ID); err != nil {
			t.Errorf("own Helper: %v", err)
		}
	})(httptest.NewRecorder(), req)
}

func TestAppServiceDeliveryRequiresLiveAgentBinding(t *testing.T) {
	s := newTestServer(t)
	admin := ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "shared-delivery", "", nil)
	member, err := s.store.CreateUser("delivery-member@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(member.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("admin-browser", admin, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check := func(session string, allowed bool) {
		t.Helper()
		req := httptest.NewRequest("POST", "/apps/callback/threads/ensure", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Apteva-User-Session", session)
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			_, err := s.callbackAgentForInstall(r, installID, helper.ID)
			if (err == nil) != allowed {
				t.Errorf("allowed=%t, err=%v", allowed, err)
			}
		})(httptest.NewRecorder(), req)
	}
	check("", false)
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, installID, helper.ID); err != nil {
		t.Fatal(err)
	}
	check("", true)
	check("admin-browser", false) // Binding never broadens a browser user's ownership.
	forged := helperLifecycleRequest("POST", "/apps/callback/threads/ensure", admin, "")
	forged.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(installID))
	if _, err := s.callbackAgentForInstall(forged, installID, helper.ID); err == nil {
		t.Fatal("forged app headers authorized delivery")
	}
	s.store.db.Exec(`UPDATE app_agent_bindings SET enabled=0 WHERE install_id=?`, installID)
	check("", false)
	s.store.db.Exec(`UPDATE app_agent_bindings SET enabled=1 WHERE install_id=?`, installID)
	s.store.db.Exec(`UPDATE app_installs SET project_id='private-project' WHERE id=?`, installID)
	check("", false)
}

func TestAppUserSessionProjectBoundary(t *testing.T) {
	s := newTestServer(t)
	admin := ensureTestAdmin(t, s)
	installID := seedAppWithTools(t, s, "project-session", "", nil)
	member, err := s.store.CreateUser("project-session-member@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	private, err := s.store.CreateProject(admin, "Private", "", "")
	if err != nil {
		t.Fatal(err)
	}
	own, err := s.store.CreateProject(member.ID, "Member", "", "")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(member.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("member-project-session", member.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		session, project string
		allowed          bool
	}{
		{"member-project-session", private.ID, false},
		{"member-project-session", own.ID, true},
		{"", private.ID, false}, // Installer admin cannot bind another user's Helper here.
		{"", own.ID, true},
	} {
		req := httptest.NewRequest("POST", "/apps/callback/threads/ensure", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Apteva-User-Session", tc.session)
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			_, allowed := s.bindCallbackThreadProject(w, r, installID, helper, "test-"+tc.project, tc.project)
			if allowed != tc.allowed {
				t.Errorf("project=%s session=%t allowed=%t", tc.project, tc.session != "", allowed)
			}
		})(httptest.NewRecorder(), req)
	}
}

func TestSharedAppThreadProjectUsesItsOwnBoundScope(t *testing.T) {
	s := newTestServer(t)
	admin := ensureTestAdmin(t, s)
	install := seedAppWithTools(t, s, "scoped-chat", "", nil)
	otherInstall := seedAppWithTools(t, s, "other-chat", "", nil)
	member, err := s.store.CreateUser("scope-member@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(member.ID, platformHelperSystemPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.BindAgentThreadScope(helper.ID, "chat-test", "member-project", install); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{install, otherInstall} {
		if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES(?,?,1)`, id, helper.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreateSession("scope-admin", admin, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		install int64
		session string
		want    string
	}{
		{install, "", "member-project"}, {otherInstall, "", ""}, {install, "scope-admin", ""},
	} {
		token, err := s.appInstallToken(tc.install)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/apps/scoped-chat/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Apteva-User-Session", tc.session)
		// MCP proxy requests have app service auth, never browser callback delegation.
		if tc.session != "" {
			req.URL.Path = "/apps/callback/agents"
		}
		req.Header.Set("X-Apteva-Caller-Agent", fmt.Sprint(helper.ID))
		req.Header.Set("X-Apteva-Caller-Thread", "chat-test")
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			got, err := s.appMCPThreadProject(r)
			if err != nil || got != tc.want {
				t.Errorf("project=%q want=%q err=%v", got, tc.want, err)
			}
		})(httptest.NewRecorder(), req)
	}
	s.store.db.Exec(`UPDATE app_agent_bindings SET enabled=0 WHERE install_id=?`, install)
	token, _ := s.appInstallToken(install)
	req := httptest.NewRequest("POST", "/apps/scoped-chat/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Apteva-Caller-Agent", fmt.Sprint(helper.ID))
	req.Header.Set("X-Apteva-Caller-Thread", "chat-test")
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if project, err := s.appMCPThreadProject(r); err != nil || project != "" {
			t.Errorf("revoked scope=%q err=%v", project, err)
		}
	})(httptest.NewRecorder(), req)
}
