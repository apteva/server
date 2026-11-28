package main

import (
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func officialManifest(name string, dynamic bool) sdk.Manifest {
	return sdk.Manifest{
		Name: name,
		Runtime: sdk.Runtime{
			Kind:   "source",
			Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps", Ref: "main", Entry: "mcp/" + name},
		},
		Requires: sdk.Requires{DynamicAppCalls: dynamic},
	}
}

func thirdPartyManifest(name string, dynamic bool) sdk.Manifest {
	return sdk.Manifest{
		Name: name,
		Runtime: sdk.Runtime{
			Kind:   "source",
			Source: &sdk.SourceSpec{Repo: "github.com/evil/x", Ref: "main", Entry: "mcp/" + name},
		},
		Requires: sdk.Requires{DynamicAppCalls: dynamic},
	}
}

// TestResolveDynamicTarget_ThirdPartyWithFlag_StaysBlocked: a non-
// apteva caller can't self-grant by flipping the manifest flag.
func TestResolveDynamicTarget_ThirdPartyWithFlag_StaysBlocked(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "evil-fn", "proj-1", thirdPartyManifest("evil-fn", true), nil)
	seedRunningInstall(t, s, "tables", "proj-1", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if ok || id != 0 {
		t.Fatalf("expected reject, got id=%d ok=%v", id, ok)
	}
	if !strings.Contains(msg, "app not bound") {
		t.Errorf("expected 'app not bound', got %q", msg)
	}
}

// TestResolveDynamicTarget_OfficialNoFlag_StaysBlocked: an official
// app that hasn't declared the flag remains on the strict gate.
func TestResolveDynamicTarget_OfficialNoFlag_StaysBlocked(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "proj-1", officialManifest("functions", false), nil)
	seedRunningInstall(t, s, "tables", "proj-1", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if ok || id != 0 {
		t.Fatalf("expected reject (no flag), got id=%d ok=%v", id, ok)
	}
	if !strings.Contains(msg, "app not bound") {
		t.Errorf("expected 'app not bound', got %q", msg)
	}
}

// TestResolveDynamicTarget_OfficialWithFlag_SameProject: happy path.
func TestResolveDynamicTarget_OfficialWithFlag_SameProject(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "proj-1", officialManifest("functions", true), nil)
	target := seedRunningInstall(t, s, "tables", "proj-1", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if !ok {
		t.Fatalf("expected resolve, got msg=%q", msg)
	}
	if id != target {
		t.Errorf("id = %d, want %d", id, target)
	}
}

// TestResolveDynamicTarget_OfficialWithFlag_GlobalFallback: caller
// is project-scoped, target is global-only — fallback resolves.
func TestResolveDynamicTarget_OfficialWithFlag_GlobalFallback(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "proj-1", officialManifest("functions", true), nil)
	target := seedRunningInstall(t, s, "tables", "" /* global */, sdk.Manifest{Name: "tables"}, nil)

	id, _, ok := s.resolveDynamicTarget(caller, "tables", "")
	if !ok || id != target {
		t.Errorf("global-fallback failed; id=%d ok=%v", id, ok)
	}
}

// A global official caller can delegate the current worker project. Target
// resolution must prefer that project's install before the global fallback.
func TestResolveDynamicTarget_GlobalCallerUsesDelegatedProject(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "", officialManifest("functions", true), nil)
	globalTarget := seedRunningInstall(t, s, "tables", "", sdk.Manifest{Name: "tables"}, nil)
	var appID int64
	if err := s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, globalTarget).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	res, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-1', 'running', 1)`, appID)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := res.LastInsertId()

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "proj-1")
	if !ok {
		t.Fatalf("expected resolve, got msg=%q", msg)
	}
	if id != target {
		t.Errorf("id = %d, want delegated-project target %d", id, target)
	}
}

// TestResolveDynamicTarget_TargetAbsent_DistinctError: eligible
// caller, target nowhere — distinct 'app not reachable' error so
// consumers can diagnose the cause.
func TestResolveDynamicTarget_TargetAbsent_DistinctError(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "proj-1", officialManifest("functions", true), nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "deploy", "")
	if ok || id != 0 {
		t.Fatalf("expected reject (no target), got id=%d ok=%v", id, ok)
	}
	if !strings.Contains(msg, "app not reachable") {
		t.Errorf("expected 'app not reachable', got %q", msg)
	}
}

