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
	"fmt"
	"io"
	"net/http"
	"strings"
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
// in the eval-mock-gateway handler when a tool call lands. State
// accumulates across iterations of an improvement run — the
// session token is stable for the whole run, even if the spawned
// core is torn down + respawned between iterations after a
// directive edit. Trajectory is one chronological list spanning
// all iterations; the runner inserts system/judge turns to mark
// iteration boundaries.
type runSession struct {
	eval             *Eval
	mu               sync.Mutex
	trajectory       []TrajectoryTurn
	maxTurns         int
	turnsUsed        int
	strict           bool     // RunOptions.StrictMocks — fail on unmocked tool calls
	strictViolations []string // first violation message bubbles up to error_message
	metrics          *EvalRunMetrics

	// pendingTools maps a real-MCP tool call's id (as assigned by
	// apteva-core's provider) to the trajectory turn that records
	// it. When the corresponding tool_result lands one or two
	// iterations later, attachToolResult uses this map to back-fill
	// the turn's Response / Error in place. Cleared after a
	// successful back-fill; orphans (no result before run end) stay
	// untouched so the judge sees an in-flight call rather than a
	// silent drop.
	pendingTools map[string]*ToolCallRecord
}

func newRunSession(ev *Eval) *runSession {
	if ev.MaxTurns <= 0 {
		ev.MaxTurns = 5
	}
	return &runSession{
		eval:         ev,
		maxTurns:     ev.MaxTurns,
		pendingTools: map[string]*ToolCallRecord{},
	}
}

// recordUser, recordAgent, recordJudge, recordSystem, resolveToolCall,
// snapshot — same trajectory buffer used by both the runner and the
// gateway handler. recordAgent is called by the runner per assistant
// reply pulled from core's thread context. resolveToolCall is called
// by the gateway handler when a tool call lands.

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

func (s *runSession) recordJudge(iteration int, feedback string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "judge",
		Content:   feedback,
		Iteration: iteration,
		Timestamp: time.Now(),
	})
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
//
// Strict mode: if no mock matches and s.strict is true, the call
// returns an MCP error to the agent AND records the violation onto
// s.strictViolations so the runner can surface it as the run's
// error_message after completion. Used by continuous monitoring of
// live agents where stub-ok would silently mask drift.
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
	case s.strict:
		rec.Error = fmt.Sprintf("strict mocks: no pinned mock for %s.%s", app, tool)
		s.mu.Lock()
		s.strictViolations = append(s.strictViolations, rec.Error)
		s.mu.Unlock()
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

// recordRealToolCall logs a tool invocation that came from apteva-core's
// own message history (i.e. the agent calling a real MCP server like
// a sandboxed app sidecar — NOT a call routed through the
// eval-mock-gateway, which has its own resolveToolCall path). The
// returned turn's ToolCall pointer is stashed in pendingTools so the
// matching tool_result can back-fill the Response field once it
// arrives one or two iterations later.
//
// App is left empty for now — we don't have a tool-name → MCP-server
// mapping in scope, and the bare Name (e.g. "status_set") is what the
// model + judge actually reason about. A follow-up can attribute
// calls to apps by walking the agent's mcp_servers config at run start.
func (s *runSession) recordRealToolCall(callID, name string, args json.RawMessage) {
	rec := &ToolCallRecord{
		Tool:   name,
		Args:   args,
		Mocked: false, // real-MCP path, not the gateway's canned response
	}
	s.mu.Lock()
	s.trajectory = append(s.trajectory, TrajectoryTurn{
		Role:      "tool",
		ToolCall:  rec,
		Timestamp: time.Now(),
	})
	if callID != "" {
		s.pendingTools[callID] = rec
	}
	s.mu.Unlock()
}

// hasPendingTools reports whether any real-MCP tool call recorded
// via recordRealToolCall is still awaiting its tool_result. Used by
// the polling loop to extend the idle window — exiting between a
// tool call and its result would silently drop the call's outcome
// from the trajectory.
func (s *runSession) hasPendingTools() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingTools) > 0
}

// iter1RaceLikelySince reports whether the agent's iter-1 output
// looks like the autonomous-loop startup race fired — i.e. the
// agent called `pace` to go to sleep AND didn't call any
// non-core MCP tool. Text alone doesn't disqualify because the
// model often rationalizes its pace ("No user request present.
// I'll sleep until something arrives.") even though it processed
// no real work; what matters is whether it took any action that
// actually moved the eval forward.
//
// Distinguishing cases:
//   - greeter (no race): text reply only, NO pace call          → returns false (no retry)
//   - greeter (no race): two text replies, NO pace              → false (no retry)
//   - sandboxed (race): pace + rationalizing text, no other tool → returns true  (retry)
//   - sandboxed (working): status_set + maybe pace at end       → false (status_set != core)
//   - agent that hallucinated work: text only, no pace          → false (let judge grade it)
func (s *runSession) iter1RaceLikelySince(since int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pacedToSleep, calledNonCore bool
	for i := since; i < len(s.trajectory); i++ {
		t := s.trajectory[i]
		if t.Role != "tool" || t.ToolCall == nil {
			continue
		}
		name := t.ToolCall.Tool
		switch name {
		case "pace":
			pacedToSleep = true
		case "done", "evolve", "search_tools", "send":
			// Other core tools — not "real work" against the eval's
			// goals but also not a race signature on their own.
		default:
			calledNonCore = true
		}
	}
	return pacedToSleep && !calledNonCore
}

