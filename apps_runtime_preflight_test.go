package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func requireRuntimeCheck(t *testing.T, checks []runtimeCompatibilityCheck, code, status string) {
	t.Helper()
	for _, check := range checks {
		if check.Code == code && check.Status == status {
			return
		}
	}
	t.Fatalf("missing %s/%s in %+v", code, status, checks)
}
func assertNoRuntimeMutation(t *testing.T, s *Server, id int64) {
	t.Helper()
	for _, table := range []string{"app_runtime_operations", "app_runtime_deployments"} {
		var n int
		if err := s.store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("preflight changed %s: %d %v", table, n, err)
		}
	}
	var managed int
	if err := s.store.db.QueryRow(`SELECT runtime_managed FROM app_installs WHERE id=?`, id).Scan(&managed); err != nil || managed != 0 {
		t.Fatalf("source adopted during preflight: %d %v", managed, err)
	}
}

func TestRuntimeConsoleIdentityFreeInspection(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	reg, _ := m.registration(p.Backend)
	reg.Caps.TargetInspection = true
	if _, err := s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/inspect-target") {
			original.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") == "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("invalid read-only inspection headers")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for _, key := range []string{"install_id", "deployment_id", "generation", "operation_id", "idempotency_key", "environment", "manifest", "execution"} {
			if _, exists := body[key]; exists {
				t.Errorf("execution identity/contents leaked to inspection: %s", key)
			}
		}
		for _, key := range []string{"contract", "target", "project_id", "gateway_url"} {
			if _, ok := body[key]; !ok {
				t.Errorf("missing %s", key)
			}
		}
		var input runtimeTargetInspectionRequest
		raw, _ := json.Marshal(body)
		json.Unmarshal(raw, &input)
		if input.Contract != runtimeBackendContract || input.Target != p.Target || input.GatewayURL != p.GatewayURL || input.Options["gateway_url"] != p.GatewayURL {
			t.Errorf("bad inspection context: %+v", input)
		}
		writeJSON(w, sdk.RuntimeTargetInfo{HostKey: "host:test", Platform: "linux-amd64", Formats: []string{"source"}})
	})
	req := authedRequest(t, "POST", "/apps/deployment-preflight", "", map[string]any{"install_id": id, "placement": p})
	w := httptest.NewRecorder()
	s.handleRuntimeConsolePreflight(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"compatible":true`) || calls.Load() != 1 {
		t.Fatalf("preflight %d %s (%d inspections)", w.Code, w.Body, calls.Load())
	}
	assertNoRuntimeMutation(t, s, id)
	if len(f.events) != 0 {
		t.Fatal("inspection performed execution operations")
	}
	// An unpublished, brand-new installation also has no execution identity.
	manifest, _ := installManifest(s, id)
	_, checks := m.preflight(context.Background(), 0, "", *manifest, p)
	if err := firstRuntimeBlock(checks); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("fresh app was not inspected")
	}
	assertNoRuntimeMutation(t, s, id)
}

func TestRuntimeConsoleInspectionErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"legacy execution validator", 400, `{"error":"install_id deployment_id generation required secret-token"}`, "inspection_protocol_incompatible"},
		{"missing endpoint", 405, `{}`, "inspection_protocol_incompatible"},
		{"ssh", 502, `{"code":"ssh_unreachable","error":"secret-token"}`, "ssh_unreachable"},
		{"systemd", 400, `{"code":"systemd_unavailable"}`, "systemd_unavailable"},
		{"gateway", 502, `{"code":"gateway_unreachable"}`, "gateway_unreachable"},
		{"capacity", 409, `{"code":"capacity_insufficient"}`, "capacity_insufficient"},
		{"authentication", 403, `{"error":"secret-token"}`, "backend_authentication_failed"},
		{"invalid json", 200, `not-json secret-token`, "inspection_protocol_incompatible"},
		{"invalid target metadata", 200, `{"platform":"linux-amd64","formats":["source"]}`, "inspection_protocol_incompatible"},
		{"diagnostic failure", 200, `{"platform":"linux-amd64","host_key":"h","formats":["source"],"checks":[{"name":"gateway","status":"blocked"}]}`, "target_gateway_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			reg, _ := m.registration(p.Backend)
			reg.Caps.TargetInspection = true
			s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID)
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/inspect-target") {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				original.ServeHTTP(w, r)
			})
			manifest, _ := installManifest(s, id)
			_, checks := m.preflight(context.Background(), id, "", *manifest, p)
			requireRuntimeCheck(t, checks, tc.code, "blocked")
			if strings.Contains(runtimeJSON(checks), "secret-token") {
				t.Fatal("backend private error leaked")
			}
			assertNoRuntimeMutation(t, s, id)
			if len(f.events) != 0 {
				t.Fatal("inspection tried execution operations")
			}
		})
	}
	code, _ := runtimeInspectionFailure(context.DeadlineExceeded)
	if code != "inspection_timeout" {
		t.Fatal(code)
	}
}

func legacyRuntimeStateFixture(t *testing.T, s *Server, m *appRuntimeManager, id int64, running bool) (sdk.Manifest, string, int) {
	t.Helper()
	manifest, _ := installManifest(s, id)
	manifest.DB = &sdk.DBConfig{Driver: "sqlite"}
	manifest.Runtime.Package = &sdk.RuntimePackage{AppCapabilities: &sdk.RuntimeAppCapabilities{PassiveStart: true, Drain: true, Routing: "key"}}
	state := m.localStateDir(runtimeDeployment{InstallID: id, Manifest: *manifest})
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "app.db"), []byte("persistent application data"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := "/bin/echo"
	port, pid := 0, 0
	endpoint := ""
	status := "disabled"
	if running {
		bin = buildLocalSidecarFixture(t)
		token, err := s.appInstallToken(id)
		if err != nil {
			t.Fatal(err)
		}
		spec := localSidecarSpec(t, id, manifest.Name, bin, 0, map[string]string{"APTEVA_APP_TOKEN": token, "APTEVA_APP_CONFIG": `{"original":"config"}`})
		if err = s.localApps.activate(spec, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		port = spec.httpPort
		pid = s.localApps.PID(id)
		endpoint = localSidecarURL(int64(port))
		status = "running"
	}
	if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name) VALUES('migration-project',1,'Migration test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='migration-project',integration_bindings='[999]',manifest_json=?,local_bin_path=?,local_port=?,local_pid=?,sidecar_url_override=?,status=? WHERE id=?`, runtimeJSON(manifest), bin, port, pid, endpoint, status, id); err != nil {
		t.Fatal(err)
	}
	return *manifest, state, pid
}

