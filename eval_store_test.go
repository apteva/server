package main

// eval_store_test.go — exercises the DB-layer CRUD on agent_evals,
// agent_eval_runs, and agent_directive_history through a real SQLite
// file (the same NewStore used in production, just pointed at a
// temp dir).
//
// What we're protecting against:
//   - Silent JSON round-trip drift (Goals/Mocks/Trajectory/Verdict
//     all flow as JSON-in-TEXT columns; a schema change that omits
//     a column from a SELECT would deserialise to zero values that
//     "look fine" until the dashboard renders them).
//   - last_status / last_run_at rollup falling out of sync with the
//     authoritative agent_eval_runs row (the dashboard's red/green
//     dot reads from the cached column).
//   - Foreign-key cascade behaviour on DeleteAgentEval (runs should
//     vanish with the parent eval).
//   - InsertEvalRun's nullable suggestions_json: clean strict runs
//     must NOT write an empty JSON object; otherwise the apply
//     handler would offer to apply zero suggestions to operators.

import (
	"encoding/json"
	"testing"
	"time"
)

// seedEvalAgent inserts a user + agent so the FK on agent_evals.agent_id
// holds. Reuses Store's public CreateUser/CreateAgent rather than
// going through handleRegister — we just need rows that exist, not
// the full auth flow.
func seedEvalAgent(t *testing.T, store *Store) *Agent {
	t.Helper()
	user, err := store.CreateUser("evals-test@example.com", "x")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	agent, err := store.CreateAgent(user.ID, "evals-test-agent", "be polite", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return agent
}

// sampleEval returns a hydrated Eval with non-trivial Goals + Mocks
// so the JSON round-trip is meaningful.
func sampleEval(agentID int64, id, name string) Eval {
	return Eval{
		ID:          id,
		AgentID:     agentID,
		Name:        name,
		Description: "Greet the user and confirm the request.",
		Goals: []string{
			"Agent greets the user by name.",
			"Agent confirms the request before acting.",
		},
		Mocks: []EvalMock{{
			App: "messaging", Tool: "send",
			ArgsMatch: map[string]any{"to": "alice"},
			Return:    json.RawMessage(`{"message_id":"m_1"}`),
		}},
		MaxTurns:  4,
		Schedule:  "manual",
		Source:    "user",
		SortOrder: 1,
	}
}

// ─── CRUD ─────────────────────────────────────────────────────────

func TestStore_CreateGetAgentEval_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	in := sampleEval(agent.ID, "ev-create-1", "Greet flow")
	created, err := store.CreateAgentEval(in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != in.ID {
		t.Errorf("id round-trip: got %q want %q", created.ID, in.ID)
	}
	if created.Description != in.Description {
		t.Errorf("description round-trip mismatch")
	}
	if len(created.Goals) != 2 {
		t.Errorf("goals len: got %d want 2", len(created.Goals))
	}
	if len(created.Mocks) != 1 || created.Mocks[0].App != "messaging" {
		t.Errorf("mocks round-trip mismatch: %+v", created.Mocks)
	}
	if string(created.Mocks[0].Return) != `{"message_id":"m_1"}` {
		t.Errorf("mock Return round-trip mismatch: %s", created.Mocks[0].Return)
	}

	fetched, err := store.GetAgentEval(in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.AgentID != agent.ID {
		t.Errorf("agent_id round-trip: got %d want %d", fetched.AgentID, agent.ID)
	}
	if fetched.MaxTurns != 4 {
		t.Errorf("max_turns round-trip: got %d want 4", fetched.MaxTurns)
	}
}

func TestStore_CreateAgentEval_DefaultsMaxTurnsAndSchedule(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	in := Eval{
		ID:          "ev-defaults",
		AgentID:     agent.ID,
		Name:        "minimal",
		Description: "x",
		Goals:       []string{"g"},
	}
	created, err := store.CreateAgentEval(in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.MaxTurns != 5 {
		t.Errorf("MaxTurns should default to 5, got %d", created.MaxTurns)
	}
	if created.Schedule != "manual" {
		t.Errorf("Schedule should default to manual, got %q", created.Schedule)
	}
	if created.Source != "user" {
		t.Errorf("Source should default to user, got %q", created.Source)
	}
}

func TestStore_CreateAgentEval_BackfillsDescriptionFromLegacyTrigger(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	in := Eval{
		ID:      "ev-legacy",
		AgentID: agent.ID,
		Name:    "legacy",
		Trigger: EvalTrigger{Type: "chat_message", Payload: map[string]any{"text": "hi"}},
		Goals:   []string{"g"},
	}
	created, err := store.CreateAgentEval(in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Description == "" {
		t.Error("Description should have been backfilled from legacy Trigger")
	}
}

func TestStore_ListAgentEvals_OrderedBySortOrderThenName(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	mk := func(id, name string, sort int) {
		e := sampleEval(agent.ID, id, name)
		e.SortOrder = sort
		if _, err := store.CreateAgentEval(e); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ev-3", "Charlie", 1)
	mk("ev-1", "Alpha", 2)
	mk("ev-2", "Bravo", 1)

	got, err := store.ListAgentEvals(agent.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: %d", len(got))
	}
	// sort_order ASC then name ASC ⇒ [Bravo(1), Charlie(1), Alpha(2)]
	wantNames := []string{"Bravo", "Charlie", "Alpha"}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("position %d: got %q want %q", i, got[i].Name, w)
		}
	}
}

func TestStore_UpdateAgentEval_PersistsEdits(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	created, err := store.CreateAgentEval(sampleEval(agent.ID, "ev-upd", "Original"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.Name = "Renamed"
	created.Description = "new description"
	created.Goals = append(created.Goals, "third goal added")
	created.MaxTurns = 8
	if err := store.UpdateAgentEval(*created); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := store.GetAgentEval(created.ID)
	if got.Name != "Renamed" {
		t.Errorf("name not updated: %q", got.Name)
	}
	if got.Description != "new description" {
		t.Errorf("description not updated: %q", got.Description)
	}
	if len(got.Goals) != 3 {
		t.Errorf("goals not extended: %d", len(got.Goals))
	}
	if got.MaxTurns != 8 {
		t.Errorf("max_turns not updated: %d", got.MaxTurns)
	}
}

func TestStore_DeleteAgentEval_CascadesRuns(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	ev, err := store.CreateAgentEval(sampleEval(agent.ID, "ev-del", "Delete me"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	runID, err := store.InsertEvalRun(EvalRun{
		EvalID:    ev.ID,
		StartedAt: time.Now(),
		Status:    "pass",
	})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if runID == 0 {
		t.Fatal("expected non-zero run id")
	}

	if err := store.DeleteAgentEval(ev.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, err := store.GetAgentEval(ev.ID); err == nil && got != nil {
		t.Error("eval row should be gone")
	}
	// DeleteAgentEval explicitly deletes child runs alongside the
	// parent row. PRAGMA foreign_keys is off store-wide so the
	// schema's ON DELETE CASCADE on agent_eval_runs.eval_id is
	// inert; the runs cleanup is done explicitly in agent_evals.go.
	runs, _ := store.ListEvalRuns(ev.ID, 10)
	if len(runs) != 0 {
		t.Errorf("runs should have cascaded; got %d survivors", len(runs))
	}
}

// ─── UpsertSeedAgentEval (idempotent template seeding) ────────────

func TestStore_UpsertSeedAgentEval_IdempotentOnSameID(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	seed := Eval{
		ID:          "tpl:default:" + i64s(agent.ID),
		AgentID:     agent.ID,
		Name:        "From template",
		Description: "x",
		Goals:       []string{"g"},
		SourceRef:   "tpl",
		SortOrder:   100,
	}
	if err := store.UpsertSeedAgentEval(seed); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Operator edits the seeded row — second upsert (e.g. on agent
	// re-template) must NOT clobber those edits.
	first, _ := store.GetAgentEval(seed.ID)
	first.Name = "Operator renamed"
	if err := store.UpdateAgentEval(*first); err != nil {
		t.Fatalf("operator update: %v", err)
	}

	// Re-seed with a "fresh" template definition — INSERT OR IGNORE
	// should leave the operator's edits in place.
	seed.Name = "From template (v2)"
	if err := store.UpsertSeedAgentEval(seed); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ := store.GetAgentEval(seed.ID)
	if got.Name != "Operator renamed" {
		t.Errorf("re-seed clobbered operator edit; got %q want %q", got.Name, "Operator renamed")
	}
}

// ─── EvalRun history ──────────────────────────────────────────────

func TestStore_InsertEvalRun_PersistsTrajectoryAndVerdict(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-run", "Run test"))

	finished := time.Now()
	started := finished.Add(-2 * time.Second)
	run := EvalRun{
		EvalID:     ev.ID,
		StartedAt:  started,
		FinishedAt: &finished,
		Status:     "pass",
		Trajectory: Trajectory{Turns: []TrajectoryTurn{
			{Role: "user", Content: "hi"},
			{Role: "agent", Content: "hello"},
		}},
		Verdict: &JudgeVerdict{
			Overall:   "pass",
			Reasoning: "looks good",
			PerGoal: []GoalVerdict{
				{Goal: "greet", Verdict: "pass", Why: "said hi"},
			},
			JudgeModel: "test-judge",
		},
		Suggestions:    &RunSuggestions{DirectiveEdits: []DirectiveEditSuggestion{{ID: "edit-1", Add: "be louder"}}},
		DurationMS:     2000,
		TurnsUsed:      1,
		IterationsUsed: 1,
	}
	id, err := store.InsertEvalRun(run)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("want non-zero id")
	}

	got, err := store.GetEvalRun(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "pass" {
		t.Errorf("status: %q", got.Status)
	}
	if got.Verdict == nil || got.Verdict.Overall != "pass" {
		t.Errorf("verdict round-trip lost data: %+v", got.Verdict)
	}
	if got.Verdict.JudgeModel != "test-judge" {
		t.Errorf("judge_model round-trip: %q", got.Verdict.JudgeModel)
	}
	if got.Suggestions == nil || len(got.Suggestions.DirectiveEdits) != 1 {
		t.Errorf("suggestions round-trip lost data: %+v", got.Suggestions)
	}
	if len(got.Trajectory.Turns) != 2 {
		t.Errorf("trajectory turns: got %d want 2", len(got.Trajectory.Turns))
	}
}

func TestStore_InsertEvalRun_NullSuggestionsWhenEmpty(t *testing.T) {
	// Clean strict pass: no suggestions. We want suggestions_json to
	// stay NULL so the apply handler can't be tricked into offering
	// to "apply" zero edits.
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-null-sug", "Clean run"))

	id, err := store.InsertEvalRun(EvalRun{
		EvalID:      ev.ID,
		StartedAt:   time.Now(),
		Status:      "pass",
		Suggestions: &RunSuggestions{}, // empty — should NOT be persisted
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _ := store.GetEvalRun(id)
	if got.Suggestions != nil {
		t.Errorf("empty suggestions should round-trip as nil, got %+v", got.Suggestions)
	}
}

func TestStore_InsertEvalRun_DefaultsIterationsToOne(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-iter", "x"))

	id, err := store.InsertEvalRun(EvalRun{
		EvalID:    ev.ID,
		StartedAt: time.Now(),
		Status:    "pass",
		// IterationsUsed: 0 — should be coerced to 1
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _ := store.GetEvalRun(id)
	if got.IterationsUsed != 1 {
		t.Errorf("IterationsUsed should default to 1, got %d", got.IterationsUsed)
	}
}

func TestStore_ListEvalRuns_OrderedByStartedAtDesc(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-list-runs", "x"))

	base := time.Now()
	for i, status := range []string{"pass", "fail", "pass"} {
		_, err := store.InsertEvalRun(EvalRun{
			EvalID:    ev.ID,
			StartedAt: base.Add(time.Duration(i) * time.Second),
			Status:    status,
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	runs, err := store.ListEvalRuns(ev.ID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("len: %d", len(runs))
	}
	// Most recent first.
	if runs[0].Status != "pass" || runs[1].Status != "fail" || runs[2].Status != "pass" {
		t.Errorf("order mismatch: %v / %v / %v", runs[0].Status, runs[1].Status, runs[2].Status)
	}
	for i := 0; i+1 < len(runs); i++ {
		if runs[i].StartedAt.Before(runs[i+1].StartedAt) {
			t.Errorf("not ordered DESC at position %d: %v before %v", i, runs[i].StartedAt, runs[i+1].StartedAt)
		}
	}
}

func TestStore_ListEvalRuns_RespectsLimit(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-limit", "x"))

	for i := 0; i < 5; i++ {
		store.InsertEvalRun(EvalRun{EvalID: ev.ID, StartedAt: time.Now().Add(time.Duration(i) * time.Millisecond), Status: "pass"})
	}
	got, _ := store.ListEvalRuns(ev.ID, 3)
	if len(got) != 3 {
		t.Errorf("limit not enforced: got %d", len(got))
	}
}

// ─── Rollup ───────────────────────────────────────────────────────

func TestStore_RollupEvalLastRun_UpdatesCachedColumns(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-rollup", "x"))

	if ev.LastStatus != "" {
		t.Errorf("fresh eval should have empty LastStatus, got %q", ev.LastStatus)
	}
	when := time.Now().Truncate(time.Second)
	if err := store.RollupEvalLastRun(ev.ID, "fail", when); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	got, _ := store.GetAgentEval(ev.ID)
	if got.LastStatus != "fail" {
		t.Errorf("last_status not rolled up; got %q", got.LastStatus)
	}
	if got.LastRunAt == nil {
		t.Fatal("last_run_at not rolled up")
	}
	if got.LastRunAt.Unix() != when.Unix() {
		t.Errorf("last_run_at drift: got %v want %v", got.LastRunAt, when)
	}
}

// ─── Directive history ────────────────────────────────────────────

func TestStore_InsertDirectiveHistory_PersistsRow(t *testing.T) {
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)
	ev, _ := store.CreateAgentEval(sampleEval(agent.ID, "ev-hist", "x"))
	runID, _ := store.InsertEvalRun(EvalRun{
		EvalID:    ev.ID,
		StartedAt: time.Now(),
		Status:    "fail",
	})

	err := store.InsertDirectiveHistory(agent.ID, "before", "before\n\nafter", "eval_suggestion", runID, agent.UserID)
	if err != nil {
		t.Fatalf("insert history: %v", err)
	}

	var count int
	var before, after, source string
	var runRef *int64
	row := store.db.QueryRow(
		`SELECT COUNT(*) FROM agent_directive_history WHERE agent_id = ?`, agent.ID,
	)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("history rows: got %d want 1", count)
	}
	row = store.db.QueryRow(
		`SELECT directive_before, directive_after, source, source_eval_run_id
		   FROM agent_directive_history WHERE agent_id = ?`,
		agent.ID,
	)
	if err := row.Scan(&before, &after, &source, &runRef); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if before != "before" || after != "before\n\nafter" {
		t.Errorf("body round-trip: before=%q after=%q", before, after)
	}
	if source != "eval_suggestion" {
		t.Errorf("source: %q", source)
	}
	if runRef == nil || *runRef != runID {
		t.Errorf("source_eval_run_id: %v want %d", runRef, runID)
	}
}

func TestStore_InsertDirectiveHistory_NullableRunIDWhenZero(t *testing.T) {
	// Non-eval sources (manual_edit) pass 0 — the column must be NULL,
	// not 0, so the FK doesn't reference a non-existent run row.
	store := newTestStore(t)
	agent := seedEvalAgent(t, store)

	if err := store.InsertDirectiveHistory(agent.ID, "a", "b", "manual_edit", 0, agent.UserID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var runRef *int64
	if err := store.db.QueryRow(
		`SELECT source_eval_run_id FROM agent_directive_history WHERE agent_id = ?`,
		agent.ID,
	).Scan(&runRef); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if runRef != nil {
		t.Errorf("source_eval_run_id should be NULL when sourceEvalRunID=0, got %d", *runRef)
	}
}
