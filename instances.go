package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apteva/server/apps/framework"
)

type runningInstance struct {
	cmd        *exec.Cmd
	port       int
	coreAPIKey string // API key injected into core for auth
	channels   *InstanceChannels // channel infrastructure for this instance
}

type InstanceManager struct {
	mu        sync.RWMutex
	processes map[int64]*runningInstance // instanceID → running process + port
	dataDir   string
	coreCmd   string // path to core binary

	// PostChannelsInit is invoked right after an instance's
	// ChannelRegistry is created and the CLI bridge is registered,
	// but BEFORE the channels MCP server boots and the core binary
	// is spawned. The Apteva Apps framework uses this hook to
	// register per-instance channels (chat, helpdesk, …) so they're
	// visible in the channels MCP tool list the agent discovers.
	//
	// The hook receives the Instance directly — it MUST NOT call
	// back into any InstanceManager accessor that takes im.mu
	// (GetPort, GetCoreAPIKey, GetChannels, …) because Start
	// already holds im.mu.Lock() and Go's sync.RWMutex is not
	// reentrant: the re-acquire would deadlock silently.
	//
	// Leave nil in tests or single-instance bring-up paths that
	// don't have an apps registry yet.
	PostChannelsInit func(inst *Instance, ic *InstanceChannels)
}

func NewInstanceManager(dataDir, coreCmd string) *InstanceManager {
	os.MkdirAll(dataDir, 0755)
	return &InstanceManager{
		processes: make(map[int64]*runningInstance),
		dataDir:   dataDir,
		coreCmd:   coreCmd,
	}
}

// allocPort asks the OS for a free ephemeral port by binding to :0 and
// immediately closing the listener. The kernel returns a high-numbered
// port that's guaranteed free at the instant of the Listen call. We
// hand that port to the child process, which binds it itself a few ms
// later.
//
// This replaces the old counter+probe approach that made us vulnerable
// to orphaned cores from previous apteva-server runs hijacking the same
// port and poisoning the in-memory map. Port 0 allocation makes that
// class of failure structurally impossible: the OS simply never returns
// a port that's currently bound, so zombies can't collide.
//
// Cross-platform: binds-and-closes works identically on Linux, macOS,
// Windows, BSD — every OS's TCP stack exposes port 0 allocation.
//
// Residual race: between our Close() and the child's subsequent
// net.Listen (~10ms window), another process could in theory grab the
// same high-numbered port. In practice this never happens — ephemeral
// ranges are thousands of ports wide and kernels spread allocations
// across them. If it ever does, the child's Listen fails and our
// spawn-health-check catches it; the next Start call gets a different
// port.
func (im *InstanceManager) allocPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// Should be unreachable on any platform we target — the
		// kernel always has ephemeral ports to hand out. Fall back to
		// a high fixed port and let the caller's bind surface the
		// eventual error.
		log.Printf("[SPAWN] port 0 allocation failed: %v — falling back to 0 (let child pick)", err)
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func (im *InstanceManager) instanceDir(id int64) string {
	dir := filepath.Join(im.dataDir, fmt.Sprintf("instance_%d", id))
	os.MkdirAll(dir, 0755)
	return dir
}

// ProviderInfo holds provider metadata for config.json injection.
type ProviderInfo struct {
	Type         string
	ModelLarge   string
	ModelMedium  string
	ModelSmall   string
	BuiltinTools []string
}

// Start launches a core process for the given instance.
// providerEnv contains decrypted provider env vars to inject.
// providerPool provides LLM provider configs for config.json (first = default).
// serverPort is this server's port so core can POST telemetry back.
// ChannelConfig holds decrypted channel config for auto-start.
type ChannelConfig struct {
	Type   string
	Config map[string]string // decrypted config (e.g. {"bot_token": "...", "bot_name": "..."})
}

func providerEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (im *InstanceManager) Start(inst *Instance, providerEnv map[string]string, serverPort string, providerPool []ProviderInfo, instanceSecret string, browserConfig map[string]any, channelConfigs ...ChannelConfig) error {
	log.Printf("[SPAWN] Start called for instance=%d name=%q project=%s", inst.ID, inst.Name, inst.ProjectID)
	im.mu.Lock()
	defer im.mu.Unlock()

	if ri, running := im.processes[inst.ID]; running && ri.cmd.ProcessState == nil {
		log.Printf("[SPAWN] instance=%d already running pid=%d port=%d", inst.ID, ri.cmd.Process.Pid, ri.port)
		return fmt.Errorf("instance %d already running", inst.ID)
	}

	port := im.allocPort()
	dir := im.instanceDir(inst.ID)

	// Get server binary path for MCP gateway
	serverBin, _ := os.Executable()

	// Build config.json — restore saved config from DB, then ensure directive/mode/gateway are current
	mode := inst.Mode
	if mode == "" {
		mode = "autonomous"
	}

	gateway := map[string]any{
		"name":        "apteva-server",
		"command":     serverBin,
		"args":        []string{"--mcp-gateway", fmt.Sprintf("--user-id=%d", inst.UserID)},
		"main_access": true,
		// no_spawn blocks sub-threads from attaching this MCP via
		// spawn(mcp="apteva-server"). Management capabilities (creating
		// instances, MCP servers, …) stay on main only.
		"no_spawn": true,
	}

	// Disk config.json is the single source of truth.
	// Core owns it — threads, directives, MCP connections are all here.
	// Server only injects gateway + channels MCP entries (URLs change on each start).
	config := map[string]any{}
	if diskConfig, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(diskConfig, &config)
	}

	// Set directive/mode from disk. Fall back to DB only for brand new instances (no config.json yet).
	if _, hasDirective := config["directive"]; !hasDirective || config["directive"] == "" {
		config["directive"] = inst.Directive
	}
	if _, hasMode := config["mode"]; !hasMode || config["mode"] == "" {
		config["mode"] = mode
	}

	// Read default_provider from instance config
	defaultProvider := ""
	if instCfg, ok := config["_instance_config"].(string); ok {
		var ic map[string]any
		json.Unmarshal([]byte(instCfg), &ic)
		defaultProvider, _ = ic["default_provider"].(string)
	}
	// Also check from the raw instance config field
	if defaultProvider == "" {
		var ic map[string]any
		json.Unmarshal([]byte(inst.Config), &ic)
		if ic != nil {
			defaultProvider, _ = ic["default_provider"].(string)
		}
	}

	// Inject providers array into config (core reads "providers" field)
	if len(providerPool) > 0 {
		var provArray []map[string]any
		for i, pi := range providerPool {
			if pi.Type == "" {
				continue
			}
			isDefault := false
			if defaultProvider != "" {
				isDefault = pi.Type == defaultProvider
			} else {
				isDefault = i == 0 // fallback: first = default
			}
			entry := map[string]any{
				"name": pi.Type,
				"models": map[string]string{
					"large":  pi.ModelLarge,
					"medium": pi.ModelMedium,
					"small":  pi.ModelSmall,
				},
				"default": isDefault,
			}
			if len(pi.BuiltinTools) > 0 {
				entry["builtin_tools"] = pi.BuiltinTools
			}
			provArray = append(provArray, entry)
		}
		if len(provArray) > 0 {
			config["providers"] = provArray
			delete(config, "provider") // remove legacy single-provider field
		}
	}

	// Inject browser/computer config if provider exists
	if browserConfig != nil {
		config["computer"] = browserConfig
	}

	// Create channels infrastructure for this instance
	ic := &InstanceChannels{registry: NewChannelRegistry()}
	ic.cli = NewCLIBridge()
	ic.registry.Register(ic.cli)

	// Let the Apteva Apps framework register its per-instance
	// channels (chat, future helpdesk, …) before the channels MCP
	// boots — the MCP's tool schema is fixed at serve() time but the
	// registry is read per tool call, so ordering here is safety +
	// consistency, not correctness.
	if im.PostChannelsInit != nil {
		im.PostChannelsInit(inst, ic)
	}

	// Start channels MCP server
	channelsMCP, err := newChannelMCPServer(ic.registry)
	if err == nil {
		channelsMCP.ic = ic
	}
	if err != nil {
		return fmt.Errorf("failed to start channels MCP: %w", err)
	}
	ic.mcp = channelsMCP
	go channelsMCP.serve()

	// Wait for channels MCP to be ready before starting core
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", channelsMCP.port), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	channelsEntry := map[string]any{
		"name":        "channels",
		"url":         channelsMCP.url(),
		"transport":   "http",
		"main_access": true,
		// Outbound user-facing chat bridge — main only. A worker that
		// could attach channels would be able to reply to end users
		// directly, which we don't want.
		"no_spawn": true,
	}

	// Read the opt-out flags for the auto-injected system MCPs. These
	// live in the instance's DB record (inst.Config JSON blob) rather
	// than disk config.json — core owns the disk config and drops
	// unknown fields on save, so any server-only state needs to live
	// elsewhere. Default true (= inject) so existing instances keep
	// current behaviour.
	includeGateway := true
	includeChannels := true
	{
		var instCfg map[string]any
		if inst.Config != "" {
			json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if v, ok := instCfg["include_apteva_server"].(bool); ok {
			includeGateway = v
		}
		if v, ok := instCfg["include_channels"].(bool); ok {
			includeChannels = v
		}
	}

	// Merge apteva-server gateway and channels into existing MCP servers.
	// Preserve all other MCP servers (schedule, social, helpdesk, etc.) that were
	// added at runtime or manually. Only replace gateway + channels entries.
	var userServers []any
	if existing, ok := config["mcp_servers"].([]any); ok {
		for _, s := range existing {
			if sm, ok := s.(map[string]any); ok {
				name, _ := sm["name"].(string)
				if name == "apteva-server" || name == "channels" || name == "apteva-channels" {
					continue // will be re-added with fresh URLs (if enabled)
				}
				userServers = append(userServers, sm)
			}
		}
	}
	var systemEntries []any
	if includeGateway {
		systemEntries = append(systemEntries, gateway)
	}
	if includeChannels {
		systemEntries = append(systemEntries, channelsEntry)
	}
	config["mcp_servers"] = append(systemEntries, userServers...)

	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(dir, "config.json"), configData, 0644)

	// Generate a unique API key for this core instance
	coreAPIKey := "core_" + generateToken(16)

	cmd := exec.Command(im.coreCmd, "--headless")
	cmd.Dir = dir
	env := append(os.Environ(),
		"API_PORT="+itoa64(int64(port)),
		"NO_TUI=1",
		"NO_CONSOLE=1", // server has its own ConsoleLogger
		"SERVER_URL=http://127.0.0.1:"+serverPort,
		"TELEMETRY_URL=http://127.0.0.1:"+serverPort+"/api/telemetry",
		"TELEMETRY_LIVE_URL=http://127.0.0.1:"+serverPort+"/api/telemetry/live",
		"INSTANCE_ID="+itoa64(inst.ID),
		"PROJECT_ID="+inst.ProjectID,
		"APTEVA_API_KEY="+coreAPIKey,
		"INSTANCE_SECRET="+instanceSecret,
	)
	for k, v := range providerEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Printf("[SPAWN] exec %s --headless dir=%s port=%d providerEnvKeys=%v", im.coreCmd, dir, port, providerEnvKeys(providerEnv))
	if err := cmd.Start(); err != nil {
		log.Printf("[SPAWN] exec failed: %v", err)
		return fmt.Errorf("failed to start core: %w", err)
	}
	log.Printf("[SPAWN] core started instance=%d pid=%d port=%d", inst.ID, cmd.Process.Pid, port)

	// Background health check — dial the port every 100ms for 5s so we can
	// see in logs exactly when (or if) core becomes reachable.
	go func(id int64, pid, p int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				log.Printf("[SPAWN] core instance=%d pid=%d port=%d is LISTENING", id, pid, p)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		log.Printf("[SPAWN] core instance=%d pid=%d port=%d FAILED to listen within 5s (last check: connection refused)", id, pid, p)
	}(inst.ID, cmd.Process.Pid, port)

	im.processes[inst.ID] = &runningInstance{cmd: cmd, port: port, coreAPIKey: coreAPIKey, channels: ic}
	inst.Port = port
	inst.Pid = cmd.Process.Pid
	inst.Status = "running"

	// Auto-start persisted channels (e.g. telegram)
	for _, cc := range channelConfigs {
		if cc.Type == "telegram" && cc.Config["bot_token"] != "" {
			corePort := port
			ck := coreAPIKey
			sendEvent := func(text, threadID string) {
				body, _ := json.Marshal(map[string]any{"message": text, "thread_id": threadID})
				req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", corePort), strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				if ck != "" {
					req.Header.Set("Authorization", "Bearer "+ck)
				}
				http.DefaultClient.Do(req)
			}
			gw := NewTelegramGateway(cc.Config["bot_token"], ic.registry, sendEvent)
			if botName, err := gw.Start(); err == nil {
				ic.telegram = gw
				ic.registry.AddFactory(gw.ChannelFactory())
				log.Printf("[CHANNELS] auto-started telegram @%s for instance %d", botName, inst.ID)
			}
		}
	}

	// Wait for process exit in background — clean up channels on exit
	instID := inst.ID
	spawnedPid := cmd.Process.Pid
	spawnedPort := port
	startedAt := time.Now()
	go func() {
		waitErr := cmd.Wait()
		lived := time.Since(startedAt)
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		log.Printf("[SPAWN] core EXITED instance=%d pid=%d port=%d exitCode=%d lived=%s waitErr=%v",
			instID, spawnedPid, spawnedPort, exitCode, lived, waitErr)
		im.mu.Lock()
		ri := im.processes[instID]
		if ri != nil && ri.channels != nil {
			ri.channels.Stop()
		}
		delete(im.processes, instID)
		im.mu.Unlock()
		log.Printf("[SPAWN] cleaned up process map for instance=%d", instID)
	}()

	return nil
}

// Stop kills a running core process and cleans up channels. Sends
// SIGTERM first and waits up to 2s for core to release its computer
// session (local Chrome / Browserbase) before escalating to SIGKILL.
// Without the grace window, Chrome is orphaned to PID 1 and keeps
// running after every instance stop.
func (im *InstanceManager) Stop(instanceID int64) {
	im.mu.Lock()
	ri, ok := im.processes[instanceID]
	if ok {
		delete(im.processes, instanceID)
	}
	im.mu.Unlock()
	if !ok {
		return
	}
	if ri.channels != nil {
		ri.channels.Stop()
	}
	if ri.cmd.Process == nil {
		return
	}
	// Phase 1: polite SIGTERM.
	_ = ri.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { ri.cmd.Wait(); close(done) }()
	select {
	case <-done:
		// clean exit
	case <-time.After(2 * time.Second):
		// Phase 2: escalate. Chrome may be stuck on a navigation or
		// an agent may be ignoring SIGTERM — don't wait forever.
		ri.cmd.Process.Kill()
		<-done
	}
}

// GetChannels returns the InstanceChannels for a running instance, or nil.
func (im *InstanceManager) GetChannels(instanceID int64) *InstanceChannels {
	im.mu.RLock()
	defer im.mu.RUnlock()
	if ri, ok := im.processes[instanceID]; ok {
		return ri.channels
	}
	return nil
}

