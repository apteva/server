package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/apteva/server/internal/managedmcp"
)

const (
	managedMCPSource      = "managed"
	managedMCPCommand     = "managed"
	managedMCPLogMaxBytes = int64(2 << 20)
)

var managedMCPAliasRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var managedMCPEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// managedMCPConfig is encrypted into the existing mcp_servers.encrypted_env
// column. Keeping bindings next to secrets lets managed MCP servers reuse the
// current registry without adding a second source of truth or another table.
type managedMCPConfig struct {
	Env      map[string]string  `json:"env,omitempty"`
	Bindings managedMCPBindings `json:"bindings,omitempty"`
}

type managedMCPBindings struct {
	Integrations map[string]int64 `json:"integrations,omitempty"`
	Apps         map[string]int64 `json:"apps,omitempty"`
}

type managedMCPCreateRequest struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	ProjectID   string                `json:"project_id"`
	Definition  managedmcp.Definition `json:"definition"`
	Env         map[string]string     `json:"env"`
	Bindings    managedMCPBindings    `json:"bindings"`
	Start       *bool                 `json:"start,omitempty"`
}

type managedMCPUpdateRequest struct {
	Description *string                `json:"description,omitempty"`
	Definition  *managedmcp.Definition `json:"definition,omitempty"`
	Env         map[string]string      `json:"env,omitempty"`
	DeleteEnv   []string               `json:"delete_env,omitempty"`
	Bindings    *managedMCPBindings    `json:"bindings,omitempty"`
}

type managedMCPResponse struct {
	Server     *MCPServerRecord      `json:"server"`
	Definition managedmcp.Definition `json:"definition"`
	Bindings   managedMCPBindings    `json:"bindings"`
	EnvKeys    []string              `json:"env_keys"`
	Warning    string                `json:"warning,omitempty"`
}

func normalizeManagedMCPConfig(cfg managedMCPConfig) managedMCPConfig {
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	if cfg.Bindings.Integrations == nil {
		cfg.Bindings.Integrations = map[string]int64{}
	}
	if cfg.Bindings.Apps == nil {
		cfg.Bindings.Apps = map[string]int64{}
	}
	return cfg
}

func (s *Server) encryptManagedMCPConfig(cfg managedMCPConfig) (string, error) {
	cfg = normalizeManagedMCPConfig(cfg)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return Encrypt(s.secret, string(raw))
}

func (s *Server) decryptManagedMCPConfig(encrypted string) (managedMCPConfig, error) {
	cfg := normalizeManagedMCPConfig(managedMCPConfig{})
	if encrypted == "" {
		return cfg, nil
	}
	plain, err := Decrypt(s.secret, encrypted)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(plain), &cfg); err != nil {
		return cfg, fmt.Errorf("decode managed MCP config: %w", err)
	}
	return normalizeManagedMCPConfig(cfg), nil
}

func (s *Server) managedMCPWorkspace(serverID int64) string {
	return filepath.Join(s.dataDir, "mcp-servers", strconv.FormatInt(serverID, 10))
}

func (s *Server) managedMCPSourceDir(serverID int64) string {
	return filepath.Join(s.managedMCPWorkspace(serverID), "source")
}

func (s *Server) managedMCPLogPath(serverID int64) string {
	return filepath.Join(s.managedMCPWorkspace(serverID), "stderr.log")
}

