package main

import (
	"database/sql"
	"os"
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
