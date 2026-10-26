package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

const componentManifestFixture = `schema: apteva-app/v1
name: component-parent
version: 1.0.0
scopes: [project, global]
runtime:
  kind: service
  port: 8080
  image: example.invalid/component-parent:1.0.0
requires:
  permissions: [platform.apps.call]
components:
  executor:
    protocol: test.executor/v1
    required: true
    capabilities:
      passive_start: true
      drain: true
      replica_safe: true
      worker_coordination: true
      routing: stateless
    manifest:
      schema: apteva-app/v1
      name: generic-worker
      version: 1.0.0
      scopes: [project, global]
      runtime:
        kind: source
        port: 8080
        source:
          repo: https://github.com/example/workers
          ref: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          entry: .
      provides:
        mcp_tools:
          - name: submit
            description: Submit an idempotent job
`

func componentDeclarationsFixture(t *testing.T) appComponentDeclarations {
	t.Helper()
	manifest, err := sdk.ParseManifest([]byte(componentManifestFixture))
	if err != nil {
		t.Fatal(err)
	}
	return manifest.Components
}
func putComponentDefinitions(t *testing.T, s *Server, parent int64, version string, defs appComponentDeclarations) {
	t.Helper()
	tx, err := s.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = saveAppComponentDeclarations(tx, parent, version, defs); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func componentRuntimeFixture(t *testing.T) (*Server, *appRuntimeManager, *runtimeBackendFixture, int64, int64, runtimePlacement) {
	t.Helper()
	s, m, f, _, p := runtimeTestSetup(t)
	manifest := sdk.Manifest{Name: "component-parent", Version: "1.0.0", Scopes: []sdk.Scope{sdk.ScopeProject, sdk.ScopeGlobal}, Runtime: sdk.Runtime{Kind: "service"}, Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}}}
	parent := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	defs := componentDeclarationsFixture(t)
	putComponentDefinitions(t, s, parent, "1.0.0", defs)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_apteva/runtime/component/describe") {
			if r.Header.Get("Authorization") == "" {
				t.Error("component descriptor missing auth")
			}
			writeJSON(w, map[string]string{"protocol": "test.executor/v1"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/mcp") {
			var rpc struct {
				Method string `json:"method"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if json.NewDecoder(r.Body).Decode(&rpc) != nil {
				t.Error("bad app call")
				http.Error(w, "bad call", 400)
				return
			}
			if rpc.Method == "tools/call" && (r.Header.Get(sdk.HeaderBoundCallerInstallID) != fmt.Sprint(parent) || r.Header.Get("Authorization") == "") {
				t.Error("lost component call provenance")
			}
			if _, bad := rpc.Params.Arguments["owner_install_id"]; bad {
				t.Error("unexpected owner routing field")
			}
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"accepted\":true,\"job_id\":\"job-1\"}"}]}}`)
			return
		}
		original.ServeHTTP(w, r)
	})
	child, _, err := s.ensureComponent(parent, "executor", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, m, f, parent, child, p
}

