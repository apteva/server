package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestAppProxyKeepsOperatorOutOfApplicationUserIdentity(t *testing.T) {
	const targetToken = "principal-target-token"
	t.Setenv("APTEVA_APP_TOKEN", targetToken)
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	const apiKey = "principal-operator-key"
	if _, err := s.store.CreateAPIKey(userID, "principal operator", HashAPIKey(apiKey), "principal"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("principal-operator-session", userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	type observed struct {
		operator, subjectType, subjectID, trusted string
		principal                                 *sdk.TrustedPrincipal
	}
	seen := make(chan observed, 4)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := sdk.PrincipalFromRequest(r)
		if err != nil {
			t.Errorf("verify principal: %v", err)
		}
		seen <- observed{
			operator:    r.Header.Get("X-Apteva-Operator-ID"),
			subjectType: r.Header.Get("X-Apteva-Subject-Type"),
			subjectID:   r.Header.Get("X-Apteva-Subject-ID"),
			trusted:     r.Header.Get(sdk.HeaderTrustedPrincipal),
			principal:   principal,
		}
		if r.URL.Path == "/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 880, AppName: "principal-app", ProjectID: projectID, SidecarURL: sidecar.URL, Token: targetToken})
	mux := http.NewServeMux()
	s.registerAppRuntimeRoutes(mux)
	handler := s.authMiddleware(mux.ServeHTTP)
	for _, auth := range []string{"api-key", "session"} {
		t.Run(auth, func(t *testing.T) {
			for _, route := range []struct {
				method, path, body string
				want               int
			}{
				{method: http.MethodGet, path: "/apps/principal-app/profile?install_id=880", want: http.StatusNoContent},
				{method: http.MethodPost, path: "/apps/principal-app/mcp?install_id=880", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"admin_list","arguments":{}}}`, want: http.StatusOK},
			} {
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				// All identity headers are server-owned. An authenticated operator
				// must not be downgraded by client-supplied delegated-user fields.
				req.Header.Set("X-Apteva-Operator-ID", "999")
				req.Header.Set("X-Apteva-Subject-Type", "user")
				req.Header.Set("X-Apteva-Subject-ID", "spoofed")
				req.Header.Set("X-Apteva-Issuer-App", "spoofed")
				req.Header.Set("X-Apteva-Scopes", `[{"type":"app_user","app":"principal-app","actions":["*"]}]`)
				if auth == "api-key" {
					req.Header.Set("Authorization", "Bearer "+apiKey)
				} else {
					req.AddCookie(&http.Cookie{Name: cookieName, Value: "principal-operator-session"})
				}
				rec := httptest.NewRecorder()
				handler(rec, req)
				if rec.Code != route.want {
					t.Fatalf("%s %s status=%d body=%s", route.method, route.path, rec.Code, rec.Body.String())
				}
				got := <-seen
				if got.operator != itoa64(userID) {
					t.Fatalf("operator=%q want=%q", got.operator, itoa64(userID))
				}
				if got.subjectType != "" || got.subjectID != "" || got.trusted != "" || got.principal != nil {
					t.Fatalf("operator became application user: %+v", got)
				}
			}
		})
	}
}
