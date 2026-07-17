package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestAppProxy_NoAuthRoutePreservesCallerAuthorization(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()

	const (
		projectID    = "proj-auth"
		appToken     = "install-token"
		callerBearer = "Bearer user-jwt"
	)

	var seenAuth string
	var seenAppToken string
	var seenOriginalAuth string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAppToken = r.Header.Get("X-Apteva-App-Token")
		seenOriginalAuth = r.Header.Get("X-Apteva-Original-Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()

	s.installedApps.Add(&InstalledApp{
		InstallID:  545,
		AppName:    "auth",
		ProjectID:  projectID,
		SidecarURL: sidecar.URL,
		Token:      appToken,
		Manifest: sdk.Manifest{Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{
			{Method: http.MethodGet, Prefix: "/me", NoAuth: true},
		}}},
	})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	req := httptest.NewRequest(http.MethodGet, "/apps/auth/me?project_id="+projectID, nil)
	req.Header.Set("Authorization", callerBearer)
	w := httptest.NewRecorder()
	apiMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if seenAuth != callerBearer {
		t.Fatalf("Authorization = %q, want caller bearer", seenAuth)
	}
	if seenAppToken != appToken {
		t.Fatalf("X-Apteva-App-Token = %q, want install token", seenAppToken)
	}
	if seenOriginalAuth != callerBearer {
		t.Fatalf("X-Apteva-Original-Authorization = %q, want caller bearer", seenOriginalAuth)
	}
}

func TestAppProxy_PrivateRouteStillUsesInstallAuthorization(t *testing.T) {
	s := newTestServer(t)
	apiKey := testPrivateAPIKey(t, s)
	s.installedApps = NewInstalledAppsRegistry()

	const (
		projectID = "proj-auth"
		appToken  = "install-token"
	)
	callerBearer := "Bearer " + apiKey

	var seenAuth string
	var seenAppToken string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAppToken = r.Header.Get("X-Apteva-App-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()

	s.installedApps.Add(&InstalledApp{
		InstallID:  545,
		AppName:    "auth",
		ProjectID:  projectID,
		SidecarURL: sidecar.URL,
		Token:      appToken,
	})

	apiMux := http.NewServeMux()
	s.registerAppRuntimeRoutes(apiMux)
	req := httptest.NewRequest(http.MethodGet, "/apps/auth/admin/users?project_id="+projectID, nil)
	req.Header.Set("Authorization", callerBearer)
	w := httptest.NewRecorder()
	apiMux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if seenAuth != "Bearer "+appToken {
		t.Fatalf("Authorization = %q, want install token", seenAuth)
	}
	if seenAppToken != "" {
		t.Fatalf("X-Apteva-App-Token = %q, want empty on private route", seenAppToken)
	}
}

func TestStartupRegistryPreloadAndPruneFailedResume(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "instances", Version: "0.4.19"}
	installID := seedInstallWithBindings(t, s, "instances", manifest, nil)
	if _, err := s.store.db.Exec(
		`UPDATE app_installs SET sidecar_url_override='http://127.0.0.1:45678' WHERE id=?`,
		installID,
	); err != nil {
		t.Fatal(err)
	}

	// Boot preloads persisted routes before sidecars resume, so an earlier
	// caller can distinguish "starting" from "not bound".
	s.LoadInstalledApps()
	entry := s.installedApps.Get(installID)
	if entry == nil || entry.AppName != "instances" || entry.SidecarURL != "http://127.0.0.1:45678" {
		t.Fatalf("preloaded entry = %+v", entry)
	}

	// If the actual process resume fails, the optimistic route is removed.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	s.PruneInactiveInstalledApps()
	if got := s.installedApps.Get(installID); got != nil {
		t.Fatalf("failed runtime remained registered: %+v", got)
	}
}
