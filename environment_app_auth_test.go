package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestEnvironmentSDKAppAuthenticationRetainsPermissionGate(t *testing.T) {
	s := newTestServer(t)
	id := seedSecurityAppInstall(t, s)
	token, err := s.appInstallToken(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		permissions string
		want        int
	}{
		{`[]`, http.StatusForbidden},
		{`["platform.environments.read"]`, http.StatusForbidden},
		{`["platform.environments.manage"]`, http.StatusBadRequest},
	} {
		_, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, tc.permissions, id)
		if err != nil {
			t.Fatal(err)
		}
		// Invalid JSON prevents a real environment spawn; reaching validation
		// proves authentication and the existing permission check both passed.
		req := httptest.NewRequest(http.MethodPost, "/environments", strings.NewReader("{"))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.authMiddleware(s.handleEnvironments)(w, req)
		if w.Code != tc.want {
			t.Fatalf("permissions %s: got %d %s, want %d", tc.permissions, w.Code, w.Body.String(), tc.want)
		}
	}
}

func TestEnvironmentSeedPrivateIdentityOnlyForAuthenticatedSource(t *testing.T) {
	s := newTestServer(t)
	id := seedSecurityAppInstall(t, s)
	_, err := s.store.db.Exec(`UPDATE app_installs SET permissions_json='["platform.environments.call"]' WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(id)
	if err != nil {
		t.Fatal(err)
	}
	var boundID, boundName string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boundID = r.Header.Get(sdk.HeaderBoundCallerInstallID)
		boundName = r.Header.Get(sdk.HeaderBoundCallerAppName)
		if r.Header.Get("X-Apteva-Caller-Agent") != "" {
			t.Error("source app mislabeled as an agent")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
	}))
	defer endpoint.Close()
	environment := &Environment{ID: "test", sourceInstallIDs: map[string]int64{"security-app": id}, installs: map[string]*localInstall{"security-app": {InstallID: id, SidecarURL: endpoint.URL}}}
	for _, sourceID := range []int64{id, id + 1, 0} {
		environment.sourceInstallIDs["security-app"] = sourceID
		req := httptest.NewRequest(http.MethodPost, "/environments/test/seed", strings.NewReader(`{"calls":[{"app":"security-app","tool":"private_tool"}]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		// Client-supplied identity must not affect the server-minted identity.
		req.Header.Set(sdk.HeaderBoundCallerInstallID, "999999")
		w := httptest.NewRecorder()
		s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			if s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsCall) {
				s.handleEnvironmentSeed(w, r, environment)
			}
		})(w, req)
		if w.Code != 200 {
			t.Fatalf("seed: %d %s", w.Code, w.Body.String())
		}
		if sourceID == id {
			if boundID != itoa(id) || boundName != "security-app" {
				t.Fatalf("missing trusted source identity: %s %s", boundID, boundName)
			}
		} else if boundID != "" || boundName != "" {
			t.Fatalf("unrelated source acquired private identity: %s %s", boundID, boundName)
		}
	}
	environment.sourceInstallIDs["security-app"] = id
	if _, err := s.ExecuteSeedPlan(environment, []SeedCall{{App: "security-app", Tool: "public_tool"}}); err != nil {
		t.Fatal(err)
	}
	if boundID != "" || boundName != "" {
		t.Fatal("ordinary seed acquired private app identity")
	}
}

func TestEnvironmentSDKAppRouteScope(t *testing.T) {
	for _, route := range []string{"/environments", "/environments/run", "/environments/run/seed", "/environments/run/snapshot", "/environments/run/agents", "/environments/run/agents/decision", "/environments/run/agent"} {
		if !appTokenRouteAllowed(route) {
			t.Fatalf("SDK route blocked: %s", route)
		}
	}
	for _, route := range []string{"/environments-other", "/environments/", "/environments/migrate-to-app", "/environments/run/start", "/environments/run/stop", "/environments/run/subscriptions", "/environments/run/agents/decision/tool", "/agents", "/settings", "/apps/installs/1"} {
		if appTokenRouteAllowed(route) {
			t.Fatalf("non-SDK management route allowed: %s", route)
		}
	}
}
