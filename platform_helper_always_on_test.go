package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootPlatformHelpersEnabledByDefault(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, "true": true, "0": false, "false": false, "off": false, " NO ": false} {
		if got := bootPlatformHelpersEnabled(v); got != want {
			t.Errorf("bootPlatformHelpersEnabled(%q)=%v want %v", v, got, want)
		}
	}
}

// activatedStoppedHelper returns a server with an activated Helper whose
// core is not running, and a counter of start attempts.
func activatedStoppedHelper(t *testing.T) (*Server, int64, *int) {
	t.Helper()
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(userID, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	installID := seedAppWithTools(t, s, defaultConversationsApp, "", []string{"send", "list"})
	if err := s.registerAppMCP(installID); err != nil {
		t.Fatal(err)
	}
	starts := 0
	s.platformHelperStarter = func(id int64) (*Agent, error) {
		starts++
		return s.store.GetPlatformHelper(id)
	}
	rec := httptest.NewRecorder()
	s.handlePlatformHelperActivate(rec, helperLifecycleRequest(http.MethodPost, "/platform/helper/activate", userID, `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", rec.Code, rec.Body.String())
	}
	starts = 0
	return s, userID, &starts
}

func TestEnsureDefaultRestartsActivatedHelperThatStopped(t *testing.T) {
	s, userID, starts := activatedStoppedHelper(t)
	req := helperLifecycleRequest(http.MethodPost, "/platform/helper/ensure-default", userID, `{}`)
	req.Header.Set("X-Apteva-Operator-ID", fmt.Sprint(userID))
	rec := httptest.NewRecorder()
	s.handlePlatformHelperEnsureDefault(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure-default status=%d body=%s", rec.Code, rec.Body.String())
	}
	if *starts != 1 {
		t.Fatalf("ensure-default start attempts=%d, want 1 for an activated stopped Helper", *starts)
	}
}

func TestEnsureDefaultDoesNotStartDeactivatedHelper(t *testing.T) {
	s, userID, starts := activatedStoppedHelper(t)
	deactivate := httptest.NewRecorder()
	s.handlePlatformHelperDeactivate(deactivate, helperLifecycleRequest(http.MethodPost, "/platform/helper/deactivate", userID, `{}`))
	if deactivate.Code != http.StatusOK {
		t.Fatalf("deactivate status=%d body=%s", deactivate.Code, deactivate.Body.String())
	}
	req := helperLifecycleRequest(http.MethodPost, "/platform/helper/ensure-default", userID, `{}`)
	req.Header.Set("X-Apteva-Operator-ID", fmt.Sprint(userID))
	s.handlePlatformHelperEnsureDefault(httptest.NewRecorder(), req)
	if *starts != 0 {
		t.Fatalf("deactivated Helper was started %d time(s)", *starts)
	}
}

func TestBootMetaAgentsStartsOnlyActivatedHelpers(t *testing.T) {
	s, userID, starts := activatedStoppedHelper(t)
	other, err := s.store.CreateUser("no-helper@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	var started []int64
	s.platformHelperStarter = func(id int64) (*Agent, error) {
		*starts++
		started = append(started, id)
		return s.store.GetPlatformHelper(id)
	}
	s.bootMetaAgents()
	if len(started) != 1 || started[0] != userID {
		t.Fatalf("boot started helpers for users %v, want only %d", started, userID)
	}
}
