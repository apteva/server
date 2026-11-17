package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitRuntimeMonitors(t *testing.T, m *appRuntimeManager) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		m.monitorMu.Lock()
		busy := false
		for _, e := range m.monitors {
			busy = busy || e.InFlight
		}
		m.monitorMu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(end) {
			t.Fatal("monitor did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
func collectRuntimeMonitor(t *testing.T, m *appRuntimeManager, id int64) {
	t.Helper()
	m.queueRuntimeObservations(id, true)
	m.dispatchRuntimeObservations(time.Now())
	waitRuntimeMonitors(t, m)
}
func nextRuntimeHealth(t *testing.T, m *appRuntimeManager, id string) {
	t.Helper()
	m.monitorMu.Lock()
	e := m.monitors[id]
	if e == nil {
		t.Fatal("monitor missing")
	}
	e.NextHealth = time.Now().Add(-time.Second)
	m.monitorMu.Unlock()
	m.dispatchRuntimeObservations(time.Now())
	waitRuntimeMonitors(t, m)
}
func runtimeInspectionCount(f *runtimeBackendFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, event := range f.events {
		if strings.HasPrefix(event, "inspect:") {
			n++
		}
	}
	return n
}

// An app may expose identity-bound work load on its existing private status
// route. No backend host inspection is involved in answering this request.
func serveRuntimeWorkload(f *runtimeBackendFixture) {
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/app/status") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/exec/"), "/")
			f.mu.Lock()
			execution := f.executions[parts[0]]
			var generation int64
			fmt.Sscan(f.env[parts[0]]["APTEVA_DEPLOYMENT_GENERATION"], &generation)
			f.mu.Unlock()
			writeJSON(w, map[string]any{"deployment_id": parts[0], "generation": generation, "admitting": true, "state": "active", "load": execution.Load})
			return
		}
		original.ServeHTTP(w, r)
	})
}

func TestManagedRuntimeMonitorReusesVerificationAndLoad(t *testing.T) {
	t.Setenv("APTEVA_RUNTIME_INSPECTION_AUDIT_INTERVAL", "")
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	f.mu.Lock()
	out := f.executions[active.ID]
	load := 1.0
	out.Load = &load
	f.executions[active.ID] = out
	f.events = nil
	f.mu.Unlock()
	serveRuntimeWorkload(f)
	var health, admission atomic.Int32
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			health.Add(1)
			if r.Header.Get("Authorization") == "" {
				t.Error("health probe missing authentication")
			}
		}
		if strings.HasSuffix(r.URL.Path, "/app/status") {
			admission.Add(1)
		}
		original.ServeHTTP(w, r)
	})
	collectRuntimeMonitor(t, m, id)
	for i := 0; i < 4; i++ {
		nextRuntimeHealth(t, m, active.ID)
	}
	if got := runtimeInspectionCount(f); got != 1 {
		t.Fatalf("stable deployment fully inspected %d times", got)
	}
	if health.Load() != 5 || admission.Load() != 5 {
		t.Fatalf("cheap probes: health=%d admission=%d", health.Load(), admission.Load())
	}
	m.monitorMu.Lock()
	entry := m.monitors[active.ID]
	nextFull := entry.NextFull
	m.monitorMu.Unlock()
	if !nextFull.IsZero() {
		t.Fatalf("stable deployment scheduled a host inspection: %v", nextFull)
	}
	// Fresh app workload remains usable even after the old five-minute host
	// inspection window. Keeping autoscaling alive must not require an audit.
	m.monitorMu.Lock()
	entry.FullChecked = time.Now().Add(-time.Hour)
	m.monitorMu.Unlock()
	nextRuntimeHealth(t, m, active.ID)
	if _, ok := m.recentRuntimeExecution(active, time.Now()); !ok {
		t.Fatal("fresh identity-bound workload rejected because host audit aged")
	}
	if runtimeInspectionCount(f) != 1 {
		t.Fatal("health sampling revived periodic host inspection")
	}

	policy := runtimeScalingPolicy{InstallID: id, Min: 1, Max: 3, TargetLoad: 1, Enabled: true}
	s.store.db.Exec(`INSERT INTO app_runtime_scaling(install_id,record_json) VALUES(?,?)`, id, runtimeJSON(policy))
	for i := 0; i < 3; i++ {
		m.deploymentSummary(id)
		m.autoscale()
	}
	if runtimeInspectionCount(f) != 1 || health.Load() != 6 {
		t.Fatal("UI/autoscaling performed duplicate network probes")
	}
	if _, ok := m.recentRuntimeExecution(active, time.Now().Add(runtimeObservationMaxAge+time.Second)); ok {
		t.Fatal("stale workload/health accepted")
	}
	// Work load follows the lightweight response independently of the last host
	// sample, and disappears immediately when the app stops reporting it.
	f.mu.Lock()
	updated := f.executions[active.ID]
	nextLoad := 3.0
	updated.Load = &nextLoad
	f.executions[active.ID] = updated
	f.mu.Unlock()
	nextRuntimeHealth(t, m, active.ID)
	if sample, ok := m.recentRuntimeExecution(active, time.Now()); !ok || *sample.Load != 3 {
		t.Fatal("new app work load not sampled without host inspection")
	}
	f.mu.Lock()
	updated.Load = nil
	f.executions[active.ID] = updated
	f.mu.Unlock()
	nextRuntimeHealth(t, m, active.ID)
	if _, ok := m.recentRuntimeExecution(active, time.Now()); ok {
		t.Fatal("old work load retained after app stopped reporting")
	}
	if runtimeInspectionCount(f) != 1 {
		t.Fatal("work load refresh inspected host")
	}
	// Restart discards cached verification; persisted UI health never authorizes scaling.
	fresh := &appRuntimeManager{s: s, ctx: m.ctx}
	if _, ok := fresh.recentRuntimeExecution(active, time.Now()); ok {
		t.Fatal("new controller trusted persisted monitoring as execution authority")
	}
}

