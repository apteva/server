package main

// platform_agent.go — the apteva platform meta-agent.
//
// The meta-agent is a real apteva-core process spawned by the same
// AgentManager that handles user agents. It lives as an agents row
// with kind='platform_helper', filtered out of the operator's
// dashboard listings. The platform uses it for:
//
//   - Judging eval runs (PR-1, this file's only consumer)
//   - Future: failure classification, patch suggestion, onboarding
//     prompts, template recommendation
//
// Lifecycle:
//   1. judgeWithMetaAgent is called from the eval runner.
//   2. ensureMetaAgentRunning makes sure the platform_helper row
//      exists (creates idempotently) and its core process is up
//      (starts it via the normal AgentManager.Start path if needed,
//      reusing the user's LLM provider pool).
//   3. We POST the judge prompt to core's /event endpoint with a
//      fresh per-judge thread_id, which core lazy-spawns.
//   4. We poll /threads/<thread_id>/context until an assistant
//      message appears, then parse the JSON verdict from it.
//
// Why not a transient process per judge call: spawning core costs
// ~1-3s (build dir, config, port allocation, LLM cold start). A
// persistent helper keeps judge calls in the sub-second range
// after the first one. The process idles cheaply between calls
// (apteva-core's autonomous loop pauses when no events arrive).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// isCoreListening dials the agent's allocated port to confirm the
// spawned core is accepting connections before we dispatch.
func isCoreListening(port int) bool {
	if port == 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// bootMetaAgents brings up the meta-agent for every user with at
// least one LLM provider configured. Called once at server boot
// (in a background goroutine so the HTTP listener doesn't wait on
// core spawns). Failures are logged but never fatal — the lazy
// start in judgeWithMetaAgent picks up anything that didn't come
// up here.
//
// Users without a provider configured at boot time get their
// meta-agent spawned lazily on their first eval run, once they
// add a provider. The contract is: a meta-agent is always running
// for any user who could plausibly ask the platform to judge.
func (s *Server) bootMetaAgents() {
	// Brief delay so the HTTP listener is definitely accepting
	// connections before we start spawning cores (the spawned cores
	// connect back to apteva-server for telemetry).
	time.Sleep(500 * time.Millisecond)
	users, err := s.store.ListUsers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[boot] list users for meta-agent boot: %v\n", err)
		return
	}
	for _, u := range users {
		pool := s.GetProviderPool(u.ID, "")
		if len(pool) == 0 {
			// Skip — lazy start handles this user when they add a
			// provider and run their first eval.
			continue
		}
		if _, err := s.ensureMetaAgentRunning(u.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[boot] meta-agent for user=%d: %v\n", u.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[boot] meta-agent up for user=%d\n", u.ID)
	}
}

// judgeWithMetaAgent grades a single eval trajectory by dispatching
// to the real meta-agent core process. On any failure (no provider,
// meta-agent failed to start, HTTP error, JSON parse error) the
// returned verdict is nil and err carries the reason — callers
// surface that in the eval run row's error_message so operators see
// a real cause instead of a silent fail.
func (s *Server) judgeWithMetaAgent(
	ctx context.Context,
	userID int64,
	projectID string,
	ev *Eval,
	trajectory Trajectory,
) (*JudgeVerdict, error) {
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		return nil, err
	}

	threadID := fmt.Sprintf("judge-%d-%d", time.Now().UnixNano(), helper.ID)
	prompt := buildJudgePrompt(ev, trajectory)

	// POST the prompt to the meta-agent's /event endpoint. Core
	// lazy-spawns the thread on first event for an unknown id, so
	// no separate "create thread" call is needed.
	corePort := s.agents.GetPort(helper.ID)
	if corePort == 0 {
		return nil, errors.New("meta-agent core is not listening yet — try again in a moment")
	}
	coreAPIKey := s.agents.GetCoreAPIKey(helper.ID)
	if err := postCoreEvent(ctx, corePort, coreAPIKey, threadID, prompt); err != nil {
		return nil, fmt.Errorf("post judge prompt: %w", err)
	}

	// Poll the thread context until the meta-agent emits an
	// assistant reply. We accept the first non-empty assistant
	// message after our user prompt.
	reply, err := waitForAssistantReply(ctx, corePort, coreAPIKey, threadID, prompt)
	if err != nil {
		return nil, fmt.Errorf("wait for judge reply: %w", err)
	}

	verdict, err := parseJudgeReply(reply)
	if err != nil {
		return nil, fmt.Errorf("judge parse: %w (raw=%s)", err, truncate(reply, 400))
	}
	verdict.JudgeModel = "meta-agent"
	return verdict, nil
}

