package main

// environment_eval.go — run an eval with the agent executing IN a Environment.
//
// The eval flow the operator authors in plain English, executed for real:
//   1. Build a Environment from the agent's app bindings (deterministic; no LLM
//      picks apps — DeriveEnvironmentSpecForAgent installs the agent's real apps).
//   2. Run the agent-under-test INSIDE that environment — its tools are the real
//      in-environment apps (reached via the token-brokering environment-app gateway),
//      its egress virtualised by the edge + per-environment interceptor.
//   3. The meta-agent judges the trajectory against the goals.
//   4. Tear the environment down.
//
// This replaces the mock-gateway puppet with the agent's real apps. Reached
// via RunOptions.UseEnvironment so the existing path is untouched. Runtime needs an
// LLM provider + the core binary (gated, like the other real-core eval paths).

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *Server) runEvalInEnvironment(ctx context.Context, userID int64, agent *Agent, ev *Eval, preview bool, opts RunOptions) (*EvalRun, error) {
	startedAt := time.Now()
	session := newRunSession(ev)
	session.recordUser(ev.Description)

	// Provider preflight — same fail-fast as the mock-gateway path.
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

	// 1. Build the Environment from the agent's bindings (real, isolated apps).
	environmentID := fmt.Sprintf("eval-%d-%d", agent.ID, time.Now().UnixNano())
	environment, err := s.CreateEnvironmentForAgent(agent, environmentID)
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"build environment from agent bindings: "+err.Error(), preview, 0)
	}
	defer environment.Stop()
	session.recordSystem(fmt.Sprintf("environment %s ready — in-environment apps: %v", environmentID, environment.InstallNames()))

	// 1b. Seed the environment's starting state by driving the apps' real tools,
	//     before the agent runs — so the eval can test behavior over
	//     pre-existing state. The plan may be hand-authored or meta-agent
	//     proposed; execution is deterministic.
	seedAfterSpawn := opts.SeedAfterSpawn || len(opts.AppEventSubscriptions) > 0
	if len(opts.SeedPlan) > 0 && !seedAfterSpawn {
		if _, err := s.ExecuteSeedPlanWithBaseDir(environment, opts.SeedPlan, opts.SeedBaseDir); err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
				"seed environment: "+err.Error(), preview, 0)
		}
		session.recordSystem(fmt.Sprintf("seeded environment with %d call(s)", len(opts.SeedPlan)))
	}

	// 2. Spawn the agent-under-test INSIDE the environment. Its mcp_servers point
	//    at the in-environment apps (via the token-brokering environment-app gateway);
	//    egress runs through the environment edge. Torn down by environment.Stop().
	eventDriven := len(opts.AppEventSubscriptions) > 0
	wa, err := s.SpawnAgentInEnvironment(environment, EnvironmentAgentSpec{
		UserID:       userID,
		Source:       agent,
		ProviderPool: pool,
		StartPaused:  !eventDriven,
	})
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"spawn agent in environment: "+err.Error(), preview, 0)
	}
	if len(opts.AppEventSubscriptions) > 0 {
		if err := s.subscribeEnvironmentAgentToAppEvents(userID, environment, wa, opts.AppEventSubscriptions); err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
				"subscribe environment agent to app events: "+err.Error(), preview, 0)
		}
		session.recordSystem(fmt.Sprintf("subscribed environment agent to %d app-event stream(s)", len(opts.AppEventSubscriptions)))
	}
	if len(opts.SeedPlan) > 0 && seedAfterSpawn {
		if _, err := s.ExecuteSeedPlanWithBaseDir(environment, opts.SeedPlan, opts.SeedBaseDir); err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
				"seed environment: "+err.Error(), preview, 0)
		}
		session.recordSystem(fmt.Sprintf("seeded environment with %d call(s) after spawning agent", len(opts.SeedPlan)))
	}

	// 3. Drive: post the eval's brief, collect the agent's replies + tool
	//    calls from its thread history.
	const threadID = "main"
	preTrajLen := session.trajectoryLen()
	if err := postCoreEvent(ctx, wa.Port, wa.APIKey, threadID, ev.Description); err != nil {
		snap := session.snapshot()
		return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, nil, nil, "error",
			"post brief to environment agent: "+err.Error(), preview, 1)
	}
	if !eventDriven {
		if err := runCoreExecution(ctx, wa.Port, wa.APIKey); err != nil {
			snap := session.snapshot()
			return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, nil, nil, "error",
				"release environment agent execution: "+err.Error(), preview, 1)
		}
	}
	// Environment runs pay startup + real-sidecar + hosted-model latency. Give
	// slow hosted tool-call streams room to complete, but stop once the agent
	// repeatedly talks without producing another completed tool call.
	collectOpts := collectAssistantRepliesOptions{
		CollectAllThreads:                 true,
		OverallTimeout:                    6 * time.Minute,
		IdleWindow:                        20 * time.Second,
		PostToolIdleWindow:                45 * time.Second,
		MaxNonToolAssistantTurnsAfterTool: 1,
	}
	if eventDriven {
		collectOpts.RequireMeaningfulActivityIdle = true
		collectOpts.FailOnCoreExit = true
	}
	if err := collectAssistantRepliesWithOptions(ctx, wa.Port, wa.APIKey, threadID, session, ev.MaxTurns, collectOpts); err != nil {
		session.recordSystem("runner: " + err.Error())
	}
	// iter-1 autonomous-loop race recovery (same as runRealEvalCore): if the
	// core's loop fired "(no events)" → paced before our brief landed, the
	// brief is effectively lost. Reset main + re-post once and re-collect.
	if !eventDriven && session.iter1RaceLikelySince(preTrajLen) {
		session.recordSystem("runner: iter-1 race detected (agent only paced/idled) — resetting + retrying brief")
		if err := resetMainThread(ctx, wa.Port, wa.APIKey); err == nil {
			time.Sleep(1500 * time.Millisecond)
			if err := postCoreEvent(ctx, wa.Port, wa.APIKey, threadID, ev.Description); err == nil {
				_ = collectAssistantReplies(ctx, wa.Port, wa.APIKey, threadID, session, ev.MaxTurns)
			}
		}
	}
	session.metrics = s.evalRunMetricsFromTelemetry(wa.AgentID, startedAt)

	// 4. Judge against the goals (no deterministic criteria — the meta-agent
	//    reads the plain-English goals and grades).
	snap := session.snapshot()
	verdict, judgeErr := s.judgeWithMetaAgent(ctx, userID, agent.ProjectID, ev, snap, agent.Directive, false)
	if judgeErr != nil {
		return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, nil, nil, "error",
			"judge: "+judgeErr.Error(), preview, 1)
	}
	status := "fail"
	if verdict != nil && verdict.Overall == "pass" {
		status = "pass"
	}
	return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, verdict, nil, status, "", preview, 1)
}

