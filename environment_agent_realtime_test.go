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
	for _, scoped := range []bool{false, true} {
		name := "auto"
		if scoped {
			name = "root_paused"
		}
		t.Run(name, func(t *testing.T) { testEnvironmentRealtimeCloneSession(t, scoped) })
	}
}

func testEnvironmentRealtimeCloneSession(t *testing.T, scoped bool) {
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
	// The text provider is a local protocol fixture too. No AI credentials are
	// read and neither the shared server nor a deployed receptionist is used.
	inferences := make(chan struct{}, 8)
	blockedInferences := make(chan struct{}, 1)
	textProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(string(request), "Perform one isolated worker iteration.") {
			select {
			case inferences <- struct{}{}:
			default:
			}
		}
		if strings.Contains(string(request), "Blocked synthetic worker.") {
			select {
			case blockedInferences <- struct{}{}:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Local worker ran.\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(textProvider.Close)
	registerRuntimeApp(s, "local-text", "ollama", map[string]string{"OLLAMA_HOST": "{{credentials.host}}"})
	addConnection(t, s, "local-text", "Local text fixture", "proj-1", map[string]string{"host": textProvider.URL})
	registerRuntimeApp(s, "local-voice", "google", map[string]string{"GOOGLE_API_KEY": "{{credentials.api_key}}"})
	addConnection(t, s, "local-voice", "Local voice fixture", "proj-1", map[string]string{"api_key": "local-test-key"})
	spec := EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: environmentRealtimeTestPool()}
	if scoped {
		spec.StartPaused, spec.StartPausedScope, spec.StartPausedThreadID = true, "thread", "main"
	}
	clone, err := s.SpawnAgentInEnvironment(runtime, spec)
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
		Execution struct {
			Mode     string `json:"mode"`
			Scope    string `json:"scope"`
			ThreadID string `json:"thread_id"`
			Threads  map[string]struct {
				Mode        string
				Breakpoints []string
			} `json:"threads"`
		} `json:"execution_control"`
		Providers []struct {
			Name    string            `json:"name"`
			Default bool              `json:"default"`
			Models  map[string]string `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		t.Fatal(err)
	}
	if config.Execution.Mode != "auto" || config.Execution.Scope == "thread" || config.Execution.ThreadID != "" {
		t.Fatalf("voice runtime has a startup execution gate: %+v", config.Execution)
	}
	if scoped && (config.Execution.Threads["main"].Mode != "paused" || len(config.Execution.Threads) != 1) {
		t.Fatalf("lost exact root pause: %+v", config.Execution)
	}
	if !scoped && len(config.Execution.Threads) != 0 {
		t.Fatal("default auto acquired thread rules")
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
	// Assert an actual worker request, independently of main's permitted
	// inference requests, including when main has an exact startup pause.
	runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodPost, "/threads/execution-local", map[string]any{"directive": "Perform one isolated worker iteration.", "provider": "ollama", "tools": []string{}, "mcp": []string{}})
	select {
	case <-inferences:
	case <-time.After(10 * time.Second):
		t.Fatal("worker remained frozen under the configured startup policy")
	}
	var status struct {
		Execution struct {
			Mode, Scope string
			ThreadID    string `json:"thread_id"`
		} `json:"execution_control"`
	}
	response := runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodGet, "/config", nil)
	if err := json.Unmarshal(response, &status); err != nil {
		t.Fatal(err)
	}
	if status.Execution.Mode != "auto" || status.Execution.ThreadID != "" {
		t.Fatalf("voice worker acquired a global execution gate: %+v", status.Execution)
	}
	if scoped {
		var paused struct {
			Execution struct {
				Waiters []struct {
					ThreadID string `json:"thread_id"`
					Scope    string
					Phase    string
				}
			} `json:"execution_control"`
		}
		data := runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodGet, "/status", nil)
		if err := json.Unmarshal(data, &paused); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, w := range paused.Execution.Waiters {
			found = found || w.ThreadID == "main" && w.Scope == "thread" && w.Phase == "iteration.start"
		}
		if !found {
			t.Fatalf("worker execution released the root: %s", data)
		}
	}
	// Opting out of instance startup pause does not exempt workers from an explicit
	// instance pause later. Verify the restored Core still enforces that gate.
	runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodPut, "/config", map[string]any{"execution_control": map[string]any{"mode": "paused", "scope": "instance", "breakpoints": []string{"iteration.start"}}})
	runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodPost, "/threads/execution-blocked", map[string]any{"directive": "Blocked synthetic worker.", "provider": "ollama", "tools": []string{}, "mcp": []string{}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		var gated struct {
			Execution struct {
				Waiting  bool   `json:"waiting"`
				ThreadID string `json:"active_thread_id"`
				Phase    string `json:"phase"`
			} `json:"execution_control"`
		}
		response := runtimeMCPTestCoreRequest(t, coreURL, clone.APIKey, http.MethodGet, "/status", nil)
		if err := json.Unmarshal(response, &gated); err != nil {
			t.Fatal(err)
		}
		if gated.Execution.Waiting && gated.Execution.ThreadID == "execution-blocked" && gated.Execution.Phase == "iteration.start" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("explicit global pause did not gate worker: %+v", gated.Execution)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-blockedInferences:
		t.Fatal("explicit global pause allowed worker inference")
	default:
	}
	unchanged, err := s.store.GetAgentByID(source.ID)
	if err != nil || unchanged.Config != source.Config {
		t.Fatalf("testing changed the source agent: %v", err)
	}
}
