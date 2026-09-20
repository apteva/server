package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const environmentFixtureMain = `package main

import (
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("APTEVA_APP_PORT"), nil)
}
`

func writeEnvironmentRunnableFixture(t *testing.T, root, name, version, requires string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/environment-"+name+"\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(environmentFixtureMain), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: apteva-app/v1\nname: " + name + "\nversion: \"" + version + "\"\n" + requires + `runtime:
  kind: source
  port: 8080
  source:
    repo: example.invalid/environment-fixture
    ref: main
    entry: .
`
	if err := os.WriteFile(filepath.Join(dir, "apteva.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCampaignsEnvironmentInstallsBindsAndTearsDownDependencyGraph(t *testing.T) {
	root := t.TempDir()

	dirs := map[string]string{}
	dirs["messaging"] = writeEnvironmentRunnableFixture(t, root, "messaging", "0.13.48", "")
	dirs["crm"] = writeEnvironmentRunnableFixture(t, root, "crm", "0.9.1", "")
	dirs["jobs"] = writeEnvironmentRunnableFixture(t, root, "jobs", "0.1.13", "")
	dirs["campaigns"] = writeEnvironmentRunnableFixture(t, root, "campaigns", "0.2.16", `requires:
  apps:
    - name: messaging
      version: ">=0.13.46"
    - name: crm
      version: ">=0.9.1"
  integrations:
    - role: crm
      kind: app
      compatible_app_names: [crm]
      required: true
    - role: jobs
      kind: app
      compatible_app_names: [jobs]
      required: true
`)
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.environments = NewEnvironmentManager(environmentDataRoot(t.TempDir()))
	s.environments.server = s
	s.environments.ResolveEnvironmentSource = func(_, name, _ string) (string, error) { return dirs[name], nil }
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })

	environment, err := s.environments.Create(EnvironmentSpec{
		ID:              "campaigns-environment",
		ProjectID:       "source-project",
		GatewayURL:      "http://127.0.0.1:5280",
		AppSrcDirs:      map[string]string{"campaigns": dirs["campaigns"]},
		NetworkMode:     EdgeBlock,
		IntegrationMode: IntegrationModeMock,
		HealthBudget:    10 * time.Second,
	})
	if err != nil {
		t.Fatalf("create campaigns environment: %v", err)
	}
	if got := environment.InstallNames(); len(got) != 4 {
		t.Fatalf("installed apps = %v, want campaigns, messaging, crm, jobs", got)
	}
	campaigns, ok := environment.Install("campaigns")
	if !ok {
		t.Fatal("campaigns install missing")
	}
	bindings := readBindings(t, s, campaigns.InstallID)
	for _, name := range []string{"messaging", "crm", "jobs"} {
		dependency, ok := environment.Install(name)
		if !ok {
			t.Fatalf("dependency %s missing", name)
		}
		got, ok := asInt64(bindings[name])
		if !ok || got != dependency.InstallID {
			t.Fatalf("binding %s = %#v, want %d", name, bindings[name], dependency.InstallID)
		}
	}
	var sourceProjectRows int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE project_id='source-project'`).Scan(&sourceProjectRows); err != nil || sourceProjectRows != 0 {
		t.Fatalf("source project changed: rows=%d err=%v", sourceProjectRows, err)
	}

	s.environments.Destroy(environment.ID)
	var temporaryRows int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE project_id='campaigns-environment'`).Scan(&temporaryRows); err != nil || temporaryRows != 0 {
		t.Fatalf("temporary installs remain: rows=%d err=%v", temporaryRows, err)
	}
	var temporaryCatalogRows int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM apps WHERE source='environment'`).Scan(&temporaryCatalogRows); err != nil || temporaryCatalogRows != 0 {
		t.Fatalf("temporary app catalog rows remain: rows=%d err=%v", temporaryCatalogRows, err)
	}
}

func TestEnvironmentDependencyInstallFailureRollsBackStartedDependencies(t *testing.T) {
	root := t.TempDir()
	dependencyDir := writeEnvironmentRunnableFixture(t, root, "rollback-dependency", "1.0.0", "")
	consumerDir := writeEnvironmentRunnableFixture(t, root, "rollback-consumer", "1.0.0", `requires:
  apps:
    - name: rollback-dependency
`)
	if err := os.WriteFile(filepath.Join(consumerDir, "main.go"), []byte("package main\nfunc main( {\n"), 0644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.environments = NewEnvironmentManager(environmentDataRoot(t.TempDir()))
	s.environments.server = s
	s.environments.ResolveEnvironmentSource = func(_, name, _ string) (string, error) {
		if name == "rollback-dependency" {
			return dependencyDir, nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })

	_, err := s.environments.Create(EnvironmentSpec{
		ID:           "rollback-environment",
		ProjectID:    "source-project",
		GatewayURL:   "http://127.0.0.1:5280",
		AppSrcDirs:   map[string]string{"rollback-consumer": consumerDir},
		NetworkMode:  EdgeBlock,
		HealthBudget: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("environment unexpectedly succeeded with an invalid consumer")
	}
	var installs int
	if countErr := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE project_id='rollback-environment'`).Scan(&installs); countErr != nil || installs != 0 {
		t.Fatalf("partial install rows remain: count=%d err=%v", installs, countErr)
	}
	s.localApps.mu.Lock()
	processes := len(s.localApps.procs)
	s.localApps.mu.Unlock()
	if processes != 0 {
		t.Fatalf("partial app processes remain: %d", processes)
	}
}
