package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestAgentBehaviorComposition(t *testing.T) {
	base := "# Role\nKeep  two spaces.\n\n# Custom instructions\nPreserve me exactly.\n"
	directive := base
	for _, mode := range []string{"autonomous", "cautious", "learn", "cautious", "autonomous"} {
		directive = withAgentBehavior(directive, mode)
		if strings.Count(directive, behaviorStart) != 1 || strings.Count(directive, behaviorEnd) != 1 {
			t.Fatal("duplicate section", directive)
		}
		if !strings.HasPrefix(directive, base) {
			t.Fatal("unrelated instructions changed")
		}
		if next := withAgentBehavior(directive, mode); next != directive {
			t.Fatal("not idempotent")
		}
		if !strings.Contains(directive, "instructions ("+mode+")") || !strings.Contains(directive, "When delegating") {
			t.Fatal("missing behavior or delegation")
		}
	}
	duplicate := withAgentBehavior("first", "learn") + "\nMIDDLE\n" + withAgentBehavior("last", "cautious")
	result := withAgentBehavior(duplicate, "autonomous")
	if strings.Count(result, behaviorStart) != 1 || !strings.Contains(result, "\nMIDDLE\nlast") {
		t.Fatal("duplicate replacement lost user text", result)
	}
	if got := withoutCoreMode(`{"mode":"learn","execution_control":{"mode":"paused"}}`); strings.Contains(got, "learn") || !strings.Contains(got, "paused") {
		t.Fatal(got)
	}
}

type behaviorTestCore struct {
	mu          sync.Mutex
	cfg         map[string]any
	workers     []map[string]any
	rejectMain  bool
	rejectVoice bool
	puts        int
	workerPuts  int
}

