package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestChannelChat_RealLLM_Codex_ActionBeforeReply is the critical
// dashboard-chat regression test: when the user asks for an action,
// the agent must call the action tool rather than only replying
// "I'll do it" and going idle.
//
// It uses a real apteva-core and the real OpenAI Codex provider, but
// the domain tool is a tiny local MCP server. No browser app, no
// integration credentials, no external sidecar.
//
// Opt-in:
//
//	APTEVA_RUN_REAL_LLM_TESTS=1 go test -run TestChannelChat_RealLLM_Codex_ActionBeforeReply -v -timeout 180s
func TestChannelChat_RealLLM_Codex_ActionBeforeReply(t *testing.T) {
	var marked atomic.Int64
	todoMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		respond := func(result any, errMsg string) {
			resp := map[string]any{"jsonrpc": "2.0"}
			if len(req.ID) > 0 {
				resp["id"] = req.ID
			}
			if errMsg != "" {
				resp["error"] = map[string]any{"code": -32603, "message": errMsg}
			} else {
				resp["result"] = result
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "todo", "version": "1.0.0"},
			}, "")
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{
				"name":        "mark_done",
				"description": "Mark the named todo item done. If the user asks to complete, finish, close, or mark a todo done, call this tool before replying with the result.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"item": map[string]any{"type": "string"}},
					"required":   []string{"item"},
				},
			}}}, "")
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name != "mark_done" {
				respond(nil, "unknown tool: "+p.Name)
				return
			}
			marked.Add(1)
			respond(map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": `{"status":"done","item":"alpha"}`,
			}}}, "")
		default:
			respond(nil, "method not found: "+req.Method)
		}
	}))
	t.Cleanup(todoMCP.Close)

	directive := strings.Join([]string{
		"# Role",
		"You complete todo actions requested in dashboard chat.",
		"# Operating Rules",
		"When the user asks you to mark a todo done, call the todo tool first, then reply in chat with the result.",
		"Do not only acknowledge action requests.",
	}, "\n")
	h := setupRealChannelChatHarness(t, "chat-action-under-test", directive,
		fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"todo","transport":"http","url":%q}]}`, todoMCP.URL))
	s, agent, chatID := h.server, h.agent, h.chatID
	h.post(t, "Mark todo alpha done now. Do not just acknowledge; complete it and tell me the result.")

	var toolSeq []string
	var finalReply string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		events, err := s.store.QueryTelemetry(agent.ID, "tool.call", time.Time{}, 100)
		if err == nil {
			toolSeq = toolNames(events)
		}
		finalReply = latestAgentChatReply(t, s, chatID)
		if marked.Load() > 0 && looksLikeCompletionReply(finalReply) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if marked.Load() == 0 {
		t.Fatalf("todo MCP mark_done was never called; tool sequence=%v", toolSeq)
	}
	if !looksLikeCompletionReply(finalReply) {
		t.Fatalf("final chat reply does not report completion: %q (tool sequence=%v)", finalReply, toolSeq)
	}
}

// TestChannelChat_RealLLM_Codex_DirectChatReplyAfterLookup reproduces the
// agent 327 failure: a read-only lookup produces an ambiguous result, so the
// agent must send a visible clarification instead of leaving it in plain model
// output and pacing.
func TestChannelChat_RealLLM_Codex_DirectChatReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-reply-after-lookup-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_DirectChatReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-reply-after-lookup-glm52-under-test", "glm-5.2", directive, config)
	})
}

func runRealDirectChatReplyAfterLookup(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	var listed atomic.Int64
	var lookupResultAtNano atomic.Int64
	scheduleMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		respond := func(result any, errMsg string) {
			response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			if errMsg != "" {
				response["error"] = map[string]any{"code": -32603, "message": errMsg}
			} else {
				response["result"] = result
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "schedule", "version": "1.0.0"},
			}, "")
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{
				"name":        "scheduled_posts_list",
				"description": "List scheduled social posts. If multiple posts match, do not guess; ask the operator which item in a visible reply.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"date": map[string]any{"type": "string"}},
				},
			}}}, "")
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name != "scheduled_posts_list" {
				respond(nil, "unknown tool: "+params.Name)
				return
			}
			listed.Add(1)
			lookupResultAtNano.Store(time.Now().UnixNano())
			respond(map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": `{"posts":[{"id":"post-10","time":"10:00 UTC","channels":["instagram"],"title":"Morning studio update"},{"id":"post-16","time":"16:00 UTC","channels":["instagram","linkedin"],"title":"Afternoon launch note"}]}`,
			}}}, "")
		default:
			respond(nil, "method not found: "+req.Method)
		}
	}))
	t.Cleanup(scheduleMCP.Close)

	directive := strings.Join([]string{
		"# Role",
		"You manage scheduled social posts requested by the operator.",
		"# Rules",
		"Inspect scheduled posts before changing them.",
		"Never guess when multiple posts match; ask which post the operator means.",
	}, "\n")
	config := fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"schedule","transport":"http","url":%q}]}`, scheduleMCP.URL)
	h := setupHarness(t, directive, config)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	_ = h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID)

	h.post(t, "Unschedule the Instagram post planned today.")
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	if listed.Load() == 0 {
		t.Fatal("agent did not inspect the ambiguous scheduled posts")
	}
	lookupAt := lookupResultAtNano.Load()
	if lookupAt == 0 {
		t.Fatal("schedule MCP did not record its lookup result time")
	}
	callEvents := newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)
	visibleAfterLookup := false
	for _, event := range callEvents {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &data) == nil &&
			(data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")) &&
			event.Time.UnixNano() > lookupAt {
			visibleAfterLookup = true
			break
		}
	}
	if !visibleAfterLookup {
		t.Fatalf("agent did not call channels_send after the lookup result; calls=%v", toolNames(callEvents))
	}

	var reply string
	err := h.server.store.db.QueryRow(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		ORDER BY id DESC LIMIT 1`, h.chatID, baselineMessageID).Scan(&reply)
	if err != nil {
		t.Fatalf("visible chat reply was not persisted after lookup: %v", err)
	}
	lower := strings.ToLower(reply)
	if !strings.Contains(lower, "which") && !strings.Contains(lower, "clarif") &&
		!(strings.Contains(lower, "10:00") && strings.Contains(lower, "16:00")) {
		t.Fatalf("visible reply does not clarify the two matches: %q", reply)
	}
}

// TestChannelChat_RealLLM_Codex_FinalReplyNotRepeatedAfterWake covers the
// sequential duplicate-reply failure mode: channels_send succeeds, its
// wake-on-result starts another reasoning iteration, and that iteration must
// settle without sending the same final answer again.
func TestChannelChat_RealLLM_Codex_FinalReplyNotRepeatedAfterWake(t *testing.T) {
	directive := strings.Join([]string{
		"# Role",
		"You answer brief dashboard-chat requests through channels_send.",
		"# Rules",
		"For the final-delivery probe, send exactly FINAL DELIVERY ACKNOWLEDGED to the current channel.",
		"That message fully completes the request; do not perform other work for the probe.",
	}, "\n")
	h := setupRealChannelChatHarness(t, "chat-final-reply-once-under-test", directive,
		`{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	if err := h.server.store.db.QueryRow(
		`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID,
	).Scan(&baselineMessageID); err != nil {
		t.Fatalf("query baseline chat message: %v", err)
	}

	h.post(t, "Run the final-delivery probe now.")
	// Production duplicates were observed a few seconds after the successful
	// send. Waiting for a quiet period after the post-result LLM iteration makes
	// the assertion cover that wake rather than racing the first persisted row.
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	calls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	var sends []channelSendCall
	for _, call := range calls {
		if call.Kind == "message" {
			sends = append(sends, call)
		}
	}
	if len(sends) != 1 {
		t.Fatalf("channels_send calls=%d, want exactly one after successful delivery: %+v", len(sends), calls)
	}
	if got := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sends[0].Text), ".!?")); got != "FINAL DELIVERY ACKNOWLEDGED" {
		t.Fatalf("channels_send text=%q, want final-delivery marker", sends[0].Text)
	}

	resultEvents := newChannelResultEvents(t, h.server, h.agent.ID, baselineResults)
	resultSucceeded := false
	for _, event := range resultEvents {
		var data struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.ID == sends[0].ID && data.Success {
			resultSucceeded = true
			break
		}
	}
	if !resultSucceeded {
		t.Fatalf("channels_send %q has no successful correlated result", sends[0].ID)
	}

	rows, err := h.server.store.db.Query(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query final chat replies: %v", err)
	}
	defer rows.Close()
	var replies []string
	for rows.Next() {
		var reply string
		if err := rows.Scan(&reply); err != nil {
			t.Fatalf("scan final chat reply: %v", err)
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate final chat replies: %v", err)
	}
	if len(replies) != 1 {
		t.Fatalf("persisted agent replies=%d, want exactly one: %q", len(replies), replies)
	}
}

// TestChannelChat_RealLLM_Codex_AllChannelKindsExactlyOnce covers the complete
// Apteva operator-channel contract with a real core and Codex call. In
// particular, the final status and visible message are deliberately sent in a
// parallel batch: this is the production pattern that previously made the
// status write return SQLITE_BUSY and caused the model to resend an already
// delivered chat response.
func TestChannelChat_RealLLM_Codex_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupRealChannelChatHarness(t, "channel-kinds-under-test", channelCoverageDirective(),
		`{"include_apteva_server":false,"include_channels":true}`)
	runRealChannelKindsExactlyOnce(t, h)
}

// TestChannelChat_RealLLM_OpenCodeGLM52_AllChannelKindsExactlyOnce runs the
// same operator-channel contract through OpenCode Go's GLM-5.2 model. Keeping
// the protocol and assertions shared makes this a provider-behavior comparison
// rather than a weaker provider-specific smoke test.
func TestChannelChat_RealLLM_OpenCodeGLM52_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupOpenCodeChannelChatHarness(t, "channel-kinds-glm52-under-test", "glm-5.2")
	runRealChannelKindsExactlyOnce(t, h)
}

func TestChannelChat_RealLLM_XAI_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupXAIChannelChatHarnessWithDirective(t, "channel-kinds-xai-under-test", channelCoverageDirective())
	runRealChannelKindsExactlyOnce(t, h)
}

// These two provider tests force core's discovery path even though the
// fixture has a small tool surface. Channels is configured as always-loaded,
// so the model must call channels_send directly without search_tools or a
// model-created worker. The deterministic core test separately covers the
// automatic >60-tool threshold with decoy schemas.
func TestChannelChat_RealLLM_Codex_AlwaysLoadedChannelsAvoidDiscovery(t *testing.T) {
	t.Setenv("APTEVA_TOOL_SEARCH", "on")
	h := setupRealChannelChatHarness(t, "channels-always-codex-under-test", channelsAlwaysLoadedDirective(),
		`{"include_apteva_server":false,"include_channels":true}`)
	runRealAlwaysLoadedChannels(t, h)
}

func TestChannelChat_RealLLM_OpenCodeGLM52_AlwaysLoadedChannelsAvoidDiscovery(t *testing.T) {
	t.Setenv("APTEVA_TOOL_SEARCH", "on")
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "channels-always-glm52-under-test", "glm-5.2", channelsAlwaysLoadedDirective())
	runRealAlwaysLoadedChannels(t, h)
}

func TestChannelChat_RealLLM_XAI_AlwaysLoadedChannelsAvoidDiscovery(t *testing.T) {
	t.Setenv("APTEVA_TOOL_SEARCH", "on")
	h := setupXAIChannelChatHarnessWithDirective(t, "channels-always-xai-under-test", channelsAlwaysLoadedDirective())
	runRealAlwaysLoadedChannels(t, h)
}

func channelsAlwaysLoadedDirective() string {
	return strings.Join([]string{
		"# Role",
		"You answer brief dashboard-chat checks directly through the visible channels_send tool.",
		"# Rules",
		"Do not call search_tools or spawn to communicate with the operator.",
		"For the readiness probe, send exactly CHANNELS ALWAYS READY to the current channel and do nothing else.",
	}, "\n")
}

func runRealAlwaysLoadedChannels(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	h.post(t, "Run the readiness probe now.")

	deadline := time.Now().Add(90 * time.Second)
	var calls []string
	var reply string
	for time.Now().Before(deadline) {
		events, err := h.server.store.QueryTelemetry(h.agent.ID, "tool.call", time.Time{}, 100)
		if err == nil {
			calls = toolNames(events)
		}
		reply = latestAgentChatReply(t, h.server, h.chatID)
		// Chat persistence and telemetry ingestion are separate writes. Wait
		// until both sides of the same successful send are observable before
		// evaluating the call sequence.
		if strings.Contains(reply, "CHANNELS ALWAYS READY") && containsTool(calls, "channels_send") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(reply, "CHANNELS ALWAYS READY") {
		t.Fatalf("always-loaded Channels probe did not produce the visible reply; calls=%v reply=%q", calls, reply)
	}
	if !containsTool(calls, "channels_send") {
		t.Fatalf("probe reply was not delivered through channels_send; calls=%v", calls)
	}
	if containsTool(calls, "search_tools") || containsTool(calls, "spawn") {
		t.Fatalf("always-loaded Channels caused discovery/delegation; calls=%v", calls)
	}
	if containsTool(calls, "channels_set_status") {
		t.Fatalf("brief channel message incorrectly created work status; calls=%v", calls)
	}
}

// TestChannelChat_RealLLM_OpenCodeGLM52_StatusNextAction verifies that a real
// model maps a natural-language scheduled responsibility onto the optional
// next/next_at status fields without publishing an Inbox item or chat message.
func TestChannelChat_RealLLM_OpenCodeGLM52_StatusNextAction(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-next-glm52-under-test", "glm-5.2", statusOnlyDirective())
	runRealStatusOnly(t, h, strings.Join([]string{
		"The daily affiliate sync just completed after importing 124 conversions.",
		"Record a completed current status with 100% progress.",
		"The nearest next responsibility must be exactly: Generate weekly performance report.",
		"It is due at 2026-07-20T09:00:00Z.",
		"This is monitoring state only; do not send a chat message or create an Inbox item.",
	}, "\n"), "Generate weekly performance report", "2026-07-20T09:00:00Z")
}

// TestChannelChat_RealLLM_OpenCodeGLM52_StatusWithoutNextAction verifies that
// the model leaves optional next fields empty when a one-off task has no
// meaningful planned follow-up.
func TestChannelChat_RealLLM_OpenCodeGLM52_StatusWithoutNextAction(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-no-next-glm52-under-test", "glm-5.2", statusOnlyDirective())
	runRealStatusOnly(t, h, strings.Join([]string{
		"A one-off cleanup of temporary test records just completed successfully.",
		"Record a completed current status with 100% progress.",
		"There is no follow-up responsibility and nothing else is scheduled or planned, so do not invent a next action or next time.",
		"This is monitoring state only; do not send a chat message or create an Inbox item.",
	}, "\n"), "", "")
}

// TestChannelChat_RealLLM_OpenCodeGLM52_RoutineWorkDoesNotPublishReport
// reproduces the Personal Agent pattern that previously produced one report
// after every hourly inbox cleanup. Routine daytime work should update mutable
// status, while the default unsolicited report remains an end-of-day digest.
func TestChannelChat_RealLLM_OpenCodeGLM52_RoutineWorkDoesNotPublishReport(t *testing.T) {
	directive := strings.Join([]string{
		"# Role",
		"You are a personal assistant responsible for routine Gmail hygiene.",
		"# Gmail Routine",
		"Check unread email about once every hour and mark only obvious promotional noise read.",
		"Report concise summaries of unread or important messages only when the operator asks.",
	}, "\n")
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "routine-no-report-glm52-under-test", "glm-5.2", directive)
	runRealWithoutReport(t, h, strings.Join([]string{
		"It is 09:00, not the end of the operator's day.",
		"The hourly Gmail check just completed: two promotional messages were marked read and three actionable messages were preserved.",
		"No operator requested a report and the directive defines no scheduled report cadence.",
		"Follow the Channels guidance for this routine result, then return to idle.",
	}, "\n"))
}

func statusOnlyDirective() string {
	return strings.Join([]string{
		"# Role",
		"You maintain concise operator-visible monitoring status for meaningful work units.",
		"# Rules",
		"Use the injected Channels capability guidance exactly.",
		"When asked only to record monitoring status, do not send chat messages or publish Inbox artifacts.",
		"Never repeat a successful status call.",
	}, "\n")
}

func TestChannelChat_RealLLM_Codex_StatusWorkSemantics(t *testing.T) {
	h := setupRealChannelChatHarness(t, "status-semantics-codex-under-test", statusSemanticsDirective(),
		`{"include_apteva_server":false,"include_channels":true}`)
	runRealStatusWorkSemantics(t, h)
}

func TestChannelChat_RealLLM_OpenCodeGLM52_StatusWorkSemantics(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-semantics-glm52-under-test", "glm-5.2", statusSemanticsDirective())
	runRealStatusWorkSemantics(t, h)
}

func TestChannelChat_RealLLM_XAI_StatusWorkSemantics(t *testing.T) {
	h := setupXAIChannelChatHarnessWithDirective(t, "status-semantics-xai-under-test", statusSemanticsDirective())
	runRealStatusWorkSemantics(t, h)
}

func TestChannelChat_RealLLM_OpenCodeKimiK27Code_StatusWorkSemantics(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-semantics-kimi-under-test", "kimi-k2.7-code", statusSemanticsDirective())
	runRealStatusWorkSemantics(t, h)
}

func TestChannelChat_RealLLM_OpenCodeMiniMaxM3_StatusWorkSemantics(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-semantics-minimax-under-test", "minimax-m3", statusSemanticsDirective())
	runRealStatusWorkSemantics(t, h)
}

func statusSemanticsDirective() string {
	return strings.Join([]string{
		"# Role",
		"You maintain operator-visible state while carrying out requested work.",
		"# Status protocol",
		"Apply the injected Channels capability guidance exactly.",
		"Use status only for meaningful operator-relevant work units, not your own administration or future-only scheduling.",
		"Do not send chat messages or Inbox artifacts during these monitoring scenarios.",
		"Never repeat a successful status call.",
	}, "\n")
}

func runRealStatusWorkSemantics(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	waitForInitialAgentTurn(t, h)

	t.Run("directive-and-future-schedule-are-not-work", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"An internal directive update just completed: the directive now says to run a CRM check every day at 09:00 UTC.",
			"No CRM check is active or completed as part of this event. Apply the Channels guidance, then return idle.",
		}, "\n"))
		assertNoStatusCalls(t, calls)
	})

	t.Run("completed-recurring-work-stays-completed", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"You just finished a meaningful multi-step daily CRM check and found no unresolved conversations.",
			"Update operator monitoring for that completed work.",
			"The nearest next responsibility is exactly: Run the next daily CRM check.",
			"That next responsibility is due at 2026-07-20T09:00:00Z.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "completed")
		assertProgress(t, call, 100, false)
		if call.Next != "Run the next daily CRM check." || call.NextAt != "2026-07-20T09:00:00Z" {
			t.Fatalf("completed recurring status next=%q next_at=%q, want exact scheduled responsibility; call=%+v", call.Next, call.NextAt, call)
		}
		assertTitleDoesNotDescribeFutureOrWait(t, call.Title)
	})

	t.Run("approval-pauses-the-current-work-unit", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A customer update is drafted as part of an unfinished publication workflow.",
			"The workflow cannot continue until the operator approves publication.",
			"Update operator monitoring. The nearest next responsibility is exactly: Publish customer update after approval.",
			"There is no deadline.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "waiting")
		assertProgress(t, call, 100, true)
		if call.Next != "Publish customer update after approval." || call.NextAt != "" {
			t.Fatalf("approval status next=%q next_at=%q, want next without invented deadline; call=%+v", call.Next, call.NextAt, call)
		}
		assertTitleDoesNotDescribeFutureOrWait(t, call.Title)
		if strings.TrimSpace(call.Detail) != "" && !containsAnyFold(call.Detail, "approval", "approve") {
			t.Fatalf("waiting status detail does not name approval dependency: %+v", call)
		}
	})

	t.Run("failed-prerequisite-is-blocked", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A CRM contact import stopped because authentication expired.",
			"The same import cannot continue until the CRM integration is reconnected.",
			"Update operator monitoring for the unfinished import.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "blocked")
		if strings.Contains(strings.ToLower(call.Title), "blocked") {
			t.Fatalf("blocked status title describes the condition instead of the work unit: %+v", call)
		}
		if !containsAnyFold(call.Detail, "auth", "expired", "reconnect") {
			t.Fatalf("blocked status detail does not explain the prerequisite failure: %+v", call)
		}
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_StatusExtendedSemantics(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t, "status-extended-glm52-under-test", "glm-5.2", statusSemanticsDirective())
	waitForInitialAgentTurn(t, h)

	t.Run("read-only-lookup-has-no-status", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, "Read your current directive and verify whether it mentions CRM. Do not change anything, then return idle.")
		assertNoStatusCalls(t, calls)
	})

	t.Run("delayed-current-work-is-waiting", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A requested notification is an active unfinished work unit and must be sent at 2026-07-20T10:00:00Z.",
			"It has not been sent yet. Update operator monitoring.",
			"The nearest next responsibility is exactly: Send requested notification.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "waiting")
		assertProgress(t, call, 100, true)
		if call.Next != "Send requested notification." || call.NextAt != "2026-07-20T10:00:00Z" {
			t.Fatalf("delayed status did not preserve exact next action/time: %+v", call)
		}
		assertTitleDoesNotDescribeFutureOrWait(t, call.Title)
	})

	t.Run("one-off-completion-clears-next", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A one-off cleanup of temporary test records just completed successfully.",
			"Update operator monitoring for the completed work. There is no follow-up responsibility or planned work.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "completed")
		assertProgress(t, call, 100, false)
		if call.Next != "" || call.NextAt != "" {
			t.Fatalf("one-off completion invented next metadata: %+v", call)
		}
	})
}

func runRealStatusOnly(t *testing.T, h *realChannelChatHarness, prompt, wantNext, wantNextAt string) {
	t.Helper()
	h.post(t, prompt)

	deadline := time.Now().Add(90 * time.Second)
	var snapshot channelCoverageSnapshot
	var persistedState, persistedNext, persistedNextAt string
	var persistedProgress float64
	found := false
	for time.Now().Before(deadline) {
		snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
		err := h.server.store.db.QueryRow(`
			SELECT
				COALESCE(json_extract(components_json, '$[0].props.state'), ''),
				COALESCE(json_extract(components_json, '$[0].props.progress'), -1),
				COALESCE(json_extract(components_json, '$[0].props.next'), ''),
				COALESCE(json_extract(components_json, '$[0].props.next_at'), '')
			FROM channel_chat_messages
			WHERE chat_id=? AND components_json LIKE '%"status-card"%'
			LIMIT 1`, h.chatID).Scan(&persistedState, &persistedProgress, &persistedNext, &persistedNextAt)
		if err == nil && persistedState == "completed" {
			found = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatalf("GLM-5.2 did not persist the completed status before timeout: %s", snapshot.summary())
	}
	// Keep observing beyond the late-retry window from the production duplicate
	// incident before asserting the final call and row counts.
	time.Sleep(8 * time.Second)
	snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
	if snapshot.failedResults != 0 || len(snapshot.calls) != 1 || snapshot.callCount("status", "completed") != 1 {
		t.Fatalf("status call was not exactly once and successful: %s", snapshot.summary())
	}
	if snapshot.normalMessages != 0 || snapshot.reportRows != 0 || snapshot.approvalRows != 0 || snapshot.alertRows != 0 {
		t.Fatalf("status-only request leaked into chat or Inbox: %s", snapshot.summary())
	}
	if snapshot.statusRows != 1 || persistedState != "completed" || persistedProgress != 100 ||
		normalizeStatusNextFixture(persistedNext) != normalizeStatusNextFixture(wantNext) || persistedNextAt != wantNextAt {
		t.Fatalf("persisted status state=%q progress=%v next=%q next_at=%q snapshot=%s",
			persistedState, persistedProgress, persistedNext, persistedNextAt, snapshot.summary())
	}
}

// Status next is operator-facing natural language, not an identifier. Models
// may render the same action phrase as a sentence, so the real-LLM fixture
// ignores one terminal sentence mark while keeping every semantic field exact.
func normalizeStatusNextFixture(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	switch value[len(value)-1] {
	case '.', '!', '?':
		return strings.TrimSpace(value[:len(value)-1])
	default:
		return value
	}
}

func TestNormalizeStatusNextFixture(t *testing.T) {
	for input, want := range map[string]string{
		"Generate weekly report":    "Generate weekly report",
		"Generate weekly report.":   "Generate weekly report",
		" Generate weekly report! ": "Generate weekly report",
		"":                          "",
	} {
		if got := normalizeStatusNextFixture(input); got != want {
			t.Fatalf("normalizeStatusNextFixture(%q) = %q, want %q", input, got, want)
		}
	}
}

func runRealWithoutReport(t *testing.T, h *realChannelChatHarness, prompt string) {
	t.Helper()
	h.post(t, prompt)

	deadline := time.Now().Add(150 * time.Second)
	processed := false
	for time.Now().Before(deadline) {
		errors, err := h.server.store.QueryTelemetry(h.agent.ID, "llm.error", time.Time{}, 20)
		if err != nil {
			t.Fatalf("query LLM errors: %v", err)
		}
		if len(errors) > 0 {
			t.Fatalf("GLM-5.2 returned an LLM error: %s", string(errors[len(errors)-1].Data))
		}
		done, err := h.server.store.QueryTelemetry(h.agent.ID, "llm.done", time.Time{}, 20)
		if err != nil {
			t.Fatalf("query LLM completion: %v", err)
		}
		if len(done) > 0 {
			processed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !processed {
		t.Fatal("GLM-5.2 did not complete an LLM turn before timeout")
	}
	// Reports are normally chosen in the completed turn. Keep observing past
	// the duplicate retry window in case another Channels result wakes the agent.
	time.Sleep(8 * time.Second)
	snapshot := readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
	if snapshot.failedResults != 0 {
		t.Fatalf("routine work produced failed channel calls: %s", snapshot.summary())
	}
	if snapshot.callCount("report", "") != 0 || snapshot.reportRows != 0 {
		t.Fatalf("routine daytime work incorrectly produced a report: %s", snapshot.summary())
	}
}

// TestChannelChat_RealLLM_OpenCodeKimiK27Code_AllChannelKindsExactlyOnce runs
// the same contract through Kimi K2.7 Code, pinned across every model tier.
func TestChannelChat_RealLLM_OpenCodeKimiK27Code_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupOpenCodeChannelChatHarness(t, "channel-kinds-kimi-k27-code-under-test", "kimi-k2.7-code")
	runRealChannelKindsExactlyOnce(t, h)
}

// TestChannelChat_RealLLM_OpenCodeMiniMaxM3_AllChannelKindsExactlyOnce runs
// the same contract through MiniMax M3, pinned across every model tier.
func TestChannelChat_RealLLM_OpenCodeMiniMaxM3_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupOpenCodeChannelChatHarness(t, "channel-kinds-minimax-m3-under-test", "minimax-m3")
	runRealChannelKindsExactlyOnce(t, h)
}

// TestChannelChat_RealLLM_OpenCodeQwen37Max_AllChannelKindsExactlyOnce runs
// the same contract through Qwen 3.7 Max, pinned across every model tier.
func TestChannelChat_RealLLM_OpenCodeQwen37Max_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupOpenCodeChannelChatHarness(t, "channel-kinds-qwen37-max-under-test", "qwen3.7-max")
	runRealChannelKindsExactlyOnce(t, h)
}

// TestChannelChat_RealLLM_OpenCodeMiMoV25Pro_AllChannelKindsExactlyOnce runs
// the same contract through MiMo V2.5 Pro, pinned across every model tier.
func TestChannelChat_RealLLM_OpenCodeMiMoV25Pro_AllChannelKindsExactlyOnce(t *testing.T) {
	h := setupOpenCodeChannelChatHarness(t, "channel-kinds-mimo-v2.5-pro-under-test", "mimo-v2.5-pro")
	runRealChannelKindsExactlyOnce(t, h)
}

func setupOpenCodeChannelChatHarness(t *testing.T, agentName, model string) *realChannelChatHarness {
	t.Helper()
	return setupOpenCodeChannelChatHarnessWithDirective(t, agentName, model, channelCoverageDirective())
}

func setupOpenCodeChannelChatHarnessWithDirective(t *testing.T, agentName, model, directive string) *realChannelChatHarness {
	t.Helper()
	return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, agentName, model, directive,
		`{"include_apteva_server":false,"include_channels":true}`)
}

func setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t *testing.T, agentName, model, directive, config string) *realChannelChatHarness {
	t.Helper()
	key := loadOpenCodeGoAPIKey(t)
	providerState := map[string]any{
		"OPENCODE_GO_API_KEY": key,
		"model_large":         model,
		"model_medium":        model,
		"model_small":         model,
	}
	return setupRealChannelChatHarnessWithProvider(t, agentName, directive,
		config,
		13, "llm", "OpenCode Go", providerState)
}

func setupXAIChannelChatHarnessWithDirective(t *testing.T, agentName, directive string) *realChannelChatHarness {
	t.Helper()
	model := strings.TrimSpace(os.Getenv("XAI_TEST_MODEL"))
	if model == "" {
		model = "grok-4.3"
	}
	providerState := map[string]any{
		"XAI_API_KEY": loadXAIAPIKey(t),
		"model_large": model, "model_medium": model, "model_small": model,
	}
	return setupRealChannelChatHarnessWithProvider(t, agentName, directive,
		`{"include_apteva_server":false,"include_channels":true}`,
		17, "llm", "xAI", providerState)
}

func channelCoverageDirective() string {
	return strings.Join([]string{
		"# Role",
		"You are an automated Apteva channel protocol test agent.",
		"# Protocol",
		"When asked to run channel coverage, follow the requested calls exactly and do not add preliminary chat messages.",
		"Never repeat a successful channel tool call. If one parallel call fails, retry only that failed call.",
		"After all requested calls succeed, use done or pace without sending another message.",
	}, "\n")
}

func runRealChannelKindsExactlyOnce(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	h.post(t, strings.Join([]string{
		"Run the channel coverage protocol now.",
		"Treat the protocol as one meaningful work phase. Follow your normal status guidance to establish exactly one working status before publishing artifacts; do not add a second working status for this phase.",
		"After that working status succeeds, make exactly these five calls together in one parallel tool batch:",
		"1. channels_set_status with title=Channel coverage, state=completed, detail=Protocol coverage complete, progress=100.",
		"2. channels_publish with kind=report, title=Channel Coverage Report, content=All requested channel artifacts were emitted, period=today.",
		"3. channels_publish with kind=approval, title=Channel Coverage Approval, content=Approve the protocol fixture.",
		"4. channels_publish with kind=alert, title=Channel Coverage Alert, content=Protocol warning fixture, severity=warning.",
		"5. channels_send with channel=current, text=CHANNEL COVERAGE COMPLETE.",
		"Do not send any other chat message. Do not repeat successful calls.",
	}, "\n"))

	deadline := time.Now().Add(120 * time.Second)
	var settleUntil time.Time
	var snapshot channelCoverageSnapshot
	for time.Now().Before(deadline) {
		snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
		if snapshot.hasAllKinds() && settleUntil.IsZero() {
			// A duplicate retry in the production incident arrived ~3.5s after
			// the first message. Keep observing beyond that window.
			settleUntil = time.Now().Add(8 * time.Second)
		}
		if !settleUntil.IsZero() && time.Now().After(settleUntil) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if settleUntil.IsZero() {
		t.Fatalf("real LLM did not produce every channel kind before timeout: %s", snapshot.summary())
	}
	snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
	if err := snapshot.validateExactlyOnce(); err != nil {
		t.Fatal(err)
	}
}

type realChannelChatHarness struct {
	server *Server
	agent  *Agent
	chatID string
	url    string
	apiKey string
}

func setupRealChannelChatHarness(t *testing.T, agentName, directive, config string) *realChannelChatHarness {
	t.Helper()
	providerState := loadOpenAICodexProviderState(t)
	return setupRealChannelChatHarnessWithProvider(t, agentName, directive, config,
		15, "llm", "OpenAI Codex", providerState)
}

func setupRealChannelChatHarnessWithProvider(t *testing.T, agentName, directive, config string,
	providerTypeID int64, providerType, providerName string, providerState map[string]any,
) *realChannelChatHarness {
	return setupRealChannelChatHarnessWithProviderPrepared(t, agentName, directive, config,
		providerTypeID, providerType, providerName, providerState, nil)
}

func setupRealChannelChatHarnessWithProviderPrepared(t *testing.T, agentName, directive, config string,
	providerTypeID int64, providerType, providerName string, providerState map[string]any,
	prepare func(*Server, int64, *Agent),
) *realChannelChatHarness {
	t.Helper()
	corePath := findCoreBinary(t)
	s, userID, agent := setupRealServerWithProviderState(t, corePath, agentName, directive,
		providerTypeID, providerType, providerName, providerState)
	if prepare != nil {
		prepare(s, userID, agent)
	}
	agent.Config = config
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatalf("update agent config: %v", err)
	}
	var configFlags map[string]any
	_ = json.Unmarshal([]byte(config), &configFlags)
	if enabled, _ := configFlags["include_apteva_server"].(bool); enabled {
		configureRealAptevaServerGateway(t, s)
	}

	appMux := http.NewServeMux()
	reg, err := s.startApps(appMux)
	if err != nil {
		t.Fatalf("startApps: %v", err)
	}
	t.Cleanup(func() { reg.Stop(500 * time.Millisecond) })
	appSrv := httptest.NewServer(appMux)
	t.Cleanup(appSrv.Close)

	apiKey := "apt_test_channel_chat"
	if _, err := s.store.CreateAPIKey(userID, agentName, HashAPIKey(apiKey), apiKey[:8]); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	providerEnv, err := s.store.GetAllProviderEnvVars(userID, s.secret, agent.ProjectID)
	if err != nil {
		t.Fatalf("provider env: %v", err)
	}
	if _, err := s.startManagedAgent(agent, providerEnv, s.GetProviderPool(userID, agent.ProjectID)); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	if !waitForCoreListening(s.agents.GetPort(agent.ID), 30*time.Second) {
		t.Fatalf("agent core did not listen on port %d", s.agents.GetPort(agent.ID))
	}

	chatID := fmt.Sprintf("default-%d", agent.ID)
	streamReq, _ := http.NewRequest(http.MethodGet, appSrv.URL+"/apps/channel-chat/stream?chat_id="+chatID, nil)
	streamReq.Header.Set("Authorization", "Bearer "+apiKey)
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("open chat stream: %v", err)
	}
	if streamResp.StatusCode != http.StatusOK {
		body, _ := readAll(streamResp)
		t.Fatalf("open chat stream status=%d body=%s", streamResp.StatusCode, body)
	}
	t.Cleanup(func() { _ = streamResp.Body.Close() })
	time.Sleep(100 * time.Millisecond)
	return &realChannelChatHarness{server: s, agent: agent, chatID: chatID, url: appSrv.URL, apiKey: apiKey}
}

func (h *realChannelChatHarness) post(t *testing.T, content string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"content": content,
		"context": map[string]any{"source": "dashboard-floating", "title": "Test", "route": "/agents/" + itoa64(h.agent.ID)},
	})
	req, _ := http.NewRequest(http.MethodPost, h.url+"/apps/channel-chat/messages?chat_id="+h.chatID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := readAll(resp)
		t.Fatalf("post chat status=%d body=%s", resp.StatusCode, responseBody)
	}
}

type channelSendCall struct {
	ID       string
	Time     time.Time
	Kind     string
	State    string
	Text     string
	Title    string
	Detail   string
	Next     string
	NextAt   string
	Progress *float64
}

type channelSendResult struct {
	Time    time.Time
	Success bool
}

type channelCoverageSnapshot struct {
	calls               []channelSendCall
	results             map[string]channelSendResult
	failedResults       int
	failedResultDetails []string
	normalMessages      int
	markerMessages      int
	statusRows          int
	statusState         string
	reportRows          int
	approvalRows        int
	alertRows           int
}

func readChannelCoverageSnapshot(t *testing.T, s *Server, agentID int64, chatID string) channelCoverageSnapshot {
	t.Helper()
	out := channelCoverageSnapshot{results: make(map[string]channelSendResult)}
	events, err := s.store.QueryTelemetry(agentID, "tool.call", time.Time{}, 500)
	if err != nil {
		t.Fatalf("query channel calls: %v", err)
	}
	for _, ev := range events {
		var data struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Args struct {
				Kind     string `json:"kind"`
				State    string `json:"state"`
				Text     string `json:"text"`
				Title    string `json:"title"`
				Detail   string `json:"detail"`
				Next     string `json:"next"`
				NextAt   string `json:"next_at"`
				Progress any    `json:"progress"`
			} `json:"args"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			kind, ok := channelCoverageToolKind(data.Name, data.Args.Kind)
			if !ok {
				continue
			}
			out.calls = append(out.calls, channelSendCall{
				ID: data.ID, Time: ev.Time, Kind: kind, State: data.Args.State, Text: data.Args.Text,
				Title: data.Args.Title, Detail: data.Args.Detail, Next: data.Args.Next, NextAt: data.Args.NextAt,
				Progress: numericStatusProgress(data.Args.Progress),
			})
		}
	}
	results, err := s.store.QueryTelemetry(agentID, "tool.result", time.Time{}, 500)
	if err != nil {
		t.Fatalf("query channel results: %v", err)
	}
	for _, ev := range results {
		var data struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Success bool   `json:"success"`
			Result  string `json:"result"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && isChannelCoverageTool(data.Name) {
			out.results[data.ID] = channelSendResult{Time: ev.Time, Success: data.Success}
			if !data.Success {
				out.failedResults++
				out.failedResultDetails = append(out.failedResultDetails, fmt.Sprintf("%s: %s", data.ID, data.Result))
			}
		}
	}
	row := s.store.db.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE role='agent' AND COALESCE(components_json, '[]')='[]'),
			COUNT(*) FILTER (WHERE role='agent' AND RTRIM(TRIM(content), '.!?')='CHANNEL COVERAGE COMPLETE'),
			COUNT(*) FILTER (WHERE COALESCE(components_json, '[]') LIKE '%"status-card"%'),
			COUNT(*) FILTER (WHERE COALESCE(components_json, '[]') LIKE '%"report-card"%'),
			COUNT(*) FILTER (WHERE COALESCE(components_json, '[]') LIKE '%"approval-card"%'),
			COUNT(*) FILTER (WHERE COALESCE(components_json, '[]') LIKE '%"alert-card"%')
		FROM channel_chat_messages WHERE chat_id=?`, chatID)
	if err := row.Scan(&out.normalMessages, &out.markerMessages, &out.statusRows, &out.reportRows, &out.approvalRows, &out.alertRows); err != nil {
		t.Fatalf("query channel rows: %v", err)
	}
	_ = s.store.db.QueryRow(`
		SELECT COALESCE(json_extract(components_json, '$[0].props.state'), '')
		FROM channel_chat_messages
		WHERE chat_id=? AND components_json LIKE '%"status-card"%'
		ORDER BY id DESC LIMIT 1`, chatID).Scan(&out.statusState)
	return out
}

