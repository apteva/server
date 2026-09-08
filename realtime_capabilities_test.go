package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
)

func readyCapabilityFixture(id string, tools, mcps []string) sdk.RealtimeCapabilities {
	return sdk.RealtimeCapabilities{Version: 1, ThreadID: id, Status: "ready", GrantsVerified: true,
		GrantedTools: tools, GrantedMCP: mcps, ConnectedMCP: mcps, RegisteredTools: tools, PresentedTools: tools,
		PresentationVerified: true, SessionGeneration: 2, AppliedSessionGeneration: 2, ConfigurationRevision: 3, AppliedRevision: 3}
}

func TestRealtimeCapabilitiesSeparateGrantsRegistrationAndPresentation(t *testing.T) {
	for _, name := range []string{"legacy_grants_only", "registered_not_presented", "ready_missing_booking", "ready_booking", "verified_none", "stale_revision", "stale_session", "incomplete_ready", "unsupported_version", "wrong_thread", "read_failure", "malformed"} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			a := behaviorAgent(t, s, "autonomous")
			posts := 0
			gets := 0
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer core-key" {
					t.Error("missing core auth")
				}
				if r.Method == http.MethodPost {
					posts++
					if r.URL.Path != "/threads/voice" {
						t.Error("unexpected mutation", r.URL.Path)
					}
					writeJSON(w, map[string]any{"status": "created", "id": "voice", "audio_token": "single-use-token"})
					return
				}
				if r.Method != http.MethodGet {
					t.Error("unexpected mutation", r.Method)
					http.Error(w, "method", 405)
					return
				}
				gets++
				if r.URL.Path == "/threads" {
					writeJSON(w, []map[string]any{{"id": "voice", "tools": []string{"check_availability", "create_booking"}, "mcp_names": []string{"bookings"}}})
					return
				}
				if r.URL.Path != "/threads/voice/capabilities" {
					http.NotFound(w, r)
					return
				}
				cap := readyCapabilityFixture("voice", []string{"check_availability", "create_booking"}, []string{"bookings"})
				switch name {
				case "legacy_grants_only":
					http.NotFound(w, r)
					return
				case "read_failure":
					http.Error(w, "unavailable", 503)
					return
				case "malformed":
					_, _ = w.Write([]byte("not JSON"))
					return
				case "registered_not_presented":
					cap.Status = "pending"
					cap.PresentationVerified = false
					cap.PresentedTools = nil
					cap.AppliedRevision = 0
				case "ready_missing_booking":
					cap.PresentedTools = []string{"check_availability"}
				case "verified_none":
					cap = readyCapabilityFixture("voice", []string{}, []string{})
				case "stale_revision":
					cap.AppliedRevision = 2
				case "stale_session":
					cap.AppliedSessionGeneration = 1
				case "incomplete_ready":
					cap.PresentedTools = nil
				case "unsupported_version":
					cap.Version = 2
				case "wrong_thread":
					cap.ThreadID = "other"
				}
				writeJSON(w, cap)
			}))
			defer core.Close()
			port, _ := strconv.Atoi(strings.TrimPrefix(core.URL, "http://127.0.0.1:"))
			resolver := s.resolver()
			inst := framework.InstanceInfo{ID: a.ID, Port: port, CoreAPIKey: "core-key"}
			result, err := resolver.SpawnRealtimeThread(inst, sdk.RealtimeSpawnRequest{AgentID: a.ID, ThreadID: "voice", Directive: "Book appointments", MCP: []string{"bookings"}})
			if err != nil {
				t.Fatal("verification converted successful spawn into failure", err)
			}
			if result.Status != "created" || result.AudioToken != "single-use-token" || posts != 1 {
				t.Fatal("lost token or duplicated spawn", result, posts)
			}
			expectedVerified := name == "ready_booking" || name == "ready_missing_booking" || name == "verified_none"
			if result.CapabilitiesVerified != expectedVerified {
				t.Fatalf("verified=%v snapshot=%+v", result.CapabilitiesVerified, result.Capabilities)
			}
			if !expectedVerified && (result.EffectiveTools != nil || result.EffectiveMCP != nil) {
				t.Fatal("unverified grants exposed as effective tools", result)
			}
			if result.Capabilities == nil || !result.Capabilities.GrantsVerified {
				t.Fatal("lost independently verified grants", result)
			}
			if name == "legacy_grants_only" && (result.Capabilities.Status != "unknown" || result.Capabilities.RegisteredTools != nil || result.Capabilities.ConnectedMCP != nil || result.Capabilities.PresentedTools != nil) {
				t.Fatal("invented runtime evidence", result.Capabilities)
			}
			if name == "registered_not_presented" && (len(result.Capabilities.RegisteredTools) != 2 || result.Capabilities.Status != "pending") {
				t.Fatal(result.Capabilities)
			}
			missing, verificationErr := result.Capabilities.MissingPresentedTools("check_availability", "create_booking")
			if name == "ready_booking" && (verificationErr != nil || len(missing) != 0) {
				t.Fatal(missing, verificationErr)
			}
			if name == "ready_missing_booking" && (verificationErr != nil || strings.Join(missing, ",") != "create_booking" || strings.Join(result.EffectiveTools, ",") != "check_availability") {
				t.Fatal("missing booking tool reported healthy", result, missing, verificationErr)
			}
			if !expectedVerified && verificationErr == nil {
				t.Fatal("unverified workflow reported healthy")
			}
			// Checking again is GET-only; no respawn or audio-token renewal.
			_ = resolver.ThreadRealtimeCapabilities(inst, "voice")
			if posts != 1 || gets < 2 {
				t.Fatal("readiness check mutated the session", posts, gets)
			}
		})
	}
}

func TestCallbackRealtimeCapabilitiesIsReadOnlyAndScoped(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "capability-reader", sdk.PermRealtimeSpawn)
	agent, err := s.store.CreateAgent(1, "reception", "bookings", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	request := func(installID int64, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-User-ID", "1")
		r.Header.Set("X-Apteva-App-Install-ID", itoa64(installID))
		w := httptest.NewRecorder()
		s.handleAppCallback(w, r)
		return w
	}
	path := "/apps/callback/threads/voice/capabilities?agent_id=" + itoa64(agent.ID)
	response := request(installID, path)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var state sdk.RealtimeCapabilities
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "unknown" || s.agents.GetPort(agent.ID) != 0 {
		t.Fatal("inspection started stopped agent", state)
	}
	other, err := s.store.CreateAgent(1, "other", "private", "autonomous", `{}`, "proj-other")
	if err != nil {
		t.Fatal(err)
	}
	denied := request(installID, "/apps/callback/threads/voice/capabilities?agent_id="+itoa64(other.ID))
	if denied.Code != 403 {
		t.Fatal("cross-project read accepted", denied.Code, denied.Body.String())
	}
	unprivileged := seedRuntimeAPIInstall(t, s, "no-capability-read")
	denied = request(unprivileged, path)
	if denied.Code != 403 {
		t.Fatal("permissionless read accepted", denied.Code, denied.Body.String())
	}
}
