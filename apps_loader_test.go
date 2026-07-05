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
	s.installedApps = NewInstalledAppsRegistry()

	const (
		projectID    = "proj-auth"
		appToken     = "install-token"
		callerBearer = "Bearer operator-key"
	)

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
