package main

// environment_handlers.go — HTTP surface for test Environments.
//
// Routes (registered under /api in main.go):
//   GET    /api/environments                         list live environments
//   POST   /api/environments                         create (optionally fork a snapshot)
//   GET    /api/environments/<id>                    environment detail
//   DELETE /api/environments/<id>                    tear down
//   GET    /api/environments/<id>/calls              edge recording (for assertions/inspection)
//   GET    /api/environments/<id>/cassette           the environment's cassette JSON
//   POST   /api/environments/<id>/assert             run assertions, get pass/fail
//   POST   /api/environments/<id>/snapshot           capture this environment's state
//   ANY    /api/environments/<id>/apps/<name>/...    reverse-proxy to the in-environment
//                                              sidecar — the "open in test
//                                              Environment" backend for panels
//   GET    /api/environment-snapshots                list snapshots
//   DELETE /api/environment-snapshots/<id>           delete a snapshot
//
// Everything is gated by authMiddleware. Environments are ephemeral test infra,
// so browser/API-key callers are scoped by their normal user/project access.
// Sidecar callers additionally need explicit platform.environments.*
// permissions on their app manifest.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// registerEnvironmentRoutes wires the environment endpoints onto the API mux.
func (s *Server) registerEnvironmentRoutes(apiMux *http.ServeMux) {
	apiMux.HandleFunc("/environments", s.authMiddleware(s.handleEnvironments))
	apiMux.HandleFunc("/environments/", s.authMiddleware(s.handleEnvironmentByID))
	apiMux.HandleFunc("/environment-snapshots", s.authMiddleware(s.handleEnvironmentSnapshots))
	apiMux.HandleFunc("/environment-snapshots/", s.authMiddleware(s.handleEnvironmentSnapshotByID))
}

type createEnvironmentRequest struct {
	ID                  string                        `json:"id"`
	ProjectID           string                        `json:"project_id"`
	GatewayURL          string                        `json:"gateway_url"`
	Apps                []string                      `json:"apps"`
	AppInstallIDs       []int64                       `json:"app_install_ids"`
	ConnectionIDs       []int64                       `json:"connection_ids"`
	Mode                string                        `json:"mode"`
	NetworkMode         string                        `json:"network_mode"`
	IntegrationMode     string                        `json:"integration_mode"`
	AllowSuffixes       []string                      `json:"allow_suffixes"`
	Mocks               []HTTPMock                    `json:"mocks"`
	IntegrationFixtures []IntegrationFixture          `json:"integration_fixtures"`
	IntegrationBindings []RuntimeIntegrationBinding    `json:"integration_bindings"`
	Subscriptions       []EnvironmentSubscriptionSpec `json:"subscriptions"`
	SeedPlan            []SeedCall                    `json:"seed_plan"`
	SeedBaseDir         string                        `json:"seed_base_dir"`
	SnapshotID          string                        `json:"snapshot_id"` // optional: fork from this snapshot
	Ephemeral           bool                          `json:"ephemeral"`
	Autostart           *bool                         `json:"autostart"`
}

type environmentSummary struct {
	ID              string                        `json:"id"`
	ProjectID       string                        `json:"project_id"`
	Mode            EdgeMode                      `json:"mode"` // legacy alias for network_mode
	NetworkMode     EdgeMode                      `json:"network_mode"`
	IntegrationMode string                        `json:"integration_mode"`
	Status          string                        `json:"status"`
	Persisted       bool                          `json:"persisted"`
	Ephemeral       bool                          `json:"ephemeral"`
	ErrorMessage    string                        `json:"error_message,omitempty"`
	ProxyURL        string                        `json:"proxy_url"`
	Apps            map[string]environmentAppInfo `json:"apps"`
	Connections     []environmentConnectionInfo   `json:"connections"`
	Agents          []environmentAgentInfo        `json:"agents"`
	Subscriptions   []environmentSubscriptionInfo `json:"subscriptions"`
}

type environmentAppInfo struct {
	URL       string           `json:"url"`
	MCPURL    string           `json:"mcp_url"`
	DataDir   string           `json:"data_dir"`
	Kind      string           `json:"kind"`
	InstallID int64            `json:"install_id,omitempty"`
	Bindings  map[string]int64 `json:"bindings,omitempty"`
}

type environmentConnectionInfo struct {
	ID        int64  `json:"id"`
	AppSlug   string `json:"app_slug"`
	AppName   string `json:"app_name"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ProjectID string `json:"project_id"`
	Logo      string `json:"logo,omitempty"`
}

type environmentAgentInfo struct {
	AgentID       int64     `json:"agent_id"`
	SourceAgentID int64     `json:"source_agent_id"`
	SourceName    string    `json:"source_name"`
	Alias         string    `json:"alias"`
	Status        string    `json:"status"`
	Port          int       `json:"port"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Server) summarizeEnvironment(w *Environment) environmentSummary {
	apps := map[string]environmentAppInfo{}
	for name, a := range w.Apps() {
		apps[name] = environmentAppInfo{URL: a.URL, MCPURL: a.MCPURL, DataDir: a.DataDir, Kind: "legacy"}
	}
	for _, name := range w.InstallNames() {
		if inst, ok := w.Install(name); ok {
			apps[name] = environmentAppInfo{
				URL:       inst.SidecarURL,
				MCPURL:    inst.SidecarURL + "/mcp",
				DataDir:   inst.DataDir,
				Kind:      "install",
				InstallID: inst.InstallID,
				Bindings:  s.environmentAppBindings(inst.InstallID),
			}
		}
	}
	return environmentSummary{
		ID:              w.ID,
		ProjectID:       w.ProjectID,
		Mode:            w.NetworkMode,
		NetworkMode:     w.NetworkMode,
		IntegrationMode: w.IntegrationMode,
		Status:          "running",
		Ephemeral:       true,
		ProxyURL:        w.ProxyURL(),
		Apps:            apps,
		Connections:     s.environmentConnectionSummaries(w.ConnectionIDs()),
		Agents:          s.summarizeEnvironmentAgents(w.Agents()),
		Subscriptions:   s.environmentSubscriptionInfos(w, nil),
	}
}

