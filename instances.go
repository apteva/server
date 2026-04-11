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
	"time"
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
	basePort  int
	nextPort  int
	dataDir   string
	coreCmd string // path to core binary
}

func NewInstanceManager(dataDir, coreCmd string, basePort int) *InstanceManager {
	os.MkdirAll(dataDir, 0755)
	return &InstanceManager{
		processes: make(map[int64]*runningInstance),
		basePort:  basePort,
		nextPort:  basePort,
		dataDir:   dataDir,
		coreCmd: coreCmd,
	}
}

func (im *InstanceManager) allocPort() int {
	im.nextPort++
	return im.nextPort
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

func (im *InstanceManager) Start(inst *Instance, providerEnv map[string]string, serverPort string, providerPool []ProviderInfo, instanceSecret string, browserConfig map[string]any, channelConfigs ...ChannelConfig) error {
	im.mu.Lock()
	defer im.mu.Unlock()

	if ri, running := im.processes[inst.ID]; running && ri.cmd.ProcessState == nil {
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
					continue // will be re-added with fresh URLs
				}
				userServers = append(userServers, sm)
			}
		}
	}
	config["mcp_servers"] = append([]any{gateway, channelsEntry}, userServers...)

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

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start core: %w", err)
	}

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
	go func() {
		cmd.Wait()
		im.mu.Lock()
		ri := im.processes[inst.ID]
		if ri != nil && ri.channels != nil {
			ri.channels.Stop()
		}
		delete(im.processes, inst.ID)
		im.mu.Unlock()
	}()

	return nil
}

// Stop kills a running core process and cleans up channels.
func (im *InstanceManager) Stop(instanceID int64) {
	im.mu.Lock()
	ri, ok := im.processes[instanceID]
	if ok {
		delete(im.processes, instanceID)
	}
	im.mu.Unlock()
	if ok {
		if ri.channels != nil {
			ri.channels.Stop()
		}
		if ri.cmd.Process != nil {
			ri.cmd.Process.Kill()
			ri.cmd.Wait()
		}
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

// getBrowserConfig returns the browser/computer config from providers if one exists.
// Supports "browser" (local Chrome or existing CDP) and "browserbase" (cloud) provider types.
func (s *Server) getBrowserConfig(userID int64) map[string]any {
	providers, err := s.store.ListProviders(userID)
	if err != nil {
		return nil
	}
	for _, p := range providers {
		if p.Type != "browserbase" && p.Type != "browser" {
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

		// Parse optional resolution from provider data (default 1024x768)
		width, height := 1024, 768
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
			return cfg
		}

		// Browserbase
		apiKey := data["BROWSERBASE_API_KEY"]
		projectID := data["BROWSERBASE_PROJECT_ID"]
		if apiKey == "" {
			continue
		}
		return map[string]any{
			"type":       "browserbase",
			"api_key":    apiKey,
			"project_id": projectID,
			"width":      width,
			"height":     height,
		}
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
		Mode       string `json:"mode"`   // "autonomous" or "supervised"
		Config     string `json:"config"` // optional JSON blob for MCP servers etc
		ProjectID  string `json:"project_id"`
		Start      *bool  `json:"start,omitempty"` // default true; set false to create without starting
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
	if body.Mode != "supervised" {
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

	// Start unless explicitly disabled
	shouldStart := body.Start == nil || *body.Start
	if shouldStart {
		providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret)
		if err != nil {
			providerEnv = map[string]string{}
		}
		pool := s.GetProviderPool(userID)
		if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID), s.loadChannelConfigs(inst.ID)...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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

	case http.MethodDelete:
		s.instances.Stop(inst.ID)
		s.store.DeleteInstance(userID, instanceID)
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
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

	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret)
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID)

	if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID), s.loadChannelConfigs(inst.ID)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret)
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID)

	if err := s.instances.Start(inst, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID), s.loadChannelConfigs(inst.ID)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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

	// GET — proxy directly to core
	if r.Method == http.MethodGet {
		if port == 0 {
			http.Error(w, "instance not running", http.StatusServiceUnavailable)
			return
		}
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		proxyReq, _ := http.NewRequest("GET", targetURL, nil)
		if coreKey := s.instances.GetCoreAPIKey(inst.ID); coreKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+coreKey)
		}
		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil {
			http.Error(w, "core unreachable", http.StatusBadGateway)
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

	if body.Directive != "" {
		inst.Directive = body.Directive
	}
	if body.Mode == "autonomous" || body.Mode == "supervised" || body.Mode == "cautious" || body.Mode == "learn" {
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
	s.store.UpdateInstance(inst)

	// Forward the FULL body to core (includes mcp_servers, computer, etc.)
	if port > 0 {
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		proxyReq, _ := http.NewRequest("PUT", targetURL, strings.NewReader(string(bodyBytes)))
		proxyReq.Header.Set("Content-Type", "application/json")
		if coreKey := s.instances.GetCoreAPIKey(inst.ID); coreKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+coreKey)
		}
		resp, err := http.DefaultClient.Do(proxyReq)
		if err == nil {
			defer resp.Body.Close()
			// Return core's response
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}
	}

	writeJSON(w, inst)
}

