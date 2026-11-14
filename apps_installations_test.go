package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestNamedInstallationsMigrateLegacyAndPreserveReferences(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE apps(id INTEGER PRIMARY KEY,name TEXT)`,
		`INSERT INTO apps VALUES(1,'database')`,
		`CREATE TABLE app_installs(id INTEGER PRIMARY KEY AUTOINCREMENT,app_id INTEGER REFERENCES apps(id),project_id TEXT DEFAULT '',version TEXT,app_token_hash TEXT,extra TEXT,UNIQUE(app_id,project_id))`,
		`INSERT INTO app_installs VALUES(10,1,'p','1.0.0','keep-token','keep-column')`,
		`CREATE TABLE binding(install_id INTEGER REFERENCES app_installs(id) ON DELETE CASCADE)`,
		`INSERT INTO binding VALUES(10)`,
		`CREATE INDEX preserved_install_version ON app_installs(version)`,
		`UPDATE sqlite_sequence SET seq=200 WHERE name='app_installs'`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db}
	if err = store.migrateAppInstallations(); err != nil {
		t.Fatal(err)
	}
	var key, token, extra string
	var isDefault bool
	if err = db.QueryRow(`SELECT installation_key,app_token_hash,extra,is_default FROM app_installs WHERE id=10`).Scan(&key, &token, &extra, &isDefault); err != nil || key != "default" || token != "keep-token" || extra != "keep-column" || !isDefault {
		t.Fatalf("legacy row changed: %s %s %s %v %v", key, token, extra, isDefault, err)
	}
	res, err := db.Exec(`INSERT INTO app_installs(app_id,project_id,version,installation_key) VALUES(1,'p','2.0.0','candidate')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if id <= 200 {
		t.Fatalf("sequence lost: %d", id)
	}
	if err = store.migrateAppInstallations(); err != nil {
		t.Fatal(err)
	}
	var total, defaults, bindings int
	db.QueryRow(`SELECT COUNT(*),SUM(is_default) FROM app_installs`).Scan(&total, &defaults)
	db.QueryRow(`SELECT COUNT(*) FROM binding WHERE install_id=10`).Scan(&bindings)
	if total != 2 || defaults != 1 || bindings != 1 {
		t.Fatalf("rows/defaults/references %d/%d/%d", total, defaults, bindings)
	}
	if _, err = db.Exec(`INSERT INTO app_installs(app_id,project_id,installation_key) VALUES(1,'p','candidate')`); err == nil {
		t.Fatal("duplicate installation key accepted")
	}
	if _, err = db.Exec(`UPDATE app_installs SET is_default=1 WHERE id=?`, id); err == nil {
		t.Fatal("two defaults accepted")
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign keys damaged")
	}
	rows.Close()
	var exists int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='preserved_install_version'`).Scan(&exists)
	if exists != 1 {
		t.Fatal("index lost")
	}
}

func TestNamedInstallationsRoutingPromotionAndAgentAttachments(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	stable := seedAppWithTools(t, s, "named-database", "proj-1", []string{"db_get"})
	var appID int64
	s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, stable).Scan(&appID)
	add := func(key, project, version string) int64 {
		t.Helper()
		r, e := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,installed_by,version,installation_key,installation_name) VALUES(?,?,'running',1,?,?,?)`, appID, project, version, key, key)
		if e != nil {
			t.Fatal(e)
		}
		id, _ := r.LastInsertId()
		return id
	}
	candidate := add("candidate", "proj-1", "2.0.0")
	global := add("default", "", "1.0.0")
	foreign := add("default", "other-project", "3.0.0")
	for _, id := range []int64{stable, candidate, global, foreign} {
		id := id
		sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"install":%d}`, id) }))
		t.Cleanup(sidecar.Close)
		if _, e := s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, sidecar.URL, id); e != nil {
			t.Fatal(e)
		}
	}
	s.LoadInstalledApps()
	for _, id := range []int64{stable, candidate} {
		if err := s.registerAppMCP(id); err != nil {
			t.Fatal(err)
		}
	}
	agent := &Agent{ID: 99, UserID: 1, ProjectID: "proj-1"}
	_, selected, err := s.appMCPConfigsForInstallIDs(1, agent, []int64{stable, candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0]["name"] == selected[1]["name"] {
		t.Fatalf("install tools collapsed: %v", selected)
	}
	if got := mutateMCPServers(selected, selected[:1], "remove"); len(got) != 1 || got[0]["name"] != selected[1]["name"] {
		t.Fatal("removing one install removed another")
	}
	proxy := func(id int64, project string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/apps/named-database/read?project_id=" + project
		if id > 0 {
			path += fmt.Sprintf("&install_id=%d", id)
		}
		r := authedRequest(t, "GET", path, "", nil)
		w := httptest.NewRecorder()
		s.handleAppProxy(w, r)
		return w
	}
	for _, id := range []int64{stable, candidate} {
		w := proxy(id, "proj-1")
		if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"install":%d`, id)) {
			t.Fatalf("explicit route %d: %d %s", id, w.Code, w.Body.String())
		}
	}
	if w := proxy(foreign, "proj-1"); w.Code < 400 {
		t.Fatal("cross-project selector accepted")
	}
	if got := s.installedApps.GetByNameAndProject("named-database", "proj-1"); got == nil || got.InstallID != stable {
		t.Fatal("candidate changed default")
	}
	promote := func(id int64) *httptest.ResponseRecorder {
		r := authedRequest(t, "PATCH", "/apps/installs/"+itoa(id)+"/installation", "", map[string]any{"is_default": true})
		w := httptest.NewRecorder()
		s.handleAppInstallation(w, r, id)
		return w
	}
	if w := promote(candidate); w.Code != 200 {
		t.Fatalf("promote: %d %s", w.Code, w.Body.String())
	}
	if w := proxy(0, "proj-1"); w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"install":%d`, candidate)) {
		t.Fatalf("default route: %d %s", w.Code, w.Body.String())
	}
	// A fresh boot reads the same selected default. Explicit stable bindings stay pinned.
	s.installedApps = NewInstalledAppsRegistry()
	s.LoadInstalledApps()
	if got := s.installedApps.GetByNameAndProject("named-database", "proj-1"); got == nil || got.InstallID != candidate {
		t.Fatal("default lost on reload")
	}
	caller := seedInstallWithBindings(t, s, "named-functions", sdk.Manifest{Name: "named-functions", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: "named-database"}}}}, map[string]any{"named-database": stable})
	if got := installBoundAppID(s, caller, "named-database"); got != stable {
		t.Fatalf("explicit dependency moved: %d", got)
	}
	if _, err = s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, candidate); err != nil {
		t.Fatal(err)
	}
	s.installedApps.Remove(candidate)
	s.LoadInstalledApps()
	if got := s.installedApps.GetByNameAndProject("named-database", "proj-1"); got != nil {
		t.Fatalf("unavailable default routed to %d", got.InstallID)
	}
	if _, ok := s.findInstalledApp("named-database", "proj-1"); ok {
		t.Fatal("dependency fallback escaped selected default")
	}
	// Bound app calls must fail on the selected unavailable version, even
	// while another version of the same app remains reachable.
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, fmt.Sprintf(`{"named-database":%d}`, candidate), caller); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	if _, ok := s.authorizeAppCallback(response, caller, "named-database", "proj-1"); ok || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable binding fell back: %d %s", response.Code, response.Body.String())
	}
	if w := promote(candidate); w.Code != 409 {
		t.Fatalf("unready promotion accepted: %d", w.Code)
	}
	if w := proxy(stable, "proj-1"); w.Code != 200 {
		t.Fatalf("stable unavailable: %d %s", w.Code, w.Body.String())
	}
}

func TestNamedInstallationsConcurrentKeyAndDefault(t *testing.T) {
	s := newTestServer(t)
	id := seedAppWithTools(t, s, "concurrent-install", "p", nil)
	var app int64
	s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, id).Scan(&app)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,installation_key) VALUES(?,'p','candidate')`, app)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successful inserts: %d", successes)
	}
	var defaults int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE app_id=? AND is_default=1`, app).Scan(&defaults)
	if defaults != 1 {
		t.Fatalf("defaults: %d", defaults)
	}
}

func TestNamedInstallationsSkillsAndExplicitAppCalls(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	stable := seedAppWithTools(t, s, "named-skills", "proj-1", []string{"lookup"})
	var app int64
	s.store.db.QueryRow(`SELECT app_id FROM app_installs WHERE id=?`, stable).Scan(&app)
	res, err := s.store.db.Exec(`INSERT INTO app_installs(app_id,project_id,status,installed_by,installation_key,installation_name) VALUES(?,'proj-1','running',1,'candidate','Candidate')`, app)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := res.LastInsertId()
	for _, id := range []int64{stable, candidate} {
		skills := []sdk.Skill{{Name: "how-to-test", Description: "Verify fixture data", Body: "Use lookup to verify data.", Command: "/lookup"}}
		if err := s.registerAppSkills(id, "named-skills", "proj-1", skills, nil); err != nil {
			t.Fatal(err)
		}
		s.installedApps.Add(&InstalledApp{InstallID: id, AppName: "named-skills", ProjectID: "proj-1", SidecarURL: "http://127.0.0.1:1"})
	}
	var total, commands int
	s.store.db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT command) FROM skills WHERE install_id IN (?,?)`, stable, candidate).Scan(&total, &commands)
	if total != 2 || commands != 2 {
		t.Fatalf("skill collision: %d skills, %d commands", total, commands)
	}
	caller := seedInstallWithBindings(t, s, "named-consumer", sdk.Manifest{Name: "named-consumer", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: "named-skills"}}}}, map[string]any{"named-skills": map[string]any{"ids": []int64{stable, candidate}, "default_id": stable}})
	for _, id := range []int64{stable, candidate} {
		w := httptest.NewRecorder()
		target, ok := s.authorizeAppCallback(w, caller, "named-skills", "proj-1", id)
		if !ok || target.targetInstallID != id {
			t.Fatalf("explicit bound target %d rejected: %d %s", id, w.Code, w.Body.String())
		}
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=?`, fmt.Sprintf(`{"named-skills":%d}`, stable), caller); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if _, ok := s.authorizeAppCallback(w, caller, "named-skills", "proj-1", candidate); ok || w.Code != 403 {
		t.Fatal("unbound candidate selected")
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='error' WHERE id=?`, stable); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if _, ok := s.authorizeAppCallback(w, caller, "named-skills", "proj-1", stable); ok || w.Code != 503 {
		t.Fatal("unavailable explicit install accepted")
	}
}