func (s *Server) summarizeEnvironmentAgents(agents []*EnvironmentAgent) []environmentAgentInfo {
	out := make([]environmentAgentInfo, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		status := "running"
		if s == nil || s.agents == nil || !s.agents.IsRunning(a.AgentID) {
			status = "stopped"
		}
		out = append(out, environmentAgentInfo{
			AgentID:       a.AgentID,
			SourceAgentID: a.SourceAgentID,
			SourceName:    a.SourceName,
			Alias:         a.Alias,
			Status:        status,
			Port:          a.Port,
			CreatedAt:     a.CreatedAt,
		})
	}
	return out
}

type persistedEnvironmentSpec struct {
	Apps                []string                      `json:"apps,omitempty"`
	AppInstallIDs       []int64                       `json:"app_install_ids,omitempty"`
	ConnectionIDs       []int64                       `json:"connection_ids,omitempty"`
	NetworkMode         EdgeMode                      `json:"network_mode,omitempty"`
	IntegrationMode     string                        `json:"integration_mode,omitempty"`
	AllowSuffixes       []string                      `json:"allow_suffixes,omitempty"`
	Mocks               []HTTPMock                    `json:"mocks,omitempty"`
	IntegrationFixtures []IntegrationFixture          `json:"integration_fixtures,omitempty"`
	Subscriptions       []EnvironmentSubscriptionSpec `json:"subscriptions,omitempty"`
	SeedPlan            []SeedCall                    `json:"seed_plan,omitempty"`
	SeedBaseDir         string                        `json:"seed_base_dir,omitempty"`
	SnapshotID          string                        `json:"snapshot_id,omitempty"`
}

func persistentSpecFromCreateRequest(req createEnvironmentRequest) persistedEnvironmentSpec {
	return persistedEnvironmentSpec{
		Apps:                append([]string(nil), req.Apps...),
		AppInstallIDs:       append([]int64(nil), req.AppInstallIDs...),
		ConnectionIDs:       append([]int64(nil), req.ConnectionIDs...),
		NetworkMode:         normalizeEnvironmentNetworkMode(EdgeMode(req.NetworkMode), EdgeMode(req.Mode)),
		IntegrationMode:     normalizeEnvironmentIntegrationMode(req.IntegrationMode, EdgeMode(req.Mode)),
		AllowSuffixes:       append([]string(nil), req.AllowSuffixes...),
		Mocks:               append([]HTTPMock(nil), req.Mocks...),
		IntegrationFixtures: append([]IntegrationFixture(nil), req.IntegrationFixtures...),
		Subscriptions:       append([]EnvironmentSubscriptionSpec(nil), req.Subscriptions...),
		SeedPlan:            append([]SeedCall(nil), req.SeedPlan...),
		SeedBaseDir:         req.SeedBaseDir,
		SnapshotID:          req.SnapshotID,
	}
}

func decodePersistedEnvironmentSpec(rec *EnvironmentRecord) persistedEnvironmentSpec {
	var spec persistedEnvironmentSpec
	if rec != nil && rec.SpecJSON != "" {
		_ = json.Unmarshal([]byte(rec.SpecJSON), &spec)
	}
	return spec
}

func (s *Server) summarizeLiveEnvironment(environment *Environment, rec *EnvironmentRecord) environmentSummary {
	out := s.summarizeEnvironment(environment)
	if rec != nil {
		out.Persisted = true
		out.Ephemeral = false
		out.ErrorMessage = rec.ErrorMessage
	}
	return out
}

func (s *Server) summarizePersistedEnvironment(rec EnvironmentRecord) environmentSummary {
	spec := decodePersistedEnvironmentSpec(&rec)
	live, ok := s.environments.Get(rec.ID)
	if ok {
		return s.summarizeLiveEnvironment(live, &rec)
	}
	networkMode := normalizeEnvironmentNetworkMode(spec.NetworkMode, EdgeMode(rec.Mode))
	integrationMode := normalizeEnvironmentIntegrationMode(spec.IntegrationMode, EdgeMode(rec.Mode))
	apps := map[string]environmentAppInfo{}
	for _, name := range spec.Apps {
		if strings.TrimSpace(name) == "" {
			continue
		}
		apps[name] = environmentAppInfo{Kind: "legacy"}
	}
	for _, info := range s.environmentInstallInfos(spec.AppInstallIDs) {
		apps[info.Name] = environmentAppInfo{Kind: "install", InstallID: info.InstallID, Bindings: s.environmentAppBindings(info.InstallID)}
	}
	status := "stopped"
	if rec.Status == "error" {
		status = "error"
	}
	return environmentSummary{
		ID:              rec.ID,
		ProjectID:       rec.ProjectID,
		Mode:            networkMode,
		NetworkMode:     networkMode,
		IntegrationMode: integrationMode,
		Status:          status,
		Persisted:       true,
		Ephemeral:       false,
		ErrorMessage:    rec.ErrorMessage,
		Apps:            apps,
		Connections:     s.environmentConnectionSummaries(spec.ConnectionIDs),
		Agents:          []environmentAgentInfo{},
		Subscriptions:   s.environmentSubscriptionInfos(nil, &rec),
	}
}

type environmentInstallInfo struct {
	Name      string
	InstallID int64
}

func (s *Server) environmentInstallInfos(ids []int64) []environmentInstallInfo {
	out := []environmentInstallInfo{}
	if s == nil || s.store == nil || len(ids) == 0 {
		return out
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		var name string
		if err := s.store.db.QueryRow(
			`SELECT a.name FROM app_installs i JOIN apps a ON a.id = i.app_id WHERE i.id = ?`,
			id,
		).Scan(&name); err == nil {
			out = append(out, environmentInstallInfo{Name: name, InstallID: id})
		}
	}
	return out
}

