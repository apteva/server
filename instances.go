package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apteva/server/apps/framework"
)

var errAgentAlreadyRunning = errors.New("agent already running")

type runningAgent struct {
	cmd        *exec.Cmd
	port       int
	pid        int
	coreAPIKey string         // API key injected into core for auth
	channels   *AgentChannels // channel infrastructure for this instance
	reattached bool           // process was inherited from an older server process
	waitOnce   sync.Once
	done       chan struct{}
	waitErr    error
	diagMu     sync.Mutex
	lastProc   string // latest /proc snapshot captured while the child was alive
}

func (r *runningAgent) wait() error {
	if r == nil || r.cmd == nil || r.reattached {
		return nil
	}
	r.waitOnce.Do(func() {
		if r.done == nil {
			r.done = make(chan struct{})
		}
		r.waitErr = r.cmd.Wait()
		close(r.done)
	})
	if r.done != nil {
		<-r.done
	}
	return r.waitErr
}

type coreRuntimeInfo struct {
	Version       string
	BuildTime     string
	UptimeSeconds int
}

func (r *runningAgent) process() *os.Process {
	if r == nil {
		return nil
	}
	if r.cmd != nil && r.cmd.Process != nil {
		return r.cmd.Process
	}
	return nil
}

func (r *runningAgent) processState() *os.ProcessState {
	if r == nil || r.cmd == nil {
		return nil
	}
	return r.cmd.ProcessState
}

func (r *runningAgent) isRunning() bool {
	if r == nil || r.port <= 0 {
		return false
	}
	if r.reattached {
		return true
	}
	if r.cmd != nil && r.cmd.Process == nil {
		return r.cmd.ProcessState == nil
	}
	return r.process() != nil && r.processState() == nil
}

func (r *runningAgent) processID() int {
	if r == nil {
		return 0
	}
	if p := r.process(); p != nil {
		return p.Pid
	}
	return r.pid
}

func (r *runningAgent) setProcSnapshot(snapshot string) {
	if r == nil || snapshot == "" {
		return
	}
	r.diagMu.Lock()
	r.lastProc = snapshot
	r.diagMu.Unlock()
}

func (r *runningAgent) procSnapshot() string {
	if r == nil {
		return ""
	}
	r.diagMu.Lock()
	defer r.diagMu.Unlock()
	return r.lastProc
}

type AgentManager struct {
	mu        sync.RWMutex
	processes map[int64]*runningAgent // instanceID → running process + port
	dataDir   string
	coreCmd   string // path to core binary
	serverCmd string // optional apteva-server binary override for the stdio management gateway

	// PostChannelsInit is invoked right after an instance's
	// ChannelRegistry is created and the CLI bridge is registered,
	// but BEFORE the channels MCP server boots and the core binary
	// is spawned. The Apteva Apps framework uses this hook to
	// register per-instance channels (chat, helpdesk, …) so they're
	// visible in the channels MCP tool list the agent discovers.
	//
	// The hook receives the Agent directly — it MUST NOT call
	// back into any AgentManager accessor that takes im.mu
	// (GetPort, GetCoreAPIKey, GetChannels, …) because Start
	// already holds im.mu.Lock() and Go's sync.RWMutex is not
	// reentrant: the re-acquire would deadlock silently.
	//
	// Leave nil in tests or single-instance bring-up paths that
	// don't have an apps registry yet.
	PostChannelsInit func(inst *Agent, ic *AgentChannels)

	// ComponentCatalog returns the chat-attachment components the
	// channel MCP advertises to a specific instance. The channel MCP
	// server bakes this list into the `respond` tool's description
	// each turn so the agent learns what's renderable without a
	// separate discovery call. Closes over the platform-wide
	// InstalledAppsRegistry; left nil in tests.
	//
	// attachedMCPNames is the set of MCP server names the instance
	// has in its user-configured mcp_servers list at start time. We
	// filter the catalog by this set so an agent that only has the
	// media MCP attached can't accidentally attach storage cards (or
	// any other app's components) it has no tool access to.
	ComponentCatalog func(projectID string, attachedMCPNames []string) []componentEntry

	// CapabilityMemorySync keeps server-owned capability guidance in
	// agent memory when system MCPs are enabled. Start calls it before
	// spawning core with live=false, while Reattach calls it after the
	// live core is recorded with live=true.
	CapabilityMemorySync func(inst *Agent, includeChannels bool, live bool) error
}

func describeProcessState(ps *os.ProcessState) string {
	if ps == nil {
		return "processState=nil"
	}
	parts := []string{fmt.Sprintf("exitCode=%d", ps.ExitCode())}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		parts = append(parts, fmt.Sprintf("waitStatus=%#x", int(ws)))
		if ws.Signaled() {
			parts = append(parts, fmt.Sprintf("signal=%s", ws.Signal()))
		}
		if ws.CoreDump() {
			parts = append(parts, "coreDump=true")
		}
		if ws.Exited() {
			parts = append(parts, fmt.Sprintf("status=%d", ws.ExitStatus()))
		}
		if ws.Stopped() {
			parts = append(parts, fmt.Sprintf("stopped=%s", ws.StopSignal()))
		}
	}
	return strings.Join(parts, " ")
}

func coreProcSnapshot(pid int) string {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Sprintf("pid=%d proc_status_unavailable=%v", pid, err)
	}
	want := map[string]bool{
		"Name": true, "State": true, "PPid": true, "Threads": true,
		"VmSize": true, "VmRSS": true, "VmData": true, "VmStk": true,
	}
	parts := []string{fmt.Sprintf("pid=%d", pid)}
	for _, line := range strings.Split(string(status), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && want[key] {
			parts = append(parts, key+"="+strings.Join(strings.Fields(val), " "))
		}
	}
	if fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		parts = append(parts, fmt.Sprintf("FDs=%d", len(fds)))
	}
	return strings.Join(parts, " ")
}

func monitorCoreProc(ri *runningAgent, pid int, stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	sample := func() {
		ri.setProcSnapshot(coreProcSnapshot(pid))
	}
	sample()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sample()
		}
	}
}

// extractMCPNames pulls the user-configured MCP server names off
// a parsed instance config, in the order they appear. Used at
// instance start to seed the channel MCP's component catalog
// filter. System MCPs are added later on the first Start but remain
// in config.json on subsequent restarts, so filter them explicitly:
// they do not ship UI components.
func extractMCPNames(config map[string]any) []string {
	raw, ok := config["mcp_servers"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name != "" {
			if name == "apteva-server" || isServerOwnedOutputMCP(name) {
				continue
			}
			out = append(out, name)
		}
	}
	return out
}

const agentOutputMCPName = "agent-output"

// channelsMCPConfig is the conversation-only durable reply/publication
// surface. Main retains the server in its deferred catalog so authenticated
// API-created conversation threads can preload it by name, but its schemas are
// absent from main's active prompt. Runtime caller-context checks remain the
// authority boundary.
func channelsMCPConfig(url string) map[string]any {
	entry := map[string]any{
		"name":      "channels",
		"url":       url,
		"transport": "http",
		"tool_loading": map[string]any{
			"default": "deferred",
		},
		"no_spawn": true,
	}
	return entry
}

// agentOutputMCPConfig is main's central operator-output surface: one mutable
// status, full Inbox publication, and explicit external notifications. It is
// always loaded for main and never attached to user conversation threads.
func agentOutputMCPConfig(url string) map[string]any {
	return map[string]any{
		"name":      agentOutputMCPName,
		"url":       url,
		"transport": "http",
		"no_spawn":  true,
		"tool_loading": map[string]any{
			"default": "always",
		},
	}
}

func isServerOwnedOutputMCP(name string) bool {
	switch strings.TrimSpace(name) {
	case "channels", "apteva-channels", agentOutputMCPName, "apteva-agent-output":
		return true
	default:
		return false
	}
}

func waitForChannelMCPReady(server *channelMCPServer) {
	if server == nil {
		return
	}
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", server.port), 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func NewAgentManager(dataDir, coreCmd string) *AgentManager {
	os.MkdirAll(dataDir, 0755)
	return &AgentManager{
		processes: make(map[int64]*runningAgent),
		dataDir:   dataDir,
		coreCmd:   coreCmd,
	}
}

