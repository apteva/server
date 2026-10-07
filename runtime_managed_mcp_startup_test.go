package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/server/internal/managedmcp"
)

func runtimeStartupFixture(t *testing.T) (*Server, *Environment, *Agent) {
	t.Helper()
	s := newRuntimeAPITestServer(t)
	s.catalog = NewAppCatalog()
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.agents.AuthorizeMCPConfig = s.authorizeAgentMCPConfig
	s.agents.coreCmd = filepath.Join(t.TempDir(), "missing-core")
	runtime, err := s.environments.Create(EnvironmentSpec{ID: "rt-startup", ProjectID: "proj-1", CreatorUserID: 1, NetworkMode: EdgeBlock})
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.store.CreateAgent(1, "source", "Use the booking tool.", "autonomous", `{"mcp_servers":[{"name":"runtime-tools","tool_loading":{"default":"always"}}]}`, "proj-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, source
}

func localRuntimeProviderPool() []ProviderInfo {
	return []ProviderInfo{{Type: "ollama", ModelLarge: "fixture-model", ModelMedium: "fixture-model", ModelSmall: "fixture-model"}}
}

func TestRuntimeManagedMCPSpawnFailureCleanup(t *testing.T) {
	s, runtime, source := runtimeStartupFixture(t)
	if err := runtime.AddManagedMCP(&RuntimeManagedMCP{Name: "runtime-tools", Token: "capability", Status: "running", Process: &MCPProcess{}}); err != nil {
		t.Fatal(err)
	}
	var authorized atomic.Int32
	s.agents.AuthorizeMCPConfig = func(a *Agent, cfg map[string]any) error {
		if err := s.authorizeAgentMCPConfig(a, cfg); err != nil {
			return err
		}
		authorized.Add(1)
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: localRuntimeProviderPool(), StartPaused: true})
		if err == nil || !strings.Contains(err.Error(), "spawn environment core") || strings.Contains(err.Error(), "invalid internal MCP reference") {
			t.Fatalf("unexpected startup failure: %v", err)
		}
		if len(runtime.pendingAgents) != 0 || len(runtime.Agents()) != 0 {
			t.Fatal("failed startup retained runtime authorization")
		}
		var count int
		if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE kind='environment_agent'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("temporary agents count=%d error=%v", count, err)
		}
		paths, err := filepath.Glob(filepath.Join(s.agents.dataDir, "instance_*"))
		if err != nil || len(paths) != 0 {
			t.Fatalf("temporary core resources remain: %v %v", paths, err)
		}
	}
	if authorized.Load() != 2 {
		t.Fatalf("valid runtime authorization was not reached twice: %d", authorized.Load())
	}
	if _, err := s.store.GetAgentByID(source.ID); err != nil {
		t.Fatal("source agent was deleted")
	}
}

func TestRuntimeManagedMCPFailedCoreLaunchClosesChannels(t *testing.T) {
	s, _, source := runtimeStartupFixture(t)
	source.Config = `{"include_apteva_server":false,"include_channels":true}`
	var outputURL string
	s.agents.AuthorizeMCPConfig = func(a *Agent, cfg map[string]any) error {
		if err := s.authorizeAgentMCPConfig(a, cfg); err != nil {
			return err
		}
		for _, entry := range cfg["mcp_servers"].([]any) {
			m := entry.(map[string]any)
			if m["name"] == agentOutputMCPName {
				outputURL, _ = m["url"].(string)
			}
		}
		return nil
	}
	if err := s.agents.Start(source, nil, s.port, localRuntimeProviderPool(), s.instanceSecret); err == nil {
		t.Fatal("missing Core executable started")
	}
	if outputURL == "" {
		t.Fatal("startup did not create an output MCP")
	}
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(outputURL); err == nil {
		response.Body.Close()
		t.Fatal("failed startup left output MCP listener open")
	}
}

func TestRuntimeManagedMCPTeardownDuringStartup(t *testing.T) {
	s, runtime, source := runtimeStartupFixture(t)
	if err := runtime.AddManagedMCP(&RuntimeManagedMCP{Name: "runtime-tools", Token: "capability", Status: "running", Process: &MCPProcess{}}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	resume := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	s.agents.AuthorizeMCPConfig = func(a *Agent, cfg map[string]any) error {
		ready <- struct{}{}
		<-resume
		return s.authorizeAgentMCPConfig(a, cfg)
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: localRuntimeProviderPool(), StartPaused: true})
		spawned <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not reach authorization")
	}
	stopped := make(chan struct{})
	go func() { runtime.Stop(); close(stopped) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.mu.Lock()
		gone := runtime.stopped && len(runtime.pendingAgents) == 0
		runtime.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("teardown did not revoke pending membership")
		}
		time.Sleep(time.Millisecond)
	}
	release.Do(func() { close(resume) })
	select {
	case err := <-spawned:
		if err == nil {
			t.Fatal("startup succeeded after runtime teardown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup cleanup deadlocked")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime teardown deadlocked")
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE kind='environment_agent'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("temporary agents count=%d error=%v", count, err)
	}
}

