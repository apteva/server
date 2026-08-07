package main

// environment.go — the Environment supervisor.
//
// A Environment is an isolated test environment: a set of real app sidecars
// (each with its own throwaway SQLite data dir, so they do REAL DB writes)
// sharing one EnvironmentEdge that virtualises their outbound HTTP. The agent
// core + meta-agent join the same edge in a later phase; Phase 1 stands up
// the app-sidecar half plus the edge, which is the part that lets us boot a
// coherent multi-app environment without modifying any app.
//
// This is a named, long-lived container that can host several sidecars and be
// reset/torn down as a unit. It's a pure addition: nothing here runs unless
// a caller explicitly creates a Environment, so production agent paths are
// untouched (s.environments is only ever consulted by environment endpoints).
//
// Control-plane vs data-plane: there is ONE shared apteva-server. A Environment
// only spawns data-plane processes (sidecars now, cores later) + an edge.
// It never forks the server. GatewayURL points the sidecars back at the
// shared server for inter-app/platform calls.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func environmentDataRoot(dataDir string) string {
	return filepath.Join(dataDir, "environments")
}

// EnvironmentSpec declares a Environment to stand up.
type EnvironmentSpec struct {
	ID         string // unique id for this environment (caller-supplied)
	ProjectID  string // project scope the in-environment apps run under
	GatewayURL string // shared apteva-server URL sidecars call back to
	// Set only by the generic app-facing runtime API. Legacy environment callers
	// leave these zero-valued.
	RuntimeOwnerInstallID int64
	RuntimeExpiresAt      time.Time
	SourceInstallIDs      map[string]int64
	Apps                  []SandboxApp // legacy prebuilt-binary sidecars
	// AppSrcDirs maps app name → local working-copy dir. These are installed
	// via the real install path (installLocalSource) under project_id=ID, so
	// each is a project-scoped REAL install: its callbacks authenticate and
	// inter-app routing resolves. This is the clean Environment path; Apps stays
	// for the legacy prebuilt-binary case.
	AppSrcDirs map[string]string
	// RestoredAppDataDirs maps app name → restored data dir from a snapshot.
	// Used for install-backed apps, where AppSrcDirs controls the app source
	// and this field carries the starting state.
	RestoredAppDataDirs map[string]string
	Policy              SandboxPolicy // edge allowlist + hand-written mocks
	Mode                EdgeMode      // legacy edge mode; prefer NetworkMode
	NetworkMode         EdgeMode      // outbound HTTP mode (passthrough | block | record | replay)
	IntegrationMode     string        // third-party integration mode (mock | real)
	Cassette            *Cassette     // optional preloaded cassette (for replay)

	// IntegrationFixtures answer third-party integration tool calls made by
	// in-environment sidecars (via the platform executor) without hitting the
	// real API. Routed per-environment by id, so concurrent environments don't collide.
	IntegrationFixtures []IntegrationFixture

	// ConnectionIDs are existing integration connections the operator wants
	// exposed to agents spawned into this Environment. The connection rows stay in
	// their real project; environment_id routing makes their tool calls mock-only.
	ConnectionIDs []int64

	// Subscriptions are environment-owned event routes. They are materialised
	// as normal subscriptions rows scoped to project_id=Environment.ID once
	// their target agent alias exists.
	Subscriptions []EnvironmentSubscriptionSpec

	// HealthBudget bounds how long each sidecar gets to answer /health
	// before the create fails. Defaults to 20s.
	HealthBudget time.Duration
}

// Environment is a live test environment.
type Environment struct {
	ID              string
	ProjectID       string
	Mode            EdgeMode // legacy alias for NetworkMode
	NetworkMode     EdgeMode
	IntegrationMode string
	ownerInstallID  int64
	expiresAt       time.Time

	edge              *EnvironmentEdge
	server            *Server // back-ref for real installs + teardown (nil for edge-only environments)
	connectionIDs     []int64
	sourceInstallIDs  map[string]int64
	mu                sync.Mutex
	apps              map[string]*SandboxAppInstance
	installs          map[string]*localInstall    // real installs (AppSrcDirs path), keyed by app name
	agents            map[int64]*EnvironmentAgent // running agent cores in this environment, keyed by transient agent id
	agentAliases      map[string]int64            // stable environment-local alias → agent id
	subscriptions     []EnvironmentSubscriptionSpec
	mcpAttachments    map[string]RuntimeMCPAttachment
	managedMCPs       map[string]*RuntimeManagedMCP
	managedMCPManager *MCPManager
	removeInterceptor func() // unregisters this environment's integration interceptor
	createdAt         time.Time
}

func (w *Environment) OwnerInstallID() int64 { return w.ownerInstallID }
func (w *Environment) ExpiresAt() time.Time  { return w.expiresAt }

func (w *Environment) SourceInstallID(appName string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sourceInstallIDs[appName]
}

func (w *Environment) AddMCPAttachment(a RuntimeMCPAttachment) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.agents) > 0 {
		return fmt.Errorf("attach MCP before spawning runtime agents")
	}
	if w.mcpAttachments == nil {
		w.mcpAttachments = map[string]RuntimeMCPAttachment{}
	}
	if _, exists := w.managedMCPs[a.Name]; exists {
		return fmt.Errorf("runtime already has managed MCP %q", a.Name)
	}
	for _, existing := range w.mcpAttachments {
		if existing.Name == a.Name {
			return fmt.Errorf("runtime already has MCP attachment %q", a.Name)
		}
	}
	w.mcpAttachments[a.ID] = a
	return nil
}

