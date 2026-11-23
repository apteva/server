package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// seedSecondInstall adds another install of an app already seeded with
// seedRunningInstall, in projectID. The first install in a scope becomes that
// scope's default.
func seedSecondInstall(t *testing.T, s *Server, firstInstallID int64, projectID string) int64 {
	t.Helper()
	res, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by, integration_bindings, manifest_json)
		SELECT app_id, ?, 'running', 1, '{}', manifest_json FROM app_installs WHERE id=?`, projectID, firstInstallID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func requiresApp(name, dependency string) sdk.Manifest {
	return sdk.Manifest{Name: name, Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: dependency}}}}
}

func uninstallInstall(t *testing.T, s *Server, installID int64) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleUninstallApp(rec, authedRequest(t, http.MethodDelete, fmt.Sprintf("/apps/installs/%d", installID), "", nil))
	return rec
}

func newUninstallTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	return s
}

func TestUninstallIgnoresDependentsBoundToAnotherInstall(t *testing.T) {
	s := newUninstallTestServer(t)
	global := seedRunningInstall(t, s, "catalog-unbound-test", "", sdk.Manifest{Name: "catalog-unbound-test"}, nil)
	project := seedSecondInstall(t, s, global, "proj-1")
	seedRunningInstall(t, s, "saas-unbound-test", "proj-1", requiresApp("saas-unbound-test", "catalog-unbound-test"),
		map[string]any{"catalog-unbound-test": global})

	rec := uninstallInstall(t, s, project)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall of an install nothing is bound to: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var receipt struct {
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &receipt)
	if len(receipt.Warnings) != 0 {
		t.Fatalf("warnings=%v, want none: the dependent is bound to the global install", receipt.Warnings)
	}
}

func TestUninstallBlocksOnDependentsBoundToThisInstall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding func(id int64) any
		status  string
	}{
		{"single binding", func(id int64) any { return id }, "running"},
		{"ids list", func(id int64) any { return map[string]any{"ids": []any{float64(id + 1000), float64(id)}, "default": float64(id + 1000)} }, "running"},
		{"pending dependent", func(id int64) any { return id }, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUninstallTestServer(t)
			target := seedRunningInstall(t, s, "catalog-bound-test", "proj-1", sdk.Manifest{Name: "catalog-bound-test"}, nil)
			dependent := seedRunningInstall(t, s, "saas-bound-test", "proj-1", requiresApp("saas-bound-test", "catalog-bound-test"),
				map[string]any{"catalog-bound-test": tc.binding(target)})
			if _, err := s.store.db.Exec(`UPDATE app_installs SET status=? WHERE id=?`, tc.status, dependent); err != nil {
				t.Fatal(err)
			}

			rec := uninstallInstall(t, s, target)
			if rec.Code != http.StatusConflict {
				t.Fatalf("uninstall of a bound install: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "saas-bound-test") {
				t.Fatalf("409 body does not name the bound dependent: %s", rec.Body.String())
			}
			var left int
			_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE id=?`, target).Scan(&left)
			if left != 1 {
				t.Fatal("blocked uninstall still deleted the install")
			}
		})
	}
}

func TestUninstallWarnsWhenUnboundDependentLosesItsOnlyInstall(t *testing.T) {
	s := newUninstallTestServer(t)
	only := seedRunningInstall(t, s, "catalog-warn-test", "", sdk.Manifest{Name: "catalog-warn-test"}, nil)
	seedRunningInstall(t, s, "saas-warn-test", "proj-1", requiresApp("saas-warn-test", "catalog-warn-test"), nil)

	rec := uninstallInstall(t, s, only)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbound dependent should not block: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var receipt struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], "saas-warn-test") {
		t.Fatalf("warnings=%v, want one naming saas-warn-test", receipt.Warnings)
	}

	// With another running install the dependent can still use, no warning.
	s = newUninstallTestServer(t)
	global := seedRunningInstall(t, s, "catalog-warn-test", "", sdk.Manifest{Name: "catalog-warn-test"}, nil)
	project := seedSecondInstall(t, s, global, "proj-1")
	seedRunningInstall(t, s, "saas-warn-test", "proj-1", requiresApp("saas-warn-test", "catalog-warn-test"), nil)
	rec = uninstallInstall(t, s, project)
	receipt.Warnings = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &receipt)
	if rec.Code != http.StatusOK || len(receipt.Warnings) != 0 {
		t.Fatalf("status=%d warnings=%v, want 200 and none while the global install remains", rec.Code, receipt.Warnings)
	}
}

