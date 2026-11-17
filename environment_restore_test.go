package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const restoreFixtureManifest = `schema: apteva-app/v1
name: restore-fixture
version: "0.1.0"
db:
  driver: sqlite
  path: app.db
runtime:
  kind: source
  port: 8080
  source:
    repo: example.invalid/restore-fixture
    ref: main
    entry: .
`

// restoreFixtureMain stores rows under its project id, its own install id and
// a link under its public URL, and maps all three in its restore hook. A
// fail-restore file in its data dir makes the hook fail; restore-count
// counts how often the hook ran.
const restoreFixtureMain = `package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	sdk "github.com/apteva/app-sdk"
)

const manifestYAML = ` + "`" + restoreFixtureManifest + "`" + `

var db *sql.DB

type app struct{}

func (app) Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest([]byte(manifestYAML))
	if err != nil {
		panic(err)
	}
	return *m
}

func (app) OnMount(c *sdk.AppCtx) error {
	db = c.AppDB()
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS items (id INTEGER PRIMARY KEY, project_id TEXT NOT NULL, owner_install_id INTEGER NOT NULL, link TEXT NOT NULL)")
	return err
}

func (app) OnUnmount(*sdk.AppCtx) error           { return nil }
func (app) MCPTools() []sdk.Tool                  { return nil }
func (app) Channels() []sdk.ChannelFactory        { return nil }
func (app) Workers() []sdk.Worker                 { return nil }
func (app) EventHandlers() []sdk.EventHandler     { return nil }

func (app) HTTPRoutes() []sdk.Route {
	return []sdk.Route{
		{Method: "POST", Pattern: "/items", NoAuth: true, Handler: func(w http.ResponseWriter, r *http.Request) {
			_, err := db.Exec("INSERT INTO items (project_id, owner_install_id, link) VALUES (?, ?, ?)",
				os.Getenv("APTEVA_PROJECT_ID"), os.Getenv("APTEVA_INSTALL_ID"), os.Getenv("APTEVA_PUBLIC_URL")+"/callback")
			if err != nil {
				http.Error(w, err.Error(), 500)
			}
		}},
		{Method: "GET", Pattern: "/items", NoAuth: true, Handler: func(w http.ResponseWriter, r *http.Request) {
			rows, err := db.Query("SELECT project_id, owner_install_id, link FROM items ORDER BY id")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var project, link string
				var owner int64
				rows.Scan(&project, &owner, &link)
				out = append(out, map[string]any{"project_id": project, "owner_install_id": owner, "link": link})
			}
			json.NewEncoder(w).Encode(out)
		}},
	}
}

func (app) OnRestore(ctx context.Context, c *sdk.AppCtx, tx *sql.Tx, rc sdk.RestoreContext) error {
	dir := c.DataDir()
	if _, err := os.Stat(filepath.Join(dir, "fail-restore")); err == nil {
		return errors.New("fixture refused restore")
	}
	n := 0
	if b, err := os.ReadFile(filepath.Join(dir, "restore-count")); err == nil {
		n, _ = strconv.Atoi(string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "restore-count"), []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return err
	}
	if _, err := sdk.RemapInstallIDColumns(ctx, tx, rc, "items.owner_install_id"); err != nil {
		return err
	}
	_, err := sdk.RewriteOriginColumns(ctx, tx, rc, "items.link")
	return err
}

func main() { sdk.Run(app{}) }
`

type restoreFixtureItem struct {
	ProjectID      string `json:"project_id"`
	OwnerInstallID int64  `json:"owner_install_id"`
	Link           string `json:"link"`
}

