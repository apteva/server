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
		Ephemeral: true, InitialMessage: "Greet the caller.", BridgeDisconnectTTLSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if spawned.AudioToken != "first" {
		t.Fatalf("spawn token = %q", spawned.AudioToken)
	}
	if spawnBody["ephemeral"] != true || spawnBody["initial_message"] != "Greet the caller." || spawnBody["bridge_disconnect_ttl_seconds"] != float64(30) {
		t.Fatalf("spawn lifecycle body = %#v", spawnBody)
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
	if len(requests) != 3 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestCallbackRealtimeSpawnInheritsAgentMCPs(t *testing.T) {
	var spawnBody struct {
		MCP   []string `json:"mcp"`
		Tools []string `json:"tools"`
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
		Ephemeral: true,
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
}

func TestSpawnableMCPNamesFiltersPrivilegeBoundary(t *testing.T) {
	got := spawnableMCPNames([]callbackMCPServerConfig{
		{Name: " bookings "},
		{Name: "channels"},
		{Name: "apteva-channels"},
		{Name: "apteva-server"},
		{Name: "private", NoSpawn: true},
		{Name: "bookings"},
		{Name: ""},
	})
	if strings.Join(got, ",") != "bookings" {
		t.Fatalf("spawnable MCPs=%v", got)
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
