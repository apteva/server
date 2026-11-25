package main

// Thread observation for apps: the extended topology thread list and one
// thread's recent steps. Both are read-only views over Core's live /threads
// list and the telemetry the server already stores. Fields are tiered by the
// existing permissions:
//
//   - platform.instances.read: the live tree, state and scheduling fields.
//   - telemetry.observe or telemetry.read: telemetry-derived fields (current
//     step, last activity, spawn links, ended threads) and metadata activity.
//   - telemetry.read for an agent the caller's user owns: free text
//     (purpose, blocked reason, results, reasons, arguments, errors).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/apteva/app-sdk"
)

const (
	threadSchemaVersion        = 1
	threadEndedWindowMax       = 24 * time.Hour
	threadActivityDefaultLimit = 50
	threadActivityMaxLimit     = 200
	threadPurposeRunes         = 200
	threadOpenStepMaxAge       = time.Hour // an older unmatched start is treated as lost
	threadMessageTextBytes     = 4 << 10
)

// coreThreadRow is the subset of Core's GET /threads entry the server reads.
type coreThreadRow struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id"`
	Name       string `json:"name"`
	Depth      int    `json:"depth"`
	Directive  string `json:"directive"`
	Rate       string `json:"rate"`
	NextWakeAt string `json:"next_wake_at"`
	Iteration  int    `json:"iteration"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Realtime   bool   `json:"realtime"`
	Ephemeral  bool   `json:"ephemeral"`
	StartedAt  string `json:"started_at"`
	Inference  struct {
		BlockedReason string `json:"blocked_reason"`
	} `json:"inference"`
}

// savedThreadRow is the subset of a stopped agent's saved thread config.
type savedThreadRow struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	Depth     int    `json:"depth"`
	Directive string `json:"directive"`
}

// threadAccess is what the caller may see beyond the topology tier.
type threadAccess struct {
	telemetry bool // telemetry.observe or telemetry.read
	content   bool // telemetry.read for an agent the caller's user owns
}

func (s *Server) threadAccess(p telemetryPrincipal, agentID int64) threadAccess {
	access := threadAccess{telemetry: p.observer || p.content}
	if p.content {
		_, err := s.telemetryAgents(p, sdk.TelemetryQuery{AgentID: agentID, ProjectID: p.project, Projection: "content"})
		access.content = err == nil
	}
	return access
}

// threadPurpose is the first non-empty line of the directive, without agent
// control blocks, cut to threadPurposeRunes.
func threadPurpose(directive string) string {
	for _, line := range strings.Split(withoutAgentControls(directive), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > threadPurposeRunes {
				return string(r[:threadPurposeRunes]) + "…"
			}
			return line
		}
	}
	return ""
}

func threadTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func threadIdentifier(v string) string {
	if telemetryIdentifier(v) {
		return v
	}
	return ""
}

// cutText bounds free text on a UTF-8 boundary.
func cutText(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	end := max
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + "...", true
}

// fetchCoreThreads reads Core's live thread list and pause flag.
func fetchCoreThreads(ctx context.Context, port int, key string) ([]coreThreadRow, bool, bool) {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, out any) bool {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(req)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out) == nil
	}
	var rows []coreThreadRow
	if !get("/threads", &rows) || len(rows) > 1000 {
		return nil, false, false
	}
	var status struct {
		Paused bool `json:"paused"`
	}
	get("/status", &status)
	return rows, status.Paused, true
}

// liveObservedThreads converts Core's rows. State is finished by
// applyThreadTelemetry, which knows about open steps.
func liveObservedThreads(rows []coreThreadRow, access threadAccess) []sdk.ObservedThread {
	out := make([]sdk.ObservedThread, 0, len(rows))
	for _, row := range rows {
		if len(row.ID) > 256 || len(row.ParentID) > 256 {
			continue
		}
		name, _ := cutText(row.Name, 256)
		t := sdk.ObservedThread{
			ID: row.ID, ParentID: row.ParentID, Name: name, Depth: row.Depth,
			NextWakeAt: threadTime(row.NextWakeAt), Turns: row.Iteration,
			Model: threadIdentifier(row.Model), Provider: threadIdentifier(row.Provider),
			Realtime: row.Realtime, Ephemeral: row.Ephemeral, StartedAt: threadTime(row.StartedAt),
		}
		if row.Inference.BlockedReason != "" {
			t.State = sdk.ThreadStateBlocked
		}
		if row.Rate == "stopped" {
			t.State = sdk.ThreadStateStopped
		}
		if access.content {
			t.Purpose = threadPurpose(row.Directive)
			t.BlockedReason, _ = cutText(row.Inference.BlockedReason, 1000)
		}
		out = append(out, t)
	}
	return out
}

func savedObservedThreads(config string, access threadAccess) ([]sdk.ObservedThread, bool) {
	var cfg struct {
		Directive string           `json:"directive"`
		Threads   []savedThreadRow `json:"threads"`
	}
	if json.Unmarshal([]byte(config), &cfg) != nil {
		return nil, false
	}
	main := sdk.ObservedThread{ID: "main", State: sdk.ThreadStateStopped}
	out := []sdk.ObservedThread{main}
	for _, row := range cfg.Threads {
		if row.ID == "" || len(row.ID) > 256 || len(row.ParentID) > 256 {
			continue
		}
		name, _ := cutText(row.Name, 256)
		t := sdk.ObservedThread{ID: row.ID, ParentID: row.ParentID, Name: name, Depth: row.Depth, State: sdk.ThreadStateStopped}
		if t.ParentID == "" {
			t.ParentID = "main"
		}
		if access.content {
			t.Purpose = threadPurpose(row.Directive)
		}
		out = append(out, t)
	}
	if access.content {
		out[0].Purpose = threadPurpose(cfg.Directive)
	}
	return out, true
}

// threadEventRow is one stored telemetry row with its decoded payload.
type threadEventRow struct {
	row    int64
	thread string
	kind   string
	at     time.Time
	data   map[string]any
}

func (e threadEventRow) str(key string) string {
	v, _ := e.data[key].(string)
	return v
}

func (e threadEventRow) num(key string) int64 {
	switch v := e.data[key].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		if f, err := v.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

func (e threadEventRow) flag(key string) (bool, bool) {
	v, ok := e.data[key].(bool)
	return v, ok
}

// toolKey pairs tool.call with tool.result: the span when Core traced the
// call, otherwise the provider call id.
func (e threadEventRow) toolKey() string {
	if span := e.str("tool_span_id"); span != "" {
		return "span:" + span
	}
	return "id:" + e.str("id")
}

func (e threadEventRow) toolName() string {
	for _, key := range []string{"name", "tool", "tool_name"} {
		if v := e.str(key); v != "" {
			return v
		}
	}
	return ""
}

func queryThreadEvents(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]threadEventRow, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []threadEventRow
	for rows.Next() {
		var e threadEventRow
		var at, data string
		if err := rows.Scan(&e.row, &e.thread, &e.kind, &at, &data); err != nil {
			return nil, err
		}
		e.at, _ = parseTime(at)
		e.at = e.at.UTC()
		e.data = map[string]any{}
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		_ = dec.Decode(&e.data)
		out = append(out, e)
	}
	return out, rows.Err()
}

const threadEventColumns = `rowid,thread_id,type,time,data`

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

var openStepTypes = []string{"llm.start", "llm.done", "llm.error", "llm.cancelled", "tool.call", "tool.result", "worker.created", "thread.spawn"}

// openThreadStep finds the model turn or tool call a thread still has open
// from its newest events (newest first). Starts from an earlier incarnation of
// the thread, or older than threadOpenStepMaxAge, are treated as lost.
func openThreadStep(events []threadEventRow, startedAt, now time.Time) *sdk.ThreadCurrentStep {
	var current *sdk.ThreadCurrentStep
	consider := func(step sdk.ThreadCurrentStep) {
		if now.Sub(step.Since) > threadOpenStepMaxAge || (!startedAt.IsZero() && step.Since.Before(startedAt)) {
			return
		}
		if current == nil || step.Since.After(current.Since) {
			current = &step
		}
	}
	closed := map[string]bool{}
	// model: 0 = no model event yet, 1 = newest is a start (collecting the
	// retries that began the same turn), 2 = settled.
	model, modelSince := 0, time.Time{}
	settleModel := func() {
		if model == 1 {
			consider(sdk.ThreadCurrentStep{Kind: "model", Since: modelSince})
		}
		model = 2
	}
walk:
	for _, e := range events {
		switch e.kind {
		case "worker.created", "thread.spawn":
			break walk // older events belong to an earlier incarnation
		case "llm.done", "llm.error", "llm.cancelled":
			settleModel()
		case "llm.start":
			if model < 2 {
				model, modelSince = 1, e.at
			}
		case "tool.result":
			closed[e.toolKey()] = true
		case "tool.call":
			if !closed[e.toolKey()] {
				consider(sdk.ThreadCurrentStep{Kind: "tool", Tool: threadIdentifier(e.toolName()), Since: e.at})
			}
		}
	}
	settleModel()
	return current
}

// applyThreadTelemetry finishes live thread state and fills the telemetry
// tier. It reads only indexed per-thread and per-type ranges.
func (s *Server) applyThreadTelemetry(ctx context.Context, agentID int64, threads []sdk.ObservedThread, paused bool, access threadAccess, includeEnded time.Duration) ([]sdk.ObservedThread, error) {
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return threads, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	live := map[string]int{}
	for i := range threads {
		live[threads[i].ID] = i
	}
	typeList := placeholders(len(openStepTypes))
	for i := range threads {
		t := &threads[i]
		stopped := t.State == sdk.ThreadStateStopped // a stopped agent or thread has nothing open
		var current *sdk.ThreadCurrentStep
		if !stopped {
			args := []any{agentID, t.ID}
			for _, kind := range openStepTypes {
				args = append(args, kind)
			}
			// +type keeps SQLite on the (agent_id, thread_id) index, which is
			// already in rowid order, instead of sorting every matching row.
			events, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND thread_id=? AND +type IN (`+typeList+`) ORDER BY rowid DESC LIMIT 64`, args...)
			if err != nil {
				return threads, err
			}
			current = openThreadStep(events, t.StartedAt, now)
		}
		if access.telemetry {
			t.Current = current
			var last string
			if err := tx.QueryRowContext(ctx, `SELECT time FROM telemetry WHERE agent_id=? AND thread_id=? ORDER BY rowid DESC LIMIT 1`, agentID, t.ID).Scan(&last); err == nil {
				t.LastActivityAt, _ = parseTime(last)
				t.LastActivityAt = t.LastActivityAt.UTC()
			}
		}
		if stopped {
			continue
		}
		switch {
		case paused:
			t.State = sdk.ThreadStatePaused
		case t.State == sdk.ThreadStateBlocked:
		case current != nil:
			t.State = sdk.ThreadStateWorking
		case t.NextWakeAt.After(now):
			t.State = sdk.ThreadStateSleeping
		default:
			t.State = sdk.ThreadStateWaiting
		}
	}
	if access.telemetry {
		if threads, err = s.addEndedThreads(ctx, tx, agentID, threads, live, access, includeEnded, now); err != nil {
			return threads, err
		}
		if err = addSpawnLinks(ctx, tx, agentID, threads); err != nil {
			return threads, err
		}
	}
	for i := range threads {
		threads[i].Status = sdk.ThreadStatusForState(threads[i].State)
	}
	return threads, nil
}

