package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEval_InEnvironment_RealLLM_CRM is the smallest app-backed Environment eval example:
// one agent, one bound app (CRM), a derived Environment, and the real core + real LLM
// driving the agent through the in-environment CRM sidecar.
//
// Gated: skips without the OpenCode key, core binary, or CRM source. Run:
//
//	go test -run TestEval_InEnvironment_RealLLM_CRM -v -timeout 600s
func TestEval_InEnvironment_RealLLM_CRM(t *testing.T) {
	providerState := loadOpenAICodexProviderState(t)
	corePath := findCoreBinary(t)
	_ = findAppSource(t, "crm")

	directive := "You manage customer contacts using the CRM app. When asked to add a contact, " +
		"call contacts_create with the contact details, then use CRM again to verify the contact exists before replying."
	s, userID, agent := setupRealServerWithProviderState(t, corePath, "crm-agent", directive, 15, "llm", "OpenAI Codex", providerState)
	t.Cleanup(func() { s.agents.StopAll(3 * time.Second) })

	if err := prewarmMetaAgent(t, s, userID, 45*time.Second); err != nil {
		t.Fatalf("prewarm meta-agent: %v", err)
	}

	// Bind only CRM to the agent. The Environment should be derived from that binding
	// and install exactly the CRM app from local source.
	seedBoundApp(t, s, "crm", "", agent.ID)

	ev, err := s.store.CreateAgentEval(Eval{
		AgentID:     agent.ID,
		Name:        "creates and verifies a CRM contact",
		Source:      "user",
		MaxTurns:    10,
		Description: "Create a CRM contact for Ada Lovelace at ada@example.com, then verify the contact exists and tell me the result.",
		Goals: []string{
			"The agent creates a contact in CRM for Ada Lovelace with email ada@example.com.",
			"The agent verifies the contact exists using CRM before replying.",
			"The agent confirms the created contact to the user.",
		},
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

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
			t.Logf("  TOOL %s.%s args=%s resp=%.180s", turn.ToolCall.App, turn.ToolCall.Tool,
				string(turn.ToolCall.Args), string(turn.ToolCall.Response))
		case turn.Role == "agent" && turn.Content != "":
			t.Logf("  AGENT %.160s", turn.Content)
		}
	}
	if run.Verdict != nil {
		t.Logf("verdict=%s reasoning=%.240s", run.Verdict.Overall, run.Verdict.Reasoning)
	}

	if run.Status == "error" {
		t.Fatalf("eval errored (infra, not agent behavior): %s", run.ErrorMessage)
	}

	calledCreate, calledRead := false, false
	for _, turn := range run.Trajectory.Turns {
		if turn.ToolCall == nil {
			continue
		}
		name := strings.ToLower(turn.ToolCall.App + " " + turn.ToolCall.Tool)
		switch {
		case strings.Contains(name, "contacts_create"):
			calledCreate = true
		case strings.Contains(name, "contacts_get") || strings.Contains(name, "contacts_search"):
			calledRead = true
		}
	}
	if !calledCreate || !calledRead {
		trajJSON, _ := json.Marshal(run.Trajectory)
		t.Fatalf("agent did not create and verify a CRM contact in the environment (create=%v read=%v).\nTrajectory: %s",
			calledCreate, calledRead, trajJSON)
	}
	t.Logf("✓ real core ran the agent IN a derived Environment with only CRM installed; agent created and verified a contact via CRM")
}

func loadOpenAICodexProviderState(t *testing.T) map[string]any {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-LLM eval test in -short mode")
	}
	if token := strings.TrimSpace(os.Getenv("OPENAI_CODEX_ACCESS_TOKEN")); token != "" {
		return map[string]any{
			"auth": map[string]any{
				"type":     providerAuthTypeDeviceCode,
				"provider": openAICodexAuthProvider,
				"mode":     "chatgpt",
				"source":   "env",
			},
			"credentials": map[string]any{
				"access_token": token,
			},
			"runtime": map[string]any{"base_url": openAICodexBackendAPIBaseURL},
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir for local OpenAI Codex provider: %v", err)
	}
	for _, dataDir := range []string{
		filepath.Join(home, ".apteva"),
		filepath.Join(home, ".apteva-prod"),
	} {
		state, ok := readLocalOpenAICodexProviderState(t, dataDir)
		if ok {
			return state
		}
	}
	t.Skip("OpenAI Codex provider auth not found in OPENAI_CODEX_ACCESS_TOKEN, ~/.apteva, or ~/.apteva-prod")
	return nil
}

func readLocalOpenAICodexProviderState(t *testing.T, dataDir string) (map[string]any, bool) {
	t.Helper()
	secret, err := LoadSecret(dataDir)
	if err != nil {
		return nil, false
	}
	dbPath := filepath.Join(dataDir, "apteva.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, false
	}
	defer db.Close()

	var enc string
	err = db.QueryRow(`
		SELECT encrypted_data
		FROM providers
		WHERE name = 'OpenAI Codex' OR provider_type_id = 15
		ORDER BY updated_at DESC, id DESC
		LIMIT 1`,
	).Scan(&enc)
	if err != nil {
		return nil, false
	}
	plain, err := Decrypt(secret, enc)
	if err != nil {
		return nil, false
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(plain), &state); err != nil {
		return nil, false
	}
	if stateMap(state, "auth")["provider"] != openAICodexAuthProvider {
		return nil, false
	}
	if strings.TrimSpace(stringFromNested(state, "credentials", "access_token")) == "" &&
		strings.TrimSpace(stringFromNested(state, "credentials", "refresh_token")) == "" {
		return nil, false
	}
	return state, true
}