func (w *Environment) MCPAttachments() []RuntimeMCPAttachment {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]RuntimeMCPAttachment, 0, len(w.mcpAttachments))
	for _, a := range w.mcpAttachments {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *Environment) MCPAttachmentByToken(token string) *RuntimeMCPAttachment {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range w.mcpAttachments {
		if a.Token == token {
			copy := a
			return &copy
		}
	}
	return nil
}

func (w *Environment) AddManagedMCP(mcp *RuntimeManagedMCP) error {
	if mcp == nil || mcp.Name == "" {
		return fmt.Errorf("managed MCP name required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.agents) > 0 {
		return fmt.Errorf("attach managed MCP before spawning runtime agents")
	}
	if w.managedMCPs == nil {
		w.managedMCPs = map[string]*RuntimeManagedMCP{}
	}
	for _, attachment := range w.mcpAttachments {
		if attachment.Name == mcp.Name {
			return fmt.Errorf("runtime already has MCP attachment %q", mcp.Name)
		}
	}
	if _, exists := w.managedMCPs[mcp.Name]; exists {
		return fmt.Errorf("runtime already has managed MCP %q", mcp.Name)
	}
	w.managedMCPs[mcp.Name] = mcp
	return nil
}

func (w *Environment) ManagedMCPs() []*RuntimeManagedMCP {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*RuntimeManagedMCP, 0, len(w.managedMCPs))
	for _, mcp := range w.managedMCPs {
		out = append(out, mcp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *Environment) ManagedMCP(name string) *RuntimeManagedMCP {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.managedMCPs[name]
}

func (w *Environment) ManagedMCPByToken(token string) *RuntimeManagedMCP {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, mcp := range w.managedMCPs {
		if mcp.Token == token {
			return mcp
		}
	}
	return nil
}

type environmentAppSource struct {
	Name string
	Dir  string
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

const (
	IntegrationModeMock = "mock"
	IntegrationModeReal = "real"
)

func normalizeEnvironmentNetworkMode(networkMode, legacy EdgeMode) EdgeMode {
	if networkMode != "" {
		return networkMode
	}
	switch legacy {
	case EdgeMock:
		return EdgePassthrough
	case "":
		return EdgePassthrough
	default:
		return legacy
	}
}

func normalizeEnvironmentIntegrationMode(mode string, legacy EdgeMode) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case IntegrationModeReal:
		return IntegrationModeReal
	case IntegrationModeMock:
		return IntegrationModeMock
	}
	return IntegrationModeMock
}

// Install returns a real in-environment install by app name.
func (w *Environment) Install(name string) (*localInstall, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	in, ok := w.installs[name]
	return in, ok
}

// InstallNames lists the real in-environment installs' app names.
func (w *Environment) InstallNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.installs))
	for k := range w.installs {
		out = append(out, k)
	}
	return out
}

// ConnectionIDs returns the existing integration connections exposed to
// agents spawned inside this Environment.
func (w *Environment) ConnectionIDs() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int64, len(w.connectionIDs))
	copy(out, w.connectionIDs)
	return out
}

func (w *Environment) AddConnectionIDs(ids ...int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[int64]struct{}, len(w.connectionIDs)+len(ids))
	for _, id := range w.connectionIDs {
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		w.connectionIDs = append(w.connectionIDs, id)
		seen[id] = struct{}{}
	}
}

// SubscriptionSpecs returns the environment-owned subscription declarations.
func (w *Environment) SubscriptionSpecs() []EnvironmentSubscriptionSpec {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]EnvironmentSubscriptionSpec, len(w.subscriptions))
	copy(out, w.subscriptions)
	return out
}

func (w *Environment) AddSubscriptionSpec(spec EnvironmentSubscriptionSpec) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.subscriptions = upsertEnvironmentSubscriptionSpec(w.subscriptions, spec)
}

func (w *Environment) RemoveSubscriptionSpec(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.subscriptions[:0]
	removed := false
	for _, spec := range w.subscriptions {
		if spec.ID == id {
			removed = true
			continue
		}
		next = append(next, spec)
	}
	w.subscriptions = next
	return removed
}

// AttachAgent records a running agent copy so Environment.Stop tears it down.
func (w *Environment) AttachAgent(a *EnvironmentAgent) error {
	if a == nil {
		return fmt.Errorf("environment agent is nil")
	}
	alias := strings.TrimSpace(a.Alias)
	if alias == "" {
		alias = "main"
		a.Alias = alias
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.agents == nil {
		w.agents = map[int64]*EnvironmentAgent{}
	}
	if w.agentAliases == nil {
		w.agentAliases = map[string]int64{}
	}
	if _, exists := w.agents[a.AgentID]; exists {
		return fmt.Errorf("environment already has agent %d", a.AgentID)
	}
	if existingID, exists := w.agentAliases[alias]; exists && existingID != a.AgentID {
		return fmt.Errorf("environment already has an agent with alias %q", alias)
	}
	w.agents[a.AgentID] = a
	w.agentAliases[alias] = a.AgentID
	return nil
}

// RemoveAgent detaches a environment-agent and returns it for caller teardown.
func (w *Environment) RemoveAgent(agentID int64) *EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[agentID]
	if a == nil {
		return nil
	}
	delete(w.agents, agentID)
	if a.Alias != "" {
		delete(w.agentAliases, a.Alias)
	}
	return a
}

// GetAgent returns a running environment-agent by transient agent id.
func (w *Environment) GetAgent(agentID int64) *EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agents[agentID]
}

