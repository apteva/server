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
type Eval struct {
	ID          string       `json:"id"`
	AgentID     int64        `json:"agent_id"`
	Name        string       `json:"name"`
	Trigger     EvalTrigger  `json:"trigger"`
	Goals       []string     `json:"goals"`
	Mocks       []EvalMock   `json:"mocks"`
	MaxTurns    int          `json:"max_turns"`
	Schedule    string       `json:"schedule"`
	LastStatus  string       `json:"last_status,omitempty"`
	LastRunAt   *time.Time   `json:"last_run_at,omitempty"`
	Source      string       `json:"source"`      // 'user' | 'template' | 'app'
	SourceRef   string       `json:"source_ref,omitempty"`
	SortOrder   int          `json:"sort_order"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// EvalTrigger describes the situation we drive the agent with for
// this test. PR-1 supports three types; future PRs add more:
//
//   type=chat_message   payload: {text, from?, channel?}
//   type=webhook        payload: {url, method, body}
//   type=scheduled_tick payload: {iso_time}
type EvalTrigger struct {
	Type    string                 `json:"type"`
	Payload map[string]any         `json:"payload,omitempty"`
}

// EvalMock declares how a single tool should answer in this eval's
// sandboxed run. First match wins on (App, Tool, optional ArgsMatch).
// One of Return or Error must be set; Return is the normal path,
// Error is the negative-path-testing path that surfaces an MCP error
// back to the agent.
type EvalMock struct {
	App       string         `json:"app"`
	Tool      string         `json:"tool"`
	ArgsMatch map[string]any `json:"args_match,omitempty"`
	Return    json.RawMessage `json:"return,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// EvalRun is one entry in the history. trajectory + verdict are
// embedded so the dashboard can render a run inline without a
// second fetch.
type EvalRun struct {
	ID            int64           `json:"id"`
	EvalID        string          `json:"eval_id"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at,omitempty"`
	Status        string          `json:"status"`      // 'pass' | 'fail' | 'error'
	Trajectory    Trajectory      `json:"trajectory"`
	Verdict       *JudgeVerdict   `json:"verdict,omitempty"`
	DurationMS    int             `json:"duration_ms"`
	TurnsUsed     int             `json:"turns_used"`
	ErrorMessage  string          `json:"error_message,omitempty"`
}

// Trajectory is the full record of one eval run. Stitched from
// (a) the spawned core's thread message history and (b) the
// gateway's tool-call recorder, ordered by ts.
type Trajectory struct {
	Turns []TrajectoryTurn `json:"turns"`
}

// TrajectoryTurn is one event in the run: an agent reply, a tool
// call attempt, a tool response (mocked or stubbed), or a system
// note. Role narrows the shape:
//   role=user   content = the trigger message
//   role=agent  content = the agent's reply text
//   role=tool   tool_call = the call + response that just happened
//   role=system content = a runner note (e.g. "max_turns reached")
type TrajectoryTurn struct {
	Role     string          `json:"role"`
	Content  string          `json:"content,omitempty"`
	ToolCall *ToolCallRecord `json:"tool_call,omitempty"`
	Timestamp time.Time      `json:"ts"`
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
type JudgeVerdict struct {
	Overall    string         `json:"overall"`   // 'pass' | 'fail'
	Reasoning  string         `json:"reasoning"`
	PerGoal    []GoalVerdict  `json:"per_goal"`
	JudgeModel string         `json:"judge_model,omitempty"`
	JudgeTokens *TokenUsage   `json:"judge_tokens,omitempty"`
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
type SuggestedEval struct {
	ID       string        `json:"id"`       // stable per-template; "<template_id>:default"
	Name     string        `json:"name"`
	Trigger  EvalTrigger   `json:"trigger"`
	Goals    []string      `json:"goals"`
	Mocks    []EvalMock    `json:"mocks"`
	MaxTurns int           `json:"max_turns,omitempty"`
}

// ─── Store helpers ─────────────────────────────────────────────────

// ListAgentEvals returns every eval for an agent ordered by
// sort_order then name. last_status is the cached rollup; the
// dashboard renders a green/red dot from it without fetching runs.
func (s *Store) ListAgentEvals(agentID int64) ([]Eval, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, name, trigger_json, goals_json, mocks_json,
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
		SELECT id, agent_id, name, trigger_json, goals_json, mocks_json,
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
func (s *Store) CreateAgentEval(e Eval) (*Eval, error) {
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
			(id, agent_id, name, trigger_json, goals_json, mocks_json,
			 max_turns, schedule, source, source_ref, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AgentID, e.Name, string(triggerJSON), string(goalsJSON),
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
			(id, agent_id, name, trigger_json, goals_json, mocks_json,
			 max_turns, schedule, source, source_ref, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'manual', 'template', ?, ?)`,
		e.ID, e.AgentID, e.Name, string(triggerJSON), string(goalsJSON),
		string(mocksJSON), e.MaxTurns, e.SourceRef, e.SortOrder,
	)
	return err
}

// UpdateAgentEval saves edits from the wizard or the agent detail
// page. Touches the JSON columns + name + max_turns + schedule —
// not source/source_ref (operator can't reassign provenance) and
// not last_status/last_run_at (those are runner-owned).
func (s *Store) UpdateAgentEval(e Eval) error {
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
		   SET name = ?, trigger_json = ?, goals_json = ?, mocks_json = ?,
		       max_turns = ?, schedule = ?, sort_order = ?,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		e.Name, string(triggerJSON), string(goalsJSON), string(mocksJSON),
		e.MaxTurns, e.Schedule, e.SortOrder, e.ID,
	)
	return err
}

// DeleteAgentEval removes the row + (via FK cascade) its run
// history.
func (s *Store) DeleteAgentEval(id string) error {
	_, err := s.db.Exec(`DELETE FROM agent_evals WHERE id = ?`, id)
	return err
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

// InsertEvalRun writes one history row. Returns the autogenerated
// id so the runner can include it in the synchronous response.
func (s *Store) InsertEvalRun(r EvalRun) (int64, error) {
	trajectoryJSON, _ := json.Marshal(r.Trajectory)
	var verdictJSON sql.NullString
	if r.Verdict != nil {
		b, _ := json.Marshal(r.Verdict)
		verdictJSON = sql.NullString{String: string(b), Valid: true}
	}
	res, err := s.db.Exec(`
		INSERT INTO agent_eval_runs
			(eval_id, started_at, finished_at, status, trajectory_json,
			 judge_verdict_json, duration_ms, turns_used, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EvalID, r.StartedAt, r.FinishedAt, r.Status, string(trajectoryJSON),
		verdictJSON, r.DurationMS, r.TurnsUsed, r.ErrorMessage,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListEvalRuns returns the most-recent runs for one eval. The
// dashboard caps at 10 in PR-1; PR-2 paginates.
func (s *Store) ListEvalRuns(evalID string, limit int) ([]EvalRun, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(`
		SELECT id, eval_id, started_at, finished_at, status,
		       trajectory_json, judge_verdict_json,
		       COALESCE(duration_ms, 0), COALESCE(turns_used, 0),
		       COALESCE(error_message, '')
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
		var (
			r              EvalRun
			startedAt      string
			finishedAt     sql.NullString
			trajectoryJSON string
			verdictJSON    sql.NullString
		)
		if err := rows.Scan(
			&r.ID, &r.EvalID, &startedAt, &finishedAt, &r.Status,
			&trajectoryJSON, &verdictJSON, &r.DurationMS, &r.TurnsUsed,
			&r.ErrorMessage,
		); err != nil {
			return nil, err
		}
		r.StartedAt, _ = parseTime(startedAt)
		if finishedAt.Valid {
			t, _ := parseTime(finishedAt.String)
			r.FinishedAt = &t
		}
		_ = json.Unmarshal([]byte(trajectoryJSON), &r.Trajectory)
		if verdictJSON.Valid {
			var v JudgeVerdict
			if err := json.Unmarshal([]byte(verdictJSON.String), &v); err == nil {
				r.Verdict = &v
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ─── HTTP handlers ─────────────────────────────────────────────────

// handleAgentEvals dispatches every /instances/:agentId/evals[/...]
// route after the main router has already normalised /agents/ →
// /instances/. Sub-routes:
//
//   GET    /instances/:agentId/evals                 — list
//   POST   /instances/:agentId/evals                 — create
//   GET    /instances/:agentId/evals/:evalId         — get one
//   PUT    /instances/:agentId/evals/:evalId         — update
//   DELETE /instances/:agentId/evals/:evalId         — remove
//   POST   /instances/:agentId/evals/:evalId/run     — execute now
//   GET    /instances/:agentId/evals/:evalId/runs    — run history
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

	// /instances/:agentId/evals/:evalId/run
	if len(parts) == 4 && parts[3] == "run" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// runEval is synchronous — typical 5-30s for the simulated
		// agent loop + judge pass combined. The request hangs for
		// the duration; dashboard renders a spinner. The 90s
		// context cap matches what the operator's wizard step is
		// willing to wait for before showing a timeout error.
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		run, err := s.runEval(ctx, userID, agent, existing)
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
			body.Source = existing.Source       // operator can't reassign provenance
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
//   {
//     "directive": "<wizard's current directive>",
//     "name":      "<wizard's current name, used only in the system prompt>",
//     "project_id": "<scope for picking the LLM provider>",
//     "eval": { "name", "trigger", "goals", "mocks", "max_turns" }
//   }
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
		Directive string `json:"directive"`
		Name      string `json:"name"`
		ProjectID string `json:"project_id"`
		Eval      Eval   `json:"eval"`
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
	draft := &Agent{
		Name:      body.Name,
		Directive: body.Directive,
		ProjectID: body.ProjectID,
		UserID:    userID,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	run, err := s.previewEval(ctx, userID, body.ProjectID, draft, &body.Eval)
	if err != nil {
		http.Error(w, "preview eval: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, run)
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
			ID:        rowID,
			AgentID:   agentID,
			Name:      se.Name,
			Trigger:   se.Trigger,
			Goals:     se.Goals,
			Mocks:     se.Mocks,
			MaxTurns:  se.MaxTurns,
			Source:    "template",
			SourceRef: templateID,
			SortOrder: 100 + i,
		})
	}
}

// scanEval reads one agent_evals row. Same rowScanner pattern as
// scanAgentTemplate so it works for both QueryRow and Query.Next.
func scanEval(r rowScanner) (Eval, error) {
	var (
		e            Eval
		triggerJSON  string
		goalsJSON    string
		mocksJSON    string
		lastStatus   sql.NullString
		lastRunAt    sql.NullString
		createdAt    string
		updatedAt    string
	)
	if err := r.Scan(
		&e.ID, &e.AgentID, &e.Name, &triggerJSON, &goalsJSON, &mocksJSON,
		&e.MaxTurns, &e.Schedule, &lastStatus, &lastRunAt,
		&e.Source, &e.SourceRef, &e.SortOrder, &createdAt, &updatedAt,
	); err != nil {
		return e, err
	}
	_ = json.Unmarshal([]byte(triggerJSON), &e.Trigger)
	if e.Trigger.Payload == nil {
		e.Trigger.Payload = map[string]any{}
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
