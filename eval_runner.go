package main

// eval_runner.go — drives one eval run end-to-end with a REAL
// apteva-core process.
//
// Architecture (no apteva-core changes):
//   1. The runner creates a fresh, transient agent row (kind='eval_run')
//      with the directive being tested + a unique tmp dir.
//   2. apteva-server spawns a real apteva-core for that row, but with
//      its mcp_servers config replaced by a single HTTP MCP entry
//      pointing at /api/eval-mock-gateway/<session_token>. No real
//      gateway, no channels — the eval core can only see the mocks.
//   3. The runner POSTs the eval's trigger to the core's /event API
//      and polls /threads/main/context for assistant replies, until
//      the agent goes idle (no new messages for ~3s) or max_turns
//      is exceeded.
//   4. Every tool call the core makes during the run lands on
//      apteva-server's eval-mock-gateway handler, which (a) records
//      the call into the session's trajectory buffer and (b) returns
//      the matching mock response (or a stub-ok if no mock matched).
//   5. When the run completes, the runner stitches the trajectory
//      from the buffer (tool calls) + the thread's message history
//      (LLM messages), ordered by timestamp.
//   6. The meta-agent (a separate real apteva-core process — see
//      platform_agent.go) grades the trajectory against the goals.
//   7. Tear down: stop the eval core, delete the transient agent row,
//      remove the tmp dir.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// httpClient5s is the shared client for short-poll requests from
// apteva-server back to a spawned core process. Local loopback;
// 5s timeout is plenty.
var httpClient5s = &http.Client{Timeout: 5 * time.Second}

// newHTTPRequest builds a Bearer-authed request against the spawned
// core. apiKey comes from AgentManager.GetCoreAPIKey for the eval
// or meta-agent's running process.
func newHTTPRequest(ctx context.Context, method, url string, body io.Reader, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("content-type", "application/json")
	return req, nil
}

// runSession is the per-eval-run state. Looked up by session token
// in the eval-mock-gateway handler when a tool call lands.
type runSession struct {
	eval       *Eval
	mu         sync.Mutex
	trajectory []TrajectoryTurn
	maxTurns   int
	turnsUsed  int
}

func newRunSession(ev *Eval) *runSession {
	if ev.MaxTurns <= 0 {
		ev.MaxTurns = 5
	}
	return &runSession{
		eval:     ev,
		maxTurns: ev.MaxTurns,
	}
}

// recordUser, recordAgent, recordSystem, resolveToolCall, snapshot —
// same trajectory buffer used by both the runner and the gateway
// handler. recordAgent is called by the runner per assistant reply
// pulled from core's thread context. resolveToolCall is called by
// the gateway handler when a tool call lands.

func (s *runSession) recordUser(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "user",
		Content:   text,
		Timestamp: time.Now(),
	})
}

func (s *runSession) recordAgent(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "agent",
		Content:   text,
		Timestamp: time.Now(),
	})
	s.turnsUsed++
}

func (s *runSession) recordSystem(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "system",
		Content:   note,
		Timestamp: time.Now(),
	})
}

// resolveToolCall is the shim's mock dispatch — invoked by the eval
// gateway HTTP handler whenever the eval core makes a tool call.
// Looks up the first matching mock in the eval's mocks[]; falls
// back to a stub-ok with a warning if no mock matches.
func (s *runSession) resolveToolCall(app, tool string, args json.RawMessage) ToolCallRecord {
	rec := ToolCallRecord{
		App:  app,
		Tool: tool,
		Args: args,
	}
	mock, found := matchMock(s.eval.Mocks, app, tool, args)
	switch {
	case found && mock.Error != "":
		rec.Error = mock.Error
		rec.Mocked = true
	case found:
		rec.Response = mock.Return
		rec.Mocked = true
	default:
		rec.Response = json.RawMessage(`{"ok":true,"_stub":true}`)
		rec.Warning = "unmocked tool call — eval is using a stub-ok default"
	}
	s.mu.Lock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "tool",
		ToolCall:  &rec,
		Timestamp: time.Now(),
	})
	s.mu.Unlock()
	return rec
}

func (s *runSession) snapshot() Trajectory {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TrajectoryTurn, len(s.trajectory))
	copy(out, s.trajectory)
	return Trajectory{Turns: out}
}