// AgentByAlias returns a running environment-agent by environment-local alias.
func (w *Environment) AgentByAlias(alias string) *EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.agentAliases[strings.TrimSpace(alias)]
	if !ok {
		return nil
	}
	return w.agents[id]
}

// DefaultAgent returns the "main" environment-agent if present, otherwise the
// oldest running agent. This preserves the legacy singular /agent route.
func (w *Environment) DefaultAgent() *EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id, ok := w.agentAliases["main"]; ok {
		return w.agents[id]
	}
	var out *EnvironmentAgent
	for _, a := range w.agents {
		if out == nil || a.CreatedAt.Before(out.CreatedAt) || (a.CreatedAt.Equal(out.CreatedAt) && a.AgentID < out.AgentID) {
			out = a
		}
	}
	return out
}

// Agents returns a deterministic snapshot of running environment-agents.
func (w *Environment) Agents() []*EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*EnvironmentAgent, 0, len(w.agents))
	for _, a := range w.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// StopAgent detaches and stops one environment-agent by id.
func (w *Environment) StopAgent(agentID int64) bool {
	a := w.RemoveAgent(agentID)
	if a == nil {
		return false
	}
	a.Stop()
	return true
}

// StopDefaultAgent detaches and stops the legacy default environment-agent.
func (w *Environment) StopDefaultAgent() bool {
	a := w.DefaultAgent()
	if a == nil {
		return false
	}
	return w.StopAgent(a.AgentID)
}

// ClearAgents detaches all environment-agents and returns them for teardown.
func (w *Environment) ClearAgents() []*EnvironmentAgent {
	w.mu.Lock()
	defer w.mu.Unlock()
	agents := make([]*EnvironmentAgent, 0, len(w.agents))
	for _, a := range w.agents {
		agents = append(agents, a)
	}
	w.agents = map[int64]*EnvironmentAgent{}
	w.agentAliases = map[string]int64{}
	return agents
}

// Agent returns the environment's default running agent copy, or nil. It preserves
// the legacy single-agent API while Environments internally support many agents.
func (w *Environment) Agent() *EnvironmentAgent {
	return w.DefaultAgent()
}

// Edge exposes the environment's HTTP intercept (for assertions / cassette save).
func (w *Environment) Edge() *EnvironmentEdge { return w.edge }

// ProxyURL is the environment edge's HTTP_PROXY address.
func (w *Environment) ProxyURL() string { return w.edge.ProxyURL() }

// App returns a running in-environment sidecar by manifest name.
func (w *Environment) App(name string) (*SandboxAppInstance, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.apps[name]
	return a, ok
}

// Apps returns a snapshot of the environment's running sidecars.
func (w *Environment) Apps() map[string]*SandboxAppInstance {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]*SandboxAppInstance, len(w.apps))
	for k, v := range w.apps {
		out[k] = v
	}
	return out
}

// AppDBPath resolves an in-environment app's SQLite file (for state assertions).
// Real installs lay it down at <cacheDir>/<name>/data/<installID>/app.db;
// legacy sandbox sidecars at <DataDir>/<name>.db.
func (w *Environment) AppDBPath(name string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if in, ok := w.installs[name]; ok && in.DBPath != "" {
		return in.DBPath, true
	}
	a, ok := w.apps[name]
	if !ok || a.DataDir == "" {
		return "", false
	}
	return filepath.Join(a.DataDir, name+".db"), true
}

// Stop tears the environment down: the agent copy, real installs, sidecars, the
// edge, and any environment-scoped DB rows. Every DB cleanup is guarded by the
// environment's project id, so it can NEVER touch a production install/connection.
// Idempotent.
func (w *Environment) Stop() {
	w.mu.Lock()
	apps := w.apps
	installs := w.installs
	agents := make([]*EnvironmentAgent, 0, len(w.agents))
	for _, a := range w.agents {
		agents = append(agents, a)
	}
	w.apps = map[string]*SandboxAppInstance{}
	w.installs = map[string]*localInstall{}
	w.agents = map[int64]*EnvironmentAgent{}
	w.agentAliases = map[string]int64{}
	managedMCPManager := w.managedMCPManager
	w.managedMCPManager = nil
	w.managedMCPs = map[string]*RuntimeManagedMCP{}
	w.mu.Unlock()

	if managedMCPManager != nil {
		managedMCPManager.StopAll()
	}
	if w.server != nil && w.server.store != nil && w.ID != "" {
		_ = w.server.deleteEnvironmentSubscriptionRows(w.ID)
	}
	for _, agent := range agents {
		if agent != nil {
			agent.Stop()
		}
	}
	// Fake integration connections can belong to cloned installs. Remove
	// runtime-scoped connection rows first so install teardown cannot be held
	// back by present or future ownership constraints.
	if w.server != nil && w.ID != "" {
		_, _ = w.server.store.db.Exec(`DELETE FROM connections WHERE project_id = ?`, w.ID)
	}
	// Tear down real installs (stop process + delete install/app rows +
	// remove the throwaway data dir).
	if w.server != nil {
		for _, in := range installs {
			w.server.deleteEnvironmentInstall(in.InstallID)
			if in.DataDir != "" {
				_ = os.RemoveAll(in.DataDir)
			}
		}
		if w.server.appEventDispatcher != nil {
			_ = w.server.appEventDispatcher.Reconcile()
		}
	}
	for _, a := range apps {
		a.Stop()
	}
	if w.removeInterceptor != nil {
		w.removeInterceptor()
	}
	if w.edge != nil {
		w.edge.Stop()
	}
}

