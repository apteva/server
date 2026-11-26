package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// addPrivateInstall adds a private installation of the same app as existing,
// copying its manifest and permissions.
func addPrivateInstall(t *testing.T, s *Server, existing int64, projectID, key string) int64 {
	t.Helper()
	res, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by, installation_key, installation_name, manifest_json, permissions_json, private)
		SELECT app_id, ?, 'running', 1, ?, ?, manifest_json, permissions_json, 1 FROM app_installs WHERE id=?`, projectID, key, key, existing)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func installIsDefault(t *testing.T, s *Server, id int64) bool {
	t.Helper()
	var isDefault bool
	if err := s.store.db.QueryRow(`SELECT is_default FROM app_installs WHERE id=?`, id).Scan(&isDefault); err != nil {
		t.Fatal(err)
	}
	return isDefault
}

func TestPrivateInstallNeverBecomesDefault(t *testing.T) {
	s := newTestServer(t)
	normal := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging"}, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	if !installIsDefault(t, s, normal) || installIsDefault(t, s, private) {
		t.Fatal("private install took the default")
	}

	// A private install first in a scope does not take the default, and the
	// next ordinary install there still does.
	firstPrivate := addPrivateInstall(t, s, normal, "proj-2", "private")
	if installIsDefault(t, s, firstPrivate) {
		t.Fatal("first private install became the default")
	}
	ordinary := addInstallOf(t, s, normal, "proj-2", "default")
	if !installIsDefault(t, s, ordinary) {
		t.Fatal("ordinary install after a private one is not the default")
	}

	for name, q := range map[string]string{
		"promote private":   fmt.Sprintf(`UPDATE app_installs SET is_default=1 WHERE id=%d`, private),
		"make private":      fmt.Sprintf(`UPDATE app_installs SET private=1 WHERE id=%d`, ordinary),
		"make public":       fmt.Sprintf(`UPDATE app_installs SET private=0 WHERE id=%d`, private),
		"insert as default": `INSERT INTO app_installs (app_id, project_id, status, installation_key, private, is_default) SELECT app_id, 'proj-9', 'running', 'x', 1, 1 FROM app_installs LIMIT 1`,
	} {
		if _, err := s.store.db.Exec(q); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	// A scope holding only a private install has no default, so lookups by
	// name fall back to the global install.
	global := addInstallOf(t, s, normal, "", "default")
	addPrivateInstall(t, s, normal, "proj-3", "private")
	defaults, err := s.loadInstallationDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := defaults[appInstallScope{"messaging", "proj-3"}]; exists {
		t.Fatal("private-only scope recorded a default")
	}
	if id, ok := s.findInstalledApp("messaging", "proj-3"); !ok || id != global {
		t.Fatalf("lookup in private-only scope = %d %v, want global %d", id, ok, global)
	}
	if id, ok := s.findInstalledApp("messaging", "proj-1"); !ok || id != normal {
		t.Fatalf("lookup in proj-1 = %d %v, want %d", id, ok, normal)
	}
}

func TestPrivateInstallHiddenFromRegistryLists(t *testing.T) {
	r := NewInstalledAppsRegistry()
	r.Add(&InstalledApp{InstallID: 1, AppName: "messaging", ProjectID: "proj-1"})
	r.Add(&InstalledApp{InstallID: 2, AppName: "messaging", ProjectID: "proj-1", Private: true})
	if got := r.List(); len(got) != 1 || got[0].InstallID != 1 {
		t.Fatalf("List = %v", got)
	}
	if got := r.ListForProject("proj-1"); len(got) != 1 || got[0].InstallID != 1 {
		t.Fatalf("ListForProject = %v", got)
	}
	if got := r.ListAll(); len(got) != 2 {
		t.Fatalf("ListAll = %d entries", len(got))
	}
	if r.Get(2) == nil {
		t.Fatal("Get by id lost the private install")
	}
	if got := r.GetByName("messaging"); got == nil || got.InstallID != 1 {
		t.Fatalf("GetByName = %v", got)
	}
	if got := r.GetByNameAndProject("messaging", "proj-1"); got == nil || got.InstallID != 1 {
		t.Fatalf("GetByNameAndProject = %v", got)
	}
	r.Remove(1)
	if got := r.GetByNameAndProject("messaging", "proj-1"); got != nil {
		t.Fatalf("name lookup reached the private install: %d", got.InstallID)
	}
	if got := r.GetByNameAndProjectExact("messaging", "proj-1"); got != nil {
		t.Fatalf("exact name lookup reached the private install: %d", got.InstallID)
	}
	if got := r.GetByName("messaging"); got != nil {
		t.Fatalf("legacy name index holds the private install: %d", got.InstallID)
	}
}

func TestPrivateInstallHiddenFromAppList(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	ensureTestAdmin(t, s)
	normal := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging"}, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	for _, path := range []string{"/apps?project_id=proj-1", "/apps"} {
		w := httptest.NewRecorder()
		s.handleListApps(w, authedRequest(t, "GET", path, "", nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var rows []AppRow
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		seen := map[int64]bool{}
		for _, row := range rows {
			seen[row.InstallID] = true
		}
		if !seen[normal] || seen[private] {
			t.Fatalf("%s listed %v", path, seen)
		}
	}
}

func TestPrivateInstallCallRule(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	ensureTestAdmin(t, s)
	seedProject(t, s, "proj-1")
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members (project_id, user_id, role) VALUES ('proj-1', 1, 'owner')`); err != nil {
		t.Fatal(err)
	}
	caller := seedRunningInstall(t, s, "functions", "", dynamicCallerManifest("functions"), nil)
	other := seedRunningInstall(t, s, "runner", "", dynamicCallerManifest("runner"), nil)
	normal := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging"}, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	privateCaller := addPrivateInstall(t, s, caller, "proj-1", "private")
	for _, inst := range []*InstalledApp{
		{InstallID: normal, AppName: "messaging", ProjectID: "proj-1", SidecarURL: "http://127.0.0.1:1"},
		{InstallID: private, AppName: "messaging", ProjectID: "proj-1", SidecarURL: "http://127.0.0.1:1", Private: true},
	} {
		s.installedApps.Add(inst)
	}
	call := func(from int64, selected ...int64) (*appCallTarget, *httptest.ResponseRecorder) {
		t.Helper()
		w := httptest.NewRecorder()
		target, _ := s.authorizeAppCallback(w, from, "messaging", "proj-1", selected...)
		return target, w
	}

	if _, w := call(caller, private); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "does not allow") {
		t.Fatalf("call before allowed: %d %s", w.Code, w.Body.String())
	}
	if target, w := call(caller); target == nil || target.targetInstallID != normal {
		t.Fatalf("call by name: %d %s", w.Code, w.Body.String())
	}
	if _, status, err := s.setPrivateInstallCallers(private, []int64{caller}); err != nil {
		t.Fatal(status, err)
	}
	if target, w := call(caller, private); target == nil || target.targetInstallID != private {
		t.Fatalf("allowed call: %d %s", w.Code, w.Body.String())
	}
	if target, w := call(caller); target == nil || target.targetInstallID != normal {
		t.Fatalf("call by name after allow: %d %s", w.Code, w.Body.String())
	}
	if _, w := call(other, private); w.Code != http.StatusForbidden {
		t.Fatalf("other caller: %d %s", w.Code, w.Body.String())
	}

	// A private installation reaches only private installations that allow it.
	if _, w := call(privateCaller, normal); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only private") {
		t.Fatalf("private to ordinary: %d %s", w.Code, w.Body.String())
	}
	if _, w := call(privateCaller); w.Code < 400 {
		t.Fatalf("private by name reached an ordinary install: %d %s", w.Code, w.Body.String())
	}
	if _, status, err := s.setPrivateInstallCallers(private, []int64{caller, privateCaller}); err != nil {
		t.Fatal(status, err)
	}
	if target, w := call(privateCaller, private); target == nil || target.targetInstallID != private {
		t.Fatalf("private to allowing private: %d %s", w.Code, w.Body.String())
	}
}

