package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
)

func TestRealtimeResolverForwardsLifecycleContract(t *testing.T) {
	t.Helper()
	var spawnBody map[string]any
	requests := make([]string, 0, 3)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer core-key" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/threads/voice":
			if err := json.NewDecoder(r.Body).Decode(&spawnBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "id": "voice", "audio_token": "first"})
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "voice", "tools": []string{"pace", "send"}, "mcp_names": []string{"bookings"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/threads/voice/audio-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "renewed", "id": "voice", "audio_token": "second"})
		case r.Method == http.MethodDelete && r.URL.Path == "/threads/voice":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	inst := framework.InstanceInfo{ID: 42, Port: port, CoreAPIKey: "core-key"}
	resolver := &serverResolver{}

	spawned, err := resolver.SpawnRealtimeThread(inst, sdk.RealtimeSpawnRequest{
		AgentID: 42, ThreadID: "voice", Directive: "answer calls", Voice: "marin",
		CallContext: &sdk.RealtimeCallContext{
			CallID: "call-1", Direction: "inbound", FromNumber: "+12025550100",
		},
		TurnDetection: &sdk.RealtimeTurnDetection{Profile: "telephony"},
		Ephemeral:     true, InitialMessage: "Greet the caller.", BridgeDisconnectTTLSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if spawned.AudioToken != "first" {
		t.Fatalf("spawn token = %q", spawned.AudioToken)
	}
	if !spawned.CapabilitiesVerified || strings.Join(spawned.EffectiveTools, ",") != "pace,send" ||
		strings.Join(spawned.EffectiveMCP, ",") != "bookings" {
		t.Fatalf("spawn capabilities = %#v", spawned)
	}
	if spawnBody["ephemeral"] != true || spawnBody["initial_message"] != "Greet the caller." || spawnBody["bridge_disconnect_ttl_seconds"] != float64(30) {
		t.Fatalf("spawn lifecycle body = %#v", spawnBody)
	}
	directive, _ := spawnBody["directive"].(string)
	if !strings.Contains(directive, "[TRUSTED CALL CONTEXT]") ||
		!strings.Contains(directive, `"call_id":"call-1"`) ||
		!strings.HasPrefix(directive, "answer calls") {
		t.Fatalf("typed call context was not translated safely:\n%s", directive)
	}
	turnDetection, ok := spawnBody["turn_detection"].(map[string]any)
	if !ok || turnDetection["profile"] != "telephony" {
		t.Fatalf("spawn turn detection = %#v", spawnBody["turn_detection"])
	}
	renewed, err := resolver.RenewRealtimeAudioBridge(inst, "voice")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.AudioToken != "second" {
		t.Fatalf("renewed token = %q", renewed.AudioToken)
	}
	if err := resolver.KillThread(inst, "voice"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestRealtimeResolverKeepsSuccessfulSpawnWhenCapabilityVerificationFails(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/threads/voice-unverified":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "created", "id": "voice-unverified", "audio_token": "must-not-be-lost",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&serverResolver{}).SpawnRealtimeThread(
		framework.InstanceInfo{ID: 42, Port: port},
		sdk.RealtimeSpawnRequest{
			AgentID: 42, ThreadID: "voice-unverified", Directive: "Answer callers.",
			CapabilityMode: sdk.RealtimeCapabilitiesNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "created" || result.AudioToken != "must-not-be-lost" ||
		result.CapabilitiesVerified || result.EffectiveTools != nil || result.EffectiveMCP != nil {
		t.Fatalf("unverified spawn result=%#v", result)
	}
}

func TestCallbackRealtimeSpawnInheritsAgentMCPs(t *testing.T) {
	var spawnBody struct {
		MCP           []string                   `json:"mcp"`
		Tools         []string                   `json:"tools"`
		TurnDetection *sdk.RealtimeTurnDetection `json:"turn_detection"`
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer core-key" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"mcp_servers": []map[string]any{
				{"name": "flexylead-bookings"},
				{"name": "crm"},
				{"name": "channels", "no_spawn": true},
				{"name": "apteva-server"},
				{"name": "flexylead-bookings"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/threads/tel-call":
			if err := json.NewDecoder(r.Body).Decode(&spawnBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "id": "tel-call", "audio_token": "audio-token"})
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "tel-call", "tools": []string{"pace", "send", "crm_search"},
				"mcp_names": []string{"flexylead-bookings", "crm"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "reception", "answer calls", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	installID := seedInstallWithBindings(t, s, "telephony-realtime-test", sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "telephony-realtime-test",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermRealtimeSpawn},
		},
	}, nil)
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	s.agents.mu.Unlock()

	body, err := json.Marshal(sdk.RealtimeSpawnRequest{
		AgentID: agent.ID, ThreadID: "tel-call", Directive: "Book an appointment.",
		TurnDetection: &sdk.RealtimeTurnDetection{Profile: "telephony"},
		Ephemeral:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://server.internal/apps/callback/threads/spawn-realtime", bytes.NewReader(body))
	request.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleCallbackSpawnRealtime(response, request, installID)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Join(spawnBody.MCP, ",") != "flexylead-bookings,crm" {
		t.Fatalf("inherited MCPs=%v", spawnBody.MCP)
	}
	if spawnBody.Tools != nil {
		t.Fatalf("tools=%v, want nil", spawnBody.Tools)
	}
	if spawnBody.TurnDetection == nil || spawnBody.TurnDetection.Profile != "telephony" {
		t.Fatalf("turn detection=%#v", spawnBody.TurnDetection)
	}
	var result sdk.RealtimeSpawnResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.CapabilitiesVerified ||
		strings.Join(result.EffectiveMCP, ",") != "flexylead-bookings,crm" ||
		strings.Join(result.EffectiveTools, ",") != "pace,send,crm_search" {
		t.Fatalf("effective capabilities=%#v", result)
	}
}

func TestCallbackRealtimeSpawnNoneDoesNotInheritAgentMCPs(t *testing.T) {
	configReads := 0
	var spawnBody struct {
		MCP   []string `json:"mcp"`
		Tools []string `json:"tools"`
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config":
			configReads++
			http.Error(w, "must not inherit", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/threads/tel-none":
			if err := json.NewDecoder(r.Body).Decode(&spawnBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "id": "tel-none", "audio_token": "audio-token"})
		case r.Method == http.MethodGet && r.URL.Path == "/threads":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "tel-none", "tools": []string{"pace", "send"}, "mcp_names": []string{},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "reception-none", "answer calls", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	installID := seedInstallWithBindings(t, s, "telephony-realtime-none-test", sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "telephony-realtime-none-test",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermRealtimeSpawn},
		},
	}, nil)
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	s.agents.mu.Unlock()

	body, err := json.Marshal(sdk.RealtimeSpawnRequest{
		AgentID: agent.ID, ThreadID: "tel-none", Directive: "Answer without app access.",
		CapabilityMode: sdk.RealtimeCapabilitiesNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://server.internal/apps/callback/threads/spawn-realtime", bytes.NewReader(body))
	request.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleCallbackSpawnRealtime(response, request, installID)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if configReads != 0 {
		t.Fatalf("inheritance config reads=%d", configReads)
	}
	if spawnBody.Tools == nil || len(spawnBody.Tools) != 0 || spawnBody.MCP == nil || len(spawnBody.MCP) != 0 {
		t.Fatalf("explicit none was not preserved: tools=%#v mcp=%#v", spawnBody.Tools, spawnBody.MCP)
	}
	var result sdk.RealtimeSpawnResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.CapabilitiesVerified || len(result.EffectiveMCP) != 0 ||
		strings.Join(result.EffectiveTools, ",") != "pace,send" {
		t.Fatalf("effective capabilities=%#v", result)
	}
}

func TestNormalizeRealtimeCapabilityMode(t *testing.T) {
	empty := []string{}
	for _, tc := range []struct {
		name  string
		mode  sdk.RealtimeCapabilityMode
		tools []string
		mcp   []string
		want  sdk.RealtimeCapabilityMode
		ok    bool
	}{
		{name: "legacy omitted", want: sdk.RealtimeCapabilitiesInheritAgent, ok: true},
		{name: "legacy explicit empty", mcp: empty, want: sdk.RealtimeCapabilitiesExplicit, ok: true},
		{name: "inherit", mode: sdk.RealtimeCapabilitiesInheritAgent, want: sdk.RealtimeCapabilitiesInheritAgent, ok: true},
		{name: "explicit", mode: sdk.RealtimeCapabilitiesExplicit, want: sdk.RealtimeCapabilitiesExplicit, ok: true},
		{name: "none", mode: sdk.RealtimeCapabilitiesNone, want: sdk.RealtimeCapabilitiesNone, ok: true},
		{name: "none with tool", mode: sdk.RealtimeCapabilitiesNone, tools: []string{"crm_search"}},
		{name: "invalid", mode: "automatic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeRealtimeCapabilityMode(tc.mode, tc.tools, tc.mcp)
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("mode=%q err=%v want=%q ok=%t", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestSpawnableMCPNamesFiltersPrivilegeBoundary(t *testing.T) {
	got := spawnableMCPNames([]callbackMCPServerConfig{
		{Name: " bookings "},
		{Name: "channels"},
		{Name: "apteva-channels"},
		{Name: "apteva-server"},
		{Name: "private", NoSpawn: true},
		{Name: "bookings"},
		{Name: "apteva-server", URL: "http://127.0.0.1:5280/api/apps/apteva-server/mcp", Transport: "http"},
		{Name: ""},
	})
	if strings.Join(got, ",") != "bookings,apteva-server" {
		t.Fatalf("spawnable MCPs=%v", got)
	}
}

func TestCallbackRealtimeAudioBaseURLUsesRuntimeGatewayForEnvironmentInstall(t *testing.T) {
	s := newTestServer(t)
	s.port = "5280"
	if err := s.store.SetSetting("public_url", "https://stale-public.example"); err != nil {
		t.Fatal(err)
	}

	const (
		installID = int64(77)
		agentID   = int64(42)
	)
	s.environments = NewEnvironmentManager(t.TempDir())
	s.environments.environments["rt-local"] = &Environment{
		ID: "rt-local",
		installs: map[string]*localInstall{
			"telephony": {InstallID: installID},
		},
		agents: map[int64]*EnvironmentAgent{
			agentID: {AgentID: agentID, Alias: "main"},
		},
		agentAliases: map[string]int64{"main": agentID},
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5280/api/apps/callback/threads/spawn-realtime", nil)
	if got := s.callbackRealtimeAudioBaseURL(request, installID, agentID); got != "http://127.0.0.1:5280" {
		t.Fatalf("runtime bridge base=%q", got)
	}
	if got := s.callbackRealtimeAudioBaseURL(request, installID+1, agentID); got != "https://stale-public.example" {
		t.Fatalf("normal bridge base=%q", got)
	}
}

func TestResolverKillThreadRemovesStoppedAgentDefinition(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	agent, err := s.store.CreateAgent(1, "stopped-chat-agent", "directive", "autonomous", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
		cfg["threads"] = []any{
			map[string]any{"id": "chat-conv-delete"},
			map[string]any{"id": "keep-worker"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	resolver := &serverResolver{srv: s}
	before, err := resolver.ListThreadIDs(framework.InstanceInfo{ID: agent.ID, UserID: agent.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(before, ",") != "chat-conv-delete,keep-worker" {
		t.Fatalf("stopped thread ids before cleanup=%v", before)
	}
	if err := resolver.KillThread(framework.InstanceInfo{ID: agent.ID, UserID: agent.UserID}, "chat-conv-delete"); err != nil {
		t.Fatal(err)
	}
	after, err := resolver.ListThreadIDs(framework.InstanceInfo{ID: agent.ID, UserID: agent.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(after, ",") != "keep-worker" {
		t.Fatalf("stopped thread ids after cleanup=%v", after)
	}
	data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(agent.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Threads []struct {
			ID string `json:"id"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Threads) != 1 || cfg.Threads[0].ID != "keep-worker" {
		t.Fatalf("threads after stopped cleanup=%+v", cfg.Threads)
	}
}