func TestNamedInstallationsThroughInstallHandler(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	ensureTestAdmin(t, s)
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	install := func(key, version string) *httptest.ResponseRecorder {
		t.Helper()
		manifest := fmt.Sprintf("schema: apteva-app/v1\nname: named-install-handler\nversion: %s\nscopes: [project]\nruntime:\n  kind: static\n  static_dir: %s\n", version, dir)
		req := authedRequest(t, "POST", "/apps/install", "", map[string]any{"manifest_yaml": manifest, "project_id": "proj-1", "installation_key": key, "installation_name": key})
		w := httptest.NewRecorder()
		s.handleInstallApp(w, req)
		return w
	}
	var ids []int64
	for _, test := range []struct{ key, version string }{{"default", "1.0.0"}, {"candidate", "2.0.0"}} {
		w := install(test.key, test.version)
		if w.Code >= 300 {
			t.Fatalf("install %s: %d %s", test.key, w.Code, w.Body.String())
		}
		var result struct {
			ID int64 `json:"install_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.ID)
		request := authedRequest(t, "GET", fmt.Sprintf("/apps/named-install-handler/index.html?project_id=proj-1&install_id=%d", result.ID), "", nil)
		response := httptest.NewRecorder()
		s.handleAppProxy(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "fixture") {
			t.Fatalf("static install proxy: %d %s", response.Code, response.Body.String())
		}

	}
	if ids[0] == ids[1] || ids[0] == 0 {
		t.Fatalf("install identities %v", ids)
	}
	if w := install("candidate", "3.0.0"); w.Code != 409 {
		t.Fatalf("duplicate key HTTP %d: %s", w.Code, w.Body.String())
	}
	// Duplicate attempts must not overwrite either pinned installation version.
	for i, version := range []string{"1.0.0", "2.0.0"} {
		var got string
		s.store.db.QueryRow(`SELECT version FROM app_installs WHERE id=?`, ids[i]).Scan(&got)
		if got != version {
			t.Fatalf("version changed %s", got)
		}
	}
	w := httptest.NewRecorder()
	s.handleListApps(w, authedRequest(t, "GET", "/apps?project_id=proj-1", "", nil))
	var listed []AppRow
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Key == listed[1].Key || listed[0].MCPName == listed[1].MCPName {
		t.Fatalf("installation metadata lost: %s", w.Body.String())
	}
}

func TestNamedInstallationsSourceCacheKeepsSameVersionBuildsSeparate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	t.Setenv("GOWORK", "off")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module namedfixture\n\ngo 1.24.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runGit(repo, "init", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	commit := func(text string) {
		t.Helper()
		source := fmt.Sprintf("package main\nimport \"fmt\"\nfunc main(){fmt.Print(%q)}\n", text)
		if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runGit(repo, "add", "."); err != nil {
			t.Fatal(err)
		}
		if err := runGit(repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", text); err != nil {
			t.Fatal(err)
		}
	}
	commit("stable")
	sup := NewLocalSupervisor(t.TempDir())
	manifest := &sdk.Manifest{Name: "named-cache", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "source", Source: &sdk.SourceSpec{Repo: "file://" + repo, Ref: "main", Entry: "."}}}
	stable, err := sup.BuildFromSourceBinary(manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit("candidate")
	candidate, err := sup.BuildFromSourceBinary(manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stable == candidate {
		t.Fatal("different source commits with same version reused one package")
	}
	for path, want := range map[string]string{stable: "stable", candidate: "candidate"} {
		out, err := exec.Command(path).Output()
		if err != nil || string(out) != want {
			t.Fatalf("package overwritten: %s: %s %v", path, out, err)
		}
	}
	reused, err := sup.BuildFromSourceBinary(manifest, nil)
	if err != nil || reused != candidate {
		t.Fatalf("identical source package not reused: %s %v", reused, err)
	}
}
