package main

// eval_realllm_test.go — full end-to-end test of the eval system
// using a REAL apteva-core for both the agent-under-test AND the
// meta-agent judge. Both are pointed at OpenCode Go (the workspace's
// flat-rate subscription provider).
//
// This is the highest-fidelity test we have. It:
//   1. Spawns the real apteva-core binary (found in
//      $APTEVA_CORE_BIN, ../core/apteva-core, or ~/.apteva/bin/apteva-core)
//      twice — once as the greeter agent, once as the platform's
//      meta-agent judge.
//   2. Wires both spawns to the OpenCode Go provider via an
//      encrypted DB row (mirrors what Settings → Providers does in
//      prod).
//   3. Stands up a real net.Listener so the spawned core can call
//      back to handleEvalMockGateway over loopback HTTP.
//   4. Calls Server.runEval and asserts the resulting EvalRun has a
//      parseable verdict whose per_goal grading covers the eval's
//      goals.
//
// Skipped quietly when:
//   - testing.Short() (it's slow + costs API calls)
//   - OPENCODE_GO_API_KEY isn't set in env OR in ../core/.env
//   - the apteva-core binary can't be located
//
// What this protects:
//   - The mock-gateway HTTP path (URL shape, JSON-RPC framing,
//     trajectory recording) actually works against a real MCP client
//     (the spawned core's HTTP MCP transport).
//   - The provider plumbing (DB row → decrypt → env-var injection →
//     spawned core picking up OPENCODE_GO_API_KEY) is intact.
//   - The meta-agent judge produces a JSON verdict that
//     parseJudgeReply accepts, against a real frontier model rather
//     than a hand-crafted fixture.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// loadOpenCodeGoKey returns the API key for opencode.ai/zen/go from
// the environment, falling back to ../core/.env (the workspace's
// canonical .env). Skips the test cleanly when neither yields a key —
// CI without OpenCode Go credentials shouldn't fail here.
func loadOpenCodeGoKey(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-LLM eval test in -short mode")
	}
	if key := os.Getenv("OPENCODE_GO_API_KEY"); key != "" {
		return key
	}
	// Fall back to ../core/.env (the workspace convention). We avoid
	// pulling in godotenv as a server/go.mod dep just for one line —
	// the file is tiny and the format is KEY=value.
	for _, candidate := range []string{
		filepath.Join("..", "core", ".env"),
		filepath.Join("..", ".env"),
	} {
		if key := readDotenvKey(candidate, "OPENCODE_GO_API_KEY"); key != "" {
			return key
		}
	}
	t.Skip("OPENCODE_GO_API_KEY not set in env or ../core/.env; skipping real-LLM eval test")
	return ""
}

func readDotenvKey(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	prefix := key + "="
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		val := strings.TrimPrefix(line, prefix)
		val = strings.Trim(val, `"'`)
		return val
	}
	return ""
}