// matchMock walks mocks[] and returns the first entry matching
// {app, tool} with optional ArgsMatch contained in args. ArgsMatch
// is a shallow subset: every key in ArgsMatch must appear in args
// with the same stringified value. Args that args has but
// ArgsMatch doesn't are fine — this lets operators write "match any
// send_message to the pushover channel" without enumerating every
// other arg.
func matchMock(mocks []EvalMock, app, tool string, args json.RawMessage) (EvalMock, bool) {
	var argsMap map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &argsMap)
	}
	for _, m := range mocks {
		if m.App != app || m.Tool != tool {
			continue
		}
		if len(m.ArgsMatch) == 0 {
			return m, true
		}
		match := true
		for k, want := range m.ArgsMatch {
			if got, ok := argsMap[k]; !ok || !looseEqual(got, want) {
				match = false
				break
			}
		}
		if match {
			return m, true
		}
	}
	return EvalMock{}, false
}

func looseEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// ─── Eval session registry ─────────────────────────────────────────

// evalSessions tracks active eval runs keyed by their session
// token. The eval-mock-gateway HTTP handler reads from this when a
// tool call lands. The runner inserts on spawn and removes on
// teardown.
var (
	evalSessionsMu sync.RWMutex
	evalSessions   = map[string]*runSession{}
)

func registerEvalSession(token string, sess *runSession) {
	evalSessionsMu.Lock()
	defer evalSessionsMu.Unlock()
	evalSessions[token] = sess
}

func lookupEvalSession(token string) *runSession {
	evalSessionsMu.RLock()
	defer evalSessionsMu.RUnlock()
	return evalSessions[token]
}

func unregisterEvalSession(token string) {
	evalSessionsMu.Lock()
	defer evalSessionsMu.Unlock()
	delete(evalSessions, token)
}

// ─── The runner ────────────────────────────────────────────────────

// runEval is the entrypoint called by handleAgentEvals when
// POST /evals/:id/run lands. Drives a real apteva-core process
// through the eval; the simulation MVP is gone.
func (s *Server) runEval(ctx context.Context, userID int64, agent *Agent, ev *Eval) (*EvalRun, error) {
	return s.runRealEvalCore(ctx, userID, agent, ev, false)
}

// previewEval runs an eval against an in-memory draft agent — same
// real-core path as runEval, but spawns a transient agent row that
// gets deleted after the run. ID=0 on the returned EvalRun signals
// "not persisted to agent_eval_runs"; the wizard's Verify step
// uses this so operators can iterate before committing.
func (s *Server) previewEval(ctx context.Context, userID int64, projectID string, draft *Agent, ev *Eval) (*EvalRun, error) {
	return s.runRealEvalCore(ctx, userID, draft, ev, true)
}