func (s *Server) environmentAppBindings(installID int64) map[string]int64 {
	if s == nil || s.store == nil || installID <= 0 {
		return nil
	}
	var raw string
	if err := s.store.db.QueryRow(`SELECT COALESCE(integration_bindings, '{}') FROM app_installs WHERE id = ?`, installID).Scan(&raw); err != nil {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil || len(decoded) == 0 {
		return nil
	}
	out := map[string]int64{}
	for role, value := range decoded {
		switch v := value.(type) {
		case float64:
			if v > 0 {
				out[role] = int64(v)
			}
		case int64:
			if v > 0 {
				out[role] = v
			}
		case int:
			if v > 0 {
				out[role] = int64(v)
			}
		case string:
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
				out[role] = parsed
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) environmentConnectionSummaries(ids []int64) []environmentConnectionInfo {
	if len(ids) == 0 || s == nil || s.store == nil {
		return []environmentConnectionInfo{}
	}
	out := make([]environmentConnectionInfo, 0, len(ids))
	for _, id := range ids {
		var c environmentConnectionInfo
		if err := s.store.db.QueryRow(
			`SELECT id, app_slug, app_name, name, status, COALESCE(project_id, '')
			 FROM connections WHERE id = ?`,
			id,
		).Scan(&c.ID, &c.AppSlug, &c.AppName, &c.Name, &c.Status, &c.ProjectID); err == nil {
			if s.catalog != nil {
				if app := s.catalog.Get(c.AppSlug); app != nil && app.Logo != nil {
					c.Logo = *app.Logo
				}
			}
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) environmentAppSrcDirsForInstalls(projectID string, installIDs []int64) (map[string]string, error) {
	if len(installIDs) == 0 {
		return nil, nil
	}
	if s.environments == nil {
		return nil, fmt.Errorf("environment manager not configured")
	}
	resolve := s.environments.ResolveSource
	if resolve == nil {
		resolve = defaultSourceResolver
	}
	out := map[string]string{}
	seen := map[int64]bool{}
	for _, installID := range installIDs {
		if installID <= 0 || seen[installID] {
			continue
		}
		seen[installID] = true
		var name, installProject, status, binPath string
		if err := s.store.db.QueryRow(
			`SELECT a.name, COALESCE(i.project_id, ''), i.status, COALESCE(i.local_bin_path, '')
			 FROM app_installs i JOIN apps a ON a.id = i.app_id
			 WHERE i.id = ?`,
			installID,
		).Scan(&name, &installProject, &status, &binPath); err != nil {
			return nil, fmt.Errorf("app install %d not found", installID)
		}
		if status != "running" {
			return nil, fmt.Errorf("app install %d (%s) is %s, not running", installID, name, status)
		}
		if projectID != "" && installProject != "" && installProject != projectID {
			return nil, fmt.Errorf("app install %d (%s) is scoped to another project", installID, name)
		}
		if dir := cachedInstallSourceDir(name, binPath); dir != "" {
			out[name] = dir
			continue
		}
		dir, err := resolve(name)
		if err != nil {
			return nil, fmt.Errorf("resolve source for app install %d (%s): %w", installID, name, err)
		}
		out[name] = dir
	}
	return out, nil
}

func cachedInstallSourceDir(name, binPath string) string {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(binPath) == "" {
		return ""
	}
	versionDir := filepath.Dir(binPath)
	candidates := []string{
		filepath.Join(versionDir, "src", "mcp", name),
		filepath.Join(versionDir, "src"),
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(filepath.Join(dir, "apteva.yaml")); err == nil && !fi.IsDir() {
			if abs, aerr := filepath.Abs(dir); aerr == nil {
				return abs
			}
			return dir
		}
	}
	return ""
}

func (s *Server) environmentVisibleConnectionIDs(userID int64, projectID string, connectionIDs []int64) ([]int64, error) {
	if len(connectionIDs) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(connectionIDs))
	seen := map[int64]bool{}
	for _, cid := range connectionIDs {
		if cid <= 0 || seen[cid] {
			continue
		}
		seen[cid] = true
		conn, _, err := s.store.GetConnection(userID, cid)
		if err != nil || conn == nil {
			return nil, fmt.Errorf("connection %d not found", cid)
		}
		if conn.Status == "disabled" {
			return nil, fmt.Errorf("connection %d is disabled", cid)
		}
		if projectID != "" && conn.ProjectID != "" && conn.ProjectID != projectID {
			return nil, fmt.Errorf("connection %d is scoped to another project", cid)
		}
		out = append(out, cid)
	}
	return out, nil
}

func (s *Server) listEnvironmentSummaries(userID int64) []environmentSummary {
	out := []environmentSummary{}
	seen := map[string]bool{}
	if s != nil && s.store != nil {
		if records, err := s.store.ListEnvironmentRecords(userID); err == nil {
			for _, rec := range records {
				out = append(out, s.summarizePersistedEnvironment(rec))
				seen[rec.ID] = true
			}
		}
	}
	for _, environment := range s.environments.List() {
		if seen[environment.ID] {
			continue
		}
		out = append(out, s.summarizeEnvironment(environment))
	}
	return out
}

func (s *Server) createPersistentEnvironment(req createEnvironmentRequest, userID int64) (environmentSummary, error) {
	if s == nil || s.store == nil {
		return environmentSummary{}, fmt.Errorf("store not configured")
	}
	if _, exists := s.environments.Get(req.ID); exists {
		return environmentSummary{}, fmt.Errorf("environment %q already exists", req.ID)
	}
	if _, err := s.store.GetEnvironmentRecord(req.ID); err == nil {
		return environmentSummary{}, fmt.Errorf("environment %q already exists", req.ID)
	}
	subscriptions, err := normalizeEnvironmentSubscriptionSpecs(req.Subscriptions)
	if err != nil {
		return environmentSummary{}, err
	}
	req.Subscriptions = subscriptions
	spec := persistentSpecFromCreateRequest(req)
	specJSON, _ := json.Marshal(spec)
	networkMode := normalizeEnvironmentNetworkMode(EdgeMode(req.NetworkMode), EdgeMode(req.Mode))
	integrationMode := normalizeEnvironmentIntegrationMode(req.IntegrationMode, EdgeMode(req.Mode))
	req.NetworkMode = string(networkMode)
	req.IntegrationMode = integrationMode
	rec := EnvironmentRecord{
		ID:        req.ID,
		ProjectID: req.ProjectID,
		Name:      req.ID,
		Mode:      string(networkMode),
		Status:    "stopped",
		SpecJSON:  string(specJSON),
		CreatedBy: userID,
	}
	if err := s.store.CreateEnvironmentRecord(rec); err != nil {
		return environmentSummary{}, err
	}
	created, err := s.store.GetEnvironmentRecord(req.ID)
	if err != nil {
		return environmentSummary{}, err
	}
	autostart := true
	if req.Autostart != nil {
		autostart = *req.Autostart
	}
	if !autostart {
		return s.summarizePersistedEnvironment(*created), nil
	}
	environment, err := s.startPersistentEnvironment(*created, userID)
	if err != nil {
		_ = s.store.UpdateEnvironmentRecordStatus(req.ID, "error", err.Error())
		return environmentSummary{}, err
	}
	running, _ := s.store.GetEnvironmentRecord(req.ID)
	return s.summarizeLiveEnvironment(environment, running), nil
}

func (s *Server) startPersistentEnvironment(rec EnvironmentRecord, userID int64) (*Environment, error) {
	if live, ok := s.environments.Get(rec.ID); ok {
		_ = s.store.UpdateEnvironmentRecordStatus(rec.ID, "running", "")
		return live, nil
	}
	spec := decodePersistedEnvironmentSpec(&rec)
	req := createEnvironmentRequest{
		ID:                  rec.ID,
		ProjectID:           rec.ProjectID,
		Apps:                spec.Apps,
		AppInstallIDs:       spec.AppInstallIDs,
		ConnectionIDs:       spec.ConnectionIDs,
		Mode:                rec.Mode,
		NetworkMode:         string(spec.NetworkMode),
		IntegrationMode:     spec.IntegrationMode,
		AllowSuffixes:       spec.AllowSuffixes,
		Mocks:               spec.Mocks,
		IntegrationFixtures: spec.IntegrationFixtures,
		Subscriptions:       spec.Subscriptions,
		SeedPlan:            spec.SeedPlan,
		SeedBaseDir:         spec.SeedBaseDir,
		SnapshotID:          spec.SnapshotID,
	}
	_ = s.store.UpdateEnvironmentRecordStatus(rec.ID, "starting", "")
	environment, err := s.createEnvironmentRuntime(req, userID)
	if err != nil {
		_ = s.store.UpdateEnvironmentRecordStatus(rec.ID, "error", err.Error())
		return nil, err
	}
	_ = s.store.UpdateEnvironmentRecordStatus(rec.ID, "running", "")
	return environment, nil
}

func (s *Server) createEnvironmentRuntime(req createEnvironmentRequest, userID int64) (*Environment, error) {
	gateway := req.GatewayURL
	if gateway == "" {
		gateway = "http://127.0.0.1:" + s.port
	}
	apps := make([]SandboxApp, 0, len(req.Apps))
	for _, name := range req.Apps {
		apps = append(apps, SandboxApp{Name: name})
	}
	appSrcDirs, err := s.environmentAppSrcDirsForInstalls(req.ProjectID, req.AppInstallIDs)
	if err != nil {
		return nil, err
	}
	connectionIDs, err := s.environmentVisibleConnectionIDs(userID, req.ProjectID, req.ConnectionIDs)
	if err != nil {
		return nil, err
	}
	subscriptions, err := normalizeEnvironmentSubscriptionSpecs(req.Subscriptions)
	if err != nil {
		return nil, err
	}
	spec := EnvironmentSpec{
		ID:                  req.ID,
		ProjectID:           req.ProjectID,
		GatewayURL:          gateway,
		Apps:                apps,
		AppSrcDirs:          appSrcDirs,
		Policy:              SandboxPolicy{AllowHostSuffixes: req.AllowSuffixes, Mocks: req.Mocks},
		Mode:                EdgeMode(req.Mode),
		NetworkMode:         normalizeEnvironmentNetworkMode(EdgeMode(req.NetworkMode), EdgeMode(req.Mode)),
		IntegrationMode:     normalizeEnvironmentIntegrationMode(req.IntegrationMode, EdgeMode(req.Mode)),
		IntegrationFixtures: req.IntegrationFixtures,
		Subscriptions:       subscriptions,
		ConnectionIDs:       connectionIDs,
	}
	var environment *Environment
	if req.SnapshotID != "" {
		environment, err = s.environments.CreateFromSnapshot(spec, req.SnapshotID)
	} else {
		environment, err = s.environments.Create(spec)
	}
	if err != nil {
		return nil, err
	}
	if len(req.IntegrationBindings) > 0 {
		if err := s.bindEnvironmentIntegrationMocks(userID, environment, req.IntegrationBindings); err != nil {
			s.environments.Destroy(environment.ID)
			return nil, fmt.Errorf("bind environment integration mocks: %w", err)
		}
	}
	if len(req.SeedPlan) > 0 {
		if _, err := s.ExecuteSeedPlanWithBaseDir(environment, req.SeedPlan, req.SeedBaseDir); err != nil {
			log.Printf("[ENVIRONMENT] seed failed env=%s err=%v edge_calls=%s",
				environment.ID, err, summarizeEnvironmentEdgeCalls(environment.Edge().Calls()))
			s.environments.Destroy(environment.ID)
			return nil, fmt.Errorf("seed environment: %w", err)
		}
	}
	return environment, nil
}

func summarizeEnvironmentEdgeCalls(calls []InterceptedCall) string {
	if len(calls) == 0 {
		return "none"
	}
	const maxCalls = 6
	parts := make([]string, 0, min(len(calls), maxCalls))
	for i, call := range calls {
		if i >= maxCalls {
			parts = append(parts, fmt.Sprintf("...+%d", len(calls)-maxCalls))
			break
		}
		disposition := "seen"
		switch {
		case call.Blocked:
			disposition = "blocked"
		case call.Mocked:
			disposition = "mocked"
		case call.Recorded:
			disposition = "recorded"
		case call.Allowed:
			disposition = "allowed"
		}
		target := call.Host + call.Path
		parts = append(parts, fmt.Sprintf("%s %s status=%d %s", call.Method, target, call.Status, disposition))
	}
	return strings.Join(parts, "; ")
}

func (s *Server) handleEnvironments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage) {
			return
		}
		out := s.listEnvironmentSummaries(getUserID(r))
		writeJSON(w, out)
	case http.MethodPost:
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsManage) {
			return
		}
		var req createEnvironmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			req.ID = fmt.Sprintf("env-%d", time.Now().UnixNano())
		}
		if req.ProjectID != "" {
			if _, _, ok := s.requireProjectAccess(w, r, req.ProjectID, ProjectEditor); !ok {
				return
			}
		}
		if req.Ephemeral {
			environment, err := s.createEnvironmentRuntime(req, getUserID(r))
			if err != nil {
				http.Error(w, "create environment: "+err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, s.summarizeEnvironment(environment))
			return
		}

		summary, err := s.createPersistentEnvironment(req, getUserID(r))
		if err != nil {
			http.Error(w, "create environment: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, summary)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleEnvironmentByID dispatches /environments/<id>[/<sub>...] and legacy
// /environments/<id>[/<sub>...].
func (s *Server) handleEnvironmentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/environments/")
	if rest == r.URL.Path {
		rest = strings.TrimPrefix(r.URL.Path, "/environments/")
	}
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if id == "migrate-to-app" && sub == "" {
		s.handleLegacyEnvironmentMigration(w, r)
		return
	}
	environment, live := s.environments.Get(id)
	var rec *EnvironmentRecord
	if s.store != nil {
		if got, err := s.store.GetEnvironmentRecord(id); err == nil {
			rec = got
		}
	}
	if !live && rec == nil {
		http.Error(w, "environment not found: "+id, http.StatusNotFound)
		return
	}
	if rec != nil && rec.ProjectID != "" {
		need := ProjectViewer
		if r.Method != http.MethodGet {
			need = ProjectEditor
		}
		if _, _, ok := s.requireProjectAccess(w, r, rec.ProjectID, need); !ok {
			return
		}
	}

	if sub == "start" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if rec == nil {
			http.Error(w, "environment is ephemeral and cannot be restarted", http.StatusBadRequest)
			return
		}
		environment, err := s.startPersistentEnvironment(*rec, getUserID(r))
		if err != nil {
			http.Error(w, "start environment: "+err.Error(), http.StatusBadRequest)
			return
		}
		updated, _ := s.store.GetEnvironmentRecord(id)
		writeJSON(w, s.summarizeLiveEnvironment(environment, updated))
		return
	}

	if sub == "stop" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if live {
			s.environments.Destroy(id)
		}
		if rec != nil {
			_ = s.store.UpdateEnvironmentRecordStatus(id, "stopped", "")
			updated, _ := s.store.GetEnvironmentRecord(id)
			if updated != nil {
				writeJSON(w, s.summarizePersistedEnvironment(*updated))
				return
			}
		}
		writeJSON(w, map[string]any{"stopped": true})
		return
	}

	// /environments/<id>/apps/<name>/... — reverse-proxy to the in-environment sidecar.
	if strings.HasPrefix(sub, "apps/") {
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsCall, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.proxyToEnvironmentApp(w, r, environment, strings.TrimPrefix(sub, "apps/"))
		return
	}

	// /environments/<id>/agents[/...] — spawn / list / stop / drive environment agents.
	if sub == "agents" || strings.HasPrefix(sub, "agents/") {
		if !s.requireEnvironmentAgentPermission(w, r) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentAgents(w, r, environment, strings.TrimPrefix(strings.TrimPrefix(sub, "agents"), "/"))
		return
	}

	if sub == "subscriptions" || strings.HasPrefix(sub, "subscriptions/") {
		s.handleEnvironmentSubscriptions(w, r, environment, rec, id, strings.TrimPrefix(strings.TrimPrefix(sub, "subscriptions"), "/"))
		return
	}

	// /environments/<id>/agent[/...] — legacy default-agent shim.
	if sub == "agent" || strings.HasPrefix(sub, "agent/") {
		if !s.requireEnvironmentAgentPermission(w, r) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentAgent(w, r, environment, strings.TrimPrefix(strings.TrimPrefix(sub, "agent"), "/"))
		return
	}

	if sub == "check" {
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsCall, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentFinalStateChecks(w, r, environment)
		return
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage) {
				return
			}
			if live {
				writeJSON(w, s.summarizeLiveEnvironment(environment, rec))
			} else {
				writeJSON(w, s.summarizePersistedEnvironment(*rec))
			}
		case http.MethodDelete:
			if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsManage) {
				return
			}
			if live {
				s.environments.Destroy(id)
			}
			if rec != nil {
				_ = s.store.DeleteEnvironmentRecord(id)
			}
			writeJSON(w, map[string]any{"destroyed": id})
		default:
			http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
		}
	case "calls":
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		writeJSON(w, environment.Edge().Calls())
	case "cassette":
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		cas := environment.Edge().Cassette()
		if cas == nil {
			writeJSON(w, map[string]any{"entries": []any{}})
			return
		}
		writeJSON(w, cas)
	case "assert":
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentAssert(w, r, environment)
	case "snapshot":
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentSnapshot(w, r, environment)
	case "seed":
		if !s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsCall, sdk.PermEnvironmentsManage) {
			return
		}
		if !live {
			http.Error(w, "environment is not running: "+id, http.StatusConflict)
			return
		}
		s.handleEnvironmentSeed(w, r, environment)
	default:
		http.NotFound(w, r)
	}
}