// writeRestoreFixture writes an SDK app next to a go.work and an app-sdk link
// to the SDK this server builds against, so genLocalGoWork builds the fixture
// with the same restore support.
func writeRestoreFixture(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/apteva/app-sdk").Output()
	sdkDir := strings.TrimSpace(string(out))
	if err != nil || sdkDir == "" {
		t.Skipf("app-sdk source not available: %v", err)
	}
	root := t.TempDir()
	if err := os.Symlink(sdkDir, filepath.Join(root, "app-sdk")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "restore-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":      "module example.com/restore-fixture\n\ngo 1.25\n",
		"main.go":     restoreFixtureMain,
		"apteva.yaml": restoreFixtureManifest,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func restoreFixtureItems(t *testing.T, env *Environment) []restoreFixtureItem {
	t.Helper()
	inst, ok := env.Install("restore-fixture")
	if !ok {
		t.Fatalf("environment %s has no restore-fixture install", env.ID)
	}
	resp, err := http.Get(inst.SidecarURL + "/items")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var items []restoreFixtureItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestEnvironmentSnapshotRestoreMapsAppData(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an app")
	}
	appDir := writeRestoreFixture(t)
	s := newTestServer(t)
	// The build downloads modules into a read-only cache under the supervisor
	// dir, which t.TempDir cannot remove.
	cache, err := os.MkdirTemp("", "restore-fixture-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("chmod", "-R", "u+w", cache).Run()
		_ = os.RemoveAll(cache)
	})
	s.localApps = NewLocalSupervisor(cache)
	s.installedApps = NewInstalledAppsRegistry()
	s.environments = NewEnvironmentManager(environmentDataRoot(t.TempDir()))
	s.environments.server = s
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	spec := func(id string) EnvironmentSpec {
		return EnvironmentSpec{
			ID:            id,
			ProjectID:     "source-project",
			GatewayURL:    "http://127.0.0.1:5280",
			CreatorUserID: 1,
			AppSrcDirs:    map[string]string{"restore-fixture": appDir},
			NetworkMode:   EdgeBlock,
			HealthBudget:  60 * time.Second,
		}
	}

	source, err := s.environments.Create(spec("rt_source"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(source.ID)
	sourceInst, _ := source.Install("restore-fixture")
	resp, err := http.Post(sourceInst.SidecarURL+"/items", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("seed row: %v %v", resp, err)
	}
	resp.Body.Close()

	man, err := s.captureEnvironmentSnapshot(source, CaptureSpec{ID: "snap-restore", ProjectID: "source-project"})
	if err != nil {
		t.Fatal(err)
	}
	if man.DataProjectID != "rt_source" || man.DataInstallIDs["restore-fixture"] != sourceInst.InstallID || man.PublicURL != s.runtimePlatformURL("rt_source") {
		t.Fatalf("snapshot ids %+v", man)
	}
	// The source keeps serving after the capture paused it.
	if items := restoreFixtureItems(t, source); len(items) != 1 {
		t.Fatalf("source after snapshot: %+v", items)
	}

	restored, err := s.environments.CreateFromSnapshot(spec("rt_restored"), "snap-restore")
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(restored.ID)
	restoredInst, _ := restored.Install("restore-fixture")
	want := restoreFixtureItem{ProjectID: "rt_restored", OwnerInstallID: restoredInst.InstallID, Link: s.runtimePlatformURL("rt_restored") + "/callback"}
	if items := restoreFixtureItems(t, restored); len(items) != 1 || items[0] != want {
		t.Fatalf("restored rows %+v, want %+v", items, want)
	}
	if items := restoreFixtureItems(t, source); items[0].ProjectID != "rt_source" {
		t.Fatalf("restore changed the source environment: %+v", items)
	}

	// A restart of the restored process keeps the context but must not run
	// the hook again.
	resume, err := s.localApps.pauseInstall(restoredInst.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if count, _ := os.ReadFile(filepath.Join(restoredInst.DataDir, "restore-count")); string(count) != "1" {
		t.Fatalf("restore hook ran %q times", count)
	}
	if items := restoreFixtureItems(t, restored); len(items) != 1 || items[0] != want {
		t.Fatalf("rows after restart %+v", items)
	}

	// A failing hook fails the create with the app's own error and leaves
	// nothing behind.
	failMarker := filepath.Join(s.environments.Snapshots().dir("snap-restore"), "apps", "restore-fixture", "fail-restore")
	if err := os.WriteFile(failMarker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.environments.CreateFromSnapshot(spec("rt_failed"), "snap-restore")
	if err == nil || !strings.Contains(err.Error(), "fixture refused restore") {
		t.Fatalf("create from failing restore: %v", err)
	}
	var installs int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE project_id='rt_failed'`).Scan(&installs); err != nil || installs != 0 {
		t.Fatalf("failed restore left %d install rows (%v)", installs, err)
	}
	if _, err := os.Stat(filepath.Join(s.environments.dataDir, "restores", "rt_failed")); !os.IsNotExist(err) {
		t.Fatalf("failed restore left its restore files: %v", err)
	}
	s.localApps.mu.Lock()
	running := len(s.localApps.procs)
	s.localApps.mu.Unlock()
	if running != 2 {
		t.Fatalf("%d sidecars running, want the source and the restored one", running)
	}
}

func TestRestorePlanSkipsUnmappedSnapshots(t *testing.T) {
	installs := map[string]*localInstall{"a": {InstallID: 9}}
	dirs := map[string]string{"a": "/snap/a"}
	if plan, err := restorePlan(&SnapshotManifest{ID: "old"}, "rt_new", "https://x/rt_new", t.TempDir(), dirs, installs); err != nil || plan != nil {
		t.Fatalf("snapshot without data ids got a plan: %v %v", plan, err)
	}
	man := &SnapshotManifest{ID: "s", DataProjectID: "rt_old", DataInstallIDs: map[string]int64{"a": 4, "gone": 5}, PublicURL: "https://x/rt_old"}
	plan, err := restorePlan(man, "rt_new", "https://x/rt_new", "/restores/rt_new", dirs, installs)
	if err != nil {
		t.Fatal(err)
	}
	rc := plan["a"].Context
	if rc.OldProjectID != "rt_old" || rc.NewProjectID != "rt_new" || len(rc.InstallIDs) != 1 || rc.InstallIDs[4] != 9 || rc.PublicBaseURLs["https://x/rt_old"] != "https://x/rt_new" {
		t.Fatalf("context %+v", rc)
	}
	if rc.ResultPath != "/restores/rt_new/a.result.json" || plan["a"].ContextPath != "/restores/rt_new/a.context.json" || rc.ID != fmt.Sprintf("s/rt_new/%d", 9) {
		t.Fatalf("paths %+v %s", rc, plan["a"].ContextPath)
	}
}

func TestRuntimePlatformURLForInstallBeforeRegistration(t *testing.T) {
	s := newTestServer(t)
	s.environments = NewEnvironmentManager(t.TempDir())
	res, err := s.store.db.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES('url-fixture','environment','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	insert := func(project, source string) int64 {
		res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,version,manifest_json,source) VALUES(?,?,'pending','1','{}',?)`, appID, project, source)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	if got := s.runtimePlatformURLForInstall(insert("rt_creating", "environment")); got != s.runtimePlatformURL("rt_creating") {
		t.Fatalf("install of an environment being created got %q", got)
	}
	if got := s.runtimePlatformURLForInstall(insert("real-project", "registry")); got != "" {
		t.Fatalf("project install got runtime URL %q", got)
	}
}
