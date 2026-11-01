package main

// App-to-app calls reach the target with the project the server authorized,
// so SDK apps that require Caller.ProjectID (Database, for one) accept them.
// Operator identity stays limited to calls made in an operator's session.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

type appCallHeaders struct{ path, project, operator string }

type appCallHeaderFixture struct {
	s        *Server
	caller   int64
	target   int64
	mu       sync.Mutex
	received []appCallHeaders
}

// newAppCallHeaderFixture binds a caller app to a target app, both installed
// in proj-1 or, with global, both global. fast makes the target accept the
// internal batch transport.
func newAppCallHeaderFixture(t *testing.T, fast, global bool) *appCallHeaderFixture {
	t.Helper()
	f := &appCallHeaderFixture{s: newTestServer(t)}
	f.s.installedApps = NewInstalledAppsRegistry()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sdk.InternalAppBatchPath {
			if !fast {
				http.NotFound(w, r)
				return
			}
			w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		f.mu.Lock()
		f.received = append(f.received, appCallHeaders{r.URL.Path, r.Header.Get("X-Apteva-Project-ID"), r.Header.Get("X-Apteva-Operator-ID")})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == sdk.InternalAppBatchPath {
			var request sdk.InternalAppBatchRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			results := make([]sdk.AppCallResult, len(request.Calls))
			for i, call := range request.Calls {
				results[i] = sdk.AppCallResult{ID: call.ID, Status: 200, Format: "json", Result: json.RawMessage(`{}`)}
			}
			_ = json.NewEncoder(w).Encode(sdk.InternalAppBatchResponse{Results: results})
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`)
	}))
	t.Cleanup(target.Close)
	if fast {
		t.Setenv("APTEVA_APP_TOKEN", "target-token")
	}

	project := "proj-1"
	if global {
		project = ""
	}
	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "header-target"}
	f.target = seedInstallWithBindings(t, f.s, "header-target", targetManifest, nil)
	f.s.installedApps.Add(&InstalledApp{
		InstallID: f.target, AppName: "header-target", ProjectID: project,
		Manifest: targetManifest, SidecarURL: target.URL, Token: "target-token",
	})
	callerManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent, Name: "header-caller",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "header-target"}},
		},
	}
	f.caller = seedInstallWithBindings(t, f.s, "header-caller", callerManifest, map[string]any{"header-target": f.target})
	if _, err := f.s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id IN (?,?)`, project, f.caller, f.target); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *appCallHeaderFixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/header-target/"+path, strings.NewReader(body))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(f.caller))
	rec := httptest.NewRecorder()
	f.s.handleAppCallback(rec, req)
	return rec
}

func (f *appCallHeaderFixture) seen() []appCallHeaders {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appCallHeaders(nil), f.received...)
}

func TestAppCallsForwardTheAuthorizedProject(t *testing.T) {
	batch := `{"calls":[{"id":"one","tool":"db_query","input":{}},{"id":"two","tool":"db_query","input":{}}]}`
	for _, tc := range []struct {
		name, path, body string
		fast             bool
		wantPath         string
		wantCalls        int
	}{
		{"single call", "call", `{"tool":"db_query","input":{}}`, false, "/mcp", 1},
		{"batch to an older app", "batch", batch, false, "/mcp", 2},
		{"internal batch", "batch", batch, true, sdk.InternalAppBatchPath, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppCallHeaderFixture(t, tc.fast, false)
			if rec := f.post(t, tc.path, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			got := f.seen()
			if len(got) != tc.wantCalls {
				t.Fatalf("target requests=%+v, want %d", got, tc.wantCalls)
			}
			for _, h := range got {
				if h.path != tc.wantPath || h.project != "proj-1" {
					t.Errorf("target got %+v, want project proj-1 on %s", h, tc.wantPath)
				}
				if h.operator != "" {
					t.Errorf("plain app call gained an operator: %+v", h)
				}
			}
		})
	}
}

func TestAppCallProjectHeaderFollowsAuthorization(t *testing.T) {
	// A project-scoped caller naming another project is refused before the
	// target sees anything.
	f := newAppCallHeaderFixture(t, false, false)
	if rec := f.post(t, "call", `{"tool":"db_query","input":{"_project_id":"proj-2"}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("refused call reached the target: %+v", got)
	}

	// A global caller without a project sends none; delegating to a project
	// its owner can see sends that project.
	f = newAppCallHeaderFixture(t, false, true)
	if rec := f.post(t, "call", `{"tool":"db_query","input":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("global status=%d body=%s", rec.Code, rec.Body.String())
	}
	visible, err := f.s.store.CreateProject(1, "Visible", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, "call", `{"tool":"db_query","input":{"_project_id":"`+visible.ID+`"}}`); rec.Code != http.StatusOK {
		t.Fatalf("delegated status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := f.seen()
	if len(got) != 2 || got[0].project != "" || got[1].project != visible.ID {
		t.Fatalf("target projects=%+v, want none then %s", got, visible.ID)
	}
}