func TestManagedRuntimeLegacyPreflightMissingSnapshots(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint("running=", running), func(t *testing.T) {
			s, m, f, id, p := runtimeTestSetup(t)
			manifest, state, pid := legacyRuntimeStateFixture(t, s, m, id, running)
			reg, _ := m.registration(p.Backend)
			reg.Caps.Snapshots = false
			s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID)
			_, checks := m.preflight(context.Background(), id, "migration-project", manifest, p)
			requireRuntimeCheck(t, checks, "destination_snapshots_required", "blocked")
			assertNoRuntimeMutation(t, s, id)
			_, err := m.begin(id, "prepare", "must-not-build", p, &manifest)
			if err == nil || !strings.Contains(err.Error(), "destination backend does not support state snapshots") {
				t.Fatalf("expected destination snapshot blocker: %v", err)
			}
			assertNoRuntimeMutation(t, s, id)
			if running && (!processAlive(pid) || s.localApps.PID(id) != pid) {
				t.Fatal("blocked migration interrupted source")
			}
			raw, _ := os.ReadFile(filepath.Join(state, "app.db"))
			if string(raw) != "persistent application data" {
				t.Fatal("source data changed")
			}
			if len(f.events) != 0 {
				t.Fatal("blocked migration built or prepared a destination")
			}
		})
	}
}

