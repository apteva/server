package main

// Wires catalog provider simulations into test runtimes: creating them for
// simulate=true bindings, answering tool calls on their fake connections,
// delivering their events to the runtime's apps in-process, exposing
// triggers and state, and carrying their state through snapshots.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/gorilla/websocket"
)

// IntegrationSimulationSnapshot is one simulation's state inside a runtime
// snapshot. Signing keys are not captured: a restored runtime generates new
// ones for its new fake connections.
type IntegrationSimulationSnapshot struct {
	Slug  string     `json:"slug"`
	App   string     `json:"app,omitempty"`
	Role  string     `json:"role,omitempty"`
	State *mockState `json:"state"`
}

type environmentSimulations struct {
	mu           sync.Mutex
	byConnection map[int64]*integrationSimulation
	ordered      []*integrationSimulation
}

// Live simulations by environment id, and the environment that owns each
// simulated connection. A simulated connection is answered by its
// simulation even when a caller omits the environment header, so it can
// never reach the real provider.
var (
	environmentSimulationRegistry sync.Map // environment id -> *environmentSimulations
	simulatedConnectionOwners     sync.Map // connection id -> environment id
)

type integrationConnectionIDKey struct{}

func withIntegrationConnectionID(ctx context.Context, connectionID int64) context.Context {
	if connectionID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, integrationConnectionIDKey{}, connectionID)
}

func integrationConnectionIDFrom(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	id, _ := ctx.Value(integrationConnectionIDKey{}).(int64)
	return id
}

func simulatedConnectionEnvironment(connectionID int64) string {
	if v, ok := simulatedConnectionOwners.Load(connectionID); ok {
		return v.(string)
	}
	return ""
}

func lookupIntegrationSimulation(environmentID string, connectionID int64) *integrationSimulation {
	if environmentID == "" || connectionID <= 0 {
		return nil
	}
	v, ok := environmentSimulationRegistry.Load(environmentID)
	if !ok {
		return nil
	}
	sims := v.(*environmentSimulations)
	sims.mu.Lock()
	defer sims.mu.Unlock()
	return sims.byConnection[connectionID]
}

// simulateIntegrationTool answers a tool call on a simulated connection.
// handled=false means the connection is not simulated.
func simulateIntegrationTool(ctx context.Context, environmentID string, app *AppTemplate, tool *AppToolDef, input map[string]any) (*ExecuteResult, bool) {
	sim := lookupIntegrationSimulation(environmentID, integrationConnectionIDFrom(ctx))
	if sim == nil || app == nil || tool == nil || app.Slug != sim.slug {
		return nil, false
	}
	evidence := InterceptedCall{
		Host: hostOf(app.BaseURL), Path: simulatedToolPath(tool, input), Method: strings.ToUpper(tool.Method),
		Mocked: true, Kind: "integration_simulation", Integration: sim.slug, Tool: tool.Name,
	}
	if raw, err := json.Marshal(input); err == nil {
		evidence.ReqBody = truncateEvidence(raw)
	}
	op := sim.mock.tools[tool.Name]
	var outcome mockOutcome
	if op == nil {
		outcome = mockOutcome{status: http.StatusNotImplemented, body: map[string]any{
			"error":   sdk.RuntimeErrMockUnsupportedTool,
			"message": fmt.Sprintf("%s tool %s is not simulated; add it to the integration's mock section or answer it with an integration fixture", sim.slug, tool.Name),
		}}
	} else {
		outcome = sim.execute(op, input)
	}
	evidence.Status = outcome.status
	if raw, err := json.Marshal(outcome.body); err == nil {
		evidence.RespBody = truncateEvidence(raw)
	}
	sim.recordEvidence(evidence)
	return &ExecuteResult{Success: outcome.status >= 200 && outcome.status < 300, Status: outcome.status, Data: outcome.body}, true
}