type legacyEnvironmentImportDefinition struct {
	ID           string                      `json:"id"`
	Name         string                      `json:"name"`
	DesiredState string                      `json:"desired_state"`
	SpecVersion  int                         `json:"spec_version"`
	Spec         legacyEnvironmentImportSpec `json:"spec"`
}

type legacyEnvironmentImportSpec struct {
	Version             int                          `json:"version"`
	TTLSeconds          int                          `json:"ttl_seconds"`
	AppInstallIDs       []int64                      `json:"app_install_ids,omitempty"`
	ConnectionIDs       []int64                      `json:"connection_ids,omitempty"`
	NetworkMode         sdk.RuntimeNetworkMode       `json:"network_mode,omitempty"`
	IntegrationMode     string                       `json:"integration_mode,omitempty"`
	AllowHostSuffixes   []string                     `json:"allow_host_suffixes,omitempty"`
	HTTPMocks           []sdk.RuntimeHTTPMock        `json:"http_mocks,omitempty"`
	IntegrationFixtures []sdk.RuntimeIntegrationMock `json:"integration_fixtures,omitempty"`
	Subscriptions       []sdk.RuntimeSubscription    `json:"subscriptions,omitempty"`
	Seeds               []sdkRuntimeSeedStep         `json:"seeds,omitempty"`
	SnapshotID          string                       `json:"snapshot_id,omitempty"`
}