func threadStateForOutcome(outcome string) string {
	switch outcome {
	case "failed":
		return sdk.ThreadStateFailed
	case "cancelled":
		return sdk.ThreadStateStopped
	}
	return sdk.ThreadStateDone
}

func threadOutcome(v string) string {
	if v == "completed" || v == "failed" || v == "cancelled" {
		return v
	}
	return ""
}

// addEndedThreads appends threads that ended within the window and are not
// live under the same id.
func (s *Server) addEndedThreads(ctx context.Context, tx *sql.Tx, agentID int64, threads []sdk.ObservedThread, live map[string]int, access threadAccess, window time.Duration, now time.Time) ([]sdk.ObservedThread, error) {
	if window <= 0 {
		return threads, nil
	}
	since := now.Add(-window).Format(time.RFC3339Nano)
	done, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type='thread.done' AND julianday(time)>=julianday(?) ORDER BY rowid DESC LIMIT 200`, agentID, since)
	if err != nil || len(done) == 0 {
		return threads, err
	}
	finished, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type='worker.finished' AND julianday(time)>=julianday(?) ORDER BY rowid DESC LIMIT 500`, agentID, since)
	if err != nil {
		return threads, err
	}
	outcomes := map[string]string{}
	for _, e := range finished {
		if _, seen := outcomes[e.thread]; !seen {
			outcomes[e.thread] = threadOutcome(e.str("outcome"))
		}
	}
	depth := map[string]int{}
	for _, t := range threads {
		depth[t.ID] = t.Depth
	}
	seen := map[string]bool{}
	var ended []sdk.ObservedThread
	for _, e := range done {
		if seen[e.thread] || len(e.thread) > 256 {
			continue
		}
		seen[e.thread] = true
		if _, ok := live[e.thread]; ok {
			continue // live again under the same id
		}
		outcome := threadOutcome(e.str("outcome"))
		if outcome == "" {
			outcome = outcomes[e.thread]
		}
		t := sdk.ObservedThread{ID: e.thread, ParentID: threadIdentifier(e.str("parent_id")), State: threadStateForOutcome(outcome), LastActivityAt: e.at,
			Ended: &sdk.ThreadEnd{At: e.at, Outcome: outcome}}
		if t.ParentID == "" {
			t.ParentID = "main"
		}
		if access.content {
			truncated, _ := e.flag("result_truncated")
			t.Ended.Result, t.Ended.ResultTruncated = cutText(e.str("result"), 4<<10)
			t.Ended.ResultTruncated = t.Ended.ResultTruncated || truncated
		}
		ended = append(ended, t)
	}
	// Oldest first so a parent's depth is known before its children's.
	for i := len(ended) - 1; i >= 0; i-- {
		t := &ended[i]
		t.Depth = depth[t.ParentID] + 1
		depth[t.ID] = t.Depth
	}
	return append(threads, ended...), nil
}

