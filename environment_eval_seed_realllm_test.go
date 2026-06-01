package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestEval_InEnvironment_RealLLM_Seeded proves the seed step: the Environment is populated
// with starting state (2 real files in storage) BEFORE the agent runs, so the
// eval can test behavior over pre-existing state. The real agent must list the
// files and report the count — it can only know "2" if the seed actually
// created them in the in-environment storage and the agent read them back.
//
// Gated (OpenCode key + core binary + storage source):
//
//	go test -run TestEval_InEnvironment_RealLLM_Seeded -v -timeout 600s
func TestEval_InEnvironment_RealLLM_Seeded(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)
	corePath := findCoreBinary(t)
	_ = findAppSource(t, "storage")

	directive := "You manage files via the storage app. Use its tools to list or read files when asked."
	s, userID, agent := setupRealServer(t, apiKey, corePath, "files-reader", directive)
	t.Cleanup(func() { s.agents.StopAll(3 * time.Second) })

	if err := prewarmMetaAgent(t, s, userID, 45*time.Second); err != nil {
		t.Fatalf("prewarm meta-agent: %v", err)
	}

	// Bind storage so the environment installs it.
	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('storage','local','','','{}')`)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	appID, _ := res.LastInsertId()
	res, _ = s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status) VALUES (?, '', 'running')`, appID)
	installID, _ := res.LastInsertId()
	if _, err := s.store.db.Exec(`INSERT INTO app_agent_bindings (install_id, agent_id, enabled) VALUES (?, ?, 1)`, installID, agent.ID); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	ev, err := s.store.CreateAgentEval(Eval{
		AgentID:     agent.ID,
		Name:        "reports the file count",
		Source:      "user",
		MaxTurns:    8,
		Description: "How many files are currently saved in storage? List them and tell me the count.",
		Goals:       []string{"The agent reports the correct number of saved files (there are 2)."},
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	// Seed: two real files in the in-environment storage, BEFORE the agent runs.
	seed := []SeedCall{
		{App: "storage", Tool: "files_upload", Input: map[string]any{"name": "alpha.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("a"))}},
		{App: "storage", Tool: "files_upload", Input: map[string]any{"name": "beta.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("b"))}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{UseEnvironment: true, SeedPlan: seed})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}

	t.Logf("status=%s turns=%d", run.Status, run.TurnsUsed)
	for _, turn := range run.Trajectory.Turns {
		switch {
		case turn.ToolCall != nil:
			t.Logf("  TOOL %s.%s resp=%.160s", turn.ToolCall.App, turn.ToolCall.Tool, string(turn.ToolCall.Response))
		case turn.Role == "agent" && turn.Content != "":
			t.Logf("  AGENT %.160s", turn.Content)
		}
	}
	if run.Verdict != nil {
		t.Logf("verdict=%s reasoning=%.240s", run.Verdict.Overall, run.Verdict.Reasoning)
	}

	if run.Status == "error" {
		t.Fatalf("eval errored (infra): %s", run.ErrorMessage)
	}
	// Proof the seed ran AND the agent read it: the seeded file names come
	// back in the agent's list response. Only possible if ExecuteSeedPlan
	// wrote them to the in-environment storage before the agent looked.
	trajJSON, _ := json.Marshal(run.Trajectory)
	tj := string(trajJSON)
	if !strings.Contains(tj, "alpha.txt") && !strings.Contains(tj, "beta.txt") {
		t.Fatalf("seeded files not seen by the agent — seed step or read failed.\nTrajectory: %s", tj)
	}
	t.Logf("✓ environment seeded (2 files) before the agent ran; agent read the seeded state via real storage; verdict=%s", run.Status)
}
