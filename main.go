package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version fields are injected at build time via -ldflags "-X main.Xxx=..."
// The Dockerfile reads each component's package.json or go.mod at build
// time and passes the extracted values, so /health reflects the actual
// shipped component versions instead of an opaque timestamp.
var (
	Version             = "dev" // apteva umbrella version (root package.json)
	BuildTime           = "dev" // ISO-ish build timestamp
	CLIVersion          = "dev" // apteva/package.json
	DashboardVersion    = "dev" // dashboard/package.json
	IntegrationsVersion = "dev" // integrations/package.json
	CoreVersion         = "dev" // core/go.mod or explicit tag
)

// versionInfo is the shape /health and /version return.
func versionInfo() map[string]any {
	return map[string]any{
		"apteva":       Version,
		"build":        BuildTime,
		"cli":          CLIVersion,
		"dashboard":    DashboardVersion,
		"integrations": IntegrationsVersion,
		"core":         CoreVersion,
	}
}

type Server struct {
	store       *Store
	instances   *InstanceManager
	mcpManager  *MCPManager
	catalog     *AppCatalog
	secret      []byte  // AES-256 key for encrypting provider data
	port        string  // server port for telemetry callback
	dataDir     string  // data directory for downloads, etc.
	appsDir     string  // path to integration app definitions
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
		instances:   NewInstanceManager(dataDir, coreCmd),
		mcpManager:  NewMCPManager(),
		catalog:     catalog,
		appsDir:     appsDir,
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

	// All REST/JSON routes live under /api/. The SPA owns everything else,
	// which means a browser refresh on /instances/42 no longer collides with
	// the API's /instances/ prefix match.
	//
	// Externally-called endpoints that can't move stay at root:
	//   - /health, /version           — public liveness checks
	//   - /webhooks/*                 — upstream services register these URLs
	//   - /oauth/local/callback       — OAuth redirect target
	//   - /mcp/*                      — core MCP Streamable HTTP endpoint
	//   - /                           — SPA catch-all
	//
	// Everything else goes on apiMux and is exposed at /api/*. Inside the
	// sub-mux the path has already been stripped, so handlers that inspect
	// r.URL.Path (e.g. strings.TrimPrefix(r.URL.Path, "/instances/")) work
	// unchanged.
	apiMux := http.NewServeMux()

	// Public routes (no auth) at root for external liveness checks.
	// /health returns ok + every injected component version so a single
	// call tells you what's running (apteva umbrella, cli, dashboard,
	// integrations, core) along with the build timestamp.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		info := versionInfo()
		info["ok"] = true
		writeJSON(w, info)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, versionInfo())
	})
	// Also expose health/version under /api for uniformity from the dashboard.
	apiMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		info := versionInfo()
		info["ok"] = true
		writeJSON(w, info)
	})
	apiMux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, versionInfo())
	})

	apiMux.HandleFunc("/auth/status", s.handleAuthStatus)
	apiMux.HandleFunc("/auth/register", s.handleRegister)
	apiMux.HandleFunc("/auth/login", s.handleLogin)
	apiMux.HandleFunc("/auth/logout", s.handleLogout)
	apiMux.HandleFunc("/auth/me", s.handleMe)

	// Authenticated routes
	apiMux.HandleFunc("/auth/keys", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListKeys(w, r)
		case http.MethodPost:
			s.handleCreateKey(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/auth/keys/", s.authMiddleware(s.handleDeleteKey))

	// Telemetry routes. Core instances also POST /telemetry and /telemetry/live
	// back to the server, so those paths also need to be reachable via /api.
	// The core was updated to target /api/telemetry{,/live} in the same pass
	// as this refactor.
	apiMux.HandleFunc("/telemetry/timeline", s.authMiddleware(s.handleTelemetryTimeline))
	apiMux.HandleFunc("/telemetry/stats", s.authMiddleware(s.handleTelemetryStats))
	apiMux.HandleFunc("/telemetry/stream", s.authMiddleware(s.handleTelemetryStream)) // SSE — cookie or API key auth
	apiMux.HandleFunc("/telemetry/live", s.handleLiveTelemetry)     // broadcast-only ingest for chunks
	apiMux.HandleFunc("/telemetry", func(w http.ResponseWriter, r *http.Request) {
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

	// Webhook receiver (unauthenticated — external services POST here).
	// One route, one handler, one URL shape: /webhooks/<opaque_token>.
	// The handler dispatches internally based on which table the token
	// matches: subscription rows (for per-sub upstream deliveries
	// registered with the external service) or provider rows (for
	// project-level trigger deliveries from Composio and friends).
	// Opaque tokens mean the URL doesn't leak project id or provider
	// kind and the route is future-proof for any new trigger backend.
	mux.HandleFunc("/webhooks/", s.handleWebhook)

	// Local OAuth2 callback (unauthenticated — upstream providers redirect here).
	// Stays at root because the redirect URI is registered with the provider.
	mux.HandleFunc("/oauth/local/callback", s.handleLocalOAuthCallback)

	// MCP Streamable HTTP endpoint (no auth — core MCP clients connect directly).
	// Stays at root because core instances connect here with a fixed URL.
	mux.HandleFunc("/mcp/", s.handleMCPEndpoint)

	// Hosted providers — proxy calls that need the stored API key
	apiMux.HandleFunc("/composio/apps", s.authMiddleware(s.handleListComposioApps))
	apiMux.HandleFunc("/composio/toolkit/", s.authMiddleware(s.handleGetComposioToolkit))
	apiMux.HandleFunc("/composio/reconcile", s.authMiddleware(s.handleComposioReconcile))

	// Subscription management
	apiMux.HandleFunc("/subscriptions", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListSubscriptions(w, r)
		case http.MethodPost:
			s.handleCreateSubscription(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/subscriptions/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
		if strings.HasSuffix(path, "/enable") || strings.HasSuffix(path, "/disable") {
			s.handleToggleSubscription(w, r)
		} else if strings.HasSuffix(path, "/test") {
			s.handleTestSubscription(w, r)
		} else {
			s.handleDeleteSubscription(w, r)
		}
	}))

	// Projects
	apiMux.HandleFunc("/projects", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListProjects(w, r)
		case http.MethodPost:
			s.handleCreateProject(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/projects/", s.authMiddleware(s.handleProject))

	// Integration catalog routes
	apiMux.HandleFunc("/integrations/catalog/reload", s.authMiddleware(s.handleCatalogReload))
	apiMux.HandleFunc("/integrations/catalog/status", s.authMiddleware(s.handleCatalogStatus))
	apiMux.HandleFunc("/integrations/catalog/download", s.authMiddleware(s.handleCatalogDownload))
	apiMux.HandleFunc("/integrations/catalog/", s.authMiddleware(s.handleGetCatalogApp))
	apiMux.HandleFunc("/integrations/catalog", s.authMiddleware(s.handleListCatalog))

	// Connection routes
	apiMux.HandleFunc("/connections", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListConnections(w, r)
		case http.MethodPost:
			s.handleCreateConnection(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	// GET /api/oauth/local/client?app_slug=github&project_id=...
	// Returns whether OAuth client credentials are saved for this user+project+app.
	// The dashboard hits this when the user picks an oauth2 app so it can
	// hide the client_id/secret form when creds already exist.
	apiMux.HandleFunc("/oauth/local/client", s.authMiddleware(s.handleOAuthClientStatus))

	// Server-wide settings (public_url and similar admin-editable things).
	// GET returns the current effective values plus their source so the
	// dashboard can show "currently using env var" vs "stored in DB". PUT
	// upserts the keys passed in the body. Locked to authenticated users —
	// in a multi-tenant deploy you'd add an admin check, but right now any
	// user with a session can edit these (server is single-tenant by
	// default and the setup-token flow ensures only the operator gets in).
	apiMux.HandleFunc("/settings/server", s.authMiddleware(s.handleServerSettings))

	apiMux.HandleFunc("/connections/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/connections/")
		if strings.HasSuffix(path, "/tools") {
			s.handleConnectionTools(w, r)
		} else if strings.HasSuffix(path, "/execute") {
			s.handleExecuteTool(w, r)
		} else if strings.HasSuffix(path, "/mcp") {
			// POST /api/connections/:id/mcp — create a scoped MCP server
			// from an existing connection. Body: { name, allowed_tools }.
			s.handleCreateScopedMCP(w, r)
		} else if strings.HasSuffix(path, "/triggers") {
			// GET /api/connections/:id/triggers — list Composio trigger
			// types available for this connection's toolkit. Only
			// meaningful for composio-source connections; returns 404
			// for local. Used by the dashboard subscription create form
			// to populate the trigger picker.
			s.handleConnectionTriggers(w, r)
		} else if r.Method == http.MethodGet {
			s.handleGetConnection(w, r)
		} else {
			s.handleDeleteConnection(w, r)
		}
	}))

	// MCP server routes
	apiMux.HandleFunc("/mcp-servers", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListMCPServers(w, r)
		case http.MethodPost:
			s.handleCreateMCPServer(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/mcp-servers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
		if strings.HasSuffix(path, "/start") {
			s.handleStartMCPServer(w, r)
		} else if strings.HasSuffix(path, "/stop") {
			s.handleStopMCPServer(w, r)
		} else if strings.HasSuffix(path, "/tools") {
			// GET  /mcp-servers/:id/tools — list tools available from the server
			// PUT  /mcp-servers/:id/tools — update the allowed_tools filter
			//   (legacy: GET used to also be handled by handleMCPServerTools —
			//    route on method to keep both working.)
			switch r.Method {
			case http.MethodGet:
				s.handleMCPServerTools(w, r)
			case http.MethodPut:
				s.handleUpdateMCPServerAllowedTools(w, r)
			default:
				http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
			}
		} else if strings.HasSuffix(path, "/call-tool") {
			s.handleCallMCPTool(w, r)
		} else {
			s.handleDeleteMCPServer(w, r)
		}
	}))

	// Composio per-toolkit action listing — powers the dashboard tool picker
	// when the user is scoping down a Composio MCP server.
	apiMux.HandleFunc("/composio/toolkits/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/composio/toolkits/")
		if strings.HasSuffix(path, "/actions") {
			s.handleListComposioToolkitActions(w, r)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))

	// Provider routes
	apiMux.HandleFunc("/provider-types", s.authMiddleware(s.handleListProviderTypes))
	apiMux.HandleFunc("/providers", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListProviders(w, r)
		case http.MethodPost:
			s.handleCreateProvider(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/providers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
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

	apiMux.HandleFunc("/instances", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
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
	apiMux.HandleFunc("/instances/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
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

		// /instances/:id/restart
		if strings.HasSuffix(path, "/restart") {
			s.handleRestartInstance(w, r)
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

	// Mount the API sub-mux under /api. http.StripPrefix rewrites r.URL.Path
	// before the sub-mux runs, so handlers that parse paths (e.g.
	// `strings.TrimPrefix(r.URL.Path, "/instances/")`) work unchanged — they
	// see the post-strip path (e.g. `/instances/42/status`) exactly as
	// before.
	mux.Handle("/api/", http.StripPrefix("/api", apiMux))

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

	// Boot-time recovery: any instance left in `status='running'` from a
	// previous server process had its core subprocess die with that
	// process group, so the DB state is stale. Walk those rows and
	// re-spawn fresh cores + channels MCPs so restarts look like a
	// brief pause rather than "all my instances silently vanished".
	// Run async so a slow resume (many instances, slow provider probe)
	// doesn't block the HTTP listener from accepting new requests.
	go s.ResumeRunningInstances()

	// Graceful shutdown: on SIGTERM or SIGINT (Ctrl+C), stop every
	// tracked core child cleanly before we exit. Prevents today's
	// "restart apteva-server and now half a dozen apteva-core zombies
	// are sitting in the process table holding ports" situation. The
	// StopAll handler uses SIGTERM → wait 5s → SIGKILL, which gives
	// cores a chance to flush session state to disk. Port-0
	// allocation (see instances.go allocPort) already ensures the
	// surviving zombie scenario no longer CAUSES new bugs; this
	// handler stops the zombies from existing at all on clean exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\napteva-server received %s — stopping children\n", sig)
		s.instances.StopAll(5 * time.Second)
		os.Exit(0)
	}()

	fmt.Fprintf(os.Stderr, "apteva-server v%s (core=%s cli=%s dashboard=%s integrations=%s build=%s) running on :%s\n",
		Version, CoreVersion, CLIVersion, DashboardVersion, IntegrationsVersion, BuildTime, port)
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
