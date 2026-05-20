package main

// world.go — the World supervisor.
//
// A World is an isolated test environment: a set of real app sidecars
// (each with its own throwaway SQLite data dir, so they do REAL DB writes)
// sharing one WorldEdge that virtualises their outbound HTTP. The agent
// core + meta-agent join the same edge in a later phase; Phase 1 stands up
// the app-sidecar half plus the edge, which is the part that lets us boot a
// coherent multi-app world without modifying any app.
//
// This generalises eval_sandbox.go's one-shot SpawnSandboxedApp pattern
// into a named, long-lived container that can host several sidecars and be
// reset/torn down as a unit. It's a pure addition: nothing here runs unless
// a caller explicitly creates a World, so production agent paths are
// untouched (s.worlds is only ever consulted by world endpoints).
//
// Control-plane vs data-plane: there is ONE shared apteva-server. A World
// only spawns data-plane processes (sidecars now, cores later) + an edge.
// It never forks the server. GatewayURL points the sidecars back at the
// shared server for inter-app/platform calls.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorldSpec declares a World to stand up.
type WorldSpec struct {
	ID         string       // unique id for this world (caller-supplied)
	ProjectID  string       // project scope the in-world apps run under
	GatewayURL string       // shared apteva-server URL sidecars call back to
	Apps       []SandboxApp // app sidecars to boot in-world (BinaryPath optional → resolver)
	Policy     SandboxPolicy // edge allowlist + hand-written mocks
	Mode       EdgeMode      // edge default mode (block | passthrough | record | replay | mock)
	Cassette   *Cassette     // optional preloaded cassette (for replay)

	// HealthBudget bounds how long each sidecar gets to answer /health
	// before the create fails. Defaults to 20s.
	HealthBudget time.Duration
}

// World is a live test environment.
type World struct {
	ID        string
	ProjectID string
	Mode      EdgeMode

	edge      *WorldEdge
	mu        sync.Mutex
	apps      map[string]*SandboxAppInstance
	agent     *WorldAgent // optional: the agent copy running in this world
	createdAt time.Time
}

// AttachAgent records the running agent copy so World.Stop tears it down.
func (w *World) AttachAgent(a *WorldAgent) {
	w.mu.Lock()
	w.agent = a
	w.mu.Unlock()
}

// Agent returns the world's running agent copy, or nil.
func (w *World) Agent() *WorldAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agent
}

// Edge exposes the world's HTTP intercept (for assertions / cassette save).
func (w *World) Edge() *WorldEdge { return w.edge }

// ProxyURL is the world edge's HTTP_PROXY address.
func (w *World) ProxyURL() string { return w.edge.ProxyURL() }

// App returns a running in-world sidecar by manifest name.
func (w *World) App(name string) (*SandboxAppInstance, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.apps[name]
	return a, ok
}

// Apps returns a snapshot of the world's running sidecars.
func (w *World) Apps() map[string]*SandboxAppInstance {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]*SandboxAppInstance, len(w.apps))
	for k, v := range w.apps {
		out[k] = v
	}
	return out
}

// AppDBPath resolves an in-world sidecar's SQLite file (for state
// assertions). SpawnSandboxedApp lays it down at <DataDir>/<name>.db.
func (w *World) AppDBPath(name string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.apps[name]
	if !ok || a.DataDir == "" {
		return "", false
	}
	return filepath.Join(a.DataDir, name+".db"), true
}

// Stop tears the world down: the agent copy, every sidecar, then the edge.
// Idempotent.
func (w *World) Stop() {
	w.mu.Lock()
	apps := w.apps
	agent := w.agent
	w.apps = map[string]*SandboxAppInstance{}
	w.agent = nil
	w.mu.Unlock()
	if agent != nil {
		agent.Stop()
	}
	for _, a := range apps {
		a.Stop()
	}
	if w.edge != nil {
		w.edge.Stop()
	}
}

// WorldManager owns the live set of Worlds. Hung off Server as s.worlds;
// nil-safe — created at boot but only touched by world endpoints.
type WorldManager struct {
	mu        sync.Mutex
	worlds    map[string]*World
	dataDir   string
	snapshots *SnapshotStore

	// ResolveBinary maps an app manifest name to its sidecar binary path
	// when a SandboxApp doesn't carry an explicit BinaryPath. Injectable
	// so prod (LocalSupervisor-known paths) and tests can override.
	// Defaults to defaultBinaryResolver.
	ResolveBinary func(name string) (string, error)
}

// NewWorldManager creates the manager and ensures its data root exists.
func NewWorldManager(dataDir string) *WorldManager {
	_ = os.MkdirAll(dataDir, 0755)
	return &WorldManager{
		worlds:        map[string]*World{},
		dataDir:       dataDir,
		snapshots:     NewSnapshotStore(dataDir),
		ResolveBinary: defaultBinaryResolver,
	}
}

// Snapshots exposes the snapshot store.
func (wm *WorldManager) Snapshots() *SnapshotStore { return wm.snapshots }

// IntegrationFixture answers one (app, tool) integration call in World
// test mode without hitting the real third-party API.
type IntegrationFixture struct {
	App    string `json:"app"`  // AppTemplate.Slug
	Tool   string `json:"tool"` // AppToolDef.Name
	Status int    `json:"status"`
	Data   any    `json:"data"`
}

