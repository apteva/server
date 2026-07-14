package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func requireRealLLMTests(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-LLM test in short mode")
	}
	if !envTruthy(os.Getenv("APTEVA_RUN_REAL_LLM_TESTS")) {
		t.Skip("set APTEVA_RUN_REAL_LLM_TESTS=1 to run real-LLM tests")
	}
}

func findCoreBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("APTEVA_CORE_BIN"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		t.Skipf("APTEVA_CORE_BIN=%s does not exist", path)
	}
	candidates := []string{filepath.Join("..", "core", "apteva-core")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".apteva", "bin", "apteva-core"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			absolute, _ := filepath.Abs(candidate)
			return absolute
		}
	}
	t.Skip("apteva-core binary not found")
	return ""
}

func loadOpenAICodexProviderState(t *testing.T) map[string]any {
	t.Helper()
	requireRealLLMTests(t)
	if token := strings.TrimSpace(os.Getenv("OPENAI_CODEX_ACCESS_TOKEN")); token != "" {
		return map[string]any{
			"auth": map[string]any{
				"type": providerAuthTypeDeviceCode, "provider": openAICodexAuthProvider,
				"mode": "chatgpt", "source": "env",
			},
			"credentials": map[string]any{"access_token": token},
			"runtime":     map[string]any{"base_url": openAICodexBackendAPIBaseURL},
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir for local OpenAI Codex provider: %v", err)
	}
	for _, dataDir := range []string{filepath.Join(home, ".apteva"), filepath.Join(home, ".apteva-prod")} {
		if state, ok := readLocalOpenAICodexProviderState(dataDir); ok {
			return state
		}
	}
	t.Skip("OpenAI Codex provider auth not found")
	return nil
}

func readLocalOpenAICodexProviderState(dataDir string) (map[string]any, bool) {
	secret, err := LoadSecret(dataDir)
	if err != nil {
		return nil, false
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "apteva.db")+"?mode=ro")
	if err != nil {
		return nil, false
	}
	defer db.Close()
	var encrypted string
	if err := db.QueryRow(`SELECT encrypted_data FROM providers WHERE name='OpenAI Codex' OR provider_type_id=15 ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&encrypted); err != nil {
		return nil, false
	}
	plain, err := Decrypt(secret, encrypted)
	if err != nil {
		return nil, false
	}
	var state map[string]any
	if json.Unmarshal([]byte(plain), &state) != nil || stateMap(state, "auth")["provider"] != openAICodexAuthProvider {
		return nil, false
	}
	if strings.TrimSpace(stringFromNested(state, "credentials", "access_token")) == "" && strings.TrimSpace(stringFromNested(state, "credentials", "refresh_token")) == "" {
		return nil, false
	}
	return state, true
}

func setupRealServerWithProviderState(t *testing.T, corePath, agentName, agentDirective string, providerTypeID int64, providerType, providerName string, providerData map[string]any) (*Server, int64, *Agent) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "apteva.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}
	s := &Server{
		store: store, secret: secret, port: strconv.Itoa(port), dataDir: dataDir,
		agents:      NewAgentManager(filepath.Join(dataDir, "agents"), corePath),
		broadcaster: NewTelemetryBroadcaster(), instanceSecret: "real-llm-test-secret",
		localApps:     NewLocalSupervisor(filepath.Join(os.TempDir(), fmt.Sprintf("apteva-environment-test-appcache-%d", time.Now().UnixNano()))),
		installedApps: NewInstalledAppsRegistry(), appBus: NewAppEventBus(), catalog: NewAppCatalog(),
		environments: NewEnvironmentManager(environmentDataRoot(dataDir)),
	}
	s.environments.server = s
	s.appEventDispatcher = NewAppEventDispatcher(s)
	s.appEventDispatcher.Start()
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/environment-app-gateway/", s.handleEnvironmentAppGateway)
	apiMux.HandleFunc("/environment-mcp", s.handleEnvironmentMCP)
	apiMux.HandleFunc("/telemetry/live", s.handleLiveTelemetry)
	apiMux.HandleFunc("/telemetry", s.handleIngestTelemetry)
	s.registerAppRuntimeRoutes(apiMux)
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", apiMux))
	mux.HandleFunc("/mcp/", s.handleMCPEndpoint)
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go httpServer.Serve(listener) //nolint:errcheck
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})
	user, err := store.CreateUser("real-core-test@example.com", "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _ := json.Marshal(providerData)
	encrypted, err := Encrypt(secret, string(plaintext))
	if err != nil {
		t.Fatalf("encrypt provider: %v", err)
	}
	if _, err := store.CreateProvider(user.ID, providerTypeID, providerType, providerName, encrypted); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	agent, err := store.CreateAgent(user.ID, agentName, agentDirective, "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(agent.ID) })
	return s, user.ID, agent
}