func (im *AgentManager) gatewayCommand() string {
	if strings.TrimSpace(im.serverCmd) != "" {
		return im.serverCmd
	}
	serverBin, _ := os.Executable()
	return serverBin
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
func (im *AgentManager) allocPort() int {
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

func (im *AgentManager) instanceDir(id int64) string {
	dir := filepath.Join(im.dataDir, fmt.Sprintf("instance_%d", id))
	os.MkdirAll(dir, 0755)
	return dir
}

// InstanceDir is the exported accessor for an instance's on-disk directory
// (config.json, history/, memory.jsonl). Used by environment snapshotting to
// capture/restore an agent's full state. Like instanceDir it ensures the
// directory exists.
func (im *AgentManager) InstanceDir(id int64) string { return im.instanceDir(id) }

// PreSeedConfig writes a starting config.json into an instance's
// directory. Start reads disk-first ("Disk config.json is the single
// source of truth"), so any field the caller wants the spawned core
// to see, including mcp_servers, has to land on disk before Start runs.
func (im *AgentManager) PreSeedConfig(instID int64, cfgJSON string) error {
	dir := im.instanceDir(instID)
	return os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0644)
}

// ProviderInfo holds provider metadata for config.json injection.
type ProviderInfo struct {
	Type              string
	ModelLarge        string
	ModelMedium       string
	ModelSmall        string
	RealtimeVoice     string
	BuiltinTools      []string
	ModelCapabilities map[string]ProviderModelCapabilities
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

func (im *AgentManager) Start(inst *Agent, providerEnv map[string]string, serverPort string, providerPool []ProviderInfo, instanceSecret string, channelConfigs ...ChannelConfig) error {
	log.Printf("[SPAWN] Start called for agent=%d name=%q project=%s", inst.ID, inst.Name, inst.ProjectID)
	im.mu.Lock()
	defer im.mu.Unlock()

	if ri, running := im.processes[inst.ID]; running && ri.isRunning() {
		log.Printf("[SPAWN] agent=%d already running pid=%d port=%d", inst.ID, ri.processID(), ri.port)
		return fmt.Errorf("%w: instance %d", errAgentAlreadyRunning, inst.ID)
	}

	port := im.allocPort()
	dir := im.instanceDir(inst.ID)

	// Get server binary path for MCP gateway.
	serverBin := im.gatewayCommand()

	// Build config.json — restore saved config from DB, then ensure directive/mode/gateway are current
	mode := inst.Mode
	if mode == "" {
		mode = "autonomous"
	}

	gateway := map[string]any{
		"name":    "apteva-server",
		"command": serverBin,
		"args":    []string{"--mcp-gateway", fmt.Sprintf("--user-id=%d", inst.UserID)},
		// no_spawn hides this server from sub-thread search_tools
		// results and refuses sub-thread spawn(mcps=[...]) attempts.
		// Management capabilities (creating agents, MCP servers, …)
		// stay reachable from main only. Main discovers them via
		// search_tools or per-turn BM25 preload, like any other MCP.
		"no_spawn": true,
	}

	// Disk config.json is the single source of truth.
	// Core owns it — threads, directives, MCP connections are all here.
	// Server only injects gateway + channels MCP entries (URLs change on each start).
	config := map[string]any{}
	if diskConfig, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(diskConfig, &config)
	}

	// Wizard hand-off: handleCreateInstance writes operator-selected
	// MCP servers (per-connection HTTP entries from the Setup step's
	// bound_connection_ids) into the DB row's inst.Config field — but
	// for a brand-new instance, disk config.json doesn't exist yet,
	// so those entries would be silently dropped at the disk-read
	// above. Merge them in (dedupe by name) so the wizard's choices
	// reach core. Once disk config.json exists and contains them,
	// subsequent starts pull them from disk and the dedupe makes
	// this a no-op.
	if inst.Config != "" {
		var instCfg map[string]any
		if json.Unmarshal([]byte(inst.Config), &instCfg) == nil {
			if staged, ok := instCfg["mcp_servers"].([]any); ok && len(staged) > 0 {
				existing, _ := config["mcp_servers"].([]any)
				seen := map[string]bool{}
				for _, s := range existing {
					if sm, ok := s.(map[string]any); ok {
						if name, _ := sm["name"].(string); name != "" {
							seen[name] = true
						}
					}
				}
				for _, s := range staged {
					if sm, ok := s.(map[string]any); ok {
						if name, _ := sm["name"].(string); name == "" || seen[name] {
							continue
						}
					}
					existing = append(existing, s)
				}
				config["mcp_servers"] = existing
			}
		}
	}

	// Set directive/mode from disk. Fall back to DB only for brand new instances (no config.json yet).
	if _, hasDirective := config["directive"]; !hasDirective || config["directive"] == "" {
		config["directive"] = inst.Directive
	}
	if _, hasMode := config["mode"]; !hasMode || config["mode"] == "" {
		config["mode"] = mode
	}

	// Read default_provider from instance config. The disk metadata branch is
	// retained for old installations; current servers persist it in the agent
	// DB config so every restart resolves the same provider.
	defaultProvider := configuredAgentDefaultProvider(inst.Config)
	if instCfg, ok := config["_instance_config"].(string); defaultProvider == "" && ok {
		var ic map[string]any
		json.Unmarshal([]byte(instCfg), &ic)
		if diskDefault, _ := ic["default_provider"].(string); strings.TrimSpace(diskDefault) != "" {
			defaultProvider = providerKeyFromName(diskDefault)
		}
	}

	// Inject providers array into config (core reads "providers" field)
	if len(providerPool) > 0 {
		provArray := buildAgentCoreProviderConfigs(providerPool, inst.Config, defaultProvider)
		if len(provArray) > 0 {
			config["providers"] = provArray
			delete(config, "provider") // remove legacy single-provider field
		}
	}
	// Realtime is capability-gated but should work out of the box when a
	// realtime provider is present. Preserve an explicit disk false.
	enableRealtimeByDefault(config, providerPool)

	// Core no longer owns browser sessions. Strip any stale legacy
	// browser config so old instance files cannot register duplicate
	// computer tools beside the Computer app MCP tools.
	delete(config, "computer")

	// Create channels infrastructure for this instance
	ic := &AgentChannels{registry: NewChannelRegistry()}
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

	// Start the conversation-only Channels MCP and main-owned operator-output
	// MCP over the same per-agent registry.
	channelsMCP, err := newProfiledChannelMCPServer(ic.registry, channelMCPProfileConversation)
	if err == nil {
		channelsMCP.ic = ic
		// Close over the project AND this instance's attached MCP
		// servers so the channel MCP enumerates only the components
		// belonging to apps the agent actually has access to. Pre-fix
		// the catalog was project-wide — an agent with only the
		// `media` MCP attached would still see `storage`'s file-card
		// in its `respond` description and dutifully attach it,
		// confusing the user (and the storage app, which the agent
		// can't actually call).
		//
		// MCP names are extracted from the user-side mcp_servers
		// list as it stood when the agent was started (system MCPs
		// like `channels` and `apteva-server` aren't in this list
		// yet at this point in Start — they get appended below).
		// Frozen-at-start: changing the agent's MCP set requires a
		// restart for the catalog to refresh.
		if im.ComponentCatalog != nil {
			pid := inst.ProjectID
			attached := extractMCPNames(config)
			channelsMCP.componentCatalog = func() []componentEntry {
				return im.ComponentCatalog(pid, attached)
			}
			// One-time diagnostic: log the catalog the agent will
			// see in its respond tool description. Helps confirm the
			// pipeline is wired correctly without having to dig
			// through truncated MCP-HTTP logs.
			cat := im.ComponentCatalog(pid, attached)
			log.Printf("[CHAT-MCP] agent=%d project=%s attached_mcps=%v catalog=%d entries",
				inst.ID, pid, attached, len(cat))
			for _, c := range cat {
				log.Printf("[CHAT-MCP]   {app:%q, name:%q, slots:%v}", c.App, c.Name, c.Slots)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("failed to start conversation channels MCP: %w", err)
	}
	outputMCP, err := newProfiledChannelMCPServer(ic.registry, channelMCPProfileAgentOutput)
	if err != nil {
		channelsMCP.close()
		return fmt.Errorf("failed to start agent output MCP: %w", err)
	}
	outputMCP.ic = ic
	ic.mcp = channelsMCP
	ic.outputMCP = outputMCP
	go channelsMCP.serve()
	go outputMCP.serve()

	waitForChannelMCPReady(channelsMCP)
	waitForChannelMCPReady(outputMCP)

	// Main keeps the conversation server only as a deferred scope that the
	// server can grant to API-created user conversations. Main's own active
	// output schemas come from the separate always-loaded operator surface.
	channelsEntry := channelsMCPConfig(channelsMCP.url())
	outputEntry := agentOutputMCPConfig(outputMCP.url())

	// Read the opt-in flags for the auto-injected system MCPs. These
	// live in the instance's DB record (inst.Config JSON blob) rather
	// than disk config.json — core owns the disk config and drops
	// unknown fields on save, so any server-only state needs to live
	// elsewhere. Channels default on; the platform gateway is private
	// and must be explicitly set by server-owned helper setup.
	includeGateway := false
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

	// Merge apteva-server and the role-split output servers into existing MCPs.
	// Preserve all other MCP servers (schedule, social, helpdesk, etc.) that were
	// added at runtime or manually. Only replace server-owned system entries.
	var userServers []any
	if existing, ok := config["mcp_servers"].([]any); ok {
		for _, s := range existing {
			if sm, ok := s.(map[string]any); ok {
				name, _ := sm["name"].(string)
				if isServerOwnedOutputMCP(name) || name == "apteva-server" {
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
		systemEntries = append(systemEntries, outputEntry, channelsEntry)
	}
	config["mcp_servers"] = append(systemEntries, userServers...)

	if im.CapabilityMemorySync != nil {
		if err := im.CapabilityMemorySync(inst, includeChannels, false); err != nil {
			log.Printf("[CAPABILITY-MEMORY] startup sync agent=%d include_channels=%v: %v", inst.ID, includeChannels, err)
		}
	}

	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(dir, "config.json"), configData, 0644)

	// Generate a unique API key for this core instance. Persisted on
	// the agent row so a new server process can reattach to a
	// detached, still-running core after an update.
	coreAPIKey := inst.CoreAPIKey
	if coreAPIKey == "" {
		coreAPIKey = "core_" + generateToken(16)
	}

	cmd := exec.Command(im.coreCmd, "--headless")
	cmd.Dir = dir
	env := append(os.Environ(),
		"API_PORT="+itoa64(int64(port)),
		"NO_TUI=1",
		"NO_CONSOLE=1", // server has its own ConsoleLogger
		"SERVER_URL=http://127.0.0.1:"+serverPort,
		"TELEMETRY_URL=http://127.0.0.1:"+serverPort+"/api/telemetry",
		"TELEMETRY_LIVE_URL=http://127.0.0.1:"+serverPort+"/api/telemetry/live",
		// Phase 2: write both legacy + canonical env names so a future
		// apteva-core build can read AGENT_ID / AGENT_SECRET first and
		// fall back to INSTANCE_*. Once every running core has been
		// upgraded past that point, Phase 4 drops the legacy vars.
		"INSTANCE_ID="+itoa64(inst.ID),
		"AGENT_ID="+itoa64(inst.ID),
		"PROJECT_ID="+inst.ProjectID,
		"APTEVA_API_KEY="+coreAPIKey,
		"INSTANCE_SECRET="+instanceSecret,
		"AGENT_SECRET="+instanceSecret,
	)
	for k, v := range providerEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	log.Printf("[SPAWN] exec %s --headless dir=%s port=%d providerEnvKeys=%v", im.coreCmd, dir, port, providerEnvKeys(providerEnv))
	if err := cmd.Start(); err != nil {
		log.Printf("[SPAWN] exec failed: %v", err)
		return fmt.Errorf("failed to start core: %w", err)
	}
	log.Printf("[SPAWN] core started agent=%d pid=%d port=%d", inst.ID, cmd.Process.Pid, port)

	// Background health check — dial the port every 100ms for 5s so we can
	// see in logs exactly when (or if) core becomes reachable.
	go func(id int64, pid, p int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				log.Printf("[SPAWN] core agent=%d pid=%d port=%d is LISTENING", id, pid, p)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		log.Printf("[SPAWN] core agent=%d pid=%d port=%d FAILED to listen within 5s (last check: connection refused)", id, pid, p)
	}(inst.ID, cmd.Process.Pid, port)

	ri := &runningAgent{cmd: cmd, port: port, pid: cmd.Process.Pid, coreAPIKey: coreAPIKey, channels: ic, done: make(chan struct{})}
	im.processes[inst.ID] = ri
	inst.Port = port
	inst.Pid = cmd.Process.Pid
	inst.CoreAPIKey = coreAPIKey
	inst.Status = "running"
	procDiagStop := make(chan struct{})
	go monitorCoreProc(ri, cmd.Process.Pid, procDiagStop)

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
		waitErr := ri.wait()
		lived := time.Since(startedAt)
		close(procDiagStop)
		procSnapshot := ri.procSnapshot()
		stateDesc := describeProcessState(cmd.ProcessState)
		log.Printf("[SPAWN] core EXITED agent=%d pid=%d port=%d lived=%s waitErr=%v %s lastProc={%s}",
			instID, spawnedPid, spawnedPort, lived, waitErr, stateDesc, procSnapshot)
		im.mu.Lock()
		current := im.processes[instID]
		if current != nil && current.channels != nil {
			current.channels.Stop()
		}
		delete(im.processes, instID)
		im.mu.Unlock()
		log.Printf("[SPAWN] cleaned up process map for agent=%d", instID)
	}()

	return nil
}

func providerIsDefault(providerType, configuredDefault string, index int) bool {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	configuredDefault = strings.ToLower(strings.TrimSpace(configuredDefault))
	if configuredDefault == "" {
		return index == 0
	}
	if providerType == configuredDefault {
		return true
	}
	baseProvider := strings.TrimSuffix(providerType, "-realtime")
	return baseProvider != providerType && baseProvider == configuredDefault
}

func isRealtimeProviderType(providerType string) bool {
	return strings.HasSuffix(providerKeyFromName(providerType), "-realtime")
}

func configuredAgentDefaultProvider(configJSON string) string {
	var config map[string]any
	if json.Unmarshal([]byte(configJSON), &config) != nil {
		return ""
	}
	value, _ := config["default_provider"].(string)
	return providerKeyFromName(value)
}

func configuredAgentModelOverride(configJSON, providerName string) string {
	var config map[string]any
	if json.Unmarshal([]byte(configJSON), &config) != nil {
		return ""
	}
	override, _ := config["model_override"].(map[string]any)
	provider, _ := override["provider"].(string)
	model, _ := override["model"].(string)
	if providerKeyFromName(provider) != providerKeyFromName(providerName) {
		return ""
	}
	return strings.TrimSpace(model)
}

// effectiveProviderDefault resolves an explicit agent pin against the current
// text-provider pool. Missing or stale pins fall back deterministically to the
// first text provider supplied by GetProviderPool.
func effectiveProviderDefault(pool []ProviderInfo, configured string) string {
	configured = providerKeyFromName(configured)
	if configured != "" {
		for _, provider := range pool {
			name := providerKeyFromName(provider.Type)
			if !isRealtimeProviderType(name) && name == configured {
				return name
			}
		}
	}
	for _, provider := range pool {
		name := providerKeyFromName(provider.Type)
		if name != "" && !isRealtimeProviderType(name) {
			return name
		}
	}
	return ""
}

func buildCoreProviderConfigs(pool []ProviderInfo, configuredDefault string) []map[string]any {
	defaultProvider := effectiveProviderDefault(pool, configuredDefault)
	providers := make([]map[string]any, 0, len(pool))
	for i, provider := range pool {
		name := providerKeyFromName(provider.Type)
		if name == "" {
			continue
		}
		entry := map[string]any{
			"name": name,
			"models": map[string]string{
				"large":  provider.ModelLarge,
				"medium": provider.ModelMedium,
				"small":  provider.ModelSmall,
			},
			"default": providerIsDefault(name, defaultProvider, i),
		}
		if provider.RealtimeVoice != "" {
			entry["realtime_voice"] = provider.RealtimeVoice
		}
		if len(provider.ModelCapabilities) > 0 {
			entry["model_capabilities"] = provider.ModelCapabilities
		}
		if len(provider.BuiltinTools) > 0 {
			entry["builtin_tools"] = provider.BuiltinTools
		}
		providers = append(providers, entry)
	}
	return providers
}

func applyAgentModelOverride(providers []map[string]any, providerName, model string) {
	providerName = providerKeyFromName(providerName)
	model = strings.TrimSpace(model)
	if providerName == "" || model == "" {
		return
	}
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		if providerKeyFromName(name) != providerName {
			continue
		}
		provider["models"] = map[string]string{
			"large":  model,
			"medium": model,
			"small":  model,
		}
		return
	}
}

func buildAgentCoreProviderConfigs(pool []ProviderInfo, configJSON string, fallbackDefault ...string) []map[string]any {
	configuredDefault := configuredAgentDefaultProvider(configJSON)
	if configuredDefault == "" && len(fallbackDefault) > 0 {
		configuredDefault = providerKeyFromName(fallbackDefault[0])
	}
	providers := buildCoreProviderConfigs(pool, configuredDefault)
	effectiveDefault := effectiveProviderDefault(pool, configuredDefault)
	applyAgentModelOverride(providers, effectiveDefault, configuredAgentModelOverride(configJSON, effectiveDefault))
	return providers
}

func requestedTextProviderDefault(providers []map[string]any) (string, error) {
	selected := ""
	for _, provider := range providers {
		isDefault, _ := provider["default"].(bool)
		if !isDefault {
			continue
		}
		name, _ := provider["name"].(string)
		name = providerKeyFromName(name)
		if name == "" || isRealtimeProviderType(name) {
			continue
		}
		if selected != "" && selected != name {
			return "", fmt.Errorf("multiple default LLM providers requested")
		}
		selected = name
	}
	return selected, nil
}

// hydrateCoreProviderConfigs turns the dashboard's intentionally narrow
// {name, default} selection into the complete provider configuration expected
// by Core. Explicit per-agent overrides remain supported for API clients.
func hydrateCoreProviderConfigs(pool []ProviderInfo, configuredDefault string, requested []map[string]any) ([]map[string]any, string, error) {
	requestedDefault, err := requestedTextProviderDefault(requested)
	if err != nil {
		return nil, "", err
	}
	if requestedDefault == "" {
		requestedDefault = configuredDefault
	}
	effectiveDefault := effectiveProviderDefault(pool, requestedDefault)
	if effectiveDefault == "" {
		return nil, "", fmt.Errorf("no LLM provider configured")
	}
	if requestedDefault != "" && providerKeyFromName(requestedDefault) != effectiveDefault {
		return nil, "", fmt.Errorf("LLM provider %q is not configured for this project", requestedDefault)
	}

	providers := buildCoreProviderConfigs(pool, effectiveDefault)
	byName := make(map[string]map[string]any, len(requested))
	for _, provider := range requested {
		name, _ := provider["name"].(string)
		name = providerKeyFromName(name)
		if name != "" {
			byName[name] = provider
		}
	}
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		override := byName[name]
		for _, field := range []string{"models", "model_capabilities", "builtin_tools", "realtime_voice"} {
			if value, ok := override[field]; ok {
				provider[field] = value
			}
		}
	}
	return providers, effectiveDefault, nil
}

func enableRealtimeByDefault(config map[string]any, providers []ProviderInfo) {
	if _, explicitlyConfigured := config["realtime_enabled"]; explicitlyConfigured {
		return
	}
	for _, provider := range providers {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(provider.Type)), "-realtime") {
			config["realtime_enabled"] = true
			return
		}
	}
}