// EnvironmentManager owns the live set of Environments. Hung off Server as s.environments;
// nil-safe — created at boot but only touched by environment endpoints.
type EnvironmentManager struct {
	mu           sync.Mutex
	environments map[string]*Environment
	creating     map[string]bool
	dataDir      string
	snapshots    *SnapshotStore
	server       *Server // set in NewServer; needed for real (install-backed) environment apps
	expiryTimers map[string]*time.Timer

	// ResolveBinary maps an app manifest name to its sidecar binary path
	// when a SandboxApp doesn't carry an explicit BinaryPath. Injectable
	// so prod (LocalSupervisor-known paths) and tests can override.
	// Defaults to defaultBinaryResolver.
	ResolveBinary func(name string) (string, error)

	// ResolveSource maps an app manifest name to its local working-copy
	// dir (for installLocalSource / environment-from-bindings derivation).
	// Injectable for tests. Defaults to defaultSourceResolver.
	ResolveSource func(name string) (string, error)
}

// NewEnvironmentManager creates the manager and ensures its data root exists.
func NewEnvironmentManager(dataDir string) *EnvironmentManager {
	_ = os.MkdirAll(dataDir, 0755)
	return &EnvironmentManager{
		environments:  map[string]*Environment{},
		creating:      map[string]bool{},
		dataDir:       dataDir,
		snapshots:     NewSnapshotStore(dataDir),
		expiryTimers:  map[string]*time.Timer{},
		ResolveBinary: defaultBinaryResolver,
		ResolveSource: defaultSourceResolver,
	}
}

// Snapshots exposes the snapshot store.
func (wm *EnvironmentManager) Snapshots() *SnapshotStore { return wm.snapshots }

// IntegrationFixture answers one (app, tool) integration call in Environment
// test mode without hitting the real third-party API.
type IntegrationFixture struct {
	App    string `json:"app"`  // AppTemplate.Slug
	Tool   string `json:"tool"` // AppToolDef.Name
	Status int    `json:"status"`
	Data   any    `json:"data"`
}

// RuntimeIntegrationBinding is the generic runtime primitive for creating a
// fake, runtime-scoped connection and binding it to one cloned app role.
type RuntimeIntegrationBinding struct {
	App            string            `json:"app"`
	Role           string            `json:"role"`
	Slug           string            `json:"slug"`
	AppName        string            `json:"app_name,omitempty"`
	Name           string            `json:"name,omitempty"`
	AuthType       string            `json:"auth_type,omitempty"`
	Credentials    map[string]string `json:"credentials,omitempty"`
	ExposeToAgents bool              `json:"expose_to_agents,omitempty"`
}