func TestPrivateInstallProxyServesOnlyPublicRoutes(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	ensureTestAdmin(t, s)
	manifest := sdk.Manifest{Name: "messaging"}
	manifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: "/webhooks/", NoAuth: true}}
	normal := seedRunningInstall(t, s, "messaging", "proj-1", manifest, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(sidecar.Close)
	s.installedApps.Add(&InstalledApp{InstallID: normal, AppName: "messaging", ProjectID: "proj-1", SidecarURL: sidecar.URL, Manifest: manifest})
	s.installedApps.Add(&InstalledApp{InstallID: private, AppName: "messaging", ProjectID: "proj-1", SidecarURL: sidecar.URL, Manifest: manifest, Private: true})

	proxy := func(method, path string, header map[string]string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.handleAppProxy(w, r)
		if w.Code == http.StatusForbidden && !strings.Contains(w.Body.String(), "private installation") {
			t.Fatalf("%s refused for another reason: %s", path, w.Body.String())
		}
		return w.Code
	}
	admin := map[string]string{"X-User-ID": "1"}
	if code := proxy("POST", fmt.Sprintf("/apps/messaging/_install/%d/webhooks/ses", private), nil); code != 200 {
		t.Fatalf("public webhook route: %d", code)
	}
	if code := proxy("GET", fmt.Sprintf("/apps/messaging/_install/%d/messages", private), admin); code != http.StatusForbidden {
		t.Fatalf("private route by a user: %d", code)
	}
	if code := proxy("GET", fmt.Sprintf("/apps/messaging/messages?project_id=proj-1&install_id=%d", private), admin); code != http.StatusForbidden {
		t.Fatalf("private route by install_id query: %d", code)
	}
	if code := proxy("GET", fmt.Sprintf("/apps/messaging/_install/%d/messages", private), map[string]string{"X-Apteva-App-Install-ID": fmt.Sprint(private)}); code != 200 {
		t.Fatalf("own credential: %d", code)
	}
	if code := proxy("GET", "/apps/messaging/messages?project_id=proj-1", admin); code != 200 {
		t.Fatalf("ordinary install by name: %d", code)
	}
}