// addSpawnLinks fills SpawnedBy and, for ended threads, StartedAt from each
// thread's newest worker.created/thread.spawn events.
func addSpawnLinks(ctx context.Context, tx *sql.Tx, agentID int64, threads []sdk.ObservedThread) error {
	want := map[string][]int{}
	for i, t := range threads {
		if t.ID != "main" {
			want[t.ID] = append(want[t.ID], i)
		}
	}
	if len(want) == 0 {
		return nil
	}
	events, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type IN ('worker.created','thread.spawn') ORDER BY rowid DESC LIMIT 5000`, agentID)
	if err != nil {
		return err
	}
	done := map[string]map[string]bool{}
	for _, e := range events {
		indexes, ok := want[e.thread]
		if !ok {
			continue
		}
		if done[e.thread] == nil {
			done[e.thread] = map[string]bool{}
		}
		if done[e.thread][e.kind] {
			continue // only the newest incarnation
		}
		done[e.thread][e.kind] = true
		for _, i := range indexes {
			t := &threads[i]
			if t.SpawnedBy == nil {
				t.SpawnedBy = &sdk.ThreadSpawnLink{ThreadID: t.ParentID}
			}
			switch e.kind {
			case "worker.created":
				if parent := threadIdentifier(e.str("parent_thread_id")); parent != "" {
					t.SpawnedBy.ThreadID = parent
				}
				t.SpawnedBy.ToolSpanID = threadIdentifier(e.str("parent_tool_span_id"))
			case "thread.spawn":
				t.SpawnedBy.ExecutionID = threadIdentifier(e.str("parent_execution_id"))
				if t.StartedAt.IsZero() {
					t.StartedAt = e.at
				}
			}
		}
	}
	return nil
}

// observeAgentThreads builds the topology thread list.
func (s *Server) observeAgentThreads(ctx context.Context, agentID int64, port int, key string, agent *Agent, access threadAccess, includeEnded time.Duration) ([]sdk.ObservedThread, bool) {
	var threads []sdk.ObservedThread
	paused := false
	if port > 0 {
		rows, isPaused, ok := fetchCoreThreads(ctx, port, key)
		if !ok {
			return nil, false
		}
		paused = isPaused
		threads = liveObservedThreads(rows, access)
	} else if agent != nil {
		saved, ok := savedObservedThreads(agent.Config, access)
		if !ok {
			return nil, false
		}
		threads = saved
	} else {
		return nil, false
	}
	if s.store != nil {
		if enriched, err := s.applyThreadTelemetry(ctx, agentID, threads, paused, access, includeEnded); err == nil {
			threads = enriched
		} else {
			// Without telemetry nothing is known to be open.
			for i := range threads {
				switch {
				case threads[i].State == sdk.ThreadStateStopped:
				case paused:
					threads[i].State = sdk.ThreadStatePaused
				case threads[i].State == sdk.ThreadStateBlocked:
				case threads[i].NextWakeAt.After(time.Now()):
					threads[i].State = sdk.ThreadStateSleeping
				default:
					threads[i].State = sdk.ThreadStateWaiting
				}
				threads[i].Status = sdk.ThreadStatusForState(threads[i].State)
			}
		}
	}
	return threads, true
}

func parseIncludeEnded(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds < 0 || time.Duration(seconds)*time.Second > threadEndedWindowMax {
		return 0, fmt.Errorf("include_ended must be seconds between 0 and %d", int64(threadEndedWindowMax/time.Second))
	}
	return time.Duration(seconds) * time.Second, nil
}

// ---- Activity ----

var errThreadActivityCursor = errors.New("invalid activity cursor or changed filters")

type threadActivityCursor struct {
	Boundary int64  `json:"b"`
	Before   int64  `json:"n"`
	Query    string `json:"q"`
}

func threadActivityFingerprint(agentID int64, threadID, projection string) string {
	return telemetryFingerprint(sdk.TelemetryQuery{AgentID: agentID, ThreadID: threadID, Projection: projection, Events: []string{"thread-activity"}})
}

var threadStartTypes = []string{"llm.start", "tool.call", "thread.message", "thread.done"}
var threadFollowTypes = []string{"llm.start", "llm.done", "llm.error", "llm.cancelled", "tool.result", "tool.arguments", "worker.finished"}

func (s *Server) handleThreadActivity(w http.ResponseWriter, r *http.Request, p telemetryPrincipal, path string) {
	w.Header().Set("X-Apteva-Thread-Activity", "1")
	raw := strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/activity")
	agentID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || agentID <= 0 {
		http.Error(w, "invalid agent_id", 400)
		return
	}
	v := r.URL.Query()
	threadID := v.Get("thread_id")
	if threadID == "" || len(threadID) > 256 {
		http.Error(w, "thread_id required", 400)
		return
	}
	projection := v.Get("projection")
	if projection == "" {
		projection = "metadata"
	}
	if projection != "metadata" && projection != "content" {
		http.Error(w, "projection must be metadata or content", 400)
		return
	}
	limit := threadActivityDefaultLimit
	if rawLimit := v.Get("limit"); rawLimit != "" {
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > threadActivityMaxLimit {
			http.Error(w, fmt.Sprintf("limit must be between 1 and %d", threadActivityMaxLimit), 400)
			return
		}
	}
	q := sdk.TelemetryQuery{AgentID: agentID, ProjectID: p.project, Projection: projection}
	if !p.telemetryAllowed(q) {
		http.Error(w, "missing telemetry observation/content permission", 403)
		return
	}
	if _, err := s.telemetryAgents(p, q); err != nil {
		http.Error(w, "agent not found", 404)
		return
	}
	page, err := s.threadActivityPage(r.Context(), agentID, threadID, v.Get("cursor"), limit, projection == "content")
	if err != nil {
		if errors.Is(err, errThreadActivityCursor) {
			http.Error(w, err.Error(), 400)
		} else {
			http.Error(w, "thread activity unavailable", 503)
		}
		return
	}
	page.ServerID = s.telemetryServerID()
	writeJSON(w, page)
}

func (s *Server) threadActivityPage(ctx context.Context, agentID int64, threadID, rawCursor string, limit int, content bool) (sdk.ThreadActivityPage, error) {
	projection := "metadata"
	if content {
		projection = "content"
	}
	out := sdk.ThreadActivityPage{AgentID: agentID, ThreadID: threadID, Projection: projection, Steps: []sdk.ThreadStep{}}
	cursor := threadActivityCursor{Query: threadActivityFingerprint(agentID, threadID, projection)}
	if rawCursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(rawCursor)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Query != threadActivityFingerprint(agentID, threadID, projection) || cursor.Boundary < 0 || cursor.Before < 1 || cursor.Before > cursor.Boundary+1 {
			return out, errThreadActivityCursor
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if rawCursor == "" {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM telemetry`).Scan(&cursor.Boundary); err != nil {
			return out, err
		}
		cursor.Before = cursor.Boundary + 1
	}

	// A renamed thread's earlier ids hold its earlier steps.
	ids := []string{threadID}
	renames, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type='thread.renamed' ORDER BY rowid DESC LIMIT 1000`, agentID)
	if err != nil {
		return out, err
	}
	for hop, id := 0, threadID; hop < 8; hop++ {
		next := ""
		for _, e := range renames {
			if e.str("new_id") == id && e.str("old_id") != id && e.str("old_id") != "" {
				next = e.str("old_id")
				break
			}
		}
		if next == "" || containsString(ids, next) {
			break
		}
		ids = append(ids, next)
		out.PreviousIDs = append(out.PreviousIDs, next)
		id = next
	}

	idArgs := func(extra ...any) []any {
		args := []any{agentID}
		for _, id := range ids {
			args = append(args, id)
		}
		return append(args, extra...)
	}
	typeArgs := func(types []string) []any {
		args := make([]any, len(types))
		for i, t := range types {
			args[i] = t
		}
		return args
	}
	startArgs := append(idArgs(typeArgs(threadStartTypes)...), cursor.Boundary, cursor.Before, limit+1)
	starts, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND thread_id IN (`+placeholders(len(ids))+`) AND +type IN (`+placeholders(len(threadStartTypes))+`) AND rowid<=? AND rowid<? ORDER BY rowid DESC LIMIT ?`, startArgs...)
	if err != nil {
		return out, err
	}
	more := len(starts) > limit
	// Follow events from just after the next older start: that catches the
	// arguments logged before this page's oldest call, and the end of an
	// older turn, which must not close one of this page's.
	lower := int64(0)
	if more {
		lower = starts[limit].row + 1
		starts = starts[:limit]
	}
	if len(starts) == 0 {
		return out, nil
	}
	oldest := starts[len(starts)-1].row
	followArgs := append(idArgs(typeArgs(threadFollowTypes)...), lower, cursor.Boundary)
	follow, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND thread_id IN (`+placeholders(len(ids))+`) AND +type IN (`+placeholders(len(threadFollowTypes))+`) AND rowid>=? AND rowid<=? ORDER BY rowid ASC LIMIT 5000`, followArgs...)
	if err != nil {
		return out, err
	}
	children, err := queryThreadEvents(ctx, tx, `SELECT `+threadEventColumns+` FROM telemetry WHERE agent_id=? AND type='worker.created' AND rowid>=? AND rowid<=? LIMIT 2000`, agentID, oldest, cursor.Boundary)
	if err != nil {
		return out, err
	}
	childBySpan := map[string]string{}
	for _, e := range children {
		if containsString(ids, e.str("parent_thread_id")) && e.str("parent_tool_span_id") != "" {
			childBySpan[e.str("parent_tool_span_id")] = e.thread
		}
	}

	steps, stepRows := buildThreadSteps(starts, follow, childBySpan, content, time.Now().UTC())
	// Old Core put the outcome only on worker.finished, just before thread.done.
	for i := range steps {
		if steps[i].Kind == "end" && steps[i].Outcome == "" {
			var data string
			if tx.QueryRowContext(ctx, `SELECT data FROM telemetry WHERE agent_id=? AND thread_id=? AND +type='worker.finished' AND rowid<=? ORDER BY rowid DESC LIMIT 1`, agentID, steps[i].ThreadID, cursor.Boundary).Scan(&data) == nil {
				var finished struct {
					Outcome string `json:"outcome"`
				}
				_ = json.Unmarshal([]byte(data), &finished)
				steps[i].Outcome = threadOutcome(finished.Outcome)
				steps[i].Status = threadStepStatusForOutcome(steps[i].Outcome)
			}
		}
	}

	// Keep pages bounded. A full page resumes below its oldest start (which
	// may be a superseded retry that is not a step); a page cut short by size
	// resumes below the last step shown.
	pageBytes := 0
	cursor.Before = oldest
	for i, step := range steps {
		encoded, _ := json.Marshal(step)
		if pageBytes+len(encoded) > 2<<20 && len(out.Steps) > 0 {
			more = true
			cursor.Before = stepRows[i-1]
			break
		}
		pageBytes += len(encoded)
		out.Steps = append(out.Steps, step)
	}
	if more {
		encoded, _ := json.Marshal(cursor)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return out, nil
}

func threadStepStatusForOutcome(outcome string) string {
	switch outcome {
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	}
	return "done"
}

// buildThreadSteps pairs this page's start events (newest first) with the
// events that follow them (oldest first). A model turn belongs to its last
// llm.start, so a retry chain split across pages is shown once. Steps come
// back newest first with the rowid of each step's start.
func buildThreadSteps(starts, follow []threadEventRow, childBySpan map[string]string, content bool, now time.Time) ([]sdk.ThreadStep, []int64) {
	onPage := map[int64]bool{}
	byRow := map[int64]threadEventRow{}
	for _, e := range starts {
		onPage[e.row] = true
		byRow[e.row] = e
	}
	for _, e := range follow {
		byRow[e.row] = e
	}
	rows := make([]int64, 0, len(byRow))
	for row := range byRow {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })

	type built struct {
		step sdk.ThreadStep
		row  int64
		keep bool
	}
	var steps []*built
	openModel := map[string]*built{}         // thread -> open turn (nil step = an older page's turn)
	openModelSince := map[string]time.Time{} // first start of the open retry chain
	openTools := map[string]*built{}
	arguments := map[string]threadEventRow{}
	text := func(v string, max int) (string, bool) { return cutText(v, max) }
	for _, row := range rows {
		e := byRow[row]
		switch e.kind {
		case "llm.start":
			since := e.at
			if _, open := openModel[e.thread]; open {
				// A retry: the earlier start is superseded by this one.
				since = openModelSince[e.thread]
				if prev := openModel[e.thread]; prev != nil {
					prev.keep = false
				}
			}
			var b *built
			if onPage[row] {
				b = &built{row: row, keep: true, step: sdk.ThreadStep{Kind: "model", ThreadID: e.thread, StartedAt: since, Status: "running",
					Model: threadIdentifier(e.str("model")), Provider: threadIdentifier(e.str("provider"))}}
				steps = append(steps, b)
			}
			openModel[e.thread], openModelSince[e.thread] = b, since
		case "llm.done", "llm.error", "llm.cancelled":
			b, open := openModel[e.thread]
			delete(openModel, e.thread)
			if !open || b == nil {
				continue
			}
			b.step.EndedAt = e.at
			b.step.DurationMS = e.at.Sub(b.step.StartedAt).Milliseconds()
			if model := threadIdentifier(e.str("model")); model != "" {
				b.step.Model = model
			}
			if provider := threadIdentifier(e.str("provider")); provider != "" {
				b.step.Provider = provider
			}
			switch e.kind {
			case "llm.done":
				b.step.Status = "done"
				b.step.TokensIn, b.step.TokensOut = e.num("tokens_in"), e.num("tokens_out")
			case "llm.error":
				b.step.Status = "failed"
				if content {
					b.step.Error, _ = text(e.str("error"), 1000)
				}
			default:
				b.step.Status = "cancelled"
			}
		case "tool.call":
			if !onPage[row] {
				continue
			}
			span := threadIdentifier(e.str("tool_span_id"))
			b := &built{row: row, keep: true, step: sdk.ThreadStep{Kind: "tool", ThreadID: e.thread, StartedAt: e.at, Status: "running",
				Tool: threadIdentifier(e.toolName()), CallID: threadIdentifier(e.str("id")), ToolSpanID: span, ChildThreadID: childBySpan[span]}}
			if content {
				b.step.Reason, _ = text(e.str("reason"), 1000)
			}
			steps = append(steps, b)
			openTools[e.thread+"\x00"+e.toolKey()] = b
		case "tool.result":
			key := e.thread + "\x00" + e.toolKey()
			b := openTools[key]
			delete(openTools, key)
			if b == nil {
				continue
			}
			b.step.EndedAt = e.at
			b.step.DurationMS = e.num("duration_ms")
			if b.step.DurationMS == 0 {
				b.step.DurationMS = e.at.Sub(b.step.StartedAt).Milliseconds()
			}
			b.step.Status = "done"
			if ok, set := e.flag("success"); set && !ok {
				b.step.Status = "failed"
			}
			if content {
				truncated, _ := e.flag("result_truncated")
				previewTruncated, _ := e.flag("result_preview_truncated")
				b.step.Result = e.str("result")
				b.step.ResultTruncated = truncated || previewTruncated
				b.step.ResultBytes = e.num("result_original_bytes")
				if b.step.Status == "failed" {
					b.step.Error, _ = text(b.step.Result, 1000)
				}
			}
		case "tool.arguments":
			if content && e.str("id") != "" {
				arguments[e.thread+"\x00"+e.str("id")] = e // the last stage logged wins
			}
		case "thread.message":
			if !onPage[row] {
				continue
			}
			b := &built{row: row, keep: true, step: sdk.ThreadStep{Kind: "message", ThreadID: e.thread, StartedAt: e.at, EndedAt: e.at, Status: "done", To: threadIdentifier(e.str("to"))}}
			if content {
				b.step.Text, b.step.TextTruncated = text(e.str("message"), threadMessageTextBytes)
			}
			steps = append(steps, b)
		case "thread.done":
			if !onPage[row] {
				continue
			}
			outcome := threadOutcome(e.str("outcome"))
			b := &built{row: row, keep: true, step: sdk.ThreadStep{Kind: "end", ThreadID: e.thread, StartedAt: e.at, EndedAt: e.at, Outcome: outcome, Status: threadStepStatusForOutcome(outcome)}}
			if content {
				truncated, _ := e.flag("result_truncated")
				b.step.Text, b.step.TextTruncated = text(e.str("result"), 4<<10)
				b.step.TextTruncated = b.step.TextTruncated || truncated
			}
			steps = append(steps, b)
		}
	}
	out := make([]sdk.ThreadStep, 0, len(steps))
	outRows := make([]int64, 0, len(steps))
	for i := len(steps) - 1; i >= 0; i-- {
		b := steps[i]
		if !b.keep {
			continue
		}
		if b.step.Kind == "tool" && content {
			if args, ok := arguments[b.step.ThreadID+"\x00"+b.step.CallID]; ok && b.step.CallID != "" {
				b.step.Arguments = args.str("json")
				b.step.ArgumentsTruncated, _ = args.flag("truncated")
			}
		}
		if b.step.Status == "running" && now.Sub(b.step.StartedAt) > threadOpenStepMaxAge {
			b.step.Status = "unknown" // no end was logged; the call was likely lost
		}
		out = append(out, b.step)
		outRows = append(outRows, b.row)
	}
	return out, outRows
}