// TestResolveDynamicTarget_CrossProject_Blocked: caller in proj-A,
// target only in proj-B (no global) — must not leak.
func TestResolveDynamicTarget_CrossProject_Blocked(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "functions", "proj-A", officialManifest("functions", true), nil)
	seedRunningInstall(t, s, "tables", "proj-B", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if ok || id != 0 {
		t.Fatalf("cross-project leak: id=%d ok=%v", id, ok)
	}
	if !strings.Contains(msg, "app not reachable") {
		t.Errorf("expected 'app not reachable', got %q", msg)
	}
}

// TestOfficialAppPrefixes_DefaultAndEnv covers prefix resolution.
func TestOfficialAppPrefixes_DefaultAndEnv(t *testing.T) {
	prefixes := officialAppPrefixes()
	if len(prefixes) != 1 || prefixes[0] != "github.com/apteva/" {
		t.Errorf("default = %v", prefixes)
	}
	t.Setenv("APTEVA_OFFICIAL_APP_PREFIXES", "github.com/myorg/, github.com/apteva/ ")
	prefixes = officialAppPrefixes()
	if len(prefixes) != 2 || prefixes[0] != "github.com/myorg/" || prefixes[1] != "github.com/apteva/" {
		t.Errorf("env = %v", prefixes)
	}
}

// TestIsOfficialCaller: nil / missing source = false; matching
// prefix = true; non-matching = false.
func TestIsOfficialCaller(t *testing.T) {
	if isOfficialCaller(nil) {
		t.Error("nil manifest reported official")
	}
	if isOfficialCaller(&sdk.Manifest{Name: "x"}) {
		t.Error("manifest with no source reported official")
	}
	if !isOfficialCaller(&sdk.Manifest{
		Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps"}},
	}) {
		t.Error("apteva-source manifest not recognised official")
	}
	if isOfficialCaller(&sdk.Manifest{
		Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Repo: "github.com/evil/x"}},
	}) {
		t.Error("third-party manifest reported official")
	}
}

// TestIsOfficialCaller_ExtendedViaEnv: an operator-extended prefix
// list lets a non-apteva repo count as official (e.g. CI mirrors).
func TestIsOfficialCaller_ExtendedViaEnv(t *testing.T) {
	t.Setenv("APTEVA_OFFICIAL_APP_PREFIXES", "github.com/myorg/")
	if !isOfficialCaller(&sdk.Manifest{
		Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Repo: "github.com/myorg/apps"}},
	}) {
		t.Error("operator-extended prefix should mark caller as official")
	}
	// The default apteva prefix is dropped when env is set; verify.
	if isOfficialCaller(&sdk.Manifest{
		Runtime: sdk.Runtime{Source: &sdk.SourceSpec{Repo: "github.com/apteva/apps"}},
	}) {
		t.Error("env-set prefixes should fully replace the default")
	}
}

func manifestWithAppDep(name, dep string, optional bool) sdk.Manifest {
	return sdk.Manifest{Name: name, Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: dep, Optional: optional}}}}
}

// A declared dependency that is not installed is reported as such, apart from
// a permissions refusal.
func TestResolveDynamicTarget_DeclaredDependencyNotInstalled(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workspaces", "proj-1", manifestWithAppDep("workspaces", "containers", true), nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "containers", "")
	if ok || id != 0 || msg != "app not installed: containers" {
		t.Fatalf("got id=%d ok=%v msg=%q, want app not installed", id, ok, msg)
	}
	if code := appCallErrorCode(msg); code != "app_not_installed" {
		t.Fatalf("code = %q", code)
	}

	// Installed in another project only: still not installed for this caller.
	seedRunningInstall(t, s, "containers", "proj-2", sdk.Manifest{Name: "containers"}, nil)
	if _, msg, _ := s.resolveDynamicTarget(caller, "containers", ""); msg != "app not installed: containers" {
		t.Fatalf("other project's install: msg=%q, want app not installed", msg)
	}
}

// Installed but never linked is a binding problem, and says so.
func TestResolveDynamicTarget_DeclaredDependencyInstalledButUnbound(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "workspaces", "proj-1", manifestWithAppDep("workspaces", "containers", true), nil)
	seedRunningInstall(t, s, "containers", "proj-1", sdk.Manifest{Name: "containers"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "containers", "")
	if ok || id != 0 || !strings.HasPrefix(msg, "app not bound: containers") || !strings.Contains(msg, "installed") {
		t.Fatalf("got id=%d ok=%v msg=%q, want app not bound (installed)", id, ok, msg)
	}
	if code := appCallErrorCode(msg); code != "app_not_bound" {
		t.Fatalf("code = %q", code)
	}
}