func runRealStatusSemanticTurn(t *testing.T, h *realChannelChatHarness, prompt string) []channelSendCall {
	t.Helper()
	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	h.post(t, prompt)
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
	return newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
}

func waitForInitialAgentTurn(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	waitForAgentTurnSettled(t, h, map[string]bool{}, map[string]bool{}, 3*time.Second)
}

func waitForAgentTurnSettled(t *testing.T, h *realChannelChatHarness, baselineDone, baselineResults map[string]bool, quietFor time.Duration) {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	var stableSince time.Time
	lastSignature := ""
	for time.Now().Before(deadline) {
		done := newTelemetryEvents(t, h.server, h.agent.ID, "llm.done", baselineDone)
		results := newChannelResultEvents(t, h.server, h.agent.ID, baselineResults)
		ready := len(done) > 0
		if ready && len(results) > 0 {
			ready = !done[0].Time.Before(results[0].Time)
		}
		signature := telemetryEventSignature(done) + ":" + telemetryEventSignature(results)
		if !ready || signature != lastSignature {
			stableSince = time.Time{}
			lastSignature = signature
		} else if stableSince.IsZero() {
			stableSince = time.Now()
		} else if time.Since(stableSince) >= quietFor {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("agent turn did not settle: done=%s results=%s",
		telemetryEventSignature(newTelemetryEvents(t, h.server, h.agent.ID, "llm.done", baselineDone)),
		telemetryEventSignature(newChannelResultEvents(t, h.server, h.agent.ID, baselineResults)))
}

func telemetryEventIDs(t *testing.T, s *Server, agentID int64, eventType string) map[string]bool {
	t.Helper()
	events, err := s.store.QueryTelemetry(agentID, eventType, time.Time{}, 1000)
	if err != nil {
		t.Fatalf("query %s telemetry: %v", eventType, err)
	}
	ids := make(map[string]bool, len(events))
	for _, event := range events {
		ids[event.ID] = true
	}
	return ids
}

func newTelemetryEvents(t *testing.T, s *Server, agentID int64, eventType string, baseline map[string]bool) []TelemetryEvent {
	t.Helper()
	events, err := s.store.QueryTelemetry(agentID, eventType, time.Time{}, 1000)
	if err != nil {
		t.Fatalf("query %s telemetry: %v", eventType, err)
	}
	out := make([]TelemetryEvent, 0, len(events))
	for _, event := range events {
		if !baseline[event.ID] {
			out = append(out, event)
		}
	}
	return out
}

func newChannelResultEvents(t *testing.T, s *Server, agentID int64, baseline map[string]bool) []TelemetryEvent {
	t.Helper()
	events := newTelemetryEvents(t, s, agentID, "tool.result", baseline)
	out := make([]TelemetryEvent, 0, len(events))
	for _, event := range events {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &data) == nil && isChannelCoverageTool(data.Name) {
			out = append(out, event)
		}
	}
	return out
}

func telemetryEventSignature(events []TelemetryEvent) string {
	if len(events) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d:%s:%s", len(events), events[0].ID, events[0].Time.UTC().Format(time.RFC3339Nano))
}

func newChannelCalls(t *testing.T, s *Server, agentID int64, baseline map[string]bool) []channelSendCall {
	t.Helper()
	events, err := s.store.QueryTelemetry(agentID, "tool.call", time.Time{}, 1000)
	if err != nil {
		t.Fatalf("query channel calls: %v", err)
	}
	var calls []channelSendCall
	for _, event := range events {
		if baseline[event.ID] {
			continue
		}
		var data struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Args struct {
				Kind     string `json:"kind"`
				State    string `json:"state"`
				Text     string `json:"text"`
				Title    string `json:"title"`
				Detail   string `json:"detail"`
				Next     string `json:"next"`
				NextAt   string `json:"next_at"`
				Progress any    `json:"progress"`
			} `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		kind, ok := channelCoverageToolKind(data.Name, data.Args.Kind)
		if !ok {
			continue
		}
		calls = append(calls, channelSendCall{
			ID: data.ID, Time: event.Time, Kind: kind, State: data.Args.State, Text: data.Args.Text,
			Title: data.Args.Title, Detail: data.Args.Detail, Next: data.Args.Next, NextAt: data.Args.NextAt,
			Progress: numericStatusProgress(data.Args.Progress),
		})
	}
	return calls
}

func numericStatusProgress(value any) *float64 {
	var progress float64
	switch typed := value.(type) {
	case float64:
		progress = typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return nil
		}
		progress = parsed
	default:
		return nil
	}
	return &progress
}

func assertNoStatusCalls(t *testing.T, calls []channelSendCall) {
	t.Helper()
	for _, call := range calls {
		if call.Kind == "status" {
			t.Fatalf("scenario incorrectly created work status: %+v (all calls=%+v)", call, calls)
		}
	}
}

func assertSingleStatusCall(t *testing.T, calls []channelSendCall, wantState string) channelSendCall {
	t.Helper()
	var statuses []channelSendCall
	for _, call := range calls {
		if call.Kind == "status" {
			statuses = append(statuses, call)
		}
	}
	if len(statuses) != 1 {
		t.Fatalf("status calls=%d, want exactly one %q status: %+v", len(statuses), wantState, calls)
	}
	if statuses[0].State != wantState {
		t.Fatalf("status state=%q, want %q: %+v", statuses[0].State, wantState, statuses[0])
	}
	if strings.TrimSpace(statuses[0].Title) == "" {
		t.Fatalf("status has empty title: %+v", statuses[0])
	}
	return statuses[0]
}

func assertProgress(t *testing.T, call channelSendCall, value float64, mustBeLess bool) {
	t.Helper()
	if call.Progress == nil {
		return
	}
	if mustBeLess {
		if *call.Progress >= value {
			t.Fatalf("status progress=%v, must be less than %v for unfinished work: %+v", *call.Progress, value, call)
		}
		return
	}
	if *call.Progress != value {
		t.Fatalf("status progress=%v, want %v: %+v", *call.Progress, value, call)
	}
}

func assertTitleDoesNotDescribeFutureOrWait(t *testing.T, title string) {
	t.Helper()
	lower := strings.ToLower(title)
	for _, forbidden := range []string{"waiting", "awaiting", "scheduled", "tomorrow", "next "} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("status title describes future work or waiting condition: %q", title)
		}
	}
}

func containsAnyFold(value string, candidates ...string) bool {
	lower := strings.ToLower(value)
	for _, candidate := range candidates {
		if strings.Contains(lower, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func channelCoverageToolKind(name, advertisedKind string) (string, bool) {
	switch {
	case name == "channels_send" || strings.HasSuffix(name, "_channels_send"):
		if advertisedKind == "" {
			return "message", true
		}
		return advertisedKind, true // Legacy typed send.
	case name == "channels_publish" || strings.HasSuffix(name, "_channels_publish"):
		return advertisedKind, advertisedKind != ""
	case name == "channels_set_status" || strings.HasSuffix(name, "_channels_set_status"):
		return "status", true
	default:
		return "", false
	}
}

func isChannelCoverageTool(name string) bool {
	_, ok := channelCoverageToolKind(name, "artifact")
	return ok
}

func (s channelCoverageSnapshot) callCount(kind, state string) int {
	count := 0
	for _, call := range s.calls {
		callState := call.State
		if call.Kind == "status" && callState == "" {
			// SetCurrentStatus intentionally defaults an omitted state to
			// working. Count the model call by its effective server behavior,
			// while still detecting a second explicit working call as a duplicate.
			callState = "working"
		}
		if call.Kind == kind && (state == "" || callState == state) {
			count++
		}
	}
	return count
}

func TestChannelCoverageSnapshotCountsImplicitWorkingStateWithoutHidingDuplicates(t *testing.T) {
	snapshot := channelCoverageSnapshot{calls: []channelSendCall{
		{Kind: "status"},
		{Kind: "status", State: "completed"},
	}}
	if got := snapshot.callCount("status", "working"); got != 1 {
		t.Fatalf("implicit working count=%d, want 1", got)
	}
	if got := snapshot.callCount("status", "completed"); got != 1 {
		t.Fatalf("completed count=%d, want 1", got)
	}

	snapshot.calls = append(snapshot.calls, channelSendCall{Kind: "status", State: "working"})
	if got := snapshot.callCount("status", "working"); got != 2 {
		t.Fatalf("duplicate working count=%d, want 2", got)
	}
}

func (s channelCoverageSnapshot) markerCallCount() int {
	count := 0
	for _, call := range s.calls {
		marker := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(call.Text), ".!?"))
		if call.Kind == "message" && marker == "CHANNEL COVERAGE COMPLETE" {
			count++
		}
	}
	return count
}

func (s channelCoverageSnapshot) hasAllKinds() bool {
	return s.callCount("status", "working") >= 1 && s.callCount("status", "completed") >= 1 &&
		s.callCount("report", "") >= 1 && s.callCount("approval", "") >= 1 && s.callCount("alert", "") >= 1 &&
		s.markerCallCount() >= 1 && s.markerMessages >= 1 && s.statusState == "completed" &&
		s.reportRows >= 1 && s.approvalRows >= 1 && s.alertRows >= 1
}

func (s channelCoverageSnapshot) summary() string {
	return fmt.Sprintf("calls=%+v failed=%d details=%q normal=%d marker=%d status=%d/%s report=%d approval=%d alert=%d",
		s.calls, s.failedResults, s.failedResultDetails, s.normalMessages, s.markerMessages, s.statusRows, s.statusState, s.reportRows, s.approvalRows, s.alertRows)
}

func (s channelCoverageSnapshot) validateExactlyOnce() error {
	if s.failedResults != 0 {
		return fmt.Errorf("channel tools had %d failed result(s): %s", s.failedResults, s.summary())
	}
	if s.callCount("status", "working") != 1 || s.callCount("status", "completed") != 1 || s.callCount("status", "") != 2 {
		return fmt.Errorf("status calls were not exactly working+completed once: %s", s.summary())
	}
	if s.callCount("report", "") != 1 || s.callCount("approval", "") != 1 || s.callCount("alert", "") != 1 || s.markerCallCount() != 1 {
		return fmt.Errorf("channel artifact calls were not exactly once: %s", s.summary())
	}
	var working channelSendCall
	for _, call := range s.calls {
		if call.Kind == "status" && (call.State == "" || call.State == "working") {
			working = call
			break
		}
	}
	if working.ID == "" {
		return fmt.Errorf("working status call has no correlation ID: %s", s.summary())
	}
	workingResult, ok := s.results[working.ID]
	if !ok || !workingResult.Success {
		return fmt.Errorf("working status did not have a successful correlated result: %s", s.summary())
	}
	for _, call := range s.calls {
		if call.ID == working.ID {
			continue
		}
		if call.Time.Before(workingResult.Time) {
			return fmt.Errorf("%s/%s call happened before working status succeeded: %s", call.Kind, call.State, s.summary())
		}
	}
	if s.normalMessages != 1 || s.markerMessages != 1 || s.statusRows != 1 || s.statusState != "completed" ||
		s.reportRows != 1 || s.approvalRows != 1 || s.alertRows != 1 {
		return fmt.Errorf("persisted channel rows were not exactly once: %s", s.summary())
	}
	return nil
}

func toolNames(events []TelemetryEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		var data struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		if data.Name != "" {
			out = append(out, data.Name)
		}
	}
	return out
}

func containsTool(seq []string, suffix string) bool {
	return indexTool(seq, suffix) >= 0
}

func indexTool(seq []string, suffix string) int {
	for i, name := range seq {
		if name == suffix || strings.HasSuffix(name, "_"+suffix) {
			return i
		}
	}
	return -1
}

func latestAgentChatReply(t *testing.T, s *Server, chatID string) string {
	t.Helper()
	var content string
	err := s.store.db.QueryRow(`SELECT content FROM channel_chat_messages WHERE chat_id = ? AND role = 'agent' ORDER BY id DESC LIMIT 1`, chatID).Scan(&content)
	if err != nil {
		return ""
	}
	return content
}

func looksLikeCompletionReply(text string) bool {
	s := strings.ToLower(text)
	return strings.Contains(s, "done") || strings.Contains(s, "marked") || strings.Contains(s, "completed")
}
