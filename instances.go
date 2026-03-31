package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type runningInstance struct {
	cmd  *exec.Cmd
	port int
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

// Start launches a core process for the given instance.
// providerEnv contains decrypted provider env vars to inject.
// serverPort is this server's port so core can POST telemetry back.
func (im *InstanceManager) Start(inst *Instance, providerEnv map[string]string, serverPort string) error {
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
		"name":    "apteva-server",
		"command": serverBin,
		"args":    []string{"--mcp-gateway", fmt.Sprintf("--user-id=%d", inst.UserID)},
	}

	// Start from saved config (preserves MCP connections, threads added at runtime)
	// Try DB first, fall back to config.json on disk (handles crash case)
	config := map[string]any{}
	if inst.Config != "" && inst.Config != "{}" {
		json.Unmarshal([]byte(inst.Config), &config)
	} else if diskConfig, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		json.Unmarshal(diskConfig, &config)
	}

	// Always update directive and mode from DB (user may have changed them)
	config["directive"] = inst.Directive
	config["mode"] = mode

	// Ensure apteva-server gateway is present (update command path in case binary moved)
	var servers []any
	if existing, ok := config["mcp_servers"].([]any); ok {
		for _, s := range existing {
			if sm, ok := s.(map[string]any); ok {
				if sm["name"] == "apteva-server" {
					continue // remove old gateway entry, we'll re-add below
				}
				servers = append(servers, sm)
			}
		}
	}
	servers = append([]any{gateway}, servers...) // gateway first
	config["mcp_servers"] = servers

	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(dir, "config.json"), configData, 0644)

	cmd := exec.Command(im.coreCmd, "--headless")
	cmd.Dir = dir
	env := append(os.Environ(),
		"API_PORT="+itoa64(int64(port)),
		"NO_TUI=1",
		"SERVER_URL=http://127.0.0.1:"+serverPort,
		"INSTANCE_ID="+itoa64(inst.ID),
		"PROJECT_ID="+inst.ProjectID,
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

	im.processes[inst.ID] = &runningInstance{cmd: cmd, port: port}
	inst.Port = port
	inst.Pid = cmd.Process.Pid
	inst.Status = "running"

	// Wait for process exit in background
	go func() {
		cmd.Wait()
		im.mu.Lock()
		delete(im.processes, inst.ID)
		im.mu.Unlock()
	}()

	return nil
}

// Stop kills a running core process.
func (im *InstanceManager) Stop(instanceID int64) {
	im.mu.Lock()
	ri, ok := im.processes[instanceID]
	if ok {
		delete(im.processes, instanceID)
	}
	im.mu.Unlock()
	if ok && ri.cmd.Process != nil {
		ri.cmd.Process.Kill()
		ri.cmd.Wait()
	}
}

// IsRunning checks if an instance process is alive.
func (im *InstanceManager) IsRunning(instanceID int64) bool {
	im.mu.RLock()
	ri, ok := im.processes[instanceID]
	im.mu.RUnlock()
	return ok && ri.cmd.ProcessState == nil
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
		if err := s.instances.Start(inst, providerEnv, s.port); err != nil {
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

	// Save config.json to DB before stopping (preserves MCP connections, threads, etc.)
	dir := s.instances.instanceDir(inst.ID)
	if configData, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		inst.Config = string(configData)
	}

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

	if err := s.instances.Start(inst, providerEnv, s.port); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.store.UpdateInstance(inst)
	writeJSON(w, inst)
}

// PUT /instances/:id/config
func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
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

	var body struct {
		Directive string `json:"directive"`
		Mode      string `json:"mode"`
		Config    string `json:"config"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if body.Directive != "" {
		inst.Directive = body.Directive
	}
	if body.Mode == "autonomous" || body.Mode == "supervised" {
		inst.Mode = body.Mode
	}
	if body.Config != "" {
		inst.Config = body.Config
	}
	s.store.UpdateInstance(inst)

	// If running, forward changes to core's API
	if cfgPort := s.instances.GetPort(inst.ID); cfgPort > 0 {
		update := map[string]string{}
		if body.Directive != "" {
			update["directive"] = body.Directive
		}
		if body.Mode == "autonomous" || body.Mode == "supervised" {
			update["mode"] = body.Mode
		}
		if len(update) > 0 {
			proxyPUT(cfgPort, "/config", update)
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
	if port == 0 {
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}

	corePath := "/" + parts[1]
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, corePath)

	// Forward the request
	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	proxyReq.Header = r.Header

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "core unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func proxyPUT(port int, path string, body any) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}
