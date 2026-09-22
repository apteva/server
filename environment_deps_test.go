package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func writeEnvironmentTestManifest(t *testing.T, root, name, requires string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	body := "schema: apteva-app/v1\nname: " + name + "\nversion: \"0.1.0\"\n"
	if requires != "" {
		body += requires
	}
	if err := os.WriteFile(filepath.Join(dir, "apteva.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write manifest %s: %v", name, err)
	}
	return dir
}

func writeEnvironmentVersionedManifest(t *testing.T, root, name, version, requires string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	body := "schema: apteva-app/v1\nname: " + name + "\nversion: \"" + version + "\"\n"
	if requires != "" {
		body += requires
	}
	if err := os.WriteFile(filepath.Join(dir, "apteva.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write manifest %s: %v", name, err)
	}
	return dir
}

func TestEnvironmentExpandAppSourcesWithRequiredDeps(t *testing.T) {
	root := t.TempDir()
	storageDir := writeEnvironmentTestManifest(t, root, "storage", "")
	jobsDir := writeEnvironmentTestManifest(t, root, "jobs", "")
	mediaDir := writeEnvironmentTestManifest(t, root, "media", `requires:
  apps:
    - name: storage
    - name: jobs
      optional: true
`)

	wm := NewEnvironmentManager(environmentDataRoot(root))
	wm.ResolveSource = func(name string) (string, error) {
		switch name {
		case "storage":
			return storageDir, nil
		case "jobs":
			return jobsDir, nil
		default:
			return "", os.ErrNotExist
		}
	}

	ordered, deps, err := wm.expandAppSourcesWithRequiredDeps("proj-1", map[string]string{"media": mediaDir})
	if err != nil {
		t.Fatalf("expand required deps: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 2 || got[0] != "storage" || got[1] != "media" {
		t.Fatalf("required dependency order = %v, want [storage media]", got)
	}
	if got := namesOfEnvironmentDependencies(deps["media"]); len(got) != 1 || got[0] != "storage" {
		t.Fatalf("media deps = %v, want [storage]", got)
	}

	ordered, deps, err = wm.expandAppSourcesWithRequiredDeps("proj-1", map[string]string{
		"media": mediaDir,
		"jobs":  jobsDir,
	})
	if err != nil {
		t.Fatalf("expand explicit optional dep: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 3 || got[0] != "jobs" || got[1] != "storage" || got[2] != "media" {
		t.Fatalf("optional dependency order = %v, want [jobs storage media]", got)
	}
	if got := namesOfEnvironmentDependencies(deps["media"]); len(got) != 2 || got[0] != "storage" || got[1] != "jobs" {
		t.Fatalf("media deps with optional = %v, want [storage jobs]", got)
	}
}

func TestBindEnvironmentAppDependencies(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()

	storageID := seedRunningInstall(t, s, "storage", "environment-1", sdk.Manifest{Name: "storage"}, nil)
	mediaID := seedRunningInstall(t, s, "media", "environment-1", sdk.Manifest{Name: "media"}, nil)
	environment := &Environment{
		installs: map[string]*localInstall{
			"storage": {InstallID: storageID, AppName: "storage", ProjectID: "environment-1"},
			"media":   {InstallID: mediaID, AppName: "media", ProjectID: "environment-1"},
		},
	}

	if err := s.bindEnvironmentAppDependencies(environment, map[string][]environmentAppDependency{"media": {
		{BindingKey: "storage", TargetName: "storage"},
		{BindingKey: "files", TargetName: "storage"},
	}}); err != nil {
		t.Fatalf("bind deps: %v", err)
	}
	bindings := readBindings(t, s, mediaID)
	got, ok := asInt64(bindings["storage"])
	if !ok || got != storageID {
		t.Fatalf("storage binding = %#v, want install id %d", bindings["storage"], storageID)
	}
	got, ok = asInt64(bindings["files"])
	if !ok || got != storageID {
		t.Fatalf("role binding = %#v, want install id %d", bindings["files"], storageID)
	}
}

func TestEnvironmentDependencyBindingValuesPreservesMultipleRoleTargets(t *testing.T) {
	environment := &Environment{installs: map[string]*localInstall{
		"primary":   {InstallID: 41, AppName: "primary"},
		"secondary": {InstallID: 42, AppName: "secondary"},
	}}
	bindings, err := environmentDependencyBindingValues(environment, []environmentAppDependency{
		{BindingKey: "providers", TargetName: "primary", Multiple: true},
		{BindingKey: "providers", TargetName: "secondary", Multiple: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, defaultID := appBindingIDs(bindings["providers"])
	if len(ids) != 2 || ids[0] != 41 || ids[1] != 42 || defaultID != 41 {
		t.Fatalf("multiple binding = %#v (ids=%v default=%d)", bindings["providers"], ids, defaultID)
	}
}

func TestEnvironmentDependencyGraphTransitiveSharedAndCycle(t *testing.T) {
	root := t.TempDir()
	sharedDir := writeEnvironmentTestManifest(t, root, "shared", "")
	leftDir := writeEnvironmentTestManifest(t, root, "left", "requires:\n  apps:\n    - name: shared\n")
	rightDir := writeEnvironmentTestManifest(t, root, "right", "requires:\n  apps:\n    - name: shared\n")
	rootDir := writeEnvironmentTestManifest(t, root, "root", "requires:\n  apps:\n    - name: left\n    - name: right\n")

	wm := NewEnvironmentManager(environmentDataRoot(root))
	dirs := map[string]string{"shared": sharedDir, "left": leftDir, "right": rightDir, "root": rootDir}
	wm.ResolveEnvironmentSource = func(_, name, _ string) (string, error) { return dirs[name], nil }
	ordered, _, err := wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"root": rootDir})
	if err != nil {
		t.Fatalf("expand graph: %v", err)
	}
	got := namesOfEnvironmentSources(ordered)
	if len(got) != 4 || got[0] != "shared" || got[3] != "root" {
		t.Fatalf("topological order/dedup = %v", got)
	}
	countShared := 0
	for _, name := range got {
		if name == "shared" {
			countShared++
		}
	}
	if countShared != 1 {
		t.Fatalf("shared dependency count = %d, want 1", countShared)
	}

	cycleADir := writeEnvironmentTestManifest(t, root, "cycle-a", "requires:\n  apps:\n    - name: cycle-b\n")
	cycleBDir := writeEnvironmentTestManifest(t, root, "cycle-b", "requires:\n  apps:\n    - name: cycle-a\n")
	dirs["cycle-a"], dirs["cycle-b"] = cycleADir, cycleBDir
	_, _, err = wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"cycle-a": cycleADir})
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestEnvironmentDependencyGraphEnforcesVersions(t *testing.T) {
	root := t.TempDir()
	depDir := writeEnvironmentVersionedManifest(t, root, "versioned-dep", "1.4.0", "")
	parentDir := writeEnvironmentVersionedManifest(t, root, "versioned-parent", "1.0.0", "requires:\n  apps:\n    - name: versioned-dep\n      version: \">=2.0.0\"\n")
	wm := NewEnvironmentManager(environmentDataRoot(root))
	wm.ResolveEnvironmentSource = func(_, name, _ string) (string, error) {
		if name == "versioned-dep" {
			return depDir, nil
		}
		return "", os.ErrNotExist
	}
	_, _, err := wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"versioned-parent": parentDir})
	if err == nil || !strings.Contains(err.Error(), "versioned-parent") || !strings.Contains(err.Error(), "versioned-dep") || !strings.Contains(err.Error(), ">=2.0.0") {
		t.Fatalf("compatibility error = %v", err)
	}
}