// trajectoryLen returns the current length of the trajectory under
// the lock, so race-detector callers can checkpoint before posting.
func (s *runSession) trajectoryLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.trajectory)
}

// attachToolResult finds the trajectory turn for a previously-seen
// real tool call and back-fills its Response (or Error) in place.
// Idempotent — late or duplicate results for the same call_id are
// swallowed silently. Results for unknown call_ids (e.g. a tool call
// that landed on the gateway path and was already recorded with its
// canned response there) are also swallowed.
func (s *runSession) attachToolResult(callID, content string, isError bool) string {
	if callID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.pendingTools[callID]
	if !ok {
		return ""
	}
	delete(s.pendingTools, callID)
	if isError {
		rec.Error = content
		return rec.Tool
	}
	// content is plain text from core's ToolResult.Content. Most MCP
	// servers return JSON-encoded payloads but a few return prose;
	// either way we store it as RawMessage by wrapping non-JSON as a
	// JSON string so the trajectory's `response: <raw>` rendering in
	// buildJudgePrompt stays valid for the judge to read.
	if json.Valid([]byte(content)) {
		rec.Response = json.RawMessage(content)
	} else {
		b, _ := json.Marshal(content)
		rec.Response = json.RawMessage(b)
	}
	return rec.Tool
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
// through the eval. opts.MaxIterations controls whether this is a
// strict single-shot verification (1) or an improvement-loop run
// (>1) — the route picks the default and the request body can
// override.
func (s *Server) runEval(ctx context.Context, userID int64, agent *Agent, ev *Eval, opts RunOptions) (*EvalRun, error) {
	if opts.UseEnvironment {
		return s.runEvalInEnvironment(ctx, userID, agent, ev, false, opts)
	}
	return s.runRealEvalCore(ctx, userID, agent, ev, false, opts, nil)
}

// previewEval runs an eval against an in-memory draft agent — same
// real-core path as runEval, but spawns a transient agent row that
// gets deleted after the run. ID=0 on the returned EvalRun signals
// "not persisted to agent_eval_runs"; the wizard's Verify step
// uses this so operators can iterate before committing. opts.MaxIterations
// defaults to 5 at the handler so the wizard naturally surfaces
// improvement suggestions.
func (s *Server) previewEval(ctx context.Context, userID int64, projectID string, draft *Agent, ev *Eval, opts RunOptions) (*EvalRun, error) {
	return s.runRealEvalCore(ctx, userID, draft, ev, true, opts, nil)
}

// runRealEvalCore is the shared backbone for runEval + previewEval.
// preview=true means: don't persist the run row and use a transient
// agent row name prefix so audit trails distinguish wizard previews
// from persisted runs.
//
// Iteration loop semantics:
//   - Attempt 1 always posts ev.Description to the spawned core.
//   - If verdict=pass: break.
//   - If verdict=fail AND suggestions.directive_edits is non-empty:
//     teardown core, append edits to the ephemeral directive,
//     respawn fresh on the next iteration with the new directive,
//     post ev.Description again.
//   - If verdict=fail AND only suggestions.in_run_feedback is set:
//     keep core running, post the feedback into the same thread on
//     the next iteration — the agent retains its prior context and
//     can address the feedback without re-starting from scratch.
//   - At MaxIterations or when the judge proposes nothing, break.
//
// Improvements never touch the live agent's stored directive. Edits
// accumulate into the local baseDirective string for the duration of
// the run; the apply-improvements handler is what writes them back
// if the operator chooses to persist.
// stepCb is the optional per-iteration pause hook (eval_streaming.go).
// nil = legacy batch mode: runner auto-continues until pass / max /
// strict-violation / no actionable suggestion. Non-nil = streaming
// mode: the runner emits each iteration's verdict + running rollup
// to the callback and honors its StepStop return to break early.
func (s *Server) runRealEvalCore(
	ctx context.Context,
	userID int64,
	agent *Agent,
	ev *Eval,
	preview bool,
	opts RunOptions,
	stepCb StepCallback,
) (*EvalRun, error) {
	startedAt := time.Now()

	// Apply RunOptions defaults + safety caps. MaxIterations is
	// hard-capped at 10 to bound LLM cost on a single run; the
	// route-level defaults (5 for preview, 1 for manual run) stay
	// well below.
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 1
	}
	if opts.MaxIterations > 10 {
		opts.MaxIterations = 10
	}

	session := newRunSession(ev)
	session.strict = opts.StrictMocks
	session.recordUser(ev.Description)

	// Provider preflight. Without LLM credentials the spawned core
	// would boot then immediately fail on first turn — fail fast
	// here with a clear message so the operator's eval row gets a
	// useful error_message instead of a stuck-pending state.
	pool := s.GetProviderPool(userID, agent.ProjectID)
	if len(pool) == 0 {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"no LLM provider configured — add one in Settings → Providers", preview, 0)
	}
	pool, overrideSummary, err := evalProviderPoolForOptions(pool, opts)
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error", err.Error(), preview, 0)
	}
	if overrideSummary != "" {
		session.recordSystem(overrideSummary)
	}

	// Create transient kind='eval_run' agent row whether preview or
	// persisted — for persisted runs we don't want to disturb the
	// live agent's running core, so the eval gets its own sibling
	// row to spawn against. Teardown deletes the row at run end.
	namePrefix := "__eval_run_"
	if preview {
		namePrefix = "__eval_preview_"
	}
	baseDirective := agent.Directive
	dirRow, err := s.store.CreateAgent(userID, fmt.Sprintf("%s%d__", namePrefix, time.Now().UnixNano()),
		baseDirective, agent.Mode, agent.Config, agent.ProjectID)
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"prepare eval agent: "+err.Error(), preview, 0)
	}
	_, _ = s.store.db.Exec(`UPDATE agents SET kind = 'eval_run' WHERE id = ?`, dirRow.ID)
	evalAgent, err := s.store.GetAgentByID(dirRow.ID)
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"reload eval agent: "+err.Error(), preview, 0)
	}
	defer func() {
		s.agents.Stop(evalAgent.ID)
		s.store.DeleteAgent(userID, evalAgent.ID)
	}()

	// Generate the eval session token and register the runSession so
	// the gateway handler can find it. The token is stable for the
	// whole run, including across mid-run respawns — the new core's
	// mcp_servers entry carries the same URL, so its tool calls land
	// on the same session and trajectory.
	token := fmt.Sprintf("eval-%d-%d", evalAgent.ID, time.Now().UnixNano())
	registerEvalSession(token, session)
	defer unregisterEvalSession(token)

	// If the run declares HTTPMocks or SandboxApps, boot the sandbox:
	// one intercept proxy + N sidecar apps. Their MCP URLs join the
	// eval-mock-gateway in the agent's mcp_servers list so the agent
	// sees real-app tools with real schemas alongside the synthesized
	// tool-level mocks (the two are complementary, not mutually
	// exclusive). Proxy + apps are torn down at run end.
	var sandboxMCPs []any
	var sandboxProxyURL string
	if len(opts.SandboxApps) > 0 || len(opts.HTTPMocks) > 0 {
		proxy, proxyURL, perr := startSandboxProxy(SandboxPolicy{Mocks: opts.HTTPMocks})
		if perr != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
				"start sandbox proxy: "+perr.Error(), preview, 0)
		}
		defer proxy.Stop()
		sandboxProxyURL = proxyURL
		for _, spec := range opts.SandboxApps {
			gatewayURL := fmt.Sprintf("http://127.0.0.1:%s", s.port)
			inst, serr := SpawnSandboxedApp(spec, proxyURL, gatewayURL, 15*time.Second)
			if serr != nil {
				return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
					"spawn sandbox app "+spec.Name+": "+serr.Error(), preview, 0)
			}
			defer inst.Stop()
			sandboxMCPs = append(sandboxMCPs, map[string]any{
				"name":      inst.Name,
				"url":       inst.MCPURL,
				"transport": "http",
				"no_spawn":  true,
			})
			session.recordSystem(fmt.Sprintf("sandbox: spawned %s on :%d (data=%s)", inst.Name, inst.Port, inst.DataDir))
		}
	}

	// Build the eval-mode config skeleton; we rewrite "directive" each
	// iteration to pick up accumulated edits.
	evalConfigFor := func(directive string) string {
		mcpServers := []any{
			map[string]any{
				"name":      "eval-mocks",
				"url":       fmt.Sprintf("http://127.0.0.1:%s/api/eval-mock-gateway/%s", s.port, token),
				"transport": "http",
				// no_spawn keeps mocks reachable from main only.
				"no_spawn": true,
			},
		}
		mcpServers = append(mcpServers, sandboxMCPs...)
		cfg := map[string]any{
			"directive":   directive,
			"mode":        evalAgent.Mode,
			"mcp_servers": mcpServers,
			// Server-only flags that turn off the auto-injected
			// gateway and channels for this run.
			"include_apteva_server": false,
			"include_channels":      false,
		}
		cfgJSON, _ := json.Marshal(cfg)
		return string(cfgJSON)
	}

	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, agent.ProjectID)
	if err != nil {
		providerEnv = map[string]string{}
	}
	// In sandbox mode, the eval-core also goes through the proxy so
	// any outbound HTTP it makes (e.g. an LLM provider call that
	// doesn't go via the resident provider env) lands on the same
	// allowlist/mock policy. LLM endpoints are in the proxy's default
	// allowlist so the provider still reaches its API.
	if sandboxProxyURL != "" {
		providerEnv["HTTP_PROXY"] = sandboxProxyURL
		providerEnv["HTTPS_PROXY"] = sandboxProxyURL
	}

	allowImprovements := opts.MaxIterations > 1
	rollup := &RunSuggestions{}
	const threadID = "main"
	var lastVerdict *JudgeVerdict
	iterationsCompleted := 0
	continueSameThread := false

	for iteration := 1; iteration <= opts.MaxIterations; iteration++ {
		iterationsCompleted = iteration
		if iteration > 1 {
			session.recordSystem(fmt.Sprintf("--- iteration %d of %d ---", iteration, opts.MaxIterations))
		}

		// Sync directive into the agent row + (re)spawn if not running.
		// continueSameThread=true means the previous iteration only
		// produced in_run_feedback, so we keep the core hot and the
		// thread context intact.
		if !continueSameThread {
			evalAgent.Directive = baseDirective
			evalAgent.Config = evalConfigFor(baseDirective)
			_ = s.store.UpdateAgent(evalAgent)
		}
		if !s.agents.IsRunning(evalAgent.ID) {
			// AgentManager.Start treats disk config.json as the single
			// source of truth for mcp_servers, so we have to seed the
			// instance dir with our eval-mock-gateway entry before
			// spawning — otherwise the core boots with no MCP and the
			// agent has no way to discover the mocked tools.
			if err := s.agents.PreSeedConfig(evalAgent.ID, evalAgent.Config); err != nil {
				snap := session.snapshot()
				return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, lastVerdict, finalRollup(rollup), "error",
					"seed eval config: "+err.Error(), preview, iterationsCompleted)
			}
			if err := s.agents.Start(evalAgent, providerEnv, s.port, pool, s.instanceSecret); err != nil {
				snap := session.snapshot()
				return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, lastVerdict, finalRollup(rollup), "error",
					"spawn eval core: "+err.Error(), preview, iterationsCompleted)
			}
			if !waitForCoreListening(s.agents.GetPort(evalAgent.ID), 10*time.Second) {
				snap := session.snapshot()
				return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, lastVerdict, finalRollup(rollup), "error",
					"eval core never listened", preview, iterationsCompleted)
			}
		}
		port := s.agents.GetPort(evalAgent.ID)
		apiKey := s.agents.GetCoreAPIKey(evalAgent.ID)

		// Drive the iteration's opening event. Attempt 1 is always
		// the description; continue-same-thread iterations post the
		// previous judge's in-run feedback; respawned iterations
		// (after directive edits) re-post the description so the
		// fresh core has the brief.
		var driveMsg string
		if iteration == 1 {
			driveMsg = ev.Description
		} else if continueSameThread && lastVerdict != nil && lastVerdict.SuggestedImprovements != nil && lastVerdict.SuggestedImprovements.InRunFeedback != "" {
			driveMsg = "Judge feedback from previous attempt: " + lastVerdict.SuggestedImprovements.InRunFeedback
			session.recordJudge(iteration, driveMsg)
		} else {
			driveMsg = ev.Description
		}

		// On a directive-edit respawn (iteration >= 2 with a fresh
		// core), give the autonomous loop a moment to fire its
		// startup heartbeat, then reset main to wipe it. Without
		// this, apteva-core's loop runs on its own goroutine the
		// instant the listener comes up — our POST /event lands in
		// the ~150ms window before the first drainEvents() call,
		// the loop processes "(no events)", emits a pacing reply,
		// and the test idles out before our brief is ever processed.
		// Skipped on iteration 1 because there's no observed race
		// there (the runner has nothing else queued so the agent's
		// "(no events)" heartbeat is harmless and the model still
		// processes the brief on its next iteration). Skipped on
		// continueSameThread because there's no fresh spawn — the
		// loop is settled and reset would wipe the prior context
		// the judge feedback builds on.
		if iteration >= 2 && !continueSameThread {
			time.Sleep(2 * time.Second)
			if err := resetMainThread(ctx, port, apiKey); err != nil {
				session.recordSystem("runner: reset main: " + err.Error())
			}
		}
		preTrajLen := session.trajectoryLen()
		if err := postCoreEvent(ctx, port, apiKey, threadID, driveMsg); err != nil {
			snap := session.snapshot()
			return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, lastVerdict, finalRollup(rollup), "error",
				"post to eval core: "+err.Error(), preview, iterationsCompleted)
		}

		if err := collectAssistantReplies(ctx, port, apiKey, threadID, session, ev.MaxTurns); err != nil {
			// Soft error — partial trajectory may still be gradable.
			session.recordSystem("runner: " + err.Error())
		}

		// Iter-1 autonomous-loop race recovery. apteva-core's loop
		// fires its first iteration the instant the listener comes
		// up. If our POST /event lands in the ~150ms window before
		// the first drainEvents, the loop processes "(no events)",
		// the model calls pace into deep sleep, and the brief is
		// effectively lost — the trajectory then has only a pace
		// tool call and nothing else. Sleep+reset+post once to
		// recover, then re-collect. Bounded to one retry to avoid
		// infinite loops on agents that genuinely won't act.
		//
		// Limited to iteration 1 because iterations >= 2 already do
		// sleep+reset+post unconditionally (see directive-edit
		// respawn path above) and continueSameThread iterations
		// have settled state that we don't want to wipe.
		if iteration == 1 && !continueSameThread && session.iter1RaceLikelySince(preTrajLen) {
			session.recordSystem("runner: iter-1 race detected (agent only paced/idled) — resetting + retrying brief")
			if err := resetMainThread(ctx, port, apiKey); err != nil {
				session.recordSystem("runner: race-retry reset: " + err.Error())
			} else {
				// Brief settle window before the re-post so the agent's
				// autonomous loop reaches a known paced state before our
				// event lands. Without it, the retry can land in the same
				// half-second the loop is processing its next iteration.
				time.Sleep(1500 * time.Millisecond)
				if err := postCoreEvent(ctx, port, apiKey, threadID, driveMsg); err != nil {
					session.recordSystem("runner: race-retry post: " + err.Error())
				} else if err := collectAssistantReplies(ctx, port, apiKey, threadID, session, ev.MaxTurns); err != nil {
					session.recordSystem("runner: race-retry collect: " + err.Error())
				}
			}
		}

		// Judge this iteration. allowImprovements gates whether the
		// judge proposes suggested_improvements at all.
		snap := session.snapshot()
		verdict, judgeErr := s.judgeWithMetaAgent(ctx, userID, agent.ProjectID, ev, snap, baseDirective, allowImprovements)
		if judgeErr != nil {
			return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, lastVerdict, finalRollup(rollup), "error",
				judgeErr.Error(), preview, iterationsCompleted)
		}
		lastVerdict = verdict

		// Decide whether this iteration is the loop's last regardless
		// of operator input. Folded into one isFinal so we have a
		// single place to gate the step callback's blocking-vs-not.
		isPass := verdict.Overall == "pass"
		isStrictViolation := len(session.strictViolations) > 0
		isMaxReached := iteration == opts.MaxIterations
		hasActionable := allowImprovements && verdict.SuggestedImprovements != nil &&
			(len(verdict.SuggestedImprovements.DirectiveEdits) > 0 ||
				strings.TrimSpace(verdict.SuggestedImprovements.InRunFeedback) != "")
		isFinal := isPass || isStrictViolation || isMaxReached || !hasActionable

		if isPass {
			// Mark every directive edit accumulated so far as
			// "helped" — they were live when we passed.
			for i := range rollup.DirectiveEdits {
				rollup.DirectiveEdits[i].Helped = true
			}
		}

		// Operator step gate. nil callback = legacy batch mode (always
		// continue). Streaming mode emits the iteration's verdict +
		// running suggestions rollup and blocks (when !isFinal) on the
		// operator's continue/stop choice.
		decision := StepContinue
		if stepCb != nil {
			decision = stepCb(IterationResult{
				Iteration:     iteration,
				MaxIterations: opts.MaxIterations,
				Verdict:       verdict,
				Suggestions:   rollup,
				Trajectory:    snap,
				Final:         isFinal,
			})
		}

		if isFinal || decision == StepStop {
			break
		}

		// Apply suggestions for the next iteration. hasActionable
		// guarantees one of these branches runs.
		sugg := verdict.SuggestedImprovements
		switch {
		case len(sugg.DirectiveEdits) > 0:
			for _, edit := range sugg.DirectiveEdits {
				rollup.DirectiveEdits = append(rollup.DirectiveEdits, edit)
				baseDirective = baseDirective + "\n\n" + strings.TrimSpace(edit.Add)
				session.recordSystem("applied directive edit: " + edit.Add)
			}
			// Teardown so the next iteration's spawn block respawns
			// fresh with the new directive. We also wipe the instance
			// dir's history/ folder — without that, apteva-core's
			// Session.LoadTail (thinker.go) loads the previous
			// iteration's assistant reply on the next spawn, the new
			// system prompt gets ignored in favour of "what I said
			// last time," and the directive edit looks like it had
			// no effect.
			s.agents.Stop(evalAgent.ID)
			if err := s.agents.WipeInstanceHistory(evalAgent.ID); err != nil {
				session.recordSystem("runner: wipe history: " + err.Error())
			}
			continueSameThread = false
		case strings.TrimSpace(sugg.InRunFeedback) != "":
			// Continue in same thread on the next loop pass.
			continueSameThread = true
		}
	}

	finishedAt := time.Now()
	trajectory := session.snapshot()

	status := "fail"
	errMsg := ""
	if lastVerdict != nil && lastVerdict.Overall == "pass" {
		status = "pass"
	}
	if len(session.strictViolations) > 0 {
		status = "error"
		errMsg = session.strictViolations[0]
	}

	return s.writeEvalRunWithDetails(ev.ID, startedAt, finishedAt, session, &trajectory, lastVerdict, finalRollup(rollup), status, errMsg, preview, iterationsCompleted)
}