func TestManagedRuntimeMonitorEscalatesFailureAndRejectsIdentityChange(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	var unhealthy atomic.Bool
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") && unhealthy.Load() {
			w.WriteHeader(503)
			return
		}
		original.ServeHTTP(w, r)
	})
	collectRuntimeMonitor(t, m, id)
	before := runtimeInspectionCount(f)
	unhealthy.Store(true)
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before {
		t.Fatal("transient health failure inspected the host")
	}
	if m.deploymentSummary(id).State != "unavailable" {
		t.Fatal("health failure hidden")
	}
	if _, ok := m.recentRuntimeExecution(active, time.Now()); ok {
		t.Fatal("failed health authorized autoscaling")
	}
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before {
		t.Fatal("second transient health failure inspected the host")
	}
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before+1 {
		t.Fatal("persistent health failure not investigated")
	}
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before+1 {
		t.Fatal("persistent outage caused repeated host inspections")
	}
	unhealthy.Store(false)
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before+1 {
		t.Fatal("healthy recovery after verified ownership needlessly inspected host")
	}
	f.mu.Lock()
	out := f.executions[active.ID]
	out.Handle = "wrong-execution"
	f.executions[active.ID] = out
	f.mu.Unlock()
	collectRuntimeMonitor(t, m, id)
	if m.deploymentSummary(id).State != "unavailable" {
		t.Fatal("wrong execution identity reported healthy")
	}
	current, _ := m.active(id)
	if current.ID != active.ID || current.Execution.Handle != active.Execution.Handle {
		t.Fatal("observation changed ownership")
	}
	before = runtimeInspectionCount(f)
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before {
		t.Fatal("backend verification failure ignored retry backoff")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if strings.HasPrefix(event, "start:") && event != "start:"+active.ID {
			t.Fatal("failed observation started a replacement")
		}
	}
}

