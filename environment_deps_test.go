package main

import (
	"os"
	"path/filepath"
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

	ordered, deps, err := wm.expandAppSourcesWithRequiredDeps(map[string]string{"media": mediaDir})
	if err != nil {
		t.Fatalf("expand required deps: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 2 || got[0] != "storage" || got[1] != "media" {
		t.Fatalf("required dependency order = %v, want [storage media]", got)
	}
	if got := deps["media"]; len(got) != 1 || got[0] != "storage" {
		t.Fatalf("media deps = %v, want [storage]", got)
	}

	ordered, deps, err = wm.expandAppSourcesWithRequiredDeps(map[string]string{
		"media": mediaDir,
		"jobs":  jobsDir,
	})
	if err != nil {
		t.Fatalf("expand explicit optional dep: %v", err)
	}
	if got := namesOfEnvironmentSources(ordered); len(got) != 3 || got[0] != "jobs" || got[1] != "storage" || got[2] != "media" {
		t.Fatalf("optional dependency order = %v, want [jobs storage media]", got)
	}
	if got := deps["media"]; len(got) != 2 || got[0] != "storage" || got[1] != "jobs" {
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

	if err := s.bindEnvironmentAppDependencies(environment, map[string][]string{"media": {"storage"}}); err != nil {
		t.Fatalf("bind deps: %v", err)
	}
	bindings := readBindings(t, s, mediaID)
	got, ok := asInt64(bindings["storage"])
	if !ok || got != storageID {
		t.Fatalf("storage binding = %#v, want install id %d", bindings["storage"], storageID)
	}
}

func namesOfEnvironmentSources(srcs []environmentAppSource) []string {
	out := make([]string, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, src.Name)
	}
	return out
}