// StartTelegram starts the Telegram gateway for an instance.
func (im *InstanceManager) StartTelegram(instanceID int64, token string) (string, error) {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	if !ok || ri.channels == nil {
		return "", fmt.Errorf("instance not running")
	}
	if ri.channels.telegram != nil {
		ri.channels.telegram.Stop()
	}
	// sendEvent function — POST to core's /event endpoint
	corePort := ri.port
	coreKey := ri.coreAPIKey
	sendEvent := func(text, threadID string) {
		body, _ := json.Marshal(map[string]any{"message": text, "thread_id": threadID})
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", corePort), strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if coreKey != "" {
			req.Header.Set("Authorization", "Bearer "+coreKey)
		}
		http.DefaultClient.Do(req)
	}
	gw := NewTelegramGateway(token, ri.channels.registry, sendEvent)
	botName, err := gw.Start()
	if err != nil {
		return "", err
	}
	ri.channels.telegram = gw
	ri.channels.registry.AddFactory(gw.ChannelFactory())
	return botName, nil
}

// browserDefaultsFor returns the recommended (width, height) for a given LLM
// provider. Anthropic uses 1024×768 because its computer-use tool was trained
// on that exact resolution and the docs recommend keeping screenshots at it.
// Everything else uses 2000×1000 — a 2:1 widescreen that gives non-native
// vision models more horizontal context per screenshot. Pure helper so both
// the spawn path (getBrowserConfig) and the hot-attach path can share it.
func browserDefaultsFor(providerName string) (int, int) {
	if providerName == "anthropic" {
		return 1024, 768
	}
	// 1600×800 — exact 2:1 at a common laptop width. Wide enough for
	// desktop layouts without horizontal scroll, but small enough to
	// keep screenshot token counts modest. Sweet spot for non-native
	// vision models (Kimi, Gemini) where every pixel costs tokens.
	return 1600, 800
}

// defaultProviderForInstance pulls the instance's preferred LLM provider
// name out of inst.Config. Used to pick provider-aware defaults (e.g.
// browser viewport size). Returns empty string when nothing is set,
// which downstream callers treat as "non-Anthropic".
func defaultProviderForInstance(inst *Instance) string {
	if inst == nil || inst.Config == "" {
		return ""
	}
	var ic map[string]any
	if err := json.Unmarshal([]byte(inst.Config), &ic); err != nil || ic == nil {
		return ""
	}
	name, _ := ic["default_provider"].(string)
	return name
}

// getBrowserConfig returns the browser/computer config from providers if one exists.
// Supports "browser" (local Chrome or existing CDP), "browserbase" (cloud), and "steel" (cloud) provider types.
// providerName picks the default viewport when WIDTH/HEIGHT aren't set on the
// provider record — pass the name of the LLM that will run inside the
// instance ("anthropic", "fireworks", "google", …). Empty string falls back
// to the non-Anthropic widescreen default.
func (s *Server) getBrowserConfig(userID int64, providerName string, projectID ...string) map[string]any {
	providers, err := s.store.ListProviders(userID, projectID...)
	if err != nil {
		return nil
	}
	for _, p := range providers {
		if p.Type != "browserbase" && p.Type != "browser" && p.Type != "steel" && p.Type != "browser-engine" {
			continue
		}
		_, encData, err := s.store.GetProvider(userID, p.ID)
		if err != nil {
			continue
		}
		plaintext, err := Decrypt(s.secret, encData)
		if err != nil {
			continue
		}
		var data map[string]string
		json.Unmarshal([]byte(plaintext), &data)
		if data == nil {
			continue
		}

		// Parse optional resolution from provider data. Default depends on
		// the LLM that will run in the instance: 1024×768 for Anthropic
		// (matches Claude's native computer-use training), 2000×1000 for
		// everything else (2:1 widescreen, better with non-native vision
		// models like Kimi/Gemini). Override per-provider via WIDTH/HEIGHT.
		width, height := browserDefaultsFor(providerName)
		if w := data["WIDTH"]; w != "" {
			fmt.Sscanf(w, "%d", &width)
		}
		if h := data["HEIGHT"]; h != "" {
			fmt.Sscanf(h, "%d", &height)
		}

		if p.Type == "browser" {
			// Local browser or existing CDP endpoint
			cfg := map[string]any{
				"type":   "local",
				"width":  width,
				"height": height,
			}
			if cdpURL := data["CDP_URL"]; cdpURL != "" {
				cfg["type"] = "service"
				cfg["url"] = cdpURL
			}
			// Optional residential / corporate proxy for the local
			// backend. When set, the agent's
			// browser_session(open, proxy=true) routes that session
			// through this URL (Chrome relaunches with --proxy-server
			// applied; auth is honored via CDP). Per-provider value
			// wins; APTEVA_LOCAL_PROXY_URL is a server-wide fallback
			// for ops who'd rather wire it once at the deployment
			// layer instead of in every dashboard provider record.
			proxyURL := data["LOCAL_PROXY_URL"]
			if proxyURL == "" {
				proxyURL = os.Getenv("APTEVA_LOCAL_PROXY_URL")
			}
			if proxyURL != "" {
				cfg["proxy_url"] = proxyURL
			}
			return cfg
		}

		if p.Type == "browser-engine" {
			apiKey := data["BROWSER_API_KEY"]
			if apiKey == "" {
				continue
			}
			cfg := map[string]any{
				"type":    "browser-engine",
				"api_key": apiKey,
				"width":   width,
				"height":  height,
			}
			// Extended Browser Engine options — all optional. Each
			// maps to a POST /sessions field on the hosted API.
			if v := data["BROWSER_API_URL"]; v != "" {
				cfg["url"] = v
			}
			if v := data["BROWSER_INITIAL_URL"]; v != "" {
				cfg["initial_url"] = v
			}
			if v := data["BROWSER_USER_AGENT"]; v != "" {
				cfg["user_agent"] = v
			}
			if v := data["BROWSER_PROXY_ENABLED"]; v == "1" || v == "true" {
				cfg["proxy_enabled"] = true
			}
			if v := data["BROWSER_PROXY_COUNTRY"]; v != "" {
				cfg["proxy_country"] = v
			}
			if v := data["BROWSER_TIMEOUT"]; v != "" {
				var t int
				fmt.Sscanf(v, "%d", &t)
				if t > 0 {
					cfg["timeout"] = t
				}
			}
			if v := data["BROWSER_PROJECT_ID"]; v != "" {
				var id int
				fmt.Sscanf(v, "%d", &id)
				if id > 0 {
					cfg["browser_project_id"] = id
				}
			}
			return cfg
		}

		if p.Type == "steel" {
			apiKey := data["STEEL_API_KEY"]
			if apiKey == "" {
				continue
			}
			cfg := map[string]any{
				"type":    "steel",
				"api_key": apiKey,
				"width":   width,
				"height":  height,
			}
			// Extended Steel options — all optional. Each maps to a
			// POST /v1/sessions field. Stored as plain strings; parsed
			// and forwarded only when set.
			if v := data["STEEL_REGION"]; v != "" {
				cfg["region"] = v
			}
			if v := data["STEEL_USER_AGENT"]; v != "" {
				cfg["user_agent"] = v
			}
			if v := data["STEEL_PROXY_URL"]; v != "" {
				cfg["proxy_url"] = v
			}
			if v := data["STEEL_USE_PROXY"]; v == "1" || v == "true" {
				cfg["use_proxy"] = true
			}
			if v := data["STEEL_BLOCK_ADS"]; v == "1" || v == "true" {
				cfg["block_ads"] = true
			}
			if v := data["STEEL_SOLVE_CAPTCHA"]; v == "1" || v == "true" {
				cfg["solve_captcha"] = true
			}
			if v := data["STEEL_TIMEOUT"]; v != "" {
				var t int
				fmt.Sscanf(v, "%d", &t)
				if t > 0 {
					cfg["timeout"] = t
				}
			}
			return cfg
		}

		// Browserbase
		apiKey := data["BROWSERBASE_API_KEY"]
		projectID := data["BROWSERBASE_PROJECT_ID"]
		if apiKey == "" {
			continue
		}
		cfg := map[string]any{
			"type":       "browserbase",
			"api_key":    apiKey,
			"project_id": projectID,
			"width":      width,
			"height":     height,
		}
		// Extended Browserbase options — all optional. Each maps to a
		// POST /v1/sessions field. Stored on the provider record as
		// plain strings; parsed and forwarded only when set.
		if v := data["BROWSERBASE_REGION"]; v != "" {
			cfg["region"] = v
		}
		if v := data["BROWSERBASE_EXTENSION_ID"]; v != "" {
			cfg["extension_id"] = v
		}
		if v := data["BROWSERBASE_KEEP_ALIVE"]; v == "1" || v == "true" {
			cfg["keep_alive"] = true
		}
		if v := data["BROWSERBASE_SOLVE_CAPTCHAS"]; v == "1" || v == "true" {
			cfg["solve_captchas"] = true
		}
		if v := data["BROWSERBASE_PROXIES"]; v == "1" || v == "true" {
			cfg["proxies"] = true
		}
		if v := data["BROWSERBASE_TIMEOUT"]; v != "" {
			var t int
			fmt.Sscanf(v, "%d", &t)
			if t > 0 {
				cfg["timeout"] = t
			}
		}
		return cfg
	}
	return nil
}