func simulatedToolPath(tool *AppToolDef, input map[string]any) string {
	path := tool.Path
	for key, value := range input {
		path = strings.ReplaceAll(path, "{"+key+"}", url.PathEscape(mockString(value)))
	}
	return path
}

// --- binding setup ---

type simulatedBindingSetup struct {
	app  *AppTemplate
	mock *compiledMock
	key  ed25519.PrivateKey
}

// prepareSimulatedBinding validates a simulate binding and adds its
// generated webhook public key to the credentials before the connection is
// created. It returns nil for an ordinary fake binding.
func (s *Server) prepareSimulatedBinding(w *Environment, b RuntimeIntegrationBinding, slug string, credentials map[string]string) (*simulatedBindingSetup, error) {
	if !b.Simulate {
		if len(b.Seed) > 0 {
			return nil, fmt.Errorf("seed requires simulate")
		}
		return nil, nil
	}
	var template *AppTemplate
	if s.catalog != nil {
		template = s.catalog.Get(slug)
	}
	if template == nil {
		return nil, fmt.Errorf("integration %q not found", slug)
	}
	mock, err := compiledAppMock(template)
	if err != nil {
		return nil, err
	}
	if mock == nil {
		return nil, fmt.Errorf("integration %q has no simulation", slug)
	}
	app, role := strings.TrimSpace(b.App), strings.TrimSpace(b.Role)
	for _, sim := range w.simulationList() {
		if sim.slug == slug && sim.app == app && sim.role == role {
			return nil, fmt.Errorf("integration %q is already simulated for this binding", slug)
		}
	}
	for name := range b.Seed {
		def := mock.collections[name]
		if def == nil {
			return nil, fmt.Errorf("seed: unknown collection %q", name)
		}
		if !def.seedable {
			return nil, fmt.Errorf("seed: collection %q cannot be seeded", name)
		}
	}
	setup := &simulatedBindingSetup{app: template, mock: mock}
	if mock.signing != nil {
		field := mock.signing.PublicKeyField
		if strings.TrimSpace(credentials[field]) != "" {
			return nil, fmt.Errorf("credential %q is generated for simulated bindings; leave it empty", field)
		}
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate simulation signing key: %w", err)
		}
		credentials[field] = base64.StdEncoding.EncodeToString(public)
		setup.key = private
	}
	return setup, nil
}

// attachSimulatedBinding starts the simulation for a created connection,
// from the runtime snapshot it was restored from or from the binding seed.
func (s *Server) attachSimulatedBinding(w *Environment, setup *simulatedBindingSetup, b RuntimeIntegrationBinding, slug string, connectionID int64) error {
	if setup == nil {
		return nil
	}
	app, role := strings.TrimSpace(b.App), strings.TrimSpace(b.Role)
	sim := newIntegrationSimulation(w.ID, slug, app, role, connectionID, setup.app, setup.mock)
	sim.privateKey = setup.key
	if w.clock != nil {
		clock := w.clock
		sim.now = func() time.Time { return clock.Now().UTC() }
	}
	if edge := w.edge; edge != nil {
		sim.evidence = edge.record
	}
	sim.deliver = func(ctx context.Context, target mockDeliveryTarget, header http.Header, body []byte) (int, []byte, error) {
		return s.deliverSimulatedWebhook(ctx, w, target, header, body)
	}
	sim.fixtureBase = strings.TrimRight(s.publicBaseURL(), "/") + "/api/integration-simulation/fixtures"
	sim.dialMedia = func(ctx context.Context, target mockDeliveryTarget) (*websocket.Conn, error) {
		return s.dialSimulatedMedia(ctx, w, target)
	}
	if restored := w.restoredSimulation(slug, app, role); restored != nil {
		if err := sim.restore(restored); err != nil {
			return err
		}
	} else if len(b.Seed) > 0 {
		if err := sim.seed(b.Seed); err != nil {
			return err
		}
	}
	w.addSimulation(sim)
	return nil
}

