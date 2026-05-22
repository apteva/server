package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestEval_InWorld_RealLLM_Pushover is the direct-integration analog of the
// storage test: an agent that just has a Pushover *connection* (no app),
// running in a World. The real core + real LLM drive it; when it calls the
// Pushover tool through its connection MCP (/mcp/<connID>?world_id=…), the
// executor returns the catalog's mock_response — the real Pushover API is
// never hit. The real meta-agent judges.
//
// Gated: needs the OpenCode key + core binary (the catalog ships pushover).
//   go test -run TestEval_InWorld_RealLLM_Pushover -v -timeout 600s
func TestEval_InWorld_RealLLM_Pushover(t *testing.T) {
	apiKey := loadOpenCodeGoKey(t)
	corePath := findCoreBinary(t)

	directive := "You send push notifications via Pushover. When asked to notify someone, " +
		"call the Pushover send_notification tool with a message, then reply confirming you sent it."
	s, userID, agent := setupRealServer(t, apiKey, corePath, "notify-agent", directive)
	t.Cleanup(func() { s.agents.StopAll(3 * time.Second) })

	// Load the real catalog so pushover (with its shipped mock_response) is
	// resolvable by the connection MCP endpoint.
	if err := s.catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if s.catalog.Get("pushover") == nil {
		t.Skip("pushover not in catalog")
	}

	// A Pushover connection — creds are irrelevant, the world mocks the call.
	enc, err := Encrypt(s.secret, "{}")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	conn, err := s.store.CreateConnection(userID, "pushover", "Pushover", "My Pushover", "api_key", enc, "")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	// Give the agent the connection's MCP, exactly as the wizard's
	// boundConnectionIDs would. SpawnAgentInWorld carries this over, tagging
	// the URL with the world id so the executor mocks it.
	agent.Config = fmt.Sprintf(`{"mcp_servers":[{"name":"pushover","url":"http://127.0.0.1:%s/mcp/%d","transport":"http"}]}`, s.port, conn.ID)
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatalf("update agent config: %v", err)
	}

	if err := prewarmMetaAgent(t, s, userID, 45*time.Second); err != nil {
		t.Fatalf("prewarm meta-agent: %v", err)
	}

	ev, err := s.store.CreateAgentEval(Eval{
		AgentID:     agent.ID,
		Name:        "sends a pushover notification",
		Source:      "user",
		MaxTurns:    8,
		Description: "Send a Pushover notification that says 'Build passed', then tell me you sent it.",
		Goals: []string{
			"The agent sends a notification via the Pushover integration.",
			"The agent confirms to the user that the notification was sent.",
		},
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{UseWorld: true})
	if err != nil {
		t.Fatalf("runEval(UseWorld): %v", err)
	}

	t.Logf("status=%s turns=%d", run.Status, run.TurnsUsed)
	for _, turn := range run.Trajectory.Turns {
		switch {
		case turn.ToolCall != nil:
			t.Logf("  TOOL %s.%s args=%s resp=%s", turn.ToolCall.App, turn.ToolCall.Tool,
				string(turn.ToolCall.Args), string(turn.ToolCall.Response))
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
	// Proof: the agent called the Pushover tool in the world, and (with fake
	// creds) only the mock could succeed — the response carries the catalog
	// mock_response (status:1 / its request id), never a real Pushover ack.
	calledPushover, gotMock := false, false
	for _, turn := range run.Trajectory.Turns {
		if turn.ToolCall == nil {
			continue
		}
		name := strings.ToLower(turn.ToolCall.App + " " + turn.ToolCall.Tool)
		if strings.Contains(name, "pushover") || strings.Contains(name, "notif") {
			calledPushover = true
			resp := string(turn.ToolCall.Response)
			if strings.Contains(resp, "647d2300") || strings.Contains(resp, `"status":1`) {
				gotMock = true
			}
		}
	}
	if !calledPushover {
		t.Fatalf("agent never called the Pushover tool in the world.\nTrajectory: %+v", run.Trajectory.Turns)
	}
	t.Logf("✓ agent called Pushover in the world via its connection MCP; mock_response served=%v (no real notification sent)", gotMock)
}
