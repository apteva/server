package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func ptrInt(i int) *int     { return &i }
func ptrI64(i int64) *int64 { return &i }

// makeSqlite creates a sqlite db at path with a contacts table holding the
// given emails. Uses the same driver the store registers (modernc.org/sqlite).
func makeSqlite(t *testing.T, path string, emails ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE contacts (id INTEGER PRIMARY KEY, email TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, e := range emails {
		if _, err := db.Exec(`INSERT INTO contacts (email) VALUES (?)`, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func TestSnapshotCaptureRestore(t *testing.T) {
	tmp := t.TempDir()
	ss := NewSnapshotStore(filepath.Join(tmp, "environments"))

	// A live crm sidecar data dir with a real DB (2 rows).
	liveDir := filepath.Join(tmp, "live-crm")
	makeSqlite(t, filepath.Join(liveDir, "crm.db"), "a@x.com", "b@x.com")

	cas := newCassette()
	cas.put("GET", "api.example.com", "/v1/x", nil, 200, nil, []byte(`{"ok":true}`))

	man, err := ss.Capture(CaptureSpec{
		ID:          "snap1",
		ProjectID:   "proj1",
		AppDataDirs: map[string]string{"crm": liveDir},
		Cassette:    cas,
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !man.HasCassette || len(man.Apps) != 1 || man.Apps[0] != "crm" {
		t.Fatalf("manifest wrong: %+v", man)
	}

	// Re-capture same id should fail.
	if _, err := ss.Capture(CaptureSpec{ID: "snap1", AppDataDirs: map[string]string{"crm": liveDir}}); err == nil {
		t.Fatalf("expected duplicate capture to fail")
	}

	// Restore into fresh dirs and verify the rows survived.
	dirs, err := ss.Restore("snap1", "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	crmDir, ok := dirs["crm"]
	if !ok {
		t.Fatalf("restore missing crm dir")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(crmDir, "crm.db")+"?mode=ro")
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n); err != nil {
		t.Fatalf("count restored: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 restored rows, got %d", n)
	}

	// Cassette restores too.
	rc, err := ss.Cassette("snap1")
	if err != nil || rc == nil {
		t.Fatalf("restore cassette: %v / %v", err, rc)
	}
	if _, ok := rc.lookup("GET", "api.example.com", "/v1/x", nil); !ok {
		t.Fatalf("restored cassette missing entry")
	}
}

func TestEdgeAssertions(t *testing.T) {
	calls := []InterceptedCall{
		{Host: "api.twitter.com", Path: "/2/tweets", Method: "POST"},
		{Host: "api.twitter.com", Path: "/2/tweets", Method: "POST"},
		{Host: "api.example.com", Path: "/health", Method: "GET"},
	}
	asserts := []Assertion{
		{Name: "two tweets", Kind: AssertEdge, Edge: &EdgeAssertion{Host: "api.twitter.com", Path: "/2/tweets", Method: "POST", Count: ptrInt(2)}},
		{Name: "no billing", Kind: AssertEdge, Edge: &EdgeAssertion{Host: "billing", Max: ptrInt(0)}},
		{Name: "wrong count", Kind: AssertEdge, Edge: &EdgeAssertion{Host: "api.twitter.com", Count: ptrInt(1)}},
	}
	results, allPass := EvaluateAssertions(asserts, AssertionInputs{EdgeCalls: calls})
	if allPass {
		t.Fatalf("expected overall fail (one assertion is wrong)")
	}
	if !results[0].Pass || !results[1].Pass || results[2].Pass {
		t.Fatalf("unexpected per-assertion results: %+v", results)
	}
}

func TestStateAssertions(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "crm.db")
	makeSqlite(t, dbPath, "x@y.com")
	resolve := func(app string) (string, bool) {
		if app == "crm" {
			return dbPath, true
		}
		return "", false
	}
	asserts := []Assertion{
		{Name: "one contact", Kind: AssertState, State: &StateAssertion{App: "crm", Query: `SELECT COUNT(*) FROM contacts WHERE email='x@y.com'`, Equals: ptrI64(1)}},
		{Name: "no missing app", Kind: AssertState, State: &StateAssertion{App: "ghost", Query: `SELECT 1`, Equals: ptrI64(1)}},
	}
	results, allPass := EvaluateAssertions(asserts, AssertionInputs{AppDBPath: resolve})
	if allPass {
		t.Fatalf("expected fail (ghost app)")
	}
	if !results[0].Pass {
		t.Fatalf("state assertion should pass: %+v", results[0])
	}
	if results[1].Pass {
		t.Fatalf("ghost-app assertion should fail")
	}
}

func TestEnvironmentMultipleAgents(t *testing.T) {
	var stopped []int64
	environment := &Environment{
		ID:           "environment-multi",
		agents:       map[int64]*EnvironmentAgent{},
		agentAliases: map[string]int64{},
	}
	a1 := &EnvironmentAgent{AgentID: 10, Alias: "main", Port: 4100, cleanup: func() { stopped = append(stopped, 10) }}
	a2 := &EnvironmentAgent{AgentID: 11, Alias: "reviewer", Port: 4101, cleanup: func() { stopped = append(stopped, 11) }}
	if err := environment.AttachAgent(a1); err != nil {
		t.Fatalf("attach main: %v", err)
	}
	if err := environment.AttachAgent(a2); err != nil {
		t.Fatalf("attach reviewer: %v", err)
	}
	if err := environment.AttachAgent(&EnvironmentAgent{AgentID: 12, Alias: "reviewer"}); err == nil {
		t.Fatalf("expected duplicate alias to fail")
	}
	if got := environment.Agent(); got == nil || got.AgentID != 10 {
		t.Fatalf("default agent = %+v, want main", got)
	}
	if got := environment.GetAgent(11); got == nil || got.Alias != "reviewer" {
		t.Fatalf("get agent 11 = %+v", got)
	}
	if got := environment.AgentByAlias("reviewer"); got == nil || got.AgentID != 11 {
		t.Fatalf("alias lookup = %+v", got)
	}
	if !environment.StopAgent(10) {
		t.Fatalf("stop main returned false")
	}
	if got := environment.Agent(); got == nil || got.AgentID != 11 {
		t.Fatalf("default after stopping main = %+v, want reviewer", got)
	}
	environment.Stop()
	if len(stopped) != 2 || stopped[0] != 10 || stopped[1] != 11 {
		t.Fatalf("stopped agents = %v, want [10 11]", stopped)
	}
}

func TestSummarizeEnvironmentAgentsUsesRuntimeStatus(t *testing.T) {
	s := &Server{agents: NewAgentManager(t.TempDir(), "apteva-core")}
	s.agents.processes[10] = &runningAgent{cmd: &exec.Cmd{}, port: 4100}
	infos := s.summarizeEnvironmentAgents([]*EnvironmentAgent{
		{AgentID: 10, Alias: "main", Port: 4100},
		{AgentID: 11, Alias: "stale", Port: 4101},
	})
	if len(infos) != 2 {
		t.Fatalf("infos len = %d, want 2", len(infos))
	}
	if infos[0].Status != "running" {
		t.Fatalf("running status = %q, want running", infos[0].Status)
	}
	if infos[1].Status != "stopped" {
		t.Fatalf("stale status = %q, want stopped", infos[1].Status)
	}
}

func TestPersistentEnvironmentListedWithoutRuntime(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	s := &Server{
		store:        store,
		port:         "5280",
		agents:       NewAgentManager(filepath.Join(dataDir, "agents"), ""),
		environments: NewEnvironmentManager(environmentDataRoot(dataDir)),
	}
	s.environments.server = s

	res, err := store.db.Exec(`INSERT INTO apps (name, source, manifest_json) VALUES ('crm', 'local', '{}')`)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, 'proj-1', 'running')`, appID)
	if err != nil {
		t.Fatalf("seed install: %v", err)
	}
	installID, _ := res.LastInsertId()

	autostart := false
	summary, err := s.createPersistentEnvironment(createEnvironmentRequest{
		ID:            "crm-demo",
		ProjectID:     "proj-1",
		AppInstallIDs: []int64{installID},
		Mode:          "block",
		Autostart:     &autostart,
	}, 1)
	if err != nil {
		t.Fatalf("create persistent environment: %v", err)
	}
	if !summary.Persisted || summary.Ephemeral || summary.Status != "stopped" {
		t.Fatalf("summary persistence/status wrong: %+v", summary)
	}
	if _, ok := s.environments.Get("crm-demo"); ok {
		t.Fatalf("autostart=false should not create a runtime")
	}

	// Simulate a fresh manager after server restart: the DB row still
	// drives list output even with no live runtime object.
	s.environments = NewEnvironmentManager(environmentDataRoot(dataDir))
	s.environments.server = s
	list := s.listEnvironmentSummaries(1)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1: %+v", len(list), list)
	}
	if list[0].ID != "crm-demo" || list[0].Status != "stopped" || !list[0].Persisted {
		t.Fatalf("listed summary wrong: %+v", list[0])
	}
	if app := list[0].Apps["crm"]; app.Kind != "install" || app.InstallID != installID {
		t.Fatalf("persisted app summary wrong: %+v", list[0].Apps)
	}
}

func TestEphemeralEnvironmentDoesNotCreatePersistentRecord(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	s := &Server{
		store:        store,
		port:         "5280",
		agents:       NewAgentManager(filepath.Join(dataDir, "agents"), ""),
		environments: NewEnvironmentManager(environmentDataRoot(dataDir)),
	}
	s.environments.server = s

	environment, err := s.createEnvironmentRuntime(createEnvironmentRequest{ID: "temp-env", Ephemeral: true, Mode: "block"}, 1)
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	defer s.environments.Destroy(environment.ID)
	if _, err := store.GetEnvironmentRecord("temp-env"); err == nil {
		t.Fatalf("ephemeral environment unexpectedly persisted")
	}
	list := s.listEnvironmentSummaries(1)
	if len(list) != 1 || !list[0].Ephemeral || list[0].Persisted {
		t.Fatalf("ephemeral summary wrong: %+v", list)
	}
}

func TestStartPersistentEnvironmentRecreatesRuntime(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "server.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	s := &Server{
		store:        store,
		port:         "5280",
		agents:       NewAgentManager(filepath.Join(dataDir, "agents"), ""),
		environments: NewEnvironmentManager(environmentDataRoot(dataDir)),
	}
	s.environments.server = s

	autostart := false
	if _, err := s.createPersistentEnvironment(createEnvironmentRequest{
		ID:        "restartable",
		ProjectID: "proj-1",
		Mode:      "block",
		Autostart: &autostart,
	}, 1); err != nil {
		t.Fatalf("create persistent: %v", err)
	}
	rec, err := store.GetEnvironmentRecord("restartable")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, ok := s.environments.Get("restartable"); ok {
		t.Fatalf("runtime should not exist before start")
	}
	runtime, err := s.startPersistentEnvironment(*rec, 1)
	if err != nil {
		t.Fatalf("start persistent: %v", err)
	}
	defer s.environments.Destroy(runtime.ID)
	if runtime.ID != "restartable" {
		t.Fatalf("runtime id = %q", runtime.ID)
	}
	rec, err = store.GetEnvironmentRecord("restartable")
	if err != nil {
		t.Fatalf("record after start: %v", err)
	}
	if rec.Status != "running" {
		t.Fatalf("status after start = %q, want running", rec.Status)
	}
}

func TestTrajectoryAssertions(t *testing.T) {
	seq := []string{"lookup_customer", "charge_card"}
	cases := []struct {
		ta   TrajectoryAssertion
		want bool
	}{
		{TrajectoryAssertion{ToolCalled: "charge_card"}, true},
		{TrajectoryAssertion{ToolNotCalled: "refund_customer"}, true},
		{TrajectoryAssertion{ToolNotCalled: "charge_card"}, false},
		{TrajectoryAssertion{ToolCalled: "lookup_customer", Before: "charge_card"}, true},
		{TrajectoryAssertion{ToolCalled: "charge_card", Before: "lookup_customer"}, false},
		{TrajectoryAssertion{ToolCalled: "never_called"}, false},
	}
	for i, c := range cases {
		got, detail := evalTrajectory(&c.ta, seq)
		if got != c.want {
			t.Errorf("case %d: got %v want %v (%s)", i, got, c.want, detail)
		}
	}
}

func TestIntegrationInterceptorSeam(t *testing.T) {
	const wid = "test-environment-1"
	app := &AppTemplate{Slug: "twitter"}
	tool := &AppToolDef{Name: "post_tweet", Method: "POST"}

	remove := RegisterEnvironmentInterceptor(wid, []IntegrationFixture{
		{App: "twitter", Tool: "post_tweet", Status: 201, Data: map[string]any{"id": "123"}},
	})
	defer remove()

	// Owned call in this environment short-circuits — no network, canned result.
	res, err := executeIntegrationTool(app, tool, map[string]string{}, map[string]any{}, wid)
	if err != nil {
		t.Fatalf("intercepted call errored: %v", err)
	}
	if res.Status != 201 || !res.Success {
		t.Fatalf("interceptor result wrong: %+v", res)
	}

	// Different environment id → not routed here. We can't run the real call in a
	// unit test, so assert the registry has nothing for an unknown environment.
	if _, ok := environmentInterceptors.Load("other-environment"); ok {
		t.Fatalf("unexpected interceptor for other-environment")
	}

	// Unowned (app,tool) within the same environment passes through (handled=false).
	v, _ := environmentInterceptors.Load(wid)
	if _, handled := v.(integrationInterceptorFn)(&AppTemplate{Slug: "hubspot"}, &AppToolDef{Name: "create_contact"}, nil); handled {
		t.Fatalf("interceptor wrongly claimed an unowned call")
	}

	remove()
	if _, ok := environmentInterceptors.Load(wid); ok {
		t.Fatalf("remove() did not clear the interceptor")
	}
}

func TestEnvironmentModeSplitDefaults(t *testing.T) {
	if got := normalizeEnvironmentNetworkMode("", ""); got != EdgePassthrough {
		t.Fatalf("empty network mode = %q, want %q", got, EdgePassthrough)
	}
	if got := normalizeEnvironmentNetworkMode("", EdgeMock); got != EdgePassthrough {
		t.Fatalf("legacy mock network mode = %q, want %q", got, EdgePassthrough)
	}
	if got := normalizeEnvironmentNetworkMode("", EdgeBlock); got != EdgeBlock {
		t.Fatalf("legacy block network mode = %q, want %q", got, EdgeBlock)
	}
	if got := normalizeEnvironmentIntegrationMode("", ""); got != IntegrationModeMock {
		t.Fatalf("empty integration mode = %q, want %q", got, IntegrationModeMock)
	}
	if got := normalizeEnvironmentIntegrationMode(IntegrationModeReal, EdgeMock); got != IntegrationModeReal {
		t.Fatalf("explicit real integration mode = %q, want %q", got, IntegrationModeReal)
	}
}
