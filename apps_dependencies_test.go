package main

// Tests for the app-dep binding pipeline.
//
// What the production code stitches together:
//
//   parent.requires.apps[].name      ──┐
//                                      ├─►  install_bindings JSON
//   findInstalledApp                  ──┤   keyed by dep.Name = id
//   reconcileAppDepBindings           ──┘
//                                          ↓
//                                       installBoundApp
//                                          ↓
//                                   /api/apps/callback/apps/<dep>/call
//                                          ↓
//                                       allowed (or 403)
//
// These tests exercise each piece independently plus the end-to-end
// "I have a manifest with requires.apps[name=jobs] and a binding row
// pointing at the jobs install — is the call authorized?" path.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// seedRunningInstall inserts an apps row + an app_installs row marked
// status='running' with the given project_id and bindings. Returns
// the install id. Doesn't touch the in-memory registry.
func seedRunningInstall(t *testing.T, s *Server, name, projectID string, manifest sdk.Manifest, bindings map[string]any) int64 {
	t.Helper()
	mj, _ := json.Marshal(manifest)
	if _, err := s.store.db.Exec(
		`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES (?, 'git', '', '', ?)`,
		name, string(mj),
	); err != nil {
		t.Fatalf("seed app %q: %v", name, err)
	}
	var appID int64
	s.store.db.QueryRow(`SELECT id FROM apps WHERE name=?`, name).Scan(&appID)
	bj := []byte("{}")
	if bindings != nil {
		bj, _ = json.Marshal(bindings)
	}
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings)
		 VALUES (?, ?, 'running', 1, ?)`,
		appID, projectID, string(bj),
	)
	if err != nil {
		t.Fatalf("seed install %q: %v", name, err)
	}
	id, _ := res.LastInsertId()
	s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'a@b.c', 'x')`)
	return id
}

