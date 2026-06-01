package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestEval_InEnvironment_RealLLM_Storage is the real end-to-end proof: an eval runs
// the agent IN a Environment, using the real stuff —
//   - the REAL apteva-core binary spawns the agent (and the judge),
//   - the REAL storage app is built from local source + installed in the environment,
//   - the agent reaches it through the environment-app gateway (token brokered),
//   - the REAL LLM (OpenCode) drives the agent,
//   - the REAL meta-agent judges the trajectory against plain-English goals.
//
// Gated: skips without the OpenCode key (env or ../core/.env), the core
// binary, or the storage source. Run:
//
//	go test -run TestEval_InEnvironment_RealLLM_Storage -v -timeout 600s
func TestEval_InEnvironment_RealLLM_Storage(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)  // skips if no key
	corePath := findCoreBinary(t)   // skips if core binary absent
	_ = findAppSource(t, "storage") // skips if storage source absent

	directive := "You manage files using the storage app. When asked to save text to a file, " +
		"call the storage upload tool with the file name and the content encoded as base64, " +
		"then reply confirming the file was saved."
	s, userID, agent := setupRealServer(t, apiKey, corePath, "files-agent", directive)
	// Stop EVERY spawned core (under-test agent, environment agent, meta-agent) so a
	// lingering child's stdio pipe doesn't hang `go test` after the run.
	t.Cleanup(func() { s.agents.StopAll(3 * time.Second) })

	// The judge meta-agent must be warm before we grade.
	if err := prewarmMetaAgent(t, s, userID, 45*time.Second); err != nil {
		t.Fatalf("prewarm meta-agent: %v", err)
	}

	// Bind storage to the agent → the Environment derives + installs it from source.
	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('storage','local','','','{}')`)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, '', 'running')`, appID)
	if err != nil {
		t.Fatalf("seed install: %v", err)
	}
	installID, _ := res.LastInsertId()
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, agent.ID); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	// The eval — plain English, LLM-judged. No deterministic criteria.
	ev, err := s.store.CreateAgentEval(Eval{
		AgentID:     agent.ID,
		Name:        "saves a file via storage",
		Source:      "user",
		MaxTurns:    8,
		Description: "Save a file named report.txt containing the text 'done' using the storage app, then tell me it's saved.",
		Goals: []string{
			"The agent uses the storage app to upload/save a file.",
			"The agent confirms to the user that the file was saved.",
		},
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	// RUN IT IN A WORLD.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{UseEnvironment: true})
	if err != nil {
		t.Fatalf("runEval(UseEnvironment): %v", err)
	}

	t.Logf("status=%s turns=%d", run.Status, run.TurnsUsed)
	for _, turn := range run.Trajectory.Turns {
		switch {
		case turn.ToolCall != nil:
			t.Logf("  TOOL %s.%s args=%s", turn.ToolCall.App, turn.ToolCall.Tool, string(turn.ToolCall.Args))
		case turn.Role == "agent" && turn.Content != "":
			t.Logf("  AGENT %.140s", turn.Content)
		}
	}
	if run.Verdict != nil {
		t.Logf("verdict=%s reasoning=%.240s", run.Verdict.Overall, run.Verdict.Reasoning)
	}

	if run.Status == "error" {
		t.Fatalf("eval errored (infra, not agent behavior): %s", run.ErrorMessage)
	}
	// Proof the agent ran IN the environment against the REAL storage app: an actual
	// storage TOOL CALL in the trajectory (only possible if the agent reached
	// the in-environment storage sidecar via the token-brokering environment-app gateway).
	// Check the ToolCall records specifically — not the whole trajectory JSON,
	// which would also match our own "in-environment apps: [storage]" system note.
	calledStorage := false
	for _, turn := range run.Trajectory.Turns {
		if turn.ToolCall == nil {
			continue
		}
		name := strings.ToLower(turn.ToolCall.App + " " + turn.ToolCall.Tool)
		if strings.Contains(name, "storage") || strings.Contains(name, "files") {
			calledStorage = true
		}
	}
	if !calledStorage {
		trajJSON, _ := json.Marshal(run.Trajectory)
		t.Fatalf("agent never called a storage tool in the environment.\nTrajectory: %s", trajJSON)
	}
	t.Logf("✓ real core ran the agent IN a derived Environment and called the REAL storage app via the environment-app gateway; meta-agent judged (overall=%s)",
		run.Status)
}
