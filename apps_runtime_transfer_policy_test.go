package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stateTestPolicy(t *testing.T) runtimeStateTransferPolicy {
	t.Helper()
	p, err := serverRuntimeStatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func stateTestArchive(t *testing.T, name, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func stateTestBackendPolicy(t *testing.T, m *appRuntimeManager, p runtimePlacement, policy *runtimeStateTransferPolicy) {
	t.Helper()
	reg, err := m.registration(p.Backend)
	if err != nil {
		t.Fatal(err)
	}
	reg.Caps.StateTransfer = policy
	if _, err = m.s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeStatePolicyNegotiationAndJournal(t *testing.T) {
	_, m, _, id, placement := runtimeTestSetup(t)
	d := prepareRuntimeFixture(t, m, id, placement, "policy-fixture")
	local := d
	local.Placement = runtimePlacement{Type: "local"}
	p, err := m.negotiateStatePolicy(local, placement, nil)
	if err != nil || p != legacyRuntimeStatePolicy() {
		t.Fatalf("legacy negotiation: %+v %v", p, err)
	}
	advertised := stateTestPolicy(t)
	advertised.MaxExpandedBytes = 3 << 30
	advertised.MaxEntries = 200000
	stateTestBackendPolicy(t, m, placement, &advertised)
	op, err := m.operation(func() string {
		var id string
		m.s.store.db.QueryRow(`SELECT id FROM app_runtime_operations LIMIT 1`).Scan(&id)
		return id
	}())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.pinStatePolicy(local, d, &op); err != nil {
		t.Fatal(err)
	}
	saved, err := m.operation(op.ID)
	if err != nil || saved.StateTransfer == nil || *saved.StateTransfer != advertised {
		t.Fatalf("policy not durable: %+v %v", saved.StateTransfer, err)
	}
	larger := stateTestPolicy(t)
	stateTestBackendPolicy(t, m, placement, &larger)
	if p, err = m.negotiateStatePolicy(local, placement, saved.StateTransfer); err != nil || p != advertised {
		t.Fatalf("retry changed policy: %+v %v", p, err)
	}
	smaller := advertised
	smaller.MaxEntries--
	stateTestBackendPolicy(t, m, placement, &smaller)
	if _, err = m.negotiateStatePolicy(local, placement, saved.StateTransfer); err == nil {
		t.Fatal("retry silently lowered pinned limits")
	}
	t.Setenv("APTEVA_RUNTIME_STATE_MAX_ARCHIVE_BYTES", "-1")
	if _, err = serverRuntimeStatePolicy(); err == nil {
		t.Fatal("invalid operator limit accepted")
	}
}
func TestRuntimeStateLargePreflightIsReadOnly(t *testing.T) {
	s, m, f, id, placement := runtimeTestSetup(t)
	manifest, root, _ := legacyRuntimeStateFixture(t, s, m, id, false, f)
	file, err := os.Create(filepath.Join(root, "large.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(runtimeSnapshotLimit + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	_, checks := m.preflight(context.Background(), id, "migration-project", manifest, placement)
	requireRuntimeCheck(t, checks, "state_size_unverified", "blocked")
	policy := stateTestPolicy(t)
	stateTestBackendPolicy(t, m, placement, &policy)
	_, checks = m.preflight(context.Background(), id, "migration-project", manifest, placement)
	if err = firstRuntimeBlock(checks); err != nil {
		t.Fatal(err)
	}
	assertNoRuntimeMutation(t, s, id)
	if len(f.events) != 0 {
		t.Fatal("preflight performed lifecycle actions")
	}
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/inspect-target") {
			free := int64(1)
			json.NewEncoder(w).Encode(map[string]any{"host_key": "test-host", "platform": "linux-amd64", "formats": []string{"source"}, "state_available_bytes": free})
			return
		}
		original.ServeHTTP(w, r)
	})
	if _, err = m.begin(id, "prepare", "disk-blocked", placement, &manifest); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("disk preflight did not block: %v", err)
	}
	assertNoRuntimeMutation(t, s, id)
}
func TestRuntimeStateLargeArchiveRoundTrip(t *testing.T) {
	policy := stateTestPolicy(t)
	root := t.TempDir()
	path := filepath.Join(root, "large.db")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	size := runtimeSnapshotLimit + 4096
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("latest-state"), size-12); err != nil {
		t.Fatal(err)
	}
	f.Close()
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	out, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeRuntimeSnapshotContext(context.Background(), root, out, policy); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if _, err = inspectRuntimeSnapshot(context.Background(), archive, legacyRuntimeStatePolicy()); err == nil {
		t.Fatal("legacy expansion limit not enforced")
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err = readRuntimeSnapshotContext(context.Background(), archive, dest, policy); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Open(filepath.Join(dest, "large.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	info, _ := restored.Stat()
	got := make([]byte, 12)
	restored.ReadAt(got, size-12)
	if info.Size() != size || string(got) != "latest-state" {
		t.Fatal("large state did not survive restoration")
	}
}
func TestRuntimeStateArchiveBoundsIntegrityAndCancellation(t *testing.T) {
	data := stateTestArchive(t, "app.db", "database")
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"archive", "expanded", "entries", "checksum", "canceled"} {
		t.Run(which, func(t *testing.T) {
			policy := stateTestPolicy(t)
			ctx := context.Background()
			raw := append([]byte(nil), data...)
			switch which {
			case "archive":
				policy.MaxArchiveBytes = int64(len(raw) - 1)
			case "expanded":
				policy.MaxExpandedBytes = 1
			case "entries":
				var b bytes.Buffer
				gz := gzip.NewWriter(&b)
				tw := tar.NewWriter(gz)
				for _, name := range []string{"first", "second"} {
					tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1})
					tw.Write([]byte("a"))
				}
				tw.Close()
				gz.Close()
				raw = b.Bytes()
				policy.MaxEntries = 1
			case "checksum":
				raw[len(raw)-8] ^= 0xff
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			os.WriteFile(archive, raw, 0600)
			dest := filepath.Join(t.TempDir(), "existing")
			os.MkdirAll(dest, 0700)
			os.WriteFile(filepath.Join(dest, "keep"), []byte("original"), 0600)
			if err := readRuntimeSnapshotContext(ctx, archive, dest, policy); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if content, _ := os.ReadFile(filepath.Join(dest, "keep")); string(content) != "original" {
				t.Fatal("failed restoration damaged destination")
			}
			stages, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".runtime-restore-*"))
			if len(stages) != 0 {
				t.Fatal("partial restoration left behind")
			}
		})
	}
}
func TestRuntimeStateRestoreRefusesRunningTarget(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	d := prepareRuntimeFixture(t, m, id, p, "restore-guard")
	op := runtimeOperation{ID: "guard", SnapshotSHA256: "invalid"}
	raw := stateTestArchive(t, "app.db", "state")
	op.SnapshotPath = filepath.Join(t.TempDir(), "state.tar.gz")
	os.WriteFile(op.SnapshotPath, raw, 0600)
	op.SnapshotSHA256 = runtimeHash(raw)
	f.mu.Lock()
	execution := f.executions[d.ID]
	execution.State = "running"
	f.executions[d.ID] = execution
	f.mu.Unlock()
	if err := m.restoreSnapshot(&d, op); err == nil || !strings.Contains(err.Error(), "quiescent") {
		t.Fatalf("running target accepted: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.snapshot) != 0 {
		t.Fatal("restored under a live writer")
	}
}
func TestRuntimeStateRollbackPreservesNewCandidateWrites(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	_, root, _ := legacyRuntimeStateFixture(t, s, m, id, true, f)
	d := prepareRuntimeFixture(t, m, id, p, "prepare-new-writes")
	newer := stateTestArchive(t, "app.db", "newer candidate writes")
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/exec/"+d.ID+"/") && strings.HasSuffix(r.URL.Path, "/activate") {
			f.mu.Lock()
			f.snapshot = newer
			f.mu.Unlock()
			http.Error(w, "activation failed after write", 500)
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(id, "activate", "activate-new-writes", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "failed" || !strings.Contains(result.Error, "previous deployment restored and verified") {
		t.Fatalf("rollback failed: %s", result.Error)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "app.db")); string(got) != "newer candidate writes" {
		t.Fatalf("rollback lost newer data: %q", got)
	}
	saved, err := m.operation(op.ID)
	if err != nil || saved.StateTransfer == nil || saved.SnapshotSourceID != d.ID || saved.SnapshotSHA256 != runtimeHash(newer) {
		t.Fatalf("rollback provenance not durable: %+v %v", saved, err)
	}
}
func TestRuntimeStateContextReaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&runtimeContextReader{ctx, strings.NewReader("data")}).Read(make([]byte, 4))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRuntimeStateRollbackRecoveryKeepsPolicyAndNewerSnapshot(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "recovery-old")
	old := activateRuntimeFixture(t, m, id, "activate-recovery-old")
	old.PersistentData = true
	if err := m.saveDeployment(old); err != nil {
		t.Fatal(err)
	}
	newer := stateTestArchive(t, "app.db", "newest state survives rollback retry")
	f.mu.Lock()
	f.snapshot = stateTestArchive(t, "app.db", "original")
	f.mu.Unlock()
	p.Target.ID = "second-target"
	candidate := prepareRuntimeFixture(t, m, id, p, "recovery-candidate")
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/exec/"+candidate.ID+"/") && strings.HasSuffix(r.URL.Path, "/activate") {
			f.mu.Lock()
			f.snapshot = newer
			f.failOldRestart = true
			f.failedCandidateID = candidate.ID
			f.mu.Unlock()
			http.Error(w, "activation failed", 500)
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(id, "activate", "recovery-cutover", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitRuntimeOperation(t, m, op)
	if failed.Status != "failed" || failed.RollbackID == "" || failed.SnapshotSourceID != candidate.ID {
		t.Fatalf("recovery journal missing: %+v", failed)
	}
	before, err := m.operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.SnapshotPath == "" || before.StateTransfer == nil || before.SnapshotSHA256 != runtimeHash(newer) {
		t.Fatalf("new state/policy lost: %+v", before)
	}
	// A new manager reads only the persisted journal, as after a restart.
	recovered := &appRuntimeManager{s: m.s, ctx: m.ctx, http: m.http, owner: m.owner}
	reloaded, err := recovered.operation(op.ID)
	if err != nil || reloaded.SnapshotPath != before.SnapshotPath || *reloaded.StateTransfer != *before.StateTransfer {
		t.Fatal("restart changed transfer journal")
	}
	f.mu.Lock()
	f.failOldRestart = false
	f.mu.Unlock()
	if err = m.retryOperation(op.ID, id); err != nil {
		t.Fatal(err)
	}
	after := waitRuntimeOperation(t, m, op)
	if !strings.Contains(after.Error, "previous deployment restored and verified") {
		t.Fatalf("retry failed: %s", after.Error)
	}
	if after.SnapshotPath != before.SnapshotPath || after.SnapshotSHA256 != before.SnapshotSHA256 || *after.StateTransfer != *before.StateTransfer {
		t.Fatal("retry replaced durable snapshot or policy")
	}
	active, err := m.active(id)
	if err != nil || active.ID != after.RollbackID || active.Status != "active" {
		t.Fatalf("rollback execution not active: %+v %v", active, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.snapshot, newer) {
		t.Fatal("retry restored stale state")
	}
	for dep, execution := range f.executions {
		if dep != active.ID && execution.State == "running" {
			t.Fatal("multiple writers after recovery")
		}
	}
}

func TestRuntimeStateStreamingCancellationCleansPartialSnapshot(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "cancel-source")
	old := activateRuntimeFixture(t, m, id, "activate-cancel-source")
	old.PersistentData = true
	if err := m.saveDeployment(old); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.snapshot = stateTestArchive(t, "app.db", "source retained")
	f.mu.Unlock()
	p.Target.ID = "cancel-destination"
	prepareRuntimeFixture(t, m, id, p, "cancel-candidate")
	original := f.server.Config.Handler
	started := make(chan runtimeStateTransferPolicy, 1)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/snapshot") {
			var policy runtimeStateTransferPolicy
			if err := json.Unmarshal([]byte(r.Header.Get("X-Apteva-State-Transfer-Policy")), &policy); err != nil {
				t.Error(err)
			}
			w.WriteHeader(200)
			w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			started <- policy
			<-r.Context().Done()
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(id, "activate", "cancel-stream", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case policy := <-started:
		saved, e := m.operation(op.ID)
		if e != nil || saved.StateTransfer == nil || *saved.StateTransfer != policy {
			t.Fatal("stream policy differs from journal")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("snapshot stream did not start")
	}
	if err = m.cancelOperation(op.ID, id); err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "cancelled" {
		t.Fatalf("stream cancellation failed: %+v", result)
	}
	partial, _ := filepath.Glob(filepath.Join(m.s.localApps.cacheDir, ".runtime-snapshots", "*.tar.gz"))
	if len(partial) != 0 {
		t.Fatal("partial snapshot left on disk")
	}
	active, err := m.active(id)
	if err != nil || active.Status != "active" {
		t.Fatal("cancellation did not restore previous owner")
	}
}
func TestRuntimeStateDoesNotRaisePackageLimit(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "static-package"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(runtimePackageLimit + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = runtimeDirectoryDigest(root); err == nil {
		t.Fatal("state limit leaked into package validation")
	}
}
