package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Regression coverage for packaged apps losing their UI after a restart:
// boot resume rebuilt <cache>/<app>/<version>/src/<entry>/ui even though the
// installed binary lives in <version>/packages/<digest>/ with its own source
// tree. The process passed its health check while every widget module 404'd.

const packagedTodoManifest = `schema: apteva-app/v1
name: todo
version: 0.5.1
runtime:
  kind: source
  port: 8080
  source:
    repo: https://example.invalid/never-clone
    ref: main
    entry: mcp/todo
db:
  driver: sqlite
  path: /data/todo.db
  migrations: migrations/
provides:
  ui_components:
    - name: todo-list
      entry: /ui/TodoListWidget.mjs
      slots: [dashboard.home]
`

const (
	packageA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	packageB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func packagedTodo(t *testing.T) *sdk.Manifest {
	t.Helper()
	m, err := sdk.ParseManifest([]byte(packagedTodoManifest))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func writePackageFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// writeSourcePackage lays out only the hashed source-package tree: no legacy
// <version>/src directory and no compatibility symlink.
func writeSourcePackage(t *testing.T, cacheDir string, m *sdk.Manifest, digest string, fixture []byte, migrations ...string) string {
	t.Helper()
	pkg := filepath.Join(cacheDir, m.Name, m.Version, "packages", digest)
	entry := filepath.Join(pkg, "src", "mcp", "todo")
	writePackageFile(t, filepath.Join(pkg, "bin"), fixture, 0755)
	writePackageFile(t, filepath.Join(entry, "apteva.yaml"), []byte(packagedTodoManifest), 0644)
	writePackageFile(t, filepath.Join(entry, "ui", "TodoListWidget.mjs"), []byte(todoModule(digest)), 0644)
	writePackageFile(t, filepath.Join(entry, "ui", "assets", "todo.css"), []byte(".todo{color:"+digest[:6]+"}"), 0644)
	for _, name := range migrations {
		writePackageFile(t, filepath.Join(entry, "migrations", name), []byte("CREATE TABLE IF NOT EXISTS todos (id INTEGER);"), 0644)
	}
	return filepath.Join(pkg, "bin")
}

func todoModule(digest string) string {
	return "export default function TodoListWidget() { return '" + digest[:6] + "' }"
}

func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(buildLocalSidecarFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fetchSidecar(t *testing.T, port int, path string) (int, string) {
	t.Helper()
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

type sidecarObservedState struct {
	UIDir         string   `json:"ui_dir"`
	MigrationsDir string   `json:"migrations_dir"`
	Applied       []string `json:"applied"`
	Records       []string `json:"records"`
}

func sidecarStateAt(t *testing.T, port int, method, title string) sidecarObservedState {
	t.Helper()
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d/state?title=%s", port, title), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st sidecarObservedState
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func waitSidecarHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sidecar on port %d never became healthy", port)
}

// assertPackagedWidget fetches the widget module and a supporting asset and
// requires the selected package's exact bytes.
func assertPackagedWidget(t *testing.T, port int, digest, label string) {
	t.Helper()
	if code, body := fetchSidecar(t, port, "/ui/TodoListWidget.mjs"); code != 200 || body != todoModule(digest) {
		t.Fatalf("%s: widget module HTTP %d body %q", label, code, body)
	}
	if code, body := fetchSidecar(t, port, "/ui/assets/todo.css"); code != 200 || !strings.Contains(body, digest[:6]) {
		t.Fatalf("%s: widget asset HTTP %d body %q", label, code, body)
	}
}

// assertNoRebuild proves a cached restart neither cloned, built nor
// downloaded: the version directory holds exactly the packages written by
// the test, and no legacy src directory appeared.
func assertNoRebuild(t *testing.T, cacheDir string, m *sdk.Manifest, layout string, want ...string) {
	t.Helper()
	versionRoot := filepath.Join(cacheDir, m.Name, m.Version)
	entries, err := os.ReadDir(versionRoot)
	if err != nil {
		t.Fatal(err)
	}
	var top []string
	for _, e := range entries {
		top = append(top, e.Name())
	}
	if !reflect.DeepEqual(top, []string{layout}) {
		t.Fatalf("version root changed on restart: %v", top)
	}
	pkgs, _ := os.ReadDir(filepath.Join(versionRoot, layout))
	var got []string
	for _, e := range pkgs {
		got = append(got, e.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("packages changed on restart: %v, want %v", got, want)
	}
}

func TestPackagedResourceDirsFollowTheExactExecutable(t *testing.T) {
	m := packagedTodo(t)
	cases := []struct {
		name, bin    string
		packaged     bool
		ui, migrated string
	}{
		{name: "source package", bin: "/c/todo/0.5.1/packages/" + packageA + "/bin", packaged: true,
			ui: "/c/todo/0.5.1/packages/" + packageA + "/src/mcp/todo/ui", migrated: "/c/todo/0.5.1/packages/" + packageA + "/src/mcp/todo/migrations"},
		{name: "second package same version", bin: "/c/todo/0.5.1/packages/" + packageB + "/bin", packaged: true,
			ui: "/c/todo/0.5.1/packages/" + packageB + "/src/mcp/todo/ui", migrated: "/c/todo/0.5.1/packages/" + packageB + "/src/mcp/todo/migrations"},
		{name: "artifact", bin: "/c/todo/0.5.1/artifacts/" + packageA + "/bin", packaged: true,
			ui: "/c/todo/0.5.1/artifacts/" + packageA + "/src/mcp/todo/ui", migrated: "/c/todo/0.5.1/artifacts/" + packageA + "/src/mcp/todo/migrations"},
		{name: "legacy flat cache", bin: "/c/todo/0.5.1/bin"},
		{name: "local development build", bin: "/work/todo/bin"},
		{name: "not a digest", bin: "/c/todo/0.5.1/packages/latest/bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"APTEVA_UI_DIR":         "/c/todo/0.5.1/src/mcp/todo/ui",
				"APTEVA_MIGRATIONS_DIR": "/c/todo/0.5.1/src/mcp/todo/migrations",
				"DB_PATH":               "/c/todo/data/7/app.db",
			}
			before := map[string]string{}
			for k, v := range env {
				before[k] = v
			}
			dirs, _, ok, err := packagedResourceDirs(m, tc.bin)
			if err != nil || ok != tc.packaged {
				t.Fatalf("packaged=%v err=%v", ok, err)
			}
			if !ok {
				if err := applyPackagedResourceEnv(7, m, tc.bin, env); err != nil || !reflect.DeepEqual(env, before) {
					t.Fatalf("non-packaged layout changed: %v %#v", err, env)
				}
				return
			}
			dirs.applyEnv(env)
			if env["APTEVA_UI_DIR"] != tc.ui || env["APTEVA_MIGRATIONS_DIR"] != tc.migrated || env["DB_PATH"] != before["DB_PATH"] {
				t.Fatalf("resolved %#v", env)
			}
		})
	}

	t.Run("empty entry is the source root", func(t *testing.T) {
		root := *m
		root.Runtime.Source = &sdk.SourceSpec{Repo: "x", Entry: ""}
		dirs, _, _, _ := packagedResourceDirs(&root, "/c/todo/0.5.1/packages/"+packageA+"/bin")
		if dirs.UI != "/c/todo/0.5.1/packages/"+packageA+"/src/ui" {
			t.Fatal(dirs.UI)
		}
	})
	t.Run("absolute migrations are kept", func(t *testing.T) {
		abs := *m
		db := *m.DB
		db.Migrations = "/srv/todo-migrations"
		abs.DB = &db
		dirs, _, _, _ := packagedResourceDirs(&abs, "/c/todo/0.5.1/packages/"+packageA+"/bin")
		if dirs.Migrations != "/srv/todo-migrations" {
			t.Fatal(dirs.Migrations)
		}
	})
	t.Run("no declared migrations clears an obsolete path", func(t *testing.T) {
		none := *m
		none.DB = nil
		env := map[string]string{"APTEVA_MIGRATIONS_DIR": "/c/todo/0.5.1/src/mcp/todo"}
		dirs, _, _, _ := packagedResourceDirs(&none, "/c/todo/0.5.1/packages/"+packageA+"/bin")
		dirs.applyEnv(env)
		if _, ok := env["APTEVA_MIGRATIONS_DIR"]; ok {
			t.Fatal("obsolete migrations path kept")
		}
	})
	t.Run("entry cannot leave the package", func(t *testing.T) {
		escape := *m
		escape.Runtime.Source = &sdk.SourceSpec{Repo: "x", Entry: "../../other"}
		if err := applyPackagedResourceEnv(7, &escape, "/c/todo/0.5.1/packages/"+packageA+"/bin", map[string]string{}); err == nil || !strings.Contains(err.Error(), "install 7") {
			t.Fatalf("escaping entry accepted: %v", err)
		}
	})
	t.Run("relocated cache keeps the recorded package", func(t *testing.T) {
		cache := t.TempDir()
		for _, digest := range []string{packageA, packageB} {
			writePackageFile(t, filepath.Join(cache, "todo", "0.5.1", "packages", digest, "bin"), []byte("x"), 0755)
		}
		got, ok := portableLocalBinPath("/old-host/apps/todo/0.5.1/packages/"+packageB+"/bin", cache, "todo", "0.5.1")
		if !ok || got != filepath.Join(cache, "todo", "0.5.1", "packages", packageB, "bin") {
			t.Fatalf("rebase: %s %v", got, ok)
		}
		if _, ok := portableLocalBinPath("/old-host/apps/todo/0.5.0/packages/"+packageB+"/bin", cache, "todo", "0.5.1"); ok {
			t.Fatal("rebased onto a different version")
		}
	})
}

func TestPackagedResourcesMissingIsReported(t *testing.T) {
	m := packagedTodo(t)
	cache := t.TempDir()
	bin := writeSourcePackage(t, cache, m, packageA, []byte("x"), "001_init.sql")
	entry := filepath.Join(filepath.Dir(bin), "src", "mcp", "todo")

	if err := os.RemoveAll(filepath.Join(entry, "migrations")); err != nil {
		t.Fatal(err)
	}
	err := applyPackagedResourceEnv(31, m, bin, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "install 31") || !strings.Contains(err.Error(), "version 0.5.1") || !strings.Contains(err.Error(), filepath.Join(entry, "migrations")) {
		t.Fatalf("missing migrations: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(entry, "ui")); err != nil {
		t.Fatal(err)
	}
	err = applyPackagedResourceEnv(31, m, bin, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "UI directory missing at "+filepath.Join(entry, "ui")) {
		t.Fatalf("missing UI: %v", err)
	}

	// An app that advertises no UI and declares no migrations needs neither.
	headless := *m
	headless.Provides = sdk.Provides{}
	headless.DB = nil
	if err := applyPackagedResourceEnv(31, &headless, bin, map[string]string{}); err != nil {
		t.Fatal(err)
	}

	// The supervisor refuses to start it rather than reporting healthy.
	sup := NewLocalSupervisor(cache)
	if err := sup.Restart(31, m, unusedTCPPort(t), bin, nil); err == nil || !strings.Contains(err.Error(), "UI directory missing") {
		t.Fatalf("restart with missing UI: %v", err)
	}
	if sup.currentProc(31) != nil {
		t.Fatal("process started without its UI")
	}
}

func TestPackagedSourceRestartServesUIAndMigrations(t *testing.T) {
	fixture := fixtureBytes(t)
	for _, persisted := range []string{"obsolete", "nil"} {
		t.Run(persisted+" environment", func(t *testing.T) {
			m := packagedTodo(t)
			sup := NewLocalSupervisor(t.TempDir())
			defer sup.StopAll(time.Second)
			// Two packages share one version. Only the installed one may be used.
			writeSourcePackage(t, sup.cacheDir, m, packageA, fixture, "001_init.sql", "002_due_dates.sql")
			bin := writeSourcePackage(t, sup.cacheDir, m, packageB, fixture, "001_init.sql", "002_labels.sql")
			if _, err := os.Stat(filepath.Join(sup.cacheDir, m.Name, m.Version, "src")); !os.IsNotExist(err) {
				t.Fatal("test must not create the legacy src directory")
			}
			port := unusedTCPPort(t)
			for cycle := 0; cycle < 3; cycle++ {
				var env map[string]string
				if persisted == "obsolete" {
					// What boot resume used to pass for every packaged app.
					env = map[string]string{
						"TEST_SIDECAR_STATE":    "1",
						"APTEVA_UI_DIR":         filepath.Join(sup.cacheDir, m.Name, m.Version, "src", "mcp", "todo", "ui"),
						"APTEVA_MIGRATIONS_DIR": filepath.Join(sup.cacheDir, m.Name, m.Version, "src", "mcp", "todo", "migrations"),
					}
				} else {
					inheritSidecarState(t)
				}
				if err := sup.Restart(52808, m, port, bin, env); err != nil {
					t.Fatal(err)
				}
				label := fmt.Sprintf("cycle %d", cycle)
				assertPackagedWidget(t, port, packageB, label)
				st := sidecarStateAt(t, port, http.MethodGet, "")
				if !reflect.DeepEqual(st.Applied, []string{"001_init.sql", "002_labels.sql"}) {
					t.Fatalf("%s: migrations from the wrong package: %v (dir %s)", label, st.Applied, st.MigrationsDir)
				}
				if cycle == 0 {
					sidecarStateAt(t, port, http.MethodPost, "buy-milk")
				}
				if st := sidecarStateAt(t, port, http.MethodGet, ""); !reflect.DeepEqual(st.Records, []string{"buy-milk"}) {
					t.Fatalf("%s: records %v", label, st.Records)
				}
				if err := sup.Stop(52808); err != nil {
					t.Fatal(err)
				}
			}
			assertNoRebuild(t, sup.cacheDir, m, "packages", packageA, packageB)
			if db := filepath.Join(sup.cacheDir, m.Name, "data", "52808", "app.db"); func() bool { _, err := os.Stat(db); return err != nil }() {
				t.Fatal("persistent database moved")
			}
		})
	}
}

func packArtifact(t *testing.T, bin []byte) []byte {
	t.Helper()
	files := map[string][]byte{
		"bin":                                  bin,
		"src/mcp/todo/apteva.yaml":             []byte(packagedTodoManifest),
		"src/mcp/todo/ui/TodoListWidget.mjs":   []byte(todoModule(packageA)),
		"src/mcp/todo/ui/assets/todo.css":      []byte(".todo{color:" + packageA[:6] + "}"),
		"src/mcp/todo/migrations/001_init.sql": []byte("CREATE TABLE todos (id INTEGER);"),
	}
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for path, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	return out.Bytes()
}

func TestPackagedArtifactRestartServesUIWithoutDownload(t *testing.T) {
	m := packagedTodo(t)
	archive := filepath.Join(t.TempDir(), "todo.tar.gz")
	data := packArtifact(t, fixtureBytes(t))
	writePackageFile(t, archive, data, 0644)
	attachArtifact(m, data, archive)
	sup := NewLocalSupervisor(t.TempDir())
	defer sup.StopAll(time.Second)
	bin, err := sup.fetchAppArtifact(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Any download attempted by a restart now fails.
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	digest := filepath.Base(filepath.Dir(bin))
	port := unusedTCPPort(t)
	for cycle := 0; cycle < 2; cycle++ {
		env := map[string]string{"TEST_SIDECAR_STATE": "1", "APTEVA_UI_DIR": filepath.Join(sup.cacheDir, m.Name, m.Version, "src", "mcp", "todo", "ui")}
		if err := sup.Restart(41, m, port, bin, env); err != nil {
			t.Fatal(err)
		}
		if code, body := fetchSidecar(t, port, "/ui/TodoListWidget.mjs"); code != 200 || body != todoModule(packageA) {
			t.Fatalf("cycle %d: HTTP %d %q", cycle, code, body)
		}
		if st := sidecarStateAt(t, port, http.MethodGet, ""); !reflect.DeepEqual(st.Applied, []string{"001_init.sql"}) {
			t.Fatalf("cycle %d: migrations %v", cycle, st.Applied)
		}
		if err := sup.Stop(41); err != nil {
			t.Fatal(err)
		}
	}
	assertNoRebuild(t, sup.cacheDir, m, "artifacts", digest)
}

// seedPackagedInstall records a running packaged install the way the server
// persists it, with the state fixture enabled through the app config.
func seedPackagedInstall(t *testing.T, s *Server, m *sdk.Manifest, bin string) (id int64, port int, cfgEnc string) {
	t.Helper()
	id = seedRunningInstall(t, s, m.Name, "", *m, nil)
	raw, _ := json.Marshal(map[string]string{"TEST_SIDECAR_STATE": "1"})
	cfgEnc, err := Encrypt(s.secret, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	port = unusedTCPPort(t)
	mj, _ := json.Marshal(m)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET config_encrypted=?, local_pid=0, local_port=?, local_bin_path=?, version=?, manifest_json=? WHERE id=?`,
		cfgEnc, port, bin, m.Version, string(mj), id); err != nil {
		t.Fatal(err)
	}
	return id, port, cfgEnc
}

func TestBootResumeAndRespawnServePackagedUI(t *testing.T) {
	fixture := fixtureBytes(t)
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := packagedTodo(t)
	writeSourcePackage(t, s.localApps.cacheDir, m, packageA, fixture, "001_init.sql")
	bin := writeSourcePackage(t, s.localApps.cacheDir, m, packageB, fixture, "001_init.sql", "002_labels.sql")
	id, port, cfgEnc := seedPackagedInstall(t, s, m, bin)
	mj, _ := json.Marshal(m)

	resume := func(recorded string) {
		t.Helper()
		s.resumeOneLocalInstall(id, 0, int64(port), recorded, m.Name, "", cfgEnc, m.Version, string(mj))
		if s.localApps.currentProc(id) == nil {
			var msg string
			s.store.db.QueryRow(`SELECT COALESCE(error_message,'') FROM app_installs WHERE id=?`, id).Scan(&msg)
			t.Fatalf("resume did not start the app: %s", msg)
		}
	}
	// Boot resume, twice, as after two container restarts.
	for cycle := 0; cycle < 2; cycle++ {
		resume(bin)
		assertPackagedWidget(t, port, packageB, fmt.Sprintf("resume %d", cycle))
		if cycle == 0 {
			sidecarStateAt(t, port, http.MethodPost, "ship-it")
		}
		if err := s.localApps.Stop(id); err != nil {
			t.Fatal(err)
		}
	}

	// A cache root moved to another host path resolves to the same package.
	resume(filepath.Join("/old-host/apps", m.Name, m.Version, "packages", packageB, "bin"))
	var persisted string
	s.store.db.QueryRow(`SELECT local_bin_path FROM app_installs WHERE id=?`, id).Scan(&persisted)
	if persisted != bin {
		t.Fatalf("relocated path persisted as %s", persisted)
	}
	assertPackagedWidget(t, port, packageB, "relocated resume")

	// Explicit respawn after a configuration or binding change.
	for cycle := 0; cycle < 2; cycle++ {
		if err := s.RespawnLocalInstall(id); err != nil {
			t.Fatal(err)
		}
		assertPackagedWidget(t, port, packageB, fmt.Sprintf("respawn %d", cycle))
	}
	st := sidecarStateAt(t, port, http.MethodGet, "")
	if !reflect.DeepEqual(st.Applied, []string{"001_init.sql", "002_labels.sql"}) || !reflect.DeepEqual(st.Records, []string{"ship-it"}) {
		t.Fatalf("state after restarts: %+v", st)
	}

	// An incomplete package is reported and leaves the running process alone.
	pid := s.localApps.PID(id)
	uiDir := filepath.Join(filepath.Dir(bin), "src", "mcp", "todo", "ui")
	if err := os.Rename(uiDir, uiDir+".gone"); err != nil {
		t.Fatal(err)
	}
	err := s.RespawnLocalInstall(id)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("install %d", id)) || !strings.Contains(err.Error(), uiDir) {
		t.Fatalf("respawn with missing UI: %v", err)
	}
	if s.localApps.PID(id) != pid {
		t.Fatal("failed respawn stopped the running process")
	}
	if err := s.localApps.Stop(id); err != nil {
		t.Fatal(err)
	}
	s.resumeOneLocalInstall(id, 0, int64(port), bin, m.Name, "", cfgEnc, m.Version, string(mj))
	var status, msg string
	s.store.db.QueryRow(`SELECT status, COALESCE(error_message,'') FROM app_installs WHERE id=?`, id).Scan(&status, &msg)
	if status != "error" || !strings.Contains(msg, uiDir) || !strings.Contains(msg, "version 0.5.1") {
		t.Fatalf("resume with missing UI: status=%s msg=%q", status, msg)
	}
	assertNoRebuild(t, s.localApps.cacheDir, m, "packages", packageA, packageB)
}

func TestManagedRuntimeStartUsesPackagedResources(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	rm := s.appRuntimeManager()
	fixture := fixtureBytes(t)
	// Prepare the deployment through the binary path, then point it at a
	// source package as a managed source deployment records it.
	binary := sdk.Manifest{Name: "todo", Version: "0.5.1", Runtime: sdk.Runtime{Kind: "service", Binaries: map[string]string{localPlatform(): "https://example.invalid/never-fetch"}}}
	id := seedRunningInstall(t, s, binary.Name, "", binary, nil)
	s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, id)
	writePackageFile(t, filepath.Join(s.localApps.cacheDir, binary.Name, binary.Version, "bin"), fixture, 0755)
	prepareRuntimeFixture(t, rm, id, runtimePlacement{Type: "local"}, "prepare")
	d := activateRuntimeFixture(t, rm, id, "activate")
	b := localRuntimeBackend{m: rm, d: d}
	stopReq, _ := rm.request(d, runtimeOperation{ID: "stop"}, "stop")
	if out, err := b.Stop(stopReq); err != nil || !out.StoppedConfirmed {
		t.Fatalf("stop: %v", err)
	}

	m := packagedTodo(t)
	writeSourcePackage(t, s.localApps.cacheDir, m, packageA, fixture, "001_init.sql")
	bin := writeSourcePackage(t, s.localApps.cacheDir, m, packageB, fixture, "001_init.sql", "002_labels.sql")
	d, err := rm.loadDeployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.LocalBin = bin
	var env map[string]string
	raw, err := Decrypt(s.secret, d.EnvironmentEncrypted)
	if err != nil || json.Unmarshal([]byte(raw), &env) != nil {
		t.Fatalf("persisted environment: %v", err)
	}
	// The persisted environment carries the obsolete flat-cache paths.
	env["APTEVA_UI_DIR"] = filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "src", "mcp", "todo", "ui")
	env["APTEVA_MIGRATIONS_DIR"] = filepath.Join(s.localApps.cacheDir, m.Name, m.Version, "src", "mcp", "todo", "migrations")
	env["TEST_SIDECAR_STATE"] = "1"
	envJSON, _ := json.Marshal(env)
	if d.EnvironmentEncrypted, err = Encrypt(s.secret, string(envJSON)); err != nil {
		t.Fatal(err)
	}
	if err := rm.saveDeployment(d); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		req, err := rm.request(d, runtimeOperation{ID: fmt.Sprintf("start-%d", cycle)}, "start")
		if err != nil {
			t.Fatal(err)
		}
		req.Manifest = *m
		req.Environment = nil
		b = localRuntimeBackend{m: rm, d: d}
		if _, err := b.Start(req); err != nil {
			t.Fatalf("cycle %d start: %v", cycle, err)
		}
		// A managed start returns once spawned; readiness is the caller's
		// separate step.
		waitSidecarHealthy(t, d.LocalPort)
		assertPackagedWidget(t, d.LocalPort, packageB, fmt.Sprintf("managed start %d", cycle))
		if st := sidecarStateAt(t, d.LocalPort, http.MethodGet, ""); !reflect.DeepEqual(st.Applied, []string{"001_init.sql", "002_labels.sql"}) {
			t.Fatalf("cycle %d migrations: %v", cycle, st.Applied)
		}
		stopReq, _ := rm.request(d, runtimeOperation{ID: fmt.Sprintf("stop-%d", cycle)}, "stop")
		if out, err := b.Stop(stopReq); err != nil || !out.StoppedConfirmed {
			t.Fatalf("cycle %d stop: %v", cycle, err)
		}
	}
}

// A restarted container starts a fresh PID namespace and installs resume
// concurrently, so the pid recorded for one install can already belong to a
// sibling app this server just spawned. Resume must leave that process alone,
// and still replace a sidecar a previous server left running.
func TestBootResumeKillsOnlyItsOwnOrphan(t *testing.T) {
	fixture := fixtureBytes(t)
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	m := packagedTodo(t)
	bin := writeSourcePackage(t, s.localApps.cacheDir, m, packageA, fixture, "001_init.sql")
	id, port, cfgEnc := seedPackagedInstall(t, s, m, bin)
	mj, _ := json.Marshal(m)
	resume := func(pid int) {
		t.Helper()
		s.resumeOneLocalInstall(id, int64(pid), int64(port), bin, m.Name, "", cfgEnc, m.Version, string(mj))
		if s.localApps.currentProc(id) == nil {
			var msg string
			s.store.db.QueryRow(`SELECT COALESCE(error_message,'') FROM app_installs WHERE id=?`, id).Scan(&msg)
			t.Fatalf("resume did not start the app: %s", msg)
		}
	}

	// The recorded pid now belongs to an unrelated live process.
	sibling, _, siblingDone := processTestChild(t)
	resume(sibling.Process.Pid)
	select {
	case err := <-siblingDone:
		t.Fatalf("resume killed an unrelated process that reused the recorded pid: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	assertPackagedWidget(t, port, packageA, "recycled pid")
	if err := s.localApps.Stop(id); err != nil {
		t.Fatal(err)
	}

	// A sidecar a previous server left running still holds the port and is replaced.
	previous := NewLocalSupervisor(s.localApps.cacheDir)
	t.Cleanup(func() { previous.StopAll(time.Second) })
	inheritSidecarState(t)
	if err := previous.Restart(id, m, port, bin, nil); err != nil {
		t.Fatal(err)
	}
	orphan := previous.PID(id)
	resume(orphan)
	if got := s.localApps.PID(id); got == orphan || processAlive(orphan) {
		t.Fatalf("orphan pid %d survived resume (new pid %d)", orphan, got)
	}
	assertPackagedWidget(t, port, packageA, "orphan replaced")
	if err := s.localApps.Stop(id); err != nil {
		t.Fatal(err)
	}

	// An interrupted upgrade records its candidate's pid while the committed
	// binary is still the previous package; that candidate is also replaced.
	candidateBin := writeSourcePackage(t, s.localApps.cacheDir, m, packageB, fixture, "001_init.sql")
	if err := previous.Restart(id, m, port, candidateBin, nil); err != nil {
		t.Fatal(err)
	}
	candidate := previous.PID(id)
	resume(candidate)
	if got := s.localApps.PID(id); got == candidate || processAlive(candidate) {
		t.Fatalf("interrupted candidate pid %d survived resume (new pid %d)", candidate, got)
	}
	assertPackagedWidget(t, port, packageA, "candidate replaced")
}

// inheritSidecarState turns on the fixture's state endpoint through the
// server's own environment, which is how a nil restart env reaches a sidecar.
// Servers that start sidecars from an environment allowlist only forward it
// when the operator lists it in APTEVA_CHILD_ENV_ALLOW.
func inheritSidecarState(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_SIDECAR_STATE", "1")
	t.Setenv("APTEVA_CHILD_ENV_ALLOW", "TEST_SIDECAR_STATE")
}