// Uninstalling a project's install must let that project's requests reach the
// global install straight away, not only after the next restart.
func TestUninstalledProjectInstallFallsBackToGlobalRoute(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	reached := map[string]int{}
	sidecar := func(label string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached[label]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	global := seedRunningInstall(t, s, "catalog-route-test", "", sdk.Manifest{Name: "catalog-route-test"}, nil)
	project := seedSecondInstall(t, s, global, projectID)
	s.installedApps.Add(&InstalledApp{InstallID: global, AppName: "catalog-route-test", SidecarURL: sidecar("global").URL})
	s.installedApps.Add(&InstalledApp{InstallID: project, AppName: "catalog-route-test", ProjectID: projectID, SidecarURL: sidecar("project").URL})
	// Routing defaults as the server loads them at boot.
	defaults, err := s.loadInstallationDefaults()
	if err != nil {
		t.Fatal(err)
	}
	s.installedApps.defaults = defaults

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/apps/catalog-route-test/mcp?project_id="+projectID,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("X-User-ID", itoa64(userID))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		return rec
	}
	if rec := call(); rec.Code != http.StatusOK || reached["project"] != 1 {
		t.Fatalf("before uninstall: status=%d reached=%v body=%s", rec.Code, reached, rec.Body.String())
	}
	if rec := uninstallInstall(t, s, project); rec.Code != http.StatusOK {
		t.Fatalf("uninstall status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call(); rec.Code != http.StatusOK || reached["global"] != 1 {
		t.Fatalf("after uninstall the project route did not reach the global install: status=%d reached=%v body=%s",
			rec.Code, reached, rec.Body.String())
	}
}

func TestAppMCPRouteErrorsAreJSONRPC(t *testing.T) {
	s, userID, projectID := newAppProxyProjectUser(t, ProjectEditor)
	post := func(app, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/apps/"+app+"/mcp?project_id="+projectID, strings.NewReader(body))
		req.Header.Set("X-User-ID", itoa64(userID))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		return rec
	}
	type rpcError struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) rpcError {
		t.Helper()
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type=%q body=%s, want application/json", ct, rec.Body.String())
		}
		var out rpcError
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
		}
		if out.JSONRPC != "2.0" || out.Error.Code != appMCPRouteErrorCode || out.Error.Message == "" {
			t.Fatalf("not a JSON-RPC error: %s", rec.Body.String())
		}
		return out
	}

	t.Run("missing app", func(t *testing.T) {
		rec := post("missing-route-app", `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		got := decode(t, rec)
		if string(got.ID) != "7" || got.Error.Message != "app not installed: missing-route-app" {
			t.Fatalf("id=%s message=%q", got.ID, got.Error.Message)
		}
	})

	t.Run("unreachable app", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		s.installedApps.Add(&InstalledApp{InstallID: 811, AppName: "unreachable-route-app", SidecarURL: gone.URL})
		rec := post("unreachable-route-app", `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		decode(t, rec)
	})

	t.Run("app responses pass through", func(t *testing.T) {
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "app's own error", http.StatusInternalServerError)
		}))
		defer app.Close()
		s.installedApps.Add(&InstalledApp{InstallID: 812, AppName: "plain-route-app", SidecarURL: app.URL})
		rec := post("plain-route-app", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != "app's own error" {
			t.Fatalf("app response was rewritten: status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("other routes keep plain text", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/apps/missing-route-app/resource?project_id="+projectID, nil)
		req.Header.Set("X-User-ID", itoa64(userID))
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("status=%d Content-Type=%q", rec.Code, rec.Header().Get("Content-Type"))
		}
	})
}