// ensureMetaAgentRunning makes sure the user's platform_helper
// agent exists in the DB and its core process is running. Returns
// the agent struct ready to dispatch to.
//
// First-call latency: ~2-3s (spawn + listener wait). Subsequent
// calls in the same server lifetime are no-ops.
func (s *Server) ensureMetaAgentRunning(userID int64) (*Agent, error) {
	helper, err := s.store.GetOrCreatePlatformHelper(userID, judgeSystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("ensure platform helper row: %w", err)
	}
	// Already running? Done.
	if s.agents.IsRunning(helper.ID) {
		return helper, nil
	}
	// Cold start. Needs the user's LLM provider pool to make calls.
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, "")
	if err != nil {
		providerEnv = map[string]string{}
	}
	pool := s.GetProviderPool(userID, "")
	if len(pool) == 0 {
		return nil, errors.New("no LLM provider configured — add one in Settings → Providers to enable evals")
	}
	if err := s.agents.Start(helper, providerEnv, s.port, pool, s.instanceSecret, s.getBrowserConfig(userID, defaultProviderForInstance(helper), "")); err != nil {
		return nil, fmt.Errorf("start meta-agent: %w", err)
	}
	// Persist new port + pid + status so future restarts pick it up.
	s.store.UpdateAgent(helper)

	// Wait for the core to be listening on its allocated port. Reuse
	// the dial pattern from AgentManager.Start's healthcheck goroutine,
	// but synchronously so we don't dispatch into a not-yet-ready
	// process.
	port := s.agents.GetPort(helper.ID)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if isCoreListening(port) {
			return helper, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, errors.New("meta-agent core failed to listen within 8s — check apteva-server logs")
}

// judgeSystemPrompt is the meta-agent's resident directive. Every
// /event posted to it produces one JSON-verdict reply. We keep the
// prompt strict so parseJudgeReply doesn't have to do prose
// gymnastics. The "be specific" line catches the most common
// failure mode of free-form judge LLMs: flat one-word "why" fields.
const judgeSystemPrompt = `You are the Apteva platform's eval judge.

Each user message you receive is a single eval grading request. The message contains:
- Trigger: the situation the agent was driven with
- Goals: a numbered list of plain-English expectations
- Trajectory: every reply the agent emitted and every tool call it made with its mocked response

Grade each goal independently against the trajectory. A goal passes only when the trajectory directly demonstrates it; "the agent could plausibly do this next" is not pass.

Respond with one JSON object, no surrounding prose, no markdown fences:

{
  "overall": "pass" | "fail",
  "reasoning": "one short paragraph summarising the run",
  "per_goal": [
    {"goal": "<verbatim goal text>", "verdict": "pass" | "fail", "why": "<one sentence of evidence, citing tool calls or replies>"}
  ]
}

overall=pass only when every per_goal entry is pass. Be specific in why — quote tool call args or reply text when relevant.

Do not call any tools. Reply with the JSON object only.`