func (w *Environment) addSimulation(sim *integrationSimulation) {
	w.mu.Lock()
	if w.simulations == nil {
		w.simulations = &environmentSimulations{byConnection: map[int64]*integrationSimulation{}}
		environmentSimulationRegistry.Store(w.ID, w.simulations)
	}
	sims := w.simulations
	w.mu.Unlock()
	sims.mu.Lock()
	sims.byConnection[sim.connectionID] = sim
	sims.ordered = append(sims.ordered, sim)
	sims.mu.Unlock()
	simulatedConnectionOwners.Store(sim.connectionID, w.ID)
}

func (w *Environment) simulationList() []*integrationSimulation {
	w.mu.Lock()
	sims := w.simulations
	w.mu.Unlock()
	if sims == nil {
		return nil
	}
	sims.mu.Lock()
	defer sims.mu.Unlock()
	return append([]*integrationSimulation(nil), sims.ordered...)
}

// findSimulation selects by slug, then by app and role when given. With no
// app and role it returns the only simulation of that slug, or nil.
func (w *Environment) findSimulation(slug, app, role string) *integrationSimulation {
	var found *integrationSimulation
	for _, sim := range w.simulationList() {
		if sim.slug != slug {
			continue
		}
		if app != "" || role != "" {
			if sim.app == app && sim.role == role {
				return sim
			}
			continue
		}
		if found != nil {
			return nil
		}
		found = sim
	}
	return found
}

func (w *Environment) countSimulations(slug string) int {
	n := 0
	for _, sim := range w.simulationList() {
		if sim.slug == slug {
			n++
		}
	}
	return n
}

func (w *Environment) restoredSimulation(slug, app, role string) *mockState {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, snap := range w.restoredSimulations {
		if snap.Slug == slug && snap.App == app && snap.Role == role {
			return snap.State
		}
	}
	return nil
}

// SimulationSnapshots captures every simulation's state for a snapshot.
func (w *Environment) SimulationSnapshots() []IntegrationSimulationSnapshot {
	var out []IntegrationSimulationSnapshot
	for _, sim := range w.simulationList() {
		out = append(out, IntegrationSimulationSnapshot{Slug: sim.slug, App: sim.app, Role: sim.role, State: sim.snapshotState()})
	}
	return out
}

// stopSimulations ends deliveries and unregisters every simulation. It runs
// before the runtime's apps and connections are torn down.
func (w *Environment) stopSimulations() {
	w.mu.Lock()
	sims := w.simulations
	w.simulations = nil
	w.mu.Unlock()
	if sims == nil {
		return
	}
	environmentSimulationRegistry.Delete(w.ID)
	sims.mu.Lock()
	ordered := append([]*integrationSimulation(nil), sims.ordered...)
	sims.mu.Unlock()
	for _, sim := range ordered {
		simulatedConnectionOwners.Delete(sim.connectionID)
		sim.stop()
	}
}

// --- in-process delivery ---

func (s *Server) deliverSimulatedWebhook(ctx context.Context, w *Environment, target mockDeliveryTarget, header http.Header, body []byte) (int, []byte, error) {
	inst, ok := w.Install(target.App)
	if !ok {
		return 0, nil, fmt.Errorf("app %q is not installed in this runtime", target.App)
	}
	if target.InstallID != 0 && target.InstallID != inst.InstallID {
		return 0, nil, fmt.Errorf("install %d is not app %q in this runtime", target.InstallID, target.App)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+mockRuntimeHost+target.Tail, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.URL.RawQuery = target.RawQuery
	req.Header = header.Clone()
	req.RemoteAddr = "127.0.0.1:0"
	recorder := newMockResponseRecorder(mockMaxEvidenceBody)
	s.proxyEnvironmentInstall(recorder, req, inst, target.Tail)
	return recorder.statusCode(), recorder.body.Bytes(), nil
}

// mockResponseRecorder captures an in-process app response, keeping at most
// limit bytes of its body.
type mockResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
}