// runRealEvalCore is the shared backbone. preview=true means: don't
// persist the run row, and create-then-delete a transient agent row
// for the spawn (so the directive being tested isn't tied to any
// live agent the operator hasn't created yet).
func (s *Server) runRealEvalCore(
	ctx context.Context,
	userID int64,
	agent *Agent,
	ev *Eval,
	preview bool,
) (*EvalRun, error) {
	startedAt := time.Now()
	session := newRunSession(ev)
	triggerText := triggerToText(ev.Trigger)
	session.recordUser(triggerText)

	// Provider preflight. Without LLM credentials the spawned core
	// would boot then immediately fail on first turn — fail fast
	// here with a clear message so the operator's eval row gets a
	// useful error_message instead of a stuck-pending state.
	pool := s.GetProviderPool(userID, agent.ProjectID)
	if len(pool) == 0 {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
			"no LLM provider configured — add one in Settings → Providers", preview)
	}

	// For preview runs the agent isn't a real DB row yet; insert a
	// transient one (kind='eval_run') so AgentManager.Start has
	// something to bind the spawned core to. We delete it again in
	// the teardown defer below.
	transientAgent := preview
	if transientAgent {
		dirRow, err := s.store.CreateAgent(userID, fmt.Sprintf("__eval_preview_%d__", time.Now().UnixNano()), agent.Directive, agent.Mode, "{}", agent.ProjectID)
		if err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
				"prepare preview agent: "+err.Error(), preview)
		}
		// Promote the transient row to kind='eval_run' so the user's
		// list endpoints filter it out even before we delete it.
		_, _ = s.store.db.Exec(`UPDATE agents SET kind = 'eval_run' WHERE id = ?`, dirRow.ID)
		// Re-fetch so any side fields (config defaults) are populated.
		fetched, err := s.store.GetAgentByID(dirRow.ID)
		if err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
				"reload preview agent: "+err.Error(), preview)
		}
		agent = fetched
		defer func() {
			s.agents.Stop(agent.ID)
			s.store.DeleteAgent(userID, agent.ID)
		}()
	} else {
		// For persisted runs we still spawn a fresh core process for
		// isolation — the live agent (if any) keeps running on its
		// own. We just stand up an eval-mode sibling, then stop it
		// at the end. To avoid id collisions with the live agent's
		// process tracking, we clone the agent row under a fresh
		// transient id with kind='eval_run' and use that for the
		// spawn lifecycle.
		dirRow, err := s.store.CreateAgent(userID, fmt.Sprintf("__eval_run_%d__", time.Now().UnixNano()), agent.Directive, agent.Mode, agent.Config, agent.ProjectID)
		if err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
				"prepare eval agent: "+err.Error(), preview)
		}
		_, _ = s.store.db.Exec(`UPDATE agents SET kind = 'eval_run' WHERE id = ?`, dirRow.ID)
		fetched, err := s.store.GetAgentByID(dirRow.ID)
		if err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
				"reload eval agent: "+err.Error(), preview)
		}
		agent = fetched
		defer func() {
			s.agents.Stop(agent.ID)
			s.store.DeleteAgent(userID, agent.ID)
		}()
	}

	// Generate the eval session token and register the runSession
	// so the gateway handler can find it.
	token := fmt.Sprintf("eval-%d-%d", agent.ID, time.Now().UnixNano())
	registerEvalSession(token, session)
	defer unregisterEvalSession(token)

	// Override the agent's config to route MCP through the eval
	// gateway only. Strip the normal apteva-server gateway and
	// channels so the eval core can't touch real apps. Inject our
	// HTTP MCP entry; core picks this up at Start time via the
	// instance config blob.
	evalConfig := map[string]any{
		"directive": agent.Directive,
		"mode":      agent.Mode,
		"mcp_servers": []any{
			map[string]any{
				"name":        "eval-mocks",
				"url":         fmt.Sprintf("http://127.0.0.1:%s/api/eval-mock-gateway/%s", s.port, token),
				"transport":   "http",
				"main_access": true,
				"no_spawn":    true,
			},
		},
		// Server-only flags that turn off the auto-injected
		// gateway and channels for this run.
		"include_apteva_server": false,
		"include_channels":      false,
	}
	cfgJSON, _ := json.Marshal(evalConfig)
	agent.Config = string(cfgJSON)
	_ = s.store.UpdateAgent(agent)

	// Spawn the real apteva-core for the eval. Reuses every
	// production lifecycle path — port allocation, env injection,
	// the disk config.json, the providers pool. Eval-specificity
	// lives entirely in the mcp_servers config above.
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, agent.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	if err := s.agents.Start(agent, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID, defaultProviderForInstance(agent), agent.ProjectID)); err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
			"spawn eval core: "+err.Error(), preview)
	}

	port := s.agents.GetPort(agent.ID)
	apiKey := s.agents.GetCoreAPIKey(agent.ID)
	if !waitForCoreListening(port, 10*time.Second) {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
			"eval core never listened", preview)
	}

	// Drive the trigger as a /event POST to thread "main". Core
	// picks it up and starts thinking. We then poll its thread
	// context to capture assistant replies + look for idle.
	if err := postCoreEvent(ctx, port, apiKey, "main", triggerText); err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, "error",
			"post trigger to eval core: "+err.Error(), preview)
	}

	if err := collectAssistantReplies(ctx, port, apiKey, "main", session, ev.MaxTurns); err != nil {
		// Soft error — we still have a partial trajectory and may
		// be able to grade it. Record the system note + continue.
		session.recordSystem("runner: " + err.Error())
	}

	finishedAt := time.Now()
	trajectory := session.snapshot()

	verdict, judgeErr := s.judgeWithMetaAgent(ctx, userID, agent.ProjectID, ev, trajectory)
	status := "fail"
	errMsg := ""
	if judgeErr != nil {
		status = "error"
		errMsg = judgeErr.Error()
	} else if verdict != nil && verdict.Overall == "pass" {
		status = "pass"
	}
	return s.writeEvalRunWithDetails(ev.ID, startedAt, finishedAt, session, &trajectory, verdict, status, errMsg, preview)
}

// writeEvalRun is the convenience shortcut for early-exit error
// paths where we haven't actually run anything against core. It
// captures the failure as an error-status run row (or a transient
// preview EvalRun when preview=true).
func (s *Server) writeEvalRun(evalID string, startedAt time.Time, session *runSession, verdict *JudgeVerdict, status, errMsg string, preview bool) (*EvalRun, error) {
	finishedAt := time.Now()
	trajectory := session.snapshot()
	return s.writeEvalRunWithDetails(evalID, startedAt, finishedAt, session, &trajectory, verdict, status, errMsg, preview)
}

