package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apteva/server/apps/framework"
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

// loadOrMintInstanceSecret returns the persisted instance secret from
// server_settings, creating + saving a fresh one the first time. A
// stable secret across server restarts is the key to not 401-ing cores
// that were running before the restart — they keep using their
// INSTANCE_SECRET env value, which now matches.
func loadOrMintInstanceSecret(store *Store) string {
	if v := store.GetSetting("instance_secret"); v != "" {
		return v
	}
	v := generateToken(16)
	if err := store.SetSetting("instance_secret", v); err != nil {
		// Fall back to the generated token — next boot will mint a
		// new one and rotate cores, but that's still better than a
		// panic here on a write failure.
		log.Printf("[BOOT] failed to persist agent_secret: %v", err)
	}
	return v
}

type Server struct {
	store       *Store
	dbPath      string  // path to apteva-server.db on disk (needed for staged restore)
	agents   *AgentManager
	mcpManager  *MCPManager
	catalog     *AppCatalog
	secret      []byte  // AES-256 key for encrypting provider data
	port        string  // server port for telemetry callback
	dataDir     string  // data directory for downloads, etc.
	appsDir     string  // path to integration app definitions
	integrationsUIDir string // path to built integration UI bundles (dist/ui/<slug>/<file>.mjs)
	publicURL   string  // public base URL for webhooks (e.g. "https://agents.example.com")
	broadcaster *TelemetryBroadcaster
	setupToken     string  // one-time token for first registration (empty after use)
	regMode        string  // "open", "locked", "setup" — controls registration
	instanceSecret string  // shared secret for MCP and telemetry auth
	// apps holds the loaded Apteva Apps registry. Apps attach to
	// instance lifecycle via NotifyInstanceAttach/Detach and expose
	// HTTP routes under /api/apps/<slug>/. Nil before startApps().
	apps *appsRegistry

	// installedApps is the new sidecar-based Apps system (see
	// apps_loader.go) — third-party apps deployed via the orchestrator,
	// referenced from the app_installs table. Coexists with apps for
	// now; the long-term plan is for built-ins to graduate to this
	// registry too.
	installedApps   *InstalledAppsRegistry

	// platformStatus polls the published version manifest and exposes
	// "update available" info to the dashboard. The actual update
	// action lives in the `apteva update` CLI subcommand.
	platformStatus *platformStatusPoller
	appBus          *AppEventBus
	orchestratorURL string
	// localApps supervises app sidecars run as native subprocesses on
	// this host (binary spawn — no Docker). Used in single-host /
	// laptop installs; coexists with the orchestrator path for prod
	// multi-worker deployments.
	localApps *LocalSupervisor
	// staticMounts holds the live URL-prefix → handler table for every
	// kind=static app install. Rebuilt on every (un)install via
	// RemountStaticApps. Read on the request hot path through a
	// catch-all handler that delegates to the dashboard SPA when no
	// static prefix matches.
	staticMounts *staticAppMounts
	// routeCache is the in-memory hostname → target map driven by the
	// `routes` app. Hit on the request hot path through
	// maybeRouteByHost; misses fall through to path-based routing.
	// Hydrated from the routes app at boot, refreshed via SSE on
	// routes.changed events. See routes_cache.go.
	routeCache  *RouteCache
	primaryHost string // APTEVA_PRIMARY_HOST — never matched by route cache (dashboard wins).

	// manifestRefreshInFlight gates the background goroutine launched
	// by handleListApps that refreshes manifest_json from upstream.
	// Without this, every dashboard poll on a cold cache spawns its
	// own N-fetch sweep — fine functionally (fetchAndCacheManifest
	// is per-URL cached) but multiplicatively wasteful.
	manifestRefreshInFlight atomic.Bool

	// appEventDispatcher bridges AppEventBus → subscriptions of
	// source='app_event'. One bus subscriber per (app, project)
	// lane, fanning to all matching rows. Reconciled on subscription
	// CRUD; gap-free across server restarts via per-row
	// last_seq_delivered + bus since-cursor.
	appEventDispatcher *AppEventDispatcher

	// liveTelemetryHook is an optional callback invoked for every
	// batch of events received on /telemetry/live, after enrichment
	// and broadcaster fanout. Used by channelchat to tap into the
	// LLM tool-args stream and surface partial `channels_respond`
	// text on the chat SSE. Gated by CHANNELCHAT_STREAMING != "0"
	// so the whole feature can be turned off with a single env var
	// without redeploying. Set once at startApps boot.
	liveTelemetryHook func([]TelemetryEvent)

	// judgeMutexes serializes judge calls per user. The meta-agent's
	// "main" thread is shared across calls and we reset+repost on
	// every call; without this lock, two concurrent evals for the
	// same user would race on the shared context. See
	// judgeWithMetaAgent in platform_agent.go.
	judgeMutexesOnce sync.Once
	judgeMutexesMu   sync.Mutex
	judgeMutexes     map[int64]*sync.Mutex
}

// appsRegistry is a thin alias over framework.Registry so main.go
// doesn't need to import the framework package just to hold a pointer.
// Defined in apps_wire.go's import path.
type appsRegistry = framework.Registry

