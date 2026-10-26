package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedRuntimeRestartPreservesLifecycleAfterShutdown(t *testing.T) {
	_, m, f, old := recoveryFixture(t)
	f.mu.Lock()
	out := f.executions[old.ID]
	out.State = "stopped"
	out.StoppedConfirmed = true
	out.Ready = false
	f.executions[old.ID] = out
	f.mu.Unlock()
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/exec/"+old.ID+"/") {
			http.Error(w, "old process is gone", 503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prepare") {
			rec := httptest.NewRecorder()
			original.ServeHTTP(rec, r)
			var execution runtimeExecution
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &execution) != nil {
				t.Error("fixture prepare failed")
				http.Error(w, "failed", 500)
				return
			}
			// The local adapter historically echoes the candidate's capabilities;
			// a missing backend field must not erase persisted release knowledge.
			execution.AppCapabilities = runtimeAppCaps{}
			writeJSON(w, execution)
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(old.InstallID, "restart", "after-process-shutdown", old.Placement, nil)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	d, err := m.active(old.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	admitted := f.admitting[d.ID]
	f.mu.Unlock()
	if !admitted {
		t.Fatal("successful recovery left the replacement in standby")
	}
	if d.ID == old.ID || !d.AppCapsKnown || !d.AppCaps.PassiveStart {
		t.Fatal("release lifecycle lost during restart")
	}
}

func TestManagedRuntimeUnknownCapabilitiesAreVerifiedAtStartup(t *testing.T) {
	for _, mode := range []string{"discover", "unavailable", "malformed", "null", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			_, m, f, id, p := runtimeTestSetup(t)
			f.mu.Lock()
			f.appCaps = runtimeAppCaps{}
			f.mu.Unlock()
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/app/describe") {
					switch mode {
					case "discover":
						writeJSON(w, runtimeAppCaps{PassiveStart: true, Drain: true, Routing: "key"})
					case "unavailable":
						http.Error(w, "temporary failure", 503)
					case "malformed":
						writeJSON(w, map[string]any{"ok": true})
					case "null":
						_, _ = w.Write([]byte("null"))
					case "unsupported":
						http.NotFound(w, r)
					}
					return
				}
				if mode == "unsupported" && strings.HasSuffix(r.URL.Path, "/app/status") {
					http.NotFound(w, r)
					return
				}
				original.ServeHTTP(w, r)
			})
			op, err := m.begin(id, "deploy", "unknown-startup", p, nil)
			if err != nil {
				t.Fatal(err)
			}
			op = waitRuntimeOperation(t, m, op)
			if mode == "unavailable" || mode == "malformed" || mode == "null" {
				if op.Status != "failed" {
					t.Fatal("unknown capability fetch was treated as unsupported")
				}
				if d, _ := m.active(id); d.ID != "" {
					t.Fatal("unverified app committed")
				}
				return
			}
			if op.Status != "succeeded" {
				t.Fatal(op.Error)
			}
			d, _ := m.active(id)
			if !d.AppCapsKnown {
				t.Fatal("verified capability result not persisted")
			}
			if mode == "discover" {
				f.mu.Lock()
				admitted := f.admitting[d.ID]
				f.mu.Unlock()
				if !admitted || !d.AppCaps.PassiveStart {
					t.Fatal("fresh managed app was not activated")
				}
			}
		})
	}
}

func TestManagedRuntimeActivationAcknowledgementRequiresAdmission(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	manifest, err := installManifest(m.s, id)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Runtime.StartupTimeoutSeconds = 1
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/activate") {
			w.WriteHeader(200)
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(id, "deploy", "activation-ack-only", p, manifest)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "failed" || !strings.Contains(op.Error, "admission") {
		t.Fatal("HTTP 200 was incorrectly treated as admission", op.Error)
	}
	if active, _ := m.active(id); active.ID != "" {
		t.Fatal("standby candidate became the active route")
	}
}

func TestManagedRuntimeAutomaticStandbyReadmission(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	// Simulate a journal produced by the old bug. Its live process is healthy
	// but passive; its all-zero capability record is unknown, not unsupported.
	d.AppCaps = runtimeAppCaps{}
	d.AppCapsKnown = false
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.admitting[d.ID] = false
	f.events = nil
	out := f.executions[d.ID]
	out.Ready = false
	f.executions[d.ID] = out
	f.mu.Unlock()
	collectRuntimeMonitor(t, m, d.InstallID)
	var n int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE kind='readmit'`).Scan(&n)
	if n != 0 {
		t.Fatal("one stale probe authorized readmission")
	}
	nextRuntimeHealth(t, m, d.ID)
	var opID string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE kind='readmit'`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, _ := m.operation(opID)
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	current, err := m.active(d.InstallID)
	if err != nil || current.ID != d.ID || current.Generation != d.Generation || !current.AppCapsKnown || !current.AppCaps.PassiveStart {
		t.Fatal("readmission changed ownership or failed capability repair", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.admitting[d.ID] {
		t.Fatal("owned runtime remains in standby")
	}
	for _, event := range f.events {
		if strings.HasPrefix(event, "start:") || strings.HasPrefix(event, "stop:") || strings.HasPrefix(event, "prepare:") {
			t.Fatal("standby recovery created/replaced a process", event)
		}
	}
}

func TestManagedRuntimeStandbyReadmissionRequiresOwnership(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-generation", "auth-failure", "draining", "digest-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f, d := recoveryFixture(t)
			if mode == "digest-mismatch" {
				f.mu.Lock()
				out := f.executions[d.ID]
				out.ReleaseDigest = strings.Repeat("b", 64)
				f.executions[d.ID] = out
				f.mu.Unlock()
			}
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/app/status") {
					if mode == "auth-failure" {
						http.Error(w, "unauthorized", 403)
						return
					}
					identity, generation, state := d.ID, d.Generation, "standby"
					if mode == "wrong-id" {
						identity = "different-owner"
					}
					if mode == "wrong-generation" {
						generation++
					}
					if mode == "draining" {
						state = "draining"
					}
					writeJSON(w, map[string]any{"deployment_id": identity, "generation": generation, "state": state, "admitting": false})
					return
				}
				original.ServeHTTP(w, r)
			})
			collectRuntimeMonitor(t, m, d.InstallID)
			nextRuntimeHealth(t, m, d.ID)
			nextRuntimeHealth(t, m, d.ID)
			var n int
			s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE kind='readmit'`).Scan(&n)
			if n != 0 {
				t.Fatal("unverified owner authorized activation")
			}
		})
	}
}

func TestManagedRuntimeReadmissionResumesAfterControllerRestart(t *testing.T) {
	s, m, f, d := recoveryFixture(t)
	f.mu.Lock()
	f.admitting[d.ID] = false
	f.events = nil
	f.mu.Unlock()
	m.Close()
	op, err := m.beginExpected(d.InstallID, "readmit", "durable-readmission", d.Placement, nil, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	restarted := &appRuntimeManager{s: s, ctx: ctx, cancel: cancel, owner: "restarted-admission-controller", http: m.http, workers: map[string]bool{}}
	s.appRuntime = restarted
	t.Cleanup(restarted.Close)
	restarted.reconcile()
	op = waitRuntimeOperation(t, restarted, op)
	if op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	current, _ := restarted.active(d.InstallID)
	if current.ID != d.ID {
		t.Fatal("controller recovery created a new deployment")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.admitting[d.ID] {
		t.Fatal("persisted readmission did not activate")
	}
	for _, event := range f.events {
		if strings.HasPrefix(event, "start:") {
			t.Fatal(fmt.Sprint("duplicate process after controller restart: ", event))
		}
	}
}
