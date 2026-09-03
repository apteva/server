package main

import (
	"context"
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

func cloneQuarantineEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_CLONE_QUARANTINE")))
	return v == "1" || v == "true"
}

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

func writeServerHealth(w http.ResponseWriter, ready bool) {
	info := versionInfo()
	info["ok"] = ready
	if !ready {
		info["status"] = "starting"
		writeJSONStatus(w, http.StatusServiceUnavailable, info)
		return
	}
	writeJSON(w, info)
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
	store                *Store
	dbPath               string // path to apteva-server.db on disk (needed for staged restore)
	agents               *AgentManager
	ready                atomic.Bool
	mcpManager           *MCPManager
	catalog              *AppCatalog
	secret               []byte // AES-256 key for encrypting provider data
	integrationWebhookMu sync.Mutex
	// Optional test transport for the public integration media relay. Nil in
	// production, where the relay installs its SSRF-hardened transport.
	integrationRelayTransport http.RoundTripper
	// Optional transport used by managed-LLM gateway tests. Production uses
	// the default HTTPS transport.
	managedLLMTransport http.RoundTripper
	// agentConfigLocks serializes read/modify/write updates to one agent's
	// config. MCP attachment changes are additive at the API, but ultimately
	// core consumes one desired mcp_servers list; without this lock two
	// concurrent attach/detach or directive edits can overwrite each other.
	agentConfigLocks sync.Map // agent id -> *sync.Mutex
	// agentSkillLocks serializes reconciliation of app-owned skill memories for
	// one agent. App attachment changes, app upgrades, and startup repair can
	// otherwise race and create parallel supersede chains for the same skill.
	agentSkillLocks     sync.Map // agent id -> *sync.Mutex
	port                string   // server port for telemetry callback
	dataDir             string   // data directory for downloads, etc.
	appsDir             string   // path to integration app definitions
	integrationsUIDir   string   // path to built integration UI bundles (dist/ui/<slug>/<file>.mjs)
	publicURL           string   // public base URL for webhooks (e.g. "https://agents.example.com")
	broadcaster         *TelemetryBroadcaster
	setupToken          string                     // one-time token for first registration (empty after use)
	regMode             string                     // "open", "locked", "setup" — controls registration
	instanceSecret      string                     // shared secret for MCP and telemetry auth
	platformGatewayExec platformGatewayExecuteFunc // test seam for Helper's HTTP management MCP
	startupIntent       agentLifecycleIntent
	agentRollouts       *agentRolloutCoordinator
	// apps holds the loaded Apteva Apps registry. Apps attach to
	// instance lifecycle via NotifyInstanceAttach/Detach and expose
	// HTTP routes under /api/apps/<slug>/. Nil before startApps().
	apps *appsRegistry

	// installedApps is the new sidecar-based Apps system (see
	// apps_loader.go) — third-party apps deployed via the orchestrator,
	// referenced from the app_installs table. Coexists with apps for
	// now; the long-term plan is for built-ins to graduate to this
	// registry too.
	installedApps *InstalledAppsRegistry

	// platformStatus polls the published version manifest and exposes
	// "update available" info to the dashboard. The actual update
	// action lives in the `apteva update` CLI subcommand.
	platformStatus  *platformStatusPoller
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
	// corsConfig is shared by the path router and HostRouter so custom
	// app:// ingress hosts enforce the same live browser-origin policy as
	// /api/apps/<slug>/ routes.
	corsConfig *corsConfig

	// ingressCerts is the server-native ACME manager. It is backed by
	// the ingress_routes table for host policy and falls back to the
	// legacy certs app cache for compatibility.
	ingressCerts *IngressCertManager

	// In-process edge cache for HostRouter-proxied responses the origin
	// marked publicly cacheable (Cache-Control: public, max-age>0).
	// See edge_cache.go.
	edgeCache *EdgeCache
	// geoCountry is an optional local Country MMDB reader (DB-IP by default,
	// with MaxMind and operator-owned files supported). It enriches public app
	// ingress only and remains inactive when no valid database is available.
	geoCountry countryLookup

	// manifestRefreshInFlight gates the background goroutine launched
	// by handleListApps that refreshes manifest_json from upstream.
	// Without this, every dashboard poll on a cold cache spawns its
	// own N-fetch sweep — fine functionally (fetchAndCacheManifest
	// is per-URL cached) but multiplicatively wasteful.
	manifestRefreshInFlight atomic.Bool
	mobilePushCancel        context.CancelFunc

	// appEventDispatcher bridges AppEventBus → subscriptions of
	// source='app_event'. One bus subscriber per (app, project)
	// lane, fanning to all matching rows. Reconciled on subscription
	// CRUD; gap-free across server restarts via per-row
	// last_seq_delivered + bus since-cursor.
	appEventDispatcher *AppEventDispatcher
	// Polling dispatcher runs integration-declared webhooks.events with
	// delivery='poll'. Rows remain source='webhook' so this is an
	// alternate delivery mode, not a separate subscription source.
	pollingDispatcher *PollingSubscriptionDispatcher
	// agentEventLifecycle relays Core's durable tracked-event outbox into
	// telemetry and retries delivery to the originating app's /events handler.
	agentEventLifecycle *AgentEventLifecycleService

	// liveTelemetryHook is an optional callback invoked for every
	// batch of events received on /telemetry/live, after enrichment
	// and broadcaster fanout. Used by channelchat to tap into the
	// LLM tool-args stream and surface partial `channels_respond`
	// text on the chat SSE. Gated by CHANNELCHAT_STREAMING != "0"
	// so the whole feature can be turned off with a single env var
	// without redeploying. Set once at startApps boot.
	liveTelemetryHook func([]TelemetryEvent)
	latestLLMDone     latestLLMDoneCache

	appTokenMu sync.Mutex

	// environments supervises isolated test Environments — sets of real app
	// sidecars sharing one HTTP edge (see environment.go / environment_edge.go).
	// Nil-safe: only ever consulted by environment endpoints, so production
	// agent paths never touch it.
	environments *EnvironmentManager

	// projectPresetPlanner is a deliberately narrow, no-tools planning hook.
	// Production uses the caller's configured LLM provider when available;
	// tests can replace it without starting Core or creating a durable thread.
	projectPresetPlanner projectPresetPlannerFunc
	// Narrow lifecycle seam for Helper activation tests. Production leaves it
	// nil and starts the real managed Core through ensureMetaAgentRunning.
	platformHelperStarter func(int64) (*Agent, error)
}