// A real Core and managed runner execute one deterministic turn. Local fixture
// sidecars persist to separate SQLite databases; no model or Staging endpoint
// is contacted. Both bound apps have original and cloned installations.
func TestRuntimeManagedMCPAgentStartupAndIsolatedWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches local Core and managed MCP runner")
	}
	s, runtime, source := runtimeStartupFixture(t)
	s.dataDir = t.TempDir()
	coreBin := filepath.Join(t.TempDir(), "apteva-core")
	build := exec.Command("go", "build", "-o", coreBin, "./cmd/apteva-core")
	build.Dir = filepath.Join("..", "core")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Core: %v\n%s", err, output)
	}
	s.agents.coreCmd = coreBin
	t.Setenv("APTEVA_MCP_RUNNER_BIN", buildManagedMCPRunner(t))

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/runtime/", s.handleRuntimeManagedMCPBridge)
	mux.Handle("/api/environment-app-gateway/", http.StripPrefix("/api", http.HandlerFunc(s.handleEnvironmentAppGateway)))
	mux.Handle("/api/runtime-managed-mcp/", http.StripPrefix("/api", http.HandlerFunc(s.handleRuntimeManagedMCPGateway)))
	mux.HandleFunc("/api/telemetry", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/telemetry/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	s.port = parsed.Port()

	const appointment = "2026-10-08T10:30:00+02:00"
	arguments := map[string]any{"time": appointment, "reservation": "reservation-1", "call": "call-1", "prospect": "prospect-1", "routing": "routing-1"}
	var sentTool atomic.Bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"content": "Done."}
		if sentTool.CompareAndSwap(false, true) {
			// A fixed native tool response drives Core's real dispatch path;
			// inference and credentials remain entirely local to the fixture.
			args, _ := json.Marshal(arguments)
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "booking-call", "type": "function", "function": map[string]any{"name": "runtime-tools_create_booking", "arguments": string(args)}}}}
		}
		chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
	}))
	t.Cleanup(model.Close)
	registerRuntimeApp(s, "local-model", "ollama", map[string]string{"OLLAMA_HOST": "{{credentials.host}}"})
	addConnection(t, s, "local-model", "Local model", "proj-1", map[string]string{"host": model.URL})

	originals := map[string]*sql.DB{}
	clones := map[string]*sql.DB{}
	bindings := managedMCPBindings{Apps: map[string]int64{}}
	for _, name := range []string{"tables", "functions"} {
		originalID := seedRuntimeAPIInstall(t, s, name)
		var appID int64
		if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, originalID).Scan(&appID); err != nil {
			t.Fatal(err)
		}
		res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id, project_id, status, installed_by) VALUES(?, ?, 'running', 1)`, appID, runtime.ID)
		if err != nil {
			t.Fatal(err)
		}
		cloneID, _ := res.LastInsertId()
		for _, installation := range []struct {
			id    int64
			clone bool
		}{{originalID, false}, {cloneID, true}} {
			db, sidecar := localRuntimeWriteSidecar(t, s, installation.id, name)
			if installation.clone {
				clones[name] = db
				runtime.installs[name] = &localInstall{InstallID: installation.id, AppName: name, SidecarURL: sidecar.URL}
			} else {
				originals[name] = db
				s.installedApps.Add(&InstalledApp{InstallID: installation.id, AppName: name, ProjectID: "proj-1", SidecarURL: sidecar.URL})
			}
		}
		bindings.Apps[name] = originalID
	}
	definition := managedmcp.Definition{Version: managedmcp.DefinitionVersion, Tools: []managedmcp.Tool{
		{Name: "create_booking", Description: "Create an isolated booking.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"time": map[string]any{"type": "string"}}}, Handler: "tools/create_booking.js", Code: `const routing = apteva.app("functions", "route", input); const booking = apteva.app("tables", "rows_create", input); return {routing, booking};`},
		{Name: "forbidden", Description: "Must be excluded.", InputSchema: map[string]any{"type": "object"}, Handler: "tools/forbidden.js", Code: `return {unexpected: true};`},
	}}
	cfg := normalizeManagedMCPConfig(managedMCPConfig{Bindings: bindings})
	record, err := s.store.CreateMCPServerExt(MCPServerInput{UserID: 1, Name: "runtime-tools", Source: managedMCPSource, Transport: "stdio", ProjectID: "proj-1", AllowedTools: []string{"create_booking"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagedMCPSource(s.managedMCPSourceDir(record.ID), definition); err != nil {
		t.Fatal(err)
	}
	if err := s.startRuntimeManagedMCP(runtime, runtimeManagedMCPSelection{Record: record, Config: cfg}); err != nil {
		t.Fatal(err)
	}

	wa, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: 1, Source: source, ProviderPool: localRuntimeProviderPool(), StartPaused: true})
	if err != nil {
		t.Fatalf("isolated agent startup: %v", err)
	}
	t.Cleanup(func() { runtime.StopAgent(wa.AgentID) })
	t.Cleanup(func() {
		if t.Failed() {
			if log, err := os.ReadFile(filepath.Join(s.agents.dataDir, fmt.Sprintf("instance_%d", wa.AgentID), "apteva-core.log")); err == nil {
				if len(log) > 12000 {
					log = log[len(log)-12000:]
				}
				t.Logf("local Core log:\n%s", log)
			}
		}
	})
	if len(runtime.pendingAgents) != 0 || runtime.GetAgent(wa.AgentID) != wa {
		t.Fatal("startup reservation not promoted")
	}
	coreURL := fmt.Sprintf("http://127.0.0.1:%d", wa.Port)
	data := runtimeMCPTestCoreRequest(t, coreURL, wa.APIKey, http.MethodGet, "/config", nil)
	var config struct {
		MCPServers []struct {
			Name      string
			Connected bool
			URL       string
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mcp := range config.MCPServers {
		if mcp.Name == "runtime-tools" {
			found = mcp.Connected && strings.Contains(mcp.URL, "/mcp/runtime/"+runtime.ID+"/")
		}
	}
	if !found {
		t.Fatalf("Core did not connect to isolated managed MCP: %s", data)
	}

	mcp := runtime.ManagedMCP("runtime-tools")
	blocked := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, s.runtimeManagedMCPURL(runtime.ID, mcp.Token), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forbidden","arguments":{}}}`))
	request.RemoteAddr = "127.0.0.1:1"
	s.handleRuntimeManagedMCPBridge(blocked, request)
	if !strings.Contains(blocked.Body.String(), "tool is not enabled") {
		t.Fatalf("tool restriction lost: %s", blocked.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/runtime-managed-mcp/%s/%s/apps/unbound/call", runtime.ID, mcp.Token), strings.NewReader(`{"tool":"write","input":{}}`))
	request.RemoteAddr = "127.0.0.1:1"
	request.Header.Set("Authorization", "Bearer "+mcp.Token)
	blocked = httptest.NewRecorder()
	s.handleRuntimeManagedMCPGateway(blocked, request)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("unbound app allowed: %d %s", blocked.Code, blocked.Body.String())
	}

	runtimeMCPTestCoreRequest(t, coreURL, wa.APIKey, http.MethodPost, "/event", map[string]any{"message": "Create the test booking now."})
	runtimeMCPTestCoreRequest(t, coreURL, wa.APIKey, http.MethodPost, "/control", map[string]any{"action": "run"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := clones["tables"].QueryRow(`SELECT COUNT(*) FROM writes`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	runtimeMCPTestCoreRequest(t, coreURL, wa.APIKey, http.MethodPost, "/control", map[string]any{"action": "pause"})
	for name, db := range clones {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM writes`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("clone %s writes=%d error=%v", name, count, err)
		}
		var stored string
		if err := db.QueryRow(`SELECT input FROM writes`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stored), &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range arguments {
			if got[key] != value {
				t.Fatalf("clone %s %s=%v want=%v", name, key, got[key], value)
			}
		}
	}
	for name, db := range originals {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM writes`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("original %s received writes=%d error=%v", name, count, err)
		}
	}
	t.Log("Real Core connected and executed booking; both clone databases contain exact time/ownership, originals have zero writes")
}

func runtimeMCPTestCoreRequest(t *testing.T, base, key, method, path string, body any) []byte {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, base+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Core %s %s: %d %s", method, path, response.StatusCode, data)
	}
	return data
}

func localRuntimeWriteSidecar(t *testing.T, s *Server, installID int64, name string) (*sql.DB, *httptest.Server) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE writes(input TEXT)`); err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "wrong installation credential", 403)
			return
		}
		var req bridgeRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch req.Method {
		case "initialize":
			writeBridgeRPCResult(w, req.ID, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": name, "version": "fixture"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeBridgeRPCResult(w, req.ID, map[string]any{"tools": []any{}})
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			input, _ := json.Marshal(params.Arguments)
			if _, err := db.Exec(`INSERT INTO writes(input) VALUES(?)`, string(input)); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			text, _ := json.Marshal(map[string]any{"installation": installID})
			writeBridgeRPCResult(w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}})
		default:
			writeBridgeRPCResult(w, req.ID, map[string]any{})
		}
	}))
	t.Cleanup(sidecar.Close)
	return db, sidecar
}