type sdkRuntimeSeedStep struct {
	App   string         `json:"app"`
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input,omitempty"`
}

func (s *Server) handleLegacyEnvironmentMigration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || strings.TrimSpace(req.ProjectID) == "" {
		http.Error(w, "project_id required", http.StatusBadRequest)
		return
	}
	userID, _, ok := s.requireProjectAccess(w, r, req.ProjectID, ProjectEditor)
	if !ok {
		return
	}
	if s.installedApps == nil {
		http.Error(w, "app registry unavailable", http.StatusServiceUnavailable)
		return
	}
	entry := s.installedApps.GetByNameAndProject("environments", req.ProjectID)
	if entry == nil || entry.SidecarURL == "" {
		http.Error(w, "install and start the Environments app in this project first", http.StatusConflict)
		return
	}
	records, err := s.store.ListEnvironmentRecords(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	definitions := []legacyEnvironmentImportDefinition{}
	warnings := []string{}
	for i := range records {
		rec := records[i]
		if rec.ProjectID != req.ProjectID {
			continue
		}
		old := decodePersistedEnvironmentSpec(&rec)
		appIDs := append([]int64(nil), old.AppInstallIDs...)
		for _, name := range old.Apps {
			if app := s.installedApps.GetByNameAndProject(name, req.ProjectID); app != nil {
				appIDs = append(appIDs, app.InstallID)
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: app %q is no longer installed", rec.ID, name))
			}
		}
		appIDs = uniqueInt64s(appIDs)
		httpMocks := make([]sdk.RuntimeHTTPMock, 0, len(old.Mocks))
		for _, mock := range old.Mocks {
			httpMocks = append(httpMocks, sdk.RuntimeHTTPMock{Host: mock.Host, Path: mock.Path, Method: mock.Method, Status: mock.Status, Headers: mock.Headers, Body: mock.Body})
		}
		fixtures := make([]sdk.RuntimeIntegrationMock, 0, len(old.IntegrationFixtures))
		for _, fixture := range old.IntegrationFixtures {
			fixtures = append(fixtures, sdk.RuntimeIntegrationMock{App: fixture.App, Tool: fixture.Tool, Status: fixture.Status, Data: fixture.Data})
		}
		subscriptions := make([]sdk.RuntimeSubscription, 0, len(old.Subscriptions))
		for _, sub := range old.Subscriptions {
			subscriptions = append(subscriptions, sdk.RuntimeSubscription{ID: sub.ID, Source: sub.Source, App: sub.App, Topic: sub.Topic, TargetAgentAlias: sub.TargetAgentAlias, ThreadID: sub.ThreadID, Name: sub.Name, Description: sub.Description, Enabled: sub.Enabled})
		}
		seeds := make([]sdkRuntimeSeedStep, 0, len(old.SeedPlan))
		for _, seed := range old.SeedPlan {
			seeds = append(seeds, sdkRuntimeSeedStep{App: seed.App, Tool: seed.Tool, Input: seed.Input})
			if seed.File != "" {
				warnings = append(warnings, fmt.Sprintf("%s: seed file %q requires manual conversion", rec.ID, seed.File))
			}
		}
		definitions = append(definitions, legacyEnvironmentImportDefinition{ID: rec.ID, Name: rec.Name, DesiredState: "stopped", SpecVersion: 1, Spec: legacyEnvironmentImportSpec{Version: 1, TTLSeconds: 3600, AppInstallIDs: appIDs, ConnectionIDs: old.ConnectionIDs, NetworkMode: sdk.RuntimeNetworkMode(old.NetworkMode), IntegrationMode: old.IntegrationMode, AllowHostSuffixes: old.AllowSuffixes, HTTPMocks: httpMocks, IntegrationFixtures: fixtures, Subscriptions: subscriptions, Seeds: seeds, SnapshotID: old.SnapshotID}})
	}
	payload := map[string]any{"migration_id": "server-environments-v1:" + req.ProjectID, "definitions": definitions}
	body, _ := json.Marshal(payload)
	call, err := http.NewRequestWithContext(r.Context(), http.MethodPost, entry.SidecarURL+"/api/import/legacy", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Authorization", "Bearer "+entry.Token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(call)
	if err != nil {
		http.Error(w, "call Environments app: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var appResult map[string]any
	if json.NewDecoder(resp.Body).Decode(&appResult) != nil {
		appResult = map[string]any{"status": resp.Status}
	}
	if resp.StatusCode >= 300 {
		writeJSONStatus(w, resp.StatusCode, appResult)
		return
	}
	ownedSnapshots := 0
	if s.environments != nil {
		if snapshots, listErr := s.environments.Snapshots().List(); listErr == nil {
			for _, snapshot := range snapshots {
				if snapshot.ProjectID == req.ProjectID && snapshot.OwnerInstallID == 0 {
					if s.environments.Snapshots().AssignOwner(snapshot.ID, req.ProjectID, entry.InstallID) == nil {
						ownedSnapshots++
					}
				}
			}
		}
	}
	writeJSON(w, map[string]any{"app": appResult, "definitions_seen": len(definitions), "snapshots_assigned": ownedSnapshots, "warnings": warnings})
}

func uniqueInt64s(in []int64) []int64 {
	out := make([]int64, 0, len(in))
	seen := map[int64]bool{}
	for _, id := range in {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) requireEnvironmentAgentPermission(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsRead, sdk.PermEnvironmentsManage)
	case http.MethodPost, http.MethodDelete:
		return s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsManage)
	default:
		// Proxying into an environment agent core can inject events or
		// call tools, so treat non-lifecycle methods as call/manage.
		return s.requireEnvironmentPermission(w, r, sdk.PermEnvironmentsCall, sdk.PermEnvironmentsManage)
	}
}

func (s *Server) requireEnvironmentPermission(w http.ResponseWriter, r *http.Request, allowed ...sdk.Permission) bool {
	installID, err := requireInstallID(r)
	if err != nil || installID <= 0 {
		return true
	}
	for _, perm := range allowed {
		if installHasPermission(s, installID, perm) {
			return true
		}
	}
	names := make([]string, 0, len(allowed))
	for _, perm := range allowed {
		names = append(names, string(perm))
	}
	http.Error(w, "missing permission: "+strings.Join(names, " or "), http.StatusForbidden)
	return false
}

type assertRequest struct {
	Assertions   []Assertion `json:"assertions"`
	ToolSequence []string    `json:"tool_sequence"`
}

func (s *Server) handleEnvironmentAssert(w http.ResponseWriter, r *http.Request, environment *Environment) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req assertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	results, allPass := EvaluateAssertions(req.Assertions, AssertionInputs{
		EdgeCalls:    environment.Edge().Calls(),
		ToolSequence: req.ToolSequence,
		AppDBPath:    environment.AppDBPath,
	})
	writeJSON(w, map[string]any{"all_pass": allPass, "results": results})
}

