package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagedRuntimeExclusiveRestartStopsBeforeBackendPreparation(t *testing.T) {
	_, m, f, d := recoveryFixture(t)
	d.Manifest.Runtime.ActivationStrategy = "exclusive"
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.rejectConcurrentPrepare = true
	f.mu.Unlock()
	op, err := m.begin(d.InstallID, "restart", "exclusive-restart", d.Placement, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatal(op.Status, op.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stop, prepare := -1, -1
	for i, e := range f.events {
		if e == "stop:"+d.ID {
			stop = i
		}
		if e == "prepare:"+op.DeploymentID {
			prepare = i
		}
	}
	if stop < 0 || prepare <= stop {
		t.Fatal("prepare preceded confirmed stop", f.events)
	}
	if op.StartedAt == "" || op.PhaseStartedAt == "" {
		t.Fatal("missing progress timestamps")
	}
}

func TestManagedRuntimeDrainCancellationInterruptsHTTP(t *testing.T) {
	_, m, f, d := recoveryFixture(t)
	entered := make(chan struct{}, 1)
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/drain") {
			io.Copy(io.Discard, r.Body)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(200)
	}))
	defer blocking.Close()
	f.mu.Lock()
	out := f.executions[d.ID]
	out.Endpoint = blocking.URL
	f.executions[d.ID] = out
	f.mu.Unlock()
	op, err := m.begin(d.InstallID, "stop", "cancel-blocked-drain", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain not reached")
	}
	progress, err := m.operation(op.ID)
	if err != nil || progress.Status != "running" {
		t.Fatal("executing operation reported queued", progress, err)
	}
	started := time.Now()
	if err := m.cancelOperation(op.ID, d.InstallID); err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "cancelled" || time.Since(started) > 2*time.Second {
		t.Fatal("cancellation was not prompt", op.Status, op.Error, time.Since(started))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.executions[d.ID].State != "running" {
		t.Fatal("cancelled drain stopped live execution")
	}
}

func TestManagedRuntimeDrainUsesFreshEndpointAndStoppedEvidence(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "refreshed endpoint", true: "confirmed stopped"}[stopped], func(t *testing.T) {
			_, m, f, d := recoveryFixture(t)
			d.Execution.Endpoint = "http://127.0.0.1:1"
			if err := m.saveDeployment(d); err != nil {
				t.Fatal(err)
			}
			if stopped {
				f.mu.Lock()
				out := f.executions[d.ID]
				out.State = "stopped"
				out.StoppedConfirmed = true
				out.Ready = false
				f.executions[d.ID] = out
				f.mu.Unlock()
			}
			op, err := m.begin(d.InstallID, "stop", "fresh-drain", runtimePlacement{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			op = waitRuntimeOperation(t, m, op)
			if op.Status != "succeeded" {
				t.Fatal(op.Status, op.Error)
			}
		})
	}
}

func TestManagedRuntimeFencedRecoveryDoesNotWaitForStaleCallRows(t *testing.T) {
	_, m, f, d := recoveryFixture(t)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/exec/"+d.ID+"/") && strings.HasSuffix(r.URL.Path, "/drain") {
			t.Error("fenced predecessor was asked to drain stale call rows")
			writeJSON(w, map[string]bool{"drained": false})
			return
		}
		original.ServeHTTP(w, r)
	})
	fencedFixture(f, d, "fenced", d.ID, "restart", 503)
	collectRuntimeMonitor(t, m, d.InstallID)
	nextRuntimeHealth(t, m, d.ID)
	var id string
	if err := m.s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND idempotency_key LIKE 'recover-%'`, d.InstallID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	op, err := m.operation(id)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatal(op.Status, op.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, out := range f.executions {
		if out.State == "running" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("expected exactly one running replacement", n)
	}
}

func TestManagedRuntimeDrainCancellationInterruptsBackendRetry(t *testing.T) {
	_, m, f, d := recoveryFixture(t)
	entered := make(chan struct{}, 1)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/inspect") {
			select {
			case entered <- struct{}{}:
			default:
			}
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(503)
			return
		}
		original.ServeHTTP(w, r)
	})
	op := runtimeOperation{ID: "drain-retry-cancellation", InstallID: d.InstallID, Kind: "stop", Phase: "draining", Status: "running", CreatedAt: runtimeTimestamp()}
	_, err := m.s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,lease_until,idempotency_key,fingerprint,record_json) VALUES(?,?,'stop','running',?,?,?,'fixture',?)`, op.ID, op.InstallID, m.owner, time.Now().Unix()+45, op.ID, runtimeJSON(runtimeStoredOperation{runtimeOperation: op}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.drain(d, op) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inspection not reached")
	}
	start := time.Now()
	if err := m.cancelOperation(op.ID, d.InstallID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled drain succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend retry ignored cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation exceeded budget")
	}
}