// A caller that does not declare the app learns nothing about installs.
func TestResolveDynamicTarget_UndeclaredAppRevealsNothing(t *testing.T) {
	s := newTestServer(t)
	caller := seedRunningInstall(t, s, "notes", "proj-1", sdk.Manifest{Name: "notes"}, nil)
	_, absent, _ := s.resolveDynamicTarget(caller, "containers", "")
	seedRunningInstall(t, s, "containers", "proj-1", sdk.Manifest{Name: "containers"}, nil)
	_, present, _ := s.resolveDynamicTarget(caller, "containers", "")
	if absent != "app not bound: containers" || present != absent {
		t.Fatalf("absent=%q present=%q, want the same plain refusal", absent, present)
	}
}

// A manifest that names an official repo but takes its runnable code from
// somewhere else is not official: prebuilt downloads win over source.
func TestIsOfficialCaller_EveryCodeLocationMustBeTrusted(t *testing.T) {
	const good = "https://github.com/apteva/apps/releases/download/functions/v1.23.0/functions-linux-amd64.tgz"
	official := func(edit func(*sdk.Manifest)) bool {
		m := officialManifest("functions", true)
		edit(&m)
		return isOfficialCaller(&m)
	}
	artifact := func(url string) func(*sdk.Manifest) {
		return func(m *sdk.Manifest) {
			m.Runtime.Artifacts = map[string]sdk.BundleSpec{"linux-amd64": {URL: url, SHA256: strings.Repeat("a", 64)}}
		}
	}

	if !official(func(*sdk.Manifest) {}) {
		t.Fatal("source-only official manifest refused")
	}
	if !official(artifact(good)) {
		t.Fatal("official artifact refused")
	}
	if !official(func(m *sdk.Manifest) { m.Runtime.Binaries = map[string]string{"darwin-arm64": good} }) {
		t.Fatal("official binary refused")
	}

	refused := map[string]func(*sdk.Manifest){
		"artifact on another host":       artifact("https://evil.example/functions.tgz"),
		"artifact in another owner":      artifact("https://github.com/evil/apps/releases/download/x.tgz"),
		"artifact with dot segments":     artifact("https://github.com/apteva/../evil/apps/releases/x.tgz"),
		"artifact with encoded segments": artifact("https://github.com/apteva/%2e%2e/evil/x.tgz"),
		"artifact over http":             artifact("http://github.com/apteva/apps/releases/x.tgz"),
		"artifact with userinfo":         artifact("https://github.com@evil.example/apteva/apps/x.tgz"),
		"artifact on a lookalike host":   artifact("https://github.com.evil.example/apteva/apps/x.tgz"),
		"artifact with a port":           artifact("https://github.com:8443/apteva/apps/x.tgz"),
		"local artifact":                 artifact("/tmp/functions.tgz"),
		"file artifact":                  artifact("file:///tmp/functions.tgz"),
		"one bad artifact among good": func(m *sdk.Manifest) {
			m.Runtime.Artifacts = map[string]sdk.BundleSpec{"linux-amd64": {URL: good}, "darwin-arm64": {URL: "https://evil.example/x.tgz"}}
		},
		"binary on another host": func(m *sdk.Manifest) {
			m.Runtime.Binaries = map[string]string{"linux-amd64": "https://evil.example/bin"}
		},
		"bundle on another host":    func(m *sdk.Manifest) { m.Runtime.Bundle = &sdk.BundleSpec{URL: "https://evil.example/ui.tgz"} },
		"repo with dot segments":    func(m *sdk.Manifest) { m.Runtime.Source.Repo = "github.com/apteva/../evil/apps" },
		"repo in a lookalike owner": func(m *sdk.Manifest) { m.Runtime.Source.Repo = "github.com/apteva-evil/apps" },
	}
	for name, edit := range refused {
		if official(edit) {
			t.Errorf("%s: manifest reported official", name)
		}
	}
}

// The spoofed manifest gets no dynamic access to other apps.
func TestResolveDynamicTarget_SpoofedOfficialManifestStaysBlocked(t *testing.T) {
	s := newTestServer(t)
	m := officialManifest("functions", true)
	m.Runtime.Artifacts = map[string]sdk.BundleSpec{"linux-amd64": {URL: "https://evil.example/functions.tgz", SHA256: strings.Repeat("a", 64)}}
	caller := seedRunningInstall(t, s, "functions", "proj-1", m, nil)
	seedRunningInstall(t, s, "tables", "proj-1", sdk.Manifest{Name: "tables"}, nil)

	id, msg, ok := s.resolveDynamicTarget(caller, "tables", "")
	if ok || id != 0 || !strings.Contains(msg, "app not bound") {
		t.Fatalf("spoofed official caller: id=%d ok=%v msg=%q, want refused", id, ok, msg)
	}
}
