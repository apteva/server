package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestCallback_ProjectScopedMultiBindingPreservesOtherProjects(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users(id,email,password_hash,role) VALUES(1,'owner@test.local','x','user'),(2,'other@test.local','x','user')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE users SET role='user' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	project := func(owner int64, name string) string {
		t.Helper()
		p, err := s.store.CreateProject(owner, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	apteva := project(1, "Apteva")
	content := project(1, "Content Sites")
	saas := project(1, "SaaS")
	other := project(2, "Not owned")
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "test-runner"}
	defaultID := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id=?`, apteva, defaultID); err != nil {
		t.Fatal(err)
	}
	addTarget := func(projectID string) int64 {
		t.Helper()
		res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,installed_by,integration_bindings,permissions_json) SELECT app_id,?,'running',1,'{}','[]' FROM app_installs WHERE id=?`, projectID, defaultID)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	contentID, saasID := addTarget(content), addTarget(saas)
	for _, target := range []struct {
		id      int64
		project string
	}{{defaultID, apteva}, {contentID, content}, {saasID, saas}} {
		s.installedApps.Add(&InstalledApp{InstallID: target.id, AppName: manifest.Name, ProjectID: target.project, Manifest: manifest, SidecarURL: "http://unused.invalid", Token: "test-token"})
	}
	callerManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "test-global-code", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: manifest.Name}}}}
	callerID := seedInstallWithBindings(t, s, callerManifest.Name, callerManifest, map[string]any{manifest.Name: defaultID})
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, callerID); err != nil {
		t.Fatal(err)
	}
	setBinding := func(binding any) {
		t.Helper()
		b, err := json.Marshal(map[string]any{manifest.Name: binding})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, string(b), callerID); err != nil {
			t.Fatal(err)
		}
	}
	check := func(projectID string, wantID int64, wantStatus int) {
		t.Helper()
		rec := httptest.NewRecorder()
		target, ok := s.authorizeAppCallback(rec, callerID, manifest.Name, projectID)
		if wantStatus != http.StatusOK {
			if ok || rec.Code != wantStatus {
				t.Fatalf("project %q: ok=%v status=%d body=%s", projectID, ok, rec.Code, rec.Body.String())
			}
			return
		}
		if !ok || target.targetInstallID != wantID {
			t.Fatalf("project %q: target=%+v status=%d body=%s", projectID, target, rec.Code, rec.Body.String())
		}
	}
	// Baseline: the scalar binding permits Apteva but does not discover the
	// installed Content Sites or SaaS runners implicitly.
	check(apteva, defaultID, http.StatusOK)
	check(content, 0, http.StatusForbidden)
	check(saas, 0, http.StatusForbidden)
	setBinding(map[string]any{"ids": []int64{defaultID, contentID}, "default_id": defaultID})
	check(content, contentID, http.StatusOK)
	check(apteva, defaultID, http.StatusOK)
	check("", defaultID, http.StatusOK)
	check(saas, 0, http.StatusForbidden)
	check(other, 0, http.StatusForbidden)
	// A stopped project runner must not become a cross-project fallback.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='stopped' WHERE id=?`, contentID); err != nil {
		t.Fatal(err)
	}
	check(content, 0, http.StatusServiceUnavailable)
	check(apteva, defaultID, http.StatusOK)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='running' WHERE id=?`, contentID); err != nil {
		t.Fatal(err)
	}
	// Single calls, batches, and streaming proxies share the scoped selection.
	for _, suffix := range []string{"call", "batch", "proxy/resource"} {
		t.Run(suffix, func(t *testing.T) {
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resource" {
					if r.URL.Query().Get("project_id") != content {
						t.Errorf("wrong proxy project: %s", r.URL.RawQuery)
					}
					_, _ = w.Write([]byte("content-runner"))
					return
				}
				if r.URL.Path != "/mcp" {
					http.NotFound(w, r)
					return
				}
				var req struct {
					Params struct {
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Params.Arguments["_project_id"] != content {
					t.Errorf("wrong forwarded project: %v", req.Params.Arguments)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"content-runner"}]}}`))
			}))
			defer mock.Close()
			s.installedApps.Get(contentID).SidecarURL = mock.URL
			body := `{"tool":"check","input":{"_project_id":"` + content + `"}}`
			if suffix == "batch" {
				body = `{"calls":[{"id":"check","tool":"check","input":{"_project_id":"` + content + `"}}]}`
			}
			req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/"+manifest.Name+"/"+suffix, strings.NewReader(body))
			if suffix == "proxy/resource" {
				req = httptest.NewRequest(http.MethodGet, "/apps/callback/apps/"+manifest.Name+"/"+suffix+"?project_id="+content, nil)
			}
			req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
			rec := httptest.NewRecorder()
			s.handleAppCallback(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "content-runner") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	// Scoped callers cannot use this feature to delegate another project.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id=? WHERE id=?`, apteva, callerID); err != nil {
		t.Fatal(err)
	}
	check(content, 0, http.StatusForbidden)
	check(apteva, defaultID, http.StatusOK)
}
