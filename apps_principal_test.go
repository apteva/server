package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestAppProxyProvidesVerifiedPrincipalWithoutAuthRoundTrip(t *testing.T) {
	const targetToken = "principal-target-token"
	t.Setenv("APTEVA_APP_TOKEN", targetToken)
	s, userID, projectID := newAppProxyProjectUser(t, ProjectViewer)
	seen := make(chan *sdk.TrustedPrincipal, 1)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := sdk.PrincipalFromRequest(r)
		if err != nil {
			t.Errorf("verify principal: %v", err)
		}
		seen <- principal
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 880, AppName: "principal-app", ProjectID: projectID, SidecarURL: sidecar.URL, Token: targetToken})
	req := httptest.NewRequest(http.MethodGet, "/apps/principal-app/profile?install_id=880", nil)
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	principal := <-seen
	if principal == nil || principal.UserID != userID || principal.SubjectType != "user" || principal.ProjectID != projectID || principal.Email == "" {
		t.Fatalf("principal=%+v", principal)
	}
}