// loadChannelConfigs fetches persisted channel configs for auto-start.
func (s *Server) loadChannelConfigs(instanceID int64) []ChannelConfig {
	records, err := s.store.ListChannels(instanceID)
	if err != nil || len(records) == 0 {
		return nil
	}
	var configs []ChannelConfig
	for _, r := range records {
		enc, err := s.store.GetChannelConfig(r.ID)
		if err != nil || enc == "" {
			continue
		}
		plain, err := Decrypt(s.secret, enc)
		if err != nil {
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg != nil {
			configs = append(configs, ChannelConfig{Type: r.Type, Config: cfg})
		}
	}
	return configs
}

// StopAll gracefully terminates every tracked child process. Called
// from the SIGTERM/SIGINT signal handler in main so apteva-server's
// children don't orphan when we're asked to shut down.
//
// Two-phase shutdown:
//   1. SIGTERM every child, wait up to `graceful` for clean exits so
//      cores can flush session state to disk and tell their own MCP
//      children to pack up.
//   2. Anything still alive after the deadline gets SIGKILL.
//
// Cross-platform caveat: on Windows os.Process.Signal only accepts
// os.Kill, so SIGTERM silently maps to Kill there — graceful phase
// collapses to hard kill. Unix gets the full two-phase behaviour.
func (im *InstanceManager) StopAll(graceful time.Duration) {
	im.mu.Lock()
	procs := make([]*runningInstance, 0, len(im.processes))
	for _, ri := range im.processes {
		if ri != nil && ri.cmd != nil && ri.cmd.Process != nil {
			procs = append(procs, ri)
		}
	}
	im.mu.Unlock()

	if len(procs) == 0 {
		return
	}
	log.Printf("[SHUTDOWN] stopping %d tracked core process(es) — graceful %s", len(procs), graceful)

	// Phase 1: polite SIGTERM.
	for _, ri := range procs {
		ri.cmd.Process.Signal(syscall.SIGTERM)
	}

	// Phase 2: wait per-process for clean exit, then SIGKILL the
	// holdouts once the global deadline fires. Each Wait runs in its
	// own goroutine so slow-draining cores don't serialise the loop.
	deadline := time.After(graceful)
	type waitResult struct {
		pid  int
		name string
		err  error
	}
	results := make(chan waitResult, len(procs))
	for _, ri := range procs {
		go func(r *runningInstance) {
			err := r.cmd.Wait()
			results <- waitResult{pid: r.cmd.Process.Pid, err: err}
		}(ri)
	}

	remaining := len(procs)
	for remaining > 0 {
		select {
		case res := <-results:
			log.Printf("[SHUTDOWN] core pid=%d exited: %v", res.pid, res.err)
			remaining--
		case <-deadline:
			log.Printf("[SHUTDOWN] graceful deadline hit, SIGKILLing %d holdout core(s)", remaining)
			for _, ri := range procs {
				if ri.cmd.ProcessState == nil {
					ri.cmd.Process.Kill()
				}
			}
			// Drain the remaining Wait results so goroutines exit.
			for remaining > 0 {
				<-results
				remaining--
			}
		}
	}
	log.Printf("[SHUTDOWN] all children stopped")
}

// IsRunning checks if an instance process is alive.
func (im *InstanceManager) IsRunning(instanceID int64) bool {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	return ok && ri.cmd.ProcessState == nil
}

// GetCoreAPIKey returns the API key for a running instance.
func (im *InstanceManager) GetCoreAPIKey(instanceID int64) string {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	if ok && ri.cmd.ProcessState == nil {
		return ri.coreAPIKey
	}
	return ""
}

// GetPort returns the port for a running instance, or 0 if not running.
func (im *InstanceManager) GetPort(instanceID int64) int {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	if ok && ri.cmd.ProcessState == nil {
		return ri.port
	}
	return 0
}

// --- HTTP Handlers ---

// POST /instances
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	var body struct {
		Name       string `json:"name"`
		Directive  string `json:"directive"`
		Mode       string `json:"mode"`   // "autonomous" | "cautious" | "learn"
		Config     string `json:"config"` // optional JSON blob for MCP servers etc
		ProjectID  string `json:"project_id"`
		Start      *bool  `json:"start,omitempty"` // default true; set false to create without starting
		// Auto-injected system MCPs. Both default to true so existing
		// callers keep the current behaviour (agent gets the apteva
		// gateway + channels out of the box). Set to false to create a
		// lean instance that only sees whatever MCPs are in `config`.
		// Useful for sandbox / test agents or when you want to swap in
		// a custom gateway.
		IncludeAptevaServer *bool `json:"include_apteva_server,omitempty"`
		IncludeChannels     *bool `json:"include_channels,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if body.Directive == "" {
		body.Directive = "Idle. Waiting for configuration via directive."
	}
	// Core supports autonomous, cautious, and learn. Anything else
	// (including the legacy "supervised" string that never existed on
	// the core side) falls back to autonomous.
	switch body.Mode {
	case "autonomous", "cautious", "learn":
		// keep
	default:
		body.Mode = "autonomous"
	}
	if body.Config == "" {
		body.Config = "{}"
	}

	inst, err := s.store.CreateInstance(userID, body.Name, body.Directive, body.Mode, body.Config, body.ProjectID)
	if err != nil {
		http.Error(w, "failed to create instance", http.StatusInternalServerError)
		return
	}

	// Persist the system-MCP opt-out flags on the instance DB row so
	// Start() picks them up on first (and every subsequent) boot.
	// Start() reads these from inst.Config, not from disk config.json
	// (which core owns and rewrites on every run), so the DB row is the
	// authoritative place for server-side flags.
	includeGateway := body.IncludeAptevaServer == nil || *body.IncludeAptevaServer
	includeChannels := body.IncludeChannels == nil || *body.IncludeChannels
	{
		var instCfg map[string]any
		if inst.Config != "" {
			json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if instCfg == nil {
			instCfg = map[string]any{}
		}
		instCfg["include_apteva_server"] = includeGateway
		instCfg["include_channels"] = includeChannels
		if out, err := json.Marshal(instCfg); err == nil {
			inst.Config = string(out)
		}
	}

	// Start unless explicitly disabled
	shouldStart := body.Start == nil || *body.Start
	if shouldStart {
		providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, inst.ProjectID)
		if err != nil {
			providerEnv = map[string]string{}
		}
		pool := s.GetProviderPool(userID, inst.ProjectID)
		if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID, defaultProviderForInstance(inst), inst.ProjectID), s.loadChannelConfigs(inst.ID)...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
	}

	s.store.UpdateInstance(inst)
	writeJSON(w, inst)
}

// GET /instances
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	instances, err := s.store.ListInstances(userID, projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Update running status
	for i := range instances {
		if s.instances.IsRunning(instances[i].ID) {
			instances[i].Status = "running"
		} else {
			instances[i].Status = "stopped"
		}
	}
	if instances == nil {
		instances = []Instance{}
	}
	writeJSON(w, instances)
}

// GET/DELETE /instances/:id
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	idStr := strings.TrimPrefix(r.URL.Path, "/instances/")
	// Strip any sub-path (for proxy routes)
	if idx := strings.Index(idStr, "/"); idx >= 0 {
		idStr = idStr[:idx]
	}
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if s.instances.IsRunning(inst.ID) {
			inst.Status = "running"
		} else {
			inst.Status = "stopped"
		}
		writeJSON(w, inst)

	case http.MethodPut, http.MethodPatch:
		// Rename / metadata edit. The only mutable field for now is name —
		// directive/mode/config go through /instances/:id/config which also
		// forwards to the running core. Keep this endpoint narrow on
		// purpose so renaming a running instance never has to touch the
		// core process.
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if len(name) > 100 {
			http.Error(w, "name too long (max 100)", http.StatusBadRequest)
			return
		}
		inst.Name = name
		if err := s.store.UpdateInstance(inst); err != nil {
			http.Error(w, "failed to update instance", http.StatusInternalServerError)
			return
		}
		writeJSON(w, inst)

	case http.MethodDelete:
		log.Printf("[LIFECYCLE] DELETE %s instance=%d remote=%s ua=%q referer=%q",
			r.URL.Path, inst.ID, r.RemoteAddr, r.UserAgent(), r.Referer())

		// 1. Capture the InstanceInfo snapshot BEFORE we tear anything
		// down — the apps registry needs it to clean up its per-
		// instance state (channelchat chats, future helpdesk tickets,
		// etc.) and we won't be able to rebuild it once the row is
		// gone. nil-safe: if the apps registry hasn't booted, skip.
		var detachInfo *framework.InstanceInfo
		if s.apps != nil {
			detachInfo = s.buildInstanceInfo(inst.ID)
		}

		// 2. Stop the running core process (kills child + per-instance
		// channels MCP + Slack/email/telegram listeners).
		s.instances.Stop(inst.ID)

		// 3. Notify apps so each one drops its instance-scoped rows
		// (channelchat: chats + messages). Done AFTER Stop so the apps
		// don't race with a still-running core writing more data into
		// the tables they're about to drop.
		if s.apps != nil && detachInfo != nil {
			s.apps.NotifyInstanceDetach(*detachInfo)
		}

		// 4. Cascade-delete server DB rows
		// (instances + telemetry + channels + subscriptions +
		// app_instance_bindings).
		if err := s.store.DeleteInstance(userID, instanceID); err != nil {
			log.Printf("[LIFECYCLE] DB cascade delete failed instance=%d err=%v", inst.ID, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}

		// 5. Remove on-disk state: config.json, apteva-core.log,
		// history/*.jsonl, workspace/. Done last so the DB row is
		// already gone — if RemoveAll fails we have an orphan dir
		// but the user-visible state matches "deleted". Logged so
		// operators can scrub manually.
		dir := s.instances.instanceDir(inst.ID)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("[LIFECYCLE] dir cleanup failed instance=%d dir=%s err=%v", inst.ID, dir, err)
		}

		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}

// POST /instances/:id/stop
func (s *Server) handleStopInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/stop")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	// Disk config.json is the source of truth — no need to save to DB.
	// Core already writes threads/MCP/directive to disk at runtime.
	s.instances.Stop(inst.ID)
	inst.Status = "stopped"
	inst.Pid = 0
	inst.Port = 0
	s.store.UpdateInstance(inst)
	writeJSON(w, inst)
}

// POST /instances/:id/start
// ResumeRunningInstances is the boot-time recovery path. When the server
// starts, every instance in the DB marked `status='running'` is assumed to
// have been left in that state by a previous server process — its core
// subprocess died when the server died (child of the same process group).
//
// We walk every such row and re-Start each one: spawn a fresh channels MCP
// subprocess, spawn a fresh core that connects to it, write a new
// instance_X/config.json with the live URLs, and populate the in-memory
// process map. Without this, a `go build && restart` silently orphans
// every running core — the DB still says "running" but nothing responds,
// and you only notice when you try to chat.
//
// Any instance that fails to resume (missing provider credentials, port
// already taken, bad config, etc.) is flipped to `stopped` in the DB so
// the dashboard's Start button can try again cleanly.
func (s *Server) ResumeRunningInstances() {
	rows, err := s.store.ListInstancesByStatus("running")
	if err != nil {
		log.Printf("[RESUME] list running instances: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("[RESUME] found %d instance(s) marked running in DB — re-spawning cores", len(rows))

	for i := range rows {
		inst := &rows[i]
		providerEnv, err := s.store.GetAllProviderEnvVars(inst.UserID, s.secret, inst.ProjectID)
		if err != nil {
			providerEnv = map[string]string{}
		}
		pool := s.GetProviderPool(inst.UserID, inst.ProjectID)

		if err := s.instances.Start(
			inst,
			providerEnv,
			s.port,
			pool,
			s.instanceSecret,
			s.getBrowserConfig(inst.UserID, defaultProviderForInstance(inst), inst.ProjectID),
			s.loadChannelConfigs(inst.ID)...,
		); err != nil {
			log.Printf("[RESUME] instance %d (%s): start failed: %v — marking stopped", inst.ID, inst.Name, err)
			inst.Status = "stopped"
			s.store.UpdateInstance(inst)
			continue
		}

		// Start() mutates inst.Port + Pid + Status to the new values;
		// persist them so the UI reflects the fresh process state.
		s.store.UpdateInstance(inst)
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
		log.Printf("[RESUME] instance %d (%s): resumed on port %d pid %d", inst.ID, inst.Name, inst.Port, inst.Pid)
	}
}

func (s *Server) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/start")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	if s.instances.IsRunning(inst.ID) {
		http.Error(w, "instance already running", http.StatusConflict)
		return
	}

	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, inst.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID, inst.ProjectID)

	if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID, defaultProviderForInstance(inst), inst.ProjectID), s.loadChannelConfigs(inst.ID)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.restoreSlackForInstance(inst)
	s.restoreEmailForInstance(inst)

	s.store.UpdateInstance(inst)
	writeJSON(w, inst)
}

// POST /instances/:id/restart
func (s *Server) handleRestartInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/restart")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	// Save config before stopping
	// Stop — disk config.json is the source of truth, no DB save needed.
	s.instances.Stop(inst.ID)

	// Start
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, inst.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID, inst.ProjectID)

	if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID, defaultProviderForInstance(inst), inst.ProjectID), s.loadChannelConfigs(inst.ID)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.restoreSlackForInstance(inst)
	s.restoreEmailForInstance(inst)

	s.store.UpdateInstance(inst)
	writeJSON(w, map[string]string{"status": "restarted"})
}

// /instances/:id/config — GET proxies to core, PUT updates DB + proxies full body to core
func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	// Extract instance ID from /instances/:id/config
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/config")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	port := s.instances.GetPort(inst.ID)

	// GET — proxy directly to core (with boot-wait retry)
	if r.Method == http.MethodGet {
		if port == 0 {
			// Instance stopped — serve saved config from disk, same as handleProxy.
			s.serveStoppedInstanceData(w, inst, "/config")
			return
		}
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		coreKey := s.instances.GetCoreAPIKey(inst.ID)
		resp, err := s.coreDoWithBootWait(inst.ID, "GET", targetURL, nil, coreKey)
		if err != nil {
			log.Printf("[PROXY] core unreachable instance=%d path=/config: %v", inst.ID, err)
			http.Error(w, fmt.Sprintf("core unreachable: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	if r.Method != http.MethodPut {
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
		return
	}

	// PUT — read body, update DB fields, then proxy FULL body to core
	bodyBytes, _ := io.ReadAll(r.Body)

	var body struct {
		Directive string         `json:"directive"`
		Mode      string         `json:"mode"`
		Config    string         `json:"config"`
		Providers []map[string]any `json:"providers"`
	}
	json.Unmarshal(bodyBytes, &body)

	// Enrich computer config with credentials from the saved provider so the
	// dashboard never has to handle them client-side. The dashboard sends a
	// thin payload like {computer: {type: "browserbase"}} or
	// {computer: {type: "service"}}; we look up the matching browser provider
	// and inject api_key/project_id (browserbase) or url (service) before
	// forwarding to the core. {computer: {type: ""}} (off) is forwarded
	// as-is. {computer: {type: "local"}} also forwards untouched —
	// chromedp doesn't need credentials.
	var rawBody map[string]any
	if err := json.Unmarshal(bodyBytes, &rawBody); err == nil && rawBody != nil {
		if compRaw, ok := rawBody["computer"].(map[string]any); ok {
			compType, _ := compRaw["type"].(string)
			needsEnrich := (compType == "browserbase" || compType == "steel" || compType == "browser-engine" || compType == "service") &&
				compRaw["api_key"] == nil && compRaw["url"] == nil
			if needsEnrich {
				if browserCfg := s.getBrowserConfig(userID, defaultProviderForInstance(inst), inst.ProjectID); browserCfg != nil {
					// getBrowserConfig returns a fully-populated map. Merge
					// it into the request, but let the user's explicit type
					// win (so "service" overrides a saved "browserbase",
					// for instance).
					for k, v := range browserCfg {
						if _, set := compRaw[k]; !set {
							compRaw[k] = v
						}
					}
					// Force the user's requested type back in case
					// browserCfg overwrote it via the merge.
					compRaw["type"] = compType
					rawBody["computer"] = compRaw
					if newBytes, err := json.Marshal(rawBody); err == nil {
						bodyBytes = newBytes
					}
				} else {
					http.Error(w, fmt.Sprintf("no %s provider configured for this project", compType), http.StatusBadRequest)
					return
				}
			}
		}
	}

	if body.Directive != "" {
		inst.Directive = body.Directive
	}
	if body.Mode == "autonomous" || body.Mode == "cautious" || body.Mode == "learn" {
		inst.Mode = body.Mode
	}
	if body.Config != "" {
		inst.Config = body.Config
	}
	// Save default provider to instance config
	if len(body.Providers) > 0 {
		for _, p := range body.Providers {
			if def, _ := p["default"].(bool); def {
				name, _ := p["name"].(string)
				if name != "" {
					var cfg map[string]any
					json.Unmarshal([]byte(inst.Config), &cfg)
					if cfg == nil {
						cfg = map[string]any{}
					}
					cfg["default_provider"] = name
					cfgBytes, _ := json.Marshal(cfg)
					inst.Config = string(cfgBytes)
				}
				break
			}
		}
	}
	// System-MCP opt-out persistence: when the client sends an
	// mcp_servers list, detect whether the system entries the server
	// auto-injects at startup (apteva-server, channels / apteva-channels)
	// are present. Absent means "user detached" — we remember that in
	// the instance DB record so the next start respects the choice.
	// Present means "user wants it", which clears any stale opt-out.
	if mcpList, ok := rawBody["mcp_servers"].([]any); ok {
		var instCfg map[string]any
		if inst.Config != "" {
			json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if instCfg == nil {
			instCfg = map[string]any{}
		}
		hasGateway := false
		hasChannels := false
		for _, s := range mcpList {
			if sm, ok := s.(map[string]any); ok {
				n, _ := sm["name"].(string)
				if n == "apteva-server" {
					hasGateway = true
				}
				if n == "channels" || n == "apteva-channels" {
					hasChannels = true
				}
			}
		}
		instCfg["include_apteva_server"] = hasGateway
		instCfg["include_channels"] = hasChannels
		if out, err := json.Marshal(instCfg); err == nil {
			inst.Config = string(out)
		}
	}

	s.store.UpdateInstance(inst)

	// Forward the FULL body to core (includes mcp_servers, computer, etc.)
	if port > 0 {
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		coreKey := s.instances.GetCoreAPIKey(inst.ID)
		resp, err := s.coreDoWithBootWait(inst.ID, "PUT", targetURL, bodyBytes, coreKey, http.Header{"Content-Type": []string{"application/json"}})
		if err != nil {
			log.Printf("[CONFIG] PUT forward to core failed instance=%d: %v", inst.ID, err)
			http.Error(w, fmt.Sprintf("core unreachable: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	// Stopped: persist to config.json on disk so the next core boot
	// picks up the edit. Fields the client sent are overlaid on the
	// existing file; unset fields are preserved. Supported keys match
	// core.Config (directive, mode, mcp_servers, computer, providers,
	// threads, unconscious) and the `reset` sub-object.
	err = s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
		if body.Directive != "" {
			cfg["directive"] = body.Directive
		}
		if body.Mode == "autonomous" || body.Mode == "cautious" || body.Mode == "learn" {
			cfg["mode"] = body.Mode
		}
		// rawBody was decoded above for computer enrichment; re-use it
		// for the surface-level fields the client may set. If a key is
		// absent in the request we keep whatever disk already held.
		for _, k := range []string{"mcp_servers", "computer", "providers", "threads", "unconscious"} {
			if v, ok := rawBody[k]; ok {
				cfg[k] = v
			}
		}
		// Honour the reset envelope on a stopped instance — only
		// threads can realistically be reset without a running core;
		// history lives in session.jsonl which we leave alone.
		if reset, ok := rawBody["reset"].(map[string]any); ok {
			if t, _ := reset["threads"].(bool); t {
				delete(cfg, "threads")
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[CONFIG] PUT stopped-write failed instance=%d: %v", inst.ID, err)
		http.Error(w, fmt.Sprintf("persist config: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("[CONFIG] PUT stopped instance=%d — persisted to config.json (applies on next start)", inst.ID)
	writeJSON(w, inst)
}

// Proxy handler: forwards to core instance's API
// errInstanceNotRunning signals that the proxy retry loop observed the core
// process disappearing from the instance manager — i.e. the reaper saw core
// exit and removed its entry. Callers translate this to 503.
var errInstanceNotRunning = fmt.Errorf("instance not running")

// coreDoWithBootWait POSTs/GETs to a core URL, retrying for up to 3 seconds
// while the connection is refused. Core takes ~1s to bind its HTTP port after
// exec, so fresh requests that race with the boot window briefly block here
// instead of bubbling up as 502s. The cmd.Wait() reaper is still the single
// source of truth for "core dead": if the entry disappears from the process
// map mid-retry, we bail with errInstanceNotRunning.
//
// headers is optional; when non-nil it's cloned onto every retry so the
// original request's headers (content-type, tracing, etc.) survive.
func (s *Server) coreDoWithBootWait(instanceID int64, method, targetURL string, bodyBytes []byte, coreKey string, headers ...http.Header) (*http.Response, error) {
	build := func() (*http.Request, error) {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequest(method, targetURL, body)
		if err != nil {
			return nil, err
		}
		if len(headers) > 0 && headers[0] != nil {
			req.Header = headers[0].Clone()
		}
		if coreKey != "" {
			req.Header.Set("Authorization", "Bearer "+coreKey)
		}
		return req, nil
	}

	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		return resp, nil
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.instances.GetPort(instanceID) == 0 {
			return nil, errInstanceNotRunning
		}
		time.Sleep(100 * time.Millisecond)
		req, err = build()
		if err != nil {
			return nil, err
		}
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			return resp, nil
		}
	}
	return nil, err
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	// Parse /instances/:id/<core-path>
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	instanceID, err := atoi64(parts[0])
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	port := s.instances.GetPort(inst.ID)
	corePath := "/" + parts[1]

	// Instance stopped — serve static data from saved config for read-only endpoints,
	// and honour a small set of mutations directly against config.json so the dashboard
	// can edit a stopped agent's configuration (add/remove MCPs, drop persisted threads)
	// without needing to boot the core.
	if port == 0 {
		if r.Method == http.MethodGet && (corePath == "/threads" || corePath == "/status" || corePath == "/config") {
			s.serveStoppedInstanceData(w, inst, corePath)
			return
		}
		if s.handleStoppedMutation(w, r, inst, corePath) {
			return
		}
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, corePath)

	// Read the body once so we can replay it across boot-wait retries. SSE/GET
	// paths have no body so this is cheap in practice.
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
	}

	coreKey := s.instances.GetCoreAPIKey(inst.ID)
	resp, err := s.coreDoWithBootWait(inst.ID, r.Method, targetURL, bodyBytes, coreKey, r.Header)
	if err != nil {
		if err == errInstanceNotRunning {
			http.Error(w, "instance not running", http.StatusServiceUnavailable)
			return
		}
		log.Printf("[PROXY] core unreachable instance=%d port=%d path=%s: %v", inst.ID, port, corePath, err)
		http.Error(w, fmt.Sprintf("core unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	// /events is telemetry-only now (thoughts, tool activity, status).
	// It used to double as "user is present on cli" — the dashboard
	// opened /events and the agent was expected to reply via
	// channels_respond(channel="cli"). That channel has been replaced
	// by the channel-chat app, which tracks its own presence via
	// the hub-subscriber count on /api/apps/channel-chat/stream.
	// So we NO LONGER increment CLIBridge on /events subscriptions —
	// otherwise every dashboard / TUI status reader made the agent
	// think cli was reachable and caused it to respond there by
	// default (stranding messages no one would ever see).
	flusher, canFlush := w.(http.Flusher)
	isSSE := canFlush && resp.Header.Get("Content-Type") == "text/event-stream"

	if isSSE {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				// SSE frames are `data: <json>\n`. Parse just the llm.done
				// frames and inject a server-computed cost_usd before
				// forwarding, so the dashboard's live stream shows cost
				// without another round-trip. Non-llm.done frames (and
				// anything that doesn't parse cleanly) pass through
				// verbatim.
				if bytes.HasPrefix(line, []byte("data: ")) {
					rewritten := enrichLLMDoneSSELine(line)
					w.Write(rewritten)
				} else {
					w.Write(line)
				}
				if len(bytes.TrimSpace(line)) == 0 {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}

// enrichLLMDoneSSELine rewrites a single SSE `data: {...}` line for an
// llm.done event to include a server-computed cost_usd. Used by the
// /api/instances/:id/events proxy so the dashboard's live stream sees
// enriched cost without refetching from the persisted telemetry table.
//
// Any parse failure returns the input unchanged — we never want to
// break the frame if the shape shifts. The event-level JSON shape here
// mirrors what core emits: `{ "id", "type", "thread_id", "data": {...} }`.
func enrichLLMDoneSSELine(line []byte) []byte {
	payload := bytes.TrimPrefix(line, []byte("data: "))
	payload = bytes.TrimRight(payload, "\r\n")
	if len(payload) == 0 {
		return line
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		return line
	}
	if env["type"] != "llm.done" {
		return line
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		return line
	}
	model, _ := data["model"].(string)
	if model == "" {
		return line
	}
	tokIn, _ := data["tokens_in"].(float64)
	tokCached, _ := data["tokens_cached"].(float64)
	tokOut, _ := data["tokens_out"].(float64)
	if tokIn == 0 && tokOut == 0 {
		return line
	}
	input, cached, output, ok := LookupModelPricing(model)
	if !ok {
		return line
	}
	uncached := tokIn - tokCached
	if uncached < 0 {
		uncached = 0
	}
	cost := (uncached*input + tokCached*cached + tokOut*output) / 1_000_000
	data["cost_usd"] = cost
	env["data"] = data
	out, err := json.Marshal(env)
	if err != nil {
		return line
	}
	return append(append([]byte("data: "), out...), '\n')
}

// POST /instances/:id/channels/telegram — connect telegram bot
// serveStoppedInstanceData returns static data from saved config when instance is stopped.
func (s *Server) serveStoppedInstanceData(w http.ResponseWriter, inst *Instance, path string) {
	// Load config: try disk first, fall back to DB
	var config map[string]any
	dir := s.instances.instanceDir(inst.ID)
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(data, &config)
	}
	// Disk is the single source of truth — no DB fallback
	if config == nil {
		config = map[string]any{}
	}

	switch path {
	case "/threads":
		// Convert PersistentThread format to threadJSON format
		var threads []map[string]any
		// Add main
		threads = append(threads, map[string]any{
			"id":        "main",
			"directive": inst.Directive,
			"depth":     0,
			"iteration": 0,
			"rate":      "stopped",
			"model":     "",
			"age":       "",
		})
		// Add persisted threads
		if rawThreads, ok := config["threads"].([]any); ok {
			for _, rt := range rawThreads {
				if t, ok := rt.(map[string]any); ok {
					depth := 0
					if d, ok := t["depth"].(float64); ok {
						depth = int(d)
					}
					threads = append(threads, map[string]any{
						"id":        t["id"],
						"parent_id": t["parent_id"],
						"depth":     depth,
						"directive": t["directive"],
						"tools":     t["tools"],
						"mcp_names": t["mcp_names"],
						"iteration": 0,
						"rate":      "stopped",
						"model":     "",
						"age":       "",
					})
				}
			}
		}
		writeJSON(w, threads)

	case "/status":
		writeJSON(w, map[string]any{
			"iteration":      0,
			"rate":           "stopped",
			"model":          "",
			"paused":         false,
			"threads":        0,
			"memories":       0,
			"uptime_seconds": 0,
			"mode":           inst.Mode,
		})

	case "/config":
		directive, _ := config["directive"].(string)
		if directive == "" {
			directive = inst.Directive
		}
		mode, _ := config["mode"].(string)
		if mode == "" {
			mode = inst.Mode
		}
		// Return the full persisted config surface — mcp_servers,
		// computer, providers, threads — so the stopped-instance UI
		// renders the real state rather than a placeholder. Before
		// this fix we hard-coded mcp_servers:[] even though the disk
		// config had them; the MCP pane showed empty for every
		// stopped agent.
		out := map[string]any{
			"directive":   directive,
			"mode":        mode,
			"mcp_servers": config["mcp_servers"],
			"computer":    config["computer"],
			"providers":   config["providers"],
			"threads":     config["threads"],
			"unconscious": config["unconscious"],
		}
		if out["mcp_servers"] == nil {
			out["mcp_servers"] = []any{}
		}
		writeJSON(w, out)
	}
}

// handleStoppedMutation attempts to satisfy a mutation request against a
// stopped instance by rewriting its on-disk config.json. Returns true if
// the request was handled (response already written). Returns false if
// the operation is one that genuinely needs a running core — caller
// should fall through to the standard 503.
//
// Supported today:
//   - DELETE /threads/:id              → drop a persisted sub-thread from config
//   - PUT    /threads/:id              → upsert fields on a persisted sub-thread
//
// Not supported while stopped (return false, caller 503s):
//   - POST /event, /chat/*, /kill, /invoke — these need the live core
//   - SSE endpoints — no live events to stream
func (s *Server) handleStoppedMutation(w http.ResponseWriter, r *http.Request, inst *Instance, corePath string) bool {
	// DELETE /threads/:id — remove from persisted threads list.
	if r.Method == http.MethodDelete && strings.HasPrefix(corePath, "/threads/") {
		tid := strings.TrimPrefix(corePath, "/threads/")
		if tid == "" || tid == "main" {
			http.Error(w, "cannot delete main thread", http.StatusBadRequest)
			return true
		}
		err := s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
			raw, _ := cfg["threads"].([]any)
			var kept []any
			for _, t := range raw {
				if m, ok := t.(map[string]any); ok {
					if id, _ := m["id"].(string); id == tid {
						continue
					}
				}
				kept = append(kept, t)
			}
			if len(kept) == 0 {
				delete(cfg, "threads")
			} else {
				cfg["threads"] = kept
			}
			return nil
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("persist threads: %v", err), http.StatusInternalServerError)
			return true
		}
		log.Printf("[THREADS] stopped instance=%d dropped persisted thread %q", inst.ID, tid)
		writeJSON(w, map[string]any{"status": "deleted", "id": tid, "applies_on": "next_start"})
		return true
	}

	// PUT /threads/:id — update fields on a persisted sub-thread.
	if r.Method == http.MethodPut && strings.HasPrefix(corePath, "/threads/") {
		tid := strings.TrimPrefix(corePath, "/threads/")
		if tid == "" {
			http.Error(w, "missing thread id", http.StatusBadRequest)
			return true
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var patch map[string]any
		if err := json.Unmarshal(bodyBytes, &patch); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return true
		}
		err := s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
			raw, _ := cfg["threads"].([]any)
			found := false
			for _, t := range raw {
				if m, ok := t.(map[string]any); ok {
					if id, _ := m["id"].(string); id == tid {
						for k, v := range patch {
							m[k] = v
						}
						found = true
						break
					}
				}
			}
			if !found {
				return fmt.Errorf("thread %q not found", tid)
			}
			cfg["threads"] = raw
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		log.Printf("[THREADS] stopped instance=%d updated persisted thread %q", inst.ID, tid)
		writeJSON(w, map[string]any{"status": "updated", "id": tid, "applies_on": "next_start"})
		return true
	}

	return false
}

// writeStoppedConfigAtomic mutates /data/instance_N/config.json directly,
// used when a client asks to edit an instance whose core is not running.
// The mutator receives the current config map (empty if no file) and
// mutates it in place; we then write via tmp+rename so a concurrent core
// boot never sees a half-written file.
//
// Why this exists: for stopped instances the dashboard needs to change
// the directive, add/remove MCPs, drop persisted threads, etc. The core
// is the runtime owner of config.json while it's alive, but when port==0
// there is no core — the file is the only source of truth. Writing it
// directly is safer than spawning a transient core just to apply edits.
func (s *Server) writeStoppedConfigAtomic(instanceID int64, mutator func(cfg map[string]any) error) error {
	dir := s.instances.instanceDir(instanceID)
	if dir == "" {
		return fmt.Errorf("no instance directory for id=%d", instanceID)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	var cfg map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	if err := mutator(cfg); err != nil {
		return err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// GET /instances/:id/channels — list connected channels for an instance
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
	instanceID, _ := atoi64(parts[0])
	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	ic := s.instances.GetChannels(inst.ID)
	var channels []map[string]string
	cliStatus := "disconnected"
	if ic != nil && ic.cli != nil && ic.cli.IsConnected() {
		cliStatus = "connected"
	}
	channels = append(channels, map[string]string{"id": "cli", "status": cliStatus})
	if ic != nil && ic.telegram != nil {
		channels = append(channels, map[string]string{
			"id":       "telegram",
			"status":   "connected",
			"bot_name": ic.telegram.BotName(),
		})
	}
	// Include persisted channels (slack, email, etc.)
	if records, _ := s.store.ListChannels(inst.ID); records != nil {
		for _, r := range records {
			switch r.Type {
			case "slack":
				status := "disconnected"
				if getSlackGateway(inst.ProjectID) != nil {
					status = "connected"
				}
				channels = append(channels, map[string]string{"id": "slack", "name": r.Name, "status": status})
			case "email":
				status := "disconnected"
				if getEmailGateway(inst.ProjectID) != nil {
					status = "connected"
				}
				channels = append(channels, map[string]string{"id": "email", "name": r.Name, "status": status})
			}
		}
	}
	writeJSON(w, channels)
}

func proxyPUT(port int, path string, body any, coreAPIKey string) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	if coreAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreAPIKey)
	}
	http.DefaultClient.Do(req)
}

// POST /instances/:id/system-mcp
//
// Body: {"name": "apteva-server"|"channels", "enable": true|false}
//
// Flips the corresponding include_apteva_server / include_channels flag
// on inst.Config. These flags are only consulted at Start() time
// (instances.go:288-299), so toggling them on a running instance does
// NOT alter the live MCP list until the instance is restarted — we
// report restart_required=true in that case so the UI can prompt.
//
// Also flips the flag off when enable=false, matching the existing PUT
// /config behaviour where omitting the system MCP from mcp_servers
// flips it off. This gives the dashboard a single, clear action for
// re-enabling a previously-opted-out system MCP without having to
// synthesize a full mcp_servers payload.
func (s *Server) handleSystemMCPToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/system-mcp")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetInstance(userID, instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	var body struct {
		Name   string `json:"name"`
		Enable *bool  `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Enable == nil {
		http.Error(w, "enable (bool) required", http.StatusBadRequest)
		return
	}

	var flag string
	switch body.Name {
	case "apteva-server":
		flag = "include_apteva_server"
	case "channels", "apteva-channels":
		flag = "include_channels"
	default:
		http.Error(w, fmt.Sprintf("unknown system MCP %q (expected apteva-server or channels)", body.Name), http.StatusBadRequest)
		return
	}

	var instCfg map[string]any
	if inst.Config != "" {
		json.Unmarshal([]byte(inst.Config), &instCfg)
	}
	if instCfg == nil {
		instCfg = map[string]any{}
	}
	previous, _ := instCfg[flag].(bool)
	instCfg[flag] = *body.Enable
	if out, merr := json.Marshal(instCfg); merr == nil {
		inst.Config = string(out)
	}
	if err := s.store.UpdateInstance(inst); err != nil {
		http.Error(w, "failed to persist", http.StatusInternalServerError)
		return
	}

	running := s.instances.GetPort(inst.ID) > 0
	writeJSON(w, map[string]any{
		"name":             body.Name,
		"enable":           *body.Enable,
		"previous":         previous,
		"restart_required": running && previous != *body.Enable,
	})
}
