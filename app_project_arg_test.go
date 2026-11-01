package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestProjectArgConflictOnlyRefusesOtherProjects(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	home, err := s.store.CreateProject(1, "Home", "", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateProject(1, "Other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args    map[string]any
		project string
		refused bool
	}{
		{map[string]any{}, home.ID, false},
		{map[string]any{"project_id": home.ID}, home.ID, false},
		{map[string]any{"project_id": other.ID}, home.ID, true},
		{map[string]any{"project_id": " " + other.ID + " "}, home.ID, true},
		// Not an Apteva project id: the app's own meaning, such as a project
		// on a remote tenant.
		{map[string]any{"project_id": "tenant-project-7"}, home.ID, false},
		{map[string]any{"project_id": "Other"}, home.ID, false},
		{map[string]any{"project_id": 42}, home.ID, false},
		// Global callers choose their project.
		{map[string]any{"project_id": other.ID}, "", false},
	} {
		err := s.projectArgConflict(tc.args, tc.project)
		if (err != nil) != tc.refused {
			t.Fatalf("args=%v project=%q err=%v", tc.args, tc.project, err)
		}
		if err != nil && strings.Contains(err.Error(), "Other") {
			t.Fatalf("refusal named the other project: %v", err)
		}
	}
}

func TestAgentCannotNameAnotherProjectWithoutGrant(t *testing.T) {
	f := newGrantFixture(t)
	rec := f.call(t, true, "tools/call", "invoices_list", map[string]any{"project_id": f.shopA})
	if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), "this call runs in project Management") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if len(f.calls) != 0 {
		t.Fatalf("refused call reached the app: %+v", f.lastCall(t))
	}
	f.call(t, true, "tools/call", "invoices_list", map[string]any{"project_id": f.home})
	if got := f.lastCall(t); got.project != f.home || got.args["project_id"] != f.home {
		t.Fatalf("own project refused or changed: %+v", got)
	}
}

func TestAppCallsCannotNameAnotherProject(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	// Test installs belong to "proj-1"; Other is a real project elsewhere.
	other, err := s.store.CreateProject(1, "Other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sdk.InternalAppBatchPath {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`)
	}))
	defer target.Close()
	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "arg-target"}
	targetID := seedInstallWithBindings(t, s, "arg-target", targetManifest, nil)
	s.installedApps.Add(&InstalledApp{
		InstallID: targetID, AppName: "arg-target", ProjectID: "proj-1",
		Manifest: targetManifest, SidecarURL: target.URL, Token: "target-token",
	})
	callerManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "arg-caller",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "arg-target"}},
		},
	}
	callerID := seedInstallWithBindings(t, s, "arg-caller", callerManifest, map[string]any{"arg-target": targetID})
	send := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/arg-target/"+path, strings.NewReader(body))
		req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
		rec := httptest.NewRecorder()
		s.handleAppCallback(rec, req)
		return rec
	}

	if rec := send("call", `{"tool":"first","input":{"project_id":"`+other.ID+`"}}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "is another project") {
		t.Fatalf("call naming another project: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := send("batch", `{"calls":[{"id":"a","tool":"first","input":{}},{"id":"b","tool":"first","input":{"project_id":"`+other.ID+`"}}]}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "calls[1]: project_id") {
		t.Fatalf("batch naming another project: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("refused calls reached the app: %d", calls.Load())
	}
	for _, body := range []string{
		`{"tool":"first","input":{"project_id":"proj-1"}}`,
		`{"tool":"first","input":{"project_id":"tenant-project-7"}}`,
	} {
		if rec := send("call", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}