func (s *Server) writeEvalRunWithDetails(evalID string, startedAt, finishedAt time.Time, session *runSession, trajectory *Trajectory, verdict *JudgeVerdict, status, errMsg string, preview bool) (*EvalRun, error) {
	run := EvalRun{
		EvalID:       evalID,
		StartedAt:    startedAt,
		FinishedAt:   &finishedAt,
		Status:       status,
		Trajectory:   *trajectory,
		Verdict:      verdict,
		DurationMS:   int(finishedAt.Sub(startedAt).Milliseconds()),
		TurnsUsed:    session.turnsUsed,
		ErrorMessage: errMsg,
	}
	if !preview {
		id, _ := s.store.InsertEvalRun(run)
		run.ID = id
		_ = s.store.RollupEvalLastRun(evalID, status, finishedAt)
	}
	return &run, nil
}

// waitForCoreListening dials the core's port until it accepts a
// connection or the deadline elapses. Mirrors the AgentManager's
// background healthcheck but synchronous, so callers can confidently
// dispatch HTTP requests next without racing the listener.
func waitForCoreListening(port int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if isCoreListening(port) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// collectAssistantReplies polls the eval core's thread context
// until the agent goes idle (no new messages for 3s) or max_turns
// assistant turns have been captured into the trajectory. Returns
// nil on a clean idle, error on timeout / network failure.
//
// Polling cadence is 500ms — fast enough to catch each turn shortly
// after it lands, slow enough not to thrash core.
func collectAssistantReplies(ctx context.Context, port int, apiKey, threadID string, session *runSession, maxTurns int) error {
	if maxTurns <= 0 {
		maxTurns = 5
	}
	overallDeadline := time.Now().Add(120 * time.Second)
	idleSince := time.Time{}
	idleWindow := 3 * time.Second
	seenLastUpdate := time.Now()
	lastAssistantCount := 0
	for time.Now().Before(overallDeadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := fetchThreadMessages(ctx, port, apiKey, threadID)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		assistantCount := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				assistantCount++
			}
		}
		if assistantCount > lastAssistantCount {
			// New assistant messages — record them all.
			i := 0
			for _, m := range msgs {
				if m.Role != "assistant" {
					continue
				}
				if i >= lastAssistantCount {
					if text := m.text(); text != "" {
						session.recordAgent(text)
					}
				}
				i++
			}
			lastAssistantCount = assistantCount
			seenLastUpdate = time.Now()
			idleSince = time.Time{}
			if assistantCount >= maxTurns {
				session.recordSystem(fmt.Sprintf("max_turns reached (%d)", maxTurns))
				return nil
			}
		} else if time.Since(seenLastUpdate) > 500*time.Millisecond {
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			if time.Since(idleSince) >= idleWindow && lastAssistantCount > 0 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("overall eval timeout (120s)")
}

// threadMessage is the parsed shape of one entry in core's
// /threads/<id>/context messages array.
type threadMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Parts   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (m threadMessage) text() string {
	if m.Content != "" {
		return m.Content
	}
	var out string
	for _, p := range m.Parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// fetchThreadMessages reads core's thread context endpoint and
// returns the full message list. Trim down to the bits we care
// about (role + text content) so the type stays small.
func fetchThreadMessages(ctx context.Context, port int, apiKey, threadID string) ([]threadMessage, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/threads/%s/context", port, threadID)
	req, _ := newHTTPRequest(ctx, "GET", url, nil, apiKey)
	resp, err := httpClient5s.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var parsed struct {
		Messages []threadMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Messages, nil
}

// triggerToText renders an EvalTrigger as the opening user message
// for the eval core. We embed the type so the agent knows whether
// this is "a chat message arrived" vs "a webhook fired" vs "the
// schedule ticked".
func triggerToText(t EvalTrigger) string {
	switch t.Type {
	case "chat_message":
		text, _ := t.Payload["text"].(string)
		from, _ := t.Payload["from"].(string)
		channel, _ := t.Payload["channel"].(string)
		out := "Incoming chat message"
		if from != "" {
			out += " from " + from
		}
		if channel != "" {
			out += " in " + channel
		}
		return out + ":\n" + text
	case "webhook":
		b, _ := json.MarshalIndent(t.Payload, "", "  ")
		return "Incoming webhook event:\n" + string(b)
	case "scheduled_tick":
		when, _ := t.Payload["iso_time"].(string)
		return "Scheduled tick at " + when
	default:
		b, _ := json.MarshalIndent(t.Payload, "", "  ")
		return "Event of type " + t.Type + ":\n" + string(b)
	}
}