func main() {
	// --preflight is a fast-exit health gate the CLI runs against
	// a freshly-extracted binary BEFORE flipping the load-bearing
	// `bin/current` symlink. Loads config, opens the DB read-only,
	// binds an ephemeral high port to confirm the new build is at
	// least syntactically capable of booting on this host, then
	// exits 0. Anything that fails here means "don't activate this
	// version" — the CLI keeps the prior version active and reports
	// the failure to the operator.
	for _, arg := range os.Args[1:] {
		if arg == "--preflight" {
			os.Exit(runPreflight())
		}
	}

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

	// Path defaults derive from APTEVA_HOME when it's set. This is
	// the single source of truth for the v0.12+ install layout
	// (~/.apteva/), and `apteva service install` writes a unit
	// that only sets APTEVA_HOME — every other env var the
	// foreground CLI used to pass explicitly now gets its default
	// from here. Pre-v0.12 the server's bare defaults (8080,
	// apteva-server.db, data/) ran because the CLI always
	// supplied the right values; the systemd unit didn't, and on
	// v0.12.0 production users started serving on :8080 against a
	// fresh apteva-server.db while their real apteva.db sat
	// unread next to it. v0.12.1 ties everything to APTEVA_HOME.
	//
	// Docker / Compose / standalone server (no APTEVA_HOME) keep
	// the legacy defaults, since those operators set the env
	// vars themselves and rely on the prior defaults if not.
	home := os.Getenv("APTEVA_HOME")

	port := os.Getenv("PORT")
	if port == "" {
		if home != "" {
			port = "5280" // canonical apteva foreground port
		} else {
			port = "8080" // legacy / docker default
		}
	}

	// Auto-rollback safety net: bump the boot-attempts counter
	// before we do anything that could panic. Once /health is
	// stably 200 we'll zero it (see ScheduleHealthyMark below).
	// The CLI's rollbackIfFailed checks this counter on its next
	// invocation; ≥ rollbackThreshold while last-good differs
	// from active means the supervisor's restart loop is thrashing
	// on a broken binary, and the CLI flips bin/current back.
	if n := BumpBootAttempts(); n > 1 {
		fmt.Fprintf(os.Stderr, "boot attempt #%d (counter resets after a healthy /health)\n", n)
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		if home != "" {
			// Canonical name. apteva.db matches what the foreground
			// CLI has always passed in via DB_PATH, so v0.11 users
			// upgrading to v0.12.1 with a service install pick up
			// their existing data instead of starting fresh.
			dbPath = filepath.Join(home, "apteva.db")
		} else {
			dbPath = "apteva-server.db"
		}
	}

	// One-shot rescue for v0.12.0 → v0.12.1: that release's bare
	// default created APTEVA_HOME/apteva-server.db right next to
	// the real APTEVA_HOME/apteva.db. v0.12.1 standardises on
	// apteva.db. If we're configured to read apteva.db AND it
	// doesn't exist yet AND apteva-server.db DOES exist in the
	// same dir, rename the legacy file (and its WAL/SHM siblings)
	// so the v0.12.0 user's data isn't abandoned a second time.
	// Idempotent: subsequent boots find apteva.db present and
	// skip the rename. Conservative: never overwrites an existing
	// apteva.db.
	if home != "" && dbPath == filepath.Join(home, "apteva.db") {
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			legacy := filepath.Join(home, "apteva-server.db")
			if _, err := os.Stat(legacy); err == nil {
				_ = os.Rename(legacy, dbPath)
				_ = os.Rename(legacy+"-wal", dbPath+"-wal")
				_ = os.Rename(legacy+"-shm", dbPath+"-shm")
				fmt.Fprintf(os.Stderr, "migrated DB: apteva-server.db → apteva.db (v0.12.0 layout fix)\n")
			}
		}
	}

	coreCmd := os.Getenv("CORE_CMD")
	if coreCmd == "" {
		if home != "" {
			// In the v0.12 install layout the core binary is a
			// symlink at ~/.apteva/bin/apteva-core that resolves
			// through ~/.apteva/bin/current/. Pointing at the
			// symlink rather than a versioned path means
			// `apteva update` flips and the running server's
			// next core spawn picks up the new binary
			// transparently.
			coreCmd = filepath.Join(home, "bin", "apteva-core")
		} else {
			coreCmd = "apteva-core"
		}
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		if home != "" {
			dataDir = home
		} else {
			dataDir = "data"
		}
	}

	// If a snapshot was uploaded via /api/platform/restore, the server DB
	// it shipped sits next to dbPath as <dbPath>.restored with a marker.
	// Swap it in before opening the store. No-op when no restore is pending.
	applyPendingRestore(dbPath)

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

	// Catalog loading priority (most → least specific):
	//
	//   1. APPS_DIR env override        — operator-provided path.
	//   2. Dev path (sibling monorepo)  — only present when running
	//                                     `apteva` from the source
	//                                     tree; lets a developer
	//                                     edit JSONs without
	//                                     rebuilding.
	//   3. Embedded snapshot            — apteva-server's binary
	//                                     ships with the catalog as
	//                                     of release time. Means
	//                                     `apteva update` brings a
	//                                     fresh catalog along with
	//                                     the new binary; pre-v0.13.1
	//                                     this was missing and prod
	//                                     boxes saw stale JSONs.
	//   4. Downloaded path (legacy)     — ~/.apteva/integrations/
	//                                     from a first-boot tarball
	//                                     pull. Only relevant on
	//                                     installs that pre-date the
	//                                     embedded catalog and
	//                                     haven't been re-downloaded.
	//   5. GitHub auto-download          — last-resort safety net for
	//                                     installs without an embed
	//                                     and without a prior
	//                                     download.
	catalog := NewAppCatalog()
	appsDir := os.Getenv("APPS_DIR")
	source := ""
	if appsDir != "" {
		source = "APPS_DIR=" + appsDir
		if err := catalog.LoadFromDir(appsDir); err != nil {
			fmt.Fprintf(os.Stderr, "catalog: APPS_DIR load failed: %v\n", err)
		}
	}
	if catalog.Count() == 0 {
		devPath := filepath.Join(dataDir, "..", "..", "integrations", "src", "apps")
		if info, err := os.Stat(devPath); err == nil && info.IsDir() {
			if err := catalog.LoadFromDir(devPath); err == nil && catalog.Count() > 0 {
				source = "dev path " + devPath
				appsDir = devPath
			}
		}
	}
	if catalog.Count() == 0 {
		// Embedded snapshot — the canonical source for prod releases.
		// LoadFromFS returns (count, error). Empty embed dir is fine
		// (count=0 falls through), missing dir is an error we ignore.
		if n, _ := catalog.LoadFromFS(integrationsCatalogEmbeddedFS); n > 0 {
			source = "embedded (binary)"
		}
	}
	if catalog.Count() == 0 {
		downloadedPath := filepath.Join(dataDir, "integrations")
		if err := catalog.LoadFromDir(downloadedPath); err == nil && catalog.Count() > 0 {
			source = "downloaded " + downloadedPath
			appsDir = downloadedPath
		}
	}
	if catalog.Count() == 0 {
		fmt.Fprintf(os.Stderr, "no integration catalog found locally — auto-downloading from %s\n", catalogRepo)
		if _, _, derr := downloadIntegrationCatalog(catalog, dataDir); derr != nil {
			fmt.Fprintf(os.Stderr, "catalog auto-download failed: %v (server starting with empty catalog)\n", derr)
		} else {
			source = "downloaded from " + catalogRepo
			appsDir = filepath.Join(dataDir, "integrations")
		}
	}
	fmt.Fprintf(os.Stderr, "loaded %d integrations from catalog (source: %s)\n", catalog.Count(), source)

	// Resolve the integrations UI bundle dir — built by
	// integrations/scripts/build-ui.ts and served by
	// handleIntegrationStatic. Same dev/prod fallback shape as appsDir.
	integrationsUIDir := os.Getenv("APTEVA_INTEGRATIONS_UI_DIR")
	if integrationsUIDir == "" {
		devUIPath := filepath.Join(dataDir, "..", "..", "integrations", "dist", "ui")
		downloadedUIPath := filepath.Join(dataDir, "integrations-ui")
		if info, err := os.Stat(devUIPath); err == nil && info.IsDir() {
			integrationsUIDir = devUIPath
		} else if info, err := os.Stat(downloadedUIPath); err == nil && info.IsDir() {
			integrationsUIDir = downloadedUIPath
		}
	}
	if integrationsUIDir != "" {
		fmt.Fprintf(os.Stderr, "integrations UI bundles: %s\n", integrationsUIDir)
	}

	publicURL := os.Getenv("PUBLIC_URL") // e.g. "https://agents.example.com"

	// Determine registration mode.
	regMode := os.Getenv("APTEVA_REGISTRATION") // "open", "locked", "setup", or empty
	setupToken := os.Getenv("APTEVA_SETUP_TOKEN")
	if regMode == "" {
		// Auto-derive: empty DB → setup, otherwise locked.
		if store.HasUsers() {
			regMode = "locked"
		} else {
			regMode = "setup"
		}
	}
	// In setup mode the /auth/register handler requires a matching
	// X-Setup-Token header. Make sure we always have one. Two sources:
	//   1. APTEVA_SETUP_TOKEN env — when our parent (apteva CLI) spawned
	//      us, it minted a token, passed it down, and surfaces it in its
	//      own terminal banner. Skipping the stderr-print here keeps the
	//      CLI's banner clean and avoids leaking the token to the
	//      captured server.log.
	//   2. Otherwise (standalone invocation) — mint one and print to
	//      stderr so the operator running apteva-server directly can
	//      copy it into the dashboard.
	if regMode == "setup" && setupToken == "" {
		setupToken = "apt_" + generateToken(16)
		fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Fprintf(os.Stderr, "  Setup token: %s\n", setupToken)
		fmt.Fprintf(os.Stderr, "  Use this to create the first admin account.\n")
		fmt.Fprintf(os.Stderr, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	}

	s := &Server{
		store:       store,
		dbPath:      dbPath,
		agents:   NewAgentManager(dataDir, coreCmd),
		mcpManager:  NewMCPManager(),
		catalog:     catalog,
		appsDir:     appsDir,
		integrationsUIDir: integrationsUIDir,
		secret:      secret,
		port:        port,
		dataDir:     dataDir,
		publicURL:   publicURL,
		broadcaster:    NewTelemetryBroadcaster(),
		setupToken:     setupToken,
		regMode:        regMode,
		// instanceSecret gates both the /api/telemetry ingest path and
		// the MCP gateway's instance auth. Cores we spawn receive it as
		// INSTANCE_SECRET and send it back in X-Agent-Secret on every
		// telemetry POST. Regenerating it on every server start breaks
		// already-running cores (they keep using their old env value
		// and hit 401 forever until re-spawned), so we persist it in
		// server_settings and only mint a fresh one on first boot.
		instanceSecret: loadOrMintInstanceSecret(store),
		platformStatus: newPlatformStatusPoller(dataDir),
		primaryHost:    strings.TrimSpace(os.Getenv("APTEVA_PRIMARY_HOST")),
	}

	// Start console telemetry logger
	if os.Getenv("QUIET") != "1" {
		console := NewConsoleLogger(s.broadcaster, store)
		go console.Run()
	}

	// Start the platform-update poller in the background. First poll
	// fires immediately so /api/platform-status has data on the very
	// first dashboard render after boot.
	go s.platformStatus.Run()

	// Boot the meta-agent for every user that already has an LLM
	// provider configured. Done in a background goroutine so a slow
	// core spawn (~2-3s) doesn't delay HTTP listener start. Users
	// without a provider configured yet get lazy-start on their
	// first eval run.
	go s.bootMetaAgents()

	s.initSlack()
	s.initEmail()

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

	// Platform self-update status — read-only view of the latest
	// published bundle vs. our own baked-in versions. The dashboard
	// reads this to render the "update available" pill; the action
	// itself lives in the `apteva update` CLI subcommand.
	apiMux.HandleFunc("/platform-status", s.handlePlatformStatus)
	apiMux.HandleFunc("/platform-status/refresh", s.handlePlatformStatusRefresh)

	apiMux.HandleFunc("/auth/status", s.handleAuthStatus)
	apiMux.HandleFunc("/auth/register", s.handleRegister)
	apiMux.HandleFunc("/auth/login", s.handleLogin)
	apiMux.HandleFunc("/auth/logout", s.handleLogout)
	apiMux.HandleFunc("/auth/me", s.handleMe)
	apiMux.HandleFunc("/auth/password", s.authMiddleware(s.handleChangePassword))
	apiMux.HandleFunc("/auth/onboarding/complete", s.authMiddleware(s.handleCompleteOnboarding))

	// User administration. GET / POST are admin-only; DELETE and
	// PATCH .../password are too. GET /users/:id is self-or-admin.
	// Policy is enforced inside each handler so the rules stay
	// next to the code they apply to.
	apiMux.HandleFunc("/users", s.authMiddleware(s.handleUsers))
	apiMux.HandleFunc("/users/", s.authMiddleware(s.handleUserByID))

	// Platform-level operations. /platform/snapshot streams a tar.gz of
	// every SQLite DB the server owns (its own + each running install's),
	// captured via VACUUM INTO so dumps are consistent without stopping
	// any sidecar. Admin-only; the handler enforces the role.
	apiMux.HandleFunc("/platform/snapshot", s.authMiddleware(s.handlePlatformSnapshot))
	// /platform/restore consumes a tar.gz produced by /platform/snapshot.
	// App DBs are swapped live; the platform DB itself is staged and
	// applied on the next server boot. Requires X-Confirm-Restore: yes.
	apiMux.HandleFunc("/platform/restore", s.authMiddleware(s.handlePlatformRestore))

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
	apiMux.HandleFunc("/telemetry/project-stats", s.authMiddleware(s.handleTelemetryProjectStats))
	apiMux.HandleFunc("/telemetry/project-timeline", s.authMiddleware(s.handleTelemetryProjectTimeline))
	apiMux.HandleFunc("/telemetry/project-tools", s.authMiddleware(s.handleTelemetryProjectTools))
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
	mux.HandleFunc("/webhooks/email", s.handleEmailWebhook)
	mux.HandleFunc("/webhooks/", s.handleWebhook)

	// Local OAuth2 callback (unauthenticated — upstream providers redirect here).
	// Stays at root because the redirect URI is registered with the provider.
	mux.HandleFunc("/oauth/local/callback", s.handleLocalOAuthCallback)

	// MCP Streamable HTTP endpoint (no auth — core MCP clients connect directly).
	// Stays at root because core instances connect here with a fixed URL.
	mux.HandleFunc("/mcp/", s.handleMCPEndpoint)

	// Channel management — unified under /channels
	apiMux.HandleFunc("/channels/connect", s.authMiddleware(s.handleChannelConnect))
	apiMux.HandleFunc("/channels/disconnect/", s.authMiddleware(s.handleChannelDisconnect))
	apiMux.HandleFunc("/channels", s.authMiddleware(s.handleChannelList))

	// Slack gateway config
	apiMux.HandleFunc("/slack/configure", s.authMiddleware(s.handleSlackConfigure))
	apiMux.HandleFunc("/slack/status", s.authMiddleware(s.handleSlackStatus))
	apiMux.HandleFunc("/slack/channels", s.authMiddleware(s.handleSlackListChannels))

	// Email (AgentMail) gateway config
	apiMux.HandleFunc("/email/configure", s.authMiddleware(s.handleEmailConfigure))
	apiMux.HandleFunc("/email/status", s.authMiddleware(s.handleEmailStatus))

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
	// Integration UI bundles. Serves /api/integrations/<slug>/ui/<file>
	// from the integrations dist tree the dashboard picked up at build
	// time. Public — no auth — same as /vendor/*.mjs, since bundles are
	// not credential-scoped. The components fetch credential-scoped
	// data through separate authenticated endpoints.
	apiMux.HandleFunc("/integrations/", s.handleIntegrationStatic)

	apiMux.HandleFunc("/integrations/catalog/reload", s.authMiddleware(s.handleCatalogReload))
	apiMux.HandleFunc("/integrations/catalog/status", s.authMiddleware(s.handleCatalogStatus))
	apiMux.HandleFunc("/integrations/catalog/download", s.authMiddleware(s.handleCatalogDownload))
	apiMux.HandleFunc("/integrations/catalog/", s.authMiddleware(s.handleGetCatalogApp))
	apiMux.HandleFunc("/integrations/catalog", s.authMiddleware(s.handleListCatalog))

	// Credential-group (suite) routes. Let templates declare that N
	// apps share one key, optionally an account-scoped one that fans
	// out across external projects. See suites_handlers.go.
	apiMux.HandleFunc("/integrations/groups", s.authMiddleware(s.handleListGroups))
	apiMux.HandleFunc("/integrations/groups/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// Subpath routing:
		//   /integrations/groups/{id}                     (GET)
		//   /integrations/groups/{id}/master              (GET, POST, DELETE)
		//   /integrations/groups/{id}/master/refresh      (POST)
		//   /integrations/groups/{id}/master/enable       (POST)
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/master/refresh"):
			s.handleRefreshGroupMaster(w, r)
		case strings.HasSuffix(path, "/master/enable"):
			s.handleEnableGroupApps(w, r)
		case strings.HasSuffix(path, "/master"):
			switch r.Method {
			case http.MethodGet:
				s.handleGetGroupMaster(w, r)
			case http.MethodPost:
				s.handleCreateGroupMaster(w, r)
			case http.MethodDelete:
				s.handleDeleteGroupMaster(w, r)
			default:
				http.Error(w, "GET, POST or DELETE", http.StatusMethodNotAllowed)
			}
		default:
			s.handleGetGroup(w, r)
		}
	}))

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
		} else if strings.HasSuffix(path, "/credentials") {
			// GET /api/connections/:id/credentials — owner-only reveal.
			// Decrypts the stored blob; logged for audit.
			s.handleGetConnectionCredentials(w, r)
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
		} else if strings.HasSuffix(path, "/test") {
			// POST /api/connections/:id/test — run the app's
			// health_check probe against the stored credentials and
			// return {ok, latency_ms, status_code, error}. Wired to
			// the dashboard's per-connection "Test" button.
			s.handleTestConnection(w, r)
		} else if strings.HasSuffix(path, "/scope") && r.Method == http.MethodPatch {
			// PATCH /api/connections/:id/scope — move a connection
			// between project and global scope. Mirror of the app
			// install scope-move endpoint (v0.14.5). See
			// connections_scope.go for the contract.
			s.handleSetConnectionScope(w, r)
		} else if r.Method == http.MethodGet {
			s.handleGetConnection(w, r)
		} else if r.Method == http.MethodPatch {
			s.handleRenameConnection(w, r)
		} else {
			s.handleDeleteConnection(w, r)
		}
	}))

	// Connection invites — operator mints a shareable link (authed);
	// the public fetch + fulfill endpoints live under /public/ WITHOUT
	// authMiddleware so a client without a dashboard login can complete
	// the flow using only the signed token.
	apiMux.HandleFunc("/invites", s.authMiddleware(s.handleCreateInvite))
	apiMux.HandleFunc("/public/invites/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/public/invites/")
		if strings.HasSuffix(rest, "/fulfill") {
			// Rewrite path so handleFulfillInvite's TrimPrefix logic works
			// unchanged (it expects /connect/:token/fulfill).
			r.URL.Path = "/connect/" + strings.TrimSuffix(rest, "/fulfill") + "/fulfill"
			s.handleFulfillInvite(w, r)
			return
		}
		r.URL.Path = "/connect/" + strings.TrimSuffix(rest, "/")
		s.handlePublicInvite(w, r)
	})

	// MCP server routes
	// /api/apps* — sidecar-based Apps system. /apps/<name>/* reverse-
	// proxies into installed app sidecars; /apps/* (the management
	// surface) handles install / list / bind. See apps_handlers.go.
	apiMux.HandleFunc("/apps", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleListApps(w, r)
			return
		}
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/apps/marketplace", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleMarketplace(w, r)
			return
		}
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
	}))
	// App event bus — generic SDK-level pub/sub for app→dashboard live UI.
	// Sidecars POST emits via APTEVA_APP_TOKEN; browsers SSE-subscribe via
	// cookie/API-key auth. Mounted under /api/app-events/ to sidestep the
	// catch-all /api/apps/<name>/... proxy further down.
	apiMux.HandleFunc("/app-events/internal/emit", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleAppEventEmit(w, r)
			return
		}
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/app-events/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.handleAppEventStream(w, r)
			return
		}
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/apps/callback/", s.authMiddleware(s.handleAppCallback))
	apiMux.HandleFunc("/apps/preview", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handlePreviewApp(w, r)
			return
		}
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/apps/install", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleInstallApp(w, r)
			return
		}
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
	}))
	apiMux.HandleFunc("/apps/install/preflight", s.authMiddleware(s.handlePreflightApp))

	// Skills — app-shipped + user-authored playbooks. v1 stores +
	// serves them; the agent runtime integration is a follow-up.
	apiMux.HandleFunc("/skills", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListSkills(w, r)
		case http.MethodPost:
			s.handleCreateSkill(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/skills/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/enabled") && r.Method == http.MethodPut:
			s.handleSetSkillEnabled(w, r)
		case r.Method == http.MethodGet:
			s.handleGetSkill(w, r)
		case r.Method == http.MethodPut:
			s.handleUpdateSkill(w, r)
		case r.Method == http.MethodDelete:
			s.handleDeleteSkill(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/apps/installs/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
		switch {
		case strings.HasSuffix(path, "/status") && r.Method == http.MethodPut:
			s.handleSetInstallStatus(w, r)
		case strings.HasSuffix(path, "/instances") && r.Method == http.MethodPut:
			s.handleSetInstallBindings(w, r)
		case strings.HasSuffix(path, "/upgrade") && r.Method == http.MethodPost:
			s.handleUpgradeApp(w, r)
		case strings.HasSuffix(path, "/bindings") && r.Method == http.MethodPut:
			s.handleSetInstallBindings2(w, r)
		case strings.HasSuffix(path, "/preflight") && r.Method == http.MethodGet:
			s.handlePreflightInstalled(w, r)
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodGet:
			s.handleGetInstallConfig(w, r)
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodPut:
			s.handleSetInstallConfig(w, r)
		case strings.HasSuffix(path, "/scope") && r.Method == http.MethodPatch:
			// Move an install between project / global scope without
			// destroying its data. See apps_scope.go for the contract.
			s.handleSetInstallScope(w, r)
		case strings.HasSuffix(path, "/permissions"),
			strings.HasSuffix(path, "/default-effect"),
			strings.Contains(path, "/grants"):
			s.handleInstallGrants(w, r)
		case !strings.Contains(path, "/") && r.Method == http.MethodDelete:
			s.handleUninstallApp(w, r)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	// Reverse-proxy: any non-management /apps/<name>/... goes to the
	// installed app's sidecar. Registered LAST so /apps/preview etc.
	// match first via Go's longest-prefix rule.
	apiMux.HandleFunc("/apps/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// Don't shadow management routes — this branch only fires when
		// the prefix is /apps/<name>/... and <name> isn't a reserved word.
		path := strings.TrimPrefix(r.URL.Path, "/apps/")
		first := path
		if i := strings.Index(path, "/"); i >= 0 {
			first = path[:i]
		}
		switch first {
		case "preview", "install", "installs", "marketplace", "callback":
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAppProxy(w, r)
	}))

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
		} else if r.Method == http.MethodPatch {
			s.handleRenameMCPServer(w, r)
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
		// POST /providers/:id/test — run the provider's credential
		// probe and return a ProviderTestResult. Pairs with the
		// pre-flight gate in handleCreateProvider so operators get
		// the same green-tick / red-X feedback after the row was
		// saved as they did on the create form.
		if strings.HasSuffix(path, "/test") && r.Method == http.MethodPost {
			s.handleTestProvider(w, r)
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

	// /agent-templates — wizard's starter-config catalog. Builtin
	// rows are seeded inline in store.migrate(); apps can contribute
	// rows via their manifest (apps_loader upserts on install/upgrade);
	// users can save their own rows. See server/agent_templates.go.
	apiMux.HandleFunc("/agent-templates", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListAgentTemplates(w, r)
		case http.MethodPost:
			s.handleCreateAgentTemplate(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	apiMux.HandleFunc("/agent-templates/", s.authMiddleware(s.handleAgentTemplateByID))

	// /evals/preview — wizard's Verify step uses this to run an
	// eval against a draft (uncreated) agent. No DB writes; the
	// returned EvalRun has ID=0. After the agent is created, the
	// regular /agents/:id/evals/:evalId/run path persists results.
	apiMux.HandleFunc("/evals/preview", s.authMiddleware(s.handleEvalPreview))

	// /agents/seed-directive — synthesize a starter directive from
	// an eval's goals. Used by the wizard's "Suggest from goals"
	// button so operators don't have to hand-write a directive for
	// simple cases. See platform_agent.go.
	apiMux.HandleFunc("/agents/seed-directive", s.authMiddleware(s.handleSeedDirective))

	// /evals/preview/stream — SSE counterpart of /evals/preview that
	// emits a per-iteration event so the wizard can pause-and-confirm
	// between steps. See eval_streaming.go.
	apiMux.HandleFunc("/evals/preview/stream", s.authMiddleware(s.handleEvalPreviewStream))

	// /eval-runs/:run_id/step — operator's continue/stop signal for an
	// in-flight streaming run. Paired with /evals/preview/stream and
	// /agents/:id/evals/:evalId/run/stream.
	apiMux.HandleFunc("/eval-runs/", s.authMiddleware(s.handleEvalStepControl))

	// /eval-mock-gateway/<token>[/...] — HTTP MCP endpoint that
	// spawned eval cores talk to in place of the real gateway.
	// Authenticates via the token in the path (it IS the credential).
	// See eval_runner.go for the runner that registers the session
	// and agent_evals.go for the handler logic.
	apiMux.HandleFunc("/eval-mock-gateway/", s.handleEvalMockGateway)

	instancesCollectionHandler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	})
	apiMux.HandleFunc("/instances", instancesCollectionHandler)
	// Phase 2 alias — /agents is the canonical name going forward.
	// Both prefixes hit the same handler; internal path parsing below
	// normalises /agents/... back to /instances/... so handlers that
	// strings.TrimPrefix on "/instances/" keep working unchanged.
	apiMux.HandleFunc("/agents", instancesCollectionHandler)

	// Agent routes — need to distinguish /instances/:id from /instances/:id/...
	instancesItemHandler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// Normalize /agents/... → /instances/... so the sub-path
		// dispatch below (and downstream handlers that re-parse the
		// path) only ever has to know one shape. The redirect is
		// invisible to the client: same URL on the response, same
		// handler logic, just rewritten before strings.TrimPrefix.
		if strings.HasPrefix(r.URL.Path, "/agents/") {
			r.URL.Path = "/instances/" + strings.TrimPrefix(r.URL.Path, "/agents/")
		}
		path := strings.TrimPrefix(r.URL.Path, "/instances/")

		// /instances/:id/config
		if strings.HasSuffix(path, "/config") {
			s.handleUpdateConfig(w, r)
			return
		}

		// /instances/:id/stop
		if strings.HasSuffix(path, "/stop") {
			log.Printf("[LIFECYCLE] POST %s remote=%s ua=%q referer=%q",
				r.URL.Path, r.RemoteAddr, r.UserAgent(), r.Referer())
			s.handleStopInstance(w, r)
			return
		}

		// /instances/:id/start
		if strings.HasSuffix(path, "/start") {
			log.Printf("[LIFECYCLE] POST %s remote=%s ua=%q referer=%q",
				r.URL.Path, r.RemoteAddr, r.UserAgent(), r.Referer())
			s.handleStartInstance(w, r)
			return
		}

		// /instances/:id/restart
		if strings.HasSuffix(path, "/restart") {
			log.Printf("[LIFECYCLE] POST %s remote=%s ua=%q referer=%q",
				r.URL.Path, r.RemoteAddr, r.UserAgent(), r.Referer())
			s.handleRestartInstance(w, r)
			return
		}

		// /instances/:id/system-mcp — enable/disable an auto-injected
		// system MCP (apteva-server, channels). Body: {name, enable}.
		// Sets the corresponding include_* flag on inst.Config; takes
		// effect on the next Start(). Response includes restart_required.
		if strings.HasSuffix(path, "/system-mcp") {
			s.handleSystemMCPToggle(w, r)
			return
		}

		// /instances/:id/chat-history — reconstructed chat from telemetry
		if strings.HasSuffix(path, "/chat-history") {
			s.handleChatHistory(w, r)
			return
		}

		// /instances/:id/channels — list connected channels (read-only, used by instance view)
		if strings.HasSuffix(path, "/channels") && !strings.Contains(path, "/channels/") {
			s.handleListChannels(w, r)
			return
		}

		// /instances/:id/skills        — list assigned skills + drift status
		// /instances/:id/skills/:skill — POST assign / DELETE unassign
		if strings.HasSuffix(path, "/skills") || strings.Contains(path, "/skills/") {
			s.handleInstanceSkills(w, r)
			return
		}

		// /instances/:id/evals                  — list / create
		// /instances/:id/evals/:evalId          — get / update / delete
		// /instances/:id/evals/:evalId/run      — POST execute the eval
		// /instances/:id/evals/:evalId/runs     — GET run history
		if strings.HasSuffix(path, "/evals") || strings.Contains(path, "/evals/") {
			s.handleAgentEvals(w, r)
			return
		}

		// /instances/:id/status, /instances/:id/threads, /instances/:id/pause, etc. → proxy
		if strings.Contains(path, "/") {
			s.handleProxy(w, r)
			return
		}

		// /instances/:id
		s.handleInstance(w, r)
	})
	apiMux.HandleFunc("/instances/", instancesItemHandler)
	// Phase 2 alias for /agents/<id>[/subroute].
	apiMux.HandleFunc("/agents/", instancesItemHandler)

	// Mount the API sub-mux under /api. http.StripPrefix rewrites r.URL.Path
	// before the sub-mux runs, so handlers that parse paths (e.g.
	// `strings.TrimPrefix(r.URL.Path, "/instances/")`) work unchanged — they
	// see the post-strip path (e.g. `/instances/42/status`) exactly as
	// before.
	// CORS: default is "permissive" (echo any origin with credentials)
	// so browser UIs hosted anywhere can authenticate out of the box.
	// Override CORS_ORIGIN with a comma-separated allowlist to lock it
	// down, "*" for API-key only clients, or "off" to disable entirely.
	corsCfg := newCORSConfig(os.Getenv("CORS_ORIGIN"))
	crossOriginCookies = corsCfg.needsCrossOriginCookies()
	mux.Handle("/api/", http.StripPrefix("/api", corsCfg.middleware(apiMux)))

	// Dashboard — served from disk (always up-to-date, copied by CLI on startup)
	// Falls back to embedded dashboard if disk copy not found.
	//
	// Static apps (kind=static installs) are layered ABOVE the
	// dashboard: requests first run through s.staticAppHandler, which
	// matches the longest registered mount prefix. Anything that
	// doesn't match a static-app prefix falls through to the dashboard
	// SPA below. RemountStaticApps() rebuilds the prefix table on
	// every install / uninstall.
	appDashDir := filepath.Join(dataDir, "dashboard")
	var dashboardSPA http.Handler
	if _, err := os.Stat(filepath.Join(appDashDir, "index.html")); err == nil {
		appFS := http.FileServer(http.Dir(appDashDir))
		dashboardSPA = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			relPath := r.URL.Path
			if relPath == "/" {
				http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
				return
			}
			filePath := filepath.Join(appDashDir, relPath)
			if _, err := os.Stat(filePath); err == nil {
				appFS.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
		})
	} else {
		dashboardSPA = dashboardHandler()
	}
	mux.Handle("/", s.staticAppHandler(dashboardSPA))

	// Boot-time recovery: any instance left in `status='running'` from a
	// previous server process had its core subprocess die with that
	// process group, so the DB state is stale. Walk those rows and
	// re-spawn fresh cores + channels MCPs so restarts look like a
	// brief pause rather than "all my instances silently vanished".
	//
	// IMPORTANT: deferred until AFTER ResumeLocalInstalls +
	// LoadInstalledApps below. Agents bind to app MCP servers via
	// the loopback proxy at /api/apps/<name>/mcp; the proxy resolves
	// the install through the installedApps registry. If we resume
	// instances first, the agent boots, calls initialize on the
	// docs proxy, and the registry isn't populated yet — proxy
	// returns "not found", the agent's connectAndRegisterMCP logs
	// an error and silently drops docs from its connected list for
	// the rest of the process lifetime. Symptom: "MCP server
	// disconnected" on every page reload until manual reconnect.
	resumeInstancesAfterApps := func() {
		go s.ResumeRunningInstances()
	}

	// One-shot on boot: any local connection that's missing its
	// auto-created mcp_servers row gets one. Covers suite-fan-out
	// children created before the auto-MCP hook was added to the
	// suite handler; also defensive against any race where the MCP
	// insert failed after the connection insert succeeded.
	go s.BackfillMissingMCPServers()

	// Graceful shutdown: on SIGTERM or SIGINT (Ctrl+C), stop every
	// tracked core child cleanly before we exit. Prevents today's
	// "restart apteva-server and now half a dozen apteva-core zombies
	// are sitting in the process table holding ports" situation. The
	// StopAll handler uses SIGTERM → wait 5s → SIGKILL, which gives
	// cores a chance to flush session state to disk. Port-0
	// allocation (see agents.go allocPort) already ensures the
	// surviving zombie scenario no longer CAUSES new bugs; this
	// handler stops the zombies from existing at all on clean exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	// Boot the Apteva Apps framework AFTER the rest of the mux is
	// set up so apps can mount routes under /api/apps/<slug>/ without
	// racing the primary handlers. The returned registry holds the
	// lifecycle handle; we Stop it on shutdown before killing
	// instances so apps can flush state that depends on per-instance
	// channels.
	appsReg, err := s.startApps(apiMux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apps startup failed: %v\n", err)
		os.Exit(1)
	}
	s.apps = appsReg

	// Sidecar-based Apps system (v2). Reads app_installs from the DB
	// and registers reverse proxies + manifest-derived MCP rows for
	// every installed app on boot. Failures don't block boot; broken
	// installs surface in the dashboard's Apps tab.
	s.installedApps = NewInstalledAppsRegistry()
	s.appBus = NewAppEventBus()
	// Bridge AppEventBus → subscriptions(source='app_event'). Started
	// here so by the time the HTTP listener accepts requests every
	// active app-event subscription is already wired to its lane and
	// any events published during boot are picked up via the bus's
	// since-cursor (replays from the ring up to last_seq_delivered).
	s.appEventDispatcher = NewAppEventDispatcher(s)
	s.appEventDispatcher.Start()
	s.orchestratorURL = os.Getenv("ORCHESTRATOR_URL")
	if s.orchestratorURL == "" {
		s.orchestratorURL = "http://46.224.26.45:8099"
	}
	// Local-spawn supervisor: cache binaries under
	// $APTEVA_HOME/apps (with $HOME/.apteva/apps as the legacy
	// fallback). Both kept-then-absolutised below — Go's GOMODCACHE
	// rejects relative paths with
	//
	//   go: GOMODCACHE entry is relative; must be absolute path
	//
	// and a systemd unit without an explicit HOME= would otherwise
	// silently produce ".apteva/apps" because os.Getenv("HOME")
	// returns "" inside the unit, joined to a relative path that
	// only works as long as cwd happens to be the parent of the
	// install root. Bit our prod operator on v0.12.0 installs.
	cacheBase := os.Getenv("APTEVA_APPS_CACHE")
	if cacheBase == "" {
		if h := os.Getenv("APTEVA_HOME"); h != "" {
			cacheBase = filepath.Join(h, "apps")
		} else if uh, err := os.UserHomeDir(); err == nil && uh != "" {
			cacheBase = filepath.Join(uh, ".apteva", "apps")
		} else {
			// Last resort: cwd-rooted. Not ideal but at least we
			// can absolutise it before handing to Go.
			cacheBase = ".apteva/apps"
		}
	}
	if abs, err := filepath.Abs(cacheBase); err == nil {
		cacheBase = abs
	}
	s.localApps = NewLocalSupervisor(cacheBase)
	s.RegisterBuiltinApps()

	// Initialise the static-mount table NOW so the listener (started
	// next) can serve requests safely before RemountStaticApps fills
	// it in below. The handler dereferences s.staticMounts.match()
	// unconditionally; a nil receiver panics — which is what
	// happened on the first revision of this boot reorder.
	if s.staticMounts == nil {
		s.staticMounts = newStaticAppMounts()
	}

	// Bind the HTTP listener BEFORE spawning sidecars. Sidecars call
	// back into the platform during their own OnMount (WhoAmI to
	// resolve bindings, GetConnectionCredentials to read S3 keys,
	// etc.). If the listener isn't bound yet, those callbacks get
	// connection-refused; the SDK silently treats that as "no
	// bindings" and the sidecar mounts on disk fallback even though
	// the operator has wired up an integration. The
	// storage-mounts-on-disk-after-server-restart bug came from
	// this race.
	hostRouter := NewHostRouter(s, mux)
	hostRouter.Start(5 * time.Second)
	// Bind interface. Explicit APTEVA_BIND env wins. When unset, we
	// default by context:
	//
	//   - System service install (root + APTEVA_HOME set, the
	//     v0.12+ systemd-system shape) → 0.0.0.0. This is the
	//     "real server on a VPS" case; binding loopback only
	//     would leave the dashboard unreachable from outside the
	//     box, which is the bug we're fixing in v0.14.4 for any
	//     v0.14.x install that upgrades without re-running
	//     `apteva service install` (whose generated unit now
	//     sets APTEVA_BIND explicitly).
	//
	//   - Anything else (foreground `apteva`, user-scope launchd /
	//     systemd, docker without explicit bind) → 127.0.0.1.
	//     Safe-by-default: don't publish to a network nobody
	//     asked us to.
	//
	// Auth gates every /api/ route + the dashboard, and the setup
	// token gates the first registration, so binding 0.0.0.0 on a
	// system service is the expected shape rather than a footgun.
	bindAddr := strings.TrimSpace(os.Getenv("APTEVA_BIND"))
	if bindAddr == "" {
		if os.Getenv("APTEVA_HOME") != "" && os.Geteuid() == 0 {
			bindAddr = "0.0.0.0"
		} else {
			bindAddr = "127.0.0.1"
		}
	}
	listenAddr := bindAddr + ":" + port
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", listenAddr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "apteva-server listening on %s\n", listenAddr)
	go func() {
		if err := http.Serve(listener, hostRouter); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()
	if tlsAddr := strings.TrimSpace(os.Getenv("APTEVA_TLS_LISTEN_ADDR")); tlsAddr != "" {
		certCache := NewCertCache(s)
		certCache.Start(60 * time.Second)
		startTLSListener(tlsAddr, hostRouter, certCache)
	}

	s.ResumeLocalInstalls()
	s.LoadInstalledApps()
	s.RemountStaticApps()
	s.backfillAppMCPs()
	// Backfill any missing requires.apps[].name bindings on running
	// installs. Catches parents installed before the cascade learned
	// to write them and out-of-order installs (parent first, dep
	// later). Idempotent — only writes when a key is genuinely
	// missing, so re-runs on every boot are cheap.
	s.reconcileAllAppDepBindings()
	s.recomputePendingOptions()

	// Apps are healthy and the installedApps registry is populated.
	// Now safe to spawn agents — their MCP proxies will resolve.
	resumeInstancesAfterApps()

	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\napteva-server received %s — stopping children\n", sig)
		s.stopApps(appsReg)
		s.agents.StopAll(5 * time.Second)
		// Sidecars are spawned with Setpgid; StopAll fans out SIGTERM
		// and falls back to SIGKILL after the grace window. Without
		// this, every clean apteva-server exit leaves the running
		// app sidecars (code, deploy, storage, …) as orphans for
		// the next boot's cleanup pass to mop up — works, but noisy.
		if s.localApps != nil {
			s.localApps.StopAll(5 * time.Second)
		}
		os.Exit(0)
	}()

	fmt.Fprintf(os.Stderr, "apteva-server v%s (core=%s cli=%s dashboard=%s integrations=%s build=%s) running on :%s\n",
		Version, CoreVersion, CLIVersion, DashboardVersion, IntegrationsVersion, BuildTime, port)

	// Now that the listener is up, schedule the auto-rollback
	// "this build is healthy" promotion. After healthyDuration of
	// continuous /health success the goroutine writes
	// last-good-version + zeros boot-attempts. See boot_status.go
	// for the contract; the CLI side is rollbackIfFailed in
	// apteva/layout.go.
	ScheduleHealthyMark(port, Version)

	// Listener was started earlier (before sidecar spawn) so apps can
	// call back during their OnMount. Just block here until shutdown.
	select {}
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
