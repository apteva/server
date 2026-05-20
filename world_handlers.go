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
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	GatewayURL    string     `json:"gateway_url"`
	Apps          []string   `json:"apps"`
	Mode          string     `json:"mode"`
	AllowSuffixes []string   `json:"allow_suffixes"`
	Mocks         []HTTPMock `json:"mocks"`
	SnapshotID    string     `json:"snapshot_id"` // optional: fork from this snapshot
}

type worldSummary struct {
	ID        string                    `json:"id"`
	ProjectID string                    `json:"project_id"`
	Mode      EdgeMode                  `json:"mode"`
	ProxyURL  string                    `json:"proxy_url"`
	Apps      map[string]worldAppInfo   `json:"apps"`
}

type worldAppInfo struct {
	URL     string `json:"url"`
	MCPURL  string `json:"mcp_url"`
	DataDir string `json:"data_dir"`
}

func summarizeWorld(w *World) worldSummary {
	apps := map[string]worldAppInfo{}
	for name, a := range w.Apps() {
		apps[name] = worldAppInfo{URL: a.URL, MCPURL: a.MCPURL, DataDir: a.DataDir}
	}
	return worldSummary{ID: w.ID, ProjectID: w.ProjectID, Mode: w.Mode, ProxyURL: w.ProxyURL(), Apps: apps}
}

func (s *Server) handleWorlds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := []worldSummary{}
		for _, world := range s.worlds.List() {
			out = append(out, summarizeWorld(world))
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
		gateway := req.GatewayURL
		if gateway == "" {
			gateway = "http://127.0.0.1:" + s.port
		}
		apps := make([]SandboxApp, 0, len(req.Apps))
		for _, name := range req.Apps {
			apps = append(apps, SandboxApp{Name: name})
		}
		spec := WorldSpec{
			ID:         req.ID,
			ProjectID:  req.ProjectID,
			GatewayURL: gateway,
			Apps:       apps,
			Policy:     SandboxPolicy{AllowHostSuffixes: req.AllowSuffixes, Mocks: req.Mocks},
			Mode:       EdgeMode(req.Mode),
		}
		var (
			world *World
			err   error
		)
		if req.SnapshotID != "" {
			world, err = s.worlds.CreateFromSnapshot(spec, req.SnapshotID)
		} else {
			world, err = s.worlds.Create(spec)
		}
		if err != nil {
			http.Error(w, "create world: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, summarizeWorld(world))
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
			writeJSON(w, summarizeWorld(world))
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
	if !ok {
		http.Error(w, "app not in world: "+appName, http.StatusNotFound)
		return
	}
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
		// In-world sidecars run in dev mode (no APTEVA_APP_TOKEN), so no
		// Authorization header is needed — see SpawnSandboxedApp.
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