// InstallIntegrationInterceptor wires the connections.go egress seam to
// answer the given fixtures and PASS EVERYTHING ELSE THROUGH to the real
// API — so it can never mask an unrelated production integration call.
// Returns a remove func the caller must defer. Process-global; see the
// caveat on worldEgressInterceptor (single active intercepting world).
func InstallIntegrationInterceptor(fixtures []IntegrationFixture) (remove func()) {
	idx := make(map[string]IntegrationFixture, len(fixtures))
	for _, f := range fixtures {
		idx[f.App+"\x00"+f.Tool] = f
	}
	worldEgressInterceptor = func(app *AppTemplate, tool *AppToolDef, _ map[string]any) (*ExecuteResult, bool) {
		f, ok := idx[app.Slug+"\x00"+tool.Name]
		if !ok {
			return nil, false // not ours → real call proceeds
		}
		st := f.Status
		if st == 0 {
			st = 200
		}
		return &ExecuteResult{Success: st >= 200 && st < 300, Status: st, Data: f.Data}, true
	}
	return func() { worldEgressInterceptor = nil }
}

// CreateFromSnapshot forks a World from a captured snapshot: it restores
// each app's data dir and the cassette, then boots the sidecars on that
// pre-populated state. This is the eval-run fork path — independent,
// repeatable, starting from a known fixture.
func (wm *WorldManager) CreateFromSnapshot(spec WorldSpec, snapshotID string) (*World, error) {
	appDataDirs, err := wm.snapshots.Restore(snapshotID, "")
	if err != nil {
		return nil, err
	}
	// Seed each declared app with its restored data dir.
	for i := range spec.Apps {
		if dir, ok := appDataDirs[spec.Apps[i].Name]; ok {
			spec.Apps[i].DataDir = dir
		}
	}
	// Replay against the snapshot's recorded externals unless the caller
	// already supplied a cassette / chose a mode.
	if spec.Cassette == nil {
		if cas, cerr := wm.snapshots.Cassette(snapshotID); cerr == nil && cas != nil {
			spec.Cassette = cas
			if spec.Mode == "" {
				spec.Mode = EdgeReplay
			}
		}
	}
	return wm.Create(spec)
}

// Create stands up a World: starts the edge, then spawns each app sidecar
// with HTTP_PROXY pointed at the edge and a throwaway data dir. On any
// failure the partially-built world is torn down so we never leak processes.
func (wm *WorldManager) Create(spec WorldSpec) (*World, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("world: ID required")
	}
	wm.mu.Lock()
	if _, exists := wm.worlds[spec.ID]; exists {
		wm.mu.Unlock()
		return nil, fmt.Errorf("world %q already exists", spec.ID)
	}
	wm.mu.Unlock()

	edge, err := startWorldEdge(spec.Policy, spec.Mode, spec.Cassette)
	if err != nil {
		return nil, err
	}

	budget := spec.HealthBudget
	if budget == 0 {
		budget = 20 * time.Second
	}
	w := &World{
		ID:        spec.ID,
		ProjectID: spec.ProjectID,
		Mode:      edge.mode,
		edge:      edge,
		apps:      map[string]*SandboxAppInstance{},
		createdAt: time.Now(),
	}

	for _, app := range spec.Apps {
		if app.BinaryPath == "" {
			if wm.ResolveBinary == nil {
				w.Stop()
				return nil, fmt.Errorf("world %q: app %q has no BinaryPath and no resolver", spec.ID, app.Name)
			}
			bin, rerr := wm.ResolveBinary(app.Name)
			if rerr != nil {
				w.Stop()
				return nil, fmt.Errorf("world %q: resolve app %q: %w", spec.ID, app.Name, rerr)
			}
			app.BinaryPath = bin
		}
		inst, serr := SpawnSandboxedApp(app, edge.ProxyURL(), spec.GatewayURL, budget)
		if serr != nil {
			w.Stop()
			return nil, fmt.Errorf("world %q: spawn app %q: %w", spec.ID, app.Name, serr)
		}
		w.mu.Lock()
		w.apps[app.Name] = inst
		w.mu.Unlock()
	}

	wm.mu.Lock()
	wm.worlds[spec.ID] = w
	wm.mu.Unlock()
	return w, nil
}

// Get returns a live World by id.
func (wm *WorldManager) Get(id string) (*World, bool) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	w, ok := wm.worlds[id]
	return w, ok
}

// List returns a snapshot of all live Worlds.
func (wm *WorldManager) List() []*World {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	out := make([]*World, 0, len(wm.worlds))
	for _, w := range wm.worlds {
		out = append(out, w)
	}
	return out
}

// Destroy stops and forgets a World. No-op if unknown.
func (wm *WorldManager) Destroy(id string) {
	wm.mu.Lock()
	w, ok := wm.worlds[id]
	if ok {
		delete(wm.worlds, id)
	}
	wm.mu.Unlock()
	if ok && w != nil {
		w.Stop()
	}
}

// StopAll tears down every World — called on server shutdown.
func (wm *WorldManager) StopAll() {
	wm.mu.Lock()
	ws := wm.worlds
	wm.worlds = map[string]*World{}
	wm.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

// defaultBinaryResolver finds a sidecar binary by manifest name. Order:
//   1. APTEVA_APP_BIN_<NAME>           (explicit override; dashes→underscores, upper)
//   2. ~/.apteva/apps/<name>/<name>    (conventional local install)
//   3. ../apps/mcp/<name>/<name> and apps/mcp/<name>/<name>  (dev checkout)
//   4. <name> on PATH
func defaultBinaryResolver(name string) (string, error) {
	envKey := "APTEVA_APP_BIN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if p := os.Getenv(envKey); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".apteva", "apps", name, name))
	}
	candidates = append(candidates,
		filepath.Join("..", "apps", "mcp", name, name),
		filepath.Join("apps", "mcp", name, name),
	)
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			if abs, aerr := filepath.Abs(c); aerr == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no binary found for app %q (set %s or pass BinaryPath)", name, envKey)
}