func TestEnvironmentRequiredAppIntegrationAndOptionalOptIn(t *testing.T) {
	root := t.TempDir()
	jobsDir := writeEnvironmentTestManifest(t, root, "jobs", "")
	storageDir := writeEnvironmentTestManifest(t, root, "storage", "")
	consumerDir := writeEnvironmentTestManifest(t, root, "consumer", `requires:
  integrations:
    - role: scheduler
      kind: app
      compatible_app_names: [jobs]
      required: true
    - role: files
      kind: app
      compatible_app_names: [storage]
      required: false
`)
	wm := NewEnvironmentManager(environmentDataRoot(root))
	dirs := map[string]string{"jobs": jobsDir, "storage": storageDir}
	wm.ResolveEnvironmentSource = func(_, name, _ string) (string, error) { return dirs[name], nil }

	ordered, deps, err := wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"consumer": consumerDir})
	if err != nil {
		t.Fatalf("required role: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 2 || got[0] != "jobs" || got[1] != "consumer" {
		t.Fatalf("required role order = %v", got)
	}
	if got := deps["consumer"]; len(got) != 1 || got[0].BindingKey != "scheduler" || got[0].TargetName != "jobs" {
		t.Fatalf("required role edge = %+v", got)
	}

	ordered, deps, err = wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"consumer": consumerDir, "storage": storageDir})
	if err != nil {
		t.Fatalf("optional role opt-in: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 3 {
		t.Fatalf("optional role source count = %v", got)
	}
	foundFiles := false
	for _, edge := range deps["consumer"] {
		foundFiles = foundFiles || edge.BindingKey == "files" && edge.TargetName == "storage"
	}
	if !foundFiles {
		t.Fatalf("optional role was not bound: %+v", deps["consumer"])
	}
}

