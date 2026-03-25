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

type InstanceManager struct {
	mu        sync.RWMutex
	processes map[int64]*exec.Cmd // instanceID → running process
	basePort  int
	nextPort  int
	dataDir   string
	cogitoCmd string // path to cogito binary
}

func NewInstanceManager(dataDir, cogitoCmd string, basePort int) *InstanceManager {
	os.MkdirAll(dataDir, 0755)
	return &InstanceManager{
		processes: make(map[int64]*exec.Cmd),
		basePort:  basePort,
		nextPort:  basePort,
		dataDir:   dataDir,
		cogitoCmd: cogitoCmd,
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

// Start launches a Cogito process for the given instance.
func (im *InstanceManager) Start(inst *Instance, fireworksKey string) error {
	im.mu.Lock()
	defer im.mu.Unlock()

	if _, running := im.processes[inst.ID]; running {
		return fmt.Errorf("instance %d already running", inst.ID)
	}

	port := im.allocPort()
	dir := im.instanceDir(inst.ID)

	// Write config.json for this instance
	config := map[string]any{
		"directive": inst.Directive,
	}
	if inst.Config != "" && inst.Config != "{}" {
		json.Unmarshal([]byte(inst.Config), &config)
		config["directive"] = inst.Directive
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(dir, "config.json"), configData, 0644)

	cmd := exec.Command(im.cogitoCmd, "--headless")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"FIREWORKS_API_KEY="+fireworksKey,
		"API_PORT="+itoa64(int64(port)),
		"NO_TUI=1",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start cogito: %w", err)
	}

	im.processes[inst.ID] = cmd
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

// Stop kills a running Cogito process.
func (im *InstanceManager) Stop(instanceID int64) {
	im.mu.Lock()
	cmd, ok := im.processes[instanceID]
	im.mu.Unlock()
	if ok && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// IsRunning checks if an instance process is alive.
func (im *InstanceManager) IsRunning(instanceID int64) bool {
	im.mu.RLock()
	_, ok := im.processes[instanceID]
	im.mu.RUnlock()
	return ok
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
		Config     string `json:"config"` // optional JSON blob for MCP servers etc
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
	if body.Config == "" {
		body.Config = "{}"
	}

	inst, err := s.store.CreateInstance(userID, body.Name, body.Directive, body.Config)
	if err != nil {
		http.Error(w, "failed to create instance", http.StatusInternalServerError)
		return
	}

	// Start the Cogito process
	if err := s.instances.Start(inst, s.fireworksKey); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	instances, err := s.store.ListInstances(userID)
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
		Config    string `json:"config"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if body.Directive != "" {
		inst.Directive = body.Directive
	}
	if body.Config != "" {
		inst.Config = body.Config
	}
	s.store.UpdateInstance(inst)

	// If running, update Cogito's config via its API
	if s.instances.IsRunning(inst.ID) && body.Directive != "" {
		proxyPUT(inst.Port, "/config", map[string]string{"directive": body.Directive})
	}

	writeJSON(w, inst)
}

// Proxy handler: forwards to Cogito instance's API
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	// Parse /instances/:id/<cogito-path>
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

	if !s.instances.IsRunning(inst.ID) {
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}

	cogitoPath := "/" + parts[1]
	targetURL := fmt.Sprintf("http://127.0.0.1:%d%s", inst.Port, cogitoPath)

	// Forward the request
	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	proxyReq.Header = r.Header

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "cogito unreachable", http.StatusBadGateway)
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