// buildJudgePrompt assembles the user-side prompt the judge sees.
// We render the trajectory as plain text (the LLM doesn't need a
// JSON envelope) and number the goals so the judge's per_goal
// array is easy to align back.
func buildJudgePrompt(ev *Eval, trajectory Trajectory) string {
	var b strings.Builder
	b.WriteString("# Trigger\n")
	if trgJSON, err := json.MarshalIndent(ev.Trigger, "", "  "); err == nil {
		b.Write(trgJSON)
	}
	b.WriteString("\n\n# Goals\n")
	for i, g := range ev.Goals {
		fmt.Fprintf(&b, "%d. %s\n", i+1, g)
	}
	b.WriteString("\n# Trajectory\n")
	for _, turn := range trajectory.Turns {
		switch turn.Role {
		case "user":
			fmt.Fprintf(&b, "USER: %s\n", turn.Content)
		case "agent":
			fmt.Fprintf(&b, "AGENT: %s\n", turn.Content)
		case "tool":
			if turn.ToolCall == nil {
				continue
			}
			tc := turn.ToolCall
			fmt.Fprintf(&b, "TOOL CALL: %s.%s\n", tc.App, tc.Tool)
			if len(tc.Args) > 0 {
				fmt.Fprintf(&b, "  args: %s\n", string(tc.Args))
			}
			if tc.Error != "" {
				fmt.Fprintf(&b, "  error: %s\n", tc.Error)
			} else if len(tc.Response) > 0 {
				fmt.Fprintf(&b, "  response: %s%s\n", string(tc.Response), mockedNote(tc.Mocked, tc.Warning))
			}
		case "system":
			fmt.Fprintf(&b, "SYSTEM: %s\n", turn.Content)
		}
	}
	b.WriteString("\nGrade the trajectory against the goals. Return the JSON object only.")
	return b.String()
}

func mockedNote(mocked bool, warning string) string {
	if warning != "" {
		return " [warning: " + warning + "]"
	}
	if mocked {
		return " [mocked]"
	}
	return " [stub-default]"
}

// parseJudgeReply tolerates two irregularities models sometimes
// produce even with a strict system prompt: leading "Here is the
// JSON:" prose and trailing markdown fences. Both get stripped
// before json.Unmarshal. Anything beyond that fails out so real
// shape errors don't get swallowed.
func parseJudgeReply(out string) (*JudgeVerdict, error) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var v JudgeVerdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	if v.Overall == "" {
		// Roll up from per_goal if the model forgot to set overall.
		v.Overall = "pass"
		for _, g := range v.PerGoal {
			if g.Verdict != "pass" {
				v.Overall = "fail"
				break
			}
		}
	}
	return &v, nil
}

// ─── Core-process HTTP helpers ─────────────────────────────────────

// postCoreEvent POSTs a user message to an apteva-core's /event
// endpoint. threadID can be a fresh id; core lazy-spawns the thread.
// The core auth uses APTEVA_API_KEY (the per-instance token set at
// spawn time) via the Authorization header.
func postCoreEvent(ctx context.Context, port int, apiKey, threadID, message string) error {
	body, _ := json.Marshal(map[string]any{
		"thread_id": threadID,
		"message":   message,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("core /event http %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// waitForAssistantReply polls core's /threads/<id>/context until
// it sees an assistant message that wasn't there when the user
// prompt was posted. Returns the assistant message's text. ctx
// timeout caps the wait — pass a 30-60s budget for judge work.
func waitForAssistantReply(ctx context.Context, port int, apiKey, threadID, userPrompt string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/threads/%s/context", port, threadID)
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(60 * time.Second)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
				// Some core builds emit Parts[] alongside Content. We
				// concatenate text parts in that case.
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		_ = json.Unmarshal(raw, &parsed)

		// Find the latest assistant message that came AFTER our
		// user prompt. We anchor on the user prompt's exact text
		// because the thread might already have a system message
		// (the directive) at the head.
		userIdx := -1
		for i, m := range parsed.Messages {
			if m.Role == "user" && (m.Content == userPrompt || partsContain(m.Parts, userPrompt)) {
				userIdx = i
			}
		}
		if userIdx >= 0 {
			for i := len(parsed.Messages) - 1; i > userIdx; i-- {
				m := parsed.Messages[i]
				if m.Role != "assistant" {
					continue
				}
				text := m.Content
				if text == "" {
					for _, p := range m.Parts {
						if p.Type == "text" {
							text += p.Text
						}
					}
				}
				if strings.TrimSpace(text) != "" {
					return text, nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return "", errors.New("no assistant reply within timeout")
}

func partsContain(parts []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}, want string) bool {
	for _, p := range parts {
		if p.Type == "text" && p.Text == want {
			return true
		}
	}
	return false
}