// RegisterEnvironmentInterceptor registers a PER-WORLD integration interceptor
// keyed by environment id. It answers the given fixtures and PASSES EVERYTHING
// ELSE THROUGH (handled=false), so it can never mask an unrelated call.
// Returns a remove func. Concurrent environments are safe — each is keyed by its
// own id in environmentInterceptors, and the executor only consults the entry for
// the call's threaded environment id (from the X-Apteva-Environment-Id header).
func RegisterEnvironmentInterceptor(environmentID string, fixtures []IntegrationFixture) (remove func()) {
	idx := make(map[string]IntegrationFixture, len(fixtures))
	for _, f := range fixtures {
		idx[f.App+"\x00"+f.Tool] = f
	}
	var fn integrationInterceptorFn = func(app *AppTemplate, tool *AppToolDef, _ map[string]any) (*ExecuteResult, bool) {
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
	environmentInterceptors.Store(environmentID, fn)
	return func() { environmentInterceptors.Delete(environmentID) }
}

// CreateFromSnapshot forks a Environment from a captured snapshot: it restores
// each app's data dir and the cassette, then boots the sidecars on that
// pre-populated state. This fork is independent and
// repeatable, starting from a known fixture.
func (wm *EnvironmentManager) CreateFromSnapshot(spec EnvironmentSpec, snapshotID string) (*Environment, error) {
	man, merr := wm.snapshots.Get(snapshotID)
	if merr != nil {
		return nil, fmt.Errorf("restore: %w", merr)
	}
	appDataDirs, err := wm.snapshots.Restore(snapshotID, "")
	if err != nil {
		return nil, err
	}
	if len(spec.Subscriptions) == 0 {
		spec.Subscriptions = append([]EnvironmentSubscriptionSpec(nil), man.Subscriptions...)
	}
	// Seed each declared app with its restored data dir.
	for i := range spec.Apps {
		if dir, ok := appDataDirs[spec.Apps[i].Name]; ok {
			spec.Apps[i].DataDir = dir
		}
	}
	if len(spec.AppSrcDirs) > 0 {
		spec.RestoredAppDataDirs = appDataDirs
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

// Create stands up a Environment: starts the edge, then spawns each app sidecar
// with HTTP_PROXY pointed at the edge and a throwaway data dir. On any
// failure the partially-built environment is torn down so we never leak processes.
func (wm *EnvironmentManager) Create(spec EnvironmentSpec) (*Environment, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("environment: ID required")
	}
	wm.mu.Lock()
	if _, exists := wm.environments[spec.ID]; exists || wm.creating[spec.ID] {
		wm.mu.Unlock()
		return nil, fmt.Errorf("environment %q already exists", spec.ID)
	}
	wm.creating[spec.ID] = true
	wm.mu.Unlock()
	registered := false
	defer func() {
		if registered {
			return
		}
		wm.mu.Lock()
		delete(wm.creating, spec.ID)
		wm.mu.Unlock()
	}()

	networkMode := normalizeEnvironmentNetworkMode(spec.NetworkMode, spec.Mode)
	integrationMode := normalizeEnvironmentIntegrationMode(spec.IntegrationMode, spec.Mode)
	edge, err := startEnvironmentEdge(spec.Policy, networkMode, spec.Cassette)
	if err != nil {
		return nil, err
	}

	budget := spec.HealthBudget
	if budget == 0 {
		budget = 20 * time.Second
	}
	w := &Environment{
		ID:                spec.ID,
		ProjectID:         spec.ProjectID,
		Mode:              edge.mode,
		NetworkMode:       edge.mode,
		IntegrationMode:   integrationMode,
		ownerInstallID:    spec.RuntimeOwnerInstallID,
		expiresAt:         spec.RuntimeExpiresAt,
		edge:              edge,
		server:            wm.server,
		connectionIDs:     append([]int64(nil), spec.ConnectionIDs...),
		sourceInstallIDs:  cloneInt64Map(spec.SourceInstallIDs),
		apps:              map[string]*SandboxAppInstance{},
		installs:          map[string]*localInstall{},
		agents:            map[int64]*EnvironmentAgent{},
		agentAliases:      map[string]int64{},
		subscriptions:     append([]EnvironmentSubscriptionSpec(nil), spec.Subscriptions...),
		mcpAttachments:    map[string]RuntimeMCPAttachment{},
		managedMCPs:       map[string]*RuntimeManagedMCP{},
		managedMCPManager: NewMCPManager(),
		createdAt:         time.Now(),
	}
	removeIntegrationMode := registerEnvironmentIntegrationMode(spec.ID, integrationMode)

	// Register this environment's integration interceptor (if any) before
	// spawning sidecars, so their first callback already routes correctly.
	if len(spec.IntegrationFixtures) > 0 {
		removeFixtures := RegisterEnvironmentInterceptor(spec.ID, spec.IntegrationFixtures)
		w.removeInterceptor = func() {
			removeFixtures()
			removeIntegrationMode()
		}
	} else {
		w.removeInterceptor = removeIntegrationMode
	}

	appSources, depBindings, err := wm.expandAppSourcesWithRequiredDeps(spec.AppSrcDirs)
	if err != nil {
		w.Stop()
		return nil, fmt.Errorf("environment %q: resolve app dependencies: %w", spec.ID, err)
	}

	for _, app := range spec.Apps {
		// Tag the sidecar with its environment id so the SDK forwards
		// X-Apteva-Environment-Id on platform callbacks → per-environment routing.
		app.EnvironmentID = spec.ID
		if app.BinaryPath == "" {
			if wm.ResolveBinary == nil {
				w.Stop()
				return nil, fmt.Errorf("environment %q: app %q has no BinaryPath and no resolver", spec.ID, app.Name)
			}
			bin, rerr := wm.ResolveBinary(app.Name)
			if rerr != nil {
				w.Stop()
				return nil, fmt.Errorf("environment %q: resolve app %q: %w", spec.ID, app.Name, rerr)
			}
			app.BinaryPath = bin
		}
		inst, serr := SpawnSandboxedApp(app, edge.ProxyURL(), spec.GatewayURL, budget)
		if serr != nil {
			w.Stop()
			return nil, fmt.Errorf("environment %q: spawn app %q: %w", spec.ID, app.Name, serr)
		}
		w.mu.Lock()
		w.apps[app.Name] = inst
		w.mu.Unlock()
	}

	// Real installs from local source — the clean Environment path. Each app is
	// installed under project_id = the environment id, with its egress routed
	// through the edge and APTEVA_ENVIRONMENT_ID set so its integration callbacks
	// route to this environment's interceptor.
	for _, src := range appSources {
		if wm.server == nil {
			w.Stop()
			return nil, fmt.Errorf("environment %q: AppSrcDirs needs a server-backed EnvironmentManager", spec.ID)
		}
		env := map[string]string{
			"HTTP_PROXY":            edge.ProxyURL(),
			"HTTPS_PROXY":           edge.ProxyURL(),
			"NO_PROXY":              "",
			"APTEVA_ENVIRONMENT_ID": spec.ID,
		}
		inst, ierr := wm.server.installLocalSource(src.Dir, spec.ID, env, spec.RestoredAppDataDirs[src.Name], nil)
		if ierr != nil {
			w.Stop()
			return nil, fmt.Errorf("environment %q: install app %q: %w", spec.ID, src.Name, ierr)
		}
		w.mu.Lock()
		w.installs[inst.AppName] = inst
		w.mu.Unlock()
	}
	if wm.server != nil && len(depBindings) > 0 {
		if err := wm.server.bindEnvironmentAppDependencies(w, depBindings); err != nil {
			w.Stop()
			return nil, fmt.Errorf("environment %q: bind app dependencies: %w", spec.ID, err)
		}
	}

	wm.mu.Lock()
	delete(wm.creating, spec.ID)
	wm.environments[spec.ID] = w
	if !spec.RuntimeExpiresAt.IsZero() {
		delay := time.Until(spec.RuntimeExpiresAt)
		if delay < 0 {
			delay = 0
		}
		wm.expiryTimers[spec.ID] = time.AfterFunc(delay, func() { wm.Destroy(spec.ID) })
	}
	wm.mu.Unlock()
	registered = true
	return w, nil
}

// expandAppSourcesWithRequiredDeps returns install order for local-source
// Environment apps. Required requires.apps dependencies are installed before their
// parents so a Environment selected with only "media" also gets "storage". Optional
// deps stay opt-in; callers can include them explicitly in AppSrcDirs.
func (wm *EnvironmentManager) expandAppSourcesWithRequiredDeps(initial map[string]string) ([]environmentAppSource, map[string][]string, error) {
	if len(initial) == 0 {
		return nil, nil, nil
	}
	resolve := wm.ResolveSource
	if resolve == nil {
		resolve = defaultSourceResolver
	}
	dirByName := map[string]string{}
	depsByName := map[string][]string{}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var ordered []environmentAppSource

	readManifest := func(dir string) (*sdk.Manifest, error) {
		data, err := os.ReadFile(filepath.Join(dir, "apteva.yaml"))
		if err != nil {
			return nil, err
		}
		m, err := sdk.ParseManifest(data)
		if err != nil {
			return nil, err
		}
		if m.Name == "" {
			return nil, fmt.Errorf("%s: manifest has no name", dir)
		}
		return m, nil
	}

	appendDep := func(parent, dep string) {
		for _, existing := range depsByName[parent] {
			if existing == dep {
				return
			}
		}
		depsByName[parent] = append(depsByName[parent], dep)
	}

	var visit func(name, dir string) (string, error)
	visit = func(name, dir string) (string, error) {
		if dir == "" {
			resolved, err := resolve(name)
			if err != nil {
				return "", fmt.Errorf("resolve source for required app %q: %w", name, err)
			}
			dir = resolved
		}
		m, err := readManifest(dir)
		if err != nil {
			return "", fmt.Errorf("read manifest for app %q: %w", name, err)
		}
		actual := m.Name
		if visited[actual] {
			return actual, nil
		}
		if visiting[actual] {
			return "", fmt.Errorf("dependency cycle involving %q", actual)
		}
		visiting[actual] = true
		dirByName[actual] = dir
		for _, dep := range m.Requires.Apps {
			if dep.Name == "" {
				continue
			}
			if dep.Optional {
				if _, explicitlyIncluded := initial[dep.Name]; !explicitlyIncluded {
					continue
				}
			}
			if depDir, ok := initial[dep.Name]; ok {
				depActual, err := visit(dep.Name, depDir)
				if err != nil {
					return "", err
				}
				appendDep(actual, depActual)
			} else {
				depActual, err := visit(dep.Name, "")
				if err != nil {
					return "", err
				}
				appendDep(actual, depActual)
			}
		}
		delete(visiting, actual)
		visited[actual] = true
		ordered = append(ordered, environmentAppSource{Name: actual, Dir: dirByName[actual]})
		return actual, nil
	}

	names := make([]string, 0, len(initial))
	for name := range initial {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := visit(name, initial[name]); err != nil {
			return nil, nil, err
		}
	}
	return ordered, depsByName, nil
}

func (s *Server) bindEnvironmentAppDependencies(w *Environment, depsByName map[string][]string) error {
	for parent, deps := range depsByName {
		parentInst, ok := w.Install(parent)
		if !ok {
			continue
		}
		bindings := map[string]any{}
		var raw string
		if err := s.store.db.QueryRow(`SELECT COALESCE(integration_bindings, '{}') FROM app_installs WHERE id = ?`, parentInst.InstallID).Scan(&raw); err == nil {
			_ = json.Unmarshal([]byte(raw), &bindings)
		}
		changed := false
		for _, dep := range deps {
			depInst, ok := w.Install(dep)
			if !ok {
				return fmt.Errorf("%s requires %s but it is not installed in environment", parent, dep)
			}
			bindings[dep] = depInst.InstallID
			changed = true
		}
		if !changed {
			continue
		}
		next, _ := json.Marshal(bindings)
		if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings = ? WHERE id = ?`, string(next), parentInst.InstallID); err != nil {
			return err
		}
	}
	s.LoadInstalledApps()
	return nil
}

func (s *Server) bindEnvironmentIntegrationMocks(userID int64, w *Environment, bindings []RuntimeIntegrationBinding) error {
	if w == nil {
		return fmt.Errorf("environment required")
	}
	for i, b := range bindings {
		appName := strings.TrimSpace(b.App)
		role := strings.TrimSpace(b.Role)
		slug := strings.TrimSpace(b.Slug)
		if b.ExposeToAgents {
			if appName != "" || role != "" {
				return fmt.Errorf("binding %d: agent-visible bindings must omit app and role", i)
			}
			conn, err := s.createEnvironmentMockConnection(userID, w, b, w.OwnerInstallID())
			if err != nil {
				return fmt.Errorf("binding %d: %w", i, err)
			}
			w.AddConnectionIDs(conn.ID)
			continue
		}
		if appName == "" || role == "" || slug == "" {
			return fmt.Errorf("binding %d: app, role, and slug required", i)
		}
		parentInst, ok := w.Install(appName)
		if !ok {
			return fmt.Errorf("binding %d: app %q is not installed in environment", i, appName)
		}
		dep, err := installRoleDep(s, parentInst.InstallID, role)
		if err != nil {
			return fmt.Errorf("binding %d: read %s role %q: %w", i, appName, role, err)
		}
		if dep == nil {
			return fmt.Errorf("binding %d: app %q does not declare integration role %q", i, appName, role)
		}
		if strings.EqualFold(dep.Kind, "app") {
			return fmt.Errorf("binding %d: role %q on app %q is an app dependency, not an integration", i, role, appName)
		}
		if len(dep.CompatibleSlugs) > 0 && !containsString(dep.CompatibleSlugs, slug) {
			return fmt.Errorf("binding %d: app %q role %q is not compatible with integration %q", i, appName, role, slug)
		}

		credentials := map[string]string{}
		for k, v := range b.Credentials {
			credentials[k] = v
		}
		applyEnvironmentMockCredentialDefaults(slug, credentials)
		raw, err := json.Marshal(credentials)
		if err != nil {
			return fmt.Errorf("binding %d: marshal credentials: %w", i, err)
		}
		enc, err := Encrypt(s.secret, string(raw))
		if err != nil {
			return fmt.Errorf("binding %d: encrypt credentials: %w", i, err)
		}
		authType := strings.TrimSpace(b.AuthType)
		if authType == "" {
			authType = "api_key"
		}
		connectionName := strings.TrimSpace(b.Name)
		if connectionName == "" {
			connectionName = "Mock " + slug
		}
		connectionAppName := strings.TrimSpace(b.AppName)
		if connectionAppName == "" {
			connectionAppName = slug
		}
		conn, err := s.store.CreateConnectionExt(ConnectionInput{
			UserID:            userID,
			AppSlug:           slug,
			AppName:           connectionAppName,
			Name:              connectionName,
			AuthType:          authType,
			EncryptedCreds:    enc,
			ProjectID:         w.ID,
			Source:            "local",
			Status:            "active",
			CreatedVia:        "app_install",
			OwnerAppInstallID: parentInst.InstallID,
		})
		if err != nil {
			return fmt.Errorf("binding %d: create connection: %w", i, err)
		}

		existing := map[string]any{}
		var existingRaw string
		if err := s.store.db.QueryRow(`SELECT COALESCE(integration_bindings, '{}') FROM app_installs WHERE id = ?`, parentInst.InstallID).Scan(&existingRaw); err == nil {
			_ = json.Unmarshal([]byte(existingRaw), &existing)
		}
		existing[role] = conn.ID
		next, _ := json.Marshal(existing)
		if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings = ? WHERE id = ?`, string(next), parentInst.InstallID); err != nil {
			return fmt.Errorf("binding %d: update app binding: %w", i, err)
		}
	}
	s.LoadInstalledApps()
	return nil
}

func validateRuntimeBindingOverlap(real []sdk.RuntimeConnectionBinding, fake []RuntimeIntegrationBinding) error {
	fakeRoles := map[string]bool{}
	for _, binding := range fake {
		if binding.ExposeToAgents {
			continue
		}
		app := strings.TrimSpace(binding.App)
		role := strings.TrimSpace(binding.Role)
		if app != "" && role != "" {
			fakeRoles[app+"\x00"+role] = true
		}
	}
	for i, binding := range real {
		app := strings.TrimSpace(binding.App)
		role := strings.TrimSpace(binding.Role)
		if fakeRoles[app+"\x00"+role] {
			return fmt.Errorf("binding %d: app %q role %q cannot use both a real and fake connection", i, app, role)
		}
	}
	return nil
}

// bindEnvironmentConnections binds existing source-project connections to
// cloned runtime installs. It never copies credentials or mutates source apps.
func (s *Server) bindEnvironmentConnections(userID int64, sourceProjectID string, w *Environment, bindings []sdk.RuntimeConnectionBinding) error {
	if w == nil {
		return fmt.Errorf("environment required")
	}
	type validatedBinding struct {
		installID    int64
		app          string
		role         string
		connectionID int64
	}
	validated := make([]validatedBinding, 0, len(bindings))
	seen := map[string]int64{}
	for i, binding := range bindings {
		app := strings.TrimSpace(binding.App)
		role := strings.TrimSpace(binding.Role)
		if app == "" || role == "" || binding.ConnectionID <= 0 {
			return fmt.Errorf("binding %d: app, role, and positive connection_id required", i)
		}
		key := app + "\x00" + role
		if existing, ok := seen[key]; ok {
			if existing == binding.ConnectionID {
				continue
			}
			return fmt.Errorf("binding %d: app %q role %q has conflicting connection IDs", i, app, role)
		}
		seen[key] = binding.ConnectionID

		clone, ok := w.Install(app)
		if !ok {
			return fmt.Errorf("binding %d: app %q is not installed in environment", i, app)
		}
		dep, err := installRoleDep(s, clone.InstallID, role)
		if err != nil {
			return fmt.Errorf("binding %d: read %s role %q: %w", i, app, role, err)
		}
		if dep == nil {
			return fmt.Errorf("binding %d: app %q does not declare integration role %q", i, app, role)
		}
		if strings.EqualFold(dep.Kind, "app") {
			return fmt.Errorf("binding %d: role %q on app %q is an app dependency, not an integration", i, role, app)
		}
		conn, _, err := s.store.GetConnection(userID, binding.ConnectionID)
		if err != nil || conn == nil {
			return fmt.Errorf("binding %d: connection %d not found", i, binding.ConnectionID)
		}
		if !strings.EqualFold(strings.TrimSpace(conn.Status), "active") {
			return fmt.Errorf("binding %d: connection %d is %s, not active", i, binding.ConnectionID, conn.Status)
		}
		if sourceProjectID != "" && conn.ProjectID != "" && conn.ProjectID != sourceProjectID {
			return fmt.Errorf("binding %d: connection %d is scoped to another project", i, binding.ConnectionID)
		}
		if len(dep.CompatibleSlugs) > 0 && !containsString(dep.CompatibleSlugs, conn.AppSlug) {
			return fmt.Errorf("binding %d: app %q role %q is not compatible with integration %q", i, app, role, conn.AppSlug)
		}
		validated = append(validated, validatedBinding{installID: clone.InstallID, app: app, role: role, connectionID: binding.ConnectionID})
	}

	byInstall := map[int64]map[string]any{}
	for _, binding := range validated {
		current := byInstall[binding.installID]
		if current == nil {
			current = bindingsForInstall(s, binding.installID)
			byInstall[binding.installID] = current
		}
		if existing := current[binding.role]; appBindingIsSet(existing) && !appBindingContains(existing, binding.connectionID) {
			return fmt.Errorf("app %q role %q is already bound", binding.app, binding.role)
		}
		current[binding.role] = binding.connectionID
	}
	for installID, nextBindings := range byInstall {
		next, err := json.Marshal(nextBindings)
		if err != nil {
			return fmt.Errorf("encode bindings for install %d: %w", installID, err)
		}
		if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings = ? WHERE id = ?`, string(next), installID); err != nil {
			return fmt.Errorf("update bindings for install %d: %w", installID, err)
		}
	}
	if len(byInstall) > 0 {
		s.LoadInstalledApps()
	}
	return nil
}

func (s *Server) createEnvironmentMockConnection(userID int64, w *Environment, b RuntimeIntegrationBinding, ownerInstallID int64) (*Connection, error) {
	slug := strings.TrimSpace(b.Slug)
	if slug == "" {
		return nil, fmt.Errorf("slug required")
	}
	if s.catalog == nil || s.catalog.Get(slug) == nil {
		return nil, fmt.Errorf("integration %q not found", slug)
	}
	credentials := map[string]string{}
	for k, v := range b.Credentials {
		credentials[k] = v
	}
	applyEnvironmentMockCredentialDefaults(slug, credentials)
	raw, err := json.Marshal(credentials)
	if err != nil {
		return nil, fmt.Errorf("marshal credentials: %w", err)
	}
	enc, err := Encrypt(s.secret, string(raw))
	if err != nil {
		return nil, fmt.Errorf("encrypt credentials: %w", err)
	}
	authType := strings.TrimSpace(b.AuthType)
	if authType == "" {
		authType = "api_key"
	}
	connectionName := strings.TrimSpace(b.Name)
	if connectionName == "" {
		connectionName = "Mock " + slug
	}
	connectionAppName := strings.TrimSpace(b.AppName)
	if connectionAppName == "" {
		connectionAppName = slug
	}
	conn, err := s.store.CreateConnectionExt(ConnectionInput{
		UserID: userID, AppSlug: slug, AppName: connectionAppName, Name: connectionName,
		AuthType: authType, EncryptedCreds: enc, ProjectID: w.ID, Source: "local",
		Status: "active", CreatedVia: "app_install", OwnerAppInstallID: ownerInstallID,
	})
	if err != nil {
		return nil, fmt.Errorf("create connection: %w", err)
	}
	return conn, nil
}

func applyEnvironmentMockCredentialDefaults(slug string, credentials map[string]string) {
	if credentials == nil {
		return
	}
	if strings.EqualFold(slug, "aws-ses") {
		if strings.TrimSpace(credentials["region"]) == "" {
			credentials["region"] = "us-east-1"
		}
		if strings.TrimSpace(credentials["access_key_id"]) == "" {
			credentials["access_key_id"] = "mock-access-key-id"
		}
		if strings.TrimSpace(credentials["secret_access_key"]) == "" {
			credentials["secret_access_key"] = "mock-secret-access-key"
		}
		return
	}
	if len(credentials) == 0 {
		credentials["mock"] = "true"
	}
}

// Get returns a live Environment by id.
func (wm *EnvironmentManager) Get(id string) (*Environment, bool) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	w, ok := wm.environments[id]
	return w, ok
}

// List returns a snapshot of all live Environments.
func (wm *EnvironmentManager) List() []*Environment {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	out := make([]*Environment, 0, len(wm.environments))
	for _, w := range wm.environments {
		out = append(out, w)
	}
	return out
}

// Destroy stops and forgets a Environment. No-op if unknown.
func (wm *EnvironmentManager) Destroy(id string) {
	wm.mu.Lock()
	w, ok := wm.environments[id]
	if ok {
		delete(wm.environments, id)
	}
	if timer := wm.expiryTimers[id]; timer != nil {
		timer.Stop()
	}
	delete(wm.expiryTimers, id)
	wm.mu.Unlock()
	if ok && w != nil {
		w.Stop()
	}
}

// StopAll tears down every Environment — called on server shutdown.
func (wm *EnvironmentManager) StopAll() {
	wm.mu.Lock()
	ws := wm.environments
	wm.environments = map[string]*Environment{}
	wm.creating = map[string]bool{}
	for _, timer := range wm.expiryTimers {
		timer.Stop()
	}
	wm.expiryTimers = map[string]*time.Timer{}
	wm.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

// defaultBinaryResolver finds a sidecar binary by manifest name. Order:
//  1. APTEVA_APP_BIN_<NAME>           (explicit override; dashes→underscores, upper)
//  2. ~/.apteva/apps/<name>/<name>    (conventional local install)
//  3. ../apps/mcp/<name>/<name> and apps/mcp/<name>/<name>  (dev checkout)
//  4. <name> on PATH
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
