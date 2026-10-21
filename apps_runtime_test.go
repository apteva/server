package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type runtimeBackendFixture struct {
	asyncPrepare      bool
	holdPrepare       bool
	mu                sync.Mutex
	server            *httptest.Server
	caps              runtimeBackendCaps
	executions        map[string]runtimeExecution
	env               map[string]map[string]string
	events            []string
	failStart         bool
	failedCandidateID string
	loseStartResponse bool
	failStop          bool
	failOldRestart    bool
	oldID             string
	appCaps           runtimeAppCaps
	snapshot          []byte
}

func newRuntimeBackendFixture(t *testing.T, id string) *runtimeBackendFixture {
	t.Helper()
	f := &runtimeBackendFixture{caps: runtimeBackendCaps{Contract: runtimeBackendContract, ID: id, RuntimeKinds: []string{"service", "source"}, Formats: []string{"artifact", "source", "image"}, Platforms: []string{"linux-amd64"}, Protocols: []string{"tcp", "udp"}, ConfirmedStop: true, Snapshots: true, Replicas: true}, executions: map[string]runtimeExecution{}, env: map[string]map[string]string{}, appCaps: runtimeAppCaps{PassiveStart: true, Drain: true, ReplicaSafe: true, WorkerCoordination: true, Routing: "stateless"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/exec/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/exec/"), "/")
			dep := parts[0]
			if strings.HasSuffix(r.URL.Path, "/health") {
				if f.executions[dep].State != "running" {
					http.Error(w, "not running", 503)
					return
				}
				w.WriteHeader(200)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/describe") {
				json.NewEncoder(w).Encode(f.appCaps)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/drain") {
				json.NewEncoder(w).Encode(map[string]bool{"drained": true})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/activate") {
				w.WriteHeader(200)
				return
			}
			http.Error(w, "not found", 404)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/describe") {
			json.NewEncoder(w).Encode(f.caps)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/restore") {
			raw, _ := io.ReadAll(r.Body)
			if runtimeHash(raw) != r.Header.Get("X-Apteva-Snapshot-SHA256") {
				http.Error(w, "bad snapshot", 400)
				return
			}
			f.snapshot = raw
			w.Header().Set("X-Apteva-Snapshot-SHA256", runtimeHash(raw))
			w.WriteHeader(200)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/snapshot") {
			w.Write(f.snapshot)
			return
		}
		var req runtimeBackendRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.events = append(f.events, action+":"+req.DeploymentID)
		out := f.executions[req.DeploymentID]
		switch action {
		case "validate-target":
			out.HostKey = runtimeHostKey(req.Target)
		case "prepare":
			out = runtimeExecution{Handle: req.DeploymentID, HostKey: runtimeHostKey(req.Target), State: "prepared", StateRef: "state:" + req.DeploymentID, ReleaseDigest: strings.Repeat("a", 64), Platform: "linux-amd64"}
			if req.StateRef != "" {
				out.StateRef = req.StateRef
			}
			out.StateBytes = new(int64)
			out.AppCapabilities = f.appCaps
			if f.asyncPrepare {
				out.State = "preparing"
				out.ReleaseDigest = ""
			}
		case "start":
			f.env[req.DeploymentID] = req.Environment
			if f.failedCandidateID == "" && req.DeploymentID != f.oldID && (f.failStart || f.loseStartResponse) {
				f.failedCandidateID = req.DeploymentID
			}
			if f.failStart && req.DeploymentID == f.failedCandidateID {
				http.Error(w, "failed", 500)
				return
			}
			if f.failOldRestart && req.DeploymentID != f.failedCandidateID {
				http.Error(w, "old restart failed", 500)
				return
			}
			out.State = "running"
			out.Ready = true
			out.StoppedConfirmed = false
			out.Endpoint = f.server.URL + "/exec/" + req.DeploymentID
			out.BrowserEndpoint = out.Endpoint
			f.executions[req.DeploymentID] = out
			if f.loseStartResponse && req.DeploymentID == f.failedCandidateID {
				http.Error(w, "lost response", 503)
				return
			}
		case "inspect":
			if out.State == "preparing" && !f.holdPrepare {
				out.State = "prepared"
				out.ReleaseDigest = strings.Repeat("a", 64)
			}
		case "stop":
			if f.failStop {
				out.StoppedConfirmed = false
				json.NewEncoder(w).Encode(out)
				return
			}
			out.State = "stopped"
			out.Ready = false
			out.StoppedConfirmed = true
		case "fence":
			out.State = "stopped"
			out.Ready = false
			out.StoppedConfirmed = true
		case "delete":
			out.State = "deleted"
			out.StoppedConfirmed = true
		case "logs":
			json.NewEncoder(w).Encode(map[string]any{"lines": []string{"safe log"}, "cursor": "1"})
			return
		default:
			http.Error(w, "unknown", 404)
			return
		}
		f.executions[req.DeploymentID] = out
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.server.Close)
	return f
}
func runtimeFixtureInstall(t *testing.T, s *Server, name string) int64 {
	t.Helper()
	id := seedRunningInstall(t, s, name, "", sdk.Manifest{Name: name, Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service"}}, nil)
	if err := s.store.SetPlatformRole(1, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	return id
}
func runtimeTestSetup(t *testing.T) (*Server, *appRuntimeManager, *runtimeBackendFixture, int64, runtimePlacement) {
	t.Helper()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := s.appRuntimeManager()
	backend := newRuntimeBackendFixture(t, "future-compute")
	provider := runtimeFixtureInstall(t, s, "arbitrary-execution-provider")
	s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, backend.server.URL, provider)
	if err := m.registerBackend(runtimeBackendRegistration{InstallID: provider, ID: backend.caps.ID, Endpoint: runtimeControlPrefix + "backends/" + backend.caps.ID}); err != nil {
		t.Fatal(err)
	}
	source := runtimeFixtureInstall(t, s, "remote-workload")
	s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, source)
	p := runtimePlacement{Type: "backend", Backend: runtimeBackendRef{provider, backend.caps.ID}, Target: runtimeResourceRef{provider, "compute.machine", "target-a"}, GatewayURL: "https://gateway.example.test"}
	return s, m, backend, source, p
}
func waitRuntimeOperation(t *testing.T, m *appRuntimeManager, op runtimeOperation) runtimeOperation {
	t.Helper()
	end := time.Now().Add(12 * time.Second)
	for {
		got, err := m.operation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "failed" || got.Status == "succeeded" || got.Status == "cancelled" {
			return got
		}
		if time.Now().After(end) {
			var dbStatus, owner string
			var flag int
			m.s.store.db.QueryRow(`SELECT status,owner,cancel_requested FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&dbStatus, &owner, &flag)
			t.Fatalf("operation timeout phase=%s rowstatus=%s flag=%d sameowner=%v", got.Phase, dbStatus, flag, owner == m.owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func prepareRuntimeFixture(t *testing.T, m *appRuntimeManager, id int64, p runtimePlacement, key string) runtimeDeployment {
	t.Helper()
	op, err := m.begin(id, "prepare", key, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "succeeded" {
		t.Fatalf("prepare: %s", result.Error)
	}
	d, err := m.loadDeployment(result.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func activateRuntimeFixture(t *testing.T, m *appRuntimeManager, id int64, key string) runtimeDeployment {
	t.Helper()
	op, err := m.begin(id, "activate", key, runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "succeeded" {
		t.Fatalf("activate: %s", result.Error)
	}
	d, err := m.active(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestManagedRuntimeGenericBackendAndIdempotency(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	op, err := m.begin(id, "prepare", "prepare-once", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := m.begin(id, "prepare", "prepare-once", p, nil)
	if err != nil || dup.ID != op.ID {
		t.Fatalf("idempotent prepare: %v", err)
	}
	changed := p
	changed.Target.ID = "target-b"
	if _, err = m.begin(id, "prepare", "prepare-once", changed, nil); err == nil {
		t.Fatal("different idempotent request accepted")
	}
	if result := waitRuntimeOperation(t, m, op); result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	d := activateRuntimeFixture(t, m, id, "activate-once")
	if d.Placement.Backend.ID != "future-compute" || s.installedApps.Get(id).SidecarURL != d.Execution.Endpoint {
		t.Fatal("generic backend routing not committed")
	}
	token, _ := m.token(d)
	got, _, _, err := s.appInstallForToken(token)
	if err != nil || got != id {
		t.Fatal("generation credential did not preserve installation identity")
	}
	raw := runtimeJSON(d)
	if strings.Contains(raw, token) || strings.Contains(raw, "environment_encrypted") {
		t.Fatal("deployment API leaks credentials")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.env[d.ID]["APTEVA_GATEWAY_URL"] != p.GatewayURL || f.env[d.ID]["APTEVA_INSTALL_ID"] != fmt.Sprint(id) {
		t.Fatal("remote runtime environment incorrect")
	}
}
func TestManagedRuntimeExclusiveRollbackAndLostResponse(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			prepareRuntimeFixture(t, m, id, p, "prepare-old")
			old := activateRuntimeFixture(t, m, id, "activate-old")
			old.Manifest.Runtime.ActivationStrategy = "exclusive"
			old.Manifest.Runtime.StartupTimeoutSeconds = 2
			if err := m.saveDeployment(old); err != nil {
				t.Fatal(err)
			}
			s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(old.Manifest), id)
			next := prepareRuntimeFixture(t, m, id, p, "prepare-new")
			f.mu.Lock()
			f.oldID = old.ID
			f.failStart = !lost
			f.loseStartResponse = lost
			f.mu.Unlock()
			op, err := m.begin(id, "activate", "activate-bad", runtimePlacement{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := waitRuntimeOperation(t, m, op)
			if result.Status != "failed" || !strings.Contains(result.Error, "restored and verified") {
				t.Fatalf("rollback: %s", result.Error)
			}
			current, _ := m.active(id)
			if current.ID == old.ID || current.Manifest.Version != old.Manifest.Version || current.Generation <= next.Generation || s.installedApps.Get(id).SidecarURL != current.Execution.Endpoint {
				t.Fatal("previous routing not restored")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			events := f.events
			stopOld, startNew, stopNew, restartOld := -1, -1, -1, -1
			for i, e := range events {
				if e == "stop:"+old.ID {
					stopOld = i
				}
				if e == "start:"+next.ID {
					startNew = i
				}
				if e == "stop:"+next.ID {
					stopNew = i
				}
				if e == "start:"+current.ID && i > stopOld {
					restartOld = i
				}
			}
			if !(stopOld >= 0 && startNew > stopOld && stopNew > startNew && restartOld > stopNew) {
				t.Fatal("exclusive rollback sequencing incorrect")
			}
		})
	}
}
func TestManagedRuntimeUnconfirmedStopBlocksReplacement(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare-old")
	old := activateRuntimeFixture(t, m, id, "activate-old")
	old.Manifest.Runtime.ActivationStrategy = "exclusive"
	m.saveDeployment(old)
	next := prepareRuntimeFixture(t, m, id, p, "prepare-new")
	f.mu.Lock()
	f.failStop = true
	f.mu.Unlock()
	op, err := m.begin(id, "activate", "blocked-stop", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := waitRuntimeOperation(t, m, op)
	if got.Status != "failed" {
		t.Fatal("unconfirmed stop activated replacement")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e == "start:"+next.ID {
			t.Fatal("replacement started before confirmed stop")
		}
	}
}
func TestManagedRuntimeUnavailableBackendNeverFallsBackLocal(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	old := activateRuntimeFixture(t, m, id, "activate")
	f.server.Close()
	m.reconcile()
	collectRuntimeMonitor(t, m, old.InstallID)
	if s.localApps.PID(id) != 0 {
		t.Fatal("unavailable remote host spawned locally")
	}
	current, _ := m.active(id)
	if current.ID != old.ID {
		t.Fatal("remote ownership lost")
	}
	msg := m.deploymentSummary(id).Message
	if !strings.Contains(msg, "ownership retained") {
		t.Fatal(msg)
	}
}
func TestManagedRuntimePortOwnershipAndUnsafeSnapshots(t *testing.T) {
	s, m, _, id, _ := runtimeTestSetup(t)
	d := runtimeDeployment{ID: "ports-a", InstallID: id, Generation: 1, Status: "prepared", Execution: runtimeExecution{HostKey: "physical:host", Ports: []runtimePortBinding{{Protocol: "udp", Start: 10000, End: 10100}}}}
	if err := m.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	if err := m.reservePorts(d); err != nil {
		t.Fatal(err)
	}
	second := runtimeFixtureInstall(t, s, "other-workload")
	other := d
	other.ID = "ports-b"
	other.InstallID = second
	other.Execution.Ports = []runtimePortBinding{{Protocol: "udp", Start: 10050, End: 10150}}
	m.saveDeployment(other)
	if err := m.reservePorts(other); err == nil {
		t.Fatal("overlapping range accepted")
	}
	other.Execution.HostKey = "physical:other-host"
	if err := m.reservePorts(other); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../outside", Size: 1, Mode: 0600})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "unsafe.tgz")
	os.WriteFile(path, out.Bytes(), 0600)
	if err := readRuntimeSnapshot(path, filepath.Join(t.TempDir(), "dest")); err == nil {
		t.Fatal("snapshot traversal accepted")
	}
}
func TestManagedRuntimeSnapshotJournalAndReplicaGates(t *testing.T) {
	_, m, _, id, p := runtimeTestSetup(t)
	op, err := m.begin(id, "prepare", "prepare", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	op.Owner = m.owner
	op.SnapshotPath = "/private/snapshot"
	op.SnapshotSHA256 = strings.Repeat("b", 64)
	if err := m.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.operation(op.ID)
	if err != nil || loaded.SnapshotPath != op.SnapshotPath {
		t.Fatal("private snapshot journal did not survive reload")
	}
	if strings.Contains(runtimeJSON(loaded), op.SnapshotPath) {
		t.Fatal("snapshot path leaked")
	}
	d := activateRuntimeFixture(t, m, id, "activate")
	d.AppCaps = runtimeAppCaps{}
	if err := m.replicaEligible(d); err == nil {
		t.Fatal("undeclared app replication accepted")
	}
}
func TestManagedRuntimePrivateControlAndBrowserCredentialScope(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	c := runtimeBrowserClaims{InstallID: id, UserID: 1, DeploymentID: d.ID, Generation: d.Generation, Operations: []string{"stream"}, Origin: s.publicBaseURL(), ExpiresAt: time.Now().Unix() + 60, Nonce: "nonce"}
	token := runtimeSignedBrowserToken(s, c)
	if _, err := s.validateRuntimeBrowser(token, id, c.Origin, "stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.validateRuntimeBrowser(token, id+1, c.Origin, "stream"); err == nil {
		t.Fatal("cross-install browser token accepted")
	}
	if _, err := s.validateRuntimeBrowser(token, id, "https://other.example", "stream"); err == nil {
		t.Fatal("wrong origin accepted")
	}
	s.store.db.Exec(`INSERT INTO app_runtime_browser_revocations(token_hash,expires_at) VALUES(?,?)`, HashAPIKey(token), c.ExpiresAt)
	if _, err := s.validateRuntimeBrowser(token, id, c.Origin, "stream"); err == nil {
		t.Fatal("revoked token accepted")
	}
	req := httptest.NewRequest(http.MethodPost, "/apps/remote-workload/_install/"+fmt.Sprint(id)+runtimeControlPrefix+"app/activate", nil)
	rec := httptest.NewRecorder()
	s.handleAppProxy(rec, req)
	if rec.Code == 200 {
		t.Fatal("private control exposed through app proxy")
	}
}

func TestManagedRuntimeStateMigrationAndInterruptedStartRecovery(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare-old")
	old := activateRuntimeFixture(t, m, id, "activate-old")
	old.Manifest.DB = &sdk.DBConfig{Driver: "sqlite"}
	old.AppCaps.PassiveStart = true
	m.saveDeployment(old)
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(old.Manifest), id)
	state := filepath.Join(t.TempDir(), "state")
	os.MkdirAll(state, 0700)
	os.WriteFile(filepath.Join(state, "app.db"), []byte("durable database bytes"), 0600)
	var snap bytes.Buffer
	if err := writeRuntimeSnapshot(state, &snap); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.snapshot = snap.Bytes()
	f.mu.Unlock()
	p.Target.ID = "target-b"
	d := prepareRuntimeFixture(t, m, id, p, "prepare-move")
	op, err := m.begin(id, "activate", "move-state", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	if result.SnapshotPath == "" || !runtimeDigest(result.SnapshotSHA256) {
		t.Fatal("snapshot journal missing")
	}
	var contents bytes.Buffer
	file, _ := os.Open(result.SnapshotPath)
	gz, _ := gzip.NewReader(file)
	tr := tar.NewReader(gz)
	hdr, e := tr.Next()
	if e != nil || hdr.Name != "app.db" {
		t.Fatal("snapshot contents missing")
	}
	io.Copy(&contents, tr)
	gz.Close()
	file.Close()
	if contents.String() != "durable database bytes" {
		t.Fatal("state migration lost bytes")
	}
	active, _ := m.active(id)
	if active.ID != d.ID {
		t.Fatal("state migration did not commit")
	}
	// Model a server crash after Start reached the provider but before the
	// candidate endpoint was committed. Resume must inspect, not start again.
	d2 := prepareRuntimeFixture(t, m, id, p, "prepare-recovery")
	d2.Status = "starting"
	m.saveDeployment(d2)
	f.mu.Lock()
	out := f.executions[d2.ID]
	out.State = "running"
	out.Ready = true
	out.Endpoint = f.server.URL + "/exec/" + d2.ID
	f.executions[d2.ID] = out
	f.mu.Unlock()
	recovered := runtimeOperation{ID: "recovered-operation", InstallID: id, Kind: "activate", Phase: "starting_candidate", Status: "running", DeploymentID: d2.ID, PreviousID: active.ID, IdempotencyKey: "recovery", CreatedAt: runtimeTimestamp()}
	s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,lease_until,idempotency_key,fingerprint,record_json) VALUES(?,?,'activate','running','dead-server',0,?,'',?)`, recovered.ID, id, recovered.IdempotencyKey, runtimeJSON(recovered))
	m.reconcile()
	got := waitRuntimeOperation(t, m, recovered)
	if got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if event == "start:"+d2.ID {
			t.Fatal("recovered running candidate was started twice")
		}
	}
}

func TestManagedRuntimeReplicaScaleAndRouting(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	primary := activateRuntimeFixture(t, m, id, "activate")
	if err := m.scale(id, 3); err != nil {
		t.Fatal(err)
	}
	var ref string
	end := time.Now().Add(12 * time.Second)
	for {
		if s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND kind='scale' ORDER BY rowid DESC LIMIT 1`, id).Scan(&ref) == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("scale operation not queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	op, _ := m.operation(ref)
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	entry := s.installedApps.Get(id)
	if len(entry.RuntimeReplicas) != 3 {
		t.Fatalf("replicas=%d", len(entry.RuntimeReplicas))
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		selected := s.runtimeProxyEntry(entry, req)
		seen[selected.SidecarURL] = true
		if selected.Token == "" {
			t.Fatal("replica token missing")
		}
	}
	if len(seen) != 3 {
		t.Fatal("requests not distributed across replicas")
	}
	originalTokens := map[string]string{}
	before, _ := m.deployments(id)
	for _, rep := range before {
		if rep.Replica > 0 && rep.Status == "active" {
			token, _ := m.token(rep)
			originalTokens[rep.ID] = token
		}
	}
	for _, want := range []int{1, 3} {
		if err := m.scale(id, want); err != nil {
			t.Fatal(err)
		}
		var latest string
		m.s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND kind='scale' ORDER BY rowid DESC LIMIT 1`, id).Scan(&latest)
		operation, _ := m.operation(latest)
		finished := waitRuntimeOperation(t, m, operation)
		if finished.Status != "succeeded" {
			t.Fatal(finished.Error)
		}
	}
	for _, token := range originalTokens {
		if _, _, _, err := s.appInstallForToken(token); err == nil {
			t.Fatal("stopped replica credential was revived")
		}
	}
	if err := m.stopSync(id, false); err != nil {
		t.Fatal(err)
	}
	ds, _ := m.deployments(id)
	for _, d := range ds {
		if d.Generation == primary.Generation && d.Status != "stopped" {
			t.Fatal("disable left a replica running")
		}
	}
}

func TestManagedRuntimeLocalAdapterOwnsProcessAcrossControllerRestart(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := s.appRuntimeManager()
	bin := buildLocalSidecarFixture(t)
	manifest := sdk.Manifest{Name: "managed-local", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service", Binaries: map[string]string{localPlatform(): "https://example.invalid/never-fetch"}}}
	id := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, id)
	target := filepath.Join(s.localApps.cacheDir, manifest.Name, manifest.Version, "bin")
	os.MkdirAll(filepath.Dir(target), 0700)
	raw, _ := os.ReadFile(bin)
	os.WriteFile(target, raw, 0755)
	prepareRuntimeFixture(t, m, id, runtimePlacement{Type: "local"}, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	pid := s.localApps.PID(id)
	if pid == 0 {
		t.Fatal("managed local process not started")
	}
	pidRecord, err := m.localRecordedPID(d)
	if err != nil || pidRecord != pid {
		t.Fatal("process ownership pid journal missing")
	}
	// New controller has no exec handle for the surviving process.
	original := s.localApps
	s.localApps = NewLocalSupervisor(original.cacheDir)
	b := localRuntimeBackend{m: m, d: d}
	request, _ := m.request(d, runtimeOperation{ID: "inspection"}, "inspect")
	observed, err := b.Inspect(request)
	if err != nil || observed.State != "running" || observed.StoppedConfirmed {
		t.Fatalf("orphan observation: %v", err)
	}
	stopped, err := b.Stop(request)
	if err != nil || !stopped.StoppedConfirmed {
		t.Fatalf("orphan stop: %v", err)
	}
	if processAlive(pid) {
		t.Fatal("confirmed stop left owned process alive")
	}
	original.StopAll(time.Second)
}

func TestManagedRuntimeDiscoveryAndManagementAuthorization(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	if err := s.store.SetPlatformRole(1, PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	providerManifest, _ := installManifest(s, p.Backend.InstallID)
	providerManifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: runtimeControlPrefix + "backends/" + p.Backend.ID}}
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(providerManifest), p.Backend.InstallID)
	found, err := m.discoverBackends(context.Background())
	if err != nil || len(found) != 1 {
		t.Fatalf("manifest discovery: %v", err)
	}
	mux := http.NewServeMux()
	s.registerAppDeploymentRoutes(mux)
	mux.HandleFunc("/apps/installs/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { s.handleAppDeployment(w, r, id) }))
	key := "runtime-management-test-key"
	if _, err := s.store.CreateAPIKey(1, "test", HashAPIKey(key), "runtime"); err != nil {
		t.Fatal(err)
	}
	request := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if got := request("/apps/execution-backends", key); got.Code != 200 {
		t.Fatalf("admin catalog status=%d", got.Code)
	}
	providerToken, _ := s.appInstallToken(p.Backend.InstallID)
	if got := request("/apps/execution-backends", providerToken); got.Code == 200 {
		t.Fatal("app token reached backend management")
	}
	prepareRuntimeFixture(t, m, id, p, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	if got := request(fmt.Sprintf("/apps/installs/%d/deployment", id), key); got.Code != 200 || strings.Contains(got.Body.String(), "credential_encrypted") {
		t.Fatal("deployment API failed or leaked encrypted credential")
	}
	token, _ := m.token(d)
	if got := request("/apps/callback/runtime/ownership", token); got.Code != 200 || !strings.Contains(got.Body.String(), `"active":true`) {
		t.Fatalf("runtime ownership status=%d", got.Code)
	}
	// Even an erroneously public backend declaration cannot expose control
	// through the main app proxy.
	activeManifest := d.Manifest
	activeManifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: runtimeControlPrefix, NoAuth: true}}
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(activeManifest), id)
	s.LoadInstalledApps()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/apps/remote-workload/_install/%d%sapp/activate", id, runtimeControlPrefix), nil)
	s.handleAppProxy(rec, req)
	if rec.Code != 404 {
		t.Fatalf("private route status=%d", rec.Code)
	}
	f.mu.Lock()
	f.failStop = false
	f.mu.Unlock()
	if err := m.stopSync(id, false); err != nil {
		t.Fatal(err)
	}
	if got := request("/apps/callback/runtime/ownership", token); got.Code == 200 {
		t.Fatal("stopped generation retained callback credentials")
	}
}

