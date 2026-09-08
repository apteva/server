package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkflowOperatorIdentityCannotBeMintedByAgentGateway(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("workflow-operator@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	const key = "workflow-operator-key"
	if _, err := s.store.CreateAPIKey(user.ID, "workflow test", HashAPIKey(key), "workflow"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("operator-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"forged", "gateway", "api_key", "session"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/apps/computer/workflow-constraints", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("X-Apteva-Operator-ID", "999")
			switch kind {
			case "gateway":
				req.Header.Set("X-Agent-Secret", s.instanceSecret)
				req.Header.Set("X-Apteva-MCP-User-ID", itoa(user.ID))
			case "api_key":
				req.Header.Set("Authorization", "Bearer "+key)
			case "session":
				req.AddCookie(&http.Cookie{Name: cookieName, Value: "operator-session"})
			}
			called := false
			operator := ""
			w := httptest.NewRecorder()
			s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
				called = true
				operator = r.Header.Get("X-Apteva-Operator-ID")
				w.WriteHeader(204)
			})(w, req)
			if kind == "forged" {
				if called || w.Code != 401 {
					t.Fatalf("forged identity accepted: %d", w.Code)
				}
				return
			}
			if !called || w.Code != 204 {
				t.Fatalf("auth failed: %d %s", w.Code, w.Body.String())
			}
			want := ""
			if kind == "api_key" || kind == "session" {
				want = itoa(user.ID)
			}
			if operator != want {
				t.Fatalf("operator=%q want=%q", operator, want)
			}
		})
	}
}

func TestWorkflowOperatorIdentitySurvivesAuthenticatedAppProxy(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	const key = "workflow-proxy-key"
	if _, err := s.store.CreateAPIKey(userID, "workflow proxy", HashAPIKey(key), "workflow"); err != nil {
		t.Fatal(err)
	}
	var seenOperator, seenAuth string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOperator, seenAuth = r.Header.Get("X-Apteva-Operator-ID"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 917, AppName: "computer", SidecarURL: sidecar.URL, Token: "sidecar-test-token"})
	mux := http.NewServeMux()
	s.registerAppRuntimeRoutes(mux)
	handler := s.authMiddleware(mux.ServeHTTP)
	for _, operator := range []bool{true, false} {
		req := httptest.NewRequest("POST", "/apps/computer/workflow-constraints?project_id="+projectID, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Apteva-Operator-ID", "999")
		if operator {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			req.Header.Set("X-Agent-Secret", s.instanceSecret)
			req.Header.Set("X-Apteva-MCP-User-ID", itoa(userID))
		}
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("proxy status=%d body=%s", w.Code, w.Body.String())
		}
		want := ""
		if operator {
			want = itoa(userID)
		}
		if seenOperator != want || seenAuth != "Bearer sidecar-test-token" {
			t.Fatalf("incorrect sidecar authority: operator=%q want=%q authenticated=%v", seenOperator, want, seenAuth == "Bearer sidecar-test-token")
		}
	}
}