func newMockResponseRecorder(limit int) *mockResponseRecorder {
	return &mockResponseRecorder{header: http.Header{}, limit: limit}
}

func (r *mockResponseRecorder) Header() http.Header { return r.header }

func (r *mockResponseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *mockResponseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if room := r.limit - r.body.Len(); room > 0 {
		if len(p) > room {
			r.body.Write(p[:room])
		} else {
			r.body.Write(p)
		}
	}
	return len(p), nil
}

func (r *mockResponseRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// --- runtime API: /api/apps/callback/runtimes/{id}/integrations/{slug}/... ---

const (
	mockTriggerDefaultWait = 10 * time.Second
	mockTriggerMaxWait     = 60 * time.Second
)

func (s *Server) handleRuntimeIntegrations(w http.ResponseWriter, r *http.Request, runtime *Environment, parts []string) {
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	slug := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "state":
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimesManage) {
			return
		}
		sim, status, message := selectRuntimeSimulation(runtime, slug, r.URL.Query().Get("app"), r.URL.Query().Get("role"))
		if sim == nil {
			http.Error(w, message, status)
			return
		}
		writeJSON(w, sim.publicState())
	case len(parts) == 3 && parts[1] == "triggers" && parts[2] != "":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !s.requireRuntimePermission(w, runtime.OwnerInstallID(), sdk.PermRuntimesManage) {
			return
		}
		var req sdk.RuntimeIntegrationTriggerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		sim, status, message := selectRuntimeSimulation(runtime, slug, req.App, req.Role)
		if sim == nil {
			http.Error(w, message, status)
			return
		}
		trigger := sim.mock.triggers[parts[2]]
		if trigger == nil {
			http.Error(w, fmt.Sprintf("integration %q has no trigger %q", slug, parts[2]), http.StatusNotFound)
			return
		}
		for _, field := range trigger.required {
			if value, ok := req.Input[field]; !ok || value == nil {
				http.Error(w, fmt.Sprintf("trigger %q requires input.%s", parts[2], field), http.StatusBadRequest)
				return
			}
		}
		outcome := sim.execute(&trigger.compiledMockOperation, req.Input)
		if outcome.status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "trigger_failed", "status": outcome.status, "result": outcome.body})
			return
		}
		if req.Wait {
			wait := mockTriggerDefaultWait
			if req.TimeoutMS > 0 {
				wait = time.Duration(req.TimeoutMS) * time.Millisecond
			}
			if wait > mockTriggerMaxWait {
				wait = mockTriggerMaxWait
			}
			waitForDeliveries(r.Context(), outcome.deliveries, wait)
		}
		result, err := json.Marshal(outcome.body)
		if err != nil {
			http.Error(w, "encode trigger result", http.StatusInternalServerError)
			return
		}
		writeJSON(w, sdk.RuntimeIntegrationTriggerResult{Result: result, Deliveries: sim.deliveryRecords(outcome.deliveries)})
	default:
		http.NotFound(w, r)
	}
}

func selectRuntimeSimulation(runtime *Environment, slug, app, role string) (*integrationSimulation, int, string) {
	app, role = strings.TrimSpace(app), strings.TrimSpace(role)
	if sim := runtime.findSimulation(slug, app, role); sim != nil {
		return sim, 0, ""
	}
	if app == "" && role == "" && runtime.countSimulations(slug) > 1 {
		return nil, http.StatusBadRequest, fmt.Sprintf("runtime has several simulated %q bindings; set app and role", slug)
	}
	return nil, http.StatusNotFound, fmt.Sprintf("runtime has no simulated %q binding", slug)
}

func waitForDeliveries(ctx context.Context, deliveries []*mockDelivery, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for _, d := range deliveries {
		select {
		case <-d.done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
