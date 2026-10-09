package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Launch the real cloned Core and open a voice session against a local Gemini
// protocol fixture. A build overlay changes only Google's endpoint constant;
// no checked-out Core source, external provider or real agent is modified.
func TestEnvironmentRealtimeCloneSessionUsesPinnedModel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches Core with a local voice protocol fixture")
	}
	s, runtime, source := runtimeStartupFixture(t)
	source.Config = `{"realtime_enabled":true,"realtime_provider":"google-realtime","realtime_model":"gemini-3.8-live","realtime_voice":"Kore","realtime_voice_mcp":[]}`
	if err := s.store.UpdateAgent(source); err != nil {
		t.Fatal(err)
	}
	setupModels := make(chan string, 1)
	voice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var setup struct {
			Setup struct{ Model string } `json:"setup"`
		}
		if err := conn.ReadJSON(&setup); err != nil {
			t.Error(err)
			return
		}
		select {
		case setupModels <- setup.Setup.Model:
		default:
		}
		if err := conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}}); err != nil {
			t.Error(err)
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(voice.Close)

	coreDir, err := filepath.Abs(filepath.Join("..", "core"))
	if err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(coreDir, "provider_google_realtime.go")
	providerSource, err := os.ReadFile(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := regexp.MustCompile(`(?m)^const googleRealtimeEndpoint = "[^"]+"$`)
	if len(endpoint.FindAll(providerSource, -1)) != 1 {
		t.Fatal("cannot locate the Google voice endpoint for the local test overlay")
	}
	buildDir := t.TempDir()
	overlaySource := filepath.Join(buildDir, "provider_google_realtime.go")
	localEndpoint := "ws" + strings.TrimPrefix(voice.URL, "http")
	if err := os.WriteFile(overlaySource, endpoint.ReplaceAll(providerSource, []byte(fmt.Sprintf("const googleRealtimeEndpoint = %q", localEndpoint))), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, _ := json.Marshal(map[string]any{"Replace": map[string]string{providerPath: overlaySource}})
	overlayPath := filepath.Join(buildDir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	coreBin := filepath.Join(buildDir, "apteva-core")
	build := exec.Command("go", "build", "-overlay", overlayPath, "-o", coreBin, "./cmd/apteva-core")
	build.Dir = coreDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build local Core fixture: %v\n%s", err, output)
	}
	s.agents.coreCmd = coreBin

	type sessionEvent struct {
		InstanceID int64           `json:"instance_id"`
		ThreadID   string          `json:"thread_id"`
		Type       string          `json:"type"`
		Data       json.RawMessage `json:"data"`
	}
	sessions := make(chan sessionEvent, 4)
	telemetry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Secret") != s.instanceSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var events []sessionEvent
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, event := range events {
			if event.Type == "realtime.session_started" {
				select {
				case sessions <- event:
				default:
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(telemetry.Close)
	telemetryURL, _ := url.Parse(telemetry.URL)
	s.port = telemetryURL.Port()
	registerRuntimeApp(s, "local-text", "ollama", map[string]string{"OLLAMA_HOST": "{{credentials.host}}"})
	addConnection(t, s, "local-text", "Local text fixture", "proj-1", map[string]string{"host": telemetry.URL})
	registerRuntimeApp(s, "local-voice", "google", map[string]string{"GOOGLE_API_KEY": "{{credentials.api_key}}"})
	addConnection(t, s, "local-voice", "Local voice fixture", "proj-1", map[string]string{"api_key": "local-test-key"})
	clone, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: environmentRealtimeTestPool(), StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.StopAgent(clone.AgentID) })
	stored, err := s.store.GetAgentByID(clone.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if provider, model := configuredAgentRealtimeSelection(stored.Config); provider != "google-realtime" || model != "gemini-3.8-live" {
		t.Fatalf("persisted clone selection changed: provider=%q model=%q", provider, model)
	}
	coreURL := fmt.Sprintf("http://127.0.0.1:%d", clone.Port)
	configJSON := runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodGet, "/config", nil)
	var config struct {
		Providers []struct {
			Name    string            `json:"name"`
			Default bool              `json:"default"`
			Models  map[string]string `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, provider := range config.Providers {
		if provider.Name == "google-realtime" && provider.Default && provider.Models["large"] == "gemini-3.8-live" && provider.Models["medium"] == "gemini-3.8-live" && provider.Models["small"] == "gemini-3.8-live" {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("clone Core configuration lost its realtime selection: %+v", config.Providers)
	}
	// Do not pass a provider or model here: the clone's defaults must be right.
	runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodPost, "/threads/voice-test", map[string]any{"realtime": true, "directive": "Answer the test call.", "tools": []string{}, "mcp": []string{}})
	select {
	case model := <-setupModels:
		if model != "models/gemini-3.8-live" {
			t.Fatalf("voice session opened with model %q", model)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("voice session did not open against the local fixture")
	}
	select {
	case event := <-sessions:
		var data struct{ Provider, Model string }
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if event.InstanceID != clone.AgentID || event.ThreadID != "voice-test" || data.Provider != "google-realtime" || data.Model != "gemini-3.8-live" {
			t.Fatalf("actual session telemetry does not match the clone: %+v data=%+v", event, data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Core did not report realtime.session_started")
	}
	unchanged, err := s.store.GetAgentByID(source.ID)
	if err != nil || unchanged.Config != source.Config {
		t.Fatalf("testing changed the source agent: %v", err)
	}
}