func TestPrivateInstallEventsReachOnlyAllowedApps(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	source := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging"}, nil)
	private := addPrivateInstall(t, s, source, "proj-1", "private")
	subscriber := sdk.Manifest{Name: "functions", Requires: sdk.Requires{Apps: []sdk.RequiredAppRef{{Name: "messaging", Events: []string{"*"}}}}}
	allowed := seedRunningInstall(t, s, "functions", "", subscriber, nil)
	subscriber.Name = "crm"
	outsider := seedRunningInstall(t, s, "crm", "", subscriber, nil)

	var mu sync.Mutex
	hits := map[int64]int{}
	for _, id := range []int64{allowed, outsider} {
		id := id
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[id]++
			mu.Unlock()
		}))
		t.Cleanup(srv.Close)
		m := subscriber
		s.installedApps.Add(&InstalledApp{InstallID: id, AppName: fmt.Sprint(id), SidecarURL: srv.URL, Manifest: m})
	}
	s.dispatchPrivateAppEvent(AppEvent{App: "messaging", ProjectID: "proj-1", InstallID: private, Topic: "message.received", Data: []byte(`{}`)}, []int64{allowed})
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := hits[allowed]
		mu.Unlock()
		if got == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if hits[allowed] != 1 || hits[outsider] != 0 {
		t.Fatalf("deliveries %v", hits)
	}
}

