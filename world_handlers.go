package main

// world_handlers.go — HTTP surface for test Worlds.
//
// Routes (registered under /api in main.go):
//   GET    /api/worlds                         list live worlds
//   POST   /api/worlds                         create (optionally fork a snapshot)
//   GET    /api/worlds/<id>                    world detail
//   DELETE /api/worlds/<id>                    tear down
//   GET    /api/worlds/<id>/calls              edge recording (for assertions/inspection)
//   GET    /api/worlds/<id>/cassette           the world's cassette JSON
//   POST   /api/worlds/<id>/assert             run assertions, get pass/fail
//   POST   /api/worlds/<id>/snapshot           capture this world's state
//   ANY    /api/worlds/<id>/apps/<name>/...    reverse-proxy to the in-world
//                                              sidecar — the "open in test
//                                              World" backend for panels
//   GET    /api/world-snapshots                list snapshots
//   DELETE /api/world-snapshots/<id>           delete a snapshot
//
// Everything is gated by authMiddleware. Worlds are ephemeral test infra,
// so we don't (yet) enforce per-user ownership beyond requiring a session.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// registerWorldRoutes wires the world endpoints onto the API mux.
func (s *Server) registerWorldRoutes(apiMux *http.ServeMux) {
	apiMux.HandleFunc("/worlds", s.authMiddleware(s.handleWorlds))
	apiMux.HandleFunc("/worlds/", s.authMiddleware(s.handleWorldByID))
	apiMux.HandleFunc("/world-snapshots", s.authMiddleware(s.handleWorldSnapshots))
	apiMux.HandleFunc("/world-snapshots/", s.authMiddleware(s.handleWorldSnapshotByID))
}

type createWorldRequest struct {
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
	SnapshotID          string               `json:"snapshot_id"` // optional: fork from this snapshot
}

type worldSummary struct {
	ID          string                  `json:"id"`
	ProjectID   string                  `json:"project_id"`
	Mode        EdgeMode                `json:"mode"`
	ProxyURL    string                  `json:"proxy_url"`
	Apps        map[string]worldAppInfo `json:"apps"`
	Connections []worldConnectionInfo   `json:"connections"`
}

type worldAppInfo struct {
	URL       string `json:"url"`
	MCPURL    string `json:"mcp_url"`
	DataDir   string `json:"data_dir"`
	Kind      string `json:"kind"`
	InstallID int64  `json:"install_id,omitempty"`
}

