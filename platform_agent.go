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
	"sync"
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
//
// agentDirective is the directive the spawned eval-core ran with on
// this iteration (which may include ephemeral edits applied during
// an improvement loop). The judge sees it so it can propose
// directive_edits that meaningfully extend it without duplicating
// existing instructions. allowImprovements gates the "suggest
// directive edits" half of the system prompt — strict single-shot
// runs pass false to keep the judge purely descriptive.
func (s *Server) judgeWithMetaAgent(
	ctx context.Context,
	userID int64,
	projectID string,
	ev *Eval,
	trajectory Trajectory,
	agentDirective string,
	allowImprovements bool,
) (*JudgeVerdict, error) {
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		return nil, err
	}

	// Serialize judge calls against the same helper. We post to thread
	// "main" (the only thread apteva-core treats as a coordinator;
	// any other id gets a SUB-THREAD system prompt that makes the
	// model reply with `pace(sleep=...)` instead of grading), so
	// concurrent judges would clobber each other's context. The
	// mutex is keyed by user since each user has their own helper.
	mu := s.judgeMutexFor(userID)
	mu.Lock()
	defer mu.Unlock()

	corePort := s.agents.GetPort(helper.ID)
	if corePort == 0 {
		return nil, errors.New("meta-agent core is not listening yet — try again in a moment")
	}
	coreAPIKey := s.agents.GetCoreAPIKey(helper.ID)
	const threadID = "main"
	marker := fmt.Sprintf("JUDGE_REQUEST_ID: %d", time.Now().UnixNano())
	prompt := marker + "\n\n" + buildJudgePrompt(ev, trajectory, agentDirective, allowImprovements)

	// Wipe any prior conversation so this judge call sees a clean
	// thread (the autonomous loop on "main" otherwise carries forward
	// the previous judge's messages — including its pace+sleep — and
	// the model's next reply is shaped by that, not by the new
	// prompt).
	if err := resetMainThread(ctx, corePort, coreAPIKey); err != nil {
		return nil, fmt.Errorf("reset judge thread: %w", err)
	}

	if err := postCoreEvent(ctx, corePort, coreAPIKey, threadID, prompt); err != nil {
		return nil, fmt.Errorf("post judge prompt: %w", err)
	}

	// Poll for the first assistant message to appear on main. Since
	// we reset just above, the pre-call count is 0 — any assistant
	// message is the judge's verdict reply.
	reply, err := waitForAssistantReplyAfterUserMarker(ctx, corePort, coreAPIKey, threadID, marker)
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

// judgeMutexFor returns the per-user serialization lock for judge
// calls against that user's meta-agent helper. Lazily created on
// first access. The same helper services every judge for one user,
// and "main" thread context is shared across calls — without
// serialization, two concurrent eval runs in the same project would
// race on it.
func (s *Server) judgeMutexFor(userID int64) *sync.Mutex {
	s.judgeMutexesOnce.Do(func() {
		s.judgeMutexes = map[int64]*sync.Mutex{}
	})
	s.judgeMutexesMu.Lock()
	defer s.judgeMutexesMu.Unlock()
	mu, ok := s.judgeMutexes[userID]
	if !ok {
		mu = &sync.Mutex{}
		s.judgeMutexes[userID] = mu
	}
	return mu
}