func TestEnvironmentCampaignsDependencyGraph(t *testing.T) {
	root := t.TempDir()
	messagingDir := writeEnvironmentVersionedManifest(t, root, "messaging", "0.13.48", "")
	crmDir := writeEnvironmentVersionedManifest(t, root, "crm", "0.9.1", "")
	jobsDir := writeEnvironmentVersionedManifest(t, root, "jobs", "0.1.13", "")
	campaignsDir := writeEnvironmentVersionedManifest(t, root, "campaigns", "0.2.16", `requires:
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
	wm := NewEnvironmentManager(environmentDataRoot(root))
	dirs := map[string]string{"campaigns": campaignsDir, "messaging": messagingDir, "crm": crmDir, "jobs": jobsDir}
	wm.ResolveEnvironmentSource = func(_, name, _ string) (string, error) { return dirs[name], nil }
	ordered, deps, err := wm.expandAppSourcesWithRequiredDeps("proj", map[string]string{"campaigns": campaignsDir})
	if err != nil {
		t.Fatalf("campaigns graph: %v", err)
	}
	got := namesOfEnvironmentSources(ordered)
	if len(got) != 4 || got[len(got)-1] != "campaigns" {
		t.Fatalf("campaigns install order = %v", got)
	}
	wantEdges := map[string]string{"messaging": "messaging", "crm": "crm", "jobs": "jobs"}
	for _, edge := range deps["campaigns"] {
		if wantEdges[edge.BindingKey] == edge.TargetName {
			delete(wantEdges, edge.BindingKey)
		}
	}
	if len(wantEdges) != 0 {
		t.Fatalf("missing campaigns bindings %v from %+v", wantEdges, deps["campaigns"])
	}
}

func TestEnvironmentSourceResolverCacheAndRegistryFallback(t *testing.T) {
	s := newTestServer(t)
	cache := t.TempDir()
	s.localApps = NewLocalSupervisor(cache)
	cachedDir := filepath.Join(cache, "cached-only", "1.2.0", "src", "mcp", "cached-only")
	writeEnvironmentVersionedManifest(t, filepath.Dir(cachedDir), "cached-only", "1.2.0", "")
	dir, err := s.resolveEnvironmentAppSource("proj", "cached-only", ">=1.0.0")
	if err != nil || dir != cachedDir {
		t.Fatalf("cache resolution = %q, %v; want %q", dir, err, cachedDir)
	}
	customEntryDir := filepath.Join(cache, "custom-entry", "3.1.0", "src", "components", "runtime")
	if err := os.MkdirAll(customEntryDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customEntryDir, "apteva.yaml"), []byte("schema: apteva-app/v1\nname: custom-entry\nversion: \"3.1.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dir, err = s.resolveEnvironmentAppSource("proj", "custom-entry", ">=3.0.0")
	if err != nil || dir != customEntryDir {
		t.Fatalf("custom-entry cache resolution = %q, %v; want %q", dir, err, customEntryDir)
	}

	registryDir := writeEnvironmentVersionedManifest(t, t.TempDir(), "registry-only", "2.0.0", "")
	registryCalls := 0
	s.environmentRegistrySource = func(name, constraint string) (string, error) {
		registryCalls++
		if name != "registry-only" || constraint != ">=2.0.0" {
			t.Fatalf("registry resolver got %q %q", name, constraint)
		}
		return registryDir, nil
	}
	dir, err = s.resolveEnvironmentAppSource("proj", "registry-only", ">=2.0.0")
	if err != nil || dir != registryDir || registryCalls != 1 {
		t.Fatalf("registry resolution = %q calls=%d err=%v", dir, registryCalls, err)
	}
}

func TestEnvironmentSourceResolverPrefersProjectThenGlobalInstall(t *testing.T) {
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	root := t.TempDir()
	type sourceInstall struct {
		project string
		version string
	}
	sources := []sourceInstall{
		{project: "source-project", version: "1.0.0"},
		{project: "", version: "2.0.0"},
		{project: "another-project", version: "9.0.0"},
	}
	manifestJSON, _ := json.Marshal(sdk.Manifest{Name: "routed-app", Version: "1.0.0"})
	res, err := s.store.db.Exec(`INSERT INTO apps(name,source,manifest_json) VALUES('routed-app','registry',?)`, string(manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	wantDirs := map[string]string{}
	for _, source := range sources {
		scope := source.project
		if scope == "" {
			scope = "global"
		}
		base := filepath.Join(root, scope, source.version)
		dir := filepath.Join(base, "src")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := sdk.Manifest{Name: "routed-app", Version: source.version}
		raw, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(dir, "apteva.yaml"), []byte("schema: apteva-app/v1\nname: routed-app\nversion: \""+source.version+"\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,version,manifest_json,local_bin_path) VALUES(?,?,'running',?,?,?)`, appID, source.project, source.version, string(raw), filepath.Join(base, "bin")); err != nil {
			t.Fatal(err)
		}
		wantDirs[source.project] = dir
	}

	dir, err := s.resolveEnvironmentAppSource("source-project", "routed-app", "")
	if err != nil || dir != wantDirs["source-project"] {
		t.Fatalf("project install resolution = %q, %v; want %q", dir, err, wantDirs["source-project"])
	}
	dir, err = s.resolveEnvironmentAppSource("source-project", "routed-app", ">=2.0.0")
	if err != nil || dir != wantDirs[""] {
		t.Fatalf("global fallback resolution = %q, %v; want %q", dir, err, wantDirs[""])
	}
}

