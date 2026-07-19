package main

import (
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
