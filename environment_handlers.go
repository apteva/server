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
// so we don't (yet) enforce per-user ownership beyond requiring a session.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// registerEnvironmentRoutes wires the environment endpoints onto the API mux.
func (s *Server) registerEnvironmentRoutes(apiMux *http.ServeMux) {
	apiMux.HandleFunc("/environments", s.authMiddleware(s.handleEnvironments))
	apiMux.HandleFunc("/environments/", s.authMiddleware(s.handleEnvironmentByID))
	apiMux.HandleFunc("/environment-snapshots", s.authMiddleware(s.handleEnvironmentSnapshots))
	apiMux.HandleFunc("/environment-snapshots/", s.authMiddleware(s.handleEnvironmentSnapshotByID))
}

type createEnvironmentRequest struct {
	ID                  string               `json:"id"`
	ProjectID           string               `json:"project_id"`
	GatewayURL          string               `json:"gateway_url"`
	Apps                []string             `json:"apps"`
	AppInstallIDs       []int64              `json:"app_install_ids"`
	ConnectionIDs       []int64              `json:"connection_ids"`
	Mode                string               `json:"mode"`
	AllowSuffixes       []string             `json:"allow_suffixes"`
	Mocks               []HTTPMock           `json:"mocks"`
	IntegrationFixtures []IntegrationFixture `json:"integration_fixtures"`
	SeedPlan            []SeedCall           `json:"seed_plan"`
	SeedBaseDir         string               `json:"seed_base_dir"`
	SnapshotID          string               `json:"snapshot_id"` // optional: fork from this snapshot
}

type environmentSummary struct {
	ID          string                        `json:"id"`
	ProjectID   string                        `json:"project_id"`
	Mode        EdgeMode                      `json:"mode"`
	ProxyURL    string                        `json:"proxy_url"`
	Apps        map[string]environmentAppInfo `json:"apps"`
	Connections []environmentConnectionInfo   `json:"connections"`
	Agents      []environmentAgentInfo        `json:"agents"`
}

type environmentAppInfo struct {
	URL       string `json:"url"`
	MCPURL    string `json:"mcp_url"`
	DataDir   string `json:"data_dir"`
	Kind      string `json:"kind"`
	InstallID int64  `json:"install_id,omitempty"`
}