func TestManagedRuntimeMigrationPreflightLimits(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	manifest, state, _ := legacyRuntimeStateFixture(t, s, m, id, false)
	source, err := m.existingSource(id)
	if err != nil {
		t.Fatal(err)
	}
	// Real files count as persistent state even without a database declaration.
	source.Deployment.Manifest.DB = nil
	manifest.DB = nil
	checks := m.migrationChecks(context.Background(), source.Deployment, manifest, p, nil, false)
	if err := firstRuntimeBlock(checks); err != nil {
		t.Fatal(err)
	}
	manifest.Version = "2.0.0"
	manifest.Runtime.Package = nil
	checks = m.migrationChecks(context.Background(), source.Deployment, manifest, p, nil, false)
	requireRuntimeCheck(t, checks, "destination_lifecycle_required", "blocked")
	manifest.Runtime.Storage = []sdk.StorageSpec{{Name: "unmapped", MountPath: "/data"}}
	checks = m.migrationChecks(context.Background(), source.Deployment, manifest, runtimePlacement{Type: "local"}, nil, true)
	requireRuntimeCheck(t, checks, "storage_mapping_unsupported", "blocked")
	if err := os.Symlink("/private", filepath.Join(state, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectRuntimeState(state); err == nil {
		t.Fatal("state symlink accepted")
	}
	os.Remove(filepath.Join(state, "link"))
	large, err := os.Create(filepath.Join(state, "too-large"))
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(runtimeSnapshotLimit + 1); err != nil {
		t.Fatal(err)
	}
	large.Close()
	if _, err = inspectRuntimeState(state); err == nil {
		t.Fatal("oversize state accepted")
	}
	if len(f.events) != 0 {
		t.Fatal("preflight performed remote execution work")
	}
	// Transfer itself enforces the shutdown invariant, even if called directly.
	var op runtimeOperation
	if err = m.transferState(&runtimeDeployment{Status: "active"}, &runtimeDeployment{}, &op); err == nil || !strings.Contains(err.Error(), "confirmed source shutdown") {
		t.Fatal(err)
	}
}

func TestManagedRuntimeLegacyMigrationPreservesIdentityAndData(t *testing.T) {
	for _, failure := range []string{"none", "start", "restore-digest"} {
		t.Run(failure, func(t *testing.T) {
			failStart := failure == "start"
			failRestore := failure == "restore-digest"
			s, m, f, id, p := runtimeTestSetup(t)
			manifest, state, pid := legacyRuntimeStateFixture(t, s, m, id, true)
			beforeToken, err := s.appInstallToken(id)
			if err != nil {
				t.Fatal(err)
			}
			var restores atomic.Int32
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/restore") || strings.HasSuffix(r.URL.Path, "/start") {
					if processAlive(pid) {
						t.Error("destination received state/start while source still writable")
					}
				}
				if strings.HasSuffix(r.URL.Path, "/restore") {
					restores.Add(1)
					if failRestore {
						w.Header().Set("X-Apteva-Snapshot-SHA256", strings.Repeat("0", 64))
						w.WriteHeader(200)
						return
					}
				}
				if strings.HasSuffix(r.URL.Path, "/start") && restores.Load() != 1 {
					t.Error("candidate started before verified restore")
				}
				original.ServeHTTP(w, r)
			})
			d := prepareRuntimeFixture(t, m, id, p, "prepare-legacy-move")
			if !processAlive(pid) {
				t.Fatal("preparation stopped source")
			}
			old, err := m.active(id)
			if err != nil {
				t.Fatal(err)
			}
			if old.InstallID != id || !old.Placement.local() || !old.PersistentData {
				t.Fatalf("legacy provenance lost: %+v", old)
			}
			f.mu.Lock()
			f.failStart = failStart
			f.mu.Unlock()
			op, err := m.begin(id, "activate", "activate-legacy-move", runtimePlacement{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := waitRuntimeOperation(t, m, op)
			if failStart || failRestore {
				if result.Status != "failed" {
					t.Fatal("startup failure succeeded")
				}
				active, _ := m.active(id)
				if active.ID != old.ID || !active.Placement.local() || s.localApps.PID(id) == 0 {
					t.Fatalf("local rollback not verified: %s", result.Error)
				}
				// The exact previous credentials and configuration must be reused.
				proc := s.localApps.currentProc(id)
				if proc.spec.env["APTEVA_APP_TOKEN"] != beforeToken || proc.spec.env["APTEVA_APP_CONFIG"] != `{"original":"config"}` {
					t.Fatal("rollback changed original configuration")
				}
			} else {
				if result.Status != "succeeded" {
					t.Fatal(result.Error)
				}
				active, _ := m.active(id)
				if active.ID != d.ID {
					t.Fatal("remote route not committed")
				}
				if processAlive(pid) {
					t.Fatal("old local process still running")
				}
				f.mu.Lock()
				snapshot := append([]byte(nil), f.snapshot...)
				f.mu.Unlock()
				restored := filepath.Join(t.TempDir(), "restored")
				archive := filepath.Join(t.TempDir(), "state.tar.gz")
				os.WriteFile(archive, snapshot, 0600)
				if err := readRuntimeSnapshot(archive, restored); err != nil {
					t.Fatal(err)
				}
				data, _ := os.ReadFile(filepath.Join(restored, "app.db"))
				if string(data) != "persistent application data" {
					t.Fatal("restored state differs")
				}
			}
			if restores.Load() != 1 {
				t.Fatal("state restore missing or duplicated")
			}
			data, _ := os.ReadFile(filepath.Join(state, "app.db"))
			if !bytes.Equal(data, []byte("persistent application data")) {
				t.Fatal("local data lost")
			}
			var project, name, bindings string
			if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,''),a.name,i.integration_bindings FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).Scan(&project, &name, &bindings); err != nil {
				t.Fatal(err)
			}
			if project != "migration-project" || name != manifest.Name || bindings != "[999]" {
				t.Fatal("installation identity/project changed")
			}
		})
	}
}