// Proxy handler: forwards to core instance's API
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

	// Instance stopped — serve static data from saved config for read-only endpoints
	if port == 0 {
		if r.Method == http.MethodGet && (corePath == "/threads" || corePath == "/status" || corePath == "/config") {
			s.serveStoppedInstanceData(w, inst, corePath)
			return
		}
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, corePath)

	// Forward the request with core's API key
	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	proxyReq.Header = r.Header.Clone()
	if coreKey := s.instances.GetCoreAPIKey(inst.ID); coreKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+coreKey)
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "core unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	// For SSE streams, flush after each line for real-time delivery
	flusher, canFlush := w.(http.Flusher)
	if canFlush && resp.Header.Get("Content-Type") == "text/event-stream" {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				w.Write(line)
				// Flush after each complete SSE frame (empty line = end of frame)
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

// POST /instances/:id/channels/cli/reply — CLI sends answer to a pending ask
func (s *Server) handleCLIReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
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
	ic := s.instances.GetChannels(inst.ID)
	if ic == nil || ic.cli == nil {
		http.Error(w, "instance not running or no CLI channel", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := ic.cli.SubmitReply(body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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
		writeJSON(w, map[string]any{
			"directive":   directive,
			"mode":        mode,
			"mcp_servers": []any{},
		})
	}
}

func (s *Server) handleTelegramConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.SplitN(path, "/", 2)
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
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	botName, err := s.instances.StartTelegram(inst.ID, body.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist channel to DB (encrypted token)
	configJSON, _ := json.Marshal(map[string]string{"bot_token": body.Token, "bot_name": botName})
	encrypted, _ := Encrypt(s.secret, string(configJSON))
	// Remove existing telegram channel for this instance, then create new
	if existing, _ := s.store.ListChannels(inst.ID); existing != nil {
		for _, ch := range existing {
			if ch.Type == "telegram" {
				s.store.DeleteChannel(ch.ID)
			}
		}
	}
	s.store.CreateChannel(userID, inst.ID, "telegram", "@"+botName, encrypted)

	// Notify core that telegram is connected
	port := s.instances.GetPort(inst.ID)
	coreKey := s.instances.GetCoreAPIKey(inst.ID)
	if port > 0 {
		event := fmt.Sprintf("[telegram] gateway connected. Bot @%s online.", botName)
		eventBody, _ := json.Marshal(map[string]any{"message": event, "thread_id": "main"})
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", port), strings.NewReader(string(eventBody)))
		req.Header.Set("Content-Type", "application/json")
		if coreKey != "" {
			req.Header.Set("Authorization", "Bearer "+coreKey)
		}
		http.DefaultClient.Do(req)
	}

	writeJSON(w, map[string]string{"status": "connected", "bot_name": botName})
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
	channels = append(channels, map[string]string{"id": "cli", "status": "connected"})
	if ic != nil && ic.telegram != nil {
		channels = append(channels, map[string]string{
			"id":       "telegram",
			"status":   "connected",
			"bot_name": ic.telegram.BotName(),
		})
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
