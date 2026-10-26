package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type ownershipLease struct {
	Active       bool   `json:"active"`
	DeploymentID string `json:"deployment_id"`
	Generation   int64  `json:"generation"`
	ExpiresAt    int64  `json:"expires_at"`
}

// Unlike the simple backend fixture, activate only arms this app. Admission
// requires a lease fetched through Server's real authentication middleware and
// ownership callback. No test directly grants admission on activation.
type ownershipFixture struct {
	t              *testing.T
	s              *Server
	f              *runtimeBackendFixture
	callback       *httptest.Server
	mu             sync.Mutex
	armed          map[string]bool
	polls          map[string]int
	beforeActivate func(string)
	statusFailure  string
	failureID      string
	blockDrain     bool
	drainObserved  chan struct{}
}

func newOwnershipFixture(t *testing.T, s *Server, f *runtimeBackendFixture) *ownershipFixture {
	t.Helper()
	x := &ownershipFixture{t: t, s: s, f: f, armed: map[string]bool{}, polls: map[string]int{}, drainObserved: make(chan struct{}, 1)}
	x.callback = httptest.NewServer(s.authMiddleware(s.handleRuntimeOwnership))
	t.Cleanup(x.callback.Close)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/exec/") {
			original.ServeHTTP(w, r)
			return
		}
		dep := strings.Split(strings.TrimPrefix(r.URL.Path, "/exec/"), "/")[0]
		if strings.HasSuffix(r.URL.Path, "/activate") {
			x.mu.Lock()
			hook := x.beforeActivate
			x.armed[dep] = true
			x.mu.Unlock()
			if hook != nil {
				hook(dep)
			}
			w.WriteHeader(200)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/drain") {
			x.mu.Lock()
			blocked := x.blockDrain
			x.mu.Unlock()
			select {
			case x.drainObserved <- struct{}{}:
			default:
			}
			if blocked {
				writeJSON(w, map[string]any{"drained": false, "active_work": 1})
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/app/status") {
			f.mu.Lock()
			generation := f.env[dep]["APTEVA_DEPLOYMENT_GENERATION"]
			token := f.env[dep]["APTEVA_APP_TOKEN"]
			running := f.executions[dep].State == "running"
			f.mu.Unlock()
			var gen int64
			fmt.Sscan(generation, &gen)
			lease, _ := x.lease(token)
			x.mu.Lock()
			admit := x.armed[dep] && running && lease.Active && lease.DeploymentID == dep && lease.Generation == gen && lease.ExpiresAt > time.Now().Unix()
			x.polls[dep]++
			failure := x.statusFailure
			if x.failureID != "" && x.failureID != dep {
				failure = ""
			}
			x.mu.Unlock()
			state := "standby"
			if admit {
				state = "active"
			}
			switch failure {
			case "malformed":
				_, _ = w.Write([]byte(`{"admitting":`))
				return
			case "wrong-generation":
				gen++
			case "wrong-deployment":
				dep = "other-execution"
			case "fenced":
				state = "fenced"
				admit = false
			case "standby":
				state = "standby"
				admit = false
			}
			writeJSON(w, map[string]any{"deployment_id": dep, "generation": gen, "admitting": admit, "state": state})
			return
		}
		original.ServeHTTP(w, r)
	})
	return x
}
func (x *ownershipFixture) lease(token string) (ownershipLease, int) {
	req, _ := http.NewRequest("GET", x.callback.URL+"/apps/callback/runtime/ownership", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := x.callback.Client().Do(req)
	if err != nil {
		return ownershipLease{}, 0
	}
	defer response.Body.Close()
	var lease ownershipLease
	_ = json.NewDecoder(response.Body).Decode(&lease)
	return lease, response.StatusCode
}
func (x *ownershipFixture) token(d runtimeDeployment) string {
	token, err := Decrypt(x.s.secret, d.CredentialEncrypted)
	if err != nil {
		x.t.Fatal(err)
	}
	return token
}
func exclusiveOwnershipManifest(t *testing.T, m *appRuntimeManager, id int64) {
	t.Helper()
	manifest, err := installManifest(m.s, id)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runtime.ActivationStrategy = "exclusive"
	manifest.Runtime.StartupTimeoutSeconds = 1
	if _, err = m.s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(manifest), id); err != nil {
		t.Fatal(err)
	}
}
func requireNoPublishedCandidate(t *testing.T, m *appRuntimeManager, id int64, candidate string) {
	t.Helper()
	var active, status, url string
	if err := m.s.store.db.QueryRow(`SELECT runtime_active_deployment,status,sidecar_url_override FROM app_installs WHERE id=?`, id).Scan(&active, &status, &url); err != nil {
		t.Fatal(err)
	}
	if active == candidate || strings.Contains(url, candidate) {
		t.Fatalf("candidate published before admission: %s %s", active, status)
	}
}
func requireLease(t *testing.T, x *ownershipFixture, d runtimeDeployment, want bool) {
	t.Helper()
	lease, code := x.lease(x.token(d))
	if want && (code != 200 || !lease.Active || lease.DeploymentID != d.ID || lease.Generation != d.Generation) {
		t.Fatalf("owner lease missing: HTTP %d %+v", code, lease)
	}
	if !want && lease.Active {
		t.Fatalf("unowned execution received lease: %+v", lease)
	}
}