type worldConnectionInfo struct {
	ID        int64  `json:"id"`
	AppSlug   string `json:"app_slug"`
	AppName   string `json:"app_name"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ProjectID string `json:"project_id"`
}

func (s *Server) summarizeWorld(w *World) worldSummary {
	apps := map[string]worldAppInfo{}
	for name, a := range w.Apps() {
		apps[name] = worldAppInfo{URL: a.URL, MCPURL: a.MCPURL, DataDir: a.DataDir, Kind: "legacy"}
	}
	for _, name := range w.InstallNames() {
		if inst, ok := w.Install(name); ok {
			apps[name] = worldAppInfo{
				URL:       inst.SidecarURL,
				MCPURL:    inst.SidecarURL + "/mcp",
				DataDir:   inst.DataDir,
				Kind:      "install",
				InstallID: inst.InstallID,
			}
		}
	}
	return worldSummary{
		ID:          w.ID,
		ProjectID:   w.ProjectID,
		Mode:        w.Mode,
		ProxyURL:    w.ProxyURL(),
		Apps:        apps,
		Connections: s.worldConnectionSummaries(w.ConnectionIDs()),
	}
}

func (s *Server) worldConnectionSummaries(ids []int64) []worldConnectionInfo {
	if len(ids) == 0 || s == nil || s.store == nil {
		return []worldConnectionInfo{}
	}
	out := make([]worldConnectionInfo, 0, len(ids))
	for _, id := range ids {
		var c worldConnectionInfo
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

func (s *Server) worldAppSrcDirsForInstalls(projectID string, installIDs []int64) (map[string]string, error) {
	if len(installIDs) == 0 {
		return nil, nil
	}
	if s.worlds == nil {
		return nil, fmt.Errorf("world manager not configured")
	}
	resolve := s.worlds.ResolveSource
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

func (s *Server) worldVisibleConnectionIDs(userID int64, projectID string, connectionIDs []int64) ([]int64, error) {
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

func (s *Server) handleWorlds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := []worldSummary{}
		for _, world := range s.worlds.List() {
			out = append(out, s.summarizeWorld(world))
		}
		writeJSON(w, out)
	case http.MethodPost:
		var req createWorldRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			req.ID = fmt.Sprintf("world-%d", time.Now().UnixNano())
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
		appSrcDirs, err := s.worldAppSrcDirsForInstalls(req.ProjectID, req.AppInstallIDs)
		if err != nil {
			http.Error(w, "create world: "+err.Error(), http.StatusBadRequest)
			return
		}
		connectionIDs, err := s.worldVisibleConnectionIDs(getUserID(r), req.ProjectID, req.ConnectionIDs)
		if err != nil {
			http.Error(w, "create world: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec := WorldSpec{
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
		var world *World
		if req.SnapshotID != "" {
			world, err = s.worlds.CreateFromSnapshot(spec, req.SnapshotID)
		} else {
			world, err = s.worlds.Create(spec)
		}
		if err != nil {
			http.Error(w, "create world: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.SeedPlan) > 0 {
			if _, err := s.ExecuteSeedPlan(world, req.SeedPlan); err != nil {
				s.worlds.Destroy(world.ID)
				http.Error(w, "seed world: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s.summarizeWorld(world))
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleWorldByID dispatches /worlds/<id>[/<sub>...].
func (s *Server) handleWorldByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/worlds/")
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
	world, ok := s.worlds.Get(id)
	if !ok {
		http.Error(w, "world not found: "+id, http.StatusNotFound)
		return
	}

	// /worlds/<id>/apps/<name>/... — reverse-proxy to the in-world sidecar.
	if strings.HasPrefix(sub, "apps/") {
		s.proxyToWorldApp(w, r, world, strings.TrimPrefix(sub, "apps/"))
		return
	}

	// /worlds/<id>/agent[/...] — spawn / stop / drive the agent copy.
	if sub == "agent" || strings.HasPrefix(sub, "agent/") {
		s.handleWorldAgent(w, r, world, strings.TrimPrefix(strings.TrimPrefix(sub, "agent"), "/"))
		return
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, s.summarizeWorld(world))
		case http.MethodDelete:
			s.worlds.Destroy(id)
			writeJSON(w, map[string]any{"destroyed": id})
		default:
			http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
		}
	case "calls":
		writeJSON(w, world.Edge().Calls())
	case "cassette":
		cas := world.Edge().Cassette()
		if cas == nil {
			writeJSON(w, map[string]any{"entries": []any{}})
			return
		}
		writeJSON(w, cas)
	case "assert":
		s.handleWorldAssert(w, r, world)
	case "snapshot":
		s.handleWorldSnapshot(w, r, world)
	case "seed":
		s.handleWorldSeed(w, r, world)
	default:
		http.NotFound(w, r)
	}
}

type assertRequest struct {
	Assertions   []Assertion `json:"assertions"`
	ToolSequence []string    `json:"tool_sequence"`
}

func (s *Server) handleWorldAssert(w http.ResponseWriter, r *http.Request, world *World) {
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
		EdgeCalls:    world.Edge().Calls(),
		ToolSequence: req.ToolSequence,
		AppDBPath:    world.AppDBPath,
	})
	writeJSON(w, map[string]any{"all_pass": allPass, "results": results})
}

type seedRequest struct {
	Calls []SeedCall `json:"calls"`
}

func (s *Server) handleWorldSeed(w http.ResponseWriter, r *http.Request, world *World) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req seedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	results, err := s.ExecuteSeedPlan(world, req.Calls)
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

func (s *Server) handleWorldSnapshot(w http.ResponseWriter, r *http.Request, world *World) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req snapshotRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.SnapshotID == "" {
		req.SnapshotID = world.ID + "-snap-" + fmt.Sprintf("%d", time.Now().Unix())
	}
	appDirs := map[string]string{}
	for name, a := range world.Apps() {
		if a.DataDir != "" {
			appDirs[name] = a.DataDir
		}
	}
	for _, name := range world.InstallNames() {
		if inst, ok := world.Install(name); ok && inst.DataDir != "" {
			appDirs[name] = inst.DataDir
		}
	}
	man, err := s.worlds.Snapshots().Capture(CaptureSpec{
		ID:          req.SnapshotID,
		ProjectID:   world.ProjectID,
		Description: req.Description,
		AppDataDirs: appDirs,
		Cassette:    world.Edge().Cassette(),
	})
	if err != nil {
		http.Error(w, "snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, man)
}

// proxyToWorldApp reverse-proxies /worlds/<id>/apps/<name>/<tail> to the
// running in-world sidecar, so a dashboard panel can drive the test World's
// app exactly as it drives the production install.
func (s *Server) proxyToWorldApp(w http.ResponseWriter, r *http.Request, world *World, rest string) {
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
	app, ok := world.App(appName)
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
			// Legacy in-world sidecars run in dev mode, so no token is needed.
		}
		proxy.ServeHTTP(w, r)
		return
	}
	inst, ok := world.Install(appName)
	if !ok {
		http.Error(w, "app not in world: "+appName, http.StatusNotFound)
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

type spawnWorldAgentRequest struct {
	SourceAgentID int64  `json:"source_agent_id"`
	Directive     string `json:"directive"`
}

// handleWorldAgent spawns/stops/drives the agent copy in a world.
//
//	POST   /worlds/<id>/agent          spawn (clone source_agent_id)
//	GET    /worlds/<id>/agent          status
//	DELETE /worlds/<id>/agent          stop
//	ANY    /worlds/<id>/agent/<tail>   reverse-proxy to the in-world core
func (s *Server) handleWorldAgent(w http.ResponseWriter, r *http.Request, world *World, tail string) {
	userID := getUserID(r)

	// Drive the running core: proxy /agent/<tail> to its HTTP API.
	if tail != "" {
		wa := world.Agent()
		if wa == nil {
			http.Error(w, "world has no agent — POST /agent first", http.StatusNotFound)
			return
		}
		s.proxyToWorldCore(w, r, wa, tail)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req spawnWorldAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		src, err := s.store.GetAgentByID(req.SourceAgentID)
		if err != nil || src == nil || src.UserID != userID {
			http.Error(w, "source agent not found", http.StatusNotFound)
			return
		}
		wa, err := s.SpawnAgentInWorld(world, WorldAgentSpec{
			UserID:            userID,
			Source:            src,
			DirectiveOverride: req.Directive,
		})
		if err != nil {
			http.Error(w, "spawn world agent: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"agent_id": wa.AgentID, "port": wa.Port})
	case http.MethodGet:
		wa := world.Agent()
		if wa == nil {
			http.Error(w, "no agent in world", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"agent_id": wa.AgentID, "port": wa.Port})
	case http.MethodDelete:
		if wa := world.Agent(); wa != nil {
			wa.Stop()
			world.AttachAgent(nil)
		}
		writeJSON(w, map[string]any{"stopped": true})
	default:
		http.Error(w, "GET, POST or DELETE", http.StatusMethodNotAllowed)
	}
}

// proxyToWorldCore reverse-proxies to the in-world agent core's HTTP API,
// injecting its core API key. Lets the dashboard/tests POST /event and read
// /threads/main/context through the world, exactly as for a live agent.
func (s *Server) proxyToWorldCore(w http.ResponseWriter, r *http.Request, wa *WorldAgent, tail string) {
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

func (s *Server) handleWorldSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.worlds.Snapshots().List()
	if err != nil {
		http.Error(w, "list snapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*SnapshotManifest{}
	}
	writeJSON(w, list)
}

func (s *Server) handleWorldSnapshotByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/world-snapshots/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		man, err := s.worlds.Snapshots().Get(id)
		if err != nil {
			http.Error(w, "snapshot not found: "+id, http.StatusNotFound)
			return
		}
		writeJSON(w, man)
	case http.MethodDelete:
		if err := s.worlds.Snapshots().Delete(id); err != nil {
			http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"deleted": id})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}