func (s *Server) subscribeEnvironmentAgentToAppEvents(userID int64, environment *Environment, wa *EnvironmentAgent, subs []RunAppEventSubscription) error {
	if s.appEventDispatcher == nil {
		return fmt.Errorf("app event dispatcher not configured")
	}
	if environment == nil || wa == nil {
		return fmt.Errorf("environment agent not running")
	}
	for i, sub := range subs {
		app := strings.TrimSpace(sub.App)
		topic := strings.TrimSpace(sub.Topic)
		if app == "" || topic == "" {
			return fmt.Errorf("subscription %d: app and topic required", i)
		}
		if strings.Contains(app, ":") {
			return fmt.Errorf("subscription %d: app must not contain ':'", i)
		}
		spec, err := normalizeEnvironmentSubscriptionSpec(EnvironmentSubscriptionSpec{
			Source:           environmentSubscriptionSourceAppEvent,
			App:              app,
			Topic:            topic,
			TargetAgentAlias: wa.Alias,
			ThreadID:         strings.TrimSpace(sub.ThreadID),
			Name:             strings.TrimSpace(sub.Name),
			Description:      strings.TrimSpace(sub.Description),
		})
		if err != nil {
			return fmt.Errorf("subscription %d: %w", i, err)
		}
		environment.AddSubscriptionSpec(spec)
		if err := s.installEnvironmentSubscription(userID, environment, wa, spec); err != nil {
			return err
		}
	}
	return s.appEventDispatcher.Reconcile()
}