func deployComponentFixture(t *testing.T, m *appRuntimeManager, child int64, p runtimePlacement, key string) runtimeOperation {
	t.Helper()
	op, err := m.begin(child, "deploy", key, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "succeeded" {
		t.Fatalf("component deployment: %s", op.Error)
	}
	return op
}

func TestAppComponentManifestValidation(t *testing.T) {
	defs := componentDeclarationsFixture(t)
	if len(defs) != 1 || !defs["executor"].Capabilities.PassiveStart {
		t.Fatal("component declaration lost")
	}
	if empty, err := sdk.ParseManifest([]byte(strings.Split(componentManifestFixture, "components:")[0])); err != nil || len(empty.Components) != 0 {
		t.Fatal("legacy compatibility", err)
	}
	if _, err := parseServerAppManifest([]byte(componentManifestFixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := parseServerAppManifest([]byte(componentManifestFixture + "unknown_typo: true\n")); err == nil {
		t.Fatal("Server extension bypassed strict SDK validation")
	}
	for _, change := range [][2]string{{"ref: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ref: main"}, {"protocol: test.executor/v1", "protocol: executor"}, {"passive_start: true", "passive_start: false"}, {"drain: true", "drain: false"}, {"routing: stateless", "routing: unknown"}, {"    required: true", "    unrecognized: true"}, {"  executor:", "  ../executor:"}} {
		if _, err := sdk.ParseManifest([]byte(strings.Replace(componentManifestFixture, change[0], change[1], 1))); err == nil {
			t.Errorf("accepted unsupported declaration: %v", change)
		}
	}
}

func TestAppComponentOperatorDeployRetryAndList(t *testing.T) {
	s, m, f, parent, child, p := componentRuntimeFixture(t)
	var operationID string
	for range 2 {
		req := httptest.NewRequest("POST", fmt.Sprintf("/apps/installs/%d/components/executor", parent), strings.NewReader(runtimeJSON(map[string]any{"placement": p})))
		req.Header.Set("X-User-ID", "1")
		req.Header.Set("Idempotency-Key", "owned-worker-request")
		resp := httptest.NewRecorder()
		s.handleAppComponents(resp, req, parent)
		if resp.Code != 202 {
			t.Fatalf("deploy %d: %s", resp.Code, resp.Body.String())
		}
		var result struct {
			InstallID int64            `json:"install_id"`
			Operation runtimeOperation `json:"operation"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.InstallID != child || (operationID != "" && operationID != result.Operation.ID) {
			t.Fatal("retry created another identity or operation")
		}
		operationID = result.Operation.ID
	}
	op, _ := m.operation(operationID)
	if op = waitRuntimeOperation(t, m, op); op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	f.mu.Lock()
	starts := 0
	for _, event := range f.events {
		if strings.HasPrefix(event, "start:") {
			starts++
		}
	}
	f.mu.Unlock()
	if starts != 1 {
		t.Fatalf("duplicate executions: %d", starts)
	}
	req := httptest.NewRequest("GET", "/apps", nil)
	req.Header.Set("X-User-ID", "1")
	resp := httptest.NewRecorder()
	s.handleListApps(resp, req)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	var apps []AppRow
	if err := json.Unmarshal(resp.Body.Bytes(), &apps); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range apps {
		if a.InstallID == child {
			t.Fatal("private component leaked into top-level app list")
		}
		if a.InstallID == parent {
			found = len(a.Components) == 1 && a.Components[0].InstallID == child
		}
	}
	if !found {
		t.Fatal("owner list row lost component metadata")
	}
	if _, err := m.begin(parent, "stop", "stop-owner-while-worker-runs", runtimePlacement{}, nil); err == nil {
		t.Fatal("owner stopped with active worker")
	}
	req = httptest.NewRequest("POST", fmt.Sprintf("/apps/installs/%d/upgrade", child), strings.NewReader(`{}`))
	req.Header.Set("X-User-ID", "1")
	resp = httptest.NewRecorder()
	s.handleUpgradeApp(resp, req)
	if resp.Code != 409 {
		t.Fatal("worker can upgrade outside its owner's release contract")
	}
}

func TestAppComponentOwnershipPersistenceAndIdempotency(t *testing.T) {
	s, _, _, parent, child, _ := componentRuntimeFixture(t)
	duplicate, _, err := s.ensureComponent(parent, "executor", nil, nil)
	if err != nil || duplicate != child {
		t.Fatal("duplicate component", err)
	}
	manifest := sdk.Manifest{Name: "second-parent", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service"}}
	second := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	putComponentDefinitions(t, s, second, "1.0.0", componentDeclarationsFixture(t))
	other, _, err := s.ensureComponent(second, "executor", nil, nil)
	if err != nil || other == child {
		t.Fatal("component reused another parent's identity", err)
	}
	if _, err = s.store.db.Exec(`DELETE FROM app_installs WHERE id=?`, parent); err == nil {
		t.Fatal("concurrent deletion can orphan a component")
	}
	if _, err = s.store.db.Exec(`UPDATE app_installs SET project_id='other' WHERE id=?`, child); err == nil {
		t.Fatal("component changed scope independently")
	}
	if _, err = s.store.db.Exec(`UPDATE app_installs SET project_id='other' WHERE id=?`, parent); err == nil {
		t.Fatal("owner scope move orphaned component")
	}
	if err = s.store.migrateAppComponents(); err != nil {
		t.Fatal("migration is not restart-safe", err)
	}
	rows, err := s.componentSummaries(parent)
	if err != nil || len(rows) != 1 || rows[0].InstallID != child {
		t.Fatal("component metadata lost", err)
	}
	defs := componentDeclarationsFixture(t)
	def := defs["executor"]
	def.Protocol = "test.executor/v2"
	defs["executor"] = def
	tx, _ := s.store.db.Begin()
	err = saveAppComponentDeclarations(tx, parent, "1.0.0", defs)
	tx.Rollback()
	if err == nil {
		t.Fatal("release declaration changed in place")
	}
}

func TestAppComponentUsesExistingControllerAndAppCall(t *testing.T) {
	s, m, _, parent, child, p := componentRuntimeFixture(t)
	deployComponentFixture(t, m, child, p, "component-first")
	d, err := m.active(child)
	if err != nil || d.Component == nil || d.Component.OwnerInstallID != parent {
		t.Fatal("deployment ownership missing", err)
	}
	if id, err := s.resolveComponentCall(parent, "executor"); err != nil || id != child {
		t.Fatal("owned route unavailable", err)
	}
	s.LoadInstalledApps()
	request := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/component:executor/call", strings.NewReader(`{"tool":"submit","input":{"job_id":"job-1"}}`))
	request.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(parent))
	request.Header.Set(sdk.HeaderAppCallResult, "inner")
	response := httptest.NewRecorder()
	s.handleAppCallback(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"accepted":true`) {
		t.Fatalf("call %d: %s", response.Code, response.Body.String())
	}
	// A registry rebuild uses persisted ownership and the active execution,
	// without preparing another deployment or falling back to a local worker.
	s.installedApps = NewInstalledAppsRegistry()
	s.LoadInstalledApps()
	if _, err = s.resolveComponentCall(parent, "executor"); err != nil {
		t.Fatal("route not recovered", err)
	}
	if err = m.replicaEligible(d); err == nil {
		t.Fatal("component replicas enabled before coordinated support")
	}
	if err = s.registerAppMCP(child); err != nil {
		t.Fatal(err)
	}
	var count int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM mcp_servers WHERE upstream_id=?`, appMCPUpstreamID(child)).Scan(&count)
	if count != 0 {
		t.Fatal("component became agent-facing MCP")
	}
	if err = m.stopSync(child, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.resolveComponentCall(parent, "executor"); err == nil {
		t.Fatal("stopped remote component silently resolved locally")
	}
	var parentStatus string
	s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, parent).Scan(&parentStatus)
	if parentStatus != "running" {
		t.Fatal("stopping the worker stopped the parent")
	}
}

func TestAppComponentRejectsCrossOwnerAndForgedProjectCalls(t *testing.T) {
	s, m, _, parent, child, p := componentRuntimeFixture(t)
	deployComponentFixture(t, m, child, p, "component-isolation")
	s.LoadInstalledApps()
	otherManifest := sdk.Manifest{Name: "unrelated-caller", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service"}, Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: fmt.Sprintf("component-%d-executor", parent)}}}}
	other := seedRunningInstall(t, s, otherManifest.Name, "", otherManifest, map[string]any{fmt.Sprintf("component-%d-executor", parent): child})
	for _, target := range []string{"component:executor", fmt.Sprintf("component-%d-executor", parent)} {
		request := httptest.NewRequest("POST", "/apps/callback/apps/"+target+"/call", strings.NewReader(`{"tool":"submit","input":{}}`))
		request.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(other))
		response := httptest.NewRecorder()
		s.handleAppCallback(response, request)
		if response.Code == 200 {
			t.Fatal("cross-owner component access", target)
		}
	}
	request := httptest.NewRequest("POST", "/apps/callback/apps/component:executor/call", strings.NewReader(`{"tool":"submit","input":{"_project_id":"unowned-project"}}`))
	// A global admin may delegate across projects. Exercise the actual owner
	// boundary with a non-admin and another user's real project instead.
	s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`)
	s.store.db.Exec(`INSERT INTO users(id,email,password_hash,role) VALUES(2,'foreign-component-owner@test.local','hash','user')`)
	foreign, err := s.store.CreateProject(2, "Foreign component", "", "")
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", "/apps/callback/apps/component:executor/call", strings.NewReader(`{"tool":"submit","input":{"_project_id":"`+foreign.ID+`"}}`))
	request.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(parent))
	response := httptest.NewRecorder()
	s.handleAppCallback(response, request)
	if response.Code == 200 {
		t.Fatal("forged delegated project accepted")
	}
	// The ordinary browser/agent proxy is not a second route to private tools.
	request = httptest.NewRequest("GET", fmt.Sprintf("/apps/component-%d-executor/_install/%d/mcp", parent, child), nil)
	request.Header.Set("X-User-ID", "1")
	response = httptest.NewRecorder()
	s.handleAppProxy(response, request)
	if response.Code != 403 {
		t.Fatalf("private worker proxy status=%d", response.Code)
	}
}

func TestAppComponentFailedUpgradeRestoresApprovedWorker(t *testing.T) {
	s, m, f, parent, child, p := componentRuntimeFixture(t)
	deployComponentFixture(t, m, child, p, "component-old")
	old, _ := m.active(child)
	f.mu.Lock()
	f.oldID = old.ID
	f.failStart = true
	f.mu.Unlock()
	defs := componentDeclarationsFixture(t)
	def := defs["executor"]
	def.Manifest.Version = "2.0.0"
	def.Manifest.Runtime.Source.Ref = strings.Repeat("b", 40)
	defs["executor"] = def
	putComponentDefinitions(t, s, parent, "2.0.0", defs)
	s.store.db.Exec(`UPDATE app_installs SET version='2.0.0' WHERE id=?`, parent)
	op, err := m.begin(child, "deploy", "component-upgrade", p, &def.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "failed" {
		t.Fatal("failed candidate accepted")
	}
	active, err := m.active(child)
	if err != nil || active.Manifest.Version != "1.0.0" || active.Component == nil {
		t.Fatalf("approved previous worker not restored: %+v %v", active, err)
	}
	if _, err = s.resolveComponentCall(parent, "executor"); err != nil {
		t.Fatal("compatible rollback endpoint not restored", err)
	}
	f.mu.Lock()
	events := append([]string(nil), f.events...)
	f.mu.Unlock()
	stop, failedStart := -1, -1
	for i, event := range events {
		if event == "stop:"+old.ID {
			stop = i
		}
		if event == "start:"+op.DeploymentID {
			failedStart = i
		}
	}
	if stop < 0 || failedStart < stop {
		t.Fatalf("component update was not drain-first: %v", events)
	}
}

func TestAppComponentProtocolMismatchNeverActivates(t *testing.T) {
	s, m, f, _, child, p := componentRuntimeFixture(t)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_apteva/runtime/component/describe") {
			writeJSON(w, map[string]string{"protocol": "wrong.worker/v3"})
			return
		}
		original.ServeHTTP(w, r)
	})
	op, err := m.begin(child, "deploy", "component-bad-protocol", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	op = waitRuntimeOperation(t, m, op)
	if op.Status != "failed" || !strings.Contains(op.Error, "protocol") {
		t.Fatal("incompatible worker accepted", op.Error)
	}
	if active, _ := m.active(child); active.ID != "" {
		t.Fatal("incompatible worker published")
	}
	_ = s
}

func TestAppComponentOperatorEndpointAndDeletionGuard(t *testing.T) {
	s, _, _, parent, child, _ := componentRuntimeFixture(t)
	request := httptest.NewRequest("GET", fmt.Sprintf("/apps/installs/%d/components", parent), nil)
	response := httptest.NewRecorder()
	s.handleAppComponents(response, request, parent)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"executor"`) {
		t.Fatal("component detail endpoint missing")
	}
	request = httptest.NewRequest("DELETE", fmt.Sprintf("/apps/installs/%d?force=1", parent), nil)
	request.Header.Set("X-User-ID", "1")
	response = httptest.NewRecorder()
	s.handleUninstallApp(response, request)
	if response.Code != 409 {
		t.Fatal("forced uninstall orphaned owned resources")
	}
	token, err := s.appInstallToken(parent)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", fmt.Sprintf("/apps/installs/%d/components/executor", parent), strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { s.handleAppComponents(w, r, parent) })(response, request)
	if response.Code == 200 || response.Code == 202 {
		t.Fatal("app token administered infrastructure")
	}
	if _, err = s.store.db.Exec(`DELETE FROM app_installs WHERE id=?`, child); err != nil {
		t.Fatal(err)
	}
	if !s.hasOwnedComponents(parent) {
		return
	}
	t.Fatal("deleted child retained ownership")
}

func TestAppComponentInstallAndPermissionApproval(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.installedApps = NewInstalledAppsRegistry()
	request := httptest.NewRequest("POST", "/apps/install", strings.NewReader(runtimeJSON(map[string]any{"manifest_yaml": componentManifestFixture})))
	request.Header.Set("X-User-ID", "1")
	response := httptest.NewRecorder()
	s.handleInstallApp(response, request)
	if response.Code != 200 {
		t.Fatalf("parent install %d: %s", response.Code, response.Body.String())
	}
	var receipt struct {
		InstallID int64 `json:"install_id"`
	}
	json.Unmarshal(response.Body.Bytes(), &receipt)
	defs, err := s.componentDeclarations(receipt.InstallID)
	if err != nil || len(defs) != 1 {
		t.Fatal("component snapshot was discarded by old SDK serialization", err)
	}
	def := defs["executor"]
	def.Manifest.Requires.Permissions = []sdk.Permission{sdk.PermAppsCall}
	defs["executor"] = def
	// A new parent release can change its component; old snapshots remain immutable.
	putComponentDefinitions(t, s, receipt.InstallID, "2.0.0", defs)
	s.store.db.Exec(`UPDATE app_installs SET version='2.0.0' WHERE id=?`, receipt.InstallID)
	if _, _, err = s.ensureComponent(receipt.InstallID, "executor", nil, nil); err == nil {
		t.Fatal("component inherited permissions without approval")
	}
	id, _, err := s.ensureComponent(receipt.InstallID, "executor", map[string]string{"scratch": "isolated"}, def.Manifest.Requires.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, _, err := s.ensureComponent(receipt.InstallID, "executor", map[string]string{"scratch": "isolated"}, def.Manifest.Requires.Permissions)
	if err != nil || id != duplicate {
		t.Fatal("retry did not retain child/configuration", err)
	}
	if _, _, err = s.ensureComponent(receipt.InstallID, "executor", map[string]string{"scratch": "different"}, def.Manifest.Requires.Permissions); err == nil {
		t.Fatal("retry changed saved configuration")
	}
	if _, _, err = s.ensureComponent(receipt.InstallID, "other", nil, nil); err == nil {
		t.Fatal("undeclared component installed")
	}
}

func TestAppComponentReleaseMetadataSurvivesCacheRefreshAndUpgrade(t *testing.T) {
	s, _, _, parent, _, _ := componentRuntimeFixture(t)
	raw := strings.Replace(componentManifestFixture, "version: 1.0.0", "version: 2.0.0", 1)
	raw = strings.Replace(raw, "protocol: test.executor/v1", "protocol: test.executor/v2", 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
	defer upstream.Close()
	release, err := s.fetchAndCacheAppRelease(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A later fetch must not detach declarations from an in-flight approved
	// release. Pass the snapshot alongside the manifest rather than rediscover
	// metadata by pointer identity in a mutable cache.
	manifestCacheMu.Lock()
	delete(manifestCache, upstream.URL)
	manifestCacheMu.Unlock()
	if err = s.stageAppUpgrade(parent, release.manifest, release.components); err != nil {
		t.Fatal(err)
	}
	current, err := s.componentDeclarations(parent)
	if err != nil || current["executor"].Protocol != "test.executor/v1" {
		t.Fatal("pending parent release changed active declarations", err)
	}
	if _, err = s.store.db.Exec(`UPDATE app_installs SET version='2.0.0' WHERE id=?`, parent); err != nil {
		t.Fatal(err)
	}
	current, err = s.componentDeclarations(parent)
	if err != nil || current["executor"].Protocol != "test.executor/v2" {
		t.Fatal("committed release lost declarations", err)
	}
	project, err := s.store.CreateProject(1, "Dependency component", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.installAppFromManifest(1, release.manifest, project.ID, release.components)
	// This metadata fixture has no local executable. Even a failed dependency
	// start must retain the release declaration with the recovery installation.
	if id == 0 || err == nil {
		t.Fatalf("expected a retained installation and unavailable fixture executable: %d %v", id, err)
	}
	current, err = s.componentDeclarations(id)
	if err != nil || len(current) != 1 {
		t.Fatal("dependency installation lost component declarations", err)
	}
}

func TestAppComponentUsesExistingPackageDelivery(t *testing.T) {
	packageTestCompiler(t)
	s, m, f, p := packageTestSetup(t, "another-compute-backend")
	worker := packageTestSource(t)
	revision, err := runtimePackageGit(context.Background(), strings.TrimPrefix(worker.Runtime.Source.Repo, "file://"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	worker.Runtime.Source.Ref = revision
	parentManifest := sdk.Manifest{Name: "package-component-parent", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "service"}, Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}}}
	project, err := s.store.CreateProject(1, "Packaged component", "", "")
	if err != nil {
		t.Fatal(err)
	}
	parent := seedRunningInstall(t, s, parentManifest.Name, project.ID, parentManifest, nil)
	caps := runtimeAppCaps{PassiveStart: true, Drain: true, Routing: "key"}
	putComponentDefinitions(t, s, parent, "1.0.0", appComponentDeclarations{"executor": {Protocol: "test.executor/v1", Capabilities: caps, Manifest: worker}})
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_apteva/runtime/component/describe") {
			if r.Header.Get("Authorization") == "" {
				t.Error("missing worker credential")
			}
			writeJSON(w, map[string]string{"protocol": "test.executor/v1"})
			return
		}
		original.ServeHTTP(w, r)
	})
	child, _, err := s.ensureComponent(parent, "executor", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.begin(child, "deploy", "component-package", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op = waitPackageOperation(t, m, op); op.Status != "succeeded" {
		t.Fatal(op.Error)
	}
	d, err := m.active(child)
	if err != nil || d.Package == nil || d.Package.Metadata.AppCapabilities != caps || d.Component == nil {
		t.Fatal("component lost package or lifecycle identity", err)
	}
	if d.Package.SourceRevision != revision {
		t.Fatal("component release was not pinned")
	}
	if id, err := s.resolveComponentCall(parent, "executor"); err != nil || id != child {
		t.Fatal("packaged component route unavailable", err)
	}
}
