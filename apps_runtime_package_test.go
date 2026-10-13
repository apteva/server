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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"gopkg.in/yaml.v3"
)

type packageBackendFixture struct {
	mu                                                       sync.Mutex
	server                                                   *httptest.Server
	id, token                                                string
	metadata                                                 map[string]runtimePackageMetadata
	files                                                    map[string]map[string][]byte
	sealed                                                   map[string]bool
	executions                                               map[string]runtimeExecution
	starts                                                   map[string]int
	environments                                             map[string]map[string]string
	store                                                    *Store
	interruptUpload, badDigest, badPlatform, badCapabilities bool
	uploads, validations                                     int
	automatic                                                bool
}

func newPackageBackendFixture(t *testing.T, id string) *packageBackendFixture {
	t.Helper()
	f := &packageBackendFixture{id: id, metadata: map[string]runtimePackageMetadata{}, files: map[string]map[string][]byte{}, sealed: map[string]bool{}, executions: map[string]runtimeExecution{}, starts: map[string]int{}, environments: map[string]map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}
func (f *packageBackendFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/exec/") {
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/exec/"), "/")[0]
		if r.Header.Get("Authorization") != "Bearer "+f.environments[id]["APTEVA_APP_TOKEN"] {
			http.Error(w, "process auth required", 403)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(200)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/describe") {
			caps := f.executions[id].AppCapabilities
			if f.badCapabilities {
				caps.ReplicaSafe = !caps.ReplicaSafe
			}
			writeJSON(w, caps)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/drain") {
			writeJSON(w, map[string]bool{"drained": true})
			return
		}
		w.WriteHeader(200)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "provider auth required", 403)
		return
	}
	prefix := runtimeControlPrefix + "backends/" + f.id + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	if tail == "describe" {
		caps := runtimeBackendCaps{Contract: runtimeBackendContract, ID: f.id, RuntimeKinds: []string{"source", "service"}, Platforms: []string{"linux-amd64", "linux-arm64"}, Formats: []string{"binary"}, Protocols: []string{"tcp", "udp"}, ConfirmedStop: true}
		if f.automatic {
			caps.TargetInspection = true
			caps.PackageDelivery = &runtimePackageDeliveryCaps{Protocol: runtimePackageProtocol, Formats: []string{"binary"}}
			caps.TargetSchema = json.RawMessage(`{"properties":{"resource_type":{"const":"compute.machine"}}}`)
		}
		writeJSON(w, caps)
		return
	}
	if tail == "artifacts" {
		var meta runtimePackageMetadata
		if json.NewDecoder(r.Body).Decode(&meta) != nil {
			http.Error(w, "bad metadata", 400)
			return
		}
		if prior, ok := f.metadata[meta.ID]; ok && !reflect.DeepEqual(prior, meta) {
			http.Error(w, "metadata conflict", 409)
			return
		}
		var persisted int
		if f.store != nil {
			f.store.db.QueryRow(`SELECT count(*) FROM app_runtime_deployments WHERE json_extract(record_json,'$.package.metadata.id')=?`, meta.ID).Scan(&persisted)
			if persisted == 0 {
				http.Error(w, "handoff not persisted", 409)
				return
			}
		}
		f.metadata[meta.ID] = meta
		if f.files[meta.ID] == nil {
			f.files[meta.ID] = map[string][]byte{}
		}
		writeJSON(w, meta)
		return
	}
	if strings.HasPrefix(tail, "artifacts/") {
		parts := strings.SplitN(strings.TrimPrefix(tail, "artifacts/"), "/", 2)
		meta, ok := f.metadata[parts[0]]
		if !ok || len(parts) != 2 {
			http.Error(w, "unknown artifact", 400)
			return
		}
		if parts[1] == "seal" {
			for _, file := range meta.Files {
				data, ok := f.files[meta.ID][file.Path]
				if !ok || int64(len(data)) != file.Size || runtimeHash(data) != file.SHA256 {
					http.Error(w, "missing file", 400)
					return
				}
			}
			f.sealed[meta.ID] = true
			writeJSON(w, map[string]any{"artifact_id": meta.ID, "sealed": true})
			return
		}
		if !strings.HasPrefix(parts[1], "files/") || r.Method != http.MethodPut || f.sealed[meta.ID] {
			http.Error(w, "sealed or invalid", 409)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "interrupted", 400)
			return
		}
		f.uploads++
		if f.interruptUpload {
			f.interruptUpload = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		name := strings.TrimPrefix(parts[1], "files/")
		f.files[meta.ID][name] = data
		writeJSON(w, map[string]any{"bytes": len(data), "sha256": runtimeHash(data)})
		return
	}
	var request runtimeBackendRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		http.Error(w, "bad lifecycle request", 400)
		return
	}
	if tail == "inspect-target" {
		writeJSON(w, sdk.RuntimeTargetInfo{Platform: "linux-amd64", HostKey: runtimeHostKey(request.Target), ABI: "gnu", SystemLibraries: []string{"libc.so.6", "libdl.so.2", "libpthread.so.0"}, Formats: []string{"binary"}})
		return
	}
	meta, ok := f.metadata[fmt.Sprint(request.Options["artifact_id"])]
	if !ok || !f.sealed[meta.ID] || request.ArtifactPlatform != meta.Platform || request.GatewayURL != request.Options["gateway_url"] || request.Package == nil || request.Package.ID != meta.ID {
		http.Error(w, "invalid package/gateway handoff", 409)
		return
	}
	out := f.executions[request.DeploymentID]
	switch tail {
	case "validate-target":
		f.validations++
		out.HostKey = runtimeHostKey(request.Target)
		out.Platform = meta.Platform
		for _, file := range meta.Files {
			if file.Path == meta.Executable {
				out.ReleaseDigest = file.SHA256
			}
		}
	case "prepare":
		var digest string
		for _, file := range meta.Files {
			if file.Path == meta.Executable {
				digest = file.SHA256
			}
		}
		out = runtimeExecution{Handle: request.DeploymentID, HostKey: runtimeHostKey(request.Target), State: "prepared", StateRef: "retained-data:" + fmt.Sprint(request.InstallID), ReleaseDigest: digest, Platform: meta.Platform, AppCapabilities: meta.AppCapabilities}
		if f.badDigest {
			out.ReleaseDigest = strings.Repeat("0", 64)
		}
		if f.badPlatform {
			out.Platform = "linux-arm64"
		}
	case "start":
		if request.Environment["APTEVA_GATEWAY_URL"] != request.GatewayURL {
			http.Error(w, "gateway conflict", 409)
			return
		}
		if out.State != "running" {
			f.starts[request.DeploymentID]++
		}
		out.State, out.Ready, out.Endpoint = "running", true, f.server.URL+"/exec/"+request.DeploymentID
		f.environments[request.DeploymentID] = request.Environment
	case "inspect":
	case "stop":
		out.State, out.Ready, out.StoppedConfirmed = "stopped", false, true
	case "delete":
		if out.State == "running" {
			http.Error(w, "still running", 409)
			return
		}
		out.State = "deleted" // Installation data remains owned by its StateRef.
	default:
		http.Error(w, "unknown lifecycle action", 404)
		return
	}
	f.executions[request.DeploymentID] = out
	writeJSON(w, out)
}
func packageTestSetup(t *testing.T, id string) (*Server, *appRuntimeManager, *packageBackendFixture, runtimePlacement) {
	t.Helper()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	m := s.appRuntimeManager()
	f := newPackageBackendFixture(t, id)
	provider := runtimeFixtureInstall(t, s, "package-provider")
	s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, f.server.URL, provider)
	f.mu.Lock()
	f.token, _ = s.appInstallToken(provider)
	f.store = s.store
	f.mu.Unlock()
	t.Cleanup(func() {
		if s.appRuntime != nil {
			s.appRuntime.Close()
		}
		// Go module directories are intentionally read-only. Make only this
		// test's temporary cache writable before testing.T removes it.
		_ = filepath.WalkDir(s.localApps.cacheDir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	if err := m.registerBackend(runtimeBackendRegistration{InstallID: provider, ID: id, Endpoint: runtimeControlPrefix + "backends/" + id, PackageDelivery: &runtimePackageDeliveryCaps{Protocol: runtimePackageProtocol, Formats: []string{"binary"}}}); err != nil {
		t.Fatal(err)
	}
	p := runtimePlacement{Type: "backend", Backend: runtimeBackendRef{provider, id}, Target: runtimeResourceRef{provider, "compute.machine", "test-host"}, GatewayURL: "https://gateway.example.test", Package: &runtimePackageSpec{Platform: "linux-amd64"}}
	return s, m, f, p
}
func packageTestSource(t *testing.T) sdk.Manifest {
	t.Helper()
	repo := t.TempDir()
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "packaged-fixture", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "source", ActivationStrategy: "exclusive", Source: &sdk.SourceSpec{Repo: "file://" + repo, Ref: "v1.0.0"}, Port: 8080, HealthCheck: "/health"}, Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{Prefix: runtimeControlPrefix + "app/"}}}, DB: &sdk.DBConfig{Driver: "sqlite", Path: "/data/app.db", Migrations: "migrations/"}}
	if err := sdk.ValidateManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	manifest = *parsed
	files := map[string][]byte{"apteva.yaml": raw, "main.go": []byte("package main\nfunc main() {}\n"), "go.mod": []byte("module example.test/packagefixture\n\ngo 1.22\n"), "ui/panel.mjs": []byte("export default 'release UI'"), "migrations/001.sql": []byte("CREATE TABLE fixture(id INTEGER);"), "native/library.txt": []byte("required runtime resource"), "apteva.runtime-package.json": []byte(`{"app_capabilities":{"passive_start":true,"drain":true,"replica_safe":false,"worker_coordination":false,"routing":"key"},"files":["native"]}`)}
	for path, content := range files {
		filename := filepath.Join(repo, path)
		if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filename, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Package Test", "-c", "user.email=package@example.test", "commit", "-m", "release fixture"}, {"tag", "v1.0.0"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
	}
	return manifest
}
func packageTestCompiler(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "builds")
	code := fmt.Sprintf(`#!%s
import os,sys,struct
if sys.argv[1]=='version':
 print('go version go1.26.6 darwin/arm64');sys.exit(0)
if sys.argv[1]!='build': sys.exit(2)
assert not any(k.startswith('APTEVA_') for k in os.environ)
assert os.environ['GOWORK']=='off' and os.environ['CGO_ENABLED']=='0'
target=sys.argv[sys.argv.index('-o')+1]
b=bytearray(64);b[:7]=b'\x7fELF\x02\x01\x01'
struct.pack_into('<HHIQQQIHHHHHH',b,16,2,183 if os.environ['GOARCH']=='arm64' else 62,1,0,0,0,0,64,56,0,64,0,0)
open(target,'wb').write(b)
with open(%q,'a') as f: f.write('build\n')
`, python, capture)
	if err = os.WriteFile(filepath.Join(dir, "go"), []byte(code), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("APTEVA_APP_TOKEN", "must-not-reach-package-build")
	return capture
}
func packageTestInstall(t *testing.T, s *Server, manifest sdk.Manifest) int64 {
	t.Helper()
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO apps(name,source,repo,ref,manifest_json) VALUES(?,'git','','',?)`, manifest.Name, runtimeJSON(manifest)); err != nil {
		t.Fatal(err)
	}
	var appID, count int64
	if err := s.store.db.QueryRow(`SELECT id FROM apps WHERE name=?`, manifest.Name).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.QueryRow(`SELECT count(*) FROM app_installs WHERE app_id=?`, appID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("package-project-%d", count)
	if _, err := s.store.db.Exec(`INSERT INTO projects(id,user_id,name) VALUES(?,1,?)`, project, project); err != nil {
		t.Fatal(err)
	}
	res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,version,manifest_json,source,installed_by,permissions_json) VALUES(?,?,'pending',?,?,'git',1,?)`, appID, project, manifest.Version, runtimeJSON(manifest), runtimeJSON(manifest.Requires.Permissions))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func waitPackageOperation(t *testing.T, m *appRuntimeManager, op runtimeOperation) runtimeOperation {
	t.Helper()
	end := time.Now().Add(180 * time.Second)
	for time.Now().Before(end) {
		got, err := m.operation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "succeeded" || got.Status == "failed" || got.Status == "cancelled" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("package operation timed out")
	return runtimeOperation{}
}
func TestManagedRuntimePackageGenericDeliveryAndReuse(t *testing.T) {
	for _, backendID := range []string{"runner-alpha", "future-compute"} {
		t.Run(backendID, func(t *testing.T) {
			capture := packageTestCompiler(t)
			s, m, f, p := packageTestSetup(t, backendID)
			manifest := packageTestSource(t)
			first, second := packageTestInstall(t, s, manifest), packageTestInstall(t, s, manifest)
			op, err := m.begin(first, "upgrade", "first-install", p, &manifest)
			if err != nil {
				t.Fatal(err)
			}
			other, err := m.begin(second, "upgrade", "second-install", p, &manifest)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []runtimeOperation{op, other} {
				if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
					t.Fatalf("deployment failed: %s", got.Error)
				}
			}
			one, _ := m.active(first)
			two, _ := m.active(second)
			if one.Package == nil || two.Package == nil || one.Package.Metadata.ID != two.Package.Metadata.ID || len(one.Package.SourceRevision) != 40 || one.Package.Metadata.Platform != "linux-amd64" {
				t.Fatalf("package reuse/pinning failed: %+v %+v", one.Package, two.Package)
			}
			builds, err := os.ReadFile(capture)
			if err != nil || string(builds) != "build\n" {
				t.Fatalf("concurrent builds were not deduplicated: %q %v", builds, err)
			}
			f.mu.Lock()
			if len(f.metadata) != 1 || f.uploads != len(one.Package.Metadata.Files) || f.starts[one.ID] != 1 || f.starts[two.ID] != 1 {
				t.Errorf("delivery/start counts: metadata=%d uploads=%d starts=%v", len(f.metadata), f.uploads, f.starts)
			}
			for _, path := range []string{"ui/panel.mjs", "migrations/001.sql", "native/library.txt"} {
				if len(f.files[one.Package.Metadata.ID][path]) == 0 {
					t.Errorf("missing runtime resource: %s", path)
				}
			}
			f.mu.Unlock()
			duplicate, err := m.begin(first, "upgrade", "first-install", p, &manifest)
			if err != nil || duplicate.ID != op.ID {
				t.Fatalf("duplicate deployment changed identity: %v %+v", err, duplicate)
			}
			for _, kind := range []string{"stop", "delete"} {
				stop, err := m.begin(first, kind, "first-"+kind, runtimePlacement{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := waitPackageOperation(t, m, stop); got.Status != "succeeded" {
					t.Fatalf("%s failed: %s", kind, got.Error)
				}
			}
			f.mu.Lock()
			if f.executions[one.ID].StateRef == "" || f.executions[two.ID].State != "running" {
				t.Error("delete disturbed installation data or another execution")
			}
			f.mu.Unlock()
		})
	}
}
func TestManagedRuntimePackageInterruptedDeliveryAndRestart(t *testing.T) {
	capture := packageTestCompiler(t)
	s, m, f, p := packageTestSetup(t, "recovery-provider")
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	f.mu.Lock()
	f.interruptUpload = true
	f.mu.Unlock()
	op, err := m.begin(id, "prepare", "interrupted", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	got := waitPackageOperation(t, m, op)
	if got.Status != "failed" {
		t.Fatalf("wanted interrupted delivery failure: %+v", got)
	}
	d, err := m.loadDeployment(op.DeploymentID)
	if err != nil || d.Package == nil {
		t.Fatal("package handoff was not persisted before upload")
	}
	f.mu.Lock()
	validations := f.validations
	f.mu.Unlock()
	if validations != 0 {
		t.Fatal("target validation ran before sealing")
	}
	m.Close()
	dbPath := s.store.path
	if err = s.store.Close(); err != nil {
		t.Fatal(err)
	}
	if s.store, err = NewStore(dbPath); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.store = s.store
	f.mu.Unlock()
	s.appRuntime = nil
	s.runtimeOnce = sync.Once{}
	m = s.appRuntimeManager()
	if err = m.retryOperation(op.ID, id); err != nil {
		t.Fatal(err)
	}
	if got = waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatalf("delivery recovery failed: %s", got.Error)
	}
	recovered, _ := m.loadDeployment(d.ID)
	if recovered.Package.Metadata.ID != d.Package.Metadata.ID || recovered.Package.ExecutableSHA256 != d.Package.ExecutableSHA256 {
		t.Fatal("recovery changed immutable package")
	}
	builds, _ := os.ReadFile(capture)
	if string(builds) != "build\n" {
		t.Fatalf("recovery rebuilt package: %q", builds)
	}
	// Model backend staging loss/restart. The persisted Server package must be
	// sufficient to republish; source/toolchain availability is not required.
	f.mu.Lock()
	f.metadata = map[string]runtimePackageMetadata{}
	f.files = map[string]map[string][]byte{}
	f.sealed = map[string]bool{}
	f.mu.Unlock()
	otherID := packageTestInstall(t, s, manifest)
	other, err := m.begin(otherID, "prepare", "backend-recovery", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got = waitPackageOperation(t, m, other); got.Status != "succeeded" {
		t.Fatalf("backend staging recovery failed: %s", got.Error)
	}
	builds, _ = os.ReadFile(capture)
	if string(builds) != "build\n" {
		t.Fatalf("backend recovery rebuilt package: %q", builds)
	}
}
func TestManagedRuntimePackageRejectsChangedDescriptor(t *testing.T) {
	for _, failure := range []string{"digest", "platform", "capabilities", "gateway"} {
		t.Run(failure, func(t *testing.T) {
			packageTestCompiler(t)
			s, m, f, p := packageTestSetup(t, "validation-provider")
			manifest := packageTestSource(t)
			id := packageTestInstall(t, s, manifest)
			f.mu.Lock()
			f.badDigest = failure == "digest"
			f.badPlatform = failure == "platform"
			f.badCapabilities = failure == "capabilities"
			f.mu.Unlock()
			if failure == "gateway" {
				p.Options = map[string]any{"gateway_url": "https://wrong.example.test"}
			}
			op, err := m.begin(id, "upgrade", "reject", p, &manifest)
			if err != nil {
				t.Fatal(err)
			}
			if got := waitPackageOperation(t, m, op); got.Status != "failed" {
				t.Fatalf("accepted changed %s: %+v", failure, got)
			}
			if _, err = m.active(id); err == nil {
				t.Fatal("invalid candidate became active")
			}
		})
	}
}
func TestManagedRuntimePackagePathsCapabilitiesAndCachePins(t *testing.T) {
	packageTestCompiler(t)
	s, m, _, p := packageTestSetup(t, "cache-provider")
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	op, err := m.begin(id, "prepare", "cache", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	d, _ := m.loadDeployment(op.DeploymentID)
	root, _ := m.packageCacheRoot()
	if err = m.pruneRuntimePackages(root, 1, 1); err == nil {
		t.Fatal("cache GC evicted retained package")
	}
	if _, err = os.Stat(filepath.Join(root, d.Package.Metadata.ID)); err != nil {
		t.Fatal("retained package missing")
	}
	if _, err = s.store.db.Exec(`UPDATE app_runtime_deployments SET status='deleted' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.pruneRuntimePackages(root, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, d.Package.Metadata.ID)); !os.IsNotExist(err) {
		t.Fatal("unreferenced package was not reclaimed")
	}
	for _, name := range []string{"../secret", "/absolute", "ui/../secret", ".env", "ui/.env", "ui\\secret"} {
		if runtimePackagePath(name) {
			t.Errorf("accepted unsafe package path %q", name)
		}
	}
	if err = validateRuntimePackageSpec(runtimePackageSpec{Platform: "linux-amd64", CGO: true}); err == nil {
		t.Fatal("cross-CGO silently accepted")
	}
	module := t.TempDir()
	if _, err = runtimePackageRecipe(module, &runtimePackageSpec{Platform: "linux-amd64"}, manifest, strings.Repeat("a", 40)); err == nil {
		t.Fatal("managed capability flags inferred without release declaration")
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(module, "ui"))
	if _, err = runtimePackageContained(module, "ui/secret"); err == nil {
		t.Fatal("package symlink traversal accepted")
	}
}
func TestManagedRuntimePackageRealTargetBuild(t *testing.T) {
	s, m, _, p := packageTestSetup(t, "real-build-provider")
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	op, err := m.begin(id, "prepare", "real-target", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatalf("real cross-platform package build: %s", got.Error)
	}
	d, _ := m.loadDeployment(op.DeploymentID)
	root, _ := m.packageCacheRoot()
	if err = runtimePackageCheckELF(filepath.Join(root, d.Package.Metadata.ID, d.Package.Metadata.Executable), "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	if d.PackagePreparation.GoVersion == "" {
		t.Fatal("build toolchain not pinned")
	}
	// Recover an interrupted workspace while retaining an active one.
	abandoned := filepath.Join(root, ".build-abandoned")
	os.MkdirAll(abandoned, 0700)
	work, cleanup, err := runtimePackageWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if work == "" {
		t.Fatal("missing workspace")
	}
	if _, err = os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatal("abandoned build workspace not reclaimed")
	}
}

func TestManagedRuntimePackageManifestCanonicalNumbers(t *testing.T) {
	manifest := packageTestSource(t)
	manifest.Imports = map[string]any{"test": map[string]any{"limit": 3}}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimPrefix(manifest.Runtime.Source.Repo, "file://")
	if err = os.WriteFile(filepath.Join(root, "apteva.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var stored sdk.Manifest
	if err = json.Unmarshal([]byte(runtimeJSON(manifest)), &stored); err != nil {
		t.Fatal(err)
	}
	if err = runtimePackageManifestMatches(root, stored); err != nil {
		t.Fatalf("JSON persisted schema numbers rejected: %v", err)
	}
	stored.Requires.Permissions = []sdk.Permission{"platform.apps.call"}
	if err = runtimePackageManifestMatches(root, stored); err == nil {
		t.Fatal("changed approved permissions accepted")
	}
}

func TestManagedRuntimePackageDeliveryCapabilityValidation(t *testing.T) {
	backend := runtimeBackendCaps{Formats: []string{"binary", "image"}}
	if err := validateRuntimePackageDelivery(&runtimePackageDeliveryCaps{Protocol: runtimePackageProtocol, Formats: []string{"binary", "image"}}, backend); err != nil {
		t.Fatal(err)
	}
	for _, caps := range []runtimePackageDeliveryCaps{{Protocol: "unknown", Formats: []string{"binary"}}, {Protocol: runtimePackageProtocol, Formats: []string{"tar"}}, {Protocol: runtimePackageProtocol, Formats: []string{"binary", "unknown"}}} {
		if err := validateRuntimePackageDelivery(&caps, backend); err == nil {
			t.Fatal("unnegotiated delivery accepted")
		}
	}
}

func TestManagedRuntimePackageVerifiedArchiveHandoff(t *testing.T) {
	packageTestCompiler(t)
	s, m, f, p := packageTestSetup(t, "archive-provider")
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	op, err := m.begin(id, "prepare", "source-package", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	d, _ := m.loadDeployment(op.DeploymentID)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	writer := tar.NewWriter(gz)
	f.mu.Lock()
	for _, file := range d.Package.Metadata.Files {
		name := "src/" + file.Path
		if file.Path == d.Package.Metadata.Executable {
			name = "bin"
		}
		if err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: file.Size}); err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write(f.files[d.Package.Metadata.ID][file.Path]); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Unlock()
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive.Bytes()) }))
	defer upstream.Close()
	sha := runtimeHash(archive.Bytes())
	manifest.Runtime.Artifacts = map[string]sdk.BundleSpec{"linux-amd64": {URL: upstream.URL, SHA256: sha}}
	p.Package.AppCapabilities = &d.Package.Metadata.AppCapabilities
	p.Package.Files = []string{"native"}
	rejectID := packageTestInstall(t, s, manifest)
	rejected, err := m.begin(rejectID, "prepare", "unsupported-archive", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, rejected); got.Status != "failed" || !strings.Contains(got.Error, "archive") {
		t.Fatalf("archive verification was bypassed: %+v", got)
	}
	reg, _ := m.registration(p.Backend)
	reg.PackageDelivery.VerifiedArchives = true
	if err = m.registerBackend(reg); err != nil {
		t.Fatal(err)
	}
	archiveID := packageTestInstall(t, s, manifest)
	verified, err := m.begin(archiveID, "prepare", "verified-archive", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, verified); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	prepared, _ := m.loadDeployment(verified.DeploymentID)
	if prepared.Package.ArchiveSHA256 != sha || prepared.Package.ExecutableSHA256 == sha || prepared.Execution.ReleaseDigest != prepared.Package.ExecutableSHA256 {
		t.Fatal("archive and executable checksums conflated")
	}
	req, err := m.request(prepared, verified, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if req.Manifest.Runtime.Artifacts["linux-amd64"].SHA256 != sha || req.Package.ArchiveSHA256 != sha {
		t.Fatal("original archive verification removed from backend handoff")
	}
}

func TestManagedRuntimePackageCacheMultipleBuildKeysAndCorruption(t *testing.T) {
	packageTestCompiler(t)
	s, m, _, p := packageTestSetup(t, "content-provider")
	manifest := packageTestSource(t)
	var deployments []runtimeDeployment
	for i := 0; i < 2; i++ {
		q := p
		spec := *p.Package
		q.Package = &spec
		if i == 1 {
			q.Package.Files = []string{"native", "ui"}
		}
		id := packageTestInstall(t, s, manifest)
		op, err := m.begin(id, "prepare", fmt.Sprintf("cache-%d", i), q, &manifest)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
			t.Fatal(got.Error)
		}
		d, _ := m.loadDeployment(op.DeploymentID)
		deployments = append(deployments, d)
	}
	one, two := deployments[0].Package, deployments[1].Package
	if one.Metadata.ID != two.Metadata.ID || one.BuildKey == two.BuildKey {
		t.Fatal("content identity not independent of build lookup key")
	}
	if _, err := m.cachedRuntimePackage(two.BuildKey); err != nil {
		t.Fatal(err)
	}
	root, _ := m.packageCacheRoot()
	if err := os.WriteFile(filepath.Join(root, one.Metadata.ID, "ui/panel.mjs"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.cachedRuntimePackage(one.BuildKey); err == nil {
		t.Fatal("corrupt retained package accepted")
	}
}

// Optional release conformance check: builds/packages the released app locally
// and delivers it only to a loopback fixture. It never starts Telephony or
// contacts a VPS, carrier, production Server, or an existing installation.
func TestManagedRuntimePackageTelephonyRelease(t *testing.T) {
	if os.Getenv("APTEVA_RUNTIME_PACKAGE_RELEASE_TEST") != "1" {
		t.Skip("set APTEVA_RUNTIME_PACKAGE_RELEASE_TEST=1 for the released-source package conformance check")
	}
	cmd := exec.Command("git", "-C", "../apps", "show", "telephony/v0.12.14:mcp/telephony/apteva.yaml")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, m, _, p := packageTestSetup(t, "released-package-fixture")
	p.Package.AppCapabilities = &runtimeAppCaps{PassiveStart: true, Drain: true, Routing: "key"}
	p.Package.Files = []string{"native"}
	p.Package.ABI = "gnu"
	p.Package.SystemLibraries = []string{"libc.so.6", "libdl.so.2", "libpthread.so.0"}
	id := packageTestInstall(t, s, *manifest)
	op, err := m.begin(id, "prepare", "released-source-package", p, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatalf("released package preparation failed: %s", got.Error)
	}
	d, err := m.loadDeployment(op.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Package.Metadata.AppVersion != "0.12.14" || d.Package.Metadata.Platform != "linux-amd64" {
		t.Fatal("wrong release package")
	}
	t.Logf("verified release package %s: %d files, executable %s", d.Package.Metadata.ID, len(d.Package.Metadata.Files), d.Package.ExecutableSHA256)
}