func TestManagedRuntimeMonitorDiscardsOldGenerationResult(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	entered, release := make(chan struct{}), make(chan struct{})
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/inspect") {
			close(entered)
			<-release
		}
		original.ServeHTTP(w, r)
	})
	m.queueRuntimeObservations(id, true)
	m.dispatchRuntimeObservations(time.Now())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inspection not started")
	}
	// Simulate a generation switch while a slow response is in transit.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET runtime_generation=runtime_generation+1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitRuntimeMonitors(t, m)
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_observations WHERE deployment_id=?`, active.ID).Scan(&n)
	if n != 0 {
		t.Fatal("old generation published a current health observation")
	}
	if _, ok := m.recentRuntimeExecution(active, time.Now()); ok {
		t.Fatal("old generation entered autoscaling cache")
	}
	m.queueRuntimeObservations(0, false)
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if len(m.monitors) != 0 {
		t.Fatal("retired generation cache not pruned")
	}
}

func TestManagedRuntimeMonitorStaggerAndConcurrencyBound(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	for i := 1; i < 10; i++ {
		clone := active
		clone.ID = fmt.Sprintf("replica-%d", i)
		clone.Replica = i
		clone.Execution.Handle = clone.ID
		clone.CredentialEncrypted, _ = Encrypt(s.secret, clone.ID+"-test-token")
		if err := m.saveDeployment(clone); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.executions[clone.ID] = clone.Execution
		f.mu.Unlock()
	}
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/inspect") {
			entered <- struct{}{}
			<-release
		}
		original.ServeHTTP(w, r)
	})
	// Remove the immediate lifecycle invalidation to check startup staggering.
	m.monitorMu.Lock()
	m.monitors = nil
	m.monitorMu.Unlock()
	m.queueRuntimeObservations(0, false)
	m.monitorMu.Lock()
	phases := map[int64]bool{}
	for _, e := range m.monitors {
		phases[e.NextHealth.UnixNano()] = true
		e.NextHealth = time.Time{}
		e.NextFull = time.Now().Add(-time.Second)
	}
	m.monitorMu.Unlock()
	if len(phases) < 8 {
		t.Fatal("deployment checks not staggered")
	}
	m.dispatchRuntimeObservations(time.Now())
	m.dispatchRuntimeObservations(time.Now())
	for i := 0; i < runtimeMonitorConcurrency; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("bounded inspection did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("global concurrency limit exceeded")
	default:
	}
	m.monitorMu.Lock()
	busy := 0
	for _, e := range m.monitors {
		if e.InFlight {
			busy++
		}
	}
	m.monitorMu.Unlock()
	if busy != runtimeMonitorConcurrency {
		t.Fatalf("in-flight jobs=%d", busy)
	}
	close(release)
	waitRuntimeMonitors(t, m)
}

func TestManagedRuntimeFullInspectionsSerializeAndHonorCancellation(t *testing.T) {
	s := newTestServer(t)
	m := s.appRuntimeManager()
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		writeJSON(w, runtimeExecution{State: "running"})
	}))
	defer backend.Close()
	b := remoteRuntimeBackend{manager: m, registration: runtimeBackendRegistration{Endpoint: runtimeControlPrefix + "backends/test"}, base: backend.URL}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = b.Inspect(runtimeBackendRequest{DeploymentID: "same"}) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.inspectContext(ctx, runtimeBackendRequest{DeploymentID: "same"}); err == nil {
		t.Fatal("overlapping inspection ignored cancellation")
	}
	if requests.Load() != 1 {
		t.Fatal("overlapping full inspection reached backend")
	}
	close(release)
	wg.Wait()
	if _, err := b.Inspect(runtimeBackendRequest{DeploymentID: "same"}); err != nil {
		t.Fatal(err)
	}
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if len(m.inspectionLocks) != 0 {
		t.Fatal("inspection locks leaked")
	}
}

func TestManagedRuntimeMonitorInvalidatesOnLifecycleChange(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	old := activateRuntimeFixture(t, m, id, "activate")
	collectRuntimeMonitor(t, m, id)
	op, err := m.begin(id, "restart", "restart-monitor-test", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitRuntimeOperation(t, m, op); result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	current, err := m.active(id)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Second)
	for {
		m.monitorMu.Lock()
		entry := m.monitors[current.ID]
		_, oldPresent := m.monitors[old.ID]
		ready := entry != nil && !entry.FullOK && entry.FullChecked.IsZero() && !entry.NextFull.After(time.Now()) && !oldPresent
		m.monitorMu.Unlock()
		if ready {
			break
		}
		if time.Now().After(end) {
			t.Fatal("lifecycle did not request immediate fresh verification")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := m.recentRuntimeExecution(current, time.Now()); ok {
		t.Fatal("new generation inherited old verified load")
	}
	// An operation starting between a probe and publication invalidates its result.
	fake := runtimeOperation{ID: "monitor-overlap", InstallID: id, Kind: "stop", Status: "queued"}
	_, err = s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,idempotency_key,fingerprint,record_json) VALUES(?,?,'stop','queued',?,'',?)`, fake.ID, id, fake.ID, runtimeJSON(fake))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := m.persistRuntimeObservation(current, runtimeHealthObservation{State: "healthy", Healthy: true}, &current.Execution)
	if err != nil || stored {
		t.Fatalf("in-progress operation accepted stale probe: stored=%v err=%v", stored, err)
	}
	m.queueRuntimeObservations(id, false)
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if len(m.monitors) != 0 {
		t.Fatal("changing installation retained its monitoring cache")
	}
}