func TestManagedRuntimeStaticPlacementAndDiscard(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := s.appRuntimeManager()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><head></head><body>static</body></html>"), 0600)
	manifest := sdk.Manifest{Name: "static-test", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "static", StaticDir: dir}, ConfigSchema: []sdk.ConfigField{{Name: "mount_path", Type: "string"}}}
	id := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, id)
	prepareRuntimeFixture(t, m, id, runtimePlacement{Type: "local"}, "prepare")
	d := activateRuntimeFixture(t, m, id, "activate")
	if d.Manifest.Runtime.Kind != "static" || !strings.HasPrefix(d.Execution.Endpoint, "static://") {
		t.Fatal("static local deployment not active")
	}
	candidate := prepareRuntimeFixture(t, m, id, runtimePlacement{Type: "local"}, "prepare-candidate")
	op, err := m.begin(id, "discard", "discard", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	record, _ := m.loadDeployment(candidate.ID)
	if record.Status != "deleted" {
		t.Fatal("discard retained candidate resources")
	}
	active, _ := m.active(id)
	if active.ID != d.ID {
		t.Fatal("discard changed active installation")
	}
	next := prepareRuntimeFixture(t, m, id, runtimePlacement{Type: "local"}, "prepare-next")
	if next.Generation <= candidate.Generation {
		t.Fatal("deployment generations were reused after discard")
	}
}

