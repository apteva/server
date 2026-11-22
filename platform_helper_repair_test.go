package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFakeHelperCoreProcess is the stand-in core that fakeHelperCore starts.
// It serves API_PORT until killed: a core version on /status, no threads on
// /threads and an empty JSON object for everything else.
func TestFakeHelperCoreProcess(t *testing.T) {
	if os.Getenv("APTEVA_FAKE_HELPER_CORE") != "1" {
		t.Skip("runs only as the fake Helper core")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("API_PORT"))
	if err != nil {
		os.Exit(1)
	}
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path := os.Getenv("APTEVA_FAKE_HELPER_CORE_LOG"); path != "" && r.URL.Path != "/status" {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = fmt.Fprintf(f, "%s %s\n", r.Method, r.URL.Path)
				_ = f.Close()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"core_version":"test","uptime_seconds":1}`))
		case "/threads":
			_, _ = w.Write([]byte("[]"))
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	os.Exit(0)
}

// fakeHelperCore returns a core command that runs this test binary as a core
// listening on its allocated port, so the real Helper start path can run, and
// the file where that core records the requests it receives.
func fakeHelperCore(t *testing.T) (string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-core")
	requests := filepath.Join(dir, "requests.log")
	body := fmt.Sprintf("#!/bin/sh\nexport APTEVA_FAKE_HELPER_CORE=1\nexport APTEVA_FAKE_HELPER_CORE_LOG='%s'\nexec '%s' -test.run='^TestFakeHelperCoreProcess$'\n", requests, exe)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, requests
}

func TestEnsureMetaAgentRunningSavesGatewayVersionAndDoesNotRestart(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	s.agents.coreCmd, _ = fakeHelperCore(t)
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		t.Fatalf("start Helper: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(helper.ID) })

	stored, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	if version, _ := helperConfigMap(stored)["helper_setup_gateway_version"].(float64); version != 2 {
		t.Fatalf("saved helper_setup_gateway_version=%v, want 2 after a fresh start", helperConfigMap(stored)["helper_setup_gateway_version"])
	}

	s.agents.mu.RLock()
	first := s.agents.processes[helper.ID]
	s.agents.mu.RUnlock()
	if _, err := s.ensureMetaAgentRunning(userID); err != nil {
		t.Fatalf("read running Helper: %v", err)
	}
	s.agents.mu.RLock()
	second := s.agents.processes[helper.ID]
	s.agents.mu.RUnlock()
	if first == nil || second != first {
		t.Fatal("a later read restarted a Helper whose runtime config was already current")
	}
}

// legacyHelper strips what explicit activation writes, leaving a Helper like
// those created before it existed: no activation key, no Conversations link
// and no Conversations tools.
func legacyHelper(t *testing.T, s *Server, userID int64) *Agent {
	t.Helper()
	helper, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := helperConfigMap(helper)
	delete(cfg, helperActivatedKey)
	cfg[helperGlobalMCPServerIDsKey] = []int64{}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	helper.Config = string(raw)
	// Compile the tool list from that empty selection, as an old Helper had it.
	if _, err := s.ensurePlatformHelperRuntimeConfig(helper); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateAgent(helper); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`DELETE FROM app_agent_bindings WHERE agent_id=?`, helper.ID); err != nil {
		t.Fatal(err)
	}
	return helper
}

func ensureDefaultHelper(t *testing.T, s *Server, userID int64) {
	t.Helper()
	req := helperLifecycleRequest(http.MethodPost, "/platform/helper/ensure-default", userID, `{}`)
	req.Header.Set("X-Apteva-Operator-ID", fmt.Sprint(userID))
	rec := httptest.NewRecorder()
	s.handlePlatformHelperEnsureDefault(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure-default status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEnsureDefaultLinksConversationsToLegacyHelper(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	helper := legacyHelper(t, s, userID)
	installID, mcpServerID, ok := s.platformHelperConversationsInstall(userID)
	if !ok || mcpServerID <= 0 {
		t.Fatalf("Conversations install=%d mcp=%d ok=%v", installID, mcpServerID, ok)
	}
	if !platformHelperActivated(helper) || s.platformHelperConversationsLinked(userID) {
		t.Fatal("fixture should be an activated Helper without a Conversations link")
	}

	for call := 1; call <= 2; call++ {
		ensureDefaultHelper(t, s, userID)
		var bindings int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE install_id=? AND agent_id=? AND enabled=1`,
			installID, helper.ID).Scan(&bindings); err != nil || bindings != 1 {
			t.Fatalf("call %d: Conversations links=%d err=%v, want 1", call, bindings, err)
		}
		stored, err := s.store.GetPlatformHelper(userID)
		if err != nil {
			t.Fatal(err)
		}
		cfg := helperConfigMap(stored)
		selected, _ := cfg[helperGlobalMCPServerIDsKey].([]any)
		matches := 0
		for _, id := range selected {
			if value, _ := id.(float64); int64(value) == mcpServerID {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("call %d: Helper MCP selection=%v, want Conversations %d once", call, selected, mcpServerID)
		}
		if activated, present := cfg[helperActivatedKey].(bool); !present || !activated {
			t.Fatalf("call %d: helper_activated=%v present=%v, want an explicit true", call, activated, present)
		}
	}
}

func TestEnsureDefaultGivesRunningLegacyHelperConversationsTools(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	var requests string
	s.agents.coreCmd, requests = fakeHelperCore(t)
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		t.Fatalf("start Helper: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(helper.ID) })
	legacyHelper(t, s, userID)
	s.agents.mu.RLock()
	before := s.agents.processes[helper.ID]
	s.agents.mu.RUnlock()
	_ = os.Remove(requests)

	ensureDefaultHelper(t, s, userID)
	if !s.platformHelperConversationsLinked(userID) {
		t.Fatal("running legacy Helper was not linked to Conversations")
	}
	s.agents.mu.RLock()
	after := s.agents.processes[helper.ID]
	s.agents.mu.RUnlock()
	if before == nil || after != before {
		t.Fatal("linking Conversations restarted the running Helper")
	}
	seen, _ := os.ReadFile(requests)
	if !strings.Contains(string(seen), "PUT /config") {
		t.Fatalf("running core never received its new tools; core requests:\n%s", seen)
	}
}

func TestEnsureDefaultDoesNotLinkDeactivatedHelper(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	rec := httptest.NewRecorder()
	s.handlePlatformHelperDeactivate(rec, helperLifecycleRequest(http.MethodPost, "/platform/helper/deactivate", userID, `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate status=%d body=%s", rec.Code, rec.Body.String())
	}
	helper, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`DELETE FROM app_agent_bindings WHERE agent_id=?`, helper.ID); err != nil {
		t.Fatal(err)
	}

	ensureDefaultHelper(t, s, userID)
	var bindings int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_agent_bindings WHERE agent_id=? AND enabled=1`, helper.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("deactivated Helper links=%d err=%v, want 0", bindings, err)
	}
	stored, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	if platformHelperActivated(stored) {
		t.Fatal("ensure-default reactivated a Helper the user turned off")
	}
}
