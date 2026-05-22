package main

// world_eval.go — run an eval with the agent executing IN a World.
//
// The eval flow the operator authors in plain English, executed for real:
//   1. Build a World from the agent's app bindings (deterministic; no LLM
//      picks apps — DeriveWorldSpecForAgent installs the agent's real apps).
//   2. Run the agent-under-test INSIDE that world — its tools are the real
//      in-world apps (reached via the token-brokering world-app gateway),
//      its egress virtualised by the edge + per-world interceptor.
//   3. The meta-agent judges the trajectory against the goals.
//   4. Tear the world down.
//
// This replaces the mock-gateway puppet with the agent's real apps. Reached
// via RunOptions.UseWorld so the existing path is untouched. Runtime needs an
// LLM provider + the core binary (gated, like the other real-core eval paths).

import (
	"context"
	"fmt"
	"time"
)

func (s *Server) runEvalInWorld(ctx context.Context, userID int64, agent *Agent, ev *Eval, preview bool, opts RunOptions) (*EvalRun, error) {
	startedAt := time.Now()
	session := newRunSession(ev)
	session.recordUser(ev.Description)

	// Provider preflight — same fail-fast as the mock-gateway path.
	pool := s.GetProviderPool(userID, agent.ProjectID)
	if len(pool) == 0 {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"no LLM provider configured — add one in Settings → Providers", preview, 0)
	}

	// 1. Build the World from the agent's bindings (real, isolated apps).
	worldID := fmt.Sprintf("eval-%d-%d", agent.ID, time.Now().UnixNano())
	world, err := s.CreateWorldForAgent(agent, worldID)
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"build world from agent bindings: "+err.Error(), preview, 0)
	}
	defer world.Stop()
	session.recordSystem(fmt.Sprintf("world %s ready — in-world apps: %v", worldID, world.InstallNames()))

	// 1b. Seed the world's starting state by driving the apps' real tools,
	//     before the agent runs — so the eval can test behavior over
	//     pre-existing state. The plan may be hand-authored or meta-agent
	//     proposed; execution is deterministic.
	if len(opts.SeedPlan) > 0 {
		if _, err := s.ExecuteSeedPlan(world, opts.SeedPlan); err != nil {
			return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
				"seed world: "+err.Error(), preview, 0)
		}
		session.recordSystem(fmt.Sprintf("seeded world with %d call(s)", len(opts.SeedPlan)))
	}

	// 2. Spawn the agent-under-test INSIDE the world. Its mcp_servers point
	//    at the in-world apps (via the token-brokering world-app gateway);
	//    egress runs through the world edge. Torn down by world.Stop().
	wa, err := s.SpawnAgentInWorld(world, WorldAgentSpec{UserID: userID, Source: agent})
	if err != nil {
		return s.writeEvalRun(ev.ID, startedAt, session, nil, nil, "error",
			"spawn agent in world: "+err.Error(), preview, 0)
	}

	// 3. Drive: post the eval's brief, collect the agent's replies + tool
	//    calls from its thread history.
	const threadID = "main"
	preTrajLen := session.trajectoryLen()
	if err := postCoreEvent(ctx, wa.Port, wa.APIKey, threadID, ev.Description); err != nil {
		snap := session.snapshot()
		return s.writeEvalRunWithDetails(ev.ID, startedAt, time.Now(), session, &snap, nil, nil, "error",
			"post brief to world agent: "+err.Error(), preview, 1)
	}
	if err := collectAssistantReplies(ctx, wa.Port, wa.APIKey, threadID, session, ev.MaxTurns); err != nil {
		session.recordSystem("runner: " + err.Error())
	}
	// iter-1 autonomous-loop race recovery (same as runRealEvalCore): if the
	// core's loop fired "(no events)" → paced before our brief landed, the
	// brief is effectively lost. Reset main + re-post once and re-collect.
	if session.iter1RaceLikelySince(preTrajLen) {
		session.recordSystem("runner: iter-1 race detected (agent only paced/idled) — resetting + retrying brief")
		if err := resetMainThread(ctx, wa.Port, wa.APIKey); err == nil {
			time.Sleep(1500 * time.Millisecond)
			if err := postCoreEvent(ctx, wa.Port, wa.APIKey, threadID, ev.Description); err == nil {
				_ = collectAssistantReplies(ctx, wa.Port, wa.APIKey, threadID, session, ev.MaxTurns)
			}
		}
	}

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
