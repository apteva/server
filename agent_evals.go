package main

// agent_evals.go — behavioural tests attached to agents. See the
// table comments in store.go and the docs in agent_templates.go for
// how template-shipped starter evals get seeded into agent_evals on
// agent create.
//
// Architecture (no apteva-core changes):
//   1. Operator clicks "Run eval" in the wizard's Verify step or on
//      the agent detail page.
//   2. apteva-server spawns a fresh apteva-core process bound to a
//      throwaway DB dir, with the same directive/tools as the live
//      agent but using a unique session token.
//   3. apteva-server registers the eval's mocks in-memory, keyed by
//      that session token.
//   4. apteva-server feeds the trigger to the spawned core via
//      core's existing /threads HTTP API.
//   5. Every MCP tool call from the spawned core lands on
//      apteva-server's gateway. The gateway looks up the session
//      token in the mocks registry: matched call returns the canned
//      response (recorded to the per-run trajectory buffer);
//      unmatched call returns a stub-ok with a warning.
//   6. apteva-server polls the spawned core's thread state until it
//      idles or hits max_turns. Stitches LLM messages from the
//      thread history with the gateway-recorded tool calls into a
//      single chronological trajectory.
//   7. apteva-server posts the trajectory + goals to the platform
//      meta-agent (see platform_agent.go) for grading. The
//      meta-agent returns a JudgeVerdict.
//   8. Result + trajectory go into agent_eval_runs;
//      agent_evals.last_status + last_run_at get rolled up.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Eval is the wire shape returned by the evals endpoints. Mirrors
// agent_evals row layout with JSON columns parsed into typed
// substructures so the dashboard doesn't have to.
//
// Description is the primary input — a plain-prose brief the agent
// reads as its opening event. It replaces the older Trigger field;
// Trigger stays on the struct (and on the DB row) for one release
// so existing rows keep parsing. New rows write Description only;
// reads of legacy rows backfill Description from Trigger via
// triggerToText so handlers + runner only ever look at Description.
type Eval struct {
	ID          string      `json:"id"`
	AgentID     int64       `json:"agent_id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Trigger     EvalTrigger `json:"trigger,omitempty"` // deprecated; backfilled to description on read
	Goals       []string    `json:"goals"`
	Mocks       []EvalMock  `json:"mocks"`
	MaxTurns    int         `json:"max_turns"`
	Schedule    string      `json:"schedule"`
	LastStatus  string      `json:"last_status,omitempty"`
	LastRunAt   *time.Time  `json:"last_run_at,omitempty"`
	Source      string      `json:"source"` // 'user' | 'template' | 'app'
	SourceRef   string      `json:"source_ref,omitempty"`
	SortOrder   int         `json:"sort_order"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// EvalTrigger is the legacy "situation we drive the agent with"
// primitive. New evals don't use it; existing rows are converted
// to Description on read via triggerToText. Removed in the next
// release once all rows have description populated.
type EvalTrigger struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// RunOptions controls a single eval run's execution policy. The
// eval row stays declarative ("what good looks like"); the caller
// passes RunOptions to choose strict-verification vs improvement
// mode at run time. Defaults come from the route:
//
//	/evals/preview                  → {MaxIterations: 5, StrictMocks: false}
//	/agents/:id/evals/:id/run       → {MaxIterations: 1, StrictMocks: false}
//	(future) continuous monitor     → {MaxIterations: 1, StrictMocks: true}
//
// MaxIterations=1 disables the improvement loop entirely: one
// attempt, one judge pass, final verdict. >1 enables the revise
// loop — if the judge fails, its per_goal feedback (and any
// directive_edits it proposes) feed into a follow-up attempt.
//
// StrictMocks=true makes any unmocked tool call fail the run with
// error_message instead of returning the lenient {"ok":true,"_stub":true}
// default. Used by continuous monitoring where stub-ok would mask
// drift.
type RunOptions struct {
	MaxIterations int  `json:"max_iterations,omitempty"`
	StrictMocks   bool `json:"strict_mocks,omitempty"`

	// ProviderOverride pins this eval run to one configured provider
	// ("anthropic", "opencode-go", "fireworks", ...). The spawned eval core
	// sees only that provider so the agent cannot drift to another model during
	// the run.
	ProviderOverride string `json:"provider_override,omitempty"`
	// ModelOverride pins the selected provider's large/medium/small tiers to
	// one concrete model id. This keeps benchmark comparisons clean even if the
	// agent paces/spawns with model="small" mid-run.
	ModelOverride string `json:"model_override,omitempty"`

	// UseEnvironment runs the eval in a real Environment derived from the
	// agent's app bindings instead of the mock gateway: the agent runs against
	// real in-environment apps, externals virtualised by the edge.
	UseEnvironment bool `json:"use_environment,omitempty"`

	// SeedPlan sets up the Environment's starting state before the agent runs, by
	// driving the in-environment apps' real tools (ExecuteSeedPlan). Lets an eval
	// test behavior over pre-existing state. Only used with environment runs. The
	// plan can be authored by hand or proposed by the meta-agent.
	SeedPlan []SeedCall `json:"seed_plan,omitempty"`
	// SeedBaseDir is the allowlisted directory for SeedCall.file fixtures.
	// Relative fixture paths resolve from here and may not escape it.
	SeedBaseDir string `json:"seed_base_dir,omitempty"`
	// SeedAfterSpawn runs SeedPlan only after the transient environment-agent
	// is spawned. Useful when seed-time app events should wake the agent.
	SeedAfterSpawn bool `json:"seed_after_spawn,omitempty"`
	// AppEventSubscriptions creates source='app_event' subscriptions for the
	// transient environment-agent before seed_after_spawn plans run.
	AppEventSubscriptions []RunAppEventSubscription `json:"app_event_subscriptions,omitempty"`

	// SandboxApps + HTTPMocks switch the eval into "real-app sandbox"
	// mode (see eval_sandbox.go). When either is non-empty, the runner:
	//  - boots an HTTP intercept proxy with the declared HTTPMocks +
	//    the conservative default allowlist (LLM hosts, loopback);
	//  - spawns each SandboxApp as a real sidecar with tmp data dir
	//    and HTTP_PROXY pointed at the intercept;
	//  - wires the sidecars' MCP URLs into the eval-core's config so
	//    the agent discovers their real tools (with real schemas).
	// Tool-level Mocks on the Eval still work in parallel for things
	// without a real app behind them (third-party integration tools,
	// etc.).
	SandboxApps []SandboxApp `json:"sandbox_apps,omitempty"`
	HTTPMocks   []HTTPMock   `json:"http_mocks,omitempty"`
}

type RunAppEventSubscription struct {
	App         string `json:"app"`
	Topic       string `json:"topic"`
	ThreadID    string `json:"thread_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// EvalMock declares how a single tool should answer in this eval's
// sandboxed run. First match wins on (App, Tool, optional ArgsMatch).
// One of Return or Error must be set; Return is the normal path,
// Error is the negative-path-testing path that surfaces an MCP error
// back to the agent.
type EvalMock struct {
	App       string          `json:"app"`
	Tool      string          `json:"tool"`
	ArgsMatch map[string]any  `json:"args_match,omitempty"`
	Return    json.RawMessage `json:"return,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// EvalRun is one entry in the history. trajectory + verdict are
// embedded so the dashboard can render a run inline without a
// second fetch. Suggestions captures the judge's improvement
// proposals across all iterations of an improvement run; nil for
// strict single-shot runs that passed first try.
type EvalRun struct {
	ID             int64           `json:"id"`
	EvalID         string          `json:"eval_id"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	Status         string          `json:"status"` // 'pass' | 'fail' | 'error'
	Trajectory     Trajectory      `json:"trajectory"`
	Verdict        *JudgeVerdict   `json:"verdict,omitempty"`
	Suggestions    *RunSuggestions `json:"suggestions,omitempty"`
	DurationMS     int             `json:"duration_ms"`
	TurnsUsed      int             `json:"turns_used"`
	IterationsUsed int             `json:"iterations_used"`
	ErrorMessage   string          `json:"error_message,omitempty"`
	Metrics        *EvalRunMetrics `json:"metrics,omitempty"`
}

type EvalRunMetrics struct {
	AgentID       int64   `json:"agent_id,omitempty"`
	LLMCalls      int     `json:"llm_calls"`
	TokensIn      int     `json:"tokens_in"`
	TokensOut     int     `json:"tokens_out"`
	TokensCached  int     `json:"tokens_cached"`
	TokensTotal   int     `json:"tokens_total"`
	CostUSD       float64 `json:"cost_usd"`
	LLMDurationMS int     `json:"llm_duration_ms"`
	ToolCalls     int     `json:"tool_calls"`
	Errors        int     `json:"errors"`
}

// RunSuggestions rolls up everything the judge proposed across the
// run's iterations into one shape the apply-handler can act on.
// Each item carries an Applied flag set by the apply-handler when
// the operator opts to persist it to the live agent.
type RunSuggestions struct {
	DirectiveEdits []DirectiveEditSuggestion `json:"directive_edits,omitempty"`
	// Carry-forward for the future: missing app/capability
	// suggestions live here once the simulator + marketplace
	// catalog feed land. Empty in this release.
	MissingCapabilities []MissingCapabilitySuggestion `json:"missing_capabilities,omitempty"`
}

// DirectiveEditSuggestion is the judge's proposal for a directive
// addition. The eval runner applies these ephemerally on respawn
// during the improvement loop (Helped tracks which ones actually
// flipped a goal to pass); the operator decides post-run whether
// to commit any to the live agent's stored directive via the
// apply-improvements handler.
type DirectiveEditSuggestion struct {
	ID     string `json:"id"`               // stable per-run id; "edit-1", "edit-2"
	Add    string `json:"add"`              // text to append to the directive
	Reason string `json:"reason,omitempty"` // judge's rationale (one sentence)
	Helped bool   `json:"helped,omitempty"` // true if a subsequent iteration passed after applying
}

// MissingCapabilitySuggestion is the judge's read on "the agent
// tried to do X but had no tool for it; consider installing app Y".
// Placeholder in this release — populated once the simulator and
// the marketplace-catalog feed to the judge land. Apply-handler
// will offer to install the app on the live agent.
type MissingCapabilitySuggestion struct {
	ID     string `json:"id"`
	App    string `json:"app"`
	Reason string `json:"reason,omitempty"`
	Helped bool   `json:"helped,omitempty"`
}

// Trajectory is the full record of one eval run. Stitched from
// (a) the spawned core's thread message history and (b) the
// gateway's tool-call recorder, ordered by ts.
type Trajectory struct {
	Turns []TrajectoryTurn `json:"turns"`
}

// TrajectoryTurn is one event in the run: an agent reply, a tool
// call attempt, a tool response (mocked or stubbed), a judge
// feedback message between iterations, or a system note. Role
// narrows the shape:
//
//	role=user      content = the description (opening event)
//	role=agent     content = the agent's reply text
//	role=tool      tool_call = the call + response that just happened
//	role=judge     content = judge feedback fed back between iterations,
//	               plus iteration = the attempt number it preceded
//	role=system    content = a runner note (e.g. "max_turns reached",
//	               "iteration 2 of 5", "applied directive edit: ...")
type TrajectoryTurn struct {
	Role      string          `json:"role"`
	Content   string          `json:"content,omitempty"`
	ToolCall  *ToolCallRecord `json:"tool_call,omitempty"`
	Iteration int             `json:"iteration,omitempty"` // populated on judge + system turns that mark a boundary
	Timestamp time.Time       `json:"ts"`
}

// ToolCallRecord captures one MCP call as the gateway saw it.
// Mocked=true means we returned the eval's mock; Mocked=false with
// a non-nil Response means we returned the stub-ok default. Real
// app calls never happen during eval runs.
type ToolCallRecord struct {
	App      string          `json:"app"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
	Mocked   bool            `json:"mocked"`
	Warning  string          `json:"warning,omitempty"`
}

// JudgeVerdict is the meta-agent's grading output. Overall is a
// rollup over PerGoal; even one fail makes Overall=fail. Reasoning
// is a one-paragraph human-readable summary.
//
// SuggestedImprovements is the structured proposal the judge emits
// alongside the verdict when the verdict is fail. The runner reads
// it to drive the improvement loop:
//   - InRunFeedback   → posted as a follow-up message in the same
//     thread so the agent can address it without
//     respawning. Cheap.
//   - DirectiveEdits  → applied ephemerally on respawn for the next
//     iteration (don't touch the live agent).
//   - MissingCapabilities → reserved for the simulator+catalog pass;
//     judge returns []  in this release.
type JudgeVerdict struct {
	Overall               string            `json:"overall"` // 'pass' | 'fail'
	Reasoning             string            `json:"reasoning"`
	PerGoal               []GoalVerdict     `json:"per_goal"`
	SuggestedImprovements *JudgeSuggestions `json:"suggested_improvements,omitempty"`
	JudgeModel            string            `json:"judge_model,omitempty"`
	JudgeTokens           *TokenUsage       `json:"judge_tokens,omitempty"`
}

// JudgeSuggestions is the judge's per-iteration improvement bundle.
// Distinct from RunSuggestions (which rolls up everything across
// the run) — this is what the judge emits for one verdict.
type JudgeSuggestions struct {
	InRunFeedback       string                        `json:"in_run_feedback,omitempty"`
	DirectiveEdits      []DirectiveEditSuggestion     `json:"directive_edits,omitempty"`
	MissingCapabilities []MissingCapabilitySuggestion `json:"missing_capabilities,omitempty"`
}

// GoalVerdict is one row of the rubric grading. Why is the
// judge's natural-language reasoning for this specific goal.
type GoalVerdict struct {
	Goal    string `json:"goal"`
	Verdict string `json:"verdict"` // 'pass' | 'fail'
	Why     string `json:"why"`
}

// TokenUsage is the LLM cost record for the judge call. Surfaced
// in PR-2 when continuous evals get a cost dashboard; recorded now
// so PR-2 has the history to draw from.
type TokenUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// SuggestedEval is the seed shape on AgentTemplate.SuggestedEvals.
// Becomes one agent_evals row at agent-create time.
//
// Description is the new primary field. Templates that still ship
// the legacy Trigger get an auto-derived description at seed time
// via triggerToText; new templates set Description directly and
// leave Trigger empty.
type SuggestedEval struct {
	ID          string      `json:"id"` // stable per-template; "<template_id>:default"
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Trigger     EvalTrigger `json:"trigger,omitempty"` // legacy; backfilled to description at seed time
	Goals       []string    `json:"goals"`
	Mocks       []EvalMock  `json:"mocks"`
	MaxTurns    int         `json:"max_turns,omitempty"`
}

// ─── Store helpers ─────────────────────────────────────────────────

// ListAgentEvals returns every eval for an agent ordered by
// sort_order then name. last_status is the cached rollup; the
// dashboard renders a green/red dot from it without fetching runs.
func (s *Store) ListAgentEvals(agentID int64) ([]Eval, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, name, description, trigger_json, goals_json, mocks_json,
		       max_turns, schedule, last_status, last_run_at,
		       source, source_ref, sort_order, created_at, updated_at
		  FROM agent_evals
		 WHERE agent_id = ?
		 ORDER BY sort_order ASC, name ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Eval{}
	for rows.Next() {
		e, err := scanEval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetAgentEval fetches a single eval by id. Agent-id check is
// implicit at the handler layer (auth + ownership).
func (s *Store) GetAgentEval(id string) (*Eval, error) {
	row := s.db.QueryRow(`
		SELECT id, agent_id, name, description, trigger_json, goals_json, mocks_json,
		       max_turns, schedule, last_status, last_run_at,
		       source, source_ref, sort_order, created_at, updated_at
		  FROM agent_evals
		 WHERE id = ?`,
		id,
	)
	e, err := scanEval(row)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// CreateAgentEval inserts a new eval. ID generation is left to the
// caller (the runner uses a stable id for template-seeded rows so
// re-seeds idempotently match; the dashboard's "+ Add eval" call
// pre-fills an id like "usr-<agent>-<slug>").
//
// New rows write Description directly. Trigger is left empty for
// new rows; legacy code paths that still produce a Trigger get
// auto-backfilled via triggerToText before insert so the row always
// has a usable Description.
func (s *Store) CreateAgentEval(e Eval) (*Eval, error) {
	if e.Description == "" && e.Trigger.Type != "" {
		e.Description = triggerToText(e.Trigger)
	}
	triggerJSON, _ := json.Marshal(e.Trigger)
	goalsJSON, _ := json.Marshal(e.Goals)
	if e.Goals == nil {
		goalsJSON = []byte("[]")
	}
	mocksJSON, _ := json.Marshal(e.Mocks)
	if e.Mocks == nil {
		mocksJSON = []byte("[]")
	}
	if e.MaxTurns <= 0 {
		e.MaxTurns = 5
	}
	if e.Schedule == "" {
		e.Schedule = "manual"
	}
	if e.Source == "" {
		e.Source = "user"
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_evals
			(id, agent_id, name, description, trigger_json, goals_json, mocks_json,
			 max_turns, schedule, source, source_ref, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AgentID, e.Name, e.Description, string(triggerJSON), string(goalsJSON),
		string(mocksJSON), e.MaxTurns, e.Schedule, e.Source, e.SourceRef,
		e.SortOrder,
	)
	if err != nil {
		return nil, err
	}
	return s.GetAgentEval(e.ID)
}

// UpsertSeedAgentEval is the idempotent insert for template-shipped
// starter evals. Same INSERT-OR-IGNORE discipline as the template
// seed itself: operator edits to existing rows survive, new
// platform-shipped starter evals (under a fresh id) get added on
// upgrade.
func (s *Store) UpsertSeedAgentEval(e Eval) error {
	if e.Description == "" && e.Trigger.Type != "" {
		e.Description = triggerToText(e.Trigger)
	}
	triggerJSON, _ := json.Marshal(e.Trigger)
	goalsJSON, _ := json.Marshal(e.Goals)
	if e.Goals == nil {
		goalsJSON = []byte("[]")
	}
	mocksJSON, _ := json.Marshal(e.Mocks)
	if e.Mocks == nil {
		mocksJSON = []byte("[]")
	}
	if e.MaxTurns <= 0 {
		e.MaxTurns = 5
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO agent_evals
			(id, agent_id, name, description, trigger_json, goals_json, mocks_json,
			 max_turns, schedule, source, source_ref, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'manual', 'template', ?, ?)`,
		e.ID, e.AgentID, e.Name, e.Description, string(triggerJSON), string(goalsJSON),
		string(mocksJSON), e.MaxTurns, e.SourceRef, e.SortOrder,
	)
	return err
}

// UpdateAgentEval saves edits from the wizard or the agent detail
// page. Touches the JSON columns + name + description + max_turns +
// schedule — not source/source_ref (operator can't reassign provenance)
// and not last_status/last_run_at (those are runner-owned).
func (s *Store) UpdateAgentEval(e Eval) error {
	if e.Description == "" && e.Trigger.Type != "" {
		e.Description = triggerToText(e.Trigger)
	}
	triggerJSON, _ := json.Marshal(e.Trigger)
	goalsJSON, _ := json.Marshal(e.Goals)
	if e.Goals == nil {
		goalsJSON = []byte("[]")
	}
	mocksJSON, _ := json.Marshal(e.Mocks)
	if e.Mocks == nil {
		mocksJSON = []byte("[]")
	}
	if e.MaxTurns <= 0 {
		e.MaxTurns = 5
	}
	_, err := s.db.Exec(`
		UPDATE agent_evals
		   SET name = ?, description = ?, trigger_json = ?, goals_json = ?, mocks_json = ?,
		       max_turns = ?, schedule = ?, sort_order = ?,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		e.Name, e.Description, string(triggerJSON), string(goalsJSON), string(mocksJSON),
		e.MaxTurns, e.Schedule, e.SortOrder, e.ID,
	)
	return err
}

// DeleteAgentEval removes the row + its run history. The schema
// declares ON DELETE CASCADE on agent_eval_runs.eval_id, but
// store-wide PRAGMA foreign_keys is OFF (a pre-existing schema bug
// elsewhere — app_agent_bindings.agent_id REFERENCES instances(id),
// a long-renamed table — would break unrelated deletes if we
// enabled it). So the cleanup happens explicitly here.
func (s *Store) DeleteAgentEval(id string) error {
	if _, err := s.db.Exec(`DELETE FROM agent_eval_runs WHERE eval_id = ?`, id); err != nil {
		return fmt.Errorf("delete child runs: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM agent_evals WHERE id = ?`, id); err != nil {
		return err
	}
	return nil
}

// RollupEvalLastRun is called by the runner after each run to keep
// agent_evals.last_status + last_run_at in sync. Cheap denorm so
// list queries don't have to join runs.
func (s *Store) RollupEvalLastRun(evalID, status string, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE agent_evals SET last_status = ?, last_run_at = ? WHERE id = ?`,
		status, at, evalID,
	)
	return err
}

// InsertDirectiveHistory writes one row to the agent_directive_history
// audit trail. Called by the apply-improvements handler whenever an
// eval-judge directive edit is persisted to a live agent. source is
// the provenance discriminator ('eval_suggestion' for now; future
// 'manual_edit' once we route the dashboard's directive editor
// through this same audit trail). sourceEvalRunID is the run row id
// that produced the suggestion — 0 for non-eval sources.
func (s *Store) InsertDirectiveHistory(agentID int64, before, after, source string, sourceEvalRunID int64, appliedByUserID int64) error {
	var runRef sql.NullInt64
	if sourceEvalRunID > 0 {
		runRef = sql.NullInt64{Int64: sourceEvalRunID, Valid: true}
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_directive_history
			(agent_id, directive_before, directive_after, source,
			 source_eval_run_id, applied_by_user_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		agentID, before, after, source, runRef, appliedByUserID,
	)
	return err
}

// InsertEvalRun writes one history row. Returns the autogenerated
// id so the runner can include it in the synchronous response.
// suggestions_json is nullable — strict single-shot runs that pass
// first try have no suggestions and we keep the column NULL rather
// than writing an empty JSON object.
func (s *Store) InsertEvalRun(r EvalRun) (int64, error) {
	trajectoryJSON, _ := json.Marshal(r.Trajectory)
	var verdictJSON sql.NullString
	if r.Verdict != nil {
		b, _ := json.Marshal(r.Verdict)
		verdictJSON = sql.NullString{String: string(b), Valid: true}
	}
	var suggestionsJSON sql.NullString
	if r.Suggestions != nil && (len(r.Suggestions.DirectiveEdits) > 0 || len(r.Suggestions.MissingCapabilities) > 0) {
		b, _ := json.Marshal(r.Suggestions)
		suggestionsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if r.IterationsUsed <= 0 {
		r.IterationsUsed = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO agent_eval_runs
			(eval_id, started_at, finished_at, status, trajectory_json,
			 judge_verdict_json, suggestions_json, duration_ms, turns_used,
			 iterations_used, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EvalID, r.StartedAt, r.FinishedAt, r.Status, string(trajectoryJSON),
		verdictJSON, suggestionsJSON, r.DurationMS, r.TurnsUsed,
		r.IterationsUsed, r.ErrorMessage,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetEvalRun fetches one run row by id. Used by the apply-improvements
// handler to load the suggestions_json the operator is acting on.
func (s *Store) GetEvalRun(runID int64) (*EvalRun, error) {
	row := s.db.QueryRow(`
		SELECT id, eval_id, started_at, finished_at, status,
		       trajectory_json, judge_verdict_json, suggestions_json,
		       COALESCE(duration_ms, 0), COALESCE(turns_used, 0),
		       COALESCE(iterations_used, 1), COALESCE(error_message, '')
		  FROM agent_eval_runs
		 WHERE id = ?`,
		runID,
	)
	r, err := scanEvalRun(row)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListEvalRuns returns the most-recent runs for one eval. The
// dashboard caps at 10 by default; future paginates.
func (s *Store) ListEvalRuns(evalID string, limit int) ([]EvalRun, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(`
		SELECT id, eval_id, started_at, finished_at, status,
		       trajectory_json, judge_verdict_json, suggestions_json,
		       COALESCE(duration_ms, 0), COALESCE(turns_used, 0),
		       COALESCE(iterations_used, 1), COALESCE(error_message, '')
		  FROM agent_eval_runs
		 WHERE eval_id = ?
		 ORDER BY started_at DESC
		 LIMIT ?`,
		evalID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvalRun{}
	for rows.Next() {
		r, err := scanEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanEvalRun(r rowScanner) (EvalRun, error) {
	var (
		run             EvalRun
		startedAt       string
		finishedAt      sql.NullString
		trajectoryJSON  string
		verdictJSON     sql.NullString
		suggestionsJSON sql.NullString
	)
	if err := r.Scan(
		&run.ID, &run.EvalID, &startedAt, &finishedAt, &run.Status,
		&trajectoryJSON, &verdictJSON, &suggestionsJSON,
		&run.DurationMS, &run.TurnsUsed, &run.IterationsUsed,
		&run.ErrorMessage,
	); err != nil {
		return run, err
	}
	run.StartedAt, _ = parseTime(startedAt)
	if finishedAt.Valid {
		t, _ := parseTime(finishedAt.String)
		run.FinishedAt = &t
	}
	_ = json.Unmarshal([]byte(trajectoryJSON), &run.Trajectory)
	if verdictJSON.Valid {
		var v JudgeVerdict
		if err := json.Unmarshal([]byte(verdictJSON.String), &v); err == nil {
			run.Verdict = &v
		}
	}
	if suggestionsJSON.Valid {
		var sg RunSuggestions
		if err := json.Unmarshal([]byte(suggestionsJSON.String), &sg); err == nil {
			run.Suggestions = &sg
		}
	}
	return run, nil
}

// ─── HTTP handlers ─────────────────────────────────────────────────

// handleAgentEvals dispatches every /instances/:agentId/evals[/...]
// route after the main router has already normalised /agents/ →
// /instances/. Sub-routes:
//
//	GET    /instances/:agentId/evals                 — list
//	POST   /instances/:agentId/evals                 — create
//	GET    /instances/:agentId/evals/:evalId         — get one
//	PUT    /instances/:agentId/evals/:evalId         — update
//	DELETE /instances/:agentId/evals/:evalId         — remove
//	POST   /instances/:agentId/evals/:evalId/run     — execute now
//	GET    /instances/:agentId/evals/:evalId/runs    — run history
//
// agentId ownership is checked via getUserID + ListAgents lookup;
// returns 404 if the caller doesn't own the agent, so the eval
// table isn't readable cross-user even by id.
func (s *Server) handleAgentEvals(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	// Path shapes we accept here (post /agents→/instances rewrite):
	//   /instances/<agentId>/evals
	//   /instances/<agentId>/evals/<evalId>
	//   /instances/<agentId>/evals/<evalId>/run
	//   /instances/<agentId>/evals/<evalId>/runs
	trimmed := strings.TrimPrefix(r.URL.Path, "/instances/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[1] != "evals" {
		http.NotFound(w, r)
		return
	}
	agentID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	// Authorise: the agent must belong to this user. Cheap row read.
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil || agent == nil || agent.UserID != userID {
		http.NotFound(w, r)
		return
	}

	// Collection: /instances/:agentId/evals
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			list, err := s.store.ListAgentEvals(agentID)
			if err != nil {
				http.Error(w, "list evals: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, list)
		case http.MethodPost:
			var body Eval
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(body.Name) == "" {
				http.Error(w, "name required", http.StatusBadRequest)
				return
			}
			body.AgentID = agentID
			body.Source = "user"
			if body.ID == "" {
				body.ID = "usr-" + i64s(agentID) + ":" + slugify(body.Name) + "-" + i64s(time.Now().Unix())
			}
			e, err := s.store.CreateAgentEval(body)
			if err != nil {
				http.Error(w, "create eval: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, e)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}

	// Beyond this point we always have an eval id.
	evalID := parts[2]
	existing, err := s.store.GetAgentEval(evalID)
	if err != nil || existing == nil || existing.AgentID != agentID {
		http.NotFound(w, r)
		return
	}

	// /instances/:agentId/evals/:evalId/run/stream — SSE variant of /run
	// for interactive step-mode. See eval_streaming.go for the framing
	// and the matching POST /eval-runs/:run_id/step control endpoint.
	if len(parts) == 5 && parts[3] == "run" && parts[4] == "stream" {
		s.handleEvalRunStream(w, r, userID, agent, existing)
		return
	}

	// /instances/:agentId/evals/:evalId/run
	if len(parts) == 4 && parts[3] == "run" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Default: strict single-shot. Operators opting into the
		// improvement loop pass {max_iterations: N, strict_mocks: bool}
		// in the body. Caps + safety bounds applied by runRealEvalCore.
		opts := RunOptions{MaxIterations: 1}
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&opts)
		}
		// Iteration runs can take much longer than single-shot ones
		// (each iteration is its own spawn + judge pass). Budget 90s
		// per iteration up to a ceiling so the operator's wizard
		// step doesn't hang indefinitely.
		budget := 90 * time.Second
		if opts.MaxIterations > 1 {
			budget = time.Duration(opts.MaxIterations) * 90 * time.Second
			if budget > 8*time.Minute {
				budget = 8 * time.Minute
			}
		}
		// A environment run builds the agent's apps from source, spawns real
		// sidecars + a fresh core, then drives + judges — far more than a
		// mock-gateway single-shot. Give it room.
		if opts.UseEnvironment && budget < 8*time.Minute {
			budget = 8 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		run, err := s.runEval(ctx, userID, agent, existing, opts)
		if err != nil {
			http.Error(w, "run eval: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, run)
		return
	}

	// /instances/:agentId/evals/:evalId/runs
	if len(parts) == 4 && parts[3] == "runs" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		runs, err := s.store.ListEvalRuns(evalID, 10)
		if err != nil {
			http.Error(w, "list runs: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, runs)
		return
	}

	// /instances/:agentId/evals/:evalId/runs/:runId/apply
	if len(parts) == 6 && parts[3] == "runs" && parts[5] == "apply" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		runID, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil {
			http.Error(w, "bad run id", http.StatusBadRequest)
			return
		}
		s.handleApplyEvalSuggestions(w, r, userID, agent, existing, runID)
		return
	}

	// /instances/:agentId/evals/:evalId
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, existing)
		case http.MethodPut:
			var body Eval
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			body.ID = evalID
			body.AgentID = agentID
			body.Source = existing.Source // operator can't reassign provenance
			body.SourceRef = existing.SourceRef
			if err := s.store.UpdateAgentEval(body); err != nil {
				http.Error(w, "update eval: "+err.Error(), http.StatusInternalServerError)
				return
			}
			updated, _ := s.store.GetAgentEval(evalID)
			writeJSON(w, updated)
		case http.MethodDelete:
			if err := s.store.DeleteAgentEval(evalID); err != nil {
				http.Error(w, "delete eval: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
		}
		return
	}

	http.NotFound(w, r)
}

// handleEvalMockGateway is the HTTP MCP endpoint that spawned eval
// cores talk to instead of the real apteva-server gateway. URL
// shape: /api/eval-mock-gateway/<session_token>/mcp (or whatever
// path; we only care about session_token). Routes JSON-RPC
// initialize/tools/list/tools/call requests against the session's
// mocks + records each call into the trajectory buffer.
//
// Unauthenticated: the eval session token IS the credential — a
// 16-byte random suffix on URLs that are only ever local-loopback.
// Anyone who could forge a token already has filesystem access to
// the apteva-server's process memory.
func (s *Server) handleEvalMockGateway(w http.ResponseWriter, r *http.Request) {
	// Extract the token from /api/eval-mock-gateway/<token>[/...]
	trimmed := strings.TrimPrefix(r.URL.Path, "/eval-mock-gateway/")
	// After /api stripper applies. /api/eval-mock-gateway/<token>/mcp.
	token := trimmed
	if i := strings.Index(token, "/"); i >= 0 {
		token = token[:i]
	}
	if token == "" {
		http.Error(w, "missing session token", http.StatusBadRequest)
		return
	}
	session := lookupEvalSession(token)
	if session == nil {
		http.Error(w, "unknown eval session — perhaps it ended", http.StatusNotFound)
		return
	}

	// Accept only POST + JSON. Other shapes shouldn't reach here.
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	respond := func(result any, errMsg string) {
		resp := map[string]any{"jsonrpc": "2.0"}
		if len(req.ID) > 0 {
			resp["id"] = req.ID
		}
		if errMsg != "" {
			resp["error"] = map[string]any{"code": -32603, "message": errMsg}
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}

	switch req.Method {
	case "initialize":
		respond(map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "apteva-eval-mocks", "version": "1.0.0"},
		}, "")
	case "tools/list":
		// Synthesise a tool entry per unique (app, tool) in the eval's
		// mocks. Names are namespaced "<app>.<tool>" so the agent's
		// directive language ("send_message via the messaging app")
		// has matching candidates. PR-2 will accept richer schemas
		// (input args, output shape) from the mock entry itself.
		seen := map[string]bool{}
		var tools []map[string]any
		for _, m := range session.eval.Mocks {
			key := m.App + "." + m.Tool
			if seen[key] {
				continue
			}
			seen[key] = true
			tools = append(tools, map[string]any{
				"name":        key,
				"description": fmt.Sprintf("Mocked %s tool in the %s app — this is an eval sandbox; the response is canned.", m.Tool, m.App),
				"inputSchema": map[string]any{"type": "object", "additionalProperties": true},
			})
		}
		respond(map[string]any{"tools": tools}, "")
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			respond(nil, "invalid tools/call params")
			return
		}
		// Tool name is "<app>.<tool>" — split back into the two.
		app := p.Name
		tool := ""
		if dot := strings.Index(p.Name, "."); dot > 0 {
			app = p.Name[:dot]
			tool = p.Name[dot+1:]
		}
		rec := session.resolveToolCall(app, tool, p.Arguments)
		// MCP tool responses are conventionally returned as a
		// content array with a single "text" part holding the
		// inner JSON payload. The agent's parser unwraps that and
		// gets back what the mock's return field declared.
		var inner string
		if rec.Error != "" {
			respond(nil, rec.Error)
			return
		}
		inner = string(rec.Response)
		respond(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": inner},
			},
		}, "")
	default:
		respond(nil, "method not found: "+req.Method)
	}
}

// handleEvalPreview runs an eval against an in-memory draft agent
// without persisting anything. The wizard's Verify step calls this
// so operators can iterate on directive + goals before the agent
// row exists. Request body shape:
//
//	{
//	  "directive": "<wizard's current directive>",
//	  "name":      "<wizard's current name, used only in the system prompt>",
//	  "project_id": "<scope for picking the LLM provider>",
//	  "eval": { "name", "description", "goals", "mocks", "max_turns" },
//	  "options": { "max_iterations": N, "strict_mocks": bool } // optional
//	}
//
// Default RunOptions for preview: MaxIterations=5. The wizard wants
// the agent to have multiple shots at the spec so the operator sees
// what improvements would help; if the first attempt passes, the
// loop exits early on iteration 1.
//
// Returns the same EvalRun shape as the persisted runner, with ID=0
// to signal "not stored — call POST /agents/:id/evals to save and
// /agents/:id/evals/:id/run to re-run after the agent exists".
func (s *Server) handleEvalPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	var body struct {
		Directive string      `json:"directive"`
		Name      string      `json:"name"`
		ProjectID string      `json:"project_id"`
		Eval      Eval        `json:"eval"`
		Options   *RunOptions `json:"options,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Directive) == "" {
		http.Error(w, "directive required for preview", http.StatusBadRequest)
		return
	}
	if len(body.Eval.Goals) == 0 {
		http.Error(w, "at least one goal required", http.StatusBadRequest)
		return
	}
	if body.Eval.MaxTurns <= 0 {
		body.Eval.MaxTurns = 5
	}
	// Backfill description from trigger for callers still on the
	// old wizard payload shape.
	if body.Eval.Description == "" && body.Eval.Trigger.Type != "" {
		body.Eval.Description = triggerToText(body.Eval.Trigger)
	}
	if strings.TrimSpace(body.Eval.Description) == "" {
		http.Error(w, "description required for preview", http.StatusBadRequest)
		return
	}
	opts := RunOptions{MaxIterations: 5}
	if body.Options != nil {
		opts = *body.Options
		if opts.MaxIterations <= 0 {
			opts.MaxIterations = 5
		}
	}
	draft := &Agent{
		Name:      body.Name,
		Directive: body.Directive,
		ProjectID: body.ProjectID,
		UserID:    userID,
	}
	// Generous budget for the multi-iteration default — 90s per
	// iteration up to 8 min ceiling. Wizard renders a spinner.
	budget := time.Duration(opts.MaxIterations) * 90 * time.Second
	if budget > 8*time.Minute {
		budget = 8 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	run, err := s.previewEval(ctx, userID, body.ProjectID, draft, &body.Eval, opts)
	if err != nil {
		http.Error(w, "preview eval: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, run)
}

// handleApplyEvalSuggestions persists a selection of the judge's
// directive-edit suggestions onto the live agent's stored directive.
// The operator picks which suggestion ids to apply; the rest are
// discarded. Each applied edit is appended to agent.Directive, or
// into # Learning for structured markdown directives, and a row is
// written to agent_directive_history for audit.
//
// Body shape:
//
//	{
//	  "directive_edit_ids": ["edit-1", "edit-3"]   // ids from RunSuggestions.DirectiveEdits
//	}
//
// We do NOT apply MissingCapabilities here — that pathway lands in
// a follow-up release with the simulator + marketplace catalog.
// Apply requests that name a missing-capability id return 400 for
// now so the operator gets an explicit signal rather than silent
// no-op.
func (s *Server) handleApplyEvalSuggestions(w http.ResponseWriter, r *http.Request, userID int64, agent *Agent, ev *Eval, runID int64) {
	run, err := s.store.GetEvalRun(runID)
	if err != nil || run == nil || run.EvalID != ev.ID {
		http.NotFound(w, r)
		return
	}
	if run.Suggestions == nil {
		http.Error(w, "no suggestions on this run", http.StatusBadRequest)
		return
	}
	var body struct {
		DirectiveEditIDs []string `json:"directive_edit_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(body.DirectiveEditIDs) == 0 {
		http.Error(w, "directive_edit_ids must be non-empty", http.StatusBadRequest)
		return
	}
	wanted := map[string]bool{}
	for _, id := range body.DirectiveEditIDs {
		wanted[id] = true
	}
	var toAppend []string
	for _, edit := range run.Suggestions.DirectiveEdits {
		if wanted[edit.ID] {
			toAppend = append(toAppend, strings.TrimSpace(edit.Add))
			delete(wanted, edit.ID)
		}
	}
	if len(wanted) > 0 {
		// Operator asked for ids that aren't on this run. Reject so
		// the dashboard rebuilds its UI state instead of half-applying.
		http.Error(w, "unknown directive_edit_ids on this run", http.StatusBadRequest)
		return
	}
	if len(toAppend) == 0 {
		http.Error(w, "no directive edits resolved", http.StatusBadRequest)
		return
	}

	before := agent.Directive
	after := appendDirectiveLearning(before, toAppend)
	agent.Directive = after
	if err := s.store.UpdateAgent(agent); err != nil {
		http.Error(w, "save directive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.InsertDirectiveHistory(agent.ID, before, after, "eval_suggestion", runID, userID); err != nil {
		// History row failed but directive change succeeded — surface
		// the failure so the operator knows audit is degraded, don't
		// roll back the user-visible change.
		http.Error(w, "directive applied; audit history failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"status":           "applied",
		"agent_id":         agent.ID,
		"directive_before": before,
		"directive_after":  after,
		"edits_applied":    len(toAppend),
	})
}

// seedTemplateEvalsForAgent copies the template's SuggestedEvals
// into agent_evals for the newly-created agent. Idempotent — uses
// the SuggestedEval ID prefixed with the agent id, so re-runs
// (e.g. an upgrade that adds a new starter eval to the template)
// only add what's missing without trampling operator edits to
// existing rows.
func (s *Server) seedTemplateEvalsForAgent(agentID int64, templateID string) {
	var tpl *AgentTemplate
	for i := range builtinAgentTemplates {
		if builtinAgentTemplates[i].ID == templateID {
			tpl = &builtinAgentTemplates[i]
			break
		}
	}
	if tpl == nil || len(tpl.SuggestedEvals) == 0 {
		return
	}
	for i, se := range tpl.SuggestedEvals {
		seedID := se.ID
		if seedID == "" {
			seedID = templateID + ":default"
		}
		// Namespace the row id with the agent id so two agents
		// created from the same template don't share an id.
		rowID := seedID + ":" + i64s(agentID)
		_ = s.store.UpsertSeedAgentEval(Eval{
			ID:          rowID,
			AgentID:     agentID,
			Name:        se.Name,
			Description: se.Description,
			Trigger:     se.Trigger,
			Goals:       se.Goals,
			Mocks:       se.Mocks,
			MaxTurns:    se.MaxTurns,
			Source:      "template",
			SourceRef:   templateID,
			SortOrder:   100 + i,
		})
	}
}

// scanEval reads one agent_evals row. Same rowScanner pattern as
// scanAgentTemplate so it works for both QueryRow and Query.Next.
//
// Legacy-row handling: rows written under the older trigger-only
// schema have description=” and a populated trigger_json. We derive
// description from trigger on read so the runner + handlers never
// have to know about the legacy shape.
func scanEval(r rowScanner) (Eval, error) {
	var (
		e           Eval
		description string
		triggerJSON string
		goalsJSON   string
		mocksJSON   string
		lastStatus  sql.NullString
		lastRunAt   sql.NullString
		createdAt   string
		updatedAt   string
	)
	if err := r.Scan(
		&e.ID, &e.AgentID, &e.Name, &description, &triggerJSON, &goalsJSON, &mocksJSON,
		&e.MaxTurns, &e.Schedule, &lastStatus, &lastRunAt,
		&e.Source, &e.SourceRef, &e.SortOrder, &createdAt, &updatedAt,
	); err != nil {
		return e, err
	}
	e.Description = description
	_ = json.Unmarshal([]byte(triggerJSON), &e.Trigger)
	if e.Trigger.Payload == nil {
		e.Trigger.Payload = map[string]any{}
	}
	if e.Description == "" && e.Trigger.Type != "" {
		e.Description = triggerToText(e.Trigger)
	}
	_ = json.Unmarshal([]byte(goalsJSON), &e.Goals)
	if e.Goals == nil {
		e.Goals = []string{}
	}
	_ = json.Unmarshal([]byte(mocksJSON), &e.Mocks)
	if e.Mocks == nil {
		e.Mocks = []EvalMock{}
	}
	if lastStatus.Valid {
		e.LastStatus = lastStatus.String
	}
	if lastRunAt.Valid {
		t, _ := parseTime(lastRunAt.String)
		e.LastRunAt = &t
	}
	e.CreatedAt, _ = parseTime(createdAt)
	e.UpdatedAt, _ = parseTime(updatedAt)
	return e, nil
}