func TestPrivateInstallHiddenFromAgentGateway(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	normal := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging"}, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	gateway := func(path string) *http.Request {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-User-ID", "1")
		r.Header.Set("X-Apteva-MCP-User-ID", "1")
		r.Header.Set("X-Agent-Secret", "test-secret")
		return r
	}
	w := httptest.NewRecorder()
	if !s.hidePrivateInstallFromGateway(w, gateway("/apps/installs/1"), private) || w.Code != http.StatusNotFound {
		t.Fatalf("gateway reached private install: %d", w.Code)
	}
	if s.hidePrivateInstallFromGateway(httptest.NewRecorder(), gateway("/apps/installs/1"), normal) {
		t.Fatal("gateway hidden from ordinary install")
	}
	dashboard := httptest.NewRequest("GET", "/apps/installs/1", nil)
	dashboard.Header.Set("X-User-ID", "1")
	if s.hidePrivateInstallFromGateway(httptest.NewRecorder(), dashboard, private) {
		t.Fatal("dashboard hidden from private install")
	}
	w = httptest.NewRecorder()
	s.handlePrivateInstalls(w, gateway("/apps/private"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("gateway listed private installs: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleInstallApp(w, func() *http.Request {
		r := gateway("/apps/install")
		r.Method = http.MethodPost
		r.Body = io.NopCloser(strings.NewReader(`{"name":"messaging","project_id":"proj-1","private":true}`))
		return r
	}())
	if w.Code != http.StatusForbidden {
		t.Fatalf("gateway created a private install: %d %s", w.Code, w.Body.String())
	}
	if !isAppManagementRoute("private") {
		t.Fatal("app tokens can reach the private installs route")
	}
}

func TestPrivateInstallCallersEndpoint(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	if _, err := s.store.db.Exec(`INSERT INTO users (id,email,password_hash,role) VALUES (2,'member@test.local','hash','user')`); err != nil {
		t.Fatal(err)
	}
	caller := seedRunningInstall(t, s, "functions", "", dynamicCallerManifest("functions"), nil)
	normal := seedRunningInstall(t, s, "messaging", "proj-1", sdk.Manifest{Name: "messaging", DisplayName: "Messaging"}, nil)
	private := addPrivateInstall(t, s, normal, "proj-1", "private")
	request := func(user, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-User-ID", user)
		w := httptest.NewRecorder()
		s.handlePrivateInstalls(w, r)
		return w
	}
	put := func(user string, id int64, body string) *httptest.ResponseRecorder {
		return request(user, "PUT", fmt.Sprintf("/apps/private/%d/callers", id), body)
	}

	if w := put("2", private, `{"caller_install_ids":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", w.Code)
	}
	if w := put("1", normal, `{"caller_install_ids":[]}`); w.Code != http.StatusConflict {
		t.Fatalf("ordinary install: %d", w.Code)
	}
	if w := put("1", private, fmt.Sprintf(`{"caller_install_ids":[%d]}`, private)); w.Code != http.StatusBadRequest {
		t.Fatalf("self caller: %d", w.Code)
	}
	if w := put("1", private, `{"caller_install_ids":[99999]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown caller: %d", w.Code)
	}
	w := put("1", private, fmt.Sprintf(`{"caller_install_ids":[%d,%d]}`, caller, caller))
	if w.Code != 200 {
		t.Fatalf("set callers: %d %s", w.Code, w.Body.String())
	}
	var view privateInstallView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Callers) != 1 || view.Callers[0].InstallID != caller || view.Callers[0].App != "functions" {
		t.Fatalf("callers %+v", view.Callers)
	}

	w = request("1", "GET", "/apps/private", "")
	var views []privateInstallView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil || w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if len(views) != 1 || views[0].InstallID != private || views[0].DisplayName != "Messaging" || len(views[0].Callers) != 1 {
		t.Fatalf("list %+v", views)
	}
}