func TestManagedRuntimeOwnershipFirstAndStoppedInstall(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			exclusiveOwnershipManifest(t, m, id)
			x := newOwnershipFixture(t, s, f)
			if stopped {
				op, err := m.begin(id, "deploy", "initial", p, nil)
				if err != nil {
					t.Fatal(err)
				}
				if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
					t.Fatal(done.Error)
				}
				if err = m.stopSync(id, false); err != nil {
					t.Fatal(err)
				}
			}
			x.mu.Lock()
			x.beforeActivate = func(dep string) {
				requireNoPublishedCandidate(t, m, id, dep)
				d, err := m.loadDeployment(dep)
				if err != nil {
					t.Error(err)
					return
				}
				requireLease(t, x, d, true)
			}
			x.mu.Unlock()
			op, err := m.begin(id, "deploy", "candidate", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := waitRuntimeOperation(t, m, op)
			if done.Status != "succeeded" {
				t.Fatal(done.Error)
			}
			d, err := m.active(id)
			if err != nil {
				t.Fatal(err)
			}
			requireLease(t, x, d, true)
			if h := m.runtimeAppHealth(d, d.Execution); !h.Admitting || !h.IdentityVerified {
				t.Fatalf("not admitted: %+v", h)
			}
		})
	}
}

func TestManagedRuntimeOwnershipDrainBeforeTransfer(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	exclusiveOwnershipManifest(t, m, id)
	x := newOwnershipFixture(t, s, f)
	op, err := m.begin(id, "deploy", "initial", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	old, _ := m.active(id)
	x.mu.Lock()
	x.blockDrain = true
	x.mu.Unlock()
	op, err = m.begin(id, "upgrade", "with-active-work", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-x.drainObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("drain was not requested")
	}
	candidate, _ := m.loadDeployment(op.DeploymentID)
	requireLease(t, x, old, true)
	requireLease(t, x, candidate, false)
	f.mu.Lock()
	events := append([]string(nil), f.events...)
	f.mu.Unlock()
	for _, event := range events {
		if event == "stop:"+old.ID || event == "start:"+candidate.ID {
			t.Fatal("active work forcibly interrupted", events)
		}
	}
	x.mu.Lock()
	x.blockDrain = false
	x.mu.Unlock()
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	d, _ := m.active(id)
	requireLease(t, x, old, false)
	requireLease(t, x, d, true)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.executions[old.ID].StoppedConfirmed {
		t.Fatal("old execution not confirmed stopped")
	}
}

// Assemble a durable ready candidate with a claimed operation but no running
// controller goroutine. Each phase can then be resumed by a fresh manager.
func ownershipReadyCandidate(t *testing.T, m *appRuntimeManager, id int64, p runtimePlacement) (runtimeOperation, runtimeDeployment) {
	t.Helper()
	d := prepareRuntimeFixture(t, m, id, p, "prepared")
	op := runtimeOperation{ID: "controlled-activation", InstallID: id, Kind: "activate", Status: "running", Phase: "starting_candidate", DeploymentID: d.ID, IdempotencyKey: "controlled", CreatedAt: runtimeTimestamp()}
	old, _ := m.active(id)
	op.PreviousID = old.ID
	op.PreviousStopped = old.Status == "stopped"
	_, err := m.s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,lease_until,idempotency_key,fingerprint,record_json) VALUES(?,?,?,'running',?,?,?,'',?)`, op.ID, id, op.Kind, m.owner, time.Now().Unix()+45, op.IdempotencyKey, runtimeJSON(op))
	if err != nil {
		t.Fatal(err)
	}
	if old.ID != "" && old.Status != "stopped" {
		if err = m.drain(old, op); err != nil {
			t.Fatal(err)
		}
		if err = m.stopDeployment(&old, op); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.startDeployment(&d, op); err != nil {
		t.Fatal(err)
	}
	return op, d
}
func TestManagedRuntimeOwnershipCASGuards(t *testing.T) {
	for _, change := range []string{"operation-owner", "operation-expired", "predecessor", "pending", "generation", "candidate-generation", "capabilities", "wrong-install"} {
		t.Run(change, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			x := newOwnershipFixture(t, s, f)
			op, d := ownershipReadyCandidate(t, m, id, p)
			switch change {
			case "operation-owner":
				s.store.db.Exec(`UPDATE app_runtime_operations SET owner='another-controller' WHERE id=?`, op.ID)
			case "operation-expired":
				s.store.db.Exec(`UPDATE app_runtime_operations SET lease_until=0 WHERE id=?`, op.ID)
			case "predecessor":
				s.store.db.Exec(`UPDATE app_installs SET runtime_active_deployment='new-owner' WHERE id=?`, id)
			case "pending":
				s.store.db.Exec(`UPDATE app_installs SET runtime_pending_deployment='new-candidate' WHERE id=?`, id)
			case "generation":
				s.store.db.Exec(`UPDATE app_installs SET runtime_generation=runtime_generation+1 WHERE id=?`, id)
			case "candidate-generation":
				d.Generation++
			case "capabilities":
				d.AppCaps.Drain = !d.AppCaps.Drain
			case "wrong-install":
				d.InstallID++
			}
			if err := m.commitExecutionOwnership(d, op); err == nil {
				t.Fatal("lost CAS granted ownership")
			}
			requireLease(t, x, d, false)
			if err := m.activateAndConfirm(d, op); err == nil {
				t.Fatal("unowned candidate activated")
			}
			if err := m.commitActive(d, op); err == nil {
				t.Fatal("unowned candidate published")
			}
		})
	}
}
func TestManagedRuntimeOwnershipFailedAdmissionAndDisabledRollback(t *testing.T) {
	for _, mode := range []string{"standby", "malformed", "wrong-generation", "wrong-deployment", "fenced"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			exclusiveOwnershipManifest(t, m, id)
			x := newOwnershipFixture(t, s, f)
			op, err := m.begin(id, "deploy", "initial", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
				t.Fatal(done.Error)
			}
			old, _ := m.active(id)
			if err = m.stopSync(id, false); err != nil {
				t.Fatal(err)
			}
			x.mu.Lock()
			x.statusFailure = mode
			x.mu.Unlock()
			op, err = m.begin(id, "upgrade", "fail-admission", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRuntimeOperation(t, m, op); done.Status != "failed" {
				t.Fatal("bad status accepted")
			}
			candidate, _ := m.loadDeployment(op.DeploymentID)
			requireLease(t, x, candidate, false)
			requireLease(t, x, old, false)
			var status string
			s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status)
			if status != "disabled" {
				t.Fatal("disabled installation restarted", status)
			}
			requireNoPublishedCandidate(t, m, id, candidate.ID)
			if !candidate.Execution.StoppedConfirmed {
				t.Fatal("candidate shutdown not confirmed")
			}
		})
	}
}
func TestManagedRuntimeOwnershipCrashRecoveryPhases(t *testing.T) {
	for _, phase := range []string{"passive_ready", "grant_written", "ownership_committed", "admitted", "publishing", "published"} {
		t.Run(phase, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			exclusiveOwnershipManifest(t, m, id)
			x := newOwnershipFixture(t, s, f)
			op, d := ownershipReadyCandidate(t, m, id, p)
			if phase != "passive_ready" {
				if err := m.commitExecutionOwnership(d, op); err != nil {
					t.Fatal(err)
				}
				if phase != "grant_written" {
					if err := m.phase(&op, "ownership_committed"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if phase == "admitted" || phase == "publishing" || phase == "published" {
				if err := m.activateAndConfirm(d, op); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "publishing" || phase == "published" {
				if err := m.phase(&op, "publishing"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "published" {
				if err := m.commitActive(d, op); err != nil {
					t.Fatal(err)
				}
			}
			m.Close()
			if err := s.store.migrateAppRuntime(); err != nil {
				t.Fatal(err)
			}
			s.store.db.Exec(`UPDATE app_runtime_operations SET lease_until=0 WHERE id=?`, op.ID)
			ctx, cancel := context.WithCancel(context.Background())
			replacement := &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: "replacement-controller", http: m.http, workers: map[string]bool{}}
			s.appRuntime = replacement
			t.Cleanup(replacement.Close)
			replacement.reconcile()
			done := waitRuntimeOperation(t, replacement, op)
			if done.Status != "succeeded" {
				t.Fatal(done.Error)
			}
			active, _ := replacement.active(id)
			if active.ID != d.ID {
				t.Fatal("recovery created a different owner")
			}
			requireLease(t, x, active, true)
			f.mu.Lock()
			starts := 0
			for _, event := range f.events {
				if event == "start:"+d.ID {
					starts++
				}
			}
			f.mu.Unlock()
			if starts != 1 {
				t.Fatalf("started %d processes", starts)
			}
		})
	}
}
func TestManagedRuntimeOwnershipCallbackCredentialsAndRevocation(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	op, d := ownershipReadyCandidate(t, m, id, p)
	requireLease(t, x, d, false) // pending and ready alone are not permission
	if lease, _ := x.lease("stale-token"); lease.Active {
		t.Fatal("stale credentials granted")
	}
	if err := m.commitExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, true)
	if err := m.revokeExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, false)
	if err := s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, false)
	// Grant/status must agree on the exact generation even for a valid token.
	s.store.db.Exec(`UPDATE app_runtime_execution_owners SET granted=1,generation=generation+1 WHERE deployment_id=?`, d.ID)
	requireLease(t, x, d, false)
}

func TestManagedRuntimeOwnershipOwnedStandbyReadmission(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	op, err := m.begin(id, "deploy", "initial", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	d, _ := m.active(id)
	x.mu.Lock()
	x.armed[d.ID] = false
	x.mu.Unlock()
	collectRuntimeMonitor(t, m, id)
	nextRuntimeHealth(t, m, d.ID)
	var opID string
	if err = s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE kind='readmit'`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	done := waitRuntimeOperation(t, m, runtimeOperation{ID: opID})
	if done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	active, _ := m.active(id)
	if active.ID != d.ID {
		t.Fatal("standby recovery replaced owner")
	}
	if h := m.runtimeAppHealth(active, active.Execution); !h.Admitting {
		t.Fatal("not readmitted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	starts := 0
	for _, e := range f.events {
		if strings.HasPrefix(e, "start:") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal("duplicate process")
	}
}

// Keep controller cancellation distinct from a failed activation. A crash must
// leave a durable pending operation, not authorize rollback under an old lease.
func TestManagedRuntimeOwnershipCancelledControllerCannotPublish(t *testing.T) {
	_, m, _, id, p := runtimeTestSetup(t)
	op, d := ownershipReadyCandidate(t, m, id, p)
	if err := m.commitExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	m.cancel()
	if err := m.commitActive(d, op); err == nil {
		t.Fatal("cancelled controller published")
	}
	if err := m.revokeExecutionOwnership(d, op); err == nil {
		t.Fatal("cancelled controller revoked successor's grant")
	}
	if m.ctx.Err() != context.Canceled {
		t.Fatal("controller context not cancelled")
	}
}

func TestManagedRuntimeOwnershipRollbackAfterGrant(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			exclusiveOwnershipManifest(t, m, id)
			x := newOwnershipFixture(t, s, f)
			op, err := m.begin(id, "deploy", "initial", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
				t.Fatal(done.Error)
			}
			old, _ := m.active(id)
			x.mu.Lock()
			x.beforeActivate = func(dep string) {
				x.mu.Lock()
				first := x.failureID == ""
				if first {
					x.failureID = dep
					x.statusFailure = "fenced"
				}
				x.mu.Unlock()
				if first && !confirmed {
					f.mu.Lock()
					f.failStop = true
					f.mu.Unlock()
				}
			}
			x.mu.Unlock()
			op, err = m.begin(id, "upgrade", "failed-candidate", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := waitRuntimeOperation(t, m, op)
			if done.Status != "failed" {
				t.Fatal("failed upgrade reported success")
			}
			failed, _ := m.loadDeployment(op.DeploymentID)
			requireLease(t, x, failed, false)
			requireLease(t, x, old, false)
			active, _ := m.active(id)
			if confirmed {
				if done.RollbackID == "" || active.ID != done.RollbackID {
					t.Fatal("verified rollback was not published", done.Error)
				}
				requireLease(t, x, active, true)
				if !failed.Execution.StoppedConfirmed {
					t.Fatal("rollback started without confirmed stop")
				}
			} else {
				if done.RollbackID != "" || active.ID != old.ID {
					t.Fatal("rollback started with unconfirmed shutdown")
				}
				requireNoPublishedCandidate(t, m, id, failed.ID)
			}
		})
	}
}