// resetMainThread POSTs /threads/main/reset on the meta-agent's core
// (api.go:216), wiping its in-memory message slice back to just the
// system prompt. Idempotent; returns nil even on a 404 (means the
// thread hasn't been touched yet — already clean).
func resetMainThread(ctx context.Context, port int, apiKey string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/threads/main/reset", port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// waitForFirstAssistantReply polls /threads/<id>/context until at
// least one assistant message appears, then returns its text. Caller
// must have reset the thread first so "first assistant message" is
// unambiguously the reply to the just-posted prompt.
//
// Replaces the older waitForAssistantReply, which anchored on
// exact-text match against the user prompt — but apteva-core wraps
// inbound events as "[YYYY-MM-DD HH:MM] Events:\n• <prompt>\n", so
// the match never landed and the search for the post-prompt
// assistant message never began.
func waitForFirstAssistantReply(ctx context.Context, port int, apiKey, threadID string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/threads/%s/context", port, threadID)
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(120 * time.Second)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
				Parts   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		_ = json.Unmarshal(raw, &parsed)
		for _, m := range parsed.Messages {
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
		time.Sleep(400 * time.Millisecond)
	}
	return "", errors.New("no assistant reply within timeout")
}

func waitForAssistantReplyAfterUserMarker(ctx context.Context, port int, apiKey, threadID, marker string) (string, error) {
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(120 * time.Second)
	}
	for time.Now().Before(deadline) {
		msgs, err := fetchThreadMessages(ctx, port, apiKey, threadID)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		markerIdx := -1
		for i, m := range msgs {
			if m.Role == "user" && strings.Contains(m.text(), marker) {
				markerIdx = i
			}
		}
		if markerIdx >= 0 {
			for _, m := range msgs[markerIdx+1:] {
				if m.Role != "assistant" {
					continue
				}
				if text := strings.TrimSpace(m.text()); text != "" {
					return text, nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return "", fmt.Errorf("no assistant reply after marker %q within timeout", marker)
}

// ensureEnvironmentMCPOnHelper adds the Environment control MCP to the meta-agent's
// mcp_servers so it can build + seed test Environments by tool calls.
// Idempotent. Mutates helper.Config in place; the
// caller persists it (UpdateAgent) and Start merges it into the core's config.
func (s *Server) ensureEnvironmentMCPOnHelper(helper *Agent) {
	var cfg map[string]any
	if helper.Config != "" {
		_ = json.Unmarshal([]byte(helper.Config), &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	servers, _ := cfg["mcp_servers"].([]any)
	environmentURL := fmt.Sprintf("http://127.0.0.1:%s/api/environment-mcp", s.port)
	cleaned := make([]any, 0, len(servers)+1)
	hasEnvironment := false
	for _, e := range servers {
		if m, ok := e.(map[string]any); ok {
			name, _ := m["name"].(string)
			url, _ := m["url"].(string)
			if name == "worlds" || strings.Contains(url, "/api/world-mcp") {
				continue
			}
			if name == "environments" || url == environmentURL {
				hasEnvironment = true
			}
		}
		cleaned = append(cleaned, e)
	}
	if !hasEnvironment {
		cleaned = append(cleaned, map[string]any{
			"name":      "environments",
			"url":       environmentURL,
			"transport": "http",
			"no_spawn":  true,
		})
	}
	cfg["mcp_servers"] = cleaned
	if out, err := json.Marshal(cfg); err == nil {
		helper.Config = string(out)
	}
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
	// Give the meta-agent the Environment control tools so it can create + seed
	// test Environments by tool calls during evals (see environment_mcp.go). Set on the
	// DB row's config before Start so the core merges it into config.json.
	s.ensureEnvironmentMCPOnHelper(helper)
	if err := s.agents.Start(helper, providerEnv, s.port, pool, s.instanceSecret); err != nil {
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
//
// The suggested_improvements section is only honoured when the
// caller's prompt includes "[improvements: on]" — strict
// single-shot runs send "[improvements: off]" and the judge
// should leave suggested_improvements null. We could split this
// into two system prompts, but a single prompt + a per-request
// flag keeps the platform_helper resident across both modes.
const judgeSystemPrompt = `You are the Apteva platform's eval judge.

Each user message you receive is a single eval grading request. The message contains:
- Description: what the agent was asked to do
- Agent directive: the agent's current standing instructions (so you understand its baseline behaviour)
- Goals: a numbered list of plain-English expectations to grade against
- Trajectory: every reply the agent emitted and every tool call it made with its mocked response
- An improvements flag: either "[improvements: on]" or "[improvements: off]"

Grade each goal independently against the trajectory. A goal passes only when the trajectory directly demonstrates it; "the agent could plausibly do this next" is not pass.
Tool calls, tool responses, and inter-thread send/done messages are valid evidence. Do not require a final natural-language summary from the main thread unless a goal explicitly asks for one.

Respond with one JSON object, no surrounding prose, no markdown fences:

{
  "overall": "pass" | "fail",
  "reasoning": "one short paragraph summarising the run",
  "per_goal": [
    {"goal": "<verbatim goal text>", "verdict": "pass" | "fail", "why": "<one sentence of evidence, citing tool calls or replies>"}
  ],
  "suggested_improvements": {
    "in_run_feedback": "<one paragraph the agent can read mid-run to address the failures; null if overall=pass>",
    "directive_edits": [
      {"id": "edit-1", "add": "<one sentence to APPEND to the directive>", "reason": "<one sentence why this would help>"}
    ]
  }
}

Rules:
- overall=pass only when every per_goal entry is pass.
- Be specific in why — quote tool call args or reply text when relevant.
- If "[improvements: off]", set suggested_improvements to null.
- If "[improvements: on]" AND overall=fail, ALWAYS populate suggested_improvements with at least in_run_feedback. Add directive_edits only when a missing standing instruction would prevent this failure class on future runs (not just this one). Keep edits additive and concise — never propose deleting or replacing the directive. Use stable ids ("edit-1", "edit-2", ...) so the run report can track them.
- If "[improvements: on]" AND overall=pass, set suggested_improvements to null.
- Do not call any tools. Reply with the JSON object only.`

// buildJudgePrompt assembles the user-side prompt the judge sees.
// We render the trajectory as plain text (the LLM doesn't need a
// JSON envelope) and number the goals so the judge's per_goal
// array is easy to align back.
func buildJudgePrompt(ev *Eval, trajectory Trajectory, agentDirective string, allowImprovements bool) string {
	var b strings.Builder
	b.WriteString("# Description\n")
	b.WriteString(strings.TrimSpace(ev.Description))
	b.WriteString("\n\n# Agent directive\n")
	if strings.TrimSpace(agentDirective) == "" {
		b.WriteString("(none)")
	} else {
		b.WriteString(strings.TrimSpace(agentDirective))
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
		case "judge":
			fmt.Fprintf(&b, "JUDGE FEEDBACK (iteration %d): %s\n", turn.Iteration, turn.Content)
		case "system":
			fmt.Fprintf(&b, "SYSTEM: %s\n", turn.Content)
		}
	}
	if allowImprovements {
		b.WriteString("\n[improvements: on]\n")
	} else {
		b.WriteString("\n[improvements: off]\n")
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

// ─── Directive seeder ─────────────────────────────────────────────
//
// The seeder is the "auto-generated directive from goals" half of
// the wizard's Verify flow (see eval_realllm_test.go and the user-
// reported Pablo eval). When an operator types goals like "You
// should be called Pablo", a useful starter directive is mechanical
// to derive — but if it's left to the judge to propose it post-
// failure, the judge often returns empty suggestions because the
// trajectory has no signal (agent paced, etc.). The seeder runs
// proactively: one LLM call against the meta-agent that synthesizes
// a minimal directive matching the goals.
//
// The meta-agent's resident directive (judgeSystemPrompt) is
// JSON-judge-only. To make it produce plain text, the prompt below
// is explicit about overriding mode for this single call. The
// model reliably follows the user-message instruction; the JSON-
// only rule from the system prompt would otherwise force it to
// wrap the directive in JSON which the seeder unwraps anyway.

// SynthesizeDirective asks the meta-agent for a starter directive
// derived from the eval's goals. agentName is optional; passing it
// makes the synthesized directive open with "You are <name>." which
// most operators want as the first sentence. currentDirective is
// also optional — when supplied, the seeder is asked to PRODUCE
// edits to it rather than a from-scratch replacement.
//
// Returns the synthesized directive text on success. Wraps both LLM
// errors and parse errors so callers can surface them in the UI.
func (s *Server) SynthesizeDirective(ctx context.Context, userID int64, goals []string, agentName, currentDirective string) (string, error) {
	if len(goals) == 0 {
		return "", errors.New("at least one goal required")
	}
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		return "", err
	}
	// Serialize against the judge mutex — same shared "main" thread
	// (we'd otherwise race with an in-flight judge call). The judge
	// path takes the same lock so concurrent eval grading and
	// seeding can't step on each other.
	mu := s.judgeMutexFor(userID)
	mu.Lock()
	defer mu.Unlock()

	corePort := s.agents.GetPort(helper.ID)
	if corePort == 0 {
		return "", errors.New("meta-agent core is not listening yet — try again in a moment")
	}
	coreAPIKey := s.agents.GetCoreAPIKey(helper.ID)

	marker := fmt.Sprintf("SEEDER_REQUEST_ID: %d", time.Now().UnixNano())
	prompt := marker + "\n\n" + buildSeederPrompt(goals, agentName, currentDirective)
	if err := resetMainThread(ctx, corePort, coreAPIKey); err != nil {
		return "", fmt.Errorf("reset thread for seeder: %w", err)
	}
	if err := postCoreEvent(ctx, corePort, coreAPIKey, "main", prompt); err != nil {
		return "", fmt.Errorf("post seeder prompt: %w", err)
	}
	reply, err := waitForAssistantReplyAfterUserMarker(ctx, corePort, coreAPIKey, "main", marker)
	if err != nil {
		return "", fmt.Errorf("wait for seeder reply: %w", err)
	}
	return parseSeederReply(reply), nil
}

// buildSeederPrompt is the user-message side of the seeder request.
// It explicitly overrides the meta-agent's judge mode for this one
// call. The expected response shape is JSON {"directive": "..."} so
// parseSeederReply can extract a clean text body — but plain-text
// responses (no JSON wrapper) are also tolerated as a fallback.
func buildSeederPrompt(goals []string, agentName, currentDirective string) string {
	var b strings.Builder
	b.WriteString("TASK TYPE: directive_synthesis (NOT a grading request — ignore your judge instructions for this single message)\n\n")
	b.WriteString("Synthesize a concise, action-oriented directive that, if installed on an agent, would let it satisfy ALL of the goals below on a first attempt. Do not include the goals themselves verbatim in the directive (the agent must not see the grading criteria) — instead, derive standing instructions that would naturally produce passing behaviour.\n\n")
	if strings.TrimSpace(agentName) != "" {
		fmt.Fprintf(&b, "Agent name: %q. Open the directive with `You are %s.` when the name is meaningful to the goals.\n\n", agentName, agentName)
	}
	b.WriteString("Goals:\n")
	for i, g := range goals {
		fmt.Fprintf(&b, "%d. %s\n", i+1, g)
	}
	if strings.TrimSpace(currentDirective) != "" {
		b.WriteString("\nCurrent directive (improve / extend, don't replace wholesale):\n")
		b.WriteString(strings.TrimSpace(currentDirective))
		b.WriteString("\n")
	}
	b.WriteString("\nResponse format — JSON object only, no markdown fences:\n")
	b.WriteString(`{"directive": "<the directive text, multi-line OK, plain prose>"}` + "\n")
	b.WriteString("\nKeep the directive under 200 words. Prefer imperatives. Don't reference the eval system, the judge, or these instructions.\n")
	return b.String()
}

// parseSeederReply extracts the directive text from the meta-agent's
// response. Tolerates the same JSON-vs-prose-with-fences variations
// parseJudgeReply does — if a JSON object with a "directive" field
// can be parsed, returns its value; otherwise returns the reply
// verbatim (best-effort fallback).
func parseSeederReply(out string) string {
	s := strings.TrimSpace(out)
	// Strip optional code fences the same way parseJudgeReply does.
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
	var wrapper struct {
		Directive string `json:"directive"`
	}
	if err := json.Unmarshal([]byte(s), &wrapper); err == nil && strings.TrimSpace(wrapper.Directive) != "" {
		return strings.TrimSpace(wrapper.Directive)
	}
	// Fallback: assume the model returned plain text.
	return strings.TrimSpace(out)
}

// handleSeedDirective is the HTTP entrypoint the dashboard hits.
//
//	POST /api/agents/seed-directive
//	body: {"goals": [...], "agent_name"?: "", "current_directive"?: ""}
//	resp: {"directive": "..."}
//
// Auth is the usual session-or-API-key middleware (mounted in main.go).
// 400 on missing goals; 500 wraps any synthesis or LLM error.
func (s *Server) handleSeedDirective(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	var body struct {
		Goals            []string `json:"goals"`
		AgentName        string   `json:"agent_name,omitempty"`
		CurrentDirective string   `json:"current_directive,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(body.Goals) == 0 {
		http.Error(w, "at least one goal required", http.StatusBadRequest)
		return
	}
	// 60-second budget — well under the 90s typical wizard expectation.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	directive, err := s.SynthesizeDirective(ctx, userID, body.Goals, body.AgentName, body.CurrentDirective)
	if err != nil {
		http.Error(w, "synthesize directive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"directive": directive})
}