// readBindings returns the parsed integration_bindings for an install.
func readBindings(t *testing.T, s *Server, installID int64) map[string]any {
	t.Helper()
	var raw string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(integration_bindings, '{}') FROM app_installs WHERE id=?`, installID,
	).Scan(&raw); err != nil {
		t.Fatalf("read bindings: %v", err)
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// asInt64 normalizes the JSON-decoded number type. Bindings written
// by Go end up as float64 after a round-trip through json.Unmarshal
// into map[string]any.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// ---------- findInstalledApp ----------

func TestFindInstalledApp_GlobalParentMatchesAnyScope(t *testing.T) {
	s := newTestServer(t)
	jobsID := seedRunningInstall(t, s, "jobs", "proj-1", sdk.Manifest{Name: "jobs"}, nil)
	id, ok := s.findInstalledApp("jobs", "" /* global parent */)
	if !ok || id != jobsID {
		t.Fatalf("global parent should pick up project-scoped jobs install %d, got id=%d ok=%v", jobsID, id, ok)
	}
}

func TestFindInstalledApp_ProjectParentPrefersSameProject(t *testing.T) {
	s := newTestServer(t)
	// In production a given app name has exactly one row in apps;
	// two installs of the same app share that app_id. Replicate
	// that here: one apps row, two app_installs rows.
	globalJobs := seedRunningInstall(t, s, "jobs", "" /* global */, sdk.Manifest{Name: "jobs"}, nil)
	var jobsAppID int64
	s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, globalJobs).Scan(&jobsAppID)
	res, err := s.store.db.Exec(
		`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings)
		 VALUES (?, 'proj-1', 'running', 1, '{}')`,
		jobsAppID,
	)
	if err != nil {
		t.Fatalf("seed second install: %v", err)
	}
	projectJobs, _ := res.LastInsertId()
	id, ok := s.findInstalledApp("jobs", "proj-1")
	if !ok {
		t.Fatalf("expected to find jobs in project scope")
	}
	if id != projectJobs {
		t.Fatalf("expected same-project install %d to win, got %d (global was %d)", projectJobs, id, globalJobs)
	}
}

func TestFindInstalledApp_ProjectParentFallsBackToGlobal(t *testing.T) {
	s := newTestServer(t)
	globalJobs := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	id, ok := s.findInstalledApp("jobs", "proj-1")
	if !ok || id != globalJobs {
		t.Fatalf("project parent should fall back to global jobs install, got id=%d ok=%v", id, ok)
	}
}

func TestFindInstalledApp_NotInstalled(t *testing.T) {
	s := newTestServer(t)
	if id, ok := s.findInstalledApp("jobs", "proj-1"); ok {
		t.Fatalf("expected not-found, got id=%d", id)
	}
}

func TestFindInstalledApp_IgnoresPendingAndErrored(t *testing.T) {
	s := newTestServer(t)
	// status defaults to 'pending' for INSERTs that don't set it; we
	// override explicitly here via seedRunningInstall + immediate UPDATE.
	id := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, id)
	if got, ok := s.findInstalledApp("jobs", ""); ok {
		t.Fatalf("expected not-found for pending install, got id=%d", got)
	}
	s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, id)
	if got, ok := s.findInstalledApp("jobs", ""); ok {
		t.Fatalf("expected not-found for errored install, got id=%d", got)
	}
}

// ---------- install dependency cascade ----------

func TestInstallDepsRecursive_SkipsUnselectedOptionalApp(t *testing.T) {
	s := newTestServer(t)
	resolved := map[string]int64{}
	err := s.installDepsRecursive(
		1,
		"",
		[]sdk.RequiredAppRef{{Name: "cdn", Optional: true}},
		map[string]any{"cdn": nil},
		map[string]string{"cdn": "http://127.0.0.1:1/should-not-fetch"},
		map[string]bool{},
		map[string]bool{},
		resolved,
	)
	if err != nil {
		t.Fatalf("optional unselected dep should be skipped, got error: %v", err)
	}
	if _, ok := resolved["cdn"]; ok {
		t.Fatalf("unselected optional dep should not be resolved: %v", resolved)
	}
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no dep installs, got %d", n)
	}
}

func TestInstallDepsRecursive_BindsSelectedOptionalExistingApp(t *testing.T) {
	s := newTestServer(t)
	cdnID := seedRunningInstall(t, s, "cdn", "", sdk.Manifest{Name: "cdn"}, nil)
	resolved := map[string]int64{}
	err := s.installDepsRecursive(
		1,
		"",
		[]sdk.RequiredAppRef{{Name: "cdn", Optional: true}},
		map[string]any{"cdn": float64(cdnID)},
		nil,
		map[string]bool{},
		map[string]bool{},
		resolved,
	)
	if err != nil {
		t.Fatalf("selected optional dep should resolve existing install: %v", err)
	}
	if id := resolved["cdn"]; id != cdnID {
		t.Fatalf("expected resolved cdn=%d, got %d (%v)", cdnID, id, resolved)
	}
}

func TestInstallDependencies_OptionalOnlyUnselectedDoesNotLoadRegistry(t *testing.T) {
	t.Setenv("APTEVA_APP_REGISTRY_URL", "http://127.0.0.1:1/unreachable")
	s := newTestServer(t)
	manifest := &sdk.Manifest{
		Name: "storage",
		Requires: sdk.Requires{
			Apps: []sdk.RequiredAppRef{{Name: "cdn", Optional: true}},
		},
	}
	resolved, err := s.installDependencies(1, manifest, "", map[string]any{"cdn": nil})
	if err != nil {
		t.Fatalf("optional-only unselected deps should not touch registry: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("expected no resolved deps, got %v", resolved)
	}
}

// ---------- reconcileAppDepBindings ----------

// The headline scenario: the bug we just fixed.
func TestReconcileAppDepBindings_FillsMissingAppDep(t *testing.T) {
	s := newTestServer(t)
	jobsID := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	parentManifest := sdk.Manifest{
		Name: "backup",
		Requires: sdk.Requires{
			Apps: []sdk.RequiredAppRef{{Name: "jobs"}},
		},
	}
	parentID := seedRunningInstall(t, s, "backup", "", parentManifest, map[string]any{
		"cloud_storage": float64(20), // pre-existing integration binding stays
	})

	changed, err := s.reconcileAppDepBindings(parentID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !changed {
		t.Fatalf("reconcile should have written the missing jobs key")
	}
	got := readBindings(t, s, parentID)
	if id, ok := asInt64(got["jobs"]); !ok || id != jobsID {
		t.Fatalf("expected bindings.jobs=%d, got %v", jobsID, got["jobs"])
	}
	if id, ok := asInt64(got["cloud_storage"]); !ok || id != 20 {
		t.Fatalf("expected cloud_storage=20 preserved, got %v", got["cloud_storage"])
	}
}

func TestReconcileAppDepBindings_PreservesExplicitNull(t *testing.T) {
	s := newTestServer(t)
	seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	parentManifest := sdk.Manifest{
		Name: "backup",
		Requires: sdk.Requires{
			Apps: []sdk.RequiredAppRef{{Name: "jobs", Optional: true}},
		},
	}
	// Operator deliberately set jobs to null to disable the integration.
	parentID := seedRunningInstall(t, s, "backup", "", parentManifest, map[string]any{
		"jobs": nil,
	})
	changed, err := s.reconcileAppDepBindings(parentID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed {
		t.Fatalf("reconcile should not touch explicit nulls")
	}
	got := readBindings(t, s, parentID)
	if v, present := got["jobs"]; !present || v != nil {
		t.Fatalf("expected explicit null preserved, got present=%v v=%v", present, v)
	}
}

func TestReconcileAppDepBindings_SkipsOptionalAppDep(t *testing.T) {
	s := newTestServer(t)
	seedRunningInstall(t, s, "cdn", "", sdk.Manifest{Name: "cdn"}, nil)
	parentManifest := sdk.Manifest{
		Name: "storage",
		Requires: sdk.Requires{
			Apps: []sdk.RequiredAppRef{{Name: "cdn", Optional: true}},
		},
	}
	parentID := seedRunningInstall(t, s, "storage", "", parentManifest, map[string]any{})
	changed, err := s.reconcileAppDepBindings(parentID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed {
		t.Fatalf("optional deps should not be backfilled automatically")
	}
	got := readBindings(t, s, parentID)
	if _, present := got["cdn"]; present {
		t.Fatalf("expected optional cdn key absent, got %v", got)
	}
}

func TestReconcileAppDepBindings_Idempotent(t *testing.T) {
	s := newTestServer(t)
	jobsID := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	parentManifest := sdk.Manifest{
		Name:     "backup",
		Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs"}}},
	}
	parentID := seedRunningInstall(t, s, "backup", "", parentManifest, map[string]any{})

	changed, err := s.reconcileAppDepBindings(parentID)
	if err != nil || !changed {
		t.Fatalf("first reconcile: changed=%v err=%v", changed, err)
	}
	changed2, err := s.reconcileAppDepBindings(parentID)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if changed2 {
		t.Fatalf("second reconcile should be a no-op, returned changed=true")
	}
	got := readBindings(t, s, parentID)
	if id, _ := asInt64(got["jobs"]); id != jobsID {
		t.Fatalf("post-idempotent state corrupted: %v", got["jobs"])
	}
}

func TestReconcileAppDepBindings_DepNotInstalled(t *testing.T) {
	s := newTestServer(t)
	parentManifest := sdk.Manifest{
		Name:     "backup",
		Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs"}}},
	}
	parentID := seedRunningInstall(t, s, "backup", "", parentManifest, map[string]any{})
	changed, err := s.reconcileAppDepBindings(parentID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed {
		t.Fatalf("no jobs install exists; reconcile should not write a key")
	}
	got := readBindings(t, s, parentID)
	if _, present := got["jobs"]; present {
		t.Fatalf("expected jobs key absent, got %v", got)
	}
}

func TestReconcileAppDepBindings_NoRequiresApps(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Name: "x"} // no deps
	id := seedRunningInstall(t, s, "x", "", manifest, map[string]any{"a": 1})
	changed, err := s.reconcileAppDepBindings(id)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed {
		t.Fatalf("nothing to reconcile, but reported changed=true")
	}
}

func TestReconcileAllAppDepBindings_HealsMultipleInstalls(t *testing.T) {
	s := newTestServer(t)
	jobsID := seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs"}, nil)
	storageID := seedRunningInstall(t, s, "storage", "", sdk.Manifest{Name: "storage"}, nil)
	backupID := seedRunningInstall(t, s, "backup", "",
		sdk.Manifest{Name: "backup", Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs"}}}},
		map[string]any{},
	)
	docsID := seedRunningInstall(t, s, "docs", "",
		sdk.Manifest{Name: "docs", Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "storage"}}}},
		map[string]any{},
	)

	s.reconcileAllAppDepBindings()

	if id, _ := asInt64(readBindings(t, s, backupID)["jobs"]); id != jobsID {
		t.Fatalf("backup→jobs not bound: got %v", readBindings(t, s, backupID))
	}
	if id, _ := asInt64(readBindings(t, s, docsID)["storage"]); id != storageID {
		t.Fatalf("docs→storage not bound: got %v", readBindings(t, s, docsID))
	}
}

// ---------- handleSetInstallBindings2 (operator PUT /bindings) ----------

func putBindings(s *Server, installID int64, body map[string]any) *httptest.ResponseRecorder {
	bj, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT",
		"/apps/installs/"+itoa(installID)+"/bindings",
		strings.NewReader(string(bj)),
	)
	rec := httptest.NewRecorder()
	s.handleSetInstallBindings2(rec, req)
	return rec
}

// Modern-shape requires.apps name is now an accepted bindings key —
// before the fix this would return 400 "unknown role: jobs".
func TestSetInstallBindings_AcceptsRequiresAppsName(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Name:     "backup",
		Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs"}}},
	}
	id := seedRunningInstall(t, s, "backup", "", manifest, nil)

	rec := putBindings(s, id, map[string]any{"jobs": 48})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	got := readBindings(t, s, id)
	if v, _ := asInt64(got["jobs"]); v != 48 {
		t.Fatalf("expected bindings.jobs=48, got %v", got)
	}
}

// MERGE semantics: the dashboard's BackupPanel sends only
// {cloud_storage} on save — pre-existing app-dep bindings must
// survive that PUT. Before the merge fix, the handler did
// SET integration_bindings = body, wiping cascade-written keys.
func TestSetInstallBindings_PartialPUTPreservesOtherKeys(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Name: "backup",
		Requires: sdk.Requires{
			Integrations: []sdk.IntegrationDep{{Role: "cloud_storage", Required: false}},
			Apps:         []sdk.RequiredAppRef{{Name: "jobs"}},
		},
	}
	id := seedRunningInstall(t, s, "backup", "", manifest, map[string]any{
		"jobs": 48, // already bound by cascade / boot reconcile
	})

	rec := putBindings(s, id, map[string]any{"cloud_storage": 20})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	got := readBindings(t, s, id)
	if v, _ := asInt64(got["cloud_storage"]); v != 20 {
		t.Fatalf("expected cloud_storage=20, got %v", got)
	}
	if v, _ := asInt64(got["jobs"]); v != 48 {
		t.Fatalf("expected jobs=48 preserved, got %v", got)
	}
}

func TestSetInstallBindings_RejectsUnknownKey(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{Name: "x"} // no deps declared
	id := seedRunningInstall(t, s, "x", "", manifest, nil)
	rec := putBindings(s, id, map[string]any{"made_up": 42})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown key, got %d", rec.Code)
	}
}

func TestSetInstallBindings_RejectsRequiredAppDepNull(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Name:     "backup",
		Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs"}}}, // required (no Optional flag)
	}
	id := seedRunningInstall(t, s, "backup", "", manifest, nil)
	// PUT explicitly sets jobs=null on a required dep.
	rec := putBindings(s, id, map[string]any{"jobs": nil})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for null required app dep, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestSetInstallBindings_AllowsOptionalAppDepNull(t *testing.T) {
	s := newTestServer(t)
	manifest := sdk.Manifest{
		Name:     "backup",
		Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "jobs", Optional: true}}},
	}
	id := seedRunningInstall(t, s, "backup", "", manifest, map[string]any{"jobs": 48})
	rec := putBindings(s, id, map[string]any{"jobs": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for optional null, got %d body=%q", rec.Code, rec.Body.String())
	}
	got := readBindings(t, s, id)
	if v, present := got["jobs"]; !present || v != nil {
		t.Fatalf("expected jobs=null preserved, got present=%v v=%v", present, v)
	}
}

// ---------- end-to-end: bound app call passes installBoundApp ----------

// After reconcile writes bindings[jobs]=<id>, a sidecar's
// /apps/callback/apps/jobs/call should pass the binding gate. (The
// call still 502s on "target app not running" because we don't have
// a real sidecar in the test, but it gets past 403.)
func TestEndToEnd_RequiresAppsBindingPassesAuthorization(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()

	jobsManifest := sdk.Manifest{Name: "jobs"}
	jobsID := seedRunningInstall(t, s, "jobs", "", jobsManifest, nil)
	// Register jobs in the in-memory registry — installBoundApp's
	// "bound install must be running and AppName must match" check
	// reads from this.
	s.installedApps.Add(&InstalledApp{
		InstallID: jobsID,
		AppName:   "jobs",
		Manifest:  jobsManifest,
	})

	parentManifest := sdk.Manifest{
		Name: "backup",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "jobs"}},
		},
	}
	backupID := seedRunningInstall(t, s, "backup", "", parentManifest, map[string]any{})

	// Reconcile writes bindings[jobs] = jobsID.
	if changed, err := s.reconcileAppDepBindings(backupID); err != nil || !changed {
		t.Fatalf("reconcile: changed=%v err=%v", changed, err)
	}

	// Now a callback to jobs.call from the backup install should pass
	// the binding gate. We don't have a real sidecar; we just check
	// it gets PAST the 403 we used to hit.
	req := httptest.NewRequest("POST", "/apps/callback/apps/jobs/call",
		strings.NewReader(`{"tool":"jobs_schedule","input":{}}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(backupID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("regression: still got 403 'app not bound', body=%q", rec.Body.String())
	}
	// 502 (no real sidecar) or success is fine — the point is that
	// we passed the binding gate.
}