// handleEnvironmentFinalStateChecks evaluates the same read-only contracts
// used by benchmark runs, without exposing app data directories or SQL.
func (s *Server) handleEnvironmentFinalStateChecks(w http.ResponseWriter, r *http.Request, environment *Environment) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Checks []EnvironmentStateCheck `json:"checks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	verdict, err := s.evaluateEnvironmentStateChecks(environment, req.Checks)
	if err != nil {
		http.Error(w, "evaluate final state: "+err.Error(), http.StatusBadRequest)
		return
	}
	if verdict == nil {
		verdict = &DeterministicVerdict{Overall: "pass", Checks: []DeterministicCheckResult{}}
	}
	writeJSON(w, verdict)
}

type seedRequest struct {
	Calls       []SeedCall `json:"calls"`
	SeedBaseDir string     `json:"seed_base_dir"`
}

func (s *Server) handleEnvironmentSeed(w http.ResponseWriter, r *http.Request, environment *Environment) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req seedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	results, err := s.ExecuteSeedPlanWithBaseDir(environment, req.Calls, req.SeedBaseDir)
	if err != nil {
		http.Error(w, "seed: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"results": results})
}

func (s *Server) handleEnvironmentSubscriptions(w http.ResponseWriter, r *http.Request, environment *Environment, rec *EnvironmentRecord, environmentID, tail string) {
	userID := getUserID(r)
	if tail == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, s.environmentSubscriptionInfos(environment, rec))
		case http.MethodPost:
			var req EnvironmentSubscriptionSpec
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			spec, err := normalizeEnvironmentSubscriptionSpec(req)
			if err != nil {
				http.Error(w, "subscription: "+err.Error(), http.StatusBadRequest)
				return
			}
			if environment != nil {
				environment.AddSubscriptionSpec(spec)
				if wa := environment.AgentByAlias(spec.TargetAgentAlias); wa != nil {
					if err := s.installEnvironmentSubscription(userID, environment, wa, spec); err != nil {
						http.Error(w, "install subscription: "+err.Error(), http.StatusBadRequest)
						return
					}
					if s.appEventDispatcher != nil {
						_ = s.appEventDispatcher.Reconcile()
					}
				}
			}
			if rec != nil {
				if err := s.upsertPersistedEnvironmentSubscription(rec, spec); err != nil {
					http.Error(w, "persist subscription: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if updated, err := s.store.GetEnvironmentRecord(rec.ID); err == nil {
					rec = updated
				}
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, s.environmentSubscriptionInfos(environment, rec))
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}

	id := strings.Trim(tail, "/")
	if id == "" {
		http.Error(w, "subscription id required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		var removedSpec *EnvironmentSubscriptionSpec
		specs := []EnvironmentSubscriptionSpec{}
		if environment != nil {
			specs = environment.SubscriptionSpecs()
		} else if rec != nil {
			specs = decodePersistedEnvironmentSpec(rec).Subscriptions
		}
		for _, spec := range specs {
			if spec.ID == id {
				copySpec := spec
				removedSpec = &copySpec
				break
			}
		}
		if environment != nil {
			environment.RemoveSubscriptionSpec(id)
		}
		if rec != nil {
			if err := s.removePersistedEnvironmentSubscription(rec, id); err != nil {
				http.Error(w, "persist subscription: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if removedSpec != nil {
			_ = s.deleteEnvironmentSubscriptionRowsForSpec(environmentID, *removedSpec)
		} else {
			_ = s.deleteEnvironmentSubscriptionRow(environmentID, id)
		}
		writeJSON(w, map[string]any{"deleted": id})
	default:
		http.Error(w, "DELETE", http.StatusMethodNotAllowed)
	}
}

func (s *Server) upsertPersistedEnvironmentSubscription(rec *EnvironmentRecord, spec EnvironmentSubscriptionSpec) error {
	if s == nil || s.store == nil || rec == nil {
		return nil
	}
	persisted := decodePersistedEnvironmentSpec(rec)
	persisted.Subscriptions = upsertEnvironmentSubscriptionSpec(persisted.Subscriptions, spec)
	b, _ := json.Marshal(persisted)
	return s.store.UpdateEnvironmentRecordSpec(rec.ID, string(b))
}

func (s *Server) removePersistedEnvironmentSubscription(rec *EnvironmentRecord, id string) error {
	if s == nil || s.store == nil || rec == nil {
		return nil
	}
	persisted := decodePersistedEnvironmentSpec(rec)
	next := persisted.Subscriptions[:0]
	for _, spec := range persisted.Subscriptions {
		if spec.ID == id {
			continue
		}
		next = append(next, spec)
	}
	persisted.Subscriptions = next
	b, _ := json.Marshal(persisted)
	return s.store.UpdateEnvironmentRecordSpec(rec.ID, string(b))
}

type snapshotRequest struct {
	SnapshotID  string `json:"snapshot_id"`
	Description string `json:"description"`
}

func (s *Server) handleEnvironmentSnapshot(w http.ResponseWriter, r *http.Request, environment *Environment) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req snapshotRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.SnapshotID == "" {
		req.SnapshotID = environment.ID + "-snap-" + fmt.Sprintf("%d", time.Now().Unix())
	}
	appDirs := map[string]string{}
	for name, a := range environment.Apps() {
		if a.DataDir != "" {
			appDirs[name] = a.DataDir
		}
	}
	for _, name := range environment.InstallNames() {
		if inst, ok := environment.Install(name); ok && inst.DataDir != "" {
			appDirs[name] = inst.DataDir
		}
	}
	man, err := s.environments.Snapshots().Capture(CaptureSpec{
		ID:            req.SnapshotID,
		ProjectID:     environment.ProjectID,
		Description:   req.Description,
		AppDataDirs:   appDirs,
		Cassette:      environment.Edge().Cassette(),
		Subscriptions: environment.SubscriptionSpecs(),
	})
	if err != nil {
		http.Error(w, "snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, man)
}

// proxyToEnvironmentApp reverse-proxies /environments/<id>/apps/<name>/<tail> to the
// running in-environment sidecar, so a dashboard panel can drive the test Environment's
// app exactly as it drives the production install.
func (s *Server) proxyToEnvironmentApp(w http.ResponseWriter, r *http.Request, environment *Environment, rest string) {
	if rest == "" {
		http.Error(w, "app name required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	appName := parts[0]
	tail := "/"
	if len(parts) == 2 {
		tail = "/" + parts[1]
	}
	app, ok := environment.App(appName)
	if ok {
		target, err := url.Parse(app.URL)
		if err != nil {
			http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.URL.Path = tail
			// Legacy in-environment sidecars run in dev mode, so no token is needed.
		}
		proxy.ServeHTTP(w, r)
		return
	}
	inst, ok := environment.Install(appName)
	if !ok {
		http.Error(w, "app not in environment: "+appName, http.StatusNotFound)
		return
	}
	target, err := url.Parse(inst.SidecarURL)
	if err != nil {
		http.Error(w, "invalid sidecar url", http.StatusInternalServerError)
		return
	}
	appToken, err := s.appInstallToken(inst.InstallID)
	if err != nil {
		http.Error(w, "app credential unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = tail
		req.Header.Set("Authorization", "Bearer "+appToken)
	}
	proxy.ServeHTTP(w, r)
}

type spawnEnvironmentAgentRequest struct {
	SourceAgentID int64  `json:"source_agent_id"`
	Directive     string `json:"directive"`
	Alias         string `json:"alias"`
	// Provider and Model pin a disposable environment agent without changing
	// the source agent's configured provider pool.
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	StartPaused bool   `json:"start_paused,omitempty"`
}

// handleEnvironmentAgent spawns/stops/drives the default agent copy in a environment.
//
//	POST   /environments/<id>/agent          spawn (clone source_agent_id)
//	GET    /environments/<id>/agent          status
//	DELETE /environments/<id>/agent          stop
//	ANY    /environments/<id>/agent/<tail>   reverse-proxy to the in-environment core
func (s *Server) handleEnvironmentAgent(w http.ResponseWriter, r *http.Request, environment *Environment, tail string) {
	userID := getUserID(r)

	// Drive the running core: proxy /agent/<tail> to its HTTP API.
	if tail != "" {
		wa := environment.Agent()
		if wa == nil {
			http.Error(w, "environment has no agent — POST /agent first", http.StatusNotFound)
			return
		}
		s.proxyToEnvironmentCore(w, r, wa, tail)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req spawnEnvironmentAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		src, err := s.store.GetAgentByID(req.SourceAgentID)
		if err != nil || src == nil || src.UserID != userID {
			http.Error(w, "source agent not found", http.StatusNotFound)
			return
		}
		wa, err := s.SpawnAgentInEnvironment(environment, EnvironmentAgentSpec{
			UserID:            userID,
			Source:            src,
			DirectiveOverride: req.Directive,
			Alias:             firstNonEmpty(req.Alias, "main"),
			Provider:          req.Provider,
			Model:             req.Model,
			StartPaused:       req.StartPaused,
		})
		if err != nil {
			http.Error(w, "spawn environment agent: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s.summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
	case http.MethodGet:
		wa := environment.Agent()
		if wa == nil {
			http.Error(w, "no agent in environment", http.StatusNotFound)
			return
		}
		writeJSON(w, s.summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
	case http.MethodDelete:
		environment.StopDefaultAgent()
		writeJSON(w, map[string]any{"stopped": true})
	default:
		http.Error(w, "GET, POST or DELETE", http.StatusMethodNotAllowed)
	}
}

// handleEnvironmentAgents is the plural multi-agent API:
//
//	POST   /environments/<id>/agents                  spawn
//	GET    /environments/<id>/agents                  list
//	GET    /environments/<id>/agents/<agent-or-alias> status
//	DELETE /environments/<id>/agents/<agent-or-alias> stop
//	ANY    /environments/<id>/agents/<agent-or-alias>/<tail> proxy to core
func (s *Server) handleEnvironmentAgents(w http.ResponseWriter, r *http.Request, environment *Environment, tail string) {
	userID := getUserID(r)
	if tail == "" {
		switch r.Method {
		case http.MethodPost:
			var req spawnEnvironmentAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			src, err := s.store.GetAgentByID(req.SourceAgentID)
			if err != nil || src == nil || src.UserID != userID {
				http.Error(w, "source agent not found", http.StatusNotFound)
				return
			}
			wa, err := s.SpawnAgentInEnvironment(environment, EnvironmentAgentSpec{
				UserID:            userID,
				Source:            src,
				DirectiveOverride: req.Directive,
				Alias:             req.Alias,
				Provider:          req.Provider,
				Model:             req.Model,
				StartPaused:       req.StartPaused,
			})
			if err != nil {
				http.Error(w, "spawn environment agent: "+err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, s.summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
		case http.MethodGet:
			writeJSON(w, s.summarizeEnvironmentAgents(environment.Agents()))
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.SplitN(tail, "/", 2)
	wa := resolveEnvironmentAgent(environment, parts[0])
	if wa == nil {
		http.Error(w, "environment agent not found: "+parts[0], http.StatusNotFound)
		return
	}
	if len(parts) == 2 {
		if parts[1] == "wait" {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var req sdk.RuntimeAgentWaitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			execution, err := s.waitRuntimeAgent(r.Context(), wa, req)
			if err != nil {
				http.Error(w, "wait environment agent: "+err.Error(), http.StatusBadGateway)
				return
			}
			writeJSON(w, execution)
			return
		}
		s.proxyToEnvironmentCore(w, r, wa, parts[1])
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
	case http.MethodDelete:
		stopped := environment.StopAgent(wa.AgentID)
		writeJSON(w, map[string]any{"stopped": stopped})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}

func resolveEnvironmentAgent(environment *Environment, key string) *EnvironmentAgent {
	if id, err := strconv.ParseInt(key, 10, 64); err == nil {
		return environment.GetAgent(id)
	}
	return environment.AgentByAlias(key)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// proxyToEnvironmentCore reverse-proxies to the in-environment agent core's HTTP API,
// injecting its core API key. Lets the dashboard/tests POST /event and read
// /threads/main/context through the environment, exactly as for a live agent.
func (s *Server) proxyToEnvironmentCore(w http.ResponseWriter, r *http.Request, wa *EnvironmentAgent, tail string) {
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", wa.Port))
	if err != nil {
		http.Error(w, "invalid core url", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = "/" + tail
		if wa.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+wa.APIKey)
		}
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) handleEnvironmentSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.environments.Snapshots().List()
	if err != nil {
		http.Error(w, "list snapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*SnapshotManifest{}
	}
	writeJSON(w, list)
}

func (s *Server) handleEnvironmentSnapshotByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/environment-snapshots/")
	if id == r.URL.Path {
		id = strings.TrimPrefix(r.URL.Path, "/environment-snapshots/")
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		man, err := s.environments.Snapshots().Get(id)
		if err != nil {
			http.Error(w, "snapshot not found: "+id, http.StatusNotFound)
			return
		}
		writeJSON(w, man)
	case http.MethodDelete:
		if err := s.environments.Snapshots().Delete(id); err != nil {
			http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"deleted": id})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}
