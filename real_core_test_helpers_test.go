package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

func findServerBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("APTEVA_SERVER_BIN"); path != "" {
		if _, err := os.Stat(path); err == nil {
			absolute, _ := filepath.Abs(path)
			return absolute
		}
		t.Skipf("APTEVA_SERVER_BIN=%s does not exist", path)
	}
	for _, candidate := range []string{"apteva-server", filepath.Join(".", "apteva-server")} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			absolute, _ := filepath.Abs(candidate)
			return absolute
		}
	}
	t.Skip("apteva-server binary not found; build it or set APTEVA_SERVER_BIN")
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

func loadOpenCodeGoAPIKey(t *testing.T) string {
	t.Helper()
	requireRealLLMTests(t)
	if key := strings.TrimSpace(os.Getenv("OPENCODE_GO_API_KEY")); key != "" {
		return key
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir for local OpenCode Go provider: %v", err)
	}
	for _, dataDir := range []string{filepath.Join(home, ".apteva"), filepath.Join(home, ".apteva-prod")} {
		secret, err := LoadSecret(dataDir)
		if err != nil {
			continue
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "apteva.db")+"?mode=ro")
		if err != nil {
			continue
		}
		var encrypted string
		err = db.QueryRow(`SELECT encrypted_data FROM providers WHERE name='OpenCode Go' OR provider_type_id=13 ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&encrypted)
		_ = db.Close()
		if err != nil {
			continue
		}
		plain, err := Decrypt(secret, encrypted)
		if err != nil {
			continue
		}
		var state map[string]any
		if json.Unmarshal([]byte(plain), &state) != nil {
			continue
		}
		if key, _ := state["OPENCODE_GO_API_KEY"].(string); strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key)
		}
	}
	// Providers/connections fusion: locally-migrated stores keep LLM keys
	// as connections rows (legacy provider rows deleted), so fall back to
	// app_slug='opencode-go' connections. Several rows may exist with a
	// mix of dead and live keys — probe each against the API and return
	// the first that authenticates.
	candidates := opencodeGoKeysFromLocalConnections()
	for _, key := range candidates {
		if opencodeGoKeyWorks(key) {
			return key
		}
	}
	if len(candidates) > 0 {
		t.Skipf("found %d local OpenCode Go key(s) but none authenticate against the API", len(candidates))
	}
	t.Skip("OpenCode Go provider auth not found in the environment or local Apteva provider stores")
	return ""
}

// opencodeGoKeysFromLocalConnections returns every distinct OpenCode Go
// key stored as a connection, newest connection first (newer rows are
// likelier to hold a live key).
func opencodeGoKeysFromLocalConnections() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, dataDir := range []string{filepath.Join(home, ".apteva"), filepath.Join(home, ".apteva-prod")} {
		secret, err := LoadSecret(dataDir)
		if err != nil {
			continue
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "apteva.db")+"?mode=ro")
		if err != nil {
			continue
		}
		rows, err := db.Query(`SELECT encrypted_credentials FROM connections
			WHERE app_slug='opencode-go' AND status='active' ORDER BY id DESC`)
		if err != nil {
			_ = db.Close()
			continue
		}
		var encryptedRows []string
		for rows.Next() {
			var encrypted string
			if rows.Scan(&encrypted) == nil {
				encryptedRows = append(encryptedRows, encrypted)
			}
		}
		rows.Close()
		_ = db.Close()
		for _, encrypted := range encryptedRows {
			plain, err := Decrypt(secret, encrypted)
			if err != nil {
				continue
			}
			var creds map[string]any
			if json.Unmarshal([]byte(plain), &creds) != nil {
				continue
			}
			for _, field := range []string{"api_key", "OPENCODE_GO_API_KEY", "key", "token"} {
				if key, _ := creds[field].(string); strings.TrimSpace(key) != "" && !seen[strings.TrimSpace(key)] {
					seen[strings.TrimSpace(key)] = true
					out = append(out, strings.TrimSpace(key))
				}
			}
		}
	}
	return out
}

// opencodeGoKeyWorks probes the key with a minimal chat completion —
// the same endpoint core uses — so tests never spawn agents on a key
// that will 401.
func opencodeGoKeyWorks(key string) bool {
	body := strings.NewReader(`{"model":"glm-5.2","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)
	req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", body)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden
}

func loadXAIAPIKey(t *testing.T) string {
	t.Helper()
	requireRealLLMTests(t)
	if key := strings.TrimSpace(os.Getenv("XAI_API_KEY")); key != "" {
		return key
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir for local xAI provider: %v", err)
	}
	for _, dataDir := range []string{filepath.Join(home, ".apteva"), filepath.Join(home, ".apteva-prod")} {
		secret, err := LoadSecret(dataDir)
		if err != nil {
			continue
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "apteva.db")+"?mode=ro")
		if err != nil {
			continue
		}
		var encrypted string
		err = db.QueryRow(`SELECT encrypted_data FROM providers WHERE name='xAI' OR provider_type_id=17 ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&encrypted)
		_ = db.Close()
		if err != nil {
			continue
		}
		plain, err := Decrypt(secret, encrypted)
		if err != nil {
			continue
		}
		var state map[string]any
		if json.Unmarshal([]byte(plain), &state) != nil {
			continue
		}
		if key, _ := state["XAI_API_KEY"].(string); strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key)
		}
	}
	t.Skip("xAI provider auth not found in the environment or local Apteva provider stores")
	return ""
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
	registerRealManagementRoutes(s, apiMux)
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

func registerRealManagementRoutes(s *Server, apiMux *http.ServeMux) {
	agentsCollection := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/agents", agentsCollection)
	apiMux.HandleFunc("/agents/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/instances/" + strings.TrimPrefix(r.URL.Path, "/agents/")
		path := strings.TrimPrefix(r.URL.Path, "/instances/")
		switch {
		case strings.HasSuffix(path, "/config"):
			s.handleUpdateConfig(w, r)
		case strings.HasSuffix(path, "/start"):
			s.handleStartInstance(w, r)
		case strings.HasSuffix(path, "/stop"):
			s.handleStopInstance(w, r)
		default:
			s.handleInstance(w, r)
		}
	}))
	apiMux.HandleFunc("/apps", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleListApps(w, r)
	}))
	apiMux.HandleFunc("/apps/marketplace", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleMarketplace(w, r)
	}))
	apiMux.HandleFunc("/apps/install", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.handleInstallApp(w, r)
	}))
}

func configureRealAptevaServerGateway(t *testing.T, s *Server) {
	t.Helper()
	s.agents.serverCmd = findServerBinary(t)
	t.Setenv("DB_PATH", filepath.Join(s.dataDir, "apteva.db"))
	t.Setenv("DATA_DIR", s.dataDir)
	t.Setenv("SERVER_SECRET", hex.EncodeToString(s.secret))
	t.Setenv("APTEVA_INTERNAL_SERVER_URL", "http://127.0.0.1:"+s.port)
}