func TestEnvironmentSourceResolverDownloadsRegistryPackage(t *testing.T) {
	manifest, bundle := artifactFixture(t, []byte("environment-artifact-binary"), nil)
	artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	defer artifactServer.Close()
	attachArtifact(manifest, bundle, artifactServer.URL)
	artifact := manifest.Runtime.Artifacts[localPlatform()]
	manifestBody := []byte(artifactFixtureManifest + fmt.Sprintf("  artifacts:\n    %s:\n      url: %q\n      sha256: %q\n", localPlatform(), artifact.URL, artifact.SHA256))
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifestBody)
	}))
	defer manifestServer.Close()
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"apps":[{"name":"artifact-test","version":"1.2.3","manifest_url":%q}]}`, manifestServer.URL)
	}))
	defer registryServer.Close()
	t.Setenv("APTEVA_APP_REGISTRY_URL", registryServer.URL)

	registryCacheMu.Lock()
	previousRegistry := registryCache
	registryCache = registryCacheEntry{}
	registryCacheMu.Unlock()
	t.Cleanup(func() {
		registryCacheMu.Lock()
		registryCache = previousRegistry
		registryCacheMu.Unlock()
	})

	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	dir, err := s.resolveEnvironmentAppSource("proj", "artifact-test", ">=1.2.0")
	if err != nil {
		t.Fatalf("registry package resolution: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apteva.yaml")); err != nil {
		t.Fatalf("resolved registry source %q: %v", dir, err)
	}
	if !strings.Contains(dir, filepath.Join("artifact-test", "1.2.3", "artifacts")) {
		t.Fatalf("resolved path %q is not the verified artifact cache", dir)
	}
}

func namesOfEnvironmentSources(srcs []environmentAppSource) []string {
	out := make([]string, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, src.Name)
	}
	return out
}

func namesOfEnvironmentDependencies(deps []environmentAppDependency) []string {
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		out = append(out, dep.TargetName)
	}
	return out
}
