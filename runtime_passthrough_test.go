package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// passthroughFixture is a runtime with one app (the caller) whose source
// project has a "catalog" app with a read-only and a writing tool.
type passthroughFixture struct {
	s         *Server
	runtime   *Environment
	callerID  int64
	catalogID int64
	sidecar   *httptest.Server
	hits      atomic.Int64
	lastArgs  atomic.Value
}

func newPassthroughFixture(t *testing.T, mode EdgeMode) *passthroughFixture {
	t.Helper()
	f := &passthroughFixture{s: newRuntimeAPITestServer(t)}
	s := f.s
	if s.appBus == nil {
		s.appBus = NewAppEventBus()
	}
	f.sidecar = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.Method == "tools/list" {
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{
				map[string]any{"name": "catalog_get", "annotations": map[string]any{"readOnlyHint": true}},
				map[string]any{"name": "catalog_put"},
			}}})
			return
		}
		f.hits.Add(1)
		f.lastArgs.Store(rpc.Params.Arguments)
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": `{"sku":"A1","price":12}`}},
		}})
	}))
	t.Cleanup(f.sidecar.Close)

	catalogManifest := sdk.Manifest{Name: "catalog", Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{
		{Name: "catalog_get", ReadOnly: true},
		{Name: "catalog_put"},
	}}}
	f.catalogID = seedInstallWithBindings(t, s, "catalog", catalogManifest, nil)
	s.installedApps.Add(&InstalledApp{InstallID: f.catalogID, AppName: "catalog", ProjectID: "proj-1", Manifest: catalogManifest, SidecarURL: f.sidecar.URL, Token: "catalog-token"})
	s.installedApps.Add(&InstalledApp{InstallID: 999, AppName: "billing", ProjectID: "proj-1", SidecarURL: f.sidecar.URL})

	// The caller is an environment install, as installLocalSource creates it.
	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, manifest_json) VALUES ('shop', 'environment', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	perms, _ := json.Marshal([]sdk.Permission{sdk.PermAppsCall})
	res, err = s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by, permissions_json, source, manifest_json)
		VALUES (?, 'rt-pass', 'running', 1, ?, 'environment', '{"name":"shop"}')`, appID, string(perms))
	if err != nil {
		t.Fatal(err)
	}
	f.callerID, _ = res.LastInsertId()

	edge, err := startEnvironmentEdge(SandboxPolicy{}, mode, newCassette())
	if err != nil {
		t.Fatal(err)
	}
	clock, _ := newEnvironmentClock(nil)
	f.runtime = &Environment{
		ID: "rt-pass", ProjectID: "proj-1", creatorUserID: 1, edge: edge, clock: clock,
		installs:  map[string]*localInstall{"shop": {InstallID: f.callerID, AppName: "shop", ProjectID: "rt-pass"}},
		agents:    map[int64]*EnvironmentAgent{},
		appGrants: []sdk.RuntimeAppGrant{{App: "catalog", Tool: "catalog_get", Arguments: map[string]any{"sku": "A1"}}, {App: "catalog", Tool: "catalog_put"}},
		createdAt: time.Now(),
	}
	s.environments.mu.Lock()
	s.environments.environments[f.runtime.ID] = f.runtime
	s.environments.mu.Unlock()
	t.Cleanup(edge.Stop)
	return f
}

func (f *passthroughFixture) call(t *testing.T, app, tool string, input map[string]any, targetInstallID int64) *httptest.ResponseRecorder {
	t.Helper()
	return runtimeAPIRequest(t, f.s, f.callerID, http.MethodPost, "/apps/callback/apps/"+app+"/call",
		map[string]any{"tool": tool, "input": input, "target_install_id": targetInstallID})
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error
}

func TestRuntimePassthroughForwardsOnlyGrantedReadOnlyCalls(t *testing.T) {
	f := newPassthroughFixture(t, EdgeRecord)
	events, _, unsubscribe := f.s.appBus.Subscribe("runtime", "rt-pass", 0)
	defer unsubscribe()

	rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1", "_project_id": "rt-pass"}, 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `\"price\":12`) {
		t.Fatalf("granted read: %d %s", rec.Code, rec.Body.String())
	}
	if args, _ := f.lastArgs.Load().(map[string]any); args["_project_id"] != "proj-1" || args["sku"] != "A1" {
		t.Fatalf("forwarded arguments %v", args)
	}
	select {
	case ev := <-events:
		if ev.Topic != "runtime.passthrough" || !strings.Contains(string(ev.Data), `"tool":"catalog_get"`) {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no runtime.passthrough event")
	}
	if calls := f.runtime.Edge().Calls(); len(calls) != 1 || calls[0].Host != "app:catalog" || !calls[0].Recorded {
		t.Fatalf("edge calls %+v", calls)
	}

	for name, tc := range map[string]struct {
		app, tool string
		input     map[string]any
		target    int64
		code      string
	}{
		"other arguments":    {"catalog", "catalog_get", map[string]any{"sku": "B2"}, 0, sdk.RuntimeErrAppNotInRuntime},
		"missing argument":   {"catalog", "catalog_get", map[string]any{}, 0, sdk.RuntimeErrAppNotInRuntime},
		"granted but writes": {"catalog", "catalog_put", map[string]any{"sku": "A1"}, 0, "passthrough_not_read_only"},
		"app not granted":    {"billing", "billing_list", map[string]any{}, 0, sdk.RuntimeErrAppNotInRuntime},
		"selected outside":   {"catalog", "catalog_get", map[string]any{"sku": "A1"}, f.catalogID, sdk.RuntimeErrAppNotInRuntime},
	} {
		before := f.hits.Load()
		rec := f.call(t, tc.app, tc.tool, tc.input, tc.target)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if f.hits.Load() != before {
			t.Errorf("%s reached the source app", name)
		}
	}

	// A grant does not stand in for the caller's own app-call permission.
	if _, err := f.s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, f.callerID); err != nil {
		t.Fatal(err)
	}
	before := f.hits.Load()
	if rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1"}, 0); rec.Code != http.StatusForbidden || f.hits.Load() != before {
		t.Fatalf("call without %s: %d %s", sdk.PermAppsCall, rec.Code, rec.Body.String())
	}
	perms, _ := json.Marshal([]sdk.Permission{sdk.PermAppsCall})
	if _, err := f.s.store.db.Exec(`UPDATE app_installs SET permissions_json=? WHERE id=?`, string(perms), f.callerID); err != nil {
		t.Fatal(err)
	}

	// Access is checked on every call, not when the runtime was created.
	if _, err := f.s.store.db.Exec(`DELETE FROM project_members WHERE project_id='proj-1' AND user_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (2, 'other@test.local', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.db.Exec(`UPDATE projects SET user_id=2 WHERE id='proj-1'`); err != nil {
		t.Fatal(err)
	}
	if rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1"}, 0); rec.Code != http.StatusForbidden {
		t.Fatalf("call after the creator lost access: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRuntimePassthroughReplaysFromCassette(t *testing.T) {
	f := newPassthroughFixture(t, EdgeRecord)
	if rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1"}, 0); rec.Code != http.StatusOK {
		t.Fatalf("record: %d %s", rec.Code, rec.Body.String())
	}
	recorded := f.runtime.Edge().Cassette()
	f.runtime.Edge().Stop()
	edge, err := startEnvironmentEdge(SandboxPolicy{}, EdgeReplay, recorded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(edge.Stop)
	f.runtime.edge = edge
	f.sidecar.Close() // replay must not need the source app

	rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1"}, 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `\"price\":12`) {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	f.runtime.appGrants = append(f.runtime.appGrants, sdk.RuntimeAppGrant{App: "catalog", Tool: "catalog_get"})
	if rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "Z9"}, 0); rec.Code != http.StatusBadGateway {
		t.Fatalf("replay miss: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRuntimeCallBoundaryAppliesWhileCreating(t *testing.T) {
	f := newPassthroughFixture(t, EdgeBlock)
	// Unregister the runtime: its installs exist, the live list does not
	// know them yet. Calls out of it are still refused.
	f.s.environments.mu.Lock()
	delete(f.s.environments.environments, f.runtime.ID)
	f.s.environments.mu.Unlock()
	rec := f.call(t, "catalog", "catalog_get", map[string]any{"sku": "A1"}, 0)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != sdk.RuntimeErrAppNotInRuntime || f.hits.Load() != 0 {
		t.Fatalf("call during create: %d %s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "catalog") {
		t.Fatalf("error does not name the app: %s", body)
	}
}

func TestValidateRuntimeAppGrants(t *testing.T) {
	for name, grants := range map[string][]sdk.RuntimeAppGrant{
		"no tool":        {{App: "catalog"}},
		"component":      {{App: "component:x", Tool: "t"}},
		"fixed project":  {{App: "catalog", Tool: "t", Arguments: map[string]any{"_project_id": "other"}}},
		"huge arguments": {{App: "catalog", Tool: "t", Arguments: map[string]any{"x": strings.Repeat("a", maxRuntimeAppGrantArgsBytes)}}},
	} {
		if _, err := validateRuntimeAppGrants(grants); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if !fixedArgumentsMatch(map[string]any{"n": 3, "tags": []any{"a"}}, map[string]any{"n": 3.0, "tags": []any{"a"}, "extra": true}) {
		t.Fatal("equal JSON values did not match")
	}
}
