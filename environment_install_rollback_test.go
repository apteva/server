package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalInstallDataDirUsesCanonicalAppRoot(t *testing.T) {
	cache := t.TempDir()
	sup := NewLocalSupervisor(cache)
	want := filepath.Join(cache, "computer", "data", "42")
	if got := localInstallDataDir(sup, "computer", 42); got != want {
		t.Fatalf("localInstallDataDir() = %q, want %q", got, want)
	}
}

func TestInstallLocalSourceRollsBackAfterPostSpawnFailure(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "fixture")
	sdkDir := filepath.Join(root, "app-sdk")
	for _, dir := range []string{appDir, sdkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.22\n\nuse (\n\t./fixture\n\t./app-sdk\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdkDir, "go.mod"), []byte("module github.com/apteva/app-sdk\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "go.mod"), []byte("module example.com/environment-rollback-fixture\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainSource := `package main

import (
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("APTEVA_APP_PORT"), nil)
}
`
	if err := os.WriteFile(filepath.Join(appDir, "main.go"), []byte(mainSource), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `schema: apteva-app/v1
name: environment-rollback-fixture
display_name: Environment rollback fixture
version: 0.1.0
runtime:
  kind: source
  port: 8080
  source:
    repo: example.invalid/fixture
    ref: main
    entry: .
`
	if err := os.WriteFile(filepath.Join(appDir, "apteva.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	s.installedApps = NewInstalledAppsRegistry()
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })

	missingRestore := filepath.Join(root, "missing-restore")
	if _, err := s.installLocalSource(appDir, "environment-test", 1, nil, missingRestore, nil, nil); err == nil {
		t.Fatal("installLocalSource unexpectedly succeeded with a missing restore directory")
	}

	for table, query := range map[string]string{
		"install": `SELECT COUNT(*) FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name='environment-rollback-fixture'`,
		"app":     `SELECT COUNT(*) FROM apps WHERE name='environment-rollback-fixture'`,
	} {
		var count int
		if err := s.store.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != 0 {
			t.Errorf("rollback left %d %s row(s)", count, table)
		}
	}

	s.localApps.mu.Lock()
	processCount := len(s.localApps.procs)
	s.localApps.mu.Unlock()
	if processCount != 0 {
		t.Errorf("rollback left %d tracked sidecar process(es)", processCount)
	}
	s.localApps.portMu.Lock()
	portCount := len(s.localApps.fixedPorts)
	s.localApps.portMu.Unlock()
	if portCount != 0 {
		t.Errorf("rollback left %d fixed-port reservation(s)", portCount)
	}

	dataRoot := filepath.Join(cache, "environment-rollback-fixture", "data")
	if entries, err := os.ReadDir(dataRoot); err == nil && len(entries) != 0 {
		t.Errorf("rollback left data directories under %s: %v", dataRoot, entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(fmt.Errorf("inspect rollback data root: %w", err))
	}
}

func TestDeleteEnvironmentInstallPreservesExistingCatalogRow(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	res, err := s.store.db.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES('catalog-app','registry','{"name":"catalog-app","version":"1.0.0"}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,version,manifest_json) VALUES(?,'environment-test','running','1.0.0','{"name":"catalog-app","version":"1.0.0"}')`, appID)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := res.LastInsertId()

	s.deleteEnvironmentInstall(installID)
	var source string
	if err := s.store.db.QueryRow(`SELECT source FROM apps WHERE id=?`, appID).Scan(&source); err != nil {
		t.Fatalf("source catalog row was removed: %v", err)
	}
	if source != "registry" {
		t.Fatalf("source catalog metadata changed to %q", source)
	}
}