func TestManagedRuntimeOwnershipRollbackCrashRecovery(t *testing.T) {
	for _, phase := range []string{"rollback_pending", "candidate_revoked", "candidate_stopped", "rollback_owned", "rollback_admitted", "rollback_published"} {
		t.Run(phase, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			exclusiveOwnershipManifest(t, m, id)
			x := newOwnershipFixture(t, s, f)
			initial, err := m.begin(id, "deploy", "initial", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRuntimeOperation(t, m, initial); done.Status != "succeeded" {
				t.Fatal(done.Error)
			}
			op, candidate := ownershipReadyCandidate(t, m, id, p)
			if err = m.commitExecutionOwnership(candidate, op); err != nil {
				t.Fatal(err)
			}
			if err = m.activateAndConfirm(candidate, op); err != nil {
				t.Fatal(err)
			}
			op.RollbackRequested = true
			if err = m.phase(&op, "rolling_back"); err != nil {
				t.Fatal(err)
			}
			if phase != "rollback_pending" {
				if err = m.revokeExecutionOwnership(candidate, op); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "candidate_stopped" || strings.HasPrefix(phase, "rollback_o") || phase == "rollback_admitted" || phase == "rollback_published" {
				if err = m.stopDeployment(&candidate, op); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "rollback_owned" || phase == "rollback_admitted" || phase == "rollback_published" {
				old, _ := m.loadDeployment(op.PreviousID)
				x.mu.Lock()
				x.beforeActivate = func(_ string) { m.cancel() }
				x.mu.Unlock()
				if err = m.rollbackRemoteDeployment(&op, old, candidate, fmt.Errorf("injected failure")); err == nil {
					t.Fatal("interrupted rollback reported success")
				}
				if op.RollbackID == "" {
					t.Fatal("recovery owner was not journaled")
				}
				x.mu.Lock()
				x.beforeActivate = nil
				x.mu.Unlock()
				if phase == "rollback_admitted" || phase == "rollback_published" {
					m.Close()
					ctx, cancel := context.WithCancel(context.Background())
					m = &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: "rollback-phase-controller", http: m.http, workers: map[string]bool{}}
					s.appRuntime = m
					t.Cleanup(m.Close)
					s.store.db.Exec(`UPDATE app_runtime_operations SET owner=?,lease_until=? WHERE id=?`, m.owner, time.Now().Unix()+45, op.ID)
					op, err = m.operation(op.ID)
					if err != nil {
						t.Fatal(err)
					}
					recovery, e := m.loadDeployment(op.RollbackID)
					if e != nil {
						t.Fatal(e)
					}
					if e = m.waitReady(&recovery, op); e != nil {
						t.Fatal(e)
					}
					if e = m.commitExecutionOwnership(recovery, op); e != nil {
						t.Fatal(e)
					}
					if e = m.activateAndConfirm(recovery, op); e != nil {
						t.Fatal(e)
					}
					if phase == "rollback_published" {
						if e = m.commitActive(recovery, op); e != nil {
							t.Fatal(e)
						}
					}
				}
			}
			m.Close()
			if err := s.store.migrateAppRuntime(); err != nil {
				t.Fatal(err)
			}
			s.store.db.Exec(`UPDATE app_runtime_operations SET lease_until=0 WHERE id=?`, op.ID)
			ctx, cancel := context.WithCancel(context.Background())
			replacement := &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: "rollback-replacement", http: m.http, workers: map[string]bool{}}
			s.appRuntime = replacement
			t.Cleanup(replacement.Close)
			replacement.reconcile()
			done := waitRuntimeOperation(t, replacement, op)
			if done.Status != "failed" || !strings.Contains(done.Error, "restored and verified") {
				t.Fatalf("rollback not truthfully reported: %+v", done)
			}
			active, _ := replacement.active(id)
			if active.ID == candidate.ID || active.ID != done.RollbackID {
				t.Fatal("wrong rollback owner")
			}
			requireLease(t, x, active, true)
			requireLease(t, x, candidate, false)
			var grants int
			s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_execution_owners WHERE install_id=? AND granted=1`, id).Scan(&grants)
			if grants != 1 {
				t.Fatalf("%d authorized owners", grants)
			}
			f.mu.Lock()
			running := 0
			for _, e := range f.executions {
				if e.State == "running" {
					running++
				}
			}
			f.mu.Unlock()
			if running != 1 {
				t.Fatalf("%d running exclusive executions", running)
			}
		})
	}
}

func TestManagedRuntimeOwnershipLegacyJournalMigration(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	x := newOwnershipFixture(t, s, f)
	if _, err := s.store.db.Exec(`DELETE FROM app_runtime_execution_owners`); err != nil {
		t.Fatal(err)
	}
	if err := s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, true)
	s.store.db.Exec(`UPDATE app_runtime_execution_owners SET granted=0 WHERE deployment_id=?`, d.ID)
	if err := s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, false)
	// The callback cannot infer permission from the still-published active route.
	active, _ := m.active(d.InstallID)
	if active.ID != d.ID {
		t.Fatal("test route changed")
	}
}

func TestManagedRuntimeOwnershipLeaseRenewalAcrossPeriods(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lease renewal across three 15-second periods")
	}
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	op, d := ownershipReadyCandidate(t, m, id, p)
	if err := m.commitExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	if err := m.activateAndConfirm(d, op); err != nil {
		t.Fatal(err)
	}
	if err := m.commitActive(d, op); err != nil {
		t.Fatal(err)
	}
	first, _ := x.lease(x.token(d))
	end := time.Now().Add(46 * time.Second)
	// Renew without changing routing or the operation record. Execution ownership
	// survives the lifecycle operation's lease expiring and a controller restart.
	for time.Now().Before(end) {
		requireLease(t, x, d, true)
		if h := m.runtimeAppHealth(d, d.Execution); !h.Admitting {
			t.Fatal("admission lost during lease renewal")
		}
		time.Sleep(5 * time.Second)
	}
	last, _ := x.lease(x.token(d))
	if last.ExpiresAt <= first.ExpiresAt+30 {
		t.Fatal("ownership lease did not renew")
	}
}

func TestManagedRuntimeOwnershipCannotPublishStandbyGrant(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	op, d := ownershipReadyCandidate(t, m, id, p)
	if err := m.commitExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	requireLease(t, x, d, true)
	if err := m.commitActive(d, op); err == nil {
		t.Fatal("owned standby published")
	}
	if err := m.publishIngress(d, op); err == nil {
		t.Fatal("owned standby ingress published")
	}
	requireNoPublishedCandidate(t, m, id, d.ID)
	if err := m.activateAndConfirm(d, op); err != nil {
		t.Fatal(err)
	}
	if err := m.commitActive(d, op); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRuntimeOwnershipReplicaLeases(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	op, err := m.begin(id, "deploy", "initial", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	primary, _ := m.active(id)
	pool := runtimePool{ID: "ownership-pool", Backend: p.Backend, Targets: []runtimeResourceRef{p.Target, {InstallID: p.Target.InstallID, Type: p.Target.Type, ID: "target-b"}}}
	if _, err = s.store.db.Exec(`INSERT INTO app_runtime_pools(id,record_json) VALUES(?,?)`, pool.ID, runtimeJSON(pool)); err != nil {
		t.Fatal(err)
	}
	primary.Placement.PoolID = pool.ID
	if err = m.saveDeployment(primary); err != nil {
		t.Fatal(err)
	}
	op, err = m.begin(id, "scale", "two-replicas", p, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	ds, _ := m.deployments(id)
	live := 0
	for _, d := range ds {
		if d.Status == "active" {
			live++
			requireLease(t, x, d, true)
			if h := m.runtimeAppHealth(d, d.Execution); !h.Admitting {
				t.Fatal("replica not admitted")
			}
		}
	}
	if live != 2 {
		t.Fatalf("%d replicas", live)
	}
	if err = s.store.migrateAppRuntime(); err != nil {
		t.Fatal(err)
	}
	current, _ := m.active(id)
	if current.ID != primary.ID {
		t.Fatal("scaling moved primary route")
	}
	op, err = m.begin(id, "scale", "one-replica", p, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	for _, d := range ds {
		requireLease(t, x, d, d.ID == primary.ID)
	}
}

func TestManagedRuntimeOwnershipCapabilityMismatchBeforeGrant(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	x := newOwnershipFixture(t, s, f)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/app/describe") {
			writeJSON(w, runtimeAppCaps{PassiveStart: true, Drain: true, Routing: "key"})
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(id, "deploy", "mismatched-release", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRuntimeOperation(t, m, op)
	if done.Status != "failed" || !strings.Contains(done.Error, "capabilities differ") {
		t.Fatal("mismatched capabilities accepted", done.Error)
	}
	d, _ := m.loadDeployment(op.DeploymentID)
	requireLease(t, x, d, false)
	requireNoPublishedCandidate(t, m, id, d.ID)
}

func TestManagedRuntimeOwnershipGenerationNotReusedAfterRollback(t *testing.T) {
	_, m, _, id, p := runtimeTestSetup(t)
	_, d := ownershipReadyCandidate(t, m, id, p)
	// A local rollback publishes a previous, lower generation. The next candidate
	// must advance past all retained deployment identities, including failures.
	if _, err := m.s.store.db.Exec(`UPDATE app_installs SET runtime_generation=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	next, err := m.makeDeployment(id, p, &d.Manifest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation <= d.Generation {
		t.Fatal("generation reused after rollback")
	}
}

func TestManagedRuntimeOwnershipPublishedCrashDoesNotHideLostAdmission(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	exclusiveOwnershipManifest(t, m, id)
	x := newOwnershipFixture(t, s, f)
	op, d := ownershipReadyCandidate(t, m, id, p)
	if err := m.commitExecutionOwnership(d, op); err != nil {
		t.Fatal(err)
	}
	if err := m.activateAndConfirm(d, op); err != nil {
		t.Fatal(err)
	}
	if err := m.commitActive(d, op); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	x.statusFailure = "standby"
	x.mu.Unlock()
	m.Close()
	s.store.db.Exec(`UPDATE app_runtime_operations SET lease_until=0 WHERE id=?`, op.ID)
	ctx, cancel := context.WithCancel(context.Background())
	replacement := &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: "published-recovery", http: m.http, workers: map[string]bool{}}
	s.appRuntime = replacement
	t.Cleanup(replacement.Close)
	replacement.reconcile()
	done := waitRuntimeOperation(t, replacement, op)
	if done.Status != "failed" || !strings.Contains(done.Error, "admission requires recovery") {
		t.Fatal("stale publication reported success", done.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	starts := 0
	for _, event := range f.events {
		if strings.HasPrefix(event, "start:") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal("lost admission created a duplicate process")
	}
}