type environmentConnectionInfo struct {
	ID        int64  `json:"id"`
	AppSlug   string `json:"app_slug"`
	AppName   string `json:"app_name"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ProjectID string `json:"project_id"`
}

type environmentAgentInfo struct {
	AgentID       int64     `json:"agent_id"`
	SourceAgentID int64     `json:"source_agent_id"`
	SourceName    string    `json:"source_name"`
	Alias         string    `json:"alias"`
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
			}
		}
	}
	return environmentSummary{
		ID:          w.ID,
		ProjectID:   w.ProjectID,
		Mode:        w.Mode,
		ProxyURL:    w.ProxyURL(),
		Apps:        apps,
		Connections: s.environmentConnectionSummaries(w.ConnectionIDs()),
		Agents:      summarizeEnvironmentAgents(w.Agents()),
	}
}

func summarizeEnvironmentAgents(agents []*EnvironmentAgent) []environmentAgentInfo {
	out := make([]environmentAgentInfo, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		out = append(out, environmentAgentInfo{
			AgentID:       a.AgentID,
			SourceAgentID: a.SourceAgentID,
			SourceName:    a.SourceName,
			Alias:         a.Alias,
			Port:          a.Port,
			CreatedAt:     a.CreatedAt,
		})
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
		var name, installProject, status string
		if err := s.store.db.QueryRow(
			`SELECT a.name, COALESCE(i.project_id, ''), i.status
			 FROM app_installs i JOIN apps a ON a.id = i.app_id
			 WHERE i.id = ?`,
			installID,
		).Scan(&name, &installProject, &status); err != nil {
			return nil, fmt.Errorf("app install %d not found", installID)
		}
		if status != "running" {
			return nil, fmt.Errorf("app install %d (%s) is %s, not running", installID, name, status)
		}
		if projectID != "" && installProject != "" && installProject != projectID {
			return nil, fmt.Errorf("app install %d (%s) is scoped to another project", installID, name)
		}
		dir, err := resolve(name)
		if err != nil {
			return nil, fmt.Errorf("resolve source for app install %d (%s): %w", installID, name, err)
		}
		out[name] = dir
	}
	return out, nil
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

func (s *Server) handleEnvironments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := []environmentSummary{}
		for _, environment := range s.environments.List() {
			out = append(out, s.summarizeEnvironment(environment))
		}
		writeJSON(w, out)
	case http.MethodPost:
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
			http.Error(w, "create environment: "+err.Error(), http.StatusBadRequest)
			return
		}
		connectionIDs, err := s.environmentVisibleConnectionIDs(getUserID(r), req.ProjectID, req.ConnectionIDs)
		if err != nil {
			http.Error(w, "create environment: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec := EnvironmentSpec{
			ID:                  req.ID,
			ProjectID:           req.ProjectID,
			GatewayURL:          gateway,
			Apps:                apps,
			AppSrcDirs:          appSrcDirs,
			Policy:              SandboxPolicy{AllowHostSuffixes: req.AllowSuffixes, Mocks: req.Mocks},
			Mode:                EdgeMode(req.Mode),
			IntegrationFixtures: req.IntegrationFixtures,
			ConnectionIDs:       connectionIDs,
		}
		var environment *Environment
		if req.SnapshotID != "" {
			environment, err = s.environments.CreateFromSnapshot(spec, req.SnapshotID)
		} else {
			environment, err = s.environments.Create(spec)
		}
		if err != nil {
			http.Error(w, "create environment: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.SeedPlan) > 0 {
			if _, err := s.ExecuteSeedPlanWithBaseDir(environment, req.SeedPlan, req.SeedBaseDir); err != nil {
				s.environments.Destroy(environment.ID)
				http.Error(w, "seed environment: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s.summarizeEnvironment(environment))
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
	environment, ok := s.environments.Get(id)
	if !ok {
		http.Error(w, "environment not found: "+id, http.StatusNotFound)
		return
	}

	// /environments/<id>/apps/<name>/... — reverse-proxy to the in-environment sidecar.
	if strings.HasPrefix(sub, "apps/") {
		s.proxyToEnvironmentApp(w, r, environment, strings.TrimPrefix(sub, "apps/"))
		return
	}

	// /environments/<id>/agents[/...] — spawn / list / stop / drive environment agents.
	if sub == "agents" || strings.HasPrefix(sub, "agents/") {
		s.handleEnvironmentAgents(w, r, environment, strings.TrimPrefix(strings.TrimPrefix(sub, "agents"), "/"))
		return
	}

	// /environments/<id>/agent[/...] — legacy default-agent shim.
	if sub == "agent" || strings.HasPrefix(sub, "agent/") {
		s.handleEnvironmentAgent(w, r, environment, strings.TrimPrefix(strings.TrimPrefix(sub, "agent"), "/"))
		return
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, s.summarizeEnvironment(environment))
		case http.MethodDelete:
			s.environments.Destroy(id)
			writeJSON(w, map[string]any{"destroyed": id})
		default:
			http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
		}
	case "calls":
		writeJSON(w, environment.Edge().Calls())
	case "cassette":
		cas := environment.Edge().Cassette()
		if cas == nil {
			writeJSON(w, map[string]any{"entries": []any{}})
			return
		}
		writeJSON(w, cas)
	case "assert":
		s.handleEnvironmentAssert(w, r, environment)
	case "snapshot":
		s.handleEnvironmentSnapshot(w, r, environment)
	case "seed":
		s.handleEnvironmentSeed(w, r, environment)
	default:
		http.NotFound(w, r)
	}
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
		ID:          req.SnapshotID,
		ProjectID:   environment.ProjectID,
		Description: req.Description,
		AppDataDirs: appDirs,
		Cassette:    environment.Edge().Cassette(),
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
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = tail
		req.Header.Set("Authorization", fmt.Sprintf("Bearer dev-%d", inst.InstallID))
	}
	proxy.ServeHTTP(w, r)
}

type spawnEnvironmentAgentRequest struct {
	SourceAgentID int64  `json:"source_agent_id"`
	Directive     string `json:"directive"`
	Alias         string `json:"alias"`
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
		})
		if err != nil {
			http.Error(w, "spawn environment agent: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
	case http.MethodGet:
		wa := environment.Agent()
		if wa == nil {
			http.Error(w, "no agent in environment", http.StatusNotFound)
			return
		}
		writeJSON(w, summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
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
			})
			if err != nil {
				http.Error(w, "spawn environment agent: "+err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
		case http.MethodGet:
			writeJSON(w, summarizeEnvironmentAgents(environment.Agents()))
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
		s.proxyToEnvironmentCore(w, r, wa, parts[1])
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, summarizeEnvironmentAgents([]*EnvironmentAgent{wa})[0])
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
