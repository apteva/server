package main

// runtime_api.go exposes the generic, app-authenticated execution primitive
// used by higher-level products such as Environments and Evals. The existing
// Environment supervisor remains the internal process engine during migration,
// but this API deliberately does not expose Environment records, host paths,
// ports, sidecar URLs, core keys, or eval-specific behavior.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
)

const (
	defaultRuntimeTTL              = time.Hour
	minRuntimeTTL                  = time.Minute
	maxRuntimeTTL                  = 24 * time.Hour
	maxInstallRuntimes             = 8
	maxRuntimeApps                 = 16
	maxRuntimeConnections          = 32
	maxRuntimeAgents               = 8
	maxRuntimeMCPAttachments       = 16
	maxRuntimeManagedMCPs          = 16
	maxRuntimeMocks                = 256
	maxRuntimeRequestBytes   int64 = 2 << 20
)

type RuntimeMCPAttachment struct {
	ID        string
	Name      string
	Path      string
	InstallID int64
	Token     string
}

func (s *Server) handleCallbackRuntimes(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 || parts[0] == "" {
		s.handleRuntimeCollection(w, r)
		return
	}
	if parts[0] == "catalog" {
		s.handleRuntimeCatalog(w, r, parts[1:])
		return
	}
	if parts[0] == "artifacts" {
		s.handleRuntimeArtifacts(w, r, parts[1:])
		return
	}

	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	runtime, ok := s.runtimeOwnedByInstall(parts[0], installID)
	if !ok {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		s.handleRuntimeRoot(w, r, runtime)
		return
	}
	switch parts[1] {
	case "apps":
		s.handleRuntimeApps(w, r, runtime, parts[2:])
	case "agents":
		s.handleRuntimeAgents(w, r, runtime, parts[2:])
	case "mcp-attachments":
		s.handleRuntimeMCPAttachments(w, r, runtime, parts[2:])
	case "managed-mcps":
		s.handleRuntimeManagedMCPs(w, r, runtime, parts[2:])
	case "edge":
		s.handleRuntimeEdge(w, r, runtime, parts[2:])
	case "snapshots":
		s.handleRuntimeSnapshots(w, r, runtime, parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeCollection(w http.ResponseWriter, r *http.Request) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesRead, sdk.PermRuntimesManage) {
			return
		}
		out := []sdk.RuntimeSummary{}
		if s.environments != nil {
			for _, runtime := range s.environments.List() {
				if runtime.OwnerInstallID() == installID {
					out = append(out, s.runtimeSummary(runtime))
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		writeJSON(w, out)
	case http.MethodPost:
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesManage) {
			return
		}
		if s.environments == nil {
			http.Error(w, "runtime manager unavailable", http.StatusServiceUnavailable)
			return
		}
		active := 0
		for _, existing := range s.environments.List() {
			if existing.OwnerInstallID() == installID {
				active++
			}
		}
		if active >= maxInstallRuntimes {
			http.Error(w, "runtime limit reached for this app install", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req sdk.RuntimeCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if len(req.AppInstallIDs) > maxRuntimeApps || len(req.ConnectionIDs) > maxRuntimeConnections || len(req.MCPServerIDs) > maxRuntimeManagedMCPs || len(req.HTTPMocks) > maxRuntimeMocks || len(req.IntegrationFixtures) > maxRuntimeMocks || len(req.IntegrationBindings) > maxRuntimeConnections || len(req.ConnectionBindings) > maxRuntimeConnections || len(req.Subscriptions) > maxRuntimeMocks {
			http.Error(w, "runtime resource limit exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		userID, projectID, ok := s.runtimeCallerProject(w, r, installID, req.ProjectID, ProjectEditor)
		if !ok {
			return
		}
		snapshotMCPRevisions := map[int64]string{}
		if req.SnapshotID != "" {
			if !validRuntimeID(req.SnapshotID) {
				http.Error(w, "invalid runtime snapshot id", http.StatusBadRequest)
				return
			}
			man, err := s.environments.Snapshots().Get(req.SnapshotID)
			if err != nil || man.OwnerInstallID != installID || man.ProjectID != projectID {
				http.Error(w, "runtime snapshot not found", http.StatusNotFound)
				return
			}
			if len(req.AppInstallIDs) == 0 && len(man.SourceInstallIDs) > 0 {
				names := make([]string, 0, len(man.SourceInstallIDs))
				for name := range man.SourceInstallIDs {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					if id := man.SourceInstallIDs[name]; id > 0 {
						req.AppInstallIDs = append(req.AppInstallIDs, id)
					}
				}
			}
			if len(req.MCPServerIDs) == 0 && len(man.ManagedMCPs) > 0 {
				for _, mcp := range man.ManagedMCPs {
					if mcp.SourceID > 0 {
						req.MCPServerIDs = append(req.MCPServerIDs, mcp.SourceID)
					}
				}
			}
			for _, mcp := range man.ManagedMCPs {
				if mcp.SourceID > 0 {
					snapshotMCPRevisions[mcp.SourceID] = mcp.Revision
				}
			}
		}
		selectedMCPs, mcpAppIDs, err := s.selectRuntimeManagedMCPs(userID, projectID, req.MCPServerIDs)
		if err != nil {
			http.Error(w, "runtime managed MCPs: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, selected := range selectedMCPs {
			if revision, pinned := snapshotMCPRevisions[selected.Record.ID]; pinned && revision != selected.Record.UpstreamID {
				http.Error(w, "runtime managed MCP changed since snapshot: "+selected.Record.Name, http.StatusConflict)
				return
			}
		}
		req.AppInstallIDs = appendUniqueInt64(req.AppInstallIDs, mcpAppIDs...)
		if len(req.AppInstallIDs) > maxRuntimeApps || len(req.ConnectionIDs) > maxRuntimeConnections || len(selectedMCPs) > maxRuntimeManagedMCPs || len(req.HTTPMocks) > maxRuntimeMocks || len(req.IntegrationFixtures) > maxRuntimeMocks || len(req.IntegrationBindings) > maxRuntimeConnections || len(req.ConnectionBindings) > maxRuntimeConnections || len(req.Subscriptions) > maxRuntimeMocks {
			http.Error(w, "runtime resource limit exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		if req.ID == "" {
			req.ID = "rt_" + randomRuntimeToken(12)
		}
		if !validRuntimeID(req.ID) {
			http.Error(w, "runtime id must contain only letters, numbers, dash, underscore, or dot", http.StatusBadRequest)
			return
		}
		if _, exists := s.environments.Get(req.ID); exists {
			http.Error(w, "runtime id already exists", http.StatusConflict)
			return
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl == 0 {
			ttl = defaultRuntimeTTL
		}
		if ttl < minRuntimeTTL || ttl > maxRuntimeTTL {
			http.Error(w, "ttl_seconds must be between 60 and 86400", http.StatusBadRequest)
			return
		}
		appSrcDirs, err := s.environmentAppSrcDirsForInstalls(projectID, req.AppInstallIDs)
		if err != nil {
			http.Error(w, "runtime apps: "+err.Error(), http.StatusBadRequest)
			return
		}
		connectionIDs, err := s.environmentVisibleConnectionIDs(userID, projectID, req.ConnectionIDs)
		if err != nil {
			http.Error(w, "runtime connections: "+err.Error(), http.StatusBadRequest)
			return
		}
		httpMocks := make([]HTTPMock, 0, len(req.HTTPMocks))
		for _, mock := range req.HTTPMocks {
			httpMocks = append(httpMocks, HTTPMock{Host: mock.Host, Path: mock.Path, Method: mock.Method, Status: mock.Status, Headers: mock.Headers, Body: mock.Body})
		}
		fixtures := make([]IntegrationFixture, 0, len(req.IntegrationFixtures))
		for _, fixture := range req.IntegrationFixtures {
			fixtures = append(fixtures, IntegrationFixture{App: fixture.App, Tool: fixture.Tool, Status: fixture.Status, Data: fixture.Data})
		}
		subscriptions := make([]EnvironmentSubscriptionSpec, 0, len(req.Subscriptions))
		for _, sub := range req.Subscriptions {
			subscriptions = append(subscriptions, EnvironmentSubscriptionSpec{ID: sub.ID, Source: sub.Source, App: sub.App, Topic: sub.Topic, TargetAgentAlias: sub.TargetAgentAlias, ThreadID: sub.ThreadID, Name: sub.Name, Description: sub.Description, Enabled: sub.Enabled})
		}
		subscriptions, err = normalizeEnvironmentSubscriptionSpecs(subscriptions)
		if err != nil {
			http.Error(w, "runtime subscriptions: "+err.Error(), http.StatusBadRequest)
			return
		}
		bindings := make([]RuntimeIntegrationBinding, 0, len(req.IntegrationBindings))
		for _, binding := range req.IntegrationBindings {
			bindings = append(bindings, RuntimeIntegrationBinding{App: binding.App, Role: binding.Role, Slug: binding.Slug, AppName: binding.AppName, Name: binding.Name, AuthType: binding.AuthType, Credentials: binding.Credentials, ExposeToAgents: binding.ExposeToAgents})
		}
		if err := validateRuntimeBindingOverlap(req.ConnectionBindings, bindings); err != nil {
			http.Error(w, "runtime connection bindings: "+err.Error(), http.StatusBadRequest)
			return
		}
		sourceIDs := map[string]int64{}
		for _, info := range s.environmentInstallInfos(req.AppInstallIDs) {
			sourceIDs[info.Name] = info.InstallID
		}
		for _, selected := range selectedMCPs {
			if _, collides := sourceIDs[selected.Record.Name]; collides {
				http.Error(w, "runtime managed MCP name collides with app: "+selected.Record.Name, http.StatusBadRequest)
				return
			}
		}
		networkMode := EdgeMode(req.NetworkMode)
		if networkMode == "" {
			networkMode = EdgeBlock
		}
		spec := EnvironmentSpec{
			ID:                    req.ID,
			ProjectID:             projectID,
			GatewayURL:            "http://127.0.0.1:" + s.port,
			RuntimeOwnerInstallID: installID,
			RuntimeExpiresAt:      time.Now().Add(ttl).UTC(),
			SourceInstallIDs:      sourceIDs,
			AppSrcDirs:            appSrcDirs,
			ConnectionIDs:         connectionIDs,
			NetworkMode:           networkMode,
			IntegrationMode:       req.IntegrationMode,
			Policy:                SandboxPolicy{AllowHostSuffixes: req.AllowHostSuffixes, Mocks: httpMocks},
			IntegrationFixtures:   fixtures,
			Subscriptions:         subscriptions,
		}
		var runtime *Environment
		if req.SnapshotID != "" {
			runtime, err = s.environments.CreateFromSnapshot(spec, req.SnapshotID)
		} else {
			runtime, err = s.environments.Create(spec)
		}
		if err != nil {
			http.Error(w, "create runtime: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.ConnectionBindings) > 0 {
			if err := s.bindEnvironmentConnections(userID, projectID, runtime, req.ConnectionBindings); err != nil {
				s.environments.Destroy(runtime.ID)
				http.Error(w, "runtime connection bindings: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if len(bindings) > 0 {
			if err := s.bindEnvironmentIntegrationMocks(userID, runtime, bindings); err != nil {
				s.environments.Destroy(runtime.ID)
				http.Error(w, "runtime integration bindings: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		for _, selected := range selectedMCPs {
			if err := s.startRuntimeManagedMCP(runtime, selected); err != nil {
				s.environments.Destroy(runtime.ID)
				http.Error(w, "runtime managed MCPs: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s.runtimeSummary(runtime))
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRuntimeRoot(w http.ResponseWriter, r *http.Request, runtime *Environment) {
	installID := runtime.OwnerInstallID()
	switch r.Method {
	case http.MethodGet:
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesRead, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, s.runtimeSummary(runtime))
	case http.MethodDelete:
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesManage) {
			return
		}
		s.environments.Destroy(runtime.ID)
		writeJSON(w, map[string]any{"destroyed": runtime.ID})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRuntimeApps(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, s.runtimeApps(runtime))
		return
	}
	appName, err := url.PathUnescape(parts[0])
	if err != nil || appName == "" {
		http.Error(w, "invalid app name", http.StatusBadRequest)
		return
	}
	if _, ok := runtime.Install(appName); !ok {
		if _, legacy := runtime.App(appName); !legacy {
			http.Error(w, "runtime app not found", http.StatusNotFound)
			return
		}
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "endpoint":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		endpoint, err := s.runtimeAppEndpoint(runtime, appName)
		if err != nil {
			http.Error(w, "runtime app endpoint: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, endpoint)
	case "tools":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		tools, err := s.runtimeAppTools(runtime, appName)
		if err != nil {
			http.Error(w, "list runtime tools: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, tools)
	case "call":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req struct {
			Tool       string         `json:"tool"`
			Input      map[string]any `json:"input"`
			AgentAlias string         `json:"agent_alias"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Tool) == "" {
			http.Error(w, "tool required", http.StatusBadRequest)
			return
		}
		inst, ok := runtime.Install(appName)
		if !ok {
			http.Error(w, "runtime app is not install-backed", http.StatusBadRequest)
			return
		}
		token, err := s.appInstallToken(inst.InstallID)
		if err != nil {
			http.Error(w, "runtime app credential unavailable", http.StatusServiceUnavailable)
			return
		}
		var agentID string
		if strings.TrimSpace(req.AgentAlias) != "" {
			agent := runtime.AgentByAlias(req.AgentAlias)
			if agent == nil {
				http.Error(w, "runtime agent alias not found", http.StatusBadRequest)
				return
			}
			agentID = strconv.FormatInt(agent.AgentID, 10)
		}
		result, err := callAppMCPToolAsAgent(inst.SidecarURL+"/mcp", token, agentID, req.Tool, req.Input)
		if err != nil {
			http.Error(w, "call runtime app: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"result": result})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeMCPAttachments(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) != 0 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, publicRuntimeAttachments(runtime.MCPAttachments()))
	case http.MethodPost:
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
			return
		}
		if len(runtime.MCPAttachments()) >= maxRuntimeMCPAttachments {
			http.Error(w, "runtime MCP attachment limit reached", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req sdk.RuntimeMCPAttachmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || !validRuntimeMCPPath(req.Path) {
			http.Error(w, "name and an absolute app-local path are required", http.StatusBadRequest)
			return
		}
		a := RuntimeMCPAttachment{ID: "mcp_" + randomRuntimeToken(8), Name: req.Name, Path: req.Path, InstallID: runtime.OwnerInstallID(), Token: randomRuntimeToken(24)}
		if err := runtime.AddMCPAttachment(a); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, publicRuntimeAttachment(a))
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRuntimeManagedMCPs(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, publicRuntimeManagedMCPs(runtime.ManagedMCPs()))
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	name, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(name) == "" {
		http.Error(w, "invalid managed MCP name", http.StatusBadRequest)
		return
	}
	mcp := runtime.ManagedMCP(name)
	if mcp == nil || mcp.Process == nil {
		http.Error(w, "runtime managed MCP not found", http.StatusNotFound)
		return
	}
	switch parts[1] {
	case "tools":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		writeJSON(w, runtimeManagedMCPTools(filterMCPTools(mcp.Process.Tools, mcp.AllowedTools)))
	case "call":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		var req struct {
			Tool  string         `json:"tool"`
			Input map[string]any `json:"input"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)).Decode(&req) != nil || strings.TrimSpace(req.Tool) == "" {
			http.Error(w, "tool required", http.StatusBadRequest)
			return
		}
		if !managedMCPAllowed(&MCPServerRecord{AllowedTools: mcp.AllowedTools}, req.Tool) {
			http.Error(w, "tool is not enabled", http.StatusForbidden)
			return
		}
		result, err := mcp.Process.call("tools/call", map[string]any{"name": req.Tool, "arguments": req.Input})
		if err != nil {
			http.Error(w, "call runtime managed MCP: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"result": json.RawMessage(result)})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeAgents(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
				return
			}
			writeJSON(w, s.publicRuntimeAgents(runtime))
		case http.MethodPost:
			if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
				return
			}
			if len(runtime.Agents()) >= maxRuntimeAgents {
				http.Error(w, "runtime agent limit reached", http.StatusTooManyRequests)
				return
			}
			s.handleSpawnRuntimeAgent(w, r, runtime)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}
	key, _ := url.PathUnescape(parts[0])
	agent := resolveEnvironmentAgent(runtime, key)
	if agent == nil {
		http.Error(w, "runtime agent not found", http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
				return
			}
			writeJSON(w, s.publicRuntimeAgent(agent))
		case http.MethodDelete:
			if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
				return
			}
			writeJSON(w, map[string]any{"stopped": runtime.StopAgent(agent.AgentID)})
		default:
			http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
		}
		return
	}
	if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
		return
	}
	switch parts[1] {
	case "event":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req sdk.RuntimeAgentEventRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Message) == "" {
			http.Error(w, "message required", http.StatusBadRequest)
			return
		}
		threadID := strings.TrimSpace(req.ThreadID)
		if threadID == "" {
			threadID = "main"
		}
		if err := postCoreEvent(r.Context(), agent.Port, agent.APIKey, threadID, req.Message); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"accepted": true})
	case "control":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Action != "run" && req.Action != "pause" && req.Action != "step" {
			http.Error(w, "action must be run, pause, or step", http.StatusBadRequest)
			return
		}
		if _, err := runtimeCoreRequest(r.Context(), agent, http.MethodPost, "/control", map[string]string{"action": req.Action}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"accepted": true})
	case "wait":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req sdk.RuntimeAgentWaitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		result, err := s.waitRuntimeAgent(r.Context(), agent, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, result)
	case "threads":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		path := "/threads"
		if len(parts) == 3 {
			threadID, err := url.PathUnescape(parts[2])
			if err != nil || threadID == "" || strings.Contains(threadID, "/") {
				http.Error(w, "invalid thread id", http.StatusBadRequest)
				return
			}
			path += "/" + url.PathEscape(threadID) + "/context"
		} else if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		raw, err := runtimeCoreRequest(r.Context(), agent, http.MethodGet, path, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeRawJSON(w, raw)
	case "telemetry":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
		var since time.Time
		if raw := r.URL.Query().Get("since"); raw != "" {
			since, _ = time.Parse(time.RFC3339Nano, raw)
		}
		events, err := s.store.QueryTelemetry(agent.AgentID, "", since, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []TelemetryEvent{}
		}
		writeJSON(w, events)
	case "realtime":
		s.handleRuntimeRealtime(w, r, runtime, agent, parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeRealtime(w http.ResponseWriter, r *http.Request, runtime *Environment, agent *EnvironmentAgent, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req sdk.RuntimeRealtimeSpawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		req.ThreadID = strings.TrimSpace(req.ThreadID)
		req.Directive = strings.TrimSpace(req.Directive)
		if req.ThreadID == "" || req.Directive == "" || strings.Contains(req.ThreadID, "/") || len(req.ThreadID) > 128 {
			http.Error(w, "valid thread_id and directive required", http.StatusBadRequest)
			return
		}
		if err := validateRealtimeCallContext(req.CallContext); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mode, err := normalizeRealtimeCapabilityMode(req.CapabilityMode, req.Tools, req.MCP)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tools := req.Tools
		mcpNames := req.MCP
		switch mode {
		case sdk.RealtimeCapabilitiesInheritAgent:
			inheritedMCPs, inheritErr := s.agentSpawnableMCPNames(agent.AgentID)
			if inheritErr != nil {
				http.Error(w, "load runtime agent realtime capabilities: "+inheritErr.Error(), http.StatusBadGateway)
				return
			}
			mcpNames = inheritedMCPs
			tools = nil
		case sdk.RealtimeCapabilitiesExplicit:
			tools = nonNilStrings(tools)
			mcpNames = nonNilStrings(mcpNames)
		case sdk.RealtimeCapabilitiesNone:
			tools = []string{}
			mcpNames = []string{}
		}
		result, err := s.resolver().SpawnRealtimeThread(framework.InstanceInfo{
			ID: agent.AgentID, Name: agent.SourceName, ProjectID: runtime.ProjectID,
			Port: agent.Port, CoreAPIKey: agent.APIKey,
		}, sdk.RealtimeSpawnRequest{
			AgentID: agent.AgentID, ThreadID: req.ThreadID, Directive: req.Directive,
			Voice: req.Voice, Provider: req.Provider, CapabilityMode: mode, Tools: tools, MCP: mcpNames,
			CallContext:   req.CallContext,
			TurnDetection: req.TurnDetection,
			Ephemeral:     req.Ephemeral, InitialMessage: req.InitialMessage,
			BridgeDisconnectTTLSeconds: req.BridgeDisconnectTTLSeconds,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if result.AudioToken != "" {
			baseURL := requestReachableBaseURL(r, s.localGatewayURL())
			bridgeURL, err := publicRealtimeAudioURL(baseURL, agent.AgentID, req.ThreadID, result.AudioToken)
			if err != nil {
				http.Error(w, "invalid runtime gateway URL", http.StatusInternalServerError)
				return
			}
			result.AudioBridgeURL = bridgeURL
		}
		writeJSON(w, result)
		return
	}

	threadID, err := url.PathUnescape(parts[0])
	if err != nil || threadID == "" || strings.Contains(threadID, "/") || len(threadID) > 128 {
		http.Error(w, "invalid thread id", http.StatusBadRequest)
		return
	}
	inst := framework.InstanceInfo{
		ID: agent.AgentID, Name: agent.SourceName, ProjectID: runtime.ProjectID,
		Port: agent.Port, CoreAPIKey: agent.APIKey,
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := s.resolver().KillThread(inst, threadID); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "audio-token" && r.Method == http.MethodPost:
		result, err := s.resolver().RenewRealtimeAudioBridge(inst, threadID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if result.AudioToken != "" {
			baseURL := requestReachableBaseURL(r, s.localGatewayURL())
			bridgeURL, err := publicRealtimeAudioURL(baseURL, agent.AgentID, threadID, result.AudioToken)
			if err != nil {
				http.Error(w, "invalid runtime gateway URL", http.StatusInternalServerError)
				return
			}
			result.AudioBridgeURL = bridgeURL
		}
		writeJSON(w, result)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleSpawnRuntimeAgent(w http.ResponseWriter, r *http.Request, runtime *Environment) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
	var req sdk.RuntimeAgentSpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID := getUserID(r)
	var source *Agent
	if req.SourceAgentID > 0 {
		got, err := s.store.GetAgentByID(req.SourceAgentID)
		if err != nil || got == nil || got.ProjectID != runtime.ProjectID || (got.Kind != "" && got.Kind != "user") {
			http.Error(w, "source agent not found", http.StatusNotFound)
			return
		}
		source = got
	} else if req.Draft != nil {
		mode := strings.TrimSpace(req.Draft.Mode)
		if mode == "" {
			mode = "autonomous"
		}
		source = &Agent{Name: req.Draft.Name, Directive: req.Draft.Directive, Mode: mode, Config: req.Draft.Config, ProjectID: runtime.ProjectID, UserID: userID}
	} else {
		http.Error(w, "source_agent_id or draft required", http.StatusBadRequest)
		return
	}
	wa, err := s.SpawnAgentInEnvironment(runtime, EnvironmentAgentSpec{UserID: userID, Source: source, DirectiveOverride: req.Directive, Alias: req.Alias, StartPaused: req.StartPaused, Provider: req.Provider, Model: req.Model})
	if err != nil {
		http.Error(w, "spawn runtime agent: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s.publicRuntimeAgent(wa))
}

func (s *Server) handleRuntimeEdge(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch parts[0] {
	case "calls":
		writeJSON(w, runtime.Edge().Calls())
	case "cassette":
		cassette := runtime.Edge().Cassette()
		if cassette == nil {
			writeJSON(w, map[string]any{"entries": []any{}})
			return
		}
		writeJSON(w, cassette)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeSnapshots(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) != 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
		return
	}
	var req sdk.RuntimeSnapshotRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID == "" {
		req.ID = runtime.ID + "-snap-" + randomRuntimeToken(6)
	}
	if !validRuntimeID(req.ID) {
		http.Error(w, "invalid snapshot id", http.StatusBadRequest)
		return
	}
	appDirs := map[string]string{}
	sourceInstallIDs := map[string]int64{}
	for name, app := range runtime.Apps() {
		if app.DataDir != "" {
			appDirs[name] = app.DataDir
		}
	}
	for _, name := range runtime.InstallNames() {
		if app, ok := runtime.Install(name); ok && app.DataDir != "" {
			appDirs[name] = app.DataDir
		}
		if installID := runtime.SourceInstallID(name); installID > 0 {
			sourceInstallIDs[name] = installID
		}
	}
	man, err := s.environments.Snapshots().Capture(CaptureSpec{ID: req.ID, ProjectID: runtime.ProjectID, OwnerInstallID: runtime.OwnerInstallID(), Description: req.Description, AppDataDirs: appDirs, SourceInstallIDs: sourceInstallIDs, ManagedMCPs: publicRuntimeManagedMCPs(runtime.ManagedMCPs()), Cassette: runtime.Edge().Cassette(), Subscriptions: runtime.SubscriptionSpecs()})
	if err != nil {
		http.Error(w, "snapshot runtime: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, publicRuntimeSnapshot(man))
}

func (s *Server) handleRuntimeCatalog(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	if parts[0] == "apps" && len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		_, projectID, ok := s.runtimeCallerProject(w, r, installID, r.URL.Query().Get("project_id"), ProjectViewer)
		if !ok {
			return
		}
		writeJSON(w, s.runtimeCatalogApps(projectID))
		return
	}
	if parts[0] == "apps" && len(parts) == 3 && parts[2] == "tools" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		targetID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			http.Error(w, "invalid app install id", http.StatusBadRequest)
			return
		}
		_, projectID, ok := s.runtimeCallerProject(w, r, installID, "", ProjectViewer)
		if !ok {
			return
		}
		if s.installedApps == nil {
			http.Error(w, "app catalog unavailable", http.StatusServiceUnavailable)
			return
		}
		entry := s.installedApps.Get(targetID)
		if entry == nil || (entry.ProjectID != "" && entry.ProjectID != projectID) || entry.SidecarURL == "" {
			http.Error(w, "app install not found", http.StatusNotFound)
			return
		}
		token, err := s.appInstallToken(targetID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tools, err := listAppMCPTools(entry.SidecarURL+"/mcp", token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, runtimeMCPTools(tools))
		return
	}
	if parts[0] == "managed-mcps" && len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		_, projectID, ok := s.runtimeCallerProject(w, r, installID, r.URL.Query().Get("project_id"), ProjectViewer)
		if !ok {
			return
		}
		out, err := s.runtimeCatalogManagedMCPs(projectID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
		return
	}
	if parts[0] == "integrations" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		if len(parts) == 1 {
			writeJSON(w, s.runtimeCatalogIntegrations())
			return
		}
		if len(parts) == 3 && parts[2] == "tools" {
			if s.catalog == nil {
				http.Error(w, "integration catalog unavailable", http.StatusServiceUnavailable)
				return
			}
			app := s.catalog.Get(parts[1])
			if app == nil {
				http.Error(w, "integration not found", http.StatusNotFound)
				return
			}
			out := make([]sdk.RuntimeCatalogIntegrationTool, 0, len(app.Tools))
			for _, tool := range app.Tools {
				out = append(out, sdk.RuntimeCatalogIntegrationTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, MockResponse: tool.MockResponse})
			}
			writeJSON(w, out)
			return
		}
		http.NotFound(w, r)
		return
	}
	if parts[0] == "realtime-providers" && len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		userID, projectID, ok := s.runtimeCallerProject(w, r, installID, r.URL.Query().Get("project_id"), ProjectViewer)
		if !ok {
			return
		}
		pool := s.GetProviderPool(userID, projectID)
		out := make([]sdk.RuntimeRealtimeProvider, 0)
		for _, provider := range pool {
			if !isRealtimeProviderType(provider.Type) {
				continue
			}
			out = append(out, sdk.RuntimeRealtimeProvider{
				Name: providerKeyFromName(provider.Type),
				Models: map[string]string{
					"large": provider.ModelLarge, "medium": provider.ModelMedium, "small": provider.ModelSmall,
				},
				DefaultVoice: provider.RealtimeVoice,
			})
		}
		writeJSON(w, out)
		return
	}
	if parts[0] != "agents" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		_, projectID, ok := s.runtimeCallerProject(w, r, installID, r.URL.Query().Get("project_id"), ProjectViewer)
		if !ok {
			return
		}
		agents, err := s.store.ListAgentsInProject(projectID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]sdk.RuntimeCatalogAgent, 0, len(agents))
		for i := range agents {
			out = append(out, runtimeCatalogAgent(&agents[i]))
		}
		writeJSON(w, out)
		return
	}
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	agentID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.Error(w, "invalid agent id", http.StatusBadRequest)
		return
	}
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil || agent == nil || (agent.Kind != "" && agent.Kind != "user") {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if _, _, ok := s.runtimeCallerProject(w, r, installID, agent.ProjectID, ProjectViewer); !ok {
		return
	}
	switch parts[2] {
	case "capabilities":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimeCatalogRead, sdk.PermRuntimesManage) {
			return
		}
		caps, err := s.runtimeAgentCapabilities(agentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, caps)
	case "directive":
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermAgentsDirectiveWrite) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
		var req sdk.AgentDirectiveUpdateRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.ExpectedETag) == "" {
			http.Error(w, "directive and expected_etag required", http.StatusBadRequest)
			return
		}
		updated, err := s.updateAgentDirectiveFromApp(agent, installID, getUserID(r), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, runtimeCatalogAgent(updated))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRuntimeArtifacts(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if len(parts) == 0 || parts[0] != "snapshots" || s.environments == nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesRead, sdk.PermRuntimesManage) {
			return
		}
		manifests, err := s.environments.Snapshots().List()
		if err != nil {
			http.Error(w, "list runtime snapshots: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := []sdk.RuntimeSnapshot{}
		for _, manifest := range manifests {
			if manifest.OwnerInstallID == installID {
				out = append(out, publicRuntimeSnapshot(manifest))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		writeJSON(w, out)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireRuntimePermission(w, installID, sdk.PermRuntimesManage) {
		return
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || !validRuntimeID(id) {
		http.Error(w, "invalid snapshot id", http.StatusBadRequest)
		return
	}
	manifest, err := s.environments.Snapshots().Get(id)
	if err != nil || manifest.OwnerInstallID != installID {
		http.Error(w, "runtime snapshot not found", http.StatusNotFound)
		return
	}
	if err := s.environments.Snapshots().Delete(id); err != nil {
		http.Error(w, "delete runtime snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": id})
}

func publicRuntimeSnapshot(manifest *SnapshotManifest) sdk.RuntimeSnapshot {
	return sdk.RuntimeSnapshot{ID: manifest.ID, ProjectID: manifest.ProjectID, Description: manifest.Description, Apps: append([]string(nil), manifest.Apps...), ManagedMCPs: append([]sdk.RuntimeManagedMCP(nil), manifest.ManagedMCPs...), HasAgent: manifest.HasAgent, HasCassette: manifest.HasCassette, CreatedAt: manifest.CreatedAt}
}

func (s *Server) runtimeSummary(runtime *Environment) sdk.RuntimeSummary {
	return sdk.RuntimeSummary{ID: runtime.ID, ProjectID: runtime.ProjectID, Status: "running", NetworkMode: sdk.RuntimeNetworkMode(runtime.NetworkMode), IntegrationMode: runtime.IntegrationMode, Apps: s.runtimeApps(runtime), Agents: s.publicRuntimeAgents(runtime), ManagedMCPs: publicRuntimeManagedMCPs(runtime.ManagedMCPs()), MCPAttachments: publicRuntimeAttachments(runtime.MCPAttachments()), CreatedAt: runtime.createdAt, ExpiresAt: runtime.ExpiresAt()}
}

func (s *Server) runtimeApps(runtime *Environment) []sdk.RuntimeApp {
	out := []sdk.RuntimeApp{}
	for _, name := range runtime.InstallNames() {
		out = append(out, sdk.RuntimeApp{Name: name, InstallID: runtime.SourceInstallID(name), Kind: "install", Status: "running"})
	}
	for name := range runtime.Apps() {
		out = append(out, sdk.RuntimeApp{Name: name, Kind: "legacy", Status: "running"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) publicRuntimeAgent(agent *EnvironmentAgent) sdk.RuntimeAgent {
	status := "stopped"
	if s.agents != nil && s.agents.IsRunning(agent.AgentID) {
		status = "running"
	}
	return sdk.RuntimeAgent{ID: agent.AgentID, SourceAgentID: agent.SourceAgentID, SourceName: agent.SourceName, Alias: agent.Alias, Status: status, Provider: agent.Provider, Model: agent.Model, CreatedAt: agent.CreatedAt}
}

func (s *Server) publicRuntimeAgents(runtime *Environment) []sdk.RuntimeAgent {
	out := []sdk.RuntimeAgent{}
	for _, agent := range runtime.Agents() {
		out = append(out, s.publicRuntimeAgent(agent))
	}
	return out
}

func publicRuntimeAttachment(a RuntimeMCPAttachment) sdk.RuntimeMCPAttachment {
	return sdk.RuntimeMCPAttachment{ID: a.ID, Name: a.Name, Path: a.Path}
}
func publicRuntimeAttachments(in []RuntimeMCPAttachment) []sdk.RuntimeMCPAttachment {
	out := make([]sdk.RuntimeMCPAttachment, 0, len(in))
	for _, a := range in {
		out = append(out, publicRuntimeAttachment(a))
	}
	return out
}

func (s *Server) runtimeAppTools(runtime *Environment, appName string) ([]installMCPToolInfo, error) {
	if app, ok := runtime.Install(appName); ok {
		token, err := s.appInstallToken(app.InstallID)
		if err != nil {
			return nil, err
		}
		return listAppMCPTools(app.SidecarURL+"/mcp", token)
	}
	if app, ok := runtime.App(appName); ok {
		return listAppMCPTools(app.MCPURL, "")
	}
	return nil, fmt.Errorf("app not found")
}

func (s *Server) runtimeCatalogApps(projectID string) []sdk.RuntimeCatalogApp {
	out := []sdk.RuntimeCatalogApp{}
	if s.installedApps == nil {
		return out
	}
	for _, app := range s.installedApps.ListForProject(projectID) {
		out = append(out, sdk.RuntimeCatalogApp{
			InstallID:        app.InstallID,
			Name:             app.AppName,
			DisplayName:      app.Manifest.DisplayName,
			Description:      app.Manifest.Description,
			Icon:             resolveInstalledAppIcon(app.AppName, app.Manifest.Icon, app.Manifest.Version, app.InstallID, firstNonEmpty(app.ProjectID, projectID)),
			IconStyle:        app.Manifest.IconStyle,
			ProjectID:        app.ProjectID,
			Status:           "running",
			IntegrationRoles: app.Manifest.Requires.Integrations,
			Publishes:        app.Manifest.Provides.Publishes,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) runtimeCatalogIntegrations() []sdk.RuntimeCatalogIntegration {
	out := []sdk.RuntimeCatalogIntegration{}
	if s.catalog == nil {
		return out
	}
	for _, app := range s.catalog.List() {
		logo := ""
		if app.Logo != nil {
			logo = *app.Logo
		}
		out = append(out, sdk.RuntimeCatalogIntegration{Slug: app.Slug, Name: app.Name, Description: app.Description, Logo: logo, Categories: app.Categories, ToolCount: app.ToolCount, Kind: app.Kind})
	}
	return out
}

func runtimeMCPTools(tools []installMCPToolInfo) []sdk.RuntimeMCPTool {
	out := make([]sdk.RuntimeMCPTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, sdk.RuntimeMCPTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return out
}

func runtimeManagedMCPTools(tools []mcpToolDef) []sdk.RuntimeMCPTool {
	out := make([]sdk.RuntimeMCPTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, sdk.RuntimeMCPTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return out
}

func (s *Server) runtimeAgentCapabilities(agentID int64) ([]sdk.RuntimeAgentCapability, error) {
	if s.installedApps == nil {
		return []sdk.RuntimeAgentCapability{}, nil
	}
	rows, err := s.store.db.Query(`SELECT i.id, a.name FROM app_agent_bindings b JOIN app_installs i ON i.id=b.install_id JOIN apps a ON a.id=i.app_id WHERE b.agent_id=? AND b.enabled=1 ORDER BY a.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sdk.RuntimeAgentCapability{}
	for rows.Next() {
		var installID int64
		var appName string
		if rows.Scan(&installID, &appName) != nil {
			continue
		}
		entry := s.installedApps.Get(installID)
		if entry == nil || entry.SidecarURL == "" {
			continue
		}
		token, err := s.appInstallToken(installID)
		if err != nil {
			continue
		}
		tools, err := listAppMCPTools(entry.SidecarURL+"/mcp", token)
		if err != nil {
			return nil, err
		}
		converted := make([]sdk.RuntimeMCPTool, 0, len(tools))
		for _, tool := range tools {
			converted = append(converted, sdk.RuntimeMCPTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
		}
		out = append(out, sdk.RuntimeAgentCapability{AppName: appName, InstallID: installID, Tools: converted})
	}
	return out, rows.Err()
}

func runtimeCatalogAgent(agent *Agent) sdk.RuntimeCatalogAgent {
	return sdk.RuntimeCatalogAgent{ID: agent.ID, Name: agent.Name, Directive: agent.Directive, DirectiveETag: directiveETag(agent.Directive), Mode: agent.Mode, Status: agent.Status, ProjectID: agent.ProjectID}
}

func directiveETag(directive string) string {
	sum := sha256.Sum256([]byte(directive))
	return hex.EncodeToString(sum[:])
}

func (s *Server) updateAgentDirectiveFromApp(agent *Agent, installID, userID int64, req sdk.AgentDirectiveUpdateRequest) (*Agent, error) {
	if directiveETag(agent.Directive) != req.ExpectedETag {
		return nil, fmt.Errorf("agent directive changed; refresh and retry")
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(agent.Config), &cfg) != nil {
		cfg = map[string]any{}
	}
	cfg["directive"] = req.Directive
	configJSON, _ := json.Marshal(cfg)
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE agents SET directive=?, config=? WHERE id=? AND directive=?`, req.Directive, string(configJSON), agent.ID, agent.Directive)
	if err != nil {
		return nil, err
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		return nil, fmt.Errorf("agent directive changed; refresh and retry")
	}
	beforeJSON, _ := json.Marshal(agent.Directive)
	afterJSON, _ := json.Marshal(req.Directive)
	_, err = tx.Exec(`INSERT INTO agent_change_history (agent_id, field, before_json, after_json, reason, source_app_install_id, applied_by_user_id) VALUES (?, 'directive', ?, ?, ?, ?, ?)`, agent.ID, string(beforeJSON), string(afterJSON), strings.TrimSpace(req.Reason), installID, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.store.GetAgentByID(agent.ID)
}

func (s *Server) runtimeCallerProject(w http.ResponseWriter, r *http.Request, installID int64, requested string, need ProjectRole) (int64, string, bool) {
	var userID int64
	var installProject string
	if err := s.store.db.QueryRow(`SELECT COALESCE(installed_by,0), COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&userID, &installProject); err != nil || userID == 0 {
		http.Error(w, "install not found", http.StatusUnauthorized)
		return 0, "", false
	}
	projectID := strings.TrimSpace(requested)
	if installProject != "" {
		if projectID != "" && projectID != installProject {
			http.Error(w, "app install is scoped to another project", http.StatusForbidden)
			return 0, "", false
		}
		projectID = installProject
	}
	if projectID == "" {
		http.Error(w, "project_id required for global app installs", http.StatusBadRequest)
		return 0, "", false
	}
	// authMiddleware normally provides this same user id. Use the install owner
	// directly as the source of truth and verify membership without trusting a
	// caller-supplied header.
	if s.store.GetPlatformRole(userID) != PlatformAdmin {
		role, err := s.store.GetProjectRole(projectID, userID)
		if err != nil || role.Rank() < need.Rank() {
			http.Error(w, "insufficient role on project", http.StatusForbidden)
			return 0, "", false
		}
	}
	return userID, projectID, true
}

func (s *Server) requireRuntimePermission(w http.ResponseWriter, installID int64, allowed ...sdk.Permission) bool {
	for _, permission := range allowed {
		if installHasPermission(s, installID, permission) {
			return true
		}
	}
	names := make([]string, 0, len(allowed))
	for _, permission := range allowed {
		names = append(names, string(permission))
	}
	http.Error(w, "missing permission: "+strings.Join(names, " or "), http.StatusForbidden)
	return false
}

func (s *Server) runtimeOwnedByInstall(id string, installID int64) (*Environment, bool) {
	if s.environments == nil {
		return nil, false
	}
	runtime, ok := s.environments.Get(id)
	return runtime, ok && runtime.OwnerInstallID() != 0 && runtime.OwnerInstallID() == installID
}

func runtimeCoreRequest(ctx context.Context, agent *EnvironmentAgent, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", agent.Port, path), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if agent.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+agent.APIKey)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("core http %d: %s", resp.StatusCode, string(raw))
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return raw, nil
}

func writeRawJSON(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func validRuntimeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func validRuntimeMCPPath(path string) bool {
	u, err := url.Parse(path)
	return err == nil && u.IsAbs() == false && strings.HasPrefix(u.Path, "/") && !strings.Contains(u.Path, "..") && u.Host == ""
}

func randomRuntimeToken(bytesN int) string {
	buf := make([]byte, bytesN)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func (s *Server) runtimeMCPAttachmentURL(runtimeID, token string) string {
	return fmt.Sprintf("http://127.0.0.1:%s/api/runtime-mcp-gateway/%s/%s", s.port, url.PathEscape(runtimeID), url.PathEscape(token))
}

func (s *Server) handleRuntimeMCPGateway(w http.ResponseWriter, r *http.Request) {
	if s.environments == nil || s.installedApps == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/runtime-mcp-gateway/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	runtimeID, _ := url.PathUnescape(parts[0])
	token, _ := url.PathUnescape(parts[1])
	runtime, ok := s.environments.Get(runtimeID)
	if !ok || runtime.OwnerInstallID() == 0 {
		http.NotFound(w, r)
		return
	}
	attachment := runtime.MCPAttachmentByToken(token)
	if attachment == nil {
		http.NotFound(w, r)
		return
	}
	entry := s.installedApps.Get(attachment.InstallID)
	if entry == nil || entry.SidecarURL == "" {
		http.Error(w, "attachment app unavailable", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(entry.SidecarURL)
	if err != nil {
		http.Error(w, "attachment app unavailable", http.StatusBadGateway)
		return
	}
	appToken, err := s.appInstallToken(attachment.InstallID)
	if err != nil {
		http.Error(w, "attachment credential unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.URL.Path = attachment.Path
		req.URL.RawQuery = ""
		if u, err := url.Parse(attachment.Path); err == nil {
			req.URL.Path = u.Path
			req.URL.RawQuery = u.RawQuery
		}
		req.Header.Set("Authorization", "Bearer "+appToken)
	}
	proxy.ServeHTTP(w, r)
}