func TestManagedRuntimeMonitorOptionalAudit(t *testing.T) {
	for _, tc := range []struct {
		interval string
		enabled  bool
	}{{"", false}, {"0", false}, {"1m", false}, {"invalid", false}, {"20m", true}, {"30m", true}, {"1h", false}} {
		t.Run(tc.interval, func(t *testing.T) {
			t.Setenv("APTEVA_RUNTIME_INSPECTION_AUDIT_INTERVAL", tc.interval)
			_, m, f, id, p := runtimeTestSetup(t)
			prepareRuntimeFixture(t, m, id, p, "prepare")
			active := activateRuntimeFixture(t, m, id, "activate")
			collectRuntimeMonitor(t, m, id)
			m.monitorMu.Lock()
			next := m.monitors[active.ID].NextFull
			m.monitorMu.Unlock()
			if next.IsZero() == tc.enabled {
				t.Fatalf("audit enabled=%v next=%v", tc.enabled, next)
			}
			if tc.enabled {
				if time.Until(next) < 15*time.Minute || time.Until(next) > 30*time.Minute {
					t.Fatal("audit outside infrequent range")
				}
				m.monitorMu.Lock()
				m.monitors[active.ID].NextFull = time.Now().Add(-time.Second)
				m.monitorMu.Unlock()
				before := runtimeInspectionCount(f)
				nextRuntimeHealth(t, m, active.ID)
				if runtimeInspectionCount(f) != before+1 {
					t.Fatal("opt-in audit did not inspect")
				}
			}
		})
	}
}

func TestManagedRuntimeMonitorTransientFailureAvoidsInspection(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	var unhealthy atomic.Bool
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") && unhealthy.Load() {
			w.WriteHeader(503)
			return
		}
		original.ServeHTTP(w, r)
	})
	collectRuntimeMonitor(t, m, id)
	before := runtimeInspectionCount(f)
	unhealthy.Store(true)
	nextRuntimeHealth(t, m, active.ID)
	unhealthy.Store(false)
	nextRuntimeHealth(t, m, active.ID)
	if runtimeInspectionCount(f) != before {
		t.Fatal("transient failure/recovery inspected the host")
	}
	if m.deploymentSummary(id).State != "healthy" {
		t.Fatal("cheap health did not recover")
	}
}

func TestManagedRuntimeWorkloadRequiresFreshAppIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		valid  bool
		status func(runtimeDeployment) map[string]any
	}{
		{"valid", true, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": d.ID, "generation": d.Generation, "admitting": true, "load": 5}
		}},
		{"legacy status", false, func(d runtimeDeployment) map[string]any { return map[string]any{"admitting": true, "load": 5} }},
		{"missing load", false, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": d.ID, "generation": d.Generation, "admitting": true}
		}},
		{"wrong deployment", false, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": "other", "generation": d.Generation, "admitting": true, "load": 5}
		}},
		{"wrong generation", false, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": d.ID, "generation": d.Generation + 1, "admitting": true, "load": 5}
		}},
		{"negative load", false, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": d.ID, "generation": d.Generation, "admitting": true, "load": -1}
		}},
		{"not admitting", false, func(d runtimeDeployment) map[string]any {
			return map[string]any{"deployment_id": d.ID, "generation": d.Generation, "admitting": false, "load": 5}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			prepareRuntimeFixture(t, m, id, p, "prepare")
			active := activateRuntimeFixture(t, m, id, "activate")
			// Even a fresh host sample must not substitute for missing app work load.
			f.mu.Lock()
			execution := f.executions[active.ID]
			load := 5.0
			execution.Load = &load
			f.executions[active.ID] = execution
			f.mu.Unlock()
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/app/status") {
					writeJSON(w, tc.status(active))
					return
				}
				original.ServeHTTP(w, r)
			})
			collectRuntimeMonitor(t, m, id)
			out, ok := m.recentRuntimeExecution(active, time.Now())
			if ok != tc.valid || ok && (out.Load == nil || *out.Load != 5) {
				t.Fatalf("workload accepted=%v, want %v", ok, tc.valid)
			}
			if !tc.valid {
				before := runtimeInspectionCount(f)
				policy := runtimeScalingPolicy{InstallID: id, Min: 1, Max: 3, TargetLoad: 1, Enabled: true}
				s.store.db.Exec(`INSERT INTO app_runtime_scaling(install_id,record_json) VALUES(?,?)`, id, runtimeJSON(policy))
				m.autoscale()
				var count int
				s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_operations WHERE kind='scale'`).Scan(&count)
				if count != 0 || runtimeInspectionCount(f) != before {
					t.Fatal("missing/untrusted workload forced inspection or scaling")
				}
			}
		})
	}
}