// findCoreBinary resolves the real apteva-core binary path. Search
// order matches what an operator would expect:
//   1. APTEVA_CORE_BIN env override
//   2. ../core/apteva-core (build-local.sh output)
//   3. ~/.apteva/bin/apteva-core (installed CLI's symlink)
// Skips the test cleanly when nothing's found.
func findCoreBinary(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("APTEVA_CORE_BIN"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
		t.Skipf("APTEVA_CORE_BIN=%s does not exist", env)
	}
	candidates := []string{
		filepath.Join("..", "core", "apteva-core"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".apteva", "bin", "apteva-core"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("apteva-core binary not found (searched ../core/apteva-core and ~/.apteva/bin/apteva-core)")
	return ""
}

// setupRealServer builds the minimum Server scaffolding needed to
// drive runEval against real spawned cores: a real listener bound
// to a random port so the spawned core can hit the mock gateway,
// a real on-disk SQLite DB, a real 32-byte secret, and an
// AgentManager pointed at the real core binary.
//
// Returns the Server, the user id seeded for it, and the agent the
// caller should run evals against. Cleans up listeners, DB,
// and tmp dirs via t.Cleanup.
func setupRealServer(t *testing.T, apiKey, corePath, agentName, agentDirective string) (*Server, int64, *Agent) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	dataDir := t.TempDir()
	store, err := NewStore(filepath.Join(dataDir, "evals.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	s := &Server{
		store:          store,
		secret:         secret,
		port:           strconv.Itoa(port),
		dataDir:        dataDir,
		agents:         NewAgentManager(filepath.Join(dataDir, "agents"), corePath),
		broadcaster:    NewTelemetryBroadcaster(),
		instanceSecret: "real-llm-test-secret",
	}

	// Only the eval-mock-gateway route needs to be reachable for the
	// spawned core. Other endpoints (telemetry, channels) are absent
	// — the spawned core's calls to them just 404 silently, which is
	// fine for a one-shot eval. The handler expects to see the path
	// without the /api prefix (production wraps apiMux in
	// http.StripPrefix("/api")), so we mirror that here.
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/eval-mock-gateway/", s.handleEvalMockGateway)
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", apiMux))
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go httpServer.Serve(listener) //nolint:errcheck // shutdown emits ErrServerClosed
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	})

	user, err := store.CreateUser("e2e-evals@example.com", "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Encrypt + persist the OpenCode Go provider. Two keys land in
	// the encrypted blob:
	//   OPENCODE_GO_API_KEY — injected as env var via
	//     GetAllProviderEnvVars (isEnvVar is true → spawned core sees
	//     it, providers.go:262).
	//   model_large — used by GetProviderPool to set the default
	//     model in config.json (kimi-k2.6 is OpenCode Go's flagship).
	providerData := map[string]string{
		"OPENCODE_GO_API_KEY": apiKey,
		"model_large":         "kimi-k2.6",
	}
	plaintext, _ := json.Marshal(providerData)
	enc, err := Encrypt(secret, string(plaintext))
	if err != nil {
		t.Fatalf("encrypt provider: %v", err)
	}
	if _, err := store.CreateProvider(user.ID, 0, "opencode-go", "OpenCode Go", enc); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	agent, err := store.CreateAgent(user.ID, agentName, agentDirective, "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// Stop any spawned cores when the test ends so we don't leak
	// processes across runs.
	t.Cleanup(func() {
		s.agents.Stop(agent.ID)
	})
	return s, user.ID, agent
}

// prewarmMetaAgent spawns the platform_helper agent through
// AgentManager directly and waits up to budget for it to bind its
// port. Two test-specific tweaks vs. ensureMetaAgentRunning:
//
//  1. We override the helper's Config to set include_apteva_server=false
//     and include_channels=false. In production, AgentManager.Start
//     injects an MCP entry that spawns the apteva-server binary as a
//     subprocess (`--mcp-gateway`); inside `go test`, os.Executable()
//     resolves to the test binary, which rejects that flag — the
//     spawned core then fails to bring up its MCP plane and never
//     listens. Disabling both system MCPs makes the meta-agent a
//     pure judge-LLM sidecar, which is the only role we need it in
//     for an eval run anyway.
//  2. We wait `budget` (30s) for the port to bind, vs.
//     ensureMetaAgentRunning's hardcoded 8s — cold spawns on a
//     loaded laptop easily exceed 8s.
func prewarmMetaAgent(t *testing.T, s *Server, userID int64, budget time.Duration) error {
	t.Helper()
	helper, err := s.store.GetOrCreatePlatformHelper(userID, judgeSystemPrompt)
	if err != nil {
		return err
	}
	helper.Config = `{"include_apteva_server":false,"include_channels":false}`
	if err := s.store.UpdateAgent(helper); err != nil {
		return err
	}
	providerEnv, _ := s.store.GetAllProviderEnvVars(userID, s.secret, "")
	pool := s.GetProviderPool(userID, "")
	if len(pool) == 0 {
		return errors.New("no provider in pool for prewarm")
	}
	if err := s.agents.Start(helper, providerEnv, s.port, pool, s.instanceSecret,
		s.getBrowserConfig(userID, defaultProviderForInstance(helper), "")); err != nil {
		return err
	}
	if !waitForCoreListening(s.agents.GetPort(helper.ID), budget) {
		return errors.New("meta-agent never listened within budget")
	}
	t.Logf("meta-agent pre-warmed and listening on port %d", s.agents.GetPort(helper.ID))
	return nil
}

func TestEval_EndToEnd_RealLLM(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)
	corePath := findCoreBinary(t)
	s, userID, agent := setupRealServer(t, apiKey, corePath, "greeter-under-test",
		"When the user greets you, reply briefly and politely with a single short sentence.")

	// Stop the meta-agent + its tmp dir get cleaned up automatically
	// (the helper sits under the same AgentManager). We just need to
	// make sure we don't leak it past the test.
	t.Cleanup(func() {
		if helper, err := s.store.GetOrCreatePlatformHelper(userID, judgeSystemPrompt); err == nil && helper != nil {
			s.agents.Stop(helper.ID)
		}
	})

	// Pre-warm the meta-agent. ensureMetaAgentRunning has an 8-second
	// hardcoded deadline for the spawned core to listen on its port,
	// which is generous in production (the meta-agent has been
	// resident since boot) but tight for a cold macOS spawn during
	// a test. We side-step the deadline by spawning the helper
	// directly through AgentManager and waiting up to 30s ourselves
	// — once it's listening, runEval's later call to
	// ensureMetaAgentRunning sees IsRunning(helper.ID)=true and
	// returns without re-checking the port.
	if err := prewarmMetaAgent(t, s, userID, 30*time.Second); err != nil {
		t.Fatalf("pre-warm meta-agent: %v", err)
	}

	ev, err := s.store.CreateAgentEval(Eval{
		ID:          "ev-e2e-greet",
		AgentID:     agent.ID,
		Name:        "Greets the user",
		Description: "Hello! How are you today?",
		Goals: []string{
			"The agent replies with a greeting or polite response in English.",
		},
		MaxTurns:  2,
		Schedule:  "manual",
		Source:    "user",
		SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	// 2-minute hard ceiling.
	//
	// Budget breakdown: agent spawn (~5s) + 1-2 agent LLM turns
	// (~10s each) + judge waitForAssistantReply (60s default in
	// platform_agent.go) leaves ~25s of slack. If runEval hits the
	// ceiling, we want to surface what state the trajectory
	// reached — not just fail blindly — so this test does its
	// assertions even on a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Logf("running eval against real apteva-core (this calls the OpenCode Go API)…")
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{MaxIterations: 1})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if run == nil {
		t.Fatal("nil run")
	}

	t.Logf("status=%s duration=%dms turns=%d iterations=%d",
		run.Status, run.DurationMS, run.TurnsUsed, run.IterationsUsed)
	if run.ErrorMessage != "" {
		t.Logf("error_message: %s", run.ErrorMessage)
	}
	for i, turn := range run.Trajectory.Turns {
		switch turn.Role {
		case "agent":
			t.Logf("  turn %d AGENT: %s", i, truncate(turn.Content, 200))
		case "user":
			t.Logf("  turn %d USER:  %s", i, truncate(turn.Content, 200))
		case "tool":
			if turn.ToolCall != nil {
				t.Logf("  turn %d TOOL:  %s.%s", i, turn.ToolCall.App, turn.ToolCall.Tool)
			}
		case "system":
			t.Logf("  turn %d SYS:   %s", i, truncate(turn.Content, 200))
		}
	}

	// ─── Agent-half assertions (must hold regardless of judge state) ───
	//
	// The eval-core path is the system-under-test's main load-bearing
	// half: spawn a core, feed it the brief, record its replies through
	// the mock gateway, persist the run row. We assert these even if
	// the judge half (below) fails — they're independent.

	agentReplies := 0
	for _, turn := range run.Trajectory.Turns {
		if turn.Role == "agent" && strings.TrimSpace(turn.Content) != "" {
			agentReplies++
		}
	}
	if agentReplies == 0 {
		t.Error("agent produced zero replies; runner can't ferry messages from core to trajectory")
	}
	if run.TurnsUsed == 0 {
		t.Error("TurnsUsed is 0 despite agent replies recorded")
	}
	if run.DurationMS <= 0 {
		t.Errorf("DurationMS=%d, expected >0", run.DurationMS)
	}
	// run.ID > 0 confirms the row was persisted to agent_eval_runs
	// (preview runs would have ID=0, but this test uses runEval not
	// previewEval).
	if run.ID == 0 {
		t.Error("eval run row was not persisted")
	} else {
		fromDB, err := s.store.GetEvalRun(run.ID)
		if err != nil {
			t.Errorf("fetch persisted run: %v", err)
		} else if fromDB.Status != run.Status {
			t.Errorf("persisted status drift: db=%q in-memory=%q", fromDB.Status, run.Status)
		}
	}

	// ─── Judge-half assertions ───
	//
	// judgeWithMetaAgent posts to the meta-agent's "main" thread
	// (resetting it first so each call starts clean) and polls for
	// the first new assistant message. The meta-agent's directive
	// (judgeSystemPrompt) tells the LLM to reply with a single JSON
	// verdict object — parseJudgeReply accepts plain JSON or
	// codefence-wrapped JSON, so minor formatting drift from the
	// model is tolerated.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("runEval hit the test deadline: %s", run.ErrorMessage)
	}
	if run.Status == "error" {
		t.Fatalf("run failed at the runner level: %s", run.ErrorMessage)
	}
	if run.Verdict == nil {
		t.Fatalf("judge produced no verdict (error_message=%q)", run.ErrorMessage)
	}
	if run.Verdict.Overall != "pass" && run.Verdict.Overall != "fail" {
		t.Errorf("verdict.Overall must be pass|fail, got %q", run.Verdict.Overall)
	}
	if len(run.Verdict.PerGoal) == 0 {
		t.Error("judge did not grade any goals — buildJudgePrompt or parser regressed")
	}
	for _, g := range run.Verdict.PerGoal {
		t.Logf("  per_goal: verdict=%-4s goal=%q why=%s",
			g.Verdict, truncate(g.Goal, 80), truncate(g.Why, 120))
		if g.Verdict != "pass" && g.Verdict != "fail" {
			t.Errorf("per_goal verdict must be pass|fail, got %q", g.Verdict)
		}
	}
}

// TestEval_EndToEnd_RealLLM_MockedTool exercises the mocked-MCP
// plumbing end-to-end: the eval declares a social.publish_post mock,
// the spawned apteva-core boots with the eval-mock-gateway as its
// only MCP server, the gateway synthesises a tool entry, the agent's
// trajectory + judge verdict are persisted.
//
// What this asserts:
//   - PreSeedConfig actually lands mcp_servers on disk (without it,
//     the spawned core boots with mcp=[none] and the agent has no
//     way to discover the tool).
//   - The mock gateway's URL path (/api/eval-mock-gateway/<token>/mcp)
//     is reachable from the spawned core — the BOOT log line "MCP
//     eval-mocks (http): 1 tools registered" is the live signal.
//   - The judge meta-agent grades a real trajectory (whether or not
//     the agent actually called the tool) and produces a parseable
//     verdict with per-goal results.
//
// What this DOES NOT assert (and why):
//   - That the agent actually called social.publish_post. There's a
//     real race in apteva-core's autonomous loop: the loop fires its
//     first "(no events)" iteration before our POST /event can land
//     (~150ms window), the model decides to pace, and the next
//     iteration is 2 minutes away. collectAssistantReplies exits
//     after 3s idle so the model never gets a second chance to
//     process the event. The fix needs a deeper change in the eval
//     runner (pause-then-post, or wait-for-first-iteration). Until
//     that lands, the model's behaviour on the first event is
//     non-deterministic.
func TestEval_EndToEnd_RealLLM_MockedTool(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)
	corePath := findCoreBinary(t)
	s, userID, agent := setupRealServer(t, apiKey, corePath, "social-publisher-under-test",
		"You are a social-media publishing assistant. When the user asks you to publish something, call the social.publish_post tool with a `content` argument containing exactly what the user asked you to post. After the tool returns, send a short confirmation to the user that includes the returned post_id.")

	t.Cleanup(func() {
		if helper, err := s.store.GetOrCreatePlatformHelper(userID, judgeSystemPrompt); err == nil && helper != nil {
			s.agents.Stop(helper.ID)
		}
	})
	if err := prewarmMetaAgent(t, s, userID, 30*time.Second); err != nil {
		t.Fatalf("pre-warm meta-agent: %v", err)
	}

	ev, err := s.store.CreateAgentEval(Eval{
		ID:          "ev-e2e-social-publish",
		AgentID:     agent.ID,
		Name:        "Publishes to social media",
		Description: "Please publish a post on Twitter that says: Hello from Apteva!",
		Goals: []string{
			"The agent calls the social.publish_post tool exactly once.",
			"The content argument passed to social.publish_post contains the phrase 'Hello from Apteva!'.",
			"The agent sends a confirmation reply to the user after the tool returns.",
		},
		Mocks: []EvalMock{{
			App:    "social",
			Tool:   "publish_post",
			Return: json.RawMessage(`{"post_id":"tw_abc123","status":"published","url":"https://twitter.com/apteva/status/tw_abc123"}`),
		}},
		MaxTurns:  4,
		Schedule:  "manual",
		Source:    "user",
		SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Logf("running mocked-tool eval against real apteva-core…")
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{MaxIterations: 1})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if run == nil {
		t.Fatal("nil run")
	}

	t.Logf("status=%s duration=%dms turns=%d iterations=%d",
		run.Status, run.DurationMS, run.TurnsUsed, run.IterationsUsed)
	if run.ErrorMessage != "" {
		t.Logf("error_message: %s", run.ErrorMessage)
	}
	for i, turn := range run.Trajectory.Turns {
		switch turn.Role {
		case "agent":
			t.Logf("  turn %d AGENT: %s", i, truncate(turn.Content, 200))
		case "user":
			t.Logf("  turn %d USER:  %s", i, truncate(turn.Content, 200))
		case "tool":
			if turn.ToolCall != nil {
				t.Logf("  turn %d TOOL:  %s.%s args=%s mocked=%v",
					i, turn.ToolCall.App, turn.ToolCall.Tool,
					truncate(string(turn.ToolCall.Args), 200), turn.ToolCall.Mocked)
			}
		case "system":
			t.Logf("  turn %d SYS:   %s", i, truncate(turn.Content, 200))
		}
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("runEval hit the test deadline: %s", run.ErrorMessage)
	}
	if run.Status == "error" {
		t.Fatalf("run failed at the runner level: %s", run.ErrorMessage)
	}

	// ─── Structural assertions ───
	//
	// We log tool calls but don't require any to land because of the
	// autonomous-loop race documented at the top of this test. If
	// the model DID call the tool we still verify the gateway
	// served it from the mock (Mocked=true) rather than falling
	// through to the stub-default — a stub-default would mean the
	// (app, tool) match logic regressed.
	var toolCalls []*ToolCallRecord
	for _, turn := range run.Trajectory.Turns {
		if turn.Role == "tool" && turn.ToolCall != nil {
			toolCalls = append(toolCalls, turn.ToolCall)
		}
	}
	t.Logf("agent made %d tool call(s) during the run", len(toolCalls))
	for _, tc := range toolCalls {
		if tc.App == "social" && tc.Tool == "publish_post" && !tc.Mocked {
			t.Errorf("social.publish_post fell through to stub-default instead of matching the declared mock: warning=%q", tc.Warning)
		}
	}

	// Judge verdict must be present and structurally valid. The pass/fail
	// outcome depends on whether the model called the tool, which is
	// flaky per the docstring — but the JUDGE has to grade what it sees,
	// either way, and produce a parseable verdict with per-goal results.
	if run.Verdict == nil {
		t.Fatalf("judge produced no verdict (error_message=%q)", run.ErrorMessage)
	}
	t.Logf("verdict.Overall=%s reasoning=%s", run.Verdict.Overall, truncate(run.Verdict.Reasoning, 200))
	if run.Verdict.Overall != "pass" && run.Verdict.Overall != "fail" {
		t.Errorf("verdict.Overall must be pass|fail, got %q", run.Verdict.Overall)
	}
	if len(run.Verdict.PerGoal) != len(ev.Goals) {
		t.Errorf("judge graded %d goals; eval declared %d", len(run.Verdict.PerGoal), len(ev.Goals))
	}
	for _, g := range run.Verdict.PerGoal {
		t.Logf("  per_goal: verdict=%-4s goal=%q why=%s",
			g.Verdict, truncate(g.Goal, 80), truncate(g.Why, 160))
		if g.Verdict != "pass" && g.Verdict != "fail" {
			t.Errorf("per_goal verdict must be pass|fail, got %q", g.Verdict)
		}
	}

	// Eval run row was persisted with the full trajectory + verdict.
	if run.ID == 0 {
		t.Error("eval run row was not persisted")
	} else {
		fromDB, err := s.store.GetEvalRun(run.ID)
		if err != nil {
			t.Errorf("fetch persisted run: %v", err)
		} else if fromDB.Verdict == nil {
			t.Error("persisted run lost the verdict")
		}
	}
}

// TestEval_EndToEnd_RealLLM_ImprovementLoop exercises the multi-iteration
// improvement loop: an eval with a goal the agent will fail under its
// initial directive, run with MaxIterations=3, judge proposes
// directive edits, the runner applies them, subsequent iterations
// retry the eval with the augmented directive. If the edit helps,
// the iteration that passes flips RunSuggestions.DirectiveEdits[*].Helped
// to true.
//
// The eval: "Hello, can you help me?" with a goal requiring the
// literal token EVAL-MODE-ACTIVE in the agent's reply. No vanilla
// "helpful assistant" model would emit that token unguided, so iter 1
// is expected to fail. The judge's natural response is a directive
// edit along the lines of "always include 'EVAL-MODE-ACTIVE' in your
// replies". After the edit lands, the model should comply.
//
// What we ASSERT vs. LOG:
//   - The run produced a verdict and the judge graded each goal —
//     hard requirement; if the judge half breaks again we want a
//     clear signal.
//   - If iter 1 happened to pass (lucky model run), we log and
//     return — the loop wasn't exercised, but the test isn't
//     "wrong".
//   - If iter 1 failed: we assert IterationsUsed > 1 AND the
//     RunSuggestions rollup carries at least one DirectiveEdit (the
//     proposed fix) AND the trajectory has iteration-boundary system
//     turns.
//   - We log Helped=true/false per edit so the run report is human-
//     readable.
//
// What we DON'T assert (and why):
//   - The final verdict is pass. Two things can prevent this: the
//     autonomous-loop startup race on respawn after a directive
//     edit (documented in the MockedTool test) and judge
//     non-determinism. Asserting "agent improved" deterministically
//     would require the upstream race fix; for now the test
//     validates the iteration MECHANICS are wired up.
func TestEval_EndToEnd_RealLLM_ImprovementLoop(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)
	corePath := findCoreBinary(t)
	s, userID, agent := setupRealServer(t, apiKey, corePath, "improving-agent",
		"You are a helpful assistant. Reply briefly to user messages.")

	t.Cleanup(func() {
		if helper, err := s.store.GetOrCreatePlatformHelper(userID, judgeSystemPrompt); err == nil && helper != nil {
			s.agents.Stop(helper.ID)
		}
	})
	if err := prewarmMetaAgent(t, s, userID, 30*time.Second); err != nil {
		t.Fatalf("pre-warm meta-agent: %v", err)
	}

	ev, err := s.store.CreateAgentEval(Eval{
		ID:          "ev-improve-marker",
		AgentID:     agent.ID,
		Name:        "Includes EVAL-MODE-ACTIVE marker",
		Description: "Hello, can you help me?",
		Goals: []string{
			"The agent's reply contains the literal token 'EVAL-MODE-ACTIVE' (case-sensitive, exact match).",
		},
		MaxTurns:  2,
		Schedule:  "manual",
		Source:    "user",
		SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	// 8-minute ceiling: 3 iterations × (agent spawn + 1-2 turns + judge
	// pass) gives roughly 90s/iter — generous.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Logf("running improvement-loop eval (MaxIterations=3) against real apteva-core…")
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{MaxIterations: 3})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if run == nil {
		t.Fatal("nil run")
	}

	t.Logf("status=%s duration=%dms turns=%d iterations=%d/%d",
		run.Status, run.DurationMS, run.TurnsUsed, run.IterationsUsed, 3)
	if run.ErrorMessage != "" {
		t.Logf("error_message: %s", run.ErrorMessage)
	}
	for i, turn := range run.Trajectory.Turns {
		switch turn.Role {
		case "agent":
			t.Logf("  turn %d AGENT: %s", i, truncate(turn.Content, 200))
		case "user":
			t.Logf("  turn %d USER:  %s", i, truncate(turn.Content, 200))
		case "judge":
			t.Logf("  turn %d JUDGE iter=%d: %s", i, turn.Iteration, truncate(turn.Content, 200))
		case "system":
			t.Logf("  turn %d SYS:   %s", i, truncate(turn.Content, 200))
		}
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("runEval hit the test deadline: %s", run.ErrorMessage)
	}
	if run.Status == "error" {
		t.Fatalf("run failed at the runner level: %s", run.ErrorMessage)
	}

	// Judge must have produced a verdict in either case.
	if run.Verdict == nil {
		t.Fatalf("judge produced no verdict (error_message=%q)", run.ErrorMessage)
	}
	if len(run.Verdict.PerGoal) != len(ev.Goals) {
		t.Errorf("judge graded %d goals; eval declared %d", len(run.Verdict.PerGoal), len(ev.Goals))
	}

	// Happy-path short-circuit: model nailed it on iter 1.
	if run.IterationsUsed == 1 && run.Verdict.Overall == "pass" {
		t.Logf("agent passed on iteration 1; improvement loop not exercised this run (the model decided to include the marker without prompting)")
		return
	}

	// Otherwise we expect the loop to have done its job: at least 2
	// iterations and at least one suggestion accumulated.
	if run.IterationsUsed < 2 {
		t.Errorf("eval failed on iter 1 but IterationsUsed=%d; loop should have made another attempt", run.IterationsUsed)
	}
	if run.Suggestions == nil {
		t.Error("multi-iteration run produced no RunSuggestions rollup; judge should have proposed at least in_run_feedback")
	} else {
		t.Logf("rollup contains %d directive edits across %d iterations",
			len(run.Suggestions.DirectiveEdits), run.IterationsUsed)
		for i, edit := range run.Suggestions.DirectiveEdits {
			t.Logf("  edit %d: helped=%v id=%q add=%q reason=%q",
				i, edit.Helped, edit.ID,
				truncate(edit.Add, 120), truncate(edit.Reason, 120))
		}
		// If the final verdict is pass, every edit should be marked Helped=true
		// (the runner flips them when a subsequent iteration passes — see
		// runRealEvalCore's pass-handling block).
		if run.Verdict.Overall == "pass" {
			for i, edit := range run.Suggestions.DirectiveEdits {
				if !edit.Helped {
					t.Errorf("run passed but rollup edit %d (id=%q) is not marked Helped", i, edit.ID)
				}
			}
		}
	}

	// Trajectory should contain iteration-boundary system turns and at
	// least one judge feedback turn (when in_run_feedback fed forward).
	iterBoundaries := 0
	for _, turn := range run.Trajectory.Turns {
		if turn.Role == "system" && strings.Contains(turn.Content, "iteration") {
			iterBoundaries++
		}
	}
	if iterBoundaries == 0 {
		t.Errorf("expected at least one iteration-boundary system turn in trajectory; got 0")
	}
	t.Logf("trajectory has %d iteration boundaries", iterBoundaries)

	// Persisted run carries the suggestions rollup so the apply-handler
	// can offer them to the operator.
	if run.ID > 0 {
		fromDB, err := s.store.GetEvalRun(run.ID)
		if err != nil {
			t.Errorf("fetch persisted run: %v", err)
		} else if run.Suggestions != nil && fromDB.Suggestions == nil {
			t.Error("persisted run dropped the suggestions rollup")
		}
	}
}
