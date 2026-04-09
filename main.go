package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version is set at build time via -ldflags.
var Version = "dev"

type Server struct {
	store       *Store
	instances   *InstanceManager
	mcpManager  *MCPManager
	catalog     *AppCatalog
	secret      []byte  // AES-256 key for encrypting provider data
	port        string  // server port for telemetry callback
	dataDir     string  // data directory for downloads, etc.
	publicURL   string  // public base URL for webhooks (e.g. "https://agents.example.com")
	broadcaster *TelemetryBroadcaster
	setupToken     string  // one-time token for first registration (empty after use)
	regMode        string  // "open", "locked", "setup" — controls registration
	instanceSecret string  // shared secret for MCP and telemetry auth
}

func main() {
	// Check for MCP server modes
	for i, arg := range os.Args[1:] {
		if arg == "--mcp-proxy" {
			var connID int64
			for _, a := range os.Args[i+2:] {
				if strings.HasPrefix(a, "--connection-id=") {
					connID, _ = strconv.ParseInt(strings.TrimPrefix(a, "--connection-id="), 10, 64)
				}
			}
			dbPath := os.Getenv("DB_PATH")
			if dbPath == "" {
				dbPath = "apteva-server.db"
			}
			dataDir := os.Getenv("DATA_DIR")
			if dataDir == "" {
				dataDir = "data"
			}
			secret, err := LoadSecret(dataDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "secret: %v\n", err)
				os.Exit(1)
			}
			if err := runMCPProxy(dbPath, connID, secret); err != nil {
				fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if arg == "--mcp-gateway" {
			var userID int64
			for _, a := range os.Args[i+2:] {
				if strings.HasPrefix(a, "--user-id=") {
					userID, _ = strconv.ParseInt(strings.TrimPrefix(a, "--user-id="), 10, 64)
				}
			}
			dbPath := os.Getenv("DB_PATH")
			if dbPath == "" {
				dbPath = "apteva-server.db"
			}
			dataDir := os.Getenv("DATA_DIR")
			if dataDir == "" {
				dataDir = "data"
			}
			secret, err := LoadSecret(dataDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "secret: %v\n", err)
				os.Exit(1)
			}
			if err := runMCPGateway(dbPath, userID, secret); err != nil {
				fmt.Fprintf(os.Stderr, "mcp-gateway: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "apteva-server.db"
	}

	coreCmd := os.Getenv("CORE_CMD")
	if coreCmd == "" {
		coreCmd = "apteva-core"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	store, err := NewStore(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	secret, err := LoadSecret(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load encryption key: %v\n", err)
		os.Exit(1)
	}

	catalog := NewAppCatalog()
	appsDir := os.Getenv("APPS_DIR")
	if appsDir == "" {
		// Try dev path first (relative to repo), then downloaded path
		devPath := filepath.Join(dataDir, "..", "..", "integrations", "src", "apps")
		downloadedPath := filepath.Join(dataDir, "integrations")
		if info, err := os.Stat(devPath); err == nil && info.IsDir() {
			appsDir = devPath
		} else {
			appsDir = downloadedPath
		}
	}
	if err := catalog.LoadFromDir(appsDir); err != nil {
		fmt.Fprintf(os.Stderr, "no integration catalog found (download via dashboard Settings)\n")
	} else {
		fmt.Fprintf(os.Stderr, "loaded %d integrations from catalog\n", catalog.Count())
	}

	publicURL := os.Getenv("PUBLIC_URL") // e.g. "https://agents.example.com"

	// Determine registration mode
	regMode := os.Getenv("APTEVA_REGISTRATION") // "open", "locked", or empty
	setupToken := ""
	if regMode == "" {
		// Check if any users exist
		hasUsers := store.HasUsers()
		if hasUsers {
			regMode = "locked"
		} else {
			regMode = "setup"
			setupToken = "apt_" + generateToken(16)
			fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
			fmt.Fprintf(os.Stderr, "  Setup token: %s\n", setupToken)
			fmt.Fprintf(os.Stderr, "  Use this to create the first admin account.\n")
			fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		}
	}

	s := &Server{
		store:       store,
		instances:   NewInstanceManager(dataDir, coreCmd, 3210),
		mcpManager:  NewMCPManager(),
		catalog:     catalog,
		secret:      secret,
		port:        port,
		dataDir:     dataDir,
		publicURL:   publicURL,
		broadcaster:    NewTelemetryBroadcaster(),
		setupToken:     setupToken,
		regMode:        regMode,
		instanceSecret: generateToken(16),
	}

	// Start console telemetry logger
	if os.Getenv("QUIET") != "1" {
		console := NewConsoleLogger(s.broadcaster, store)
		go console.Run()
	}

	mux := http.NewServeMux()

	// Public routes (no auth)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": Version})
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": Version})
	})
	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/auth/me", s.handleMe)

	// Authenticated routes
	mux.HandleFunc("/auth/keys", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListKeys(w, r)
		case http.MethodPost:
			s.handleCreateKey(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/auth/keys/", s.authMiddleware(s.handleDeleteKey))

	// Telemetry routes (ingest is unauthenticated — core instances POST here)
	mux.HandleFunc("/telemetry/timeline", s.authMiddleware(s.handleTelemetryTimeline))
	mux.HandleFunc("/telemetry/stats", s.authMiddleware(s.handleTelemetryStats))
	mux.HandleFunc("/telemetry/stream", s.handleTelemetryStream) // SSE — no auth (needs cookie passthrough)
	mux.HandleFunc("/telemetry/live", s.handleLiveTelemetry)     // broadcast-only ingest for chunks
	mux.HandleFunc("/telemetry", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.handleIngestTelemetry(w, r)
		case http.MethodGet:
			s.authMiddleware(s.handleQueryTelemetry)(w, r)
		case http.MethodDelete:
			s.authMiddleware(s.handleWipeTelemetry)(w, r)
		default:
			http.Error(w, "GET, POST, or DELETE", http.StatusMethodNotAllowed)
		}
	})

	// Webhook receiver (unauthenticated — external services POST here)
	mux.HandleFunc("/webhooks/", s.handleWebhook)

	// Subscription management
	mux.HandleFunc("/subscriptions", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListSubscriptions(w, r)
		case http.MethodPost:
			s.handleCreateSubscription(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/subscriptions/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
		if strings.HasSuffix(path, "/enable") || strings.HasSuffix(path, "/disable") {
			s.handleToggleSubscription(w, r)
		} else if strings.HasSuffix(path, "/test") {
			s.handleTestSubscription(w, r)
		} else {
			s.handleDeleteSubscription(w, r)
		}
	}))

	// MCP Streamable HTTP endpoint (no auth — MCP clients connect directly)
	mux.HandleFunc("/mcp/", s.handleMCPEndpoint)

	// Projects
	mux.HandleFunc("/projects", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListProjects(w, r)
		case http.MethodPost:
			s.handleCreateProject(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/projects/", s.authMiddleware(s.handleProject))

	// Integration catalog routes
	mux.HandleFunc("/integrations/catalog/status", s.authMiddleware(s.handleCatalogStatus))
	mux.HandleFunc("/integrations/catalog/download", s.authMiddleware(s.handleCatalogDownload))
	mux.HandleFunc("/integrations/catalog/", s.authMiddleware(s.handleGetCatalogApp))
	mux.HandleFunc("/integrations/catalog", s.authMiddleware(s.handleListCatalog))

	// Connection routes
	mux.HandleFunc("/connections", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListConnections(w, r)
		case http.MethodPost:
			s.handleCreateConnection(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/connections/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/connections/")
		if strings.HasSuffix(path, "/tools") {
			s.handleConnectionTools(w, r)
		} else if strings.HasSuffix(path, "/execute") {
			s.handleExecuteTool(w, r)
		} else {
			s.handleDeleteConnection(w, r)
		}
	}))

	// MCP server routes
	mux.HandleFunc("/mcp-servers", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListMCPServers(w, r)
		case http.MethodPost:
			s.handleCreateMCPServer(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/mcp-servers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
		if strings.HasSuffix(path, "/start") {
			s.handleStartMCPServer(w, r)
		} else if strings.HasSuffix(path, "/stop") {
			s.handleStopMCPServer(w, r)
		} else if strings.HasSuffix(path, "/tools") {
			s.handleMCPServerTools(w, r)
		} else {
			s.handleDeleteMCPServer(w, r)
		}
	}))

	// Provider routes
	mux.HandleFunc("/provider-types", s.authMiddleware(s.handleListProviderTypes))
	mux.HandleFunc("/providers", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListProviders(w, r)
		case http.MethodPost:
			s.handleCreateProvider(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/providers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/providers/")
		if strings.HasSuffix(path, "/models") {
			s.handleProviderModels(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleGetProvider(w, r)
		case http.MethodPut:
			s.handleUpdateProvider(w, r)
		case http.MethodDelete:
			s.handleDeleteProvider(w, r)
		default:
			http.Error(w, "GET, PUT, POST, or DELETE", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/instances", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))

	// Instance routes — need to distinguish /instances/:id from /instances/:id/...
	mux.HandleFunc("/instances/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/instances/")

		// /instances/:id/config
		if strings.HasSuffix(path, "/config") {
			s.handleUpdateConfig(w, r)
			return
		}

		// /instances/:id/stop
		if strings.HasSuffix(path, "/stop") {
			s.handleStopInstance(w, r)
			return
		}

		// /instances/:id/start
		if strings.HasSuffix(path, "/start") {
			s.handleStartInstance(w, r)
			return
		}

		// /instances/:id/channels — list connected channels
		if strings.HasSuffix(path, "/channels") && !strings.Contains(path, "/channels/") {
			s.handleListChannels(w, r)
			return
		}

		// /instances/:id/channels/cli/reply — CLI sends answer to pending ask
		if strings.HasSuffix(path, "/channels/cli/reply") {
			s.handleCLIReply(w, r)
			return
		}

		// /instances/:id/channels/telegram — connect/disconnect telegram
		if strings.HasSuffix(path, "/channels/telegram") {
			s.handleTelegramConnect(w, r)
			return
		}

		// /instances/:id/status, /instances/:id/threads, /instances/:id/pause, etc. → proxy
		if strings.Contains(path, "/") {
			s.handleProxy(w, r)
			return
		}

		// /instances/:id
		s.handleInstance(w, r)
	}))

	// Dashboard — served from disk (always up-to-date, copied by CLI on startup)
	// Falls back to embedded dashboard if disk copy not found
	appDashDir := filepath.Join(dataDir, "dashboard")
	if _, err := os.Stat(filepath.Join(appDashDir, "index.html")); err == nil {
		appFS := http.FileServer(http.Dir(appDashDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// Let API routes handle their paths (registered before this)
			relPath := r.URL.Path
			if relPath == "/" {
				http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
				return
			}
			// Try static file
			filePath := filepath.Join(appDashDir, relPath)
			if _, err := os.Stat(filePath); err == nil {
				appFS.ServeHTTP(w, r)
				return
			}
			// SPA fallback
			http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
		})
	} else {
		// Fallback: embedded dashboard (stale but better than nothing)
		mux.Handle("/", dashboardHandler())
	}

	fmt.Fprintf(os.Stderr, "apteva-server v%s running on :%s\n", Version, port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func atoi64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