// appsRegistry is a thin alias over framework.Registry so main.go
// doesn't need to import the framework package just to hold a pointer.
// Defined in apps_wire.go's import path.
type appsRegistry = framework.Registry

func main() {
	invocation, err := parseServerInvocation(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "apteva-server: %v\n\n%s\n", err, serverUsage())
		os.Exit(2)
	}
	switch invocation.mode {
	case serverModeVersion:
		fmt.Printf("apteva-server %s (build %s)\n", Version, BuildTime)
		return
	case serverModeHelp:
		fmt.Println(serverUsage())
		return
	case serverModePreflight:
		// Fast-exit health gate the CLI runs against a freshly-extracted
		// binary before flipping the load-bearing bin/current symlink.
		os.Exit(runPreflight())
	case serverModeMCPProxy:
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
		if err := runMCPProxy(dbPath, invocation.connectionID, secret); err != nil {
			fmt.Fprintf(os.Stderr, "mcp-proxy: %v\n", err)
			os.Exit(1)
		}
		return
	case serverModeMCPGateway:
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
		if err := runMCPGateway(dbPath, invocation.userID, secret); err != nil {
			fmt.Fprintf(os.Stderr, "mcp-gateway: %v\n", err)
			os.Exit(1)
		}
		return
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
	aptevaCfg, aptevaCfgPath, err := loadAptevaConfig(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load Apteva config: %v\n", err)
		os.Exit(1)
	}
	if aptevaCfgPath != "" {
		fmt.Fprintf(os.Stderr, "loaded Apteva config: %s\n", aptevaCfgPath)
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
	if marked, err := store.MarkLegacyCJDropshippingConnectionsForReconnect(secret); err != nil {
		fmt.Fprintf(os.Stderr, "CJ Dropshipping connection migration failed: %v\n", err)
	} else if marked > 0 {
		fmt.Fprintf(os.Stderr, "marked %d legacy CJ Dropshipping connection(s) for reconnection\n", marked)
	}
	if user, err := applyAptevaBootstrap(store, aptevaCfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to apply Apteva bootstrap: %v\n", err)
		os.Exit(1)
	} else if user != nil {
		fmt.Fprintf(os.Stderr, "bootstrapped Apteva admin: %s\n", user.Email)
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
	if publicURL == "" && aptevaCfg != nil {
		publicURL = aptevaCfg.Server.PublicURL
	}

	// Determine registration mode.
	regMode := os.Getenv("APTEVA_REGISTRATION") // "open", "locked", "setup", or empty
	if regMode == "" && aptevaCfg != nil {
		regMode = aptevaCfg.Server.Registration
	}
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
		store:             store,
		dbPath:            dbPath,
		agents:            NewAgentManager(dataDir, coreCmd),
		mcpManager:        NewMCPManager(),
		catalog:           catalog,
		appsDir:           appsDir,
		integrationsUIDir: integrationsUIDir,
		secret:            secret,
		port:              port,
		dataDir:           dataDir,
		publicURL:         publicURL,
		broadcaster:       NewTelemetryBroadcaster(),
		setupToken:        setupToken,
		regMode:           regMode,
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
		environments:   NewEnvironmentManager(environmentDataRoot(dataDir)),
		geoCountry:     newManagedGeoCountryLookup(dataDir),
	}
	s.startupIntent = s.readLifecycleIntent(false)
	quarantined := cloneQuarantineEnabled()
	s.agentRollouts = newAgentRolloutCoordinator(s.updateAgentCore)
	s.installCapabilityMemoryHooks()
	s.ingressCerts = NewIngressCertManager(s)
	// Back-reference so Environments can drive real (install-backed) app
	// seeding + teardown. Only ever used by environment endpoints.
	s.environments.server = s

	// Start console telemetry logger
	if os.Getenv("QUIET") != "1" {
		console := NewConsoleLogger(s.broadcaster, store)
		go console.Run()
	}

	// Start the platform-update poller in the background. First poll
	// fires immediately so /api/platform-status has data on the very
	// first dashboard render after boot.
	if !quarantined {
		go s.platformStatus.Run()
		go s.startTelemetryRetention()
		go s.startDelegatedAPIKeyRetention()
		go s.startWorkspaceLifecycle()
	}

	// Platform helpers are lazy by default. Eagerly booting one helper
	// per provider-backed user makes updates noisy and can emit a burst of
	// telemetry before anyone asks for dashboard help. Keep an
	// opt-in escape hatch for deployments that prefer warm helpers.
	if !quarantined && envTruthy(os.Getenv("APTEVA_BOOT_META_AGENTS")) {
		go s.bootMetaAgents()
	}

	if !quarantined {
		s.initSlack()
		s.initEmail()
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
		writeServerHealth(w, s.ready.Load())
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, versionInfo())
	})
	// Agent-facing bridge for custom subprocess MCP servers. Loopback-only;
	// unlike the dashboard API it intentionally has no browser auth because
	// apteva-core is the caller.
	mux.HandleFunc("/mcp/custom/", s.handleCustomMCPBridge)
	mux.HandleFunc("/mcp/runtime/", s.handleRuntimeManagedMCPBridge)
	// Also expose health/version under /api for uniformity from the dashboard.
	apiMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeServerHealth(w, s.ready.Load())
	})
	apiMux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, versionInfo())
	})
	// Managed installation enrollment/reconciliation has its own workload
	// identity and deliberately bypasses dashboard session auth.
	apiMux.HandleFunc("/managed/", s.handleManagedPublic)

	// Platform self-update status — read-only view of the latest
	// published bundle vs. our own baked-in versions. The dashboard
	// reads this to render the "update available" pill; the action
	// itself lives in the `apteva update` CLI subcommand.
	apiMux.HandleFunc("/platform-status", s.handlePlatformStatus)
	apiMux.HandleFunc("/platform-status/refresh", s.handlePlatformStatusRefresh)

	apiMux.HandleFunc("/auth/status", s.handleAuthStatus)
	apiMux.HandleFunc("/auth/register", s.handleRegister)
	apiMux.HandleFunc("/auth/login", s.handleLogin)
	apiMux.HandleFunc("/auth/mfa/verify", s.handleMFAVerify)
	apiMux.HandleFunc("/auth/logout", s.handleLogout)
	apiMux.HandleFunc("/auth/me", s.handleMe)
	apiMux.HandleFunc("/auth/password", s.authMiddleware(s.handleChangePassword))
	apiMux.HandleFunc("/auth/mfa", s.authMiddleware(s.handleMFAStatus))
	apiMux.HandleFunc("/auth/mfa/enroll", s.authMiddleware(s.handleMFAEnroll))
	apiMux.HandleFunc("/auth/mfa/confirm", s.authMiddleware(s.handleMFAConfirm))
	apiMux.HandleFunc("/auth/mfa/disable", s.authMiddleware(s.handleMFADisable))
	apiMux.HandleFunc("/auth/mfa/recovery-codes", s.authMiddleware(s.handleMFARecoveryCodes))
	apiMux.HandleFunc("/auth/preferences", s.authMiddleware(s.handleAuthPreferences))
	apiMux.HandleFunc("/auth/delegated-users", s.authMiddleware(s.handleCreateDelegatedUser))
	apiMux.HandleFunc("/ui-layout/projects/", s.authMiddleware(s.handleUILayoutSurface))
	apiMux.HandleFunc("/ui/surfaces/", s.authMiddleware(s.handleUISurfaceResolution))
	apiMux.HandleFunc("/ui/contributions", s.authMiddleware(s.handleUIContributions))
	apiMux.HandleFunc("/presets", s.authMiddleware(s.handlePresets))
	apiMux.HandleFunc("/presets/capture", s.authMiddleware(s.handlePresetCapture))
	apiMux.HandleFunc("/presets/", s.authMiddleware(s.handlePresetByID))
	// Canonical product API. /presets remains an alias for older clients.
	apiMux.HandleFunc("/templates", s.authMiddleware(s.handlePresets))
	apiMux.HandleFunc("/templates/", s.authMiddleware(s.handlePresetByID))
	// Compatibility catalog for older dashboards and SDK clients. New code
	// should use the generic /presets envelope.
	apiMux.HandleFunc("/project-presets", s.authMiddleware(s.handleProjectPresets))
	apiMux.HandleFunc("/auth/onboarding/complete", s.authMiddleware(s.handleCompleteOnboarding))
	apiMux.HandleFunc("/mobile/push/config", s.authMiddleware(s.handleMobilePushConfig))
	apiMux.HandleFunc("/mobile/push/subscriptions", s.authMiddleware(s.handleMobilePushSubscriptions))
	apiMux.HandleFunc("/mobile/push/subscriptions/", s.authMiddleware(s.handleMobilePushSubscription))

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
	// Push provisioning uses the instance's normal admin API key. It remains
	// inert unless an authenticated controller explicitly applies desired state.
	apiMux.HandleFunc("/provisioning/apply", s.authMiddleware(s.handleProvisioningApply))

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
	// Agent cores use their row-scoped core_ credential here. The handler
	// performs its own narrow authentication and never accepts user API keys.
	apiMux.HandleFunc("/llm/chat/completions", s.handleManagedLLMChat)
	apiMux.HandleFunc("/telemetry/live", s.handleLiveTelemetry) // broadcast-only ingest for chunks
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
	// Tokens resolve to subscription rows registered with external services.
	// Opaque values keep internal subscription and project ids out of the URL.
	mux.HandleFunc("/webhooks/email", s.handleEmailWebhook)
	mux.HandleFunc("/webhooks/", s.handleWebhook)
	// Token-authenticated realtime audio bridge. Mounted outside the regular
	// dashboard auth middleware so telephony sidecars can connect.
	mux.HandleFunc("/api/realtime/audio", s.handleRealtimeAudioProxy)

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
		} else if strings.HasSuffix(path, "/notify-agent") {
			s.handleSetSubscriptionNotifyAgent(w, r)
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

	// Multi-user invite endpoints. /invites/:token is auth-required
	// (the accept handler binds the invite to the caller's user_id),
	// but /invites/:token preview is also auth-only because the
	// dashboard fetches it from the logged-in /login route after the
	// user clicks the invite link.
	apiMux.HandleFunc("/invites/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// /invites/:token            → GET  preview
		// /invites/:token/accept     → POST accept
		if strings.HasSuffix(r.URL.Path, "/accept") {
			s.handleInviteAccept(w, r)
			return
		}
		s.handleInvitePreview(w, r)
	}))

	// /admin/users — platform-admin only. List + role changes.
	apiMux.HandleFunc("/admin/users", s.authMiddleware(s.handleAdminUsers))
	apiMux.HandleFunc("/admin/users/", s.authMiddleware(s.handleAdminUsers))
	// Live, server-wide CORS origin registry. The handler enforces platform
	// admin access; there is intentionally no dashboard UI dependency.
	apiMux.HandleFunc("/admin/cors-origins", s.authMiddleware(s.handleAdminCORSOrigins))
	apiMux.HandleFunc("/admin/cors-origins/", s.authMiddleware(s.handleAdminCORSOrigins))

	// Integration catalog routes
	apiMux.HandleFunc("/integrations/usage", s.authMiddleware(s.handleIntegrationUsage))

	// Integration UI bundles. Serves /api/integrations/<slug>/ui/<file>
	// from the integrations dist tree the dashboard picked up at build
	// time. Public — no auth — same as /vendor/*.mjs, since bundles are
	// not credential-scoped. The components fetch credential-scoped
	// data through separate authenticated endpoints.
	apiMux.HandleFunc("/integrations/", s.handleIntegrationStatic)

	apiMux.HandleFunc("/integrations/catalog/reload", s.authMiddleware(s.handleCatalogReload))
	apiMux.HandleFunc("/integrations/catalog/status", s.authMiddleware(s.handleCatalogStatus))
	apiMux.HandleFunc("/integrations/catalog/download", s.authMiddleware(s.handleCatalogDownload))
	apiMux.HandleFunc("/integrations/catalog/", s.authMiddleware(s.handleIntegrationURLProperties))
	apiMux.HandleFunc("/integrations/catalog", s.authMiddleware(s.handleListCatalog))
	// Public provider verification files and encrypted, short-lived media relay.
	apiMux.HandleFunc("/relay/", s.handleIntegrationRelay)

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

	// Server-wide settings (public_url, access policy, and lifecycle). GET
	// returns effective values to authenticated users with managed connection
	// identifiers redacted for non-admins. PUT is platform-admin-only.
	apiMux.HandleFunc("/settings/server", s.authMiddleware(s.handleServerSettings))
	apiMux.HandleFunc("/ingress/routes", s.authMiddleware(s.handleIngressRoutes))
	apiMux.HandleFunc("/ingress/routes/", s.authMiddleware(s.handleIngressRoute))
	apiMux.HandleFunc("/ingress/certs", s.authMiddleware(s.handleIngressCerts))

	// GET /api/connections/runtime — the Models settings tab's list:
	// connections whose catalog entry declares a `runtime` block,
	// in the same precedence order GetProviderPool resolves them.
	apiMux.HandleFunc("/connections/runtime", s.authMiddleware(s.handleListRuntimeConnections))

	apiMux.HandleFunc("/connections/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/connections/")
		if strings.HasPrefix(path, "auth/") {
			s.handlePollConnectionDeviceAuth(w, r)
		} else if strings.HasSuffix(path, "/primary") {
			// PATCH /api/connections/:id/primary — pick which key backs
			// the runtime when several exist for one app. Replaces the
			// providers table's implicit lowest-id dedup.
			s.handleSetConnectionPrimary(w, r)
		} else if strings.HasSuffix(path, "/runtime-config") {
			// GET/PATCH /api/connections/:id/runtime-config — model picks
			// and non-secret knobs. PATCH merges.
			s.handleConnectionRuntimeConfig(w, r)
		} else if strings.HasSuffix(path, "/usage") {
			// GET /api/connections/:id/usage — subscription quota, polled
			// live from the upstream (not integration_usage_events).
			s.handleConnectionUsage(w, r)
		} else if strings.HasSuffix(path, "/models") {
			// GET /api/connections/:id/models — live model list for the
			// Models and Helper pickers.
			s.handleConnectionModels(w, r)
		} else if strings.HasSuffix(path, "/tools") {
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
		} else if strings.HasSuffix(path, "/test") {
			// POST /api/connections/:id/test — run the app's
			// health_check probe against the stored credentials and
			// return {ok, latency_ms, status_code, error}. Wired to
			// the dashboard's per-connection "Test" button.
			s.handleTestConnection(w, r)
		} else if strings.HasSuffix(path, "/oauth/reauth") {
			// POST /api/connections/:id/oauth/reauth — start an OAuth
			// popup that refreshes tokens on the same connection row. Kept
			// as a backward-compatible alias for browser-OAuth clients.
			s.handleReauthConnection(w, r)
		} else if strings.HasSuffix(path, "/reauth") {
			// POST /api/connections/:id/reauth — refresh credentials on the
			// same connection row, dispatching by its catalog auth type.
			s.handleReauthConnection(w, r)
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
		idPart := strings.SplitN(path, "/", 2)[0]
		installID, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil || installID <= 0 {
			http.Error(w, "invalid install id", http.StatusBadRequest)
			return
		}
		need := ProjectViewer
		if r.Method != http.MethodGet {
			need = ProjectEditor
		}
		if strings.HasSuffix(path, "/agent-default") {
			need = ProjectOwner
		}
		if strings.HasSuffix(path, "/delegated-access-policies") {
			need = ProjectOwner
		}
		if _, ok := s.requireAppInstallAccess(w, r, installID, need); !ok {
			return
		}
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
		case strings.HasSuffix(path, "/tools") && r.Method == http.MethodGet:
			s.handleInstallTools(w, r)
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodGet:
			s.handleGetInstallConfig(w, r)
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodPut:
			s.handleSetInstallConfig(w, r)
		case strings.HasSuffix(path, "/imports") && r.Method == http.MethodGet:
			s.handleGetInstallImports(w, r)
		case strings.Contains(path, "/imports/") && strings.HasSuffix(path, "/run") && r.Method == http.MethodPost:
			s.handleRunInstallImport(w, r)
		case strings.HasSuffix(path, "/scope") && r.Method == http.MethodPatch:
			// Move an install between project / global scope without
			// destroying its data. See apps_scope.go for the contract.
			s.handleSetInstallScope(w, r)
		case strings.HasSuffix(path, "/agent-default") && r.Method == http.MethodPatch:
			s.handleSetInstallAgentDefault(w, r)
		case strings.HasSuffix(path, "/delegated-access-policies"):
			s.handleDelegatedAccessPolicies(w, r)
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
	s.registerAppRuntimeRoutes(apiMux)

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
	apiMux.HandleFunc("/mcp-servers/managed", s.authMiddleware(s.handleCreateManagedMCPServer))
	apiMux.HandleFunc("/mcp-servers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
		if strings.HasSuffix(path, "/start") {
			s.handleStartMCPServer(w, r)
		} else if strings.HasSuffix(path, "/stop") {
			s.handleStopMCPServer(w, r)
		} else if strings.HasSuffix(path, "/managed") {
			s.handleManagedMCPServer(w, r)
		} else if strings.HasSuffix(path, "/validate") {
			s.handleValidateManagedMCPServer(w, r)
		} else if strings.HasSuffix(path, "/logs") {
			s.handleManagedMCPLogs(w, r)
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
	// Isolated managed-MCP runners exchange their row-scoped HMAC capability
	// here for calls to explicitly bound apps/integrations. The handler itself
	// enforces loopback, revision token, alias, and project scope.
	apiMux.HandleFunc("/managed-mcp-runtime/", s.handleManagedMCPRuntimeGateway)
	apiMux.HandleFunc("/runtime-managed-mcp/", s.handleRuntimeManagedMCPGateway)

	// Provider routes — reduced to the one endpoint that must outlive
	// the providers table.
	//
	// apteva-core builds this URL itself from the OPENAI_CODEX_PROVIDER_ID
	// we inject at spawn, and holds it for the life of the process, so a
	// core started before the providers/connections fusion keeps calling
	// /api/providers/<old id>/auth/runtime-token forever. handleRuntimeToken
	// serves a provider row if one still exists and otherwise resolves the
	// migrated connection via legacy_provider_id. See runtime_token.go.
	//
	// Everything else the providers table used to expose — CRUD, types,
	// model discovery, usage, device auth, credential probes — now lives
	// on connections. See runtime_connection_handlers.go.
	apiMux.HandleFunc("/providers/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(strings.TrimPrefix(r.URL.Path, "/providers/"), "/auth/runtime-token") {
			s.handleRuntimeToken(w, r)
			return
		}
		http.Error(w, "providers have been replaced by connections; see /api/connections/runtime", http.StatusGone)
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

	apiMux.HandleFunc("/platform/helper", s.authMiddleware(s.handlePlatformHelper))
	apiMux.HandleFunc("/platform/helper/status", s.authMiddleware(s.handlePlatformHelperStatus))
	apiMux.HandleFunc("/platform/helper/activate", s.authMiddleware(s.handlePlatformHelperActivate))
	apiMux.HandleFunc("/platform/helper/deactivate", s.authMiddleware(s.handlePlatformHelperDeactivate))
	apiMux.HandleFunc("/platform/helper/capabilities", s.authMiddleware(s.handlePlatformHelperCapabilities))
	// Private capability-token gateway used by temporary runtime agents to
	// reach dynamic MCP sessions exposed by the runtime-owning app.
	apiMux.HandleFunc("/runtime-mcp-gateway/", s.handleRuntimeMCPGateway)

	// /environments, /environment-snapshots — isolated test environments.
	s.registerEnvironmentRoutes(apiMux)

	// /environment-mcp — Environment control surface as MCP tools
	// (create/seed/list/destroy).
	apiMux.HandleFunc("/environment-mcp", s.handleEnvironmentMCP)
	// Helper's normal HTTP management MCP. The app-shaped path makes Core
	// inject its hidden caller thread identity; the handler resolves that
	// identity through the server-owned agent_thread_scopes table.
	apiMux.HandleFunc("/apps/apteva-server/mcp", s.handlePlatformMCP)

	// /environment-app-gateway/<environmentID>/<app>/... — token-brokering
	// proxy so an in-environment agent core can reach token-protected app
	// sidecars.
	apiMux.HandleFunc("/environment-app-gateway/", s.handleEnvironmentAppGateway)
	apiMux.HandleFunc("/environment-app-public/", s.handleEnvironmentAppPublicGateway)

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
	apiMux.HandleFunc("/agents/core-rollout", s.authMiddleware(s.handleCoreRollout))

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
		agentIDPart := strings.SplitN(path, "/", 2)[0]
		agentID, err := strconv.ParseInt(agentIDPart, 10, 64)
		if err != nil || agentID <= 0 {
			http.Error(w, "invalid agent id", http.StatusBadRequest)
			return
		}
		need := ProjectViewer
		if r.Method != http.MethodGet {
			need = ProjectEditor
		}
		if _, ok := s.requireAgentAccess(w, r, agentID, need); !ok {
			return
		}

		// /instances/:id/config
		if strings.HasSuffix(path, "/config") {
			s.handleUpdateConfig(w, r)
			return
		}

		// /instances/:id/mcp-servers — atomically add/remove registered MCP
		// inventory rows without making callers replace the complete config.
		if strings.HasSuffix(path, "/mcp-servers") {
			s.handleAgentMCPServers(w, r)
			return
		}

		// /instances/:id/background-memory — inspect or toggle the
		// per-agent unconscious consolidation thread. Runtime changes
		// require a controlled restart so core boot owns spawn/teardown.
		if strings.HasSuffix(path, "/background-memory") {
			s.handleBackgroundMemory(w, r)
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

		// /instances/:id/core-update — replace just this running core with
		// the server's bundled target version through the rollout coordinator.
		if strings.HasSuffix(path, "/core-update") {
			s.handleAgentCoreUpdate(w, r, agentID)
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

		// /instances/:id/status, /instances/:id/threads, /instances/:id/pause,
		// /instances/:id/control, etc. → proxy
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
	// CORS is disabled by default because the bundled dashboard is same-origin.
	// Set CORS_ORIGIN to a comma-separated allowlist for trusted remote UIs,
	// "*" for credential-free public clients, or "off" explicitly.
	corsCfg := newCORSConfig(os.Getenv("CORS_ORIGIN"))
	s.corsConfig = corsCfg
	crossOriginCookies = corsCfg.needsCrossOriginCookies()
	mux.Handle("/api/", limitAPIRequestBody(compressHTTP(http.StripPrefix("/api", corsCfg.middlewareWithDynamicPolicy(apiMux, s.dynamicAppCORSPolicy)))))

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
				setStaticCacheHeaders(w, relPath, true)
				http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
				return
			}
			filePath := filepath.Join(appDashDir, relPath)
			if _, err := os.Stat(filePath); err == nil {
				setStaticCacheHeaders(w, relPath, false)
				appFS.ServeHTTP(w, r)
				return
			}
			setStaticCacheHeaders(w, relPath, true)
			http.ServeFile(w, r, filepath.Join(appDashDir, "index.html"))
		})
	} else {
		dashboardSPA = dashboardHandler()
	}
	mux.Handle("/", compressHTTP(s.staticAppHandler(dashboardSPA)))

	// Boot-time recovery: any user agent still left in `status='running'`
	// was intentionally active before the previous server process stopped.
	// Walk those rows and re-spawn fresh cores + channels MCPs so updates
	// and crashes recover as a brief pause. Platform-owned helpers stay
	// lazy and are skipped by the resume path.
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
		if !quarantined {
			go s.ResumeRunningInstances()
		}
	}

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
	if !quarantined {
		s.startMobilePushWorker()
	}

	// Sidecar-based Apps system (v2). Reads app_installs from the DB
	// and registers reverse proxies + manifest-derived MCP rows for
	// every installed app on boot. Failures don't block boot; broken
	// installs surface in the dashboard's Apps tab.
	s.installedApps = NewInstalledAppsRegistry()
	s.appBus = NewAppEventBus()
	// Bridge AppEventBus → subscriptions(source='app_event'). Started
	// here so by the time the HTTP listener accepts requests every
	// active app-event subscription is already wired to its lane. Reconcile
	// restores each in-memory producer counter from the durable subscription
	// cursors before sidecars can publish. The process-local ring only replays
	// reconnect gaps within this server process; it does not survive restart.
	s.appEventDispatcher = NewAppEventDispatcher(s)
	s.pollingDispatcher = NewPollingSubscriptionDispatcher(s)
	if !quarantined {
		s.appEventDispatcher.Start()
		s.pollingDispatcher.Start()
	}
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

	// Move legacy `providers` rows onto `connections` (providers/
	// connections fusion). Runs after the catalog is loaded because the
	// catalog's runtime.env block is what maps a provider's env-var-keyed
	// blob onto a connection's credential fields. Idempotent and
	// non-destructive — provider rows are left in place, and a row whose
	// credentials disagree with an existing connection is logged rather
	// than merged. See provider_migration.go.
	s.migrateProvidersToConnections()

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
	httpServer := &http.Server{
		Handler:           hostRouter,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// WriteTimeout remains zero because telemetry and chat use long-lived SSE.
	}
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()
	httpIngressAddr, httpsIngressAddr := ingressListenAddrs(listenAddr)
	if !quarantined && s.ingressCerts != nil && httpsIngressAddr != "" {
		s.ingressCerts.Start(60 * time.Second)
	}
	if httpIngressAddr != "" && httpIngressAddr != listenAddr {
		startIngressHTTPListener(httpIngressAddr, hostRouter)
	}
	if httpsIngressAddr != "" {
		startIngressTLSListener(httpsIngressAddr, hostRouter, s.ingressCerts)
	}

	if quarantined {
		if err := s.PrepareCloneLocalRuntimes(); err != nil {
			log.Printf("[CLONE-QUARANTINE] runtime preparation failed: %v", err)
		} else {
			log.Printf("[CLONE-QUARANTINE] runtimes prepared; agents, apps, subscriptions, and refresh workers remain stopped")
		}
	} else {
		// Seed exact install IDs, credentials, and persisted local sidecar
		// URLs before process resume. ResumeLocalInstalls starts bound targets
		// before their callers; this initial registry keeps authorization
		// correct throughout that sequence instead of returning a false
		// "app not bound" while a valid target is starting.
		s.LoadInstalledApps()
		s.ResumeLocalInstalls()
		s.PruneInstalledAppVersions()
		s.ResumePendingLocalInstalls()
	}
	s.LoadInstalledApps()
	if !quarantined {
		s.agentEventLifecycle = NewAgentEventLifecycleService(s)
		s.agentEventLifecycle.Start()
		s.RemountStaticApps()
		s.backfillAppMCPs()
		// Reconstruct integration MCP rows synchronously before cores resume.
		// A background backfill allowed an agent to boot with an unresolved
		// connection URL during the startup recovery window.
		s.BackfillMissingMCPServers()
		// Repair legacy app_agent_bindings drift from each agent's actual
		// persisted MCP configuration before cores resume.
		s.reconcileAllAgentAppBindings()
	}
	// Apply the repeatable binding migration on every boot. The current
	// manifest is authoritative: obsolete integration/app-dependency
	// grants are pruned and missing required app dependencies are
	// backfilled.
	if !quarantined {
		s.reconcileAllAppDepBindings()
		s.recomputePendingOptions()
		s.ResumeManagedMCPs()
	}

	// Apps are healthy and the installedApps registry is populated.
	// Now safe to spawn agents — their MCP proxies will resolve.
	resumeInstancesAfterApps()
	if !quarantined {
	}
	s.ready.Store(true)
	if !quarantined && aptevaCfg != nil && aptevaCfg.Managed.ControllerURL != "" {
		s.startManagedTenantReconciler(context.Background(), aptevaCfg.Managed)
	}
	if !quarantined && providerAuthRefreshEnvEnabled() {
		s.startProviderAuthRefresher(context.Background())
	}

	go func() {
		sig := <-sigCh
		s.ready.Store(false)
		fmt.Fprintf(os.Stderr, "\napteva-server received %s — shutting down\n", sig)
		intent := s.readLifecycleIntent(false)
		shutdownPolicy := s.resolvedShutdownPolicy(intent)
		preserveAgents := shutdownPolicy == "preserve" || shutdownPolicy == "rolling"
		log.Printf("[LIFECYCLE] shutdown reason=%s agent_policy=%s", map[bool]string{true: intent.Reason, false: "stop"}[intent.Reason != ""], shutdownPolicy)
		if preserveAgents {
			if rows, err := s.store.ListAgentsByStatus("running"); err == nil {
				for i := range rows {
					if rows[i].Kind != "" && rows[i].Kind != "user" {
						s.agents.Stop(rows[i].ID)
					}
				}
			}
		}
		if stopped, err := s.store.MarkPlatformAgentsStoppedForShutdown(); err != nil {
			log.Printf("[SHUTDOWN] mark platform agents stopped: %v", err)
		} else if stopped > 0 {
			log.Printf("[SHUTDOWN] marked %d platform agent(s) stopped for clean shutdown", stopped)
		}
		s.stopMobilePushWorker()
		if s.agentEventLifecycle != nil {
			s.agentEventLifecycle.Stop()
		}
		if s.geoCountry != nil {
			if err := s.geoCountry.Close(); err != nil {
				log.Printf("[GEOIP] close country database: %v", err)
			}
		}
		s.stopApps(appsReg)
		if preserveAgents {
			log.Printf("[SHUTDOWN] leaving user agent core process(es) alive for reattach")
		} else {
			s.agents.StopAll(5 * time.Second)
		}
		if s.environments != nil {
			s.environments.StopAll()
		}
		// Sidecars are spawned with Setpgid; StopAll fans out SIGTERM
		// and falls back to SIGKILL after the grace window. Without
		// this, every clean apteva-server exit leaves the running
		// app sidecars (code, deploy, storage, …) as orphans for
		// the next boot's cleanup pass to mop up — works, but noisy.
		if s.localApps != nil {
			s.localApps.StopAll(5 * time.Second)
		}
		if s.mcpManager != nil {
			s.mcpManager.StopAll()
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