func TestManagedRuntimeFailedRollbackAndReplicaSQLiteRefusal(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare-old")
	old := activateRuntimeFixture(t, m, id, "activate-old")
	old.Manifest.Runtime.ActivationStrategy = "exclusive"
	m.saveDeployment(old)
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(old.Manifest), id)
	prepareRuntimeFixture(t, m, id, p, "prepare-new")
	f.mu.Lock()
	f.oldID = old.ID
	f.failStart = true
	f.failOldRestart = true
	f.mu.Unlock()
	op, err := m.begin(id, "activate", "activate-failure", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := waitRuntimeOperation(t, m, op)
	if got.Status != "failed" || !strings.Contains(got.Error, "previous deployment readiness failed") {
		t.Fatal("failed rollback falsely verified")
	}
	var status string
	s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status)
	if status == "running" {
		t.Fatal("failed rollback advertised a running installation")
	}
}

func TestManagedRuntimeAsyncPreparationCancellationAndCleanup(t *testing.T) {
	_, m, f, id, p := runtimeTestSetup(t)
	f.mu.Lock()
	f.asyncPrepare = true
	f.holdPrepare = true
	f.mu.Unlock()
	op, err := m.begin(id, "prepare", "async-cancel", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(5 * time.Second)
	for {
		d, e := m.loadDeployment(op.DeploymentID)
		if e == nil && d.Execution.State == "preparing" {
			break
		}
		if time.Now().After(end) {
			t.Fatal("async preparation did not journal handle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = m.cancelOperation(op.ID, id); err != nil {
		t.Fatal(err)
	}
	var cancelFlag int
	if err = m.s.store.db.QueryRow(`SELECT cancel_requested FROM app_runtime_operations WHERE id=?`, op.ID).Scan(&cancelFlag); err != nil || cancelFlag != 1 {
		t.Fatalf("cancel flag=%d error=%v", cancelFlag, err)
	}
	cancelled := waitRuntimeOperation(t, m, op)
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancellation status=%s error=%s", cancelled.Status, cancelled.Error)
	}
	discard, err := m.begin(id, "discard", "discard-cancelled", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitRuntimeOperation(t, m, discard); result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	f.mu.Lock()
	f.holdPrepare = false
	f.mu.Unlock()
	prepareRuntimeFixture(t, m, id, p, "async-ready")
}
func TestManagedRuntimeAutoscalingUsesWorkLoadAndCooldown(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare")
	active := activateRuntimeFixture(t, m, id, "activate")
	f.mu.Lock()
	state := f.executions[active.ID]
	load := 5.0
	state.Load = &load
	f.executions[active.ID] = state
	f.mu.Unlock()
	serveRuntimeWorkload(f)
	collectRuntimeMonitor(t, m, id)
	policy := runtimeScalingPolicy{InstallID: id, Min: 1, Max: 3, TargetLoad: 1, CooldownSeconds: 30, Enabled: true}
	s.store.db.Exec(`INSERT INTO app_runtime_scaling(install_id,record_json) VALUES(?,?)`, id, runtimeJSON(policy))
	m.autoscale()
	var ref string
	if err := s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=? AND kind='scale' ORDER BY rowid DESC LIMIT 1`, id).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	op, _ := m.operation(ref)
	if result := waitRuntimeOperation(t, m, op); result.Status != "succeeded" {
		t.Fatal(result.Error)
	}
	m.autoscale()
	var count int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations WHERE install_id=? AND kind='scale'`, id).Scan(&count)
	if count != 1 {
		t.Fatal("cooldown did not prevent immediate scale-out")
	}
}