func attachBehaviorTestCore(t *testing.T, s *Server, a *Agent) *behaviorTestCore {
	t.Helper()
	c := &behaviorTestCore{cfg: map[string]any{"directive": withAgentBehavior("live evolved instructions", "autonomous"), "mcp_servers": []any{map[string]any{"name": "crm", "transport": "http", "url": "http://127.0.0.1/crm"}}, "execution_control": map[string]any{"mode": "paused"}}, workers: []map[string]any{
		{"id": "main", "directive": "main"},
		{"id": "research", "directive": "Research only", "tools": []any{"read"}, "history": "keep"},
		{"id": "voice", "directive": "Speak concisely", "realtime": true},
		{"id": "unconscious", "directive": "internal memory rules", "system": true},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/config":
			writeJSON(w, c.cfg)
		case r.Method == "GET" && r.URL.Path == "/threads":
			writeJSON(w, c.workers)
		case r.Method == "GET" && r.URL.Path == "/status":
			writeJSON(w, map[string]any{"rate": "slow", "execution_control": map[string]any{"mode": "paused"}})
		case r.Method == "PUT" && r.URL.Path == "/config":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, exists := body["mode"]; exists {
				t.Error("server forwarded mode")
			}
			c.puts++
			if c.rejectMain {
				http.Error(w, "unavailable", 503)
				return
			}
			for key, value := range body {
				c.cfg[key] = value
			}
			writeJSON(w, map[string]any{"status": "updated"})
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/threads/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || body["directive"] == nil {
				t.Error("worker update changes fields beyond directive", body)
			}
			id := strings.TrimPrefix(r.URL.Path, "/threads/")
			if id == "voice" && c.rejectVoice {
				http.Error(w, "restart required", 409)
				return
			}
			for _, worker := range c.workers {
				if worker["id"] == id {
					worker["directive"] = body["directive"]
					c.workerPuts++
					writeJSON(w, map[string]any{"status": "updated"})
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	s.agents.processes[a.ID] = &runningAgent{port: port, coreAPIKey: "test-core", reattached: true}
	return c
}

func behaviorRequest(t *testing.T, s *Server, id int64, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleUpdateConfig(w, authedRequest(t, http.MethodPut, fmt.Sprintf("/instances/%d/config", id), "", payload))
	return w
}

func behaviorAgent(t *testing.T, s *Server, mode string) *Agent {
	t.Helper()
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "behavior", "saved base", mode, `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func readBehaviorDisk(t *testing.T, s *Server, id int64) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.agents.instanceDir(id), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAgentBehaviorStoppedUpdatesAndValidation(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "autonomous")
	if err := s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error {
		cfg["directive"] = "disk evolved"
		cfg["mode"] = "learn"
		cfg["execution_control"] = map[string]any{"mode": "paused"}
		cfg["threads"] = []any{map[string]any{"id": "worker", "directive": "worker job", "tools": []any{"read"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cautious", "learn", "autonomous"} {
		w := behaviorRequest(t, s, a.ID, map[string]any{"mode": mode})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		a, _ = s.store.GetAgentByID(a.ID)
		cfg := readBehaviorDisk(t, s, a.ID)
		if a.Mode != mode || cfg["directive"] != a.Directive || !strings.HasPrefix(a.Directive, "disk evolved") {
			t.Fatal(a, cfg)
		}
		if _, exists := cfg["mode"]; exists {
			t.Fatal("legacy mode persisted")
		}
		if cfg["execution_control"].(map[string]any)["mode"] != "paused" {
			t.Fatal("execution mode changed")
		}
		state, err := s.store.agentBehaviorState(a.ID)
		if err != nil || state.Pending {
			t.Fatal(state, err)
		}
		worker := cfg["threads"].([]any)[0].(map[string]any)
		if !strings.HasPrefix(worker["directive"].(string), "worker job") || len(worker["tools"].([]any)) != 1 {
			t.Fatal(worker)
		}
	}
	for _, mode := range []any{"", "supervised", 42, nil} {
		w := behaviorRequest(t, s, a.ID, map[string]any{"mode": mode})
		if w.Code != 400 {
			t.Fatal("accepted invalid mode", mode, w.Code)
		}
	}
	w := behaviorRequest(t, s, a.ID, map[string]any{"directive": ""})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a, _ = s.store.GetAgentByID(a.ID)
	if strings.Contains(a.Directive, "disk evolved") || !strings.Contains(a.Directive, behaviorStart) {
		t.Fatal("explicit empty directive not handled")
	}
}

func TestAgentBehaviorRunningModeAndVoicePending(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "autonomous")
	c := attachBehaviorTestCore(t, s, a)
	c.rejectVoice = true
	w := behaviorRequest(t, s, a.ID, map[string]any{"mode": "cautious"})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if response["mode"] != "cautious" || response["behavior_sync"].(map[string]any)["pending"] != true {
		t.Fatal(response)
	}
	a, _ = s.store.GetAgentByID(a.ID)
	if !strings.HasPrefix(a.Directive, "live evolved instructions") {
		t.Fatal("mode patch lost evolved directive", a.Directive)
	}
	c.mu.Lock()
	if !strings.Contains(c.workers[1]["directive"].(string), "instructions (cautious)") || c.workers[1]["history"] != "keep" {
		t.Fatal(c.workers)
	}
	if c.workers[2]["directive"] != "Speak concisely" || c.workers[3]["directive"] != "internal memory rules" {
		t.Fatal("voice/system changed")
	}
	c.rejectVoice = false
	// Evolution after the main update must survive worker retries.
	c.cfg["directive"] = c.cfg["directive"].(string) + "\nnew learned instruction"
	c.mu.Unlock()
	unlock := s.lockAgentConfig(a.ID)
	err := s.reconcileAgentBehavior(context.Background(), a, s.agents.GetPort(a.ID))
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	state, _ := s.store.agentBehaviorState(a.ID)
	if state.Pending {
		t.Fatal(state)
	}
	if !strings.Contains(a.Directive, "new learned instruction") {
		t.Fatal("worker retry lost evolution")
	}
	// Core responses have no mode; the server must still supply its choice.
	for _, path := range []string{"config", "status"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", fmt.Sprintf("/instances/%d/%s", a.ID, path), nil)
		if path == "config" {
			s.handleUpdateConfig(w, r)
		} else {
			s.handleProxy(w, r)
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got["mode"] != "cautious" {
			t.Fatal(path, w.Code, got)
		}
		if got["execution_control"].(map[string]any)["mode"] != "paused" {
			t.Fatal("nested execution mode lost", got)
		}
	}
}

func TestAgentBehaviorFailedUpdateSurvivesRestart(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "autonomous")
	c := attachBehaviorTestCore(t, s, a)
	c.rejectMain = true
	w := behaviorRequest(t, s, a.ID, map[string]any{"mode": "learn", "directive": "desired edit"})
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	a, _ = s.store.GetAgentByID(a.ID)
	state, _ := s.store.agentBehaviorState(a.ID)
	if !state.Pending || state.MainRevision >= state.Revision || !strings.HasPrefix(a.Directive, "desired edit") {
		t.Fatal(a, state)
	}
	s.agents.mu.Lock()
	delete(s.agents.processes, a.ID)
	s.agents.mu.Unlock()
	if err := s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error {
		cfg["directive"] = "stale disk"
		cfg["mode"] = "autonomous"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Reopen storage to prove pending state is durable, not an in-memory flag.
	reopened, err := NewStore(s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	previous := s.store
	s.store = reopened
	defer func() { s.store = previous }()
	a, _ = s.store.GetAgentByID(a.ID)
	unlock := s.lockAgentConfig(a.ID)
	err = s.reconcileAgentBehavior(context.Background(), a, 0)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	cfg := readBehaviorDisk(t, s, a.ID)
	if !strings.HasPrefix(cfg["directive"].(string), "desired edit") || !strings.Contains(cfg["directive"].(string), "instructions (learn)") {
		t.Fatal(cfg)
	}
	state, _ = s.store.agentBehaviorState(a.ID)
	if state.Pending {
		t.Fatal(state)
	}
}

func TestAgentBehaviorMigrationIdempotent(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "learn")
	// Simulate a pre-migration row and its newer core-owned file.
	_, err := s.store.db.Exec(`UPDATE agents SET directive='old DB' WHERE id=?`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.db.Exec(`UPDATE agent_behavior_state SET revision=0,main_revision=0,applied_revision=0,version=0 WHERE agent_id=?`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error {
		cfg["directive"] = "new disk\nKeep custom text"
		cfg["mode"] = "autonomous"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.store.GetAgentByID(a.ID)
	for i := 0; i < 2; i++ {
		unlock := s.lockAgentConfig(a.ID)
		err = s.reconcileAgentBehavior(context.Background(), a, 0)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		state, _ := s.store.agentBehaviorState(a.ID)
		if state.Pending || state.Revision != 1 || !strings.HasPrefix(a.Directive, "new disk\nKeep custom text") || !strings.Contains(a.Directive, "instructions (learn)") {
			t.Fatal(state, a.Directive)
		}
		if err = s.store.migrateReviewFixes(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentBehaviorConcurrentModeAndDirectiveEdits(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "autonomous")
	var wg sync.WaitGroup
	for _, payload := range []map[string]any{{"mode": "learn"}, {"directive": "concurrent edit"}} {
		wg.Add(1)
		go func(body map[string]any) {
			defer wg.Done()
			w := behaviorRequest(t, s, a.ID, body)
			if w.Code != 200 {
				t.Error(w.Code, w.Body.String())
			}
		}(payload)
	}
	wg.Wait()
	a, _ = s.store.GetAgentByID(a.ID)
	if a.Mode != "learn" || !strings.HasPrefix(a.Directive, "concurrent edit") || strings.Count(a.Directive, behaviorStart) != 1 || !strings.Contains(a.Directive, "instructions (learn)") {
		t.Fatal(a)
	}
}

// Explicit binary opt-in keeps the ordinary suite independent of stale locally
// installed cores. This exercises the current mode-free core without an LLM.
func TestAgentBehaviorRealCore(t *testing.T) {
	binary := os.Getenv("APTEVA_BEHAVIOR_CORE_BIN")
	if binary == "" {
		t.Skip("set APTEVA_BEHAVIOR_CORE_BIN to a freshly built mode-free core")
	}
	s := newTestServer(t)
	a := behaviorAgent(t, s, "learn")
	a.Config = `{"include_apteva_server":false,"include_channels":false}`
	if err := s.store.UpdateAgent(a); err != nil {
		t.Fatal(err)
	}
	s.agents = NewAgentManager(t.TempDir(), binary)
	s.port = "1"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No model responses are required while execution is paused.
		http.Error(w, "paused test must not invoke a provider", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	env := map[string]string{"OLLAMA_HOST": provider.URL}
	pool := []ProviderInfo{{Type: "ollama", ModelLarge: "test", ModelMedium: "test", ModelSmall: "test"}}
	if err := s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error {
		cfg["directive"] = "real core task"
		cfg["mode"] = "cautious"
		cfg["execution_control"] = map[string]any{"mode": "paused", "scope": "instance", "breakpoints": []string{"iteration.start"}}
		cfg["threads"] = []any{map[string]any{"id": "research", "directive": "worker-specific task", "tools": []string{"pace"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.agents.Stop(a.ID) })
	if _, err := s.startManagedAgent(a, env, pool); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := s.behaviorCoreJSON(context.Background(), a, "GET", "/config", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg["mode"]; exists {
		t.Fatal("core still owns mode", cfg)
	}
	if !strings.Contains(cfg["directive"].(string), "instructions (learn)") {
		t.Fatal(cfg)
	}
	w := behaviorRequest(t, s, a.ID, map[string]any{"mode": "cautious"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var workers []map[string]any
	if err := s.behaviorCoreJSON(context.Background(), a, "GET", "/threads", nil, &workers); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, worker := range workers {
		if worker["id"] == "research" {
			found = true
			d := worker["directive"].(string)
			if !strings.Contains(d, "worker-specific task") || !strings.Contains(d, "instructions (cautious)") {
				t.Fatal(worker)
			}
		}
	}
	if !found {
		t.Fatal("worker missing")
	}
	s.agents.Stop(a.ID)
	if err := s.store.SetAgentRuntimeStopped(a); err != nil {
		t.Fatal(err)
	}
	w = behaviorRequest(t, s, a.ID, map[string]any{"mode": "autonomous"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a, _ = s.store.GetAgentByID(a.ID)
	if _, err := s.startManagedAgent(a, env, pool); err != nil {
		t.Fatal(err)
	}
	if err := s.behaviorCoreJSON(context.Background(), a, "GET", "/config", nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["execution_control"].(map[string]any)["mode"] != "paused" || !strings.Contains(cfg["directive"].(string), "instructions (autonomous)") {
		t.Fatal(cfg)
	}
}