func evalProviderPoolForOptions(pool []ProviderInfo, opts RunOptions) ([]ProviderInfo, string, error) {
	providerOverride := providerKeyFromName(opts.ProviderOverride)
	modelOverride := strings.TrimSpace(opts.ModelOverride)
	if providerOverride == "" && modelOverride == "" {
		return pool, "", nil
	}
	if len(pool) == 0 {
		return nil, "", fmt.Errorf("no LLM provider configured — add one in Settings → Providers")
	}

	selected := pool[0]
	if providerOverride != "" {
		found := false
		for _, p := range pool {
			if providerKeyFromName(p.Type) == providerOverride {
				selected = p
				found = true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("LLM provider override %q is not configured for this project", providerOverride)
		}
	}
	if modelOverride != "" {
		selected.ModelLarge = modelOverride
		selected.ModelMedium = modelOverride
		selected.ModelSmall = modelOverride
	}

	summary := "eval provider override: provider=" + selected.Type
	if modelOverride != "" {
		summary += " model=" + modelOverride
	}
	return []ProviderInfo{selected}, summary, nil
}

// finalRollup nils the RunSuggestions pointer when it carries
// nothing actionable, so InsertEvalRun leaves suggestions_json NULL
// for clean strict runs.
func finalRollup(r *RunSuggestions) *RunSuggestions {
	if r == nil {
		return nil
	}
	if len(r.DirectiveEdits) == 0 && len(r.MissingCapabilities) == 0 {
		return nil
	}
	return r
}

// writeEvalRun is the convenience shortcut for early-exit error
// paths where we haven't actually run anything against core. It
// captures the failure as an error-status run row (or a transient
// preview EvalRun when preview=true).
func (s *Server) writeEvalRun(evalID string, startedAt time.Time, session *runSession, verdict *JudgeVerdict, suggestions *RunSuggestions, status, errMsg string, preview bool, iterations int) (*EvalRun, error) {
	finishedAt := time.Now()
	trajectory := session.snapshot()
	return s.writeEvalRunWithDetails(evalID, startedAt, finishedAt, session, &trajectory, verdict, suggestions, status, errMsg, preview, iterations)
}

func (s *Server) writeEvalRunWithDetails(evalID string, startedAt, finishedAt time.Time, session *runSession, trajectory *Trajectory, verdict *JudgeVerdict, suggestions *RunSuggestions, status, errMsg string, preview bool, iterations int) (*EvalRun, error) {
	if iterations <= 0 {
		iterations = 1
	}
	run := EvalRun{
		EvalID:         evalID,
		StartedAt:      startedAt,
		FinishedAt:     &finishedAt,
		Status:         status,
		Trajectory:     *trajectory,
		Verdict:        verdict,
		Suggestions:    suggestions,
		DurationMS:     int(finishedAt.Sub(startedAt).Milliseconds()),
		TurnsUsed:      session.turnsUsed,
		IterationsUsed: iterations,
		ErrorMessage:   errMsg,
		Metrics:        session.metrics,
	}
	if !preview {
		id, _ := s.store.InsertEvalRun(run)
		run.ID = id
		_ = s.store.RollupEvalLastRun(evalID, status, finishedAt)
	}
	return &run, nil
}

func (s *Server) evalRunMetricsFromTelemetry(agentID int64, since time.Time) *EvalRunMetrics {
	if s == nil || s.store == nil || agentID == 0 {
		return nil
	}
	events, err := s.store.QueryTelemetry(agentID, "", since, 1000)
	if err != nil || len(events) == 0 {
		return nil
	}
	m := &EvalRunMetrics{AgentID: agentID}
	for _, ev := range events {
		switch ev.Type {
		case "llm.done":
			m.LLMCalls++
			var data map[string]any
			if json.Unmarshal(ev.Data, &data) == nil {
				m.TokensIn += intFromJSONNumber(data["tokens_in"])
				m.TokensOut += intFromJSONNumber(data["tokens_out"])
				m.TokensCached += intFromJSONNumber(data["tokens_cached"])
				m.LLMDurationMS += intFromJSONNumber(data["duration_ms"])
				m.CostUSD += floatFromJSONNumber(data["cost_usd"])
			}
		case "tool.call":
			m.ToolCalls++
		case "llm.error", "tool.error", "error":
			m.Errors++
		case "tool.result":
			var data map[string]any
			if json.Unmarshal(ev.Data, &data) == nil {
				if isErr, _ := data["is_error"].(bool); isErr {
					m.Errors++
				}
			}
		}
	}
	m.TokensTotal = m.TokensIn + m.TokensOut
	return m
}

func intFromJSONNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func floatFromJSONNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
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
// until the agent goes idle or max_turns assistant turns have been
// captured into the trajectory. Tool-result turns get a longer idle
// window than plain prose because the next model iteration can start
// several seconds after a real MCP tool result lands.
//
// Polling cadence is 500ms — fast enough to catch each turn shortly
// after it lands, slow enough not to thrash core.
type collectAssistantRepliesOptions struct {
	OverallTimeout                    time.Duration
	IdleWindow                        time.Duration
	PostToolIdleWindow                time.Duration
	MaxNonToolAssistantTurnsAfterTool int
	RequireMeaningfulActivityIdle     bool
	FailOnCoreExit                    bool
	CollectAllThreads                 bool
}

func collectAssistantReplies(ctx context.Context, port int, apiKey, threadID string, session *runSession, maxTurns int) error {
	return collectAssistantRepliesWithOptions(ctx, port, apiKey, threadID, session, maxTurns, collectAssistantRepliesOptions{})
}

func collectAssistantRepliesWithOptions(ctx context.Context, port int, apiKey, threadID string, session *runSession, maxTurns int, opts collectAssistantRepliesOptions) error {
	if maxTurns <= 0 {
		maxTurns = 5
	}
	overallTimeout := opts.OverallTimeout
	if overallTimeout <= 0 {
		overallTimeout = 120 * time.Second
	}
	overallDeadline := time.Now().Add(overallTimeout)
	idleSince := time.Time{}
	idleWindow := opts.IdleWindow
	if idleWindow <= 0 {
		idleWindow = 3 * time.Second
	}
	postToolIdleWindow := opts.PostToolIdleWindow
	if postToolIdleWindow <= 0 {
		postToolIdleWindow = 18 * time.Second
	}
	seenLastUpdate := time.Now()
	lastToolActivity := time.Time{}
	meaningfulActivity := false
	nonToolAssistantTurnsAfterTool := 0
	consecutiveFetchErrors := 0
	// lastMsgCount is the total number of messages (any role) we've
	// already processed per thread. Each poll, we walk the suffix from
	// this index forward and record anything new — assistant text via
	// recordAgent, assistant tool_calls via recordRealToolCall, and user
	// tool_results via attachToolResult. Tracking by overall message
	// index (instead of just assistant count, the old way) is what lets
	// tool_calls + tool_results land in the trajectory — they live on
	// assistant + user messages respectively.
	lastMsgCount := map[string]int{threadID: 0}
	// assistantTurns counts assistant messages (with text OR tool
	// calls) for the max_turns gate. A tool-call-only assistant
	// message still counts as a turn — it's real LLM work.
	assistantTurns := 0
	for time.Now().Before(overallDeadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		threadIDs := []string{threadID}
		if opts.CollectAllThreads {
			if ids, err := fetchThreadIDs(ctx, port, apiKey); err == nil && len(ids) > 0 {
				seen := map[string]bool{}
				threadIDs = threadIDs[:0]
				for _, id := range ids {
					if id == "" || seen[id] {
						continue
					}
					seen[id] = true
					threadIDs = append(threadIDs, id)
					if _, ok := lastMsgCount[id]; !ok {
						lastMsgCount[id] = 0
					}
				}
				if !seen[threadID] {
					threadIDs = append([]string{threadID}, threadIDs...)
				}
			}
		}
		hadNewMessages := false
		mainFetchOK := false
		for _, id := range threadIDs {
			msgs, err := fetchThreadMessages(ctx, port, apiKey, id)
			if err != nil {
				if id == threadID {
					consecutiveFetchErrors++
				}
				continue
			}
			if id == threadID {
				mainFetchOK = true
			}
			last := lastMsgCount[id]
			if len(msgs) > last {
				for _, m := range msgs[last:] {
					switch m.Role {
					case "assistant":
						text := m.text()
						if text != "" {
							session.recordAgent(text)
						}
						hasToolCalls := len(m.ToolCalls) > 0
						for _, tc := range m.ToolCalls {
							session.recordRealToolCall(tc.ID, tc.Name, tc.Args)
							if isPacingToolName(tc.Name) {
								continue
							}
							lastToolActivity = time.Now()
							nonToolAssistantTurnsAfterTool = 0
							meaningfulActivity = true
						}
						if text != "" && !hasToolCalls && !lastToolActivity.IsZero() {
							nonToolAssistantTurnsAfterTool++
						}
						if text != "" || hasToolCalls {
							assistantTurns++
						}
					case "user":
						// User messages can carry tool_results back-filling
						// prior tool calls. The Content field is usually
						// empty in that case; the framework events ("(no
						// events)" / "Events:..." / judge feedback) we
						// don't re-record from here since those come from
						// the runner itself.
						for _, tr := range m.ToolResults {
							toolName := session.attachToolResult(tr.CallID, tr.Content, tr.IsError)
							if toolName == "" || isPacingToolName(toolName) {
								continue
							}
							lastToolActivity = time.Now()
							nonToolAssistantTurnsAfterTool = 0
						}
					}
				}
				lastMsgCount[id] = len(msgs)
				hadNewMessages = true
			}
		}
		if !mainFetchOK && opts.FailOnCoreExit && consecutiveFetchErrors >= 3 && !isCoreListening(port) {
			return fmt.Errorf("eval core on port %d stopped while collecting replies", port)
		}
		if mainFetchOK {
			consecutiveFetchErrors = 0
		}
		if hadNewMessages {
			seenLastUpdate = time.Now()
			idleSince = time.Time{}
			if assistantTurns >= maxTurns {
				session.recordSystem(fmt.Sprintf("max_turns reached (%d)", maxTurns))
				return nil
			}
			if opts.MaxNonToolAssistantTurnsAfterTool > 0 &&
				meaningfulActivity &&
				!lastToolActivity.IsZero() &&
				!session.hasPendingTools() &&
				nonToolAssistantTurnsAfterTool >= opts.MaxNonToolAssistantTurnsAfterTool {
				session.recordSystem(fmt.Sprintf("no completed tool call after %d assistant turns", nonToolAssistantTurnsAfterTool))
				return nil
			}
		} else if time.Since(seenLastUpdate) > 500*time.Millisecond {
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			// If there are tool calls without matching results yet, the
			// agent is mid-iteration: an in-flight tool dispatch will
			// land a result + likely a follow-up assistant message
			// within seconds. Bumping the idle window here keeps us
			// from cutting off the trajectory between a tool call and
			// its result — that gap is exactly the case where the
			// judge needs the full picture to grade tool usage.
			effectiveIdle := idleWindow
			if session.hasPendingTools() || (!lastToolActivity.IsZero() && time.Since(lastToolActivity) < postToolIdleWindow) {
				effectiveIdle = postToolIdleWindow
			}
			canIdleOut := assistantTurns > 0
			if opts.RequireMeaningfulActivityIdle && !meaningfulActivity {
				canIdleOut = false
			}
			if time.Since(idleSince) >= effectiveIdle && canIdleOut {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("overall eval timeout (%s)", overallTimeout)
}

func isPacingToolName(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	return name == "pace" || strings.HasSuffix(name, ".pace")
}

// threadMessage is the parsed shape of one entry in core's
// /threads/<id>/context messages array. ToolCalls live on assistant
// messages; ToolResults on user messages (matched by call_id). We
// surface both so the eval trajectory captures real-MCP tool usage,
// not just the agent's prose.
type threadMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Parts   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
	ToolCalls   []threadToolCall   `json:"tool_calls,omitempty"`
	ToolResults []threadToolResult `json:"tool_results,omitempty"`
}

// threadToolCall mirrors apteva-core's JSON shape for one tool call
// inside an assistant message. The Args field is left as RawMessage
// so we don't lose precision re-marshalling for the trajectory.
type threadToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// threadToolResult mirrors apteva-core's tool-result entry. Content
// is a plain string from core's ToolResult — typically the MCP tool's
// response text (often a JSON-encoded payload, sometimes prose).
// IsError flags tool-side failure so the trajectory can route it to
// ToolCallRecord.Error instead of .Response.
type threadToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
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

func fetchThreadIDs(ctx context.Context, port int, apiKey string) ([]string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/threads", port)
	req, _ := newHTTPRequest(ctx, "GET", url, nil, apiKey)
	resp, err := httpClient5s.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var parsed []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed))
	for _, t := range parsed {
		if strings.TrimSpace(t.ID) != "" {
			out = append(out, t.ID)
		}
	}
	return out, nil
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
