package main

import (
	"encoding/json"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedRuntimeAutomaticAppAndTarget(t *testing.T) {
	for _, backendID := range []string{"runner-alpha", "future-compute"} {
		t.Run(backendID, func(t *testing.T) {
			s, m, f, p := packageTestSetup(t, backendID)
			packageTestCompiler(t)
			f.automatic = true
			providerManifest, _ := installManifest(s, p.Backend.InstallID)
			providerManifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: runtimeControlPrefix + "backends/" + backendID + "/"}}
			s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(providerManifest), p.Backend.InstallID)
			s.store.db.Exec(`DELETE FROM app_runtime_backends`)
			s.store.SetSetting("public_url", "https://automatic.example.test/")
			manifest := packageTestSource(t)
			manifest.Runtime.Package = &sdk.RuntimePackage{AppCapabilities: &sdk.RuntimeAppCapabilities{PassiveStart: true, Drain: true, Routing: "key"}}
			repo := strings.TrimPrefix(manifest.Runtime.Source.Repo, "file://")
			raw, _ := yaml.Marshal(manifest)
			if err := os.WriteFile(filepath.Join(repo, "apteva.yaml"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(repo, "apteva.runtime-package.json")); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "standard SDK release metadata"}, {"tag", "-f", "v1.0.0"}} {
				c := exec.Command("git", args...)
				c.Dir = repo
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			id := packageTestInstall(t, s, manifest)
			minimal := runtimePlacement{Type: "backend", Target: p.Target}
			op, err := m.begin(id, "upgrade", "automatic-deploy", minimal, &manifest)
			if err != nil {
				t.Fatal(err)
			}
			saved := waitPackageOperation(t, m, op)
			if saved.Status != "succeeded" {
				t.Fatalf("%s %s", saved.Phase, saved.Error)
			}
			d, err := m.active(id)
			if err != nil {
				t.Fatal(err)
			}
			if d.TargetInfo == nil || d.Placement.GatewayURL != "https://automatic.example.test" || d.Package == nil || d.Package.Metadata.Platform != "linux-amd64" {
				t.Fatalf("incomplete automation: %+v", d)
			}
			found := false
			for _, file := range d.Package.Metadata.Files {
				if file.Path == "native/library.txt" {
					found = true
				}
			}
			if !found {
				t.Fatal("standard native runtime directory missing")
			}
			duplicate, err := m.begin(id, "upgrade", "automatic-deploy", minimal, &manifest)
			if err != nil || duplicate.ID != op.ID {
				t.Fatal("duplicate did not reuse deployment")
			}
		})
	}
}
func TestManagedRuntimeTargetRequirements(t *testing.T) {
	info := &sdk.RuntimeTargetInfo{Platform: "linux-amd64", ABI: "gnu", SystemLibraries: []string{"libc.so.6"}}
	p := &runtimePreparedPackage{Metadata: runtimePackageMetadata{Platform: "linux-amd64"}, ABI: "gnu", SystemLibraries: []string{"libc.so.6"}}
	if err := verifyRuntimeTargetRequirements(info, p); err != nil {
		t.Fatal(err)
	}
	p.SystemLibraries = append(p.SystemLibraries, "libmissing.so")
	if err := verifyRuntimeTargetRequirements(info, p); err == nil {
		t.Fatal("accepted missing dependency")
	}
	p.SystemLibraries = nil
	p.ABI = "musl"
	if err := verifyRuntimeTargetRequirements(info, p); err == nil {
		t.Fatal("accepted incompatible ABI")
	}
}

func TestManagedRuntimeRedeployDeletedInstallationPreservesPlacementAndPackage(t *testing.T) {
	s, m, f, p := packageTestSetup(t, "future-compute")
	packageTestCompiler(t)
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	original, err := m.begin(id, "upgrade", "original", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, original); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	first, _ := m.active(id)
	remove, err := m.begin(id, "delete", "delete-test", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, remove); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	if _, err = m.active(id); err == nil {
		t.Fatal("deleted execution remained active")
	}
	var persisted string
	s.store.db.QueryRow(`SELECT runtime_placement_json FROM app_installs WHERE id=?`, id).Scan(&persisted)
	if !strings.Contains(persisted, "future-compute") {
		t.Fatal("delete lost placement")
	}
	next, err := m.begin(id, "upgrade", "redeploy", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, next); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	second, _ := m.active(id)
	if second.Package.Metadata.ID != first.Package.Metadata.ID || second.Execution.StateRef != first.Execution.StateRef || second.Generation <= first.Generation {
		t.Fatal("redeployment changed package/data identity")
	}
	f.mu.Lock()
	count := len(f.metadata)
	f.mu.Unlock()
	if count != 1 {
		t.Fatal("package was not reused")
	}
}

func TestManagedRuntimeHTTPRestartKeepsBackendPlacement(t *testing.T) {
	s, m, _, p := packageTestSetup(t, "future-compute")
	packageTestCompiler(t)
	manifest := packageTestSource(t)
	id := packageTestInstall(t, s, manifest)
	op, err := m.begin(id, "upgrade", "initial", p, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPackageOperation(t, m, op); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/apps/installs/%d/deployment/restart", id), nil)
	req.Header.Set("X-User-ID", "1")
	req.Header.Set("Idempotency-Key", "restart-same-target")
	w := httptest.NewRecorder()
	s.handleAppDeployment(w, req, id)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var restart runtimeOperation
	if err = json.Unmarshal(w.Body.Bytes(), &restart); err != nil {
		t.Fatal(err)
	}
	d, err := m.loadDeployment(restart.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Placement.Backend != p.Backend || d.Placement.Target != p.Target || d.Placement.local() {
		t.Fatal("restart changed execution placement")
	}
	if got := waitPackageOperation(t, m, restart); got.Status != "succeeded" {
		t.Fatal(got.Error)
	}
}