// ---------- preflight emits app-dep candidates ----------

func TestPreflight_EmitsKindAppRowsForRequiresApps(t *testing.T) {
	s := newTestServer(t)
	seedRunningInstall(t, s, "jobs", "", sdk.Manifest{Name: "jobs", DisplayName: "Jobs"}, nil)

	yaml := `schema: apteva-app/v1
name: backup
display_name: Backup
version: 0.1.0
requires:
  apps:
    - name: jobs
      reason: Cron scheduling
provides:
  http_routes: []
  mcp_tools: []
runtime:
  kind: source
  port: 8080
  source:
    repo: github.com/x/y
    ref: main
    entry: backup
`
	body := map[string]any{"manifest_yaml": yaml, "project_id": "proj-1"}
	bj, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/apps/install/preflight", strings.NewReader(string(bj)))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handlePreflightApp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Roles []struct {
			Role     string `json:"role"`
			Kind     string `json:"kind"`
			Required bool   `json:"required"`
			AppCands []struct {
				InstallID int64  `json:"install_id"`
				AppName   string `json:"app_name"`
			} `json:"app_candidates"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var jobsRow *struct {
		Role     string `json:"role"`
		Kind     string `json:"kind"`
		Required bool   `json:"required"`
		AppCands []struct {
			InstallID int64  `json:"install_id"`
			AppName   string `json:"app_name"`
		} `json:"app_candidates"`
	}
	for i := range out.Roles {
		if out.Roles[i].Role == "jobs" && out.Roles[i].Kind == "app" {
			jobsRow = &out.Roles[i]
			break
		}
	}
	if jobsRow == nil {
		t.Fatalf("expected a kind=app row for jobs, got %+v", out.Roles)
	}
	if !jobsRow.Required {
		t.Fatalf("non-optional dep should be Required=true")
	}
	if len(jobsRow.AppCands) == 0 {
		t.Fatalf("expected at least one app candidate, got 0")
	}
	// At least one candidate must have AppName=jobs.
	hit := false
	for _, c := range jobsRow.AppCands {
		if c.AppName == "jobs" {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("no candidate with AppName=jobs in %+v", jobsRow.AppCands)
	}
}
