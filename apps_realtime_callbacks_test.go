package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