func TestManagedRuntimeRemoteStateSizePreflight(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "initial-prepare")
	old := activateRuntimeFixture(t, m, id, "initial-activate")
	old.Manifest.DB = &sdk.DBConfig{Driver: "sqlite"}
	if err := m.saveDeployment(old); err != nil {
		t.Fatal(err)
	}
	p.Target.ID = "another-machine"
	for _, tc := range []struct {
		name    string
		size    *int64
		blocked bool
	}{
		{"known empty", new(int64), false}, {"unknown", nil, true},
		{"over limit", func() *int64 { n := runtimeSnapshotLimit + 1; return &n }(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			execution := f.executions[old.ID]
			execution.StateBytes = tc.size
			f.executions[old.ID] = execution
			f.mu.Unlock()
			checks := m.migrationChecks(context.Background(), old, old.Manifest, p, nil, false)
			status := "passed"
			if tc.blocked {
				status = "blocked"
			}
			requireRuntimeCheck(t, checks, "state_size_unverified", status)
			if err := firstRuntimeBlock(checks); (err != nil) != tc.blocked {
				t.Fatal(err)
			}
		})
	}
	reg, _ := m.registration(p.Backend)
	reg.Caps.Snapshots = false
	s.store.db.Exec(`UPDATE app_runtime_backends SET record_json=? WHERE install_id=? AND backend_id=?`, runtimeJSON(reg), reg.InstallID, reg.ID)
	checks := m.migrationChecks(context.Background(), old, old.Manifest, p, nil, false)
	requireRuntimeCheck(t, checks, "source_snapshots_required", "blocked")
	requireRuntimeCheck(t, checks, "destination_snapshots_required", "blocked")
	// A same-target update reuses its state rather than demanding a migration.
	p.Target = old.Placement.Target
	if checks = m.migrationChecks(context.Background(), old, old.Manifest, p, nil, false); len(checks) != 0 {
		t.Fatal(checks)
	}
}

func TestRuntimeConsolePackagePreflightRequirements(t *testing.T) {
	manifest := sdk.Manifest{Runtime: sdk.Runtime{Kind: "source", Package: &sdk.RuntimePackage{ABI: "gnu", SystemLibraries: []string{"libc.so.6"}}}}
	reg := runtimeBackendRegistration{Caps: runtimeBackendCaps{RuntimeKinds: []string{"source"}, Platforms: []string{"linux-amd64"}}}
	p := runtimePlacement{Package: &runtimePackageSpec{Platform: "linux-amd64"}}
	target := sdk.RuntimeTargetInfo{Platform: "linux-amd64", ABI: "gnu", SystemLibraries: []string{"libc.so.6"}, Formats: []string{"binary"}}
	if err := runtimeReleaseTargetCompatibility(manifest, p, reg, &target); err != nil {
		t.Fatal(err)
	}
	reg.Caps.Formats = []string{"binary"}
	reg.PackageDelivery = &runtimePackageDeliveryCaps{Protocol: runtimePackageProtocol, Formats: []string{"binary"}}
	manifest.Runtime.Source = &sdk.SourceSpec{Repo: "github.com/example/app", Ref: "v1.0.0"}
	manifest.Runtime.Package.CGO = true
	if err := runtimeReleaseTargetCompatibility(manifest, p, reg, &target); err == nil || !strings.Contains(err.Error(), "CGO") {
		t.Fatalf("missing early CGO check: %v", err)
	}
	manifest.Runtime.Package.CGO = false
	target.ABI = "musl"
	if err := runtimeReleaseTargetCompatibility(manifest, p, reg, &target); err == nil {
		t.Fatal("accepted incompatible ABI")
	}
	target.ABI = "gnu"
	target.SystemLibraries = nil
	if err := runtimeReleaseTargetCompatibility(manifest, p, reg, &target); err == nil {
		t.Fatal("accepted missing library")
	}
	target.SystemLibraries = []string{"libc.so.6"}
	target.Platform = "linux-arm64"
	if err := runtimeReleaseTargetCompatibility(manifest, p, reg, &target); err == nil {
		t.Fatal("accepted mismatched target platform")
	}
}

func TestManagedRuntimeStoppedLegacyFailureRemainsStopped(t *testing.T) {
	s, m, f, id, p := runtimeTestSetup(t)
	_, state, _ := legacyRuntimeStateFixture(t, s, m, id, false)
	prepareRuntimeFixture(t, m, id, p, "prepare-stopped-source")
	f.mu.Lock()
	f.failStart = true
	f.mu.Unlock()
	op, err := m.begin(id, "activate", "fail-stopped-source", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !op.PreviousStopped {
		t.Fatal("original stopped state not journaled")
	}
	result := waitRuntimeOperation(t, m, op)
	if result.Status != "failed" || !strings.Contains(result.Error, "remains stopped") {
		t.Fatalf("wrong rollback result: %+v", result)
	}
	var status string
	if err = s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || s.localApps.PID(id) != 0 {
		t.Fatal("failed migration started a previously stopped source")
	}
	raw, err := os.ReadFile(filepath.Join(state, "app.db"))
	if err != nil || string(raw) != "persistent application data" {
		t.Fatal("stopped installation data changed", err)
	}
}