func coreHealthOK(port int, coreAPIKey string, timeout time.Duration) bool {
	if port <= 0 {
		return false
	}
	client := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if coreAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreAPIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func terminateRuntimePID(pid, port int, coreAPIKey string, graceful time.Duration) {
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(graceful)
	for time.Now().Before(deadline) {
		if port <= 0 || !coreHealthOK(port, coreAPIKey, 100*time.Millisecond) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Kill()
}

func (im *AgentManager) Reattach(inst *Agent, serverPort string, channelConfigs ...ChannelConfig) error {
	if inst == nil {
		return fmt.Errorf("agent is nil")
	}
	if inst.Port <= 0 || inst.Pid <= 0 || inst.CoreAPIKey == "" {
		return fmt.Errorf("missing persisted runtime metadata")
	}
	if !processAlive(inst.Pid) {
		return fmt.Errorf("pid %d is not alive", inst.Pid)
	}
	if !coreHealthOK(inst.Port, inst.CoreAPIKey, 500*time.Millisecond) {
		return fmt.Errorf("core health failed on port %d", inst.Port)
	}

	im.mu.Lock()
	if ri, running := im.processes[inst.ID]; running && ri.isRunning() {
		im.mu.Unlock()
		return fmt.Errorf("%w: instance %d", errAgentAlreadyRunning, inst.ID)
	}
	im.mu.Unlock()

	dir := im.instanceDir(inst.ID)
	serverBin := im.gatewayCommand()
	config := map[string]any{}
	if diskConfig, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(diskConfig, &config)
	}

	ic := &AgentChannels{registry: NewChannelRegistry()}
	ic.cli = NewCLIBridge()
	ic.registry.Register(ic.cli)
	if im.PostChannelsInit != nil {
		im.PostChannelsInit(inst, ic)
	}

	channelsMCP, err := newProfiledChannelMCPServer(ic.registry, channelMCPProfileConversation)
	if err != nil {
		return fmt.Errorf("failed to start conversation channels MCP: %w", err)
	}
	outputMCP, err := newProfiledChannelMCPServer(ic.registry, channelMCPProfileAgentOutput)
	if err != nil {
		channelsMCP.close()
		return fmt.Errorf("failed to start agent output MCP: %w", err)
	}
	channelsMCP.ic = ic
	outputMCP.ic = ic
	ic.mcp = channelsMCP
	ic.outputMCP = outputMCP
	if im.ComponentCatalog != nil {
		pid := inst.ProjectID
		attached := extractMCPNames(config)
		channelsMCP.componentCatalog = func() []componentEntry {
			return im.ComponentCatalog(pid, attached)
		}
	}
	go channelsMCP.serve()
	go outputMCP.serve()
	waitForChannelMCPReady(channelsMCP)
	waitForChannelMCPReady(outputMCP)

	gateway := map[string]any{
		"name":     "apteva-server",
		"command":  serverBin,
		"args":     []string{"--mcp-gateway", fmt.Sprintf("--user-id=%d", inst.UserID)},
		"no_spawn": true,
	}
	channelsEntry := channelsMCPConfig(channelsMCP.url())
	outputEntry := agentOutputMCPConfig(outputMCP.url())

	includeGateway := false
	includeChannels := true
	{
		var instCfg map[string]any
		if inst.Config != "" {
			_ = json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if v, ok := instCfg["include_apteva_server"].(bool); ok {
			includeGateway = v
		}
		if v, ok := instCfg["include_channels"].(bool); ok {
			includeChannels = v
		}
	}

	var userServers []any
	if existing, ok := config["mcp_servers"].([]any); ok {
		for _, s := range existing {
			if sm, ok := s.(map[string]any); ok {
				name, _ := sm["name"].(string)
				if isServerOwnedOutputMCP(name) || name == "apteva-server" {
					continue
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
		systemEntries = append(systemEntries, outputEntry, channelsEntry)
	}
	config["mcp_servers"] = append(systemEntries, userServers...)
	configData, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), configData, 0644); err != nil {
		ic.Stop()
		return fmt.Errorf("write refreshed config: %w", err)
	}

	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/config", inst.Port), bytes.NewReader(configData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+inst.CoreAPIKey)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		ic.Stop()
		return fmt.Errorf("refresh live core config: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ic.Stop()
		return fmt.Errorf("refresh live core config returned %d", resp.StatusCode)
	}

	proc, _ := os.FindProcess(inst.Pid)
	ri := &runningAgent{cmd: &exec.Cmd{Process: proc}, port: inst.Port, pid: inst.Pid, coreAPIKey: inst.CoreAPIKey, channels: ic, reattached: true}
	im.mu.Lock()
	im.processes[inst.ID] = ri
	im.mu.Unlock()

	if im.CapabilityMemorySync != nil {
		if err := im.CapabilityMemorySync(inst, includeChannels, true); err != nil {
			log.Printf("[CAPABILITY-MEMORY] reattach sync agent=%d include_channels=%v: %v", inst.ID, includeChannels, err)
		}
	}

	for _, cc := range channelConfigs {
		if cc.Type == "telegram" && cc.Config["bot_token"] != "" {
			corePort := inst.Port
			ck := inst.CoreAPIKey
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
				log.Printf("[CHANNELS] reattached telegram @%s for instance %d", botName, inst.ID)
			}
		}
	}

	log.Printf("[RESUME] agent=%d reattached existing core pid=%d port=%d", inst.ID, inst.Pid, inst.Port)
	return nil
}

// Stop kills a running core process and cleans up channels. Sends
// SIGTERM first and waits up to 2s for core to flush state before
// escalating to SIGKILL.
func (im *AgentManager) Stop(instanceID int64) {
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
	proc := ri.process()
	if proc == nil {
		return
	}
	// Phase 1: polite SIGTERM.
	log.Printf("[SPAWN] Stop requested agent=%d pid=%d port=%d reattached=%v lastProc={%s}", instanceID, ri.processID(), ri.port, ri.reattached, ri.procSnapshot())
	_ = proc.Signal(syscall.SIGTERM)
	if ri.reattached {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !coreHealthOK(ri.port, ri.coreAPIKey, 100*time.Millisecond) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		log.Printf("[SPAWN] Stop escalating SIGKILL agent=%d pid=%d reattached=true", instanceID, ri.processID())
		_ = proc.Kill()
		return
	}
	done := ri.done
	if done == nil {
		done = make(chan struct{})
		ri.done = done
		go func() {
			err := ri.wait()
			log.Printf("[SPAWN] Stop wait agent=%d pid=%d err=%v %s", instanceID, ri.processID(), err, describeProcessState(ri.processState()))
		}()
	}
	select {
	case <-done:
		// clean exit
	case <-time.After(2 * time.Second):
		// Phase 2: escalate. Chrome may be stuck on a navigation or
		// an agent may be ignoring SIGTERM — don't wait forever.
		log.Printf("[SPAWN] Stop escalating SIGKILL agent=%d pid=%d lastProc={%s}", instanceID, ri.processID(), ri.procSnapshot())
		proc.Kill()
		<-done
	}
}

// GetChannels returns the AgentChannels for a running instance, or nil.
func (im *AgentManager) GetChannels(instanceID int64) *AgentChannels {
	im.mu.RLock()
	defer im.mu.RUnlock()
	if ri, ok := im.processes[instanceID]; ok {
		return ri.channels
	}
	return nil
}

// StartTelegram starts the Telegram gateway for an instance.
func (im *AgentManager) StartTelegram(instanceID int64, token string) (string, error) {
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
//  1. SIGTERM every child, wait up to `graceful` for clean exits so
//     cores can flush session state to disk and tell their own MCP
//     children to pack up.
//  2. Anything still alive after the deadline gets SIGKILL.
//
// Cross-platform caveat: on Windows os.Process.Signal only accepts
// os.Kill, so SIGTERM silently maps to Kill there — graceful phase
// collapses to hard kill. Unix gets the full two-phase behaviour.
func (im *AgentManager) StopAll(graceful time.Duration) {
	im.mu.Lock()
	procs := make([]*runningAgent, 0, len(im.processes))
	for _, ri := range im.processes {
		if ri != nil && ri.process() != nil {
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
		ri.process().Signal(syscall.SIGTERM)
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
		go func(r *runningAgent) {
			if r.reattached {
				deadline := time.Now().Add(graceful)
				for time.Now().Before(deadline) {
					if !coreHealthOK(r.port, r.coreAPIKey, 100*time.Millisecond) {
						results <- waitResult{pid: r.processID(), err: nil}
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				if p := r.process(); p != nil {
					_ = p.Kill()
				}
				results <- waitResult{pid: r.processID(), err: fmt.Errorf("killed after graceful stop timeout")}
				return
			}
			err := r.wait()
			results <- waitResult{pid: r.processID(), err: err}
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
				if ri.reattached || ri.processState() == nil {
					ri.process().Kill()
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
func (im *AgentManager) IsRunning(instanceID int64) bool {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	return ok && ri.isRunning()
}

// GetCoreAPIKey returns the API key for a running instance.
func (im *AgentManager) GetCoreAPIKey(instanceID int64) string {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	if ok && ri.isRunning() {
		return ri.coreAPIKey
	}
	return ""
}

// GetPort returns the port for a running instance, or 0 if not running.
func (im *AgentManager) GetPort(instanceID int64) int {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	if ok && ri.isRunning() {
		return ri.port
	}
	return 0
}

func (im *AgentManager) CoreRuntimeInfo(instanceID int64) (coreRuntimeInfo, bool) {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	if !ok || !ri.isRunning() {
		im.mu.RUnlock()
		return coreRuntimeInfo{}, false
	}
	port := ri.port
	coreAPIKey := ri.coreAPIKey
	im.mu.RUnlock()

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/status", port), nil)
	if coreAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreAPIKey)
	}
	resp, err := (&http.Client{Timeout: 800 * time.Millisecond}).Do(req)
	if err != nil {
		return coreRuntimeInfo{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return coreRuntimeInfo{}, false
	}
	var body struct {
		Version       string `json:"core_version"`
		BuildTime     string `json:"core_build_time"`
		UptimeSeconds int    `json:"uptime_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return coreRuntimeInfo{}, false
	}
	return coreRuntimeInfo{
		Version:       strings.TrimSpace(body.Version),
		BuildTime:     strings.TrimSpace(body.BuildTime),
		UptimeSeconds: body.UptimeSeconds,
	}, true
}

func coreVersionOutdated(runtimeVersion, targetVersion string) bool {
	runtimeVersion = strings.TrimSpace(runtimeVersion)
	targetVersion = strings.TrimSpace(targetVersion)
	if runtimeVersion == "" || targetVersion == "" || runtimeVersion == "dev" || targetVersion == "dev" {
		return false
	}
	return runtimeVersion != targetVersion
}

func (s *Server) enrichAgentRuntime(inst *Agent) {
	if inst == nil {
		return
	}
	inst.TargetCoreVersion = CoreVersion
	if !s.agents.IsRunning(inst.ID) {
		inst.Status = "stopped"
		return
	}
	inst.Status = "running"
	info, ok := s.agents.CoreRuntimeInfo(inst.ID)
	if !ok {
		inst.CoreUpdateAvailable = coreVersionOutdated(inst.CoreVersion, CoreVersion)
		return
	}
	versionChanged := info.Version != "" && info.Version != inst.CoreVersion
	buildChanged := info.BuildTime != "" && info.BuildTime != inst.CoreBuildTime
	startedAt := coreRuntimeStartedAt(info)
	startChanged := inst.CoreStartedAt == ""
	if existing, err := parseTime(inst.CoreStartedAt); err != nil || existing.Sub(startedAt).Abs() > 2*time.Second {
		startChanged = true
	}
	shouldPersist := versionChanged || buildChanged || startChanged
	if info.Version != "" {
		inst.CoreVersion = info.Version
	}
	if info.BuildTime != "" {
		inst.CoreBuildTime = info.BuildTime
	}
	if shouldPersist {
		inst.CoreStartedAt = startedAt.Format(time.RFC3339Nano)
	}
	if shouldPersist && inst.CoreVersion != "" {
		if err := s.store.SetAgentRuntimeRunning(inst, startedAt); err != nil {
			log.Printf("[RUNTIME] reconcile agent=%d: %v", inst.ID, err)
		}
	}
	inst.CoreUpdateAvailable = coreVersionOutdated(inst.CoreVersion, CoreVersion)
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
		Name      string `json:"name"`
		Directive string `json:"directive"`
		Mode      string `json:"mode"`   // "autonomous" | "cautious" | "learn"
		Config    string `json:"config"` // optional JSON blob for MCP servers etc
		ProjectID string `json:"project_id"`
		Start     *bool  `json:"start,omitempty"` // default true; set false to create without starting
		// Auto-injected channels MCP. Defaults to true so agents can
		// reply through dashboard/chat channels. The Apteva server
		// gateway is no longer a public create option; only server-owned
		// helpers may opt into it by writing internal config directly.
		IncludeChannels *bool `json:"include_channels,omitempty"`
		// Unconscious — when true, core spawns the background
		// memory-consolidation thread (see core/thinker.go
		// unconsciousDirectiveV2). Toggled per-agent so a fast,
		// stateless agent can stay out of the memory-write cycle
		// while a personal-assistant-style agent enables it.
		Unconscious *bool `json:"unconscious,omitempty"`
		// BoundAppInstallIDs — installed apps the operator picked in the
		// wizard's Setup step. MCP-capable apps are added to the agent's
		// actual mcp_servers config; app_agent_bindings is synchronized
		// metadata used by environments, grants, and skill inheritance.
		// Omitted resolves creation defaults; a present [] explicitly opts out.
		BoundAppInstallIDs *[]int64 `json:"bound_app_install_ids,omitempty"`
		// BoundAppGrants — optional scoped access policies for the
		// bound apps above. Written before auto-start so the first
		// app MCP call sees the intended fail-closed policy.
		BoundAppGrants []struct {
			InstallID     int64      `json:"install_id"`
			DefaultEffect string     `json:"default_effect,omitempty"`
			Rules         []grantRow `json:"rules"`
		} `json:"bound_app_grants,omitempty"`
		// BoundConnectionIDs — same idea for integration connections
		// the operator wants attached as MCP servers. Each id is
		// resolved to its /mcp/<id> URL and appended to the agent's
		// config.json mcp_servers list so core attaches it at boot.
		BoundConnectionIDs []int64 `json:"bound_connection_ids,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	// Multi-user: editor+ on the target project is required to create
	// an agent in it. Empty project_id is the legacy "no project"
	// path — kept for back-compat; no project access check applies
	// there since there's nothing to check against.
	if body.ProjectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectEditor); !ok {
			return
		}
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

	// Omitted means "use the project's creation defaults". A present array,
	// including [], is an exact operator selection and therefore an opt-out.
	selectedAppInstallIDs := []int64{}
	if body.BoundAppInstallIDs == nil {
		var err error
		selectedAppInstallIDs, err = s.defaultAppInstallIDsForProject(body.ProjectID)
		if err != nil {
			http.Error(w, "resolve default apps", http.StatusInternalServerError)
			return
		}
	} else {
		selectedAppInstallIDs = append(selectedAppInstallIDs, (*body.BoundAppInstallIDs)...)
	}
	// Validate and resolve every attachment before creating the DB row. This
	// keeps defaults trustworthy: a selected app cannot silently disappear
	// while the new agent starts without it.
	attachmentProbe := &Agent{UserID: userID, ProjectID: body.ProjectID, Config: body.Config}
	validAppInstallIDs, appMCPConfigs, err := s.appMCPConfigsForInstallIDs(
		userID, attachmentProbe, selectedAppInstallIDs,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	selectedAppInstallIDs = validAppInstallIDs

	inst, err := s.store.CreateAgent(userID, body.Name, body.Directive, body.Mode, body.Config, body.ProjectID)
	if err != nil {
		http.Error(w, "failed to create instance", http.StatusInternalServerError)
		return
	}

	// Write the operator's effective app selection before startup. App
	// attachments are part of the creation contract: fail and remove the
	// not-yet-started agent instead of silently creating it without a default.
	if len(selectedAppInstallIDs) > 0 {
		tx, err := s.store.db.Begin()
		if err != nil {
			_ = s.store.DeleteAgent(userID, inst.ID)
			http.Error(w, "begin app attachment", http.StatusInternalServerError)
			return
		}
		for _, installID := range selectedAppInstallIDs {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`,
				installID, inst.ID,
			); err != nil {
				_ = tx.Rollback()
				_ = s.store.DeleteAgent(userID, inst.ID)
				http.Error(w, "attach selected app", http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			_ = s.store.DeleteAgent(userID, inst.ID)
			http.Error(w, "commit app attachments", http.StatusInternalServerError)
			return
		}
		if len(appMCPConfigs) > 0 {
			var instCfg map[string]any
			_ = json.Unmarshal([]byte(inst.Config), &instCfg)
			if instCfg == nil {
				instCfg = map[string]any{}
			}
			instCfg["mcp_servers"] = mcpMapsAsAny(mutateMCPServers(
				mcpMaps(instCfg["mcp_servers"]), appMCPConfigs, "add"))
			out, err := json.Marshal(instCfg)
			if err != nil {
				_ = s.store.DeleteAgent(userID, inst.ID)
				http.Error(w, "encode app attachments", http.StatusInternalServerError)
				return
			}
			inst.Config = string(out)
		}
		// Skills are agent-scoped companions to the attached apps. One memory
		// assignment is shared by main and every existing or future thread.
		if _, err := s.reconcileAgentAppSkills(inst); err != nil {
			_ = s.store.DeleteAgent(userID, inst.ID)
			http.Error(w, "attach app skills", http.StatusInternalServerError)
			return
		}
	}
	// Integration connection picks retain their existing best-effort behavior;
	// unlike app defaults, they are optional per-agent setup conveniences.
	if len(body.BoundAppGrants) > 0 {
		allowedInstalls := map[int64]bool{}
		for _, id := range selectedAppInstallIDs {
			allowedInstalls[id] = true
		}
		for _, policy := range body.BoundAppGrants {
			if policy.InstallID <= 0 {
				log.Printf("[CREATE] skip app grants for invalid install_id=%d agent=%d", policy.InstallID, inst.ID)
				continue
			}
			if len(allowedInstalls) > 0 && !allowedInstalls[policy.InstallID] {
				log.Printf("[CREATE] skip app grants for unbound install_id=%d agent=%d", policy.InstallID, inst.ID)
				continue
			}
			if _, err := s.replaceGrantsForInstance(
				policy.InstallID, inst.ID, policy.DefaultEffect, policy.Rules, getUserName(r),
			); err != nil {
				log.Printf("[CREATE] app grants install_id=%d agent=%d: %v", policy.InstallID, inst.ID, err)
			}
		}
	}
	if len(body.BoundConnectionIDs) > 0 {
		// Each selected connection becomes an http-transport MCP
		// server entry in the agent's config.json. core attaches
		// it on boot via its mcp_servers list. The URL points at
		// apteva-server's /mcp/<id> endpoint which proxies to the
		// connection's tools using the stored credentials.
		extraServers := []map[string]any{}
		for _, cid := range body.BoundConnectionIDs {
			conn, _, err := s.store.GetConnection(userID, cid)
			if err != nil || conn == nil {
				log.Printf("[CREATE] skip bound connection id=%d: %v", cid, err)
				continue
			}
			mcpRecord, lookupErr := s.store.FindCanonicalMCPServerByConnection(conn.ID)
			if lookupErr != nil {
				log.Printf("[CREATE] resolve MCP for bound connection id=%d: %v", cid, lookupErr)
				continue
			}
			if mcpRecord == nil {
				toolCount := 0
				if app := s.catalog.Get(conn.AppSlug); app != nil {
					toolCount = len(app.Tools)
				}
				mcpID, createErr := s.store.CreateMCPServerFromConnection(userID, conn, toolCount)
				if createErr != nil {
					log.Printf("[CREATE] create MCP for bound connection id=%d: %v", cid, createErr)
					continue
				}
				mcpRecord, lookupErr = s.store.GetMCPServerByIDUnscoped(mcpID)
				if lookupErr != nil || mcpRecord == nil {
					log.Printf("[CREATE] reload MCP for bound connection id=%d mcp=%d: %v", cid, mcpID, lookupErr)
					continue
				}
			}
			extraServers = append(extraServers, map[string]any{
				"name":      mcpRecord.Name,
				"transport": "http",
				"url":       fmt.Sprintf("http://127.0.0.1:%s/mcp/%d", s.port, mcpRecord.ID),
				// main_access stays implicit (default true); no_spawn
				// not set so worker threads can also attach if they
				// inherit this connection's role.
			})
		}
		if len(extraServers) > 0 {
			var instCfg map[string]any
			if inst.Config != "" {
				json.Unmarshal([]byte(inst.Config), &instCfg)
			}
			if instCfg == nil {
				instCfg = map[string]any{}
			}
			// Append rather than replace so the gateway/channels
			// system entries added later still land on top of these.
			existing, _ := instCfg["mcp_servers"].([]any)
			for _, e := range extraServers {
				existing = append(existing, e)
			}
			instCfg["mcp_servers"] = existing
			if out, err := json.Marshal(instCfg); err == nil {
				inst.Config = string(out)
			}
		}
	}

	// Persist the system-MCP opt-out flags on the instance DB row so
	// Start() picks them up on first (and every subsequent) boot.
	// Start() reads these from inst.Config, not from disk config.json
	// (which core owns and rewrites on every run), so the DB row is the
	// authoritative place for server-side flags.
	// Replacement default (conversations app): new agents get NO
	// auto-injected channels/agent-output MCPs — the conversations app
	// owns the conversation surface, attaching its own MCP through the
	// normal app-binding path. Explicit include_channels:true still
	// opts an agent into the legacy system MCPs; existing agents keep
	// whatever their stored flag says.
	includeChannels := body.IncludeChannels != nil && *body.IncludeChannels
	{
		var instCfg map[string]any
		if inst.Config != "" {
			json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if instCfg == nil {
			instCfg = map[string]any{}
		}
		delete(instCfg, "include_apteva_server")
		instCfg["include_channels"] = includeChannels
		if body.Unconscious != nil {
			instCfg["unconscious"] = *body.Unconscious
		}
		if out, err := json.Marshal(instCfg); err == nil {
			inst.Config = string(out)
		}
	}
	if err := s.syncChannelsCapabilityMemoryDisk(inst.ID, includeChannels); err != nil {
		log.Printf("[CAPABILITY-MEMORY] create sync agent=%d include_channels=%v: %v", inst.ID, includeChannels, err)
	}
	// Persist the fully assembled initial config before any early return from
	// the provider/start gates below.
	if err := s.store.UpdateAgent(inst); err != nil {
		http.Error(w, "persist initial agent config", http.StatusInternalServerError)
		return
	}

	// Start unless explicitly disabled. P1 fix: refuse to start when
	// the user has no LLM provider configured for this project. The
	// agent row is still created (status=stopped) so the operator can
	// fix it up via Settings → Providers and hit Start afterwards
	// without going through the create form again. Without this gate,
	// the core child spawns with config["providers"]=[] and every
	// first-message attempt hangs with a cryptic "no provider" error
	// hidden in core logs.
	shouldStart := body.Start == nil || *body.Start
	if shouldStart {
		providerEnv, err := s.GetAllProviderEnvVars(userID, inst.ProjectID)
		if err != nil {
			writeJSON(w, map[string]any{
				"id":      inst.ID,
				"name":    inst.Name,
				"status":  "stopped",
				"warning": "agent created but not started: " + err.Error(),
			})
			return
		}
		pool := s.GetProviderPool(userID, inst.ProjectID)
		if len(pool) == 0 {
			writeJSON(w, map[string]any{
				"id":     inst.ID,
				"name":   inst.Name,
				"status": "stopped",
				"warning": "agent created but not started: no LLM provider configured. " +
					"Add one in Settings → Providers, then click Start.",
			})
			return
		}
		if _, err := s.startManagedAgent(inst, providerEnv, pool, s.loadChannelConfigs(inst.ID)...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
		s.notifyAgentSubscriptionStartup(inst)
	}

	s.store.UpdateAgent(inst)
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

	// Multi-user listing:
	//   - With a project_id: anyone with viewer+ on that project sees
	//     every agent in it (regardless of which user_id created them).
	//   - Without a project_id: we union every project the caller has
	//     access to (membership + admin short-circuit) and return all
	//     agents across them. This is what the dashboard's "all
	//     projects" view needs.
	var instances []Agent
	var err error
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
		instances, err = s.store.ListAgentsInProject(projectID)
	} else {
		// Walk every visible project and concat their agents.
		visible, lerr := s.store.ListProjectsForUser(userID)
		if lerr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, p := range visible {
			batch, berr := s.store.ListAgentsInProject(p.ID)
			if berr != nil {
				continue
			}
			instances = append(instances, batch...)
		}
		// Plus any legacy agents that have no project_id — surface them
		// for the user that owns them so single-user installs from
		// before projects existed keep working.
		legacy, _ := s.store.ListAgents(userID, "")
		for _, a := range legacy {
			if a.ProjectID == "" {
				instances = append(instances, a)
			}
		}
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Update running status
	for i := range instances {
		s.enrichAgentRuntime(&instances[i])
	}
	if instances == nil {
		instances = []Agent{}
	}
	writeJSON(w, instances)
}

// GET/DELETE /instances/:id
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
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

	// Load the agent regardless of user_id — multi-user members may
	// access agents created by other users in a project they're in.
	// Authz comes from the agent's project membership, not the row's
	// user_id.
	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	// Legacy agents with no project_id stay user-scoped (single-user
	// pre-projects era). Reject anyone else.
	if inst.ProjectID == "" {
		if getUserID(r) != inst.UserID && s.store.GetPlatformRole(getUserID(r)) != PlatformAdmin {
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}
	} else {
		// Mutations require editor+; reads need viewer.
		need := ProjectViewer
		if r.Method != http.MethodGet {
			need = ProjectEditor
		}
		if _, _, ok := s.requireProjectAccess(w, r, inst.ProjectID, need); !ok {
			return
		}
	}
	userID := getUserID(r)
	_ = userID // retained for downstream paths that read it via getUserID(r)

	switch r.Method {
	case http.MethodGet:
		s.enrichAgentRuntime(inst)
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
		if err := s.store.UpdateAgent(inst); err != nil {
			http.Error(w, "failed to update instance", http.StatusInternalServerError)
			return
		}
		writeJSON(w, inst)

	case http.MethodDelete:
		log.Printf("[LIFECYCLE] DELETE %s agent=%d remote=%s ua=%q referer=%q",
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
		s.agents.Stop(inst.ID)

		// 3. Notify apps so each one drops its instance-scoped rows
		// (channelchat: chats + messages). Done AFTER Stop so the apps
		// don't race with a still-running core writing more data into
		// the tables they're about to drop.
		if s.apps != nil && detachInfo != nil {
			s.apps.NotifyInstanceDetach(*detachInfo)
		}

		// 4. Cascade-delete server DB rows
		// (instances + telemetry + channels + subscriptions +
		// app_agent_bindings).
		if err := s.store.DeleteAgent(userID, instanceID); err != nil {
			log.Printf("[LIFECYCLE] DB cascade delete failed agent=%d err=%v", inst.ID, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}

		// 5. Remove on-disk state: config.json, apteva-core.log,
		// history/*.jsonl, workspace/. Done last so the DB row is
		// already gone — if RemoveAll fails we have an orphan dir
		// but the user-visible state matches "deleted". Logged so
		// operators can scrub manually.
		dir := s.agents.instanceDir(inst.ID)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("[LIFECYCLE] dir cleanup failed agent=%d dir=%s err=%v", inst.ID, dir, err)
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
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/stop")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	// Disk config.json is the source of truth — no need to save to DB.
	// Core already writes threads/MCP/directive to disk at runtime.
	s.agents.Stop(inst.ID)
	inst.Status = "stopped"
	inst.Pid = 0
	inst.Port = 0
	inst.CoreAPIKey = ""
	s.store.UpdateAgent(inst)
	writeJSON(w, inst)
}

// POST /instances/:id/start
// ResumeRunningInstances is the boot-time recovery path. When the server
// starts, every user agent in the DB marked `status='running'` was active
// before the previous server process stopped. In detach mode we first try
// to reattach to the old core process using its persisted pid/port/API key.
// If that fails, or detach mode is disabled, we fall back to spawning a
// fresh core. Without this path, a server update would leave the DB saying
// "running" while no in-memory process entry exists for chat/proxy routes.
//
// Any instance that fails to resume (missing provider credentials, port
// already taken, bad config, etc.) is flipped to `stopped` in the DB so
// the dashboard's Start button can try again cleanly.
func (s *Server) ResumeRunningInstances() {
	if s.startupIntent.Reason != "" {
		defer os.Remove(s.lifecycleIntentPath())
	}
	mode := s.agentBootResumeMode()
	if mode == "manual" {
		log.Printf("[RESUME] boot resume disabled by policy — leaving status='running' rows untouched")
		return
	}
	resumePolicy := s.agentUpdatePolicy()
	if s.startupIntent.Reason != "" {
		resumePolicy = s.resolvedShutdownPolicy(s.startupIntent)
	}
	reattachEnabled := resumePolicy != "restart"
	if providerAuthRefreshEnvEnabled() && s.store.RunningAgentsUseCodexProvider() {
		result := s.refreshExpiringCodexProviders(context.Background(), codexProviderRefreshSkew, false)
		if result.ProvidersRefreshed+result.ConnectionsRefreshed > 0 && disableCoreReattachForCodexRefresh() {
			reattachEnabled = false
			log.Printf("[RESUME] OpenAI Codex provider refreshed before resume; forcing fresh core spawn for updated token env")
		}
	}
	rows, err := s.store.ListAgentsByStatus("running")
	if err != nil {
		log.Printf("[RESUME] list running instances: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("[RESUME] found %d instance(s) marked running in DB — reattaching or re-spawning cores", len(rows))

	for i := range rows {
		inst := &rows[i]
		// Platform-owned agents are lazy-managed by their owning paths; skip them
		// here so restart recovery only wakes real user agents.
		if inst.Kind != "" && inst.Kind != "user" {
			log.Printf("[RESUME] instance %d (%s): kind=%s is platform-managed, skipping", inst.ID, inst.Name, inst.Kind)
			inst.Status = "stopped"
			inst.Port = 0
			inst.Pid = 0
			inst.CoreAPIKey = ""
			_ = s.store.UpdateAgent(inst)
			continue
		}
		if s.agents.IsRunning(inst.ID) {
			log.Printf("[RESUME] instance %d (%s): already managed by another boot path, skipping", inst.ID, inst.Name)
			continue
		}
		if reattachEnabled && inst.CoreAPIKey != "" && inst.Port > 0 && inst.Pid > 0 {
			if _, err := s.reattachManagedAgent(inst, s.loadChannelConfigs(inst.ID)...); err == nil {
				s.restoreSlackForInstance(inst)
				s.restoreEmailForInstance(inst)
				log.Printf("[RESUME] instance %d (%s): reattached existing core on port %d pid %d", inst.ID, inst.Name, inst.Port, inst.Pid)
				continue
			} else {
				log.Printf("[RESUME] instance %d (%s): reattach failed: %v — falling back to fresh spawn", inst.ID, inst.Name, err)
				terminateRuntimePID(inst.Pid, inst.Port, inst.CoreAPIKey, 2*time.Second)
			}
		}
		providerEnv, err := s.GetAllProviderEnvVars(inst.UserID, inst.ProjectID)
		if err != nil {
			log.Printf("[RESUME] instance %d (%s): provider env failed: %v — leaving stopped", inst.ID, inst.Name, err)
			inst.Status = "stopped"
			inst.Pid = 0
			inst.Port = 0
			inst.CoreAPIKey = ""
			_ = s.store.UpdateAgent(inst)
			continue
		}
		pool := s.GetProviderPool(inst.UserID, inst.ProjectID)

		if _, err := s.startManagedAgent(
			inst,
			providerEnv,
			pool,
			s.loadChannelConfigs(inst.ID)...,
		); err != nil {
			// "already running" is a benign race with another start
			// path (bootMetaAgents typically) — the process is up,
			// just not via us. Don't flip status to stopped.
			if errors.Is(err, errAgentAlreadyRunning) {
				log.Printf("[RESUME] instance %d (%s): already running via another path, leaving as-is", inst.ID, inst.Name)
				continue
			}
			log.Printf("[RESUME] instance %d (%s): start failed: %v — marking stopped", inst.ID, inst.Name, err)
			inst.Status = "stopped"
			s.store.UpdateAgent(inst)
			continue
		}

		// Start() mutates inst.Port + Pid + Status to the new values;
		// persist them so the UI reflects the fresh process state.
		s.store.UpdateAgent(inst)
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
		s.notifyAgentSubscriptionStartup(inst)
		log.Printf("[RESUME] instance %d (%s): resumed on port %d pid %d", inst.ID, inst.Name, inst.Port, inst.Pid)
		if mode == "staggered" && i < len(rows)-1 {
			time.Sleep(s.agentBootResumeDelay())
		}
	}

	// The old server leaves the short-lived intent for this replacement to
	// consume. In rolling mode every healthy core is reattached first, then
	// outdated cores are replaced serially so the fleet never cold-starts at
	// once. Preserve mode deliberately leaves versions untouched.
	intent := s.startupIntent
	if intent.Reason != "" && s.resolvedShutdownPolicy(intent) == "rolling" && s.agentRollouts != nil {
		if _, err := s.startCoreRollout(nil, "", "all", true, s.agentRolloutDelay()); err != nil && !strings.Contains(err.Error(), "no running agents") {
			log.Printf("[ROLLOUT] automatic post-%s rollout not started: %v", intent.Reason, err)
		}
	}
}

func (s *Server) agentShutdownPolicy() string {
	raw := strings.TrimSpace(os.Getenv("APTEVA_AGENT_SHUTDOWN_POLICY"))
	if raw == "" && s != nil && s.store != nil {
		raw = strings.TrimSpace(s.store.GetSetting("agent_shutdown_policy"))
	}
	switch strings.ToLower(raw) {
	case "detach", "survive", "reattach":
		return "detach"
	case "", "stop", "respawn", "restart", "false", "0", "off", "disabled":
		return "stop"
	default:
		log.Printf("[SHUTDOWN] unknown APTEVA_AGENT_SHUTDOWN_POLICY=%q — using stop", raw)
		return "stop"
	}
}

func (s *Server) agentBootResumeMode() string {
	raw := strings.TrimSpace(os.Getenv("APTEVA_AGENT_BOOT_RESUME"))
	if raw == "" && s != nil && s.store != nil {
		raw = strings.TrimSpace(s.store.GetSetting("agent_boot_resume"))
	}
	switch strings.ToLower(raw) {
	case "":
		return "staggered"
	case "auto", "true", "1", "yes":
		return "auto"
	case "manual", "off", "false", "0", "no", "disabled":
		return "manual"
	case "stagger", "staggered":
		return "staggered"
	default:
		log.Printf("[RESUME] unknown APTEVA_AGENT_BOOT_RESUME=%q — using staggered", raw)
		return "staggered"
	}
}

func (s *Server) agentBootResumeDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv("APTEVA_AGENT_BOOT_RESUME_DELAY"))
	if raw == "" && s != nil && s.store != nil {
		raw = strings.TrimSpace(s.store.GetSetting("agent_boot_resume_delay"))
	}
	if raw == "" {
		return 5 * time.Second
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	log.Printf("[RESUME] invalid boot resume delay %q — using 5s", raw)
	return 5 * time.Second
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

	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	if s.agents.IsRunning(inst.ID) {
		// Starting is idempotent: the requested state has already been
		// reached, including when boot recovery won the race.
		writeJSON(w, inst)
		return
	}

	providerEnv, err := s.GetAllProviderEnvVars(userID, inst.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pool := s.GetProviderPool(userID, inst.ProjectID)
	// P1 fix — refuse to start with no LLM provider; see the same
	// gate in handleCreateInstance for the rationale.
	if len(pool) == 0 {
		http.Error(w,
			"no LLM provider configured — add one in Settings → Providers before starting an agent",
			http.StatusBadRequest)
		return
	}

	if _, err := s.startManagedAgent(inst, providerEnv, pool, s.loadChannelConfigs(inst.ID)...); err != nil {
		if errors.Is(err, errAgentAlreadyRunning) {
			// Auto-resume or another start request may have won between the
			// IsRunning check and AgentManager.Start. Treat that race exactly
			// like the fast path above instead of leaking a spurious 500.
			if current, getErr := s.store.GetAgentByID(inst.ID); getErr == nil {
				inst = current
			}
			writeJSON(w, inst)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.restoreSlackForInstance(inst)
	s.restoreEmailForInstance(inst)
	s.notifyAgentSubscriptionStartup(inst)

	s.store.UpdateAgent(inst)
	writeJSON(w, inst)
}

// POST /instances/:id/restart
func (s *Server) handleRestartInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/restart")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	if _, err := s.store.GetAgentByID(instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	if err := s.updateAgentCore(r.Context(), instanceID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "restarted"})
}

// /instances/:id/config — GET proxies to core, PUT updates DB + proxies full body to core
func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	// Extract instance ID from /instances/:id/config
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/config")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	port := s.agents.GetPort(inst.ID)

	// GET — proxy directly to core (with boot-wait retry)
	if r.Method == http.MethodGet {
		if port == 0 {
			// Agent stopped — serve saved config from disk, same as handleProxy.
			s.serveStoppedInstanceData(w, inst, "/config")
			return
		}
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		coreKey := s.agents.GetCoreAPIKey(inst.ID)
		resp, err := s.coreDoWithBootWait(inst.ID, "GET", targetURL, nil, coreKey)
		if err != nil {
			log.Printf("[PROXY] core unreachable agent=%d path=/config: %v", inst.ID, err)
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

	// Core's PUT /config consumes mcp_servers as one complete desired list.
	// Serialize the read/merge/write here so a directive edit or two
	// simultaneous attachment changes cannot silently disconnect tools.
	unlockConfig := s.lockAgentConfig(instanceID)
	defer unlockConfig()

	// PUT — read body, update DB fields, then proxy FULL body to core
	bodyBytes, _ := io.ReadAll(r.Body)

	var body struct {
		Directive     string           `json:"directive"`
		Mode          string           `json:"mode"`
		Config        string           `json:"config"`
		Providers     []map[string]any `json:"providers"`
		ModelOverride *string          `json:"model_override"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var rawBody map[string]any
	if err := json.Unmarshal(bodyBytes, &rawBody); err != nil || rawBody == nil {
		http.Error(w, "invalid JSON object", http.StatusBadRequest)
		return
	}
	if _, ok := rawBody["computer"]; ok {
		http.Error(w, "core computer config has been removed; use the Computer app instead", http.StatusGone)
		return
	}
	if inst.Kind == "platform_helper" {
		if _, ok := rawBody["mcp_servers"]; ok || rawBody["_mcp_action"] != nil {
			http.Error(w, "platform Helper capabilities must be updated through /platform/helper/capabilities", http.StatusForbidden)
			return
		}
		if body.Config != "" {
			http.Error(w, "platform Helper runtime config is server-managed", http.StatusForbidden)
			return
		}
	}

	// The public additive MCP endpoint is rewritten to these internal fields
	// before entering this handler. Resolve inventory ids and merge them while
	// the same per-agent lock used by every other config update is held.
	action, hasMCPAction := rawBody["_mcp_action"].(string)
	if hasMCPAction {
		rawIDs, ok := rawBody["_mcp_server_ids"].([]any)
		if !ok {
			http.Error(w, "mcp_server_ids must be an array", http.StatusBadRequest)
			return
		}
		serverIDs := make([]int64, 0, len(rawIDs))
		for _, rawID := range rawIDs {
			id, ok := rawID.(float64)
			if !ok || id <= 0 || id != float64(int64(id)) {
				http.Error(w, "mcp_server_ids must contain positive integers", http.StatusBadRequest)
				return
			}
			serverIDs = append(serverIDs, int64(id))
		}
		current, err := s.currentAgentMCPServers(inst, port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		selected, err := s.resolveAgentMCPConfigs(getUserID(r), inst, serverIDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rawBody["mcp_servers"] = mcpMapsAsAny(mutateMCPServers(current, selected, action))
		delete(rawBody, "_mcp_action")
		delete(rawBody, "_mcp_server_ids")
	} else if _, hasMCPServers := rawBody["mcp_servers"]; !hasMCPServers {
		// Dashboard and gateway config writes are patch-shaped (directive,
		// mode, provider selection, reset). Core's config endpoint is not:
		// omitting mcp_servers means "detach all non-system MCPs". Preserve
		// the canonical current set explicitly before forwarding.
		current, err := s.currentAgentMCPServers(inst, port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		rawBody["mcp_servers"] = mcpMapsAsAny(current)
	}
	normalizeAppMCPProjectURLs(rawBody, inst.ProjectID)

	effectiveDefault := ""
	if len(body.Providers) > 0 {
		pool := s.GetProviderPool(inst.UserID, inst.ProjectID)
		configuredDefault := configuredAgentDefaultProvider(inst.Config)
		if body.Config != "" {
			configuredDefault = configuredAgentDefaultProvider(body.Config)
		}
		hydrated, selected, err := hydrateCoreProviderConfigs(pool, configuredDefault, body.Providers)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		modelOverride := configuredAgentModelOverride(inst.Config, selected)
		if body.ModelOverride != nil {
			modelOverride = strings.TrimSpace(*body.ModelOverride)
		}
		applyAgentModelOverride(hydrated, selected, modelOverride)
		rawBody["providers"] = hydrated
		effectiveDefault = selected
	}
	// model_override is server-owned agent metadata. Core receives the
	// resulting per-provider model map, not this persistence envelope.
	delete(rawBody, "model_override")
	if encoded, err := json.Marshal(rawBody); err == nil {
		bodyBytes = encoded
	} else {
		http.Error(w, "encode provider configuration", http.StatusInternalServerError)
		return
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
	// Save the validated effective provider rather than relying on whichever
	// order SQLite or the dashboard happened to return.
	if effectiveDefault != "" {
		var cfg map[string]any
		if strings.TrimSpace(inst.Config) != "" {
			if err := json.Unmarshal([]byte(inst.Config), &cfg); err != nil {
				http.Error(w, "invalid stored agent config", http.StatusInternalServerError)
				return
			}
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		cfg["default_provider"] = effectiveDefault
		if body.ModelOverride != nil {
			model := strings.TrimSpace(*body.ModelOverride)
			if model == "" {
				delete(cfg, "model_override")
			} else {
				cfg["model_override"] = map[string]string{
					"provider": effectiveDefault,
					"model":    model,
				}
			}
		}
		cfgBytes, _ := json.Marshal(cfg)
		inst.Config = string(cfgBytes)
	}
	// Channels-MCP opt-out persistence: when the client sends an
	// mcp_servers list, detect whether the channels entry the server
	// auto-injects at startup (channels / apteva-channels) is present.
	// Absent means "user detached" — we remember that in the instance DB
	// record so the next start respects the choice. Present means "user
	// wants it", which clears any stale opt-out.
	if mcpList, ok := rawBody["mcp_servers"].([]any); ok {
		var instCfg map[string]any
		if inst.Config != "" {
			json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if instCfg == nil {
			instCfg = map[string]any{}
		}
		hasChannels := false
		for _, s := range mcpList {
			if sm, ok := s.(map[string]any); ok {
				n, _ := sm["name"].(string)
				if isServerOwnedOutputMCP(n) {
					hasChannels = true
				}
			}
		}
		instCfg["include_channels"] = hasChannels
		if out, err := json.Marshal(instCfg); err == nil {
			inst.Config = string(out)
		}
	}

	if err := s.store.UpdateAgent(inst); err != nil {
		http.Error(w, "persist agent config", http.StatusInternalServerError)
		return
	}

	// Forward the FULL body to core (includes mcp_servers, providers, etc.)
	if port > 0 {
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		coreKey := s.agents.GetCoreAPIKey(inst.ID)
		resp, err := s.coreDoWithBootWait(inst.ID, "PUT", targetURL, bodyBytes, coreKey, http.Header{"Content-Type": []string{"application/json"}})
		if err != nil {
			log.Printf("[CONFIG] PUT forward to core failed agent=%d: %v", inst.ID, err)
			http.Error(w, fmt.Sprintf("core unreachable: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := s.syncAppBindingsFromMCPServers(inst.ID, inst.ProjectID, rawBody["mcp_servers"]); err != nil {
				log.Printf("[CONFIG] sync app bindings failed agent=%d: %v", inst.ID, err)
				http.Error(w, "core config updated but app attachment metadata could not be synchronized", http.StatusInternalServerError)
				return
			}
			if body.Directive != "" {
				s.refreshChannelChatConversationDirectives(inst.ID)
			}
		}
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
	// core.Config (directive, mode, mcp_servers, providers,
	// threads, unconscious) and the `reset` sub-object.
	err = s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
		delete(cfg, "computer")
		if body.Directive != "" {
			cfg["directive"] = body.Directive
		}
		if body.Mode == "autonomous" || body.Mode == "cautious" || body.Mode == "learn" {
			cfg["mode"] = body.Mode
		}
		// rawBody was decoded above; re-use it for the surface-level
		// fields the client may set. If a key is absent in the request
		// we keep whatever disk already held. main_pace is accepted only
		// on this stopped-agent path so test/bootstrap callers can preload
		// an exact first wake; a running Core remains the sole pace owner.
		for _, k := range []string{"mcp_servers", "providers", "threads", "unconscious", "execution_control", "realtime_enabled", "realtime_voice", "realtime_voice_mcp", "main_pace"} {
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
		log.Printf("[CONFIG] PUT stopped-write failed agent=%d: %v", inst.ID, err)
		http.Error(w, fmt.Sprintf("persist config: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.syncAppBindingsFromMCPServers(inst.ID, inst.ProjectID, rawBody["mcp_servers"]); err != nil {
		log.Printf("[CONFIG] sync stopped app bindings failed agent=%d: %v", inst.ID, err)
		http.Error(w, "config saved but app attachment metadata could not be synchronized", http.StatusInternalServerError)
		return
	}
	if body.Directive != "" {
		s.refreshChannelChatConversationDirectives(inst.ID)
	}
	log.Printf("[CONFIG] PUT stopped agent=%d — persisted to config.json (applies on next start)", inst.ID)
	writeJSON(w, inst)
}

func normalizeAppMCPProjectURLs(rawBody map[string]any, projectID string) bool {
	if projectID == "" || rawBody == nil {
		return false
	}
	mcpList, ok := rawBody["mcp_servers"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, entry := range mcpList {
		sm, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		rawURL, _ := sm["url"].(string)
		if rawURL == "" || !strings.Contains(rawURL, "/api/apps/") || !strings.Contains(rawURL, "/mcp") || strings.Contains(rawURL, "project_id=") {
			continue
		}
		sm["url"] = addQueryParam(rawURL, "project_id", projectID)
		changed = true
	}
	return changed
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
		if s.agents.GetPort(instanceID) == 0 {
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

	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	port := s.agents.GetPort(inst.ID)
	corePath := "/" + parts[1]

	// Agent stopped — serve static data from saved config for read-only endpoints,
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

	coreKey := s.agents.GetCoreAPIKey(inst.ID)
	resp, err := s.coreDoWithBootWait(inst.ID, r.Method, targetURL, bodyBytes, coreKey, r.Header)
	if err != nil {
		if err == errInstanceNotRunning {
			http.Error(w, "instance not running", http.StatusServiceUnavailable)
			return
		}
		log.Printf("[PROXY] core unreachable agent=%d port=%d path=%s: %v", inst.ID, port, corePath, err)
		http.Error(w, fmt.Sprintf("core unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
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
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			http.Error(w, "failed to read core response", http.StatusBadGateway)
			return
		}
		rewritten := false
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && r.Method == http.MethodGet {
			switch corePath {
			case "/status":
				body, rewritten = s.enrichAgentStatusBody(inst.ID, body, time.Now())
			case "/threads":
				body, rewritten = s.enrichAgentThreadsBody(inst.ID, body, time.Now())
			}
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		if rewritten {
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
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
func (s *Server) serveStoppedInstanceData(w http.ResponseWriter, inst *Agent, path string) {
	// Load config: try disk first, fall back to DB
	var config map[string]any
	dir := s.agents.instanceDir(inst.ID)
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
			"id":          "main",
			"directive":   inst.Directive,
			"depth":       0,
			"iteration":   0,
			"rate":        "stopped",
			"model":       "",
			"age":         "",
			"sleep_state": "stopped",
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
						"id":          t["id"],
						"parent_id":   t["parent_id"],
						"depth":       depth,
						"directive":   t["directive"],
						"tools":       t["tools"],
						"mcp_names":   t["mcp_names"],
						"realtime":    t["realtime"],
						"voice":       t["voice"],
						"provider":    t["provider"],
						"iteration":   0,
						"rate":        "stopped",
						"model":       "",
						"age":         "",
						"sleep_state": "stopped",
					})
				}
			}
		}
		writeJSON(w, threads)

	case "/status":
		executionControl := config["execution_control"]
		if executionControl == nil {
			executionControl = map[string]any{"mode": "auto", "scope": "instance", "follow": "active", "waiting": false}
		}
		writeJSON(w, map[string]any{
			"iteration":          0,
			"rate":               "stopped",
			"model":              "",
			"paused":             false,
			"threads":            0,
			"memories":           0,
			"uptime_seconds":     0,
			"mode":               inst.Mode,
			"execution_control":  executionControl,
			"sleep_state":        "stopped",
			"sleep_remaining_ms": 0,
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
		// Provider credentials and catalogs can change while an agent is
		// stopped. Rebuild the same effective provider surface Start will
		// inject so the dashboard never guesses from a differently ordered
		// provider list or displays stale disk defaults.
		if pool := s.GetProviderPool(inst.UserID, inst.ProjectID); len(pool) > 0 {
			config["providers"] = buildAgentCoreProviderConfigs(pool, inst.Config)
		}
		// Return the full persisted config surface — mcp_servers,
		// providers, threads — so the stopped-instance UI
		// renders the real state rather than a placeholder. Before
		// this fix we hard-coded mcp_servers:[] even though the disk
		// config had them; the MCP pane showed empty for every
		// stopped agent.
		out := map[string]any{
			"directive":          directive,
			"mode":               mode,
			"mcp_servers":        config["mcp_servers"],
			"providers":          config["providers"],
			"threads":            config["threads"],
			"unconscious":        config["unconscious"],
			"execution_control":  config["execution_control"],
			"realtime_enabled":   config["realtime_enabled"],
			"realtime_voice":     config["realtime_voice"],
			"realtime_voice_mcp": config["realtime_voice_mcp"],
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
func (s *Server) handleStoppedMutation(w http.ResponseWriter, r *http.Request, inst *Agent, corePath string) bool {
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
		log.Printf("[THREADS] stopped agent=%d dropped persisted thread %q", inst.ID, tid)
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
		log.Printf("[THREADS] stopped agent=%d updated persisted thread %q", inst.ID, tid)
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
	dir := s.agents.instanceDir(instanceID)
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
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
	instanceID, _ := atoi64(parts[0])
	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	ic := s.agents.GetChannels(inst.ID)
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
// Body: {"name": "channels", "enable": true|false}
//
// Flips the include_channels flag
// on inst.Config. These flags are only consulted at Start() time
// (agents.go:288-299), so toggling them on a running instance does
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
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/system-mcp")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetAgentByID(instanceID)
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
	case "channels", "apteva-channels":
		flag = "include_channels"
	default:
		http.Error(w, fmt.Sprintf("unknown system MCP %q (expected channels)", body.Name), http.StatusBadRequest)
		return
	}

	var instCfg map[string]any
	if inst.Config != "" {
		json.Unmarshal([]byte(inst.Config), &instCfg)
	}
	if instCfg == nil {
		instCfg = map[string]any{}
	}
	previous, ok := instCfg[flag].(bool)
	if !ok && flag == "include_channels" {
		previous = true
	}
	instCfg[flag] = *body.Enable
	if out, merr := json.Marshal(instCfg); merr == nil {
		inst.Config = string(out)
	}
	if err := s.store.UpdateAgent(inst); err != nil {
		http.Error(w, "failed to persist", http.StatusInternalServerError)
		return
	}
	if err := s.syncChannelsCapabilityMemory(inst.ID, *body.Enable); err != nil {
		log.Printf("[CAPABILITY-MEMORY] toggle sync agent=%d include_channels=%v: %v", inst.ID, *body.Enable, err)
	}

	running := s.agents.GetPort(inst.ID) > 0
	writeJSON(w, map[string]any{
		"name":             body.Name,
		"enable":           *body.Enable,
		"previous":         previous,
		"restart_required": running && previous != *body.Enable,
	})
}

// GET/PUT /instances/:id/background-memory
//
// GET returns the effective persisted setting. PUT accepts
// {"enabled":bool,"restart":bool}. A running agent must be restarted for
// the core to spawn or tear down its system-owned unconscious thread; callers
// that omit restart receive 409 and no state is changed. Disabling removes the
// persisted unconscious thread definition but deliberately preserves
// memory.jsonl and thread history.
func (s *Server) handleBackgroundMemory(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	idStr := strings.TrimSuffix(path, "/background-memory")
	instanceID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetAgentByID(instanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	previous := s.backgroundMemoryEnabled(inst)
	running := s.agents.IsRunning(inst.ID)

	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{
			"enabled":          previous,
			"running":          running,
			"restart_required": false,
		})
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
		Restart bool  `json:"restart"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Enabled == nil {
		http.Error(w, "enabled (bool) required", http.StatusBadRequest)
		return
	}
	changed := previous != *body.Enabled
	if running && changed && !body.Restart {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":          previous,
			"running":          true,
			"restart_required": true,
			"error":            "restart required to change background memory on a running agent",
		})
		return
	}

	restarted := false
	if running && changed {
		s.agents.Stop(inst.ID)
		inst.Status = "stopped"
		inst.Pid = 0
		inst.Port = 0
		inst.CoreAPIKey = ""
		_ = s.store.UpdateAgent(inst)
	}

	if changed {
		if err := s.writeStoppedConfigAtomic(inst.ID, func(cfg map[string]any) error {
			setBackgroundMemoryConfig(cfg, *body.Enabled)
			return nil
		}); err != nil {
			http.Error(w, "persist background memory: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var instCfg map[string]any
		if inst.Config != "" {
			_ = json.Unmarshal([]byte(inst.Config), &instCfg)
		}
		if instCfg == nil {
			instCfg = map[string]any{}
		}
		setBackgroundMemoryConfig(instCfg, *body.Enabled)
		if out, err := json.Marshal(instCfg); err == nil {
			inst.Config = string(out)
		}
		if err := s.store.UpdateAgent(inst); err != nil {
			http.Error(w, "persist agent background memory: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if running && changed {
		providerEnv, err := s.GetAllProviderEnvVars(inst.UserID, inst.ProjectID)
		if err != nil {
			http.Error(w, "provider environment: "+err.Error(), http.StatusBadGateway)
			return
		}
		pool := s.GetProviderPool(inst.UserID, inst.ProjectID)
		if len(pool) == 0 {
			http.Error(w, "no LLM provider configured", http.StatusBadRequest)
			return
		}
		if _, err := s.startManagedAgent(inst, providerEnv, pool, s.loadChannelConfigs(inst.ID)...); err != nil {
			http.Error(w, "restart agent: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.store.UpdateAgent(inst)
		s.restoreSlackForInstance(inst)
		s.restoreEmailForInstance(inst)
		s.notifyAgentSubscriptionStartup(inst)
		restarted = true
	}

	writeJSON(w, map[string]any{
		"enabled":          *body.Enabled,
		"previous":         previous,
		"running":          s.agents.IsRunning(inst.ID),
		"restarted":        restarted,
		"restart_required": false,
		"memory_preserved": true,
	})
}

func (s *Server) backgroundMemoryEnabled(inst *Agent) bool {
	if inst == nil {
		return false
	}
	path := filepath.Join(s.agents.instanceDir(inst.ID), "config.json")
	if data, err := os.ReadFile(path); err == nil {
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) == nil {
			if enabled, ok := cfg["unconscious"].(bool); ok {
				return enabled
			}
		}
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(inst.Config), &cfg) == nil {
		enabled, _ := cfg["unconscious"].(bool)
		return enabled
	}
	return false
}

func setBackgroundMemoryConfig(cfg map[string]any, enabled bool) {
	cfg["unconscious"] = enabled
	if enabled {
		return
	}
	threads, ok := cfg["threads"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(threads))
	for _, raw := range threads {
		thread, _ := raw.(map[string]any)
		id, _ := thread["id"].(string)
		if id == "unconscious" {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		delete(cfg, "threads")
	} else {
		cfg["threads"] = kept
	}
}