func managedMCPRevision(def managedmcp.Definition, cfg managedMCPConfig) (string, error) {
	payload := struct {
		Definition managedmcp.Definition `json:"definition"`
		Config     managedMCPConfig      `json:"config"`
	}{
		Definition: managedmcp.Normalize(def),
		Config:     normalizeManagedMCPConfig(cfg),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func writeManagedMCPSource(sourceDir string, def managedmcp.Definition) error {
	return managedmcp.Write(sourceDir, managedmcp.Normalize(def))
}

// replaceManagedMCPSource swaps a validated staged source tree into place and
// retains exactly one previous revision for automatic rollback.
func replaceManagedMCPSource(workspace string, def managedmcp.Definition) error {
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(workspace, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := writeManagedMCPSource(stage, def); err != nil {
		return err
	}
	source := filepath.Join(workspace, "source")
	previous := filepath.Join(workspace, "previous")
	_ = os.RemoveAll(previous)
	if _, err := os.Stat(source); err == nil {
		if err := os.Rename(source, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, source); err != nil {
		if _, statErr := os.Stat(previous); statErr == nil {
			_ = os.Rename(previous, source)
		}
		return err
	}
	return nil
}

func rollbackManagedMCPSource(workspace string) error {
	source := filepath.Join(workspace, "source")
	previous := filepath.Join(workspace, "previous")
	if _, err := os.Stat(previous); err != nil {
		return err
	}
	failed := filepath.Join(workspace, ".failed")
	_ = os.RemoveAll(failed)
	if err := os.Rename(source, failed); err != nil {
		return err
	}
	if err := os.Rename(previous, source); err != nil {
		_ = os.Rename(failed, source)
		return err
	}
	_ = os.RemoveAll(failed)
	return nil
}

func (s *Store) UpdateManagedMCPServer(userID, serverID int64, description, encryptedConfig, revision string, toolCount int) error {
	result, err := s.db.Exec(
		`UPDATE mcp_servers
		    SET description = ?, encrypted_env = ?, upstream_id = ?, tool_count = ?
		  WHERE id = ? AND user_id = ? AND source = ?`,
		description, encryptedConfig, revision, toolCount, serverID, userID, managedMCPSource,
	)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("managed MCP server not found")
	}
	return nil
}

func (s *Store) ListManagedMCPsByStatus(status string) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT id FROM mcp_servers WHERE source = ? AND status = ? ORDER BY id`,
		managedMCPSource, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ListManagedMCPsInProject(projectID string) ([]MCPServerRecord, error) {
	rows, err := s.db.Query(
		`SELECT id FROM mcp_servers WHERE source = ? AND project_id = ? ORDER BY id`,
		managedMCPSource, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServerRecord
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		record, err := s.GetMCPServerByIDUnscoped(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func (s *Server) requireManagedMCPAccess(w http.ResponseWriter, r *http.Request, serverID int64, need ProjectRole) (*MCPServerRecord, string, bool) {
	record, err := s.store.GetMCPServerByIDUnscoped(serverID)
	if err != nil || record == nil || record.Source != managedMCPSource {
		http.Error(w, "managed MCP server not found", http.StatusNotFound)
		return nil, "", false
	}
	if record.ProjectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, record.ProjectID, need); !ok {
			return nil, "", false
		}
	} else {
		userID := getUserID(r)
		if userID == 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, "", false
		}
		if userID != record.UserID && s.store.GetPlatformRole(userID) != PlatformAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, "", false
		}
	}
	_, encrypted, err := s.store.GetMCPServer(record.UserID, record.ID)
	if err != nil {
		http.Error(w, "managed MCP server not found", http.StatusNotFound)
		return nil, "", false
	}
	return record, encrypted, true
}

func (s *Server) requireMCPServerAccess(w http.ResponseWriter, r *http.Request, serverID int64, need ProjectRole) (*MCPServerRecord, string, bool) {
	record, err := s.store.GetMCPServerByIDUnscoped(serverID)
	if err != nil || record == nil {
		http.Error(w, "MCP server not found", http.StatusNotFound)
		return nil, "", false
	}
	if record.Source == managedMCPSource {
		return s.requireManagedMCPAccess(w, r, serverID, need)
	}
	userID := getUserID(r)
	owned, encrypted, err := s.store.GetMCPServer(userID, serverID)
	if err != nil {
		http.Error(w, "MCP server not found", http.StatusNotFound)
		return nil, "", false
	}
	return owned, encrypted, true
}

func (s *Server) validateManagedMCPBindings(ownerID int64, projectID string, bindings managedMCPBindings) error {
	for alias, connectionID := range bindings.Integrations {
		if !managedMCPAliasRE.MatchString(strings.TrimSpace(alias)) {
			return fmt.Errorf("invalid integration alias %q", alias)
		}
		conn, _, err := s.store.GetConnection(ownerID, connectionID)
		if err != nil {
			return fmt.Errorf("integration binding %q does not exist", alias)
		}
		if conn.Status != "active" && conn.Status != "connected" {
			return fmt.Errorf("integration binding %q is not active", alias)
		}
		if conn.ProjectID != "" && conn.ProjectID != projectID {
			return fmt.Errorf("integration binding %q belongs to another project", alias)
		}
	}
	for alias, installID := range bindings.Apps {
		if !managedMCPAliasRE.MatchString(strings.TrimSpace(alias)) {
			return fmt.Errorf("invalid app alias %q", alias)
		}
		if s.installedApps == nil {
			return fmt.Errorf("app binding %q is not running", alias)
		}
		app := s.installedApps.Get(installID)
		if app == nil {
			return fmt.Errorf("app binding %q is not running", alias)
		}
		if app.ProjectID != "" && app.ProjectID != projectID {
			return fmt.Errorf("app binding %q belongs to another project", alias)
		}
		if strings.TrimSpace(app.SidecarURL) == "" {
			return fmt.Errorf("app binding %q does not expose a callable MCP endpoint", alias)
		}
	}
	return nil
}

func managedMCPEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) managedMCPResponse(record *MCPServerRecord, encrypted string) (*managedMCPResponse, error) {
	def, err := managedmcp.Load(s.managedMCPSourceDir(record.ID))
	if err != nil {
		return nil, err
	}
	cfg, err := s.decryptManagedMCPConfig(encrypted)
	if err != nil {
		return nil, err
	}
	return &managedMCPResponse{
		Server:     record,
		Definition: def,
		Bindings:   cfg.Bindings,
		EnvKeys:    managedMCPEnvKeys(cfg.Env),
	}, nil
}

// POST /mcp-servers/managed
func (s *Server) handleCreateManagedMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireCapability(w, r, "custom_mcp") {
		return
	}
	var body managedMCPCreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Description = strings.TrimSpace(body.Description)
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	if !managedMCPAliasRE.MatchString(body.Name) {
		http.Error(w, "name must start with a letter and contain only letters, numbers, _ or -", http.StatusBadRequest)
		return
	}
	if body.ProjectID == "" {
		http.Error(w, "project_id is required for managed MCP servers", http.StatusBadRequest)
		return
	}
	userID, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectEditor)
	if !ok {
		return
	}
	body.Definition = managedmcp.Normalize(body.Definition)
	if err := managedmcp.Validate(body.Definition); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg := normalizeManagedMCPConfig(managedMCPConfig{Env: body.Env, Bindings: body.Bindings})
	for key := range cfg.Env {
		if !managedMCPEnvRE.MatchString(key) || strings.HasPrefix(key, "APTEVA_MCP_") {
			http.Error(w, "invalid or reserved environment variable: "+key, http.StatusBadRequest)
			return
		}
	}
	if err := s.validateManagedMCPBindings(userID, body.ProjectID, cfg.Bindings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	encrypted, err := s.encryptManagedMCPConfig(cfg)
	if err != nil {
		http.Error(w, "failed to encrypt configuration", http.StatusInternalServerError)
		return
	}
	revision, err := managedMCPRevision(body.Definition, cfg)
	if err != nil {
		http.Error(w, "failed to hash definition", http.StatusInternalServerError)
		return
	}
	if body.Description == "" {
		body.Description = body.Name
	}
	record, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: userID, Name: body.Name, Description: body.Description,
		Source: managedMCPSource, Transport: "stdio", Command: managedMCPCommand,
		Args: "[]", EncryptedEnv: encrypted, ProjectID: body.ProjectID,
		UpstreamID: revision, ToolCount: len(body.Definition.Tools),
	})
	if err != nil {
		http.Error(w, "failed to create managed MCP server", http.StatusInternalServerError)
		return
	}
	if err := writeManagedMCPSource(s.managedMCPSourceDir(record.ID), body.Definition); err != nil {
		_ = s.store.DeleteMCPServer(userID, record.ID)
		_ = os.RemoveAll(s.managedMCPWorkspace(record.ID))
		http.Error(w, "failed to write managed MCP source: "+err.Error(), http.StatusInternalServerError)
		return
	}
	start := body.Start == nil || *body.Start
	startWarning := ""
	if start {
		if _, err := s.startManagedMCP(record, cfg); err != nil {
			s.store.UpdateMCPServerStatus(record.ID, "failed", len(body.Definition.Tools), 0)
			record.Status = "failed"
			startWarning = "created, but failed to start: " + err.Error()
		} else {
			record.Status = "running"
			record.ToolCount = len(body.Definition.Tools)
		}
	}
	_, encrypted, _ = s.store.GetMCPServer(userID, record.ID)
	response, err := s.managedMCPResponse(record, encrypted)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response.Warning = startWarning
	writeJSONStatus(w, http.StatusCreated, response)
}

// GET|PUT /mcp-servers/:id/managed
func (s *Server) handleManagedMCPServer(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idPart := strings.TrimSuffix(path, "/managed")
	serverID, err := atoi64(idPart)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	need := ProjectViewer
	if r.Method != http.MethodGet {
		need = ProjectEditor
	}
	record, encrypted, ok := s.requireManagedMCPAccess(w, r, serverID, need)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		response, err := s.managedMCPResponse(record, encrypted)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, response)
	case http.MethodPut:
		s.updateManagedMCPServer(w, r, record, encrypted)
	default:
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
	}
}

func (s *Server) updateManagedMCPServer(w http.ResponseWriter, r *http.Request, record *MCPServerRecord, encrypted string) {
	var body managedMCPUpdateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	oldDef, err := managedmcp.Load(s.managedMCPSourceDir(record.ID))
	if err != nil {
		http.Error(w, "load current definition: "+err.Error(), http.StatusInternalServerError)
		return
	}
	oldCfg, err := s.decryptManagedMCPConfig(encrypted)
	if err != nil {
		http.Error(w, "load current configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	nextDef := oldDef
	if body.Definition != nil {
		nextDef = managedmcp.Normalize(*body.Definition)
	}
	if err := managedmcp.Validate(nextDef); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	nextCfg := normalizeManagedMCPConfig(oldCfg)
	for key, value := range body.Env {
		key = strings.TrimSpace(key)
		if !managedMCPEnvRE.MatchString(key) || strings.HasPrefix(key, "APTEVA_MCP_") {
			http.Error(w, "invalid or reserved environment variable: "+key, http.StatusBadRequest)
			return
		}
		nextCfg.Env[key] = value
	}
	for _, key := range body.DeleteEnv {
		delete(nextCfg.Env, key)
	}
	if body.Bindings != nil {
		nextCfg.Bindings = *body.Bindings
		nextCfg = normalizeManagedMCPConfig(nextCfg)
	}
	if err := s.validateManagedMCPBindings(record.UserID, record.ProjectID, nextCfg.Bindings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	nextDescription := record.Description
	oldDescription := record.Description
	if body.Description != nil {
		nextDescription = strings.TrimSpace(*body.Description)
		if nextDescription == "" {
			http.Error(w, "description cannot be empty", http.StatusBadRequest)
			return
		}
	}
	nextEncrypted, err := s.encryptManagedMCPConfig(nextCfg)
	if err != nil {
		http.Error(w, "failed to encrypt configuration", http.StatusInternalServerError)
		return
	}
	nextRevision, err := managedMCPRevision(nextDef, nextCfg)
	if err != nil {
		http.Error(w, "failed to hash definition", http.StatusInternalServerError)
		return
	}
	wasRunning := record.Status == "running" || s.mcpManager.IsRunning(record.ID)
	if err := replaceManagedMCPSource(s.managedMCPWorkspace(record.ID), nextDef); err != nil {
		http.Error(w, "failed to stage managed MCP source: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if wasRunning {
		s.mcpManager.Stop(record.ID)
	}
	if err := s.store.UpdateManagedMCPServer(record.UserID, record.ID, nextDescription, nextEncrypted, nextRevision, len(nextDef.Tools)); err != nil {
		_ = rollbackManagedMCPSource(s.managedMCPWorkspace(record.ID))
		if wasRunning {
			_, _ = s.startManagedMCP(record, oldCfg)
		}
		http.Error(w, "failed to save managed MCP server", http.StatusInternalServerError)
		return
	}
	record.Description = nextDescription
	record.UpstreamID = nextRevision
	record.ToolCount = len(nextDef.Tools)
	if wasRunning {
		if _, err := s.startManagedMCP(record, nextCfg); err != nil {
			_ = rollbackManagedMCPSource(s.managedMCPWorkspace(record.ID))
			oldRevision, _ := managedMCPRevision(oldDef, oldCfg)
			_ = s.store.UpdateManagedMCPServer(record.UserID, record.ID, oldDescription, encrypted, oldRevision, len(oldDef.Tools))
			record.Description = oldDescription
			record.UpstreamID = oldRevision
			if _, rollbackErr := s.startManagedMCP(record, oldCfg); rollbackErr != nil {
				s.store.UpdateMCPServerStatus(record.ID, "failed", len(oldDef.Tools), 0)
				http.Error(w, "new revision failed and previous revision could not restart: "+rollbackErr.Error(), http.StatusBadGateway)
				return
			}
			http.Error(w, "new revision failed to start; previous revision restored: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	fresh, freshEncrypted, _ := s.store.GetMCPServer(record.UserID, record.ID)
	response, err := s.managedMCPResponse(fresh, freshEncrypted)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, response)
}

// POST /mcp-servers/:id/validate accepts either {"definition": ...} or a
// complete managed create payload and performs syntax/schema checks without
// writing source or changing a running process.
func (s *Server) handleValidateManagedMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idPart := strings.TrimSuffix(path, "/validate")
	serverID, err := atoi64(idPart)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	record, _, ok := s.requireManagedMCPAccess(w, r, serverID, ProjectEditor)
	if !ok {
		return
	}
	var body struct {
		Definition managedmcp.Definition `json:"definition"`
		Bindings   *managedMCPBindings   `json:"bindings,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.Definition = managedmcp.Normalize(body.Definition)
	if err := managedmcp.Validate(body.Definition); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	if body.Bindings != nil {
		if err := s.validateManagedMCPBindings(record.UserID, record.ProjectID, *body.Bindings); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"valid": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"valid": true, "tool_count": len(body.Definition.Tools)})
}

// GET /mcp-servers/:id/logs
func (s *Server) handleManagedMCPLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/mcp-servers/")
	idPart := strings.TrimSuffix(path, "/logs")
	serverID, err := atoi64(idPart)
	if err != nil {
		http.Error(w, "invalid ID", http.StatusBadRequest)
		return
	}
	if _, _, ok := s.requireManagedMCPAccess(w, r, serverID, ProjectViewer); !ok {
		return
	}
	maxBytes := int64(128 << 10)
	raw, err := readFileTail(s.managedMCPLogPath(serverID), maxBytes)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"logs": string(raw)})
}

func readFileTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

func (s *Server) managedMCPRunnerBinary() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("APTEVA_MCP_RUNNER_BIN")); configured != "" {
		if filepath.IsAbs(configured) {
			return configured, nil
		}
		return exec.LookPath(configured)
	}
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), "apteva-mcp-runner")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return exec.LookPath("apteva-mcp-runner")
}

func (s *Server) managedMCPToken(record *MCPServerRecord) string {
	payload := fmt.Sprintf("%d:%d:%s:%s", record.ID, record.UserID, record.ProjectID, record.UpstreamID)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = io.WriteString(mac, payload)
	return "v1." + strconv.FormatInt(record.ID, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validateManagedMCPToken(record *MCPServerRecord, token string) bool {
	expected := s.managedMCPToken(record)
	if len(token) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (s *Server) startManagedMCP(record *MCPServerRecord, cfg managedMCPConfig) (*MCPProcess, error) {
	if record == nil || record.Source != managedMCPSource {
		return nil, errors.New("not a managed MCP server")
	}
	if s.mcpManager.IsRunning(record.ID) {
		proc, _ := s.mcpManager.processByID(record.ID)
		return proc, nil
	}
	runner, err := s.managedMCPRunnerBinary()
	if err != nil {
		return nil, fmt.Errorf("find apteva-mcp-runner: %w", err)
	}
	logFile, err := newRotatingLogFile(s.managedMCPLogPath(record.ID), managedMCPLogMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("open runner log: %w", err)
	}
	runtimeRecord := *record
	runtimeRecord.Command = runner
	runtimeRecord.Args = "[]"
	runtimeRecord.Transport = "stdio"
	env := map[string]string{}
	for key, value := range cfg.Env {
		if strings.HasPrefix(key, "APTEVA_MCP_") {
			continue
		}
		env[key] = value
	}
	env["APTEVA_MCP_WORKSPACE"] = s.managedMCPSourceDir(record.ID)
	env["APTEVA_MCP_GATEWAY_URL"] = fmt.Sprintf("http://127.0.0.1:%s/api/managed-mcp-runtime/%d", s.port, record.ID)
	env["APTEVA_MCP_TOKEN"] = s.managedMCPToken(record)
	proc, err := s.mcpManager.StartIsolatedWithStderr(&runtimeRecord, env, logFile)
	if err != nil {
		if f, openErr := os.OpenFile(s.managedMCPLogPath(record.ID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); openErr == nil {
			_, _ = fmt.Fprintf(f, "runner start failed: %v\n", err)
			_ = f.Close()
		}
		return nil, err
	}
	pid := 0
	if proc.cmd != nil && proc.cmd.Process != nil {
		pid = proc.cmd.Process.Pid
	}
	s.store.UpdateMCPServerStatus(record.ID, "running", len(proc.Tools), pid)
	return proc, nil
}

func (s *Server) ensureManagedMCPRunning(record *MCPServerRecord) (*MCPProcess, error) {
	if proc, ok := s.mcpManager.processByID(record.ID); ok {
		return proc, nil
	}
	_, encrypted, err := s.store.GetMCPServer(record.UserID, record.ID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.decryptManagedMCPConfig(encrypted)
	if err != nil {
		return nil, err
	}
	return s.startManagedMCP(record, cfg)
}

// ResumeManagedMCPs restores only rows explicitly left in status=running.
// A manual Stop survives restart; a runner crash is healed on boot and also
// lazily on the next agent tools/list or tools/call.
func (s *Server) ResumeManagedMCPs() {
	ids, err := s.store.ListManagedMCPsByStatus("running")
	if err != nil {
		log.Printf("[MANAGED-MCP] list resumable servers: %v", err)
		return
	}
	for _, id := range ids {
		record, err := s.store.GetMCPServerByIDUnscoped(id)
		if err != nil {
			log.Printf("[MANAGED-MCP] load server %d: %v", id, err)
			continue
		}
		if _, err := s.ensureManagedMCPRunning(record); err != nil {
			log.Printf("[MANAGED-MCP] resume server %d: %v", id, err)
			s.store.UpdateMCPServerStatus(id, "failed", record.ToolCount, 0)
		}
	}
}

func requestIsLoopback(r *http.Request) bool {
	if r.RemoteAddr == "" {
		return true // direct unit-test invocation
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// detachMCPServerFromAgents removes a deleted custom server from both stopped
// config.json files and live core configs. It is best-effort by design: the DB
// row and bridge disappear regardless, so a transiently unreachable core can
// no longer call the deleted server and will shed the stale entry on restart.
func (s *Server) detachMCPServerFromAgents(record *MCPServerRecord) {
	if record == nil || record.ProjectID == "" {
		return
	}
	agents, err := s.store.ListAgentsInProject(record.ProjectID)
	if err != nil {
		log.Printf("[MANAGED-MCP] list agents for detach server=%d: %v", record.ID, err)
		return
	}
	for i := range agents {
		agent := &agents[i]
		port := s.agents.GetPort(agent.ID)
		if port == 0 {
			if err := s.writeStoppedConfigAtomic(agent.ID, func(cfg map[string]any) error {
				removeMCPServerFromConfig(cfg, record)
				return nil
			}); err != nil {
				log.Printf("[MANAGED-MCP] detach stopped agent=%d server=%d: %v", agent.ID, record.ID, err)
			}
			continue
		}
		target := fmt.Sprintf("http://127.0.0.1:%d/config", port)
		coreKey := s.agents.GetCoreAPIKey(agent.ID)
		resp, err := s.coreDoWithBootWait(agent.ID, http.MethodGet, target, nil, coreKey)
		if err != nil {
			log.Printf("[MANAGED-MCP] read live agent=%d for detach server=%d: %v", agent.ID, record.ID, err)
			continue
		}
		var cfg map[string]any
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&cfg)
		_ = resp.Body.Close()
		if decodeErr != nil || cfg == nil {
			log.Printf("[MANAGED-MCP] decode live agent=%d config for detach server=%d: %v", agent.ID, record.ID, decodeErr)
			continue
		}
		if !removeMCPServerFromConfig(cfg, record) {
			continue
		}
		raw, _ := json.Marshal(cfg)
		putResp, err := s.coreDoWithBootWait(
			agent.ID, http.MethodPut, target, raw, coreKey,
			http.Header{"Content-Type": []string{"application/json"}},
		)
		if err != nil {
			log.Printf("[MANAGED-MCP] detach live agent=%d server=%d: %v", agent.ID, record.ID, err)
			continue
		}
		_ = putResp.Body.Close()
	}
}

func removeMCPServerFromConfig(cfg map[string]any, record *MCPServerRecord) bool {
	rawList, ok := cfg["mcp_servers"].([]any)
	if !ok {
		return false
	}
	expectedURLSuffix := "/mcp/custom/" + strconv.FormatInt(record.ID, 10)
	kept := make([]any, 0, len(rawList))
	changed := false
	for _, raw := range rawList {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		urlValue, _ := entry["url"].(string)
		if strings.HasSuffix(strings.TrimRight(urlValue, "/"), expectedURLSuffix) ||
			(name == record.Name && (record.Source == "custom" || record.Source == managedMCPSource)) {
			changed = true
			continue
		}
		kept = append(kept, raw)
	}
	if changed {
		cfg["mcp_servers"] = kept
	}
	return changed
}
