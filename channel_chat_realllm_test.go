package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
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
	runRealActionBeforeReply(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-action-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_ActionBeforeReply(t *testing.T) {
	runRealActionBeforeReply(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-action-glm52-under-test", "glm-5.2", directive, config)
	})
}

func runRealActionBeforeReply(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
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
				"_meta":       map[string]any{"io.apteva/wakeOnResult": "always"},
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
	h := setupHarness(t, directive,
		fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"todo","transport":"http","url":%q}]}`, todoMCP.URL))
	s, agent, chatID := h.server, h.agent, h.chatID
	baselineCalls := telemetryEventIDs(t, s, agent.ID, "tool.call")
	var baselineMessageID int64
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, chatID,
	).Scan(&baselineMessageID); err != nil {
		t.Fatalf("query baseline chat message: %v", err)
	}
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
		var finalPhaseRows int
		_ = s.store.db.QueryRow(`
			SELECT COUNT(*) FROM channel_chat_messages
			WHERE chat_id=? AND role='agent' AND id>?
			  AND COALESCE(json_extract(metadata_json, '$.phase'), 'final')='final'`,
			chatID, baselineMessageID,
		).Scan(&finalPhaseRows)
		finalPhaseTelemetry := false
		for _, event := range newTelemetryEvents(t, s, agent.ID, "tool.call", baselineCalls) {
			var data struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			phase, _ := data.Args["phase"].(string)
			if (data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")) && phase == "final" {
				finalPhaseTelemetry = true
				break
			}
		}
		if marked.Load() > 0 && finalPhaseRows > 0 && finalPhaseTelemetry && looksLikeCompletionReply(finalReply) {
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

	type lifecycleCall struct {
		name  string
		phase string
	}
	var ordered []lifecycleCall
	calls := newTelemetryEvents(t, s, agent.ID, "tool.call", baselineCalls)
	sort.Slice(calls, func(i, j int) bool { return calls[i].Time.Before(calls[j].Time) })
	for _, event := range calls {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		phase, _ := data.Args["phase"].(string)
		ordered = append(ordered, lifecycleCall{name: data.Name, phase: phase})
	}
	var visible []lifecycleCall
	markIndex := -1
	for index, call := range ordered {
		if call.name == "mark_done" || call.name == "todo_mark_done" || strings.HasSuffix(call.name, "_mark_done") {
			markIndex = index
		}
		if call.name == "channels_send" || strings.HasSuffix(call.name, "_channels_send") {
			visible = append(visible, call)
		}
	}
	if len(visible) != 2 || visible[0].phase != "acknowledgement" || visible[1].phase != "final" {
		t.Fatalf("visible lifecycle calls=%+v, want acknowledgement then final; all=%+v", visible, ordered)
	}
	firstVisible, finalVisible := -1, -1
	for index, call := range ordered {
		if call.name != "channels_send" && !strings.HasSuffix(call.name, "_channels_send") {
			continue
		}
		if firstVisible < 0 {
			firstVisible = index
		} else {
			finalVisible = index
		}
	}
	if markIndex < 0 || firstVisible < 0 || finalVisible < 0 || !(firstVisible < markIndex && markIndex < finalVisible) {
		t.Fatalf("tool order=%+v, want acknowledgement < mark_done < final", ordered)
	}

	rows, err := s.store.db.Query(`
		SELECT content, COALESCE(json_extract(metadata_json, '$.phase'), 'final')
		FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		ORDER BY id`, chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query lifecycle messages: %v", err)
	}
	defer rows.Close()
	var persistedPhases []string
	for rows.Next() {
		var content, phase string
		if err := rows.Scan(&content, &phase); err != nil {
			t.Fatal(err)
		}
		persistedPhases = append(persistedPhases, phase)
	}
	if strings.Join(persistedPhases, ",") != "acknowledgement,final" {
		t.Fatalf("persisted phases=%v, want acknowledgement,final", persistedPhases)
	}
}

func TestChannelChat_RealLLM_Codex_ConversationUsesChildAndReportsProgress(t *testing.T) {
	runRealConversationUsesChildAndReportsProgress(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-child-progress-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_ConversationUsesChildAndReportsProgress(t *testing.T) {
	runRealConversationUsesChildAndReportsProgress(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-child-progress-glm52-under-test", "glm-5.2", directive, config)
	})
}

func TestChannelChat_RealLLM_Codex_ReportOnlyMilestoneDoesNotWait(t *testing.T) {
	runRealReportOnlyMilestoneDoesNotWait(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-report-only-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_ReportOnlyMilestoneDoesNotWait(t *testing.T) {
	runRealReportOnlyMilestoneDoesNotWait(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-report-only-glm52-under-test", "glm-5.2", directive, config)
	})
}

// runRealConversationUsesChildAndReportsProgress proves the server-created
// conversation is a capable leader: it delegates one substantial, self-
// contained workstream to a temporary child, reports meaningful progress to
// the user, consumes the child result, and finishes without polluting main.
func runRealConversationUsesChildAndReportsProgress(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	directive := strings.Join([]string{
		"# Role",
		"You prepare concise launch plans for the operator.",
		"# Working Rules",
		"Self-contained launch plans are interactive conversation work and do not need main.",
		"When the operator explicitly requests a temporary child for a separate workstream, create exactly one child, use its result, and remain responsible for the user-facing response.",
	}, "\n")
	h := setupHarness(t, directive, `{"include_apteva_server":false,"include_channels":true}`)
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

	h.post(t, "Prepare a two-part launch plan. This is substantial: create one temporary child to draft the risk section while you draft the timeline. Send me a brief progress update after the child starts, then give me one final answer with clear Timeline and Risks sections. This is self-contained work.")

	conversationThreadID := "chat-" + h.chatID
	var finalReply string
	// GLM 5.2 can take several provider turns to acknowledge, spawn, receive the
	// child result, and synthesize the final. Allow enough time to distinguish
	// a missing completion contract from ordinary provider latency.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		calls := newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)
		spawned := false
		childReported := false
		for _, event := range calls {
			var data struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			if event.ThreadID == conversationThreadID && data.Name == "spawn" {
				spawned = true
			}
			target, _ := data.Args["id"].(string)
			if event.ThreadID != conversationThreadID && event.ThreadID != "main" &&
				(data.Name == "done" || (data.Name == "send" && (target == "parent" || target == conversationThreadID))) {
				childReported = true
			}
		}
		finalReply = latestAgentChatReply(t, h.server, h.chatID)
		if spawned && childReported && looksLikeLaunchPlanFinal(finalReply) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 6*time.Second)

	type visibleCall struct {
		At   time.Time
		Text string
	}
	var spawnCalls int
	var mainSends int
	var globalStatusCalls int
	var childResultAt time.Time
	var visible []visibleCall
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		target, _ := data.Args["id"].(string)
		switch {
		case isStatusToolName(data.Name):
			globalStatusCalls++
		case event.ThreadID == conversationThreadID && data.Name == "spawn":
			spawnCalls++
		case event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent"):
			mainSends++
		case event.ThreadID != conversationThreadID && event.ThreadID != "main" &&
			(data.Name == "done" || (data.Name == "send" && (target == "parent" || target == conversationThreadID))):
			if childResultAt.IsZero() || event.Time.Before(childResultAt) {
				childResultAt = event.Time
			}
		case event.ThreadID == conversationThreadID &&
			(data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")):
			text, _ := data.Args["text"].(string)
			visible = append(visible, visibleCall{At: event.Time, Text: text})
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].At.Before(visible[j].At) })
	if spawnCalls != 1 {
		t.Fatalf("conversation spawn calls=%d, want exactly one temporary child; calls=%v",
			spawnCalls, toolNames(newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)))
	}
	if mainSends != 0 {
		t.Fatalf("self-contained conversation sent %d reports or requests to main", mainSends)
	}
	if globalStatusCalls != 0 {
		t.Fatalf("self-contained conversation wrote %d global statuses; long chat progress must remain conversation-visible", globalStatusCalls)
	}
	if childResultAt.IsZero() {
		t.Fatalf("temporary child did not report a result to the conversation; calls=%v",
			toolNames(newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)))
	}
	if len(visible) < 2 || len(visible) > 4 {
		t.Fatalf("visible messages=%d, want meaningful progress plus one final without narration: %+v", len(visible), visible)
	}
	progressFound := false
	for _, message := range visible[:len(visible)-1] {
		lower := strings.ToLower(message.Text)
		if strings.Contains(lower, "risk") &&
			(strings.Contains(lower, "start") || strings.Contains(lower, "draft") ||
				strings.Contains(lower, "analy") || strings.Contains(lower, "progress")) {
			progressFound = true
		}
	}
	if !progressFound {
		t.Fatalf("conversation did not provide the requested meaningful child-work progress: %+v", visible)
	}
	final := visible[len(visible)-1]
	if !looksLikeLaunchPlanFinal(final.Text) {
		t.Fatalf("final response did not synthesize both workstreams: %q", final.Text)
	}
	if !final.At.After(childResultAt) {
		t.Fatalf("final response preceded child result: child=%s final=%s", childResultAt, final.At)
	}

	rows, err := h.server.store.db.Query(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		  AND COALESCE(components_json, '') NOT LIKE '%"status-card"%'
		ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query child-progress replies: %v", err)
	}
	defer rows.Close()
	var persisted []string
	for rows.Next() {
		var reply string
		if err := rows.Scan(&reply); err != nil {
			t.Fatal(err)
		}
		persisted = append(persisted, reply)
	}
	if len(persisted) != len(visible) {
		t.Fatalf("persisted replies=%d tool deliveries=%d: %q", len(persisted), len(visible), persisted)
	}
}

func looksLikeLaunchPlanFinal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if !strings.Contains(lower, "timeline") || !strings.Contains(lower, "risk") {
		return false
	}
	for _, progressOnly := range []string{
		"risk analysis is underway",
		"risk section is underway",
		"while i draft",
		"i'm drafting",
		"i am drafting",
		"will follow",
	} {
		if strings.Contains(lower, progressOnly) {
			return false
		}
	}
	return strings.Count(text, "\n") >= 2
}

// runRealReportOnlyMilestoneDoesNotWait exercises the non-blocking upward
// reporting path separately from durable ACTION REQUIRED handoffs. Main is
// informed once, does not reply, and the conversation still completes its
// user-facing turn immediately after the send receipt.
func runRealReportOnlyMilestoneDoesNotWait(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	directive := strings.Join([]string{
		"# Role",
		"You assess launch readiness for the operator.",
		"# Reporting",
		"When a dashboard conversation reaches a launch-readiness decision, send main exactly one REPORT ONLY milestone with the decision, then immediately give the user the result without waiting.",
		"When main receives a message beginning REPORT ONLY, take no action and send no reply.",
	}, "\n")
	h := setupHarness(t, directive, `{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	h.post(t, "Assess launch readiness: automated tests pass, but the required operator documentation is missing. Record the decision as the appropriate milestone and tell me whether we should launch.")

	conversationThreadID := "chat-" + h.chatID
	deadline := time.Now().Add(120 * time.Second)
	var reportAt, finalAt time.Time
	var reportText, finalText string
	for time.Now().Before(deadline) {
		for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
			var data struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			if json.Unmarshal(event.Data, &data) != nil || event.ThreadID != conversationThreadID {
				continue
			}
			target, _ := data.Args["id"].(string)
			switch {
			case data.Name == "send" && (target == "main" || target == "parent"):
				reportText, _ = data.Args["message"].(string)
				reportAt = event.Time
			case data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send"):
				if finalAt.IsZero() || event.Time.After(finalAt) {
					finalText, _ = data.Args["text"].(string)
					finalAt = event.Time
				}
			}
		}
		if !reportAt.IsZero() && !finalAt.IsZero() && finalAt.After(reportAt) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 5*time.Second)

	var reportCalls, mainReplies int
	var visible []string
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		target, _ := data.Args["id"].(string)
		switch {
		case event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent"):
			reportCalls++
			reportText, _ = data.Args["message"].(string)
			reportAt = event.Time
		case event.ThreadID == "main" && data.Name == "send" && target == conversationThreadID:
			mainReplies++
		case event.ThreadID == conversationThreadID &&
			(data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")):
			text, _ := data.Args["text"].(string)
			visible = append(visible, text)
			if finalAt.IsZero() || event.Time.After(finalAt) {
				finalText = text
				finalAt = event.Time
			}
		}
	}
	if reportCalls != 1 {
		t.Fatalf("report-only sends=%d, want exactly one; calls=%v",
			reportCalls, toolNames(newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)))
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(reportText)), "REPORT ONLY") {
		t.Fatalf("main message was not explicitly non-blocking: %q", reportText)
	}
	if mainReplies != 0 {
		t.Fatalf("main replied %d times to a report-only milestone", mainReplies)
	}
	if len(visible) < 1 || len(visible) > 2 {
		t.Fatalf("report-only turn visible messages=%d, want optional acknowledgement plus one final: %q", len(visible), visible)
	}
	if reportAt.IsZero() || finalAt.IsZero() || !finalAt.After(reportAt) {
		t.Fatalf("conversation did not continue after its report receipt: report=%s final=%s",
			reportAt.Format(time.RFC3339Nano), finalAt.Format(time.RFC3339Nano))
	}
	finalLower := strings.ToLower(finalText)
	if !strings.Contains(finalLower, "documentation") ||
		(!strings.Contains(finalLower, "not ready") && !strings.Contains(finalLower, "do not launch") &&
			!strings.Contains(finalLower, "should not launch") && !strings.Contains(finalLower, "blocked")) {
		t.Fatalf("launch-readiness final result is unclear: %q", finalText)
	}
}

// TestChannelChat_RealLLM_Codex_DirectChatReplyAfterLookup reproduces the
// agent 327 failure: a read-only lookup produces an ambiguous result, so the
// agent must acknowledge before the lookup, then send a visible clarification
// instead of leaving it in plain model output and pacing.
func TestChannelChat_RealLLM_Codex_DirectChatReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-reply-after-lookup-codex-under-test", directive, config)
	})
}

// TestChannelChat_RealLLM_Codex_PlatformHelperSequentialReplyAfterLookup covers
// the Build-page regression where Apteva Helper emitted duplicate answers
// around apps_list. Dashboard chat requires one concrete acknowledgement before
// the lookup and exactly one final reply after it.
func TestChannelChat_RealLLM_Codex_PlatformHelperSequentialReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealPlatformHelperChannelChatHarness(t, "platform-helper-sequential-reply-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_PlatformHelperSequentialReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodePlatformHelperChannelChatHarness(t, "platform-helper-sequential-reply-glm52-under-test", "glm-5.2", directive, config)
	})
}

// TestChannelChat_RealLLM_Codex_PlatformHelperUsesDashboardProject verifies
// the model-facing half of project scoping. The same Helper process can serve
// several project conversations, so the dashboard context—not the process
// identity—must supply the exact project_id used by a mutation tool.
func TestChannelChat_RealLLM_Codex_PlatformHelperUsesDashboardProject(t *testing.T) {
	runRealPlatformHelperUsesDashboardProject(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealPlatformHelperChannelChatHarness(t, "platform-helper-project-scope-codex-under-test", directive, config)
	})
}

// TestChannelChat_RealLLM_Codex_PlatformHelperUpdatesAgentDirectiveDirectly
// covers the Build-page regression where a Helper conversation handed an
// atomic agents_update operation to Helper main. The conversation already has
// the authoritative apteva-server tool, so it must inspect and mutate the
// target directly, then publish one final reply without a core send handoff.
// A single acknowledgement must precede those tools.
func TestChannelChat_RealLLM_Codex_PlatformHelperUpdatesAgentDirectiveDirectly(t *testing.T) {
	h := setupRealPlatformHelperChannelChatHarness(t,
		"platform-helper-direct-agent-update-codex-under-test",
		platformHelperSystemPrompt,
		`{"include_apteva_server":true,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	project, err := h.server.store.CreateProject(h.agent.UserID, "Helper Direct Update", "", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	targetDirective := strings.Join([]string{
		"# Role",
		"You are Directive Target.",
		"# Goals",
		"Carry out explicit operator requests.",
		"# Schedule",
		"- Every day at 09:00 UTC, send exactly: Daily check-in.",
	}, "\n")
	target, err := h.server.store.CreateAgent(h.agent.UserID, "Directive Target", targetDirective, "autonomous", `{}`, project.ID)
	if err != nil {
		t.Fatalf("create target agent: %v", err)
	}
	createBody, _ := json.Marshal(map[string]any{
		"project_id":    project.ID,
		"title":         "Edit target schedule",
		"agent_ids":     []int64{h.agent.ID},
		"lead_agent_id": h.agent.ID,
	})
	createReq, _ := http.NewRequest(http.MethodPost, h.url+"/apps/channel-chat/conversations", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+h.apiKey)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create Build conversation: %v", err)
	}
	createResponseBody, readErr := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	var conversation struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	decodeErr := json.Unmarshal(createResponseBody, &conversation)
	if createResp.StatusCode != http.StatusOK || readErr != nil || decodeErr != nil || conversation.ID == "" {
		t.Fatalf("create Build conversation status=%d read=%v decode=%v body=%s",
			createResp.StatusCode, readErr, decodeErr, createResponseBody)
	}
	h.chatID = conversation.ID

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	_ = h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID)

	h.postWithContext(t, fmt.Sprintf(
		"Inspect agent Directive Target (ID %d), remove all of its scheduled behavior while preserving its other sections, and tell me when it is done.", target.ID,
	), map[string]any{
		"source":       "dashboard-build",
		"route":        "/build?session=" + conversation.ID,
		"title":        conversation.Title,
		"project_id":   project.ID,
		"project_name": project.Name,
		"page_kind":    "build",
	})
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	updated, err := h.server.store.GetAgent(h.agent.UserID, target.ID)
	if err != nil {
		t.Fatalf("reload target agent: %v", err)
	}
	if strings.Contains(updated.Directive, "09:00") || strings.Contains(updated.Directive, "Daily check-in") {
		t.Fatalf("scheduled behavior remains in target directive: %s", updated.Directive)
	}
	for _, preserved := range []string{"# Role", "You are Directive Target.", "# Goals", "Carry out explicit operator requests."} {
		if !strings.Contains(updated.Directive, preserved) {
			t.Fatalf("target directive lost %q: %s", preserved, updated.Directive)
		}
	}

	conversationThreadID := "chat-" + conversation.ID
	var getAt, updateAt time.Time
	var visibleAt []time.Time
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		switch {
		case data.Name == "send" && event.ThreadID == conversationThreadID:
			t.Fatalf("Helper conversation incorrectly handed atomic agents_update to main: %s", event.Data)
		case strings.HasSuffix(data.Name, "agents_get"):
			if event.ThreadID != conversationThreadID {
				t.Fatalf("agents_get ran on %q, want %q", event.ThreadID, conversationThreadID)
			}
			getAt = event.Time
		case strings.HasSuffix(data.Name, "agents_update"):
			if event.ThreadID != conversationThreadID {
				t.Fatalf("agents_update ran on %q, want %q", event.ThreadID, conversationThreadID)
			}
			updateAt = event.Time
		case data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send"):
			if event.ThreadID == conversationThreadID {
				visibleAt = append(visibleAt, event.Time)
			}
		}
	}
	sort.Slice(visibleAt, func(i, j int) bool { return visibleAt[i].Before(visibleAt[j]) })
	if getAt.IsZero() || updateAt.IsZero() || len(visibleAt) == 0 {
		t.Fatalf("missing direct Helper sequence get=%v update=%v visible=%v calls=%v",
			getAt, updateAt, visibleAt, toolNames(newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)))
	}
	finalAt := visibleAt[len(visibleAt)-1]
	if !getAt.Before(updateAt) || !updateAt.Before(finalAt) {
		t.Fatalf("Helper sequence out of order: get=%s update=%s final=%s", getAt, updateAt, finalAt)
	}
	if len(visibleAt) != 2 {
		t.Fatalf("Helper visible replies=%d, want exactly one acknowledgement plus exactly one final", len(visibleAt))
	}
	if !visibleAt[0].Before(getAt) {
		t.Fatalf("Helper acknowledgement did not precede action tools: acknowledgement=%s get=%s", visibleAt[0], getAt)
	}

	rows, err := h.server.store.db.Query(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query Helper replies: %v", err)
	}
	defer rows.Close()
	var replies []string
	for rows.Next() {
		var reply string
		if err := rows.Scan(&reply); err != nil {
			t.Fatalf("scan Helper reply: %v", err)
		}
		replies = append(replies, reply)
	}
	if len(replies) != 2 || !containsAnyFold(replies[len(replies)-1], "removed", "no scheduled") {
		t.Fatalf("Helper replies=%q, want exactly one acknowledgement then one completed schedule-removal result", replies)
	}
}

func TestChannelChat_RealLLM_OpenCodeGLM52_PlatformHelperUsesDashboardProject(t *testing.T) {
	runRealPlatformHelperUsesDashboardProject(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodePlatformHelperChannelChatHarness(t, "platform-helper-project-scope-glm52-under-test", "glm-5.2", directive, config)
	})
}

// TestChannelChat_RealLLM_Codex_PlatformHelperIsolatesTwoProjects keeps one
// global Helper core alive while two saved Build conversations address it from
// different projects. This catches context leakage that a one-project fixture
// cannot detect: each tool call must carry its conversation's project_id.
func TestChannelChat_RealLLM_Codex_PlatformHelperIsolatesTwoProjects(t *testing.T) {
	type markerCall struct {
		Name      string `json:"name"`
		ProjectID string `json:"project_id"`
	}
	calls := make(chan markerCall, 4)
	markerMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"serverInfo":      map[string]string{"name": "project-markers", "version": "1.0.0"},
			}, "")
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{
				"name":        "project_marker_create",
				"description": "Create a named marker in the exact current dashboard project. Always pass the authoritative project_id from conversation context and report the successful receipt once.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string"},
						"project_id": map[string]any{"type": "string"},
					},
					"required": []string{"name", "project_id"},
				},
			}}}, "")
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name != "project_marker_create" {
				respond(nil, "unknown tool: "+params.Name)
				return
			}
			var call markerCall
			_ = json.Unmarshal(params.Arguments, &call)
			calls <- call
			receipt, _ := json.Marshal(map[string]any{
				"status": "created", "name": call.Name, "project_id": call.ProjectID,
			})
			respond(map[string]any{"content": []map[string]any{{"type": "text", "text": string(receipt)}}}, "")
		default:
			respond(nil, "method not found: "+req.Method)
		}
	}))
	t.Cleanup(markerMCP.Close)

	directive := strings.Join([]string{
		"# Role",
		"You are Apteva Helper. Help the operator with the project identified by each dashboard conversation.",
		"# Rules",
		"Treat each conversation's project_id as authoritative and never reuse project context from another conversation.",
		"For project mutations, call the matching tool and then report its receipt once in the current conversation.",
	}, "\n")
	h := setupRealPlatformHelperChannelChatHarness(t, "platform-helper-two-project-isolation-codex-under-test", directive,
		fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"project-markers","transport":"http","url":%q}]}`, markerMCP.URL))
	waitForInitialAgentTurn(t, h)
	originalPort := h.server.agents.GetPort(h.agent.ID)

	projectA, err := h.server.store.CreateProject(h.agent.UserID, "North Project", "", "")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	projectB, err := h.server.store.CreateProject(h.agent.UserID, "South Project", "", "")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}

	type projectCase struct {
		project *Project
		marker  string
		chatID  string
	}
	cases := []projectCase{
		{project: projectA, marker: "north-marker"},
		{project: projectB, marker: "south-marker"},
	}
	for i := range cases {
		testCase := &cases[i]
		createBody, _ := json.Marshal(map[string]any{
			"project_id": testCase.project.ID,
			"title":      "Build " + testCase.project.Name,
			"agent_ids":  []int64{h.agent.ID},
		})
		req, _ := http.NewRequest(http.MethodPost, h.url+"/apps/channel-chat/conversations", bytes.NewReader(createBody))
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatalf("create %s conversation: %v", testCase.project.Name, requestErr)
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var created struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			ProjectID string `json:"project_id"`
		}
		decodeErr := json.Unmarshal(responseBody, &created)
		if resp.StatusCode != http.StatusOK || decodeErr != nil || created.ID == "" {
			t.Fatalf("create %s conversation status=%d read=%v decode=%v body=%s", testCase.project.Name, resp.StatusCode, readErr, decodeErr, responseBody)
		}
		if created.ProjectID != testCase.project.ID {
			t.Fatalf("conversation project=%q want %q", created.ProjectID, testCase.project.ID)
		}
		testCase.chatID = created.ID
		h.chatID = created.ID
		baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
		baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
		h.postWithContext(t, "Create a project marker named "+testCase.marker+" and tell me when it is done.", map[string]any{
			"source":       "dashboard-build",
			"route":        "/build?session=" + created.ID,
			"title":        created.Title,
			"project_id":   testCase.project.ID,
			"project_name": testCase.project.Name,
			"page_kind":    "build",
		})

		select {
		case call := <-calls:
			if call.Name != testCase.marker || call.ProjectID != testCase.project.ID {
				t.Fatalf("%s tool call=%+v, want marker=%q project_id=%q", testCase.project.Name, call, testCase.marker, testCase.project.ID)
			}
		case <-time.After(90 * time.Second):
			t.Fatalf("%s conversation never called project_marker_create", testCase.project.Name)
		}
		waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
		reply := latestAgentChatReply(t, h.server, created.ID)
		if !strings.Contains(strings.ToLower(reply), strings.ToLower(testCase.marker)) {
			t.Fatalf("%s conversation did not receive its own receipt: %q", testCase.project.Name, reply)
		}
	}

	if got := h.server.agents.GetPort(h.agent.ID); got != originalPort {
		t.Fatalf("Helper core changed across project conversations: port before=%d after=%d", originalPort, got)
	}
	for _, testCase := range cases {
		other := cases[0]
		if other.chatID == testCase.chatID {
			other = cases[1]
		}
		reply := latestAgentChatReply(t, h.server, testCase.chatID)
		if strings.Contains(strings.ToLower(reply), strings.ToLower(other.marker)) {
			t.Fatalf("project conversation %s leaked the other project's marker in reply %q", testCase.project.Name, reply)
		}
	}
}

func runRealPlatformHelperUsesDashboardProject(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	const projectID = "project-storefront"
	var uninstallCalls atomic.Int64
	var uninstallAtNano atomic.Int64
	called := make(chan struct {
		InstallID string `json:"install_id"`
		ProjectID string `json:"project_id"`
	}, 2)
	appsMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"serverInfo":      map[string]string{"name": "apteva-apps", "version": "1.0.0"},
			}, "")
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{
				"name":        "apps_uninstall",
				"description": "Uninstall an app from the explicitly named project. The successful result is an authoritative receipt; do not re-list or reinterpret it.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"install_id": map[string]any{"type": "string"},
						"project_id": map[string]any{"type": "string", "description": "Must be the exact current dashboard project_id."},
					},
					"required": []string{"install_id", "project_id"},
				},
			}}}, "")
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name != "apps_uninstall" {
				respond(nil, "unknown tool: "+params.Name)
				return
			}
			var args struct {
				InstallID string `json:"install_id"`
				ProjectID string `json:"project_id"`
			}
			_ = json.Unmarshal(params.Arguments, &args)
			uninstallCalls.Add(1)
			uninstallAtNano.Store(time.Now().UnixNano())
			select {
			case called <- args:
			default:
			}
			respond(map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": `{"status":"uninstalled","app_name":"torrent","display_name":"Torrent","install_id":71,"app_id":31,"project_id":"project-storefront","version":"0.1.16"}`,
			}}}, "")
		default:
			respond(nil, "method not found: "+req.Method)
		}
	}))
	t.Cleanup(appsMCP.Close)

	directive := strings.Join([]string{
		"# Role",
		"You are Apteva Helper. Help the operator manage the project identified by dashboard context.",
		"# Rules",
		"Use project-aware tools with the exact project_id supplied by dashboard context.",
		"After a mutation returns a successful authoritative receipt, report that result once without second-guessing it.",
	}, "\n")
	config := fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"apteva-apps","transport":"http","url":%q}]}`, appsMCP.URL)
	h := setupHarness(t, directive, config)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	_ = h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID)

	h.postWithContext(t, "Uninstall Torrent install 71 from this project and tell me when it is done.", map[string]any{
		"source":       "dashboard-build",
		"title":        "Build Storefront",
		"route":        "/build",
		"project_name": "Storefront",
		"project_id":   projectID,
		"page_kind":    "build",
	})
	var receiptReply string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		receiptReply = latestAgentChatReply(t, h.server, h.chatID)
		lower := strings.ToLower(receiptReply)
		if uninstallCalls.Load() > 0 && strings.Contains(lower, "torrent") &&
			(strings.Contains(lower, "uninstall") || strings.Contains(lower, "removed")) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	select {
	case args := <-called:
		if args.InstallID != "71" || args.ProjectID != projectID {
			t.Fatalf("apps_uninstall args=%+v, want install_id=71 project_id=%s", args, projectID)
		}
	default:
		t.Fatal("Helper did not call apps_uninstall")
	}
	if uninstallCalls.Load() != 1 {
		t.Fatalf("apps_uninstall calls=%d, want exactly one", uninstallCalls.Load())
	}
	if receiptReply == "" {
		t.Fatal("Helper did not send a visible authoritative uninstall receipt")
	}

	toolAt := uninstallAtNano.Load()
	if toolAt == 0 {
		t.Fatal("apps_uninstall did not record its call time")
	}
	visibleBeforeTool := 0
	visibleAfterTool := 0
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &data) == nil && (data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")) {
			if event.Time.UnixNano() < toolAt {
				visibleBeforeTool++
			} else {
				visibleAfterTool++
			}
		}
	}
	if visibleBeforeTool != 1 || visibleAfterTool != 1 {
		t.Fatalf("channels_send sequence before_tool=%d after_tool=%d, want exactly one acknowledgement before and one final after the tool", visibleBeforeTool, visibleAfterTool)
	}
	rows, err := h.server.store.db.Query(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query Helper replies: %v", err)
	}
	defer rows.Close()
	var replies []string
	for rows.Next() {
		var reply string
		if err := rows.Scan(&reply); err != nil {
			t.Fatalf("scan Helper reply: %v", err)
		}
		replies = append(replies, reply)
	}
	if len(replies) != visibleBeforeTool+1 {
		t.Fatalf("Helper replies=%d, want optional acknowledgement plus exactly one final: %q", len(replies), replies)
	}
	reply := replies[len(replies)-1]
	lower := strings.ToLower(reply)
	if !strings.Contains(lower, "torrent") || (!strings.Contains(lower, "uninstall") && !strings.Contains(lower, "removed")) {
		t.Fatalf("Helper did not report the authoritative uninstall receipt: %q", reply)
	}
	if strings.Contains(lower, "nothing to remove") || strings.Contains(lower, "not installed") {
		t.Fatalf("Helper contradicted the successful uninstall receipt: %q", reply)
	}
}

// TestChannelChat_RealLLM_Codex_NonPrimaryConversationReply verifies the
// runtime provenance path added for durable conversations: a Channels MCP
// reply from a custom chat context must land in that conversation, never the
// agent's permanent primary chat.
func TestChannelChat_RealLLM_Codex_NonPrimaryConversationReply(t *testing.T) {
	runRealNonPrimaryConversationReply(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "conversation-routing-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_NonPrimaryConversationReply(t *testing.T) {
	runRealNonPrimaryConversationReply(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "conversation-routing-glm52-under-test", "glm-5.2", directive, config)
	})
}

// TestChannelChat_RealLLM_Codex_NonPrimaryConversationDurableHandoff proves
// that an owner command which must outlive a custom conversation is handed to
// main, persisted in main's directive, acknowledged back to the originating
// chat thread, and not persisted into the disposable conversation thread.
// The notification MCP is local and must not be called during configuration.
func TestChannelChat_RealLLM_Codex_NonPrimaryConversationDurableHandoff(t *testing.T) {
	runRealNonPrimaryConversationDurableHandoff(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "conversation-durable-handoff-codex-under-test", directive, config)
	}, true, false)
}

func TestChannelChat_RealLLM_OpenCodeGLM52_NonPrimaryConversationDurableHandoff(t *testing.T) {
	runRealNonPrimaryConversationDurableHandoff(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "conversation-durable-handoff-glm52-under-test", "glm-5.2", directive, config)
	}, false, false)
}

// This narrower test owns one invariant independently from the end-to-end UX
// test above: a disposable chat thread must recognize durable work as main's
// responsibility. It may acknowledge and hand off, but it must neither evolve
// itself nor execute the future notification. Main must perform the evolve.
func TestChannelChat_RealLLM_Codex_ConversationDelegatesDurableWorkToMain(t *testing.T) {
	runRealNonPrimaryConversationDurableHandoff(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "conversation-ownership-boundary-codex-under-test", directive, config)
	}, false, true)
}

func runRealNonPrimaryConversationDurableHandoff(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness, verifyScheduleChange, ownershipOnly bool) {
	t.Helper()
	var notificationCalls atomic.Int64
	notificationsMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"serverInfo":      map[string]string{"name": "notifications", "version": "1.0.0"},
			}, "")
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{
				"name":        "send_notification",
				"description": "Send a notification immediately. Call only when the notification is currently due; configuring future recurring behavior must not call this tool early.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
					},
					"required": []string{"text"},
				},
			}}}, "")
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Name != "send_notification" {
				respond(nil, "unknown tool: "+params.Name)
				return
			}
			notificationCalls.Add(1)
			respond(map[string]any{"content": []map[string]any{{
				"type": "text", "text": `{"status":"sent"}`,
			}}}, "")
		default:
			respond(nil, "method not found: "+req.Method)
		}
	}))
	t.Cleanup(notificationsMCP.Close)

	directive := strings.Join([]string{
		"# Role",
		"You help the operator manage reminders and notifications.",
		"# Operating Rules",
		"Send notifications only when they are currently due; never send a future notification early.",
	}, "\n")
	config := fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"notifications","transport":"http","url":%q}]}`, notificationsMCP.URL)
	h := setupHarness(t, directive, config)
	waitForInitialAgentTurn(t, h)

	createBody, _ := json.Marshal(map[string]any{
		"project_id": h.agent.ProjectID,
		"title":      "Daily notification setup",
		"agent_ids":  []int64{h.agent.ID},
	})
	createReq, _ := http.NewRequest(http.MethodPost, h.url+"/apps/channel-chat/conversations", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+h.apiKey)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create durable-handoff conversation: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create durable-handoff conversation status=%d body=%s", createResp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("decode durable-handoff conversation: id=%q err=%v", created.ID, err)
	}
	h.chatID = created.ID
	conversationThreadID := "chat-" + created.ID

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineAll := telemetryEventIDs(t, h.server, h.agent.ID, "")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	_ = h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID)

	h.post(t, "From now on, every day at 09:00 UTC, send me a notification saying exactly: Daily check-in.")

	resolver := &serverResolver{srv: h.server}
	inst, err := resolver.OwnedInstance(h.agent.UserID, h.agent.ID)
	if err != nil {
		t.Fatalf("resolve live agent: %v", err)
	}
	var mainDirective, visibleReply string
	var childToMain, mainEvolved, mainToChild, childEvolved bool
	childParentSends := map[string]bool{}
	type visibleCall struct {
		At   time.Time
		Text string
	}
	childVisibleCalls := map[string]visibleCall{}
	var childHandoffAt, mainReplyAt time.Time
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		mainDirective, _ = resolver.MainDirective(inst)
		visibleReply = latestAgentChatReply(t, h.server, h.chatID)
		for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
			var data struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			target, _ := data.Args["id"].(string)
			if event.ThreadID == conversationThreadID &&
				(data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")) {
				text, _ := data.Args["text"].(string)
				childVisibleCalls[event.ID] = visibleCall{At: event.Time, Text: text}
			}
			if event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent") {
				childParentSends[event.ID] = true
			}
			switch {
			case event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent"):
				childToMain = true
				childHandoffAt = event.Time
			case event.ThreadID == "main" && data.Name == "evolve":
				mainEvolved = true
			case event.ThreadID == "main" && data.Name == "send" && target == conversationThreadID:
				mainToChild = true
				mainReplyAt = event.Time
			case event.ThreadID == conversationThreadID && data.Name == "evolve":
				childEvolved = true
			}
		}
		directiveReady := strings.Contains(strings.ToLower(mainDirective), "daily check-in") && strings.Contains(mainDirective, "09:00")
		replyReady := false
		if !mainReplyAt.IsZero() {
			for _, call := range childVisibleCalls {
				lower := strings.ToLower(call.Text)
				if call.At.After(mainReplyAt) && (strings.Contains(lower, "daily") || strings.Contains(call.Text, "09:00")) {
					replyReady = true
					break
				}
			}
		}
		if ownershipOnly && childToMain && mainEvolved && directiveReady {
			break
		}
		if !ownershipOnly && childToMain && mainEvolved && mainToChild && directiveReady && replyReady {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
	// Re-read after the quiet window so a late visible message or
	// post-confirmation parent acknowledgement cannot escape the assertions.
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		target, _ := data.Args["id"].(string)
		if event.ThreadID == conversationThreadID &&
			(data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")) {
			text, _ := data.Args["text"].(string)
			childVisibleCalls[event.ID] = visibleCall{At: event.Time, Text: text}
		}
		if event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent") {
			childParentSends[event.ID] = true
			childToMain = true
			childHandoffAt = event.Time
		}
		if event.ThreadID == "main" && data.Name == "send" && target == conversationThreadID {
			mainReplyAt = event.Time
		}
	}
	visibleReply = latestAgentChatReply(t, h.server, h.chatID)
	ownershipFailed := !childToMain || !mainEvolved || childEvolved || len(childParentSends) != 1 || notificationCalls.Load() != 0
	fullSequenceFailed := !ownershipOnly && (!mainToChild || len(childVisibleCalls) < 1 || len(childVisibleCalls) > 3)
	if ownershipFailed || fullSequenceFailed {
		t.Logf("durable handoff state child_to_main=%v main_evolved=%v main_to_child=%v child_evolved=%v directive=%q visible_reply=%q",
			childToMain, mainEvolved, mainToChild, childEvolved, mainDirective, visibleReply)
		events, queryErr := h.server.store.QueryTelemetry(h.agent.ID, "", time.Time{}, 1000)
		if queryErr != nil {
			t.Logf("query durable handoff telemetry: %v", queryErr)
		} else {
			for i := len(events) - 1; i >= 0; i-- {
				event := events[i]
				if baselineAll[event.ID] {
					continue
				}
				switch event.Type {
				case "event.received", "llm.thinking", "llm.chunk", "llm.done", "llm.error", "tool.call", "tool.result", "thread.message", "thread.done":
					t.Logf("handoff trace time=%s thread=%s type=%s data=%s", event.Time.Format(time.RFC3339Nano), event.ThreadID, event.Type, event.Data)
				}
			}
		}
	}

	if !childToMain {
		t.Fatal("conversation thread did not hand the durable owner request to main")
	}
	if !mainEvolved {
		t.Fatal("main did not persist the recurring responsibility with evolve")
	}
	if childEvolved {
		t.Fatal("conversation thread incorrectly persisted the recurring responsibility into itself")
	}
	if len(childParentSends) != 1 {
		t.Fatalf("conversation thread sent %d main/parent messages, want exactly one durable handoff", len(childParentSends))
	}
	if !strings.Contains(strings.ToLower(mainDirective), "daily check-in") || !strings.Contains(mainDirective, "09:00") {
		t.Fatalf("main directive missing durable daily notification responsibility:\n%s", mainDirective)
	}
	if notificationCalls.Load() != 0 {
		t.Fatalf("notification tool calls=%d, want zero while configuring future work", notificationCalls.Load())
	}
	if ownershipOnly {
		return
	}
	if !mainToChild {
		t.Fatal("main did not send configuration confirmation back to the originating conversation thread")
	}
	visibleSequence := make([]visibleCall, 0, len(childVisibleCalls))
	for _, call := range childVisibleCalls {
		visibleSequence = append(visibleSequence, call)
	}
	sort.Slice(visibleSequence, func(i, j int) bool { return visibleSequence[i].At.Before(visibleSequence[j].At) })
	if len(visibleSequence) < 1 || len(visibleSequence) > 3 {
		t.Fatalf("conversation emitted %d visible channel messages, want selective progress plus one final: %+v", len(visibleSequence), visibleSequence)
	}
	finalVisible := visibleSequence[len(visibleSequence)-1]
	if mainReplyAt.IsZero() || !finalVisible.At.After(mainReplyAt) {
		t.Fatalf("final visible result was not delivered after main replied: main_reply=%s final=%s sequence=%+v",
			mainReplyAt.Format(time.RFC3339Nano), finalVisible.At.Format(time.RFC3339Nano), visibleSequence)
	}
	for _, progress := range visibleSequence[:len(visibleSequence)-1] {
		if childHandoffAt.IsZero() || mainReplyAt.IsZero() || !progress.At.Before(mainReplyAt) {
			t.Fatalf("visible progress was not delivered before main's result: progress=%s handoff=%s main_reply=%s sequence=%+v",
				progress.At.Format(time.RFC3339Nano), childHandoffAt.Format(time.RFC3339Nano), mainReplyAt.Format(time.RFC3339Nano), visibleSequence)
		}
		progressLower := strings.ToLower(strings.TrimSpace(progress.Text))
		if progressLower == "" ||
			(!strings.Contains(progressLower, "daily") && !strings.Contains(progressLower, "notification") &&
				!strings.Contains(progressLower, "schedule")) {
			t.Fatalf("durable handoff progress was not concrete: %q", progress.Text)
		}
		for _, premature := range []string{"confirmed", "has been scheduled", "is scheduled", "completed"} {
			if strings.Contains(progressLower, premature) {
				t.Fatalf("durable handoff progress claimed completion before main acted (%q): %q", premature, progress.Text)
			}
		}
	}
	visibleReplyLower := strings.ToLower(visibleReply)
	if !strings.Contains(visibleReplyLower, "daily check-in") || !strings.Contains(visibleReply, "09:00") {
		t.Fatalf("originating conversation confirmation was not clear about the schedule: %q", visibleReply)
	}
	for _, internal := range []string{"main agent", "parent thread", "conversation thread", "directive", "handoff"} {
		if strings.Contains(visibleReplyLower, internal) {
			t.Fatalf("originating conversation exposed internal coordination %q: %q", internal, visibleReply)
		}
	}
	var replyCount int
	if err := h.server.store.db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?`, created.ID, baselineMessageID).Scan(&replyCount); err != nil {
		t.Fatalf("count durable-handoff conversation replies: %v", err)
	}
	if replyCount < 1 || replyCount > 3 {
		t.Fatalf("conversation produced %d visible replies, want selective progress plus exactly one final", replyCount)
	}
	if verifyScheduleChange {
		verifyRealScheduleChange(t, h, resolver, inst, conversationThreadID, notificationCalls.Load)
		mainDirective, err = resolver.MainDirective(inst)
		if err != nil {
			t.Fatalf("read changed main directive: %v", err)
		}
	}

	deleteReq, _ := http.NewRequest(http.MethodDelete, h.url+"/apps/channel-chat/conversation?id="+created.ID, nil)
	deleteReq.Header.Set("Authorization", "Bearer "+h.apiKey)
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatalf("delete durable-handoff conversation: %v", err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete durable-handoff conversation status=%d body=%s", deleteResp.StatusCode, body)
	}
	mainDirectiveAfterDelete, err := resolver.MainDirective(inst)
	if err != nil {
		t.Fatalf("read main directive after conversation deletion: %v", err)
	}
	wantSchedule := "09:00"
	if verifyScheduleChange {
		wantSchedule = "10:00"
	}
	if !strings.Contains(strings.ToLower(mainDirectiveAfterDelete), "daily check-in") || !strings.Contains(mainDirectiveAfterDelete, wantSchedule) {
		t.Fatalf("durable responsibility disappeared with conversation deletion:\n%s", mainDirectiveAfterDelete)
	}
}

// verifyRealScheduleChange exercises the natural follow-up users actually send:
// no restatement of the durable-handoff protocol and no hint about evolve/main.
// The disposable conversation must hand the revision to main, main must replace
// the old time, and the originating conversation must receive acknowledgement
// followed by one final confirmation.
func verifyRealScheduleChange(t *testing.T, h *realChannelChatHarness, resolver *serverResolver, inst framework.InstanceInfo, conversationThreadID string, notificationCount func() int64) {
	t.Helper()
	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	var baselineMessageID int64
	_ = h.server.store.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM channel_chat_messages WHERE chat_id=?`, h.chatID).Scan(&baselineMessageID)

	h.post(t, "Change that schedule to 10:00 UTC instead.")

	type visibleCall struct {
		At   time.Time
		Text string
	}
	parentSends := map[string]bool{}
	visibleCalls := map[string]visibleCall{}
	var mainEvolved, mainReplied bool
	var directive string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		directive, _ = resolver.MainDirective(inst)
		for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
			var data struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			target, _ := data.Args["id"].(string)
			switch {
			case event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent"):
				parentSends[event.ID] = true
			case event.ThreadID == "main" && data.Name == "evolve":
				mainEvolved = true
			case event.ThreadID == "main" && data.Name == "send" && target == conversationThreadID:
				mainReplied = true
			case event.ThreadID == conversationThreadID && (data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")):
				text, _ := data.Args["text"].(string)
				visibleCalls[event.ID] = visibleCall{At: event.Time, Text: text}
			}
		}
		if len(parentSends) == 1 && mainEvolved && mainReplied && len(visibleCalls) >= 2 &&
			strings.Contains(directive, "10:00") && !strings.Contains(directive, "09:00") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
	// Re-read after the quiet window so late progress or a repeated final cannot
	// escape the assertions.
	for _, event := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		target, _ := data.Args["id"].(string)
		switch {
		case event.ThreadID == conversationThreadID && data.Name == "send" && (target == "main" || target == "parent"):
			parentSends[event.ID] = true
		case event.ThreadID == "main" && data.Name == "evolve":
			mainEvolved = true
		case event.ThreadID == "main" && data.Name == "send" && target == conversationThreadID:
			mainReplied = true
		case event.ThreadID == conversationThreadID && (data.Name == "channels_send" || strings.HasSuffix(data.Name, "_channels_send")):
			text, _ := data.Args["text"].(string)
			visibleCalls[event.ID] = visibleCall{At: event.Time, Text: text}
		}
	}

	if len(parentSends) != 1 {
		t.Fatalf("schedule change produced %d main/parent handoffs, want exactly one", len(parentSends))
	}
	if !mainEvolved || !mainReplied {
		t.Fatalf("schedule change did not complete through main: evolved=%v replied=%v directive=%q", mainEvolved, mainReplied, directive)
	}
	if !strings.Contains(strings.ToLower(directive), "daily check-in") || !strings.Contains(directive, "10:00") || strings.Contains(directive, "09:00") {
		t.Fatalf("main did not replace 09:00 with 10:00 in the durable schedule:\n%s", directive)
	}
	if notificationCount() != 0 {
		t.Fatalf("notification was sent while only changing its future schedule")
	}

	sequence := make([]visibleCall, 0, len(visibleCalls))
	for _, call := range visibleCalls {
		sequence = append(sequence, call)
	}
	sort.Slice(sequence, func(i, j int) bool { return sequence[i].At.Before(sequence[j].At) })
	if len(sequence) < 1 || len(sequence) > 4 {
		t.Fatalf("schedule change attempted %d visible replies, want selective progress plus one final with at most one boundary-suppressed paraphrase: %+v", len(sequence), sequence)
	}
	var finalText string
	for i := len(sequence) - 1; i >= 0; i-- {
		if strings.Contains(sequence[i].Text, "10:00") {
			finalText = sequence[i].Text
			break
		}
	}
	finalLower := strings.ToLower(finalText)
	if finalText == "" ||
		(!strings.Contains(finalLower, "changed") && !strings.Contains(finalLower, "updated") &&
			!strings.Contains(finalLower, "scheduled") && !strings.Contains(finalLower, "now")) {
		t.Fatalf("schedule-change final confirmation is unclear: %q", finalText)
	}
	rows, err := h.server.store.db.Query(`
		SELECT content FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>? ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query schedule-change replies: %v", err)
	}
	defer rows.Close()
	var persistedReplies []string
	for rows.Next() {
		var reply string
		if err := rows.Scan(&reply); err != nil {
			t.Fatalf("scan schedule-change reply: %v", err)
		}
		persistedReplies = append(persistedReplies, reply)
	}
	if len(persistedReplies) < 1 || len(persistedReplies) > 3 {
		t.Fatalf("schedule change persisted %d replies, want selective progress plus exactly one final: %q", len(persistedReplies), persistedReplies)
	}
}

func runRealNonPrimaryConversationReply(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	h := setupHarness(t, strings.Join([]string{
		"# Role",
		"You answer operator messages through the current Apteva channel.",
		"# Rules",
		"For a direct chat request, send exactly the requested reply once through channels_send(channel=\"current\", ...), then return idle.",
	}, "\n"), `{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	createBody, _ := json.Marshal(map[string]any{
		"project_id": h.agent.ProjectID,
		"title":      "Provider routing room",
		"agent_ids":  []int64{h.agent.ID},
	})
	req, _ := http.NewRequest(http.MethodPost, h.url+"/apps/channel-chat/conversations", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create conversation status=%d body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("decode conversation: id=%q err=%v", created.ID, err)
	}

	primaryID := h.chatID
	primaryBefore := ordinaryAgentMessageCount(t, h.server, primaryID)
	h.chatID = created.ID
	baselineAll := telemetryEventIDs(t, h.server, h.agent.ID, "")
	h.post(t, "Reply exactly CUSTOM CONVERSATION ACKNOWLEDGED. Do not perform any other work.")
	deadline := time.Now().Add(90 * time.Second)
	var reply string
	for time.Now().Before(deadline) {
		reply = latestAgentChatReply(t, h.server, created.ID)
		if strings.Contains(reply, "CUSTOM CONVERSATION ACKNOWLEDGED") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(reply, "CUSTOM CONVERSATION ACKNOWLEDGED") {
		events, queryErr := h.server.store.QueryTelemetry(h.agent.ID, "", time.Time{}, 1000)
		if queryErr != nil {
			t.Logf("query custom conversation telemetry: %v", queryErr)
		} else {
			for i := len(events) - 1; i >= 0; i-- {
				event := events[i]
				if baselineAll[event.ID] {
					continue
				}
				switch event.Type {
				case "event.received", "llm.thinking", "llm.chunk", "llm.done", "llm.error", "tool.call", "tool.result":
					t.Logf("custom conversation trace time=%s thread=%s type=%s data=%s", event.Time.Format(time.RFC3339Nano), event.ThreadID, event.Type, event.Data)
				}
			}
		}
		t.Fatalf("custom conversation reply missing: %q", reply)
	}
	if got := ordinaryAgentMessageCount(t, h.server, primaryID); got != primaryBefore {
		t.Fatalf("custom reply leaked to primary conversation: before=%d after=%d", primaryBefore, got)
	}
}

func TestChannelChat_RealLLM_OpenCodeGLM52_DirectChatReplyAfterLookup(t *testing.T) {
	runRealDirectChatReplyAfterLookup(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-reply-after-lookup-glm52-under-test", "glm-5.2", directive, config)
	})
}

func runRealDirectChatReplyAfterLookup(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	var listed atomic.Int64
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
				"_meta":       map[string]any{"io.apteva/wakeOnResult": "always"},
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
	var finalReply string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		finalReply = latestAgentChatReply(t, h.server, h.chatID)
		lower := strings.ToLower(finalReply)
		if listed.Load() > 0 && (strings.Contains(lower, "which") || strings.Contains(lower, "clarif") ||
			(strings.Contains(lower, "10:00") && strings.Contains(lower, "16:00"))) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	if listed.Load() == 0 {
		t.Fatal("agent did not inspect the ambiguous scheduled posts")
	}
	channelCalls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	sort.Slice(channelCalls, func(i, j int) bool { return channelCalls[i].Time.Before(channelCalls[j].Time) })
	var visibleMessages []channelSendCall
	finalMessages := 0
	for _, call := range channelCalls {
		if call.Kind != "message" {
			continue
		}
		visibleMessages = append(visibleMessages, call)
		lower := strings.ToLower(call.Text)
		if strings.Contains(lower, "which") || strings.Contains(lower, "clarif") ||
			(strings.Contains(lower, "10:00") && strings.Contains(lower, "16:00")) {
			finalMessages++
		}
	}
	if finalMessages != 1 {
		t.Fatalf("agent did not send exactly one clarification based on the lookup result; calls=%+v", visibleMessages)
	}
	if len(visibleMessages) != 2 ||
		visibleMessages[0].Phase != "acknowledgement" ||
		visibleMessages[1].Phase != "final" {
		t.Fatalf("channels_send lifecycle=%+v, want acknowledgement then final", visibleMessages)
	}

	rows, err := h.server.store.db.Query(`
		SELECT content, COALESCE(json_extract(metadata_json, '$.phase'), 'final')
		FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND id>?
		  AND COALESCE(components_json, '') NOT LIKE '%"status-card"%'
		ORDER BY id`, h.chatID, baselineMessageID)
	if err != nil {
		t.Fatalf("query visible chat replies after lookup: %v", err)
	}
	defer rows.Close()
	var replies, persistedPhases []string
	for rows.Next() {
		var reply, phase string
		if err := rows.Scan(&reply, &phase); err != nil {
			t.Fatalf("scan visible chat reply after lookup: %v", err)
		}
		replies = append(replies, reply)
		persistedPhases = append(persistedPhases, phase)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate visible chat replies after lookup: %v", err)
	}
	if len(replies) != len(visibleMessages) {
		t.Fatalf("persisted agent replies=%d, want acknowledgement plus exactly one final after lookup: %q", len(replies), replies)
	}
	if strings.Join(persistedPhases, ",") != "acknowledgement,final" {
		t.Fatalf("persisted phases=%v, want acknowledgement,final", persistedPhases)
	}
	reply := replies[len(replies)-1]
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
	runRealFinalReplyNotRepeatedAfterWake(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupRealChannelChatHarness(t, "chat-final-reply-once-codex-under-test", directive, config)
	})
}

func TestChannelChat_RealLLM_OpenCodeGLM52_FinalReplyNotRepeatedAfterWake(t *testing.T) {
	runRealFinalReplyNotRepeatedAfterWake(t, func(t *testing.T, directive, config string) *realChannelChatHarness {
		return setupOpenCodeChannelChatHarnessWithDirectiveAndConfig(t, "chat-final-reply-once-glm52-under-test", "glm-5.2", directive, config)
	})
}

func runRealFinalReplyNotRepeatedAfterWake(t *testing.T, setupHarness func(*testing.T, string, string) *realChannelChatHarness) {
	t.Helper()
	directive := strings.Join([]string{
		"# Role",
		"You are a helpful social assistant speaking with the operator in dashboard chat.",
		"# Rules",
		"Answer naturally and follow the injected Channels capability guidance.",
		"Do not perform unrelated work when the operator asks you to wait.",
	}, "\n")
	h := setupHarness(t, directive,
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

	h.post(t, "Wait for now.")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var visible int
		for _, call := range newChannelCalls(t, h.server, h.agent.ID, baselineCalls) {
			if call.Kind == "message" {
				visible++
			}
		}
		if visible > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
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
	if strings.TrimSpace(sends[0].Text) == "" {
		t.Fatal("channels_send produced an empty acknowledgement")
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
	var deliveryAttempts int
	if err := h.server.store.db.QueryRow(`
		SELECT d.attempts
		FROM channel_chat_deliveries d
		JOIN channel_chat_messages m ON m.id=d.message_id
		WHERE m.chat_id=? AND m.role='user' AND m.id>?
		ORDER BY m.id DESC LIMIT 1`, h.chatID, baselineMessageID).Scan(&deliveryAttempts); err != nil {
		t.Fatalf("query inbound chat delivery: %v", err)
	}
	if deliveryAttempts != 1 {
		t.Fatalf("inbound user event attempts=%d, want one", deliveryAttempts)
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
	for _, name := range calls {
		if isStatusToolName(name) {
			t.Fatalf("brief channel message incorrectly created work status; calls=%v", calls)
		}
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

// TestChannelChat_RealLLM_Codex_OfflineReplyAndAutonomousSilence validates the
// durable-chat boundary through Codex.
func TestChannelChat_RealLLM_Codex_OfflineReplyAndAutonomousSilence(t *testing.T) {
	h := setupRealChannelChatHarness(t, "offline-autonomous-silence-codex-under-test",
		offlineAutonomousSilenceDirective(), `{"include_apteva_server":false,"include_channels":true}`)
	runRealOfflineReplyAndAutonomousSilence(t, h)
}

// TestChannelChat_RealLLM_OpenCodeGLM52_OfflineReplyAndAutonomousSilence runs
// the identical durable-reply and autonomous-silence contract through GLM-5.2.
func TestChannelChat_RealLLM_OpenCodeGLM52_OfflineReplyAndAutonomousSilence(t *testing.T) {
	h := setupOpenCodeChannelChatHarnessWithDirective(t,
		"offline-autonomous-silence-glm52-under-test", "glm-5.2", offlineAutonomousSilenceDirective())
	runRealOfflineReplyAndAutonomousSilence(t, h)
}

func offlineAutonomousSilenceDirective() string {
	return strings.Join([]string{
		"# Role",
		"You monitor CRM activity when an explicit scheduled event arrives.",
		"# Rules",
		"Wait for events; do not run the CRM check merely because the agent started.",
		"Follow the injected Channels capability guidance exactly.",
		"Do not convert routine unchanged monitoring into operator chat.",
	}, "\n")
}

// runRealOfflineReplyAndAutonomousSilence asserts that a requested chat outcome
// remains durable after the UI stream disconnects, while a later autonomous
// no-change check updates status, paces, and avoids ordinary chat entirely.
func runRealOfflineReplyAndAutonomousSilence(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	waitForInitialAgentTurn(t, h)
	h.closeChatStream(t)

	// A direct chat request remains replyable even though no SSE subscriber is
	// listening by the time the agent handles it.
	directBaselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	directBaselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	directBaselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBeforeDirect := ordinaryAgentMessageCount(t, h.server, h.chatID)
	h.post(t, "Reply exactly OFFLINE CHAT ACKNOWLEDGED. Do not perform any other work.")
	waitForAgentTurnSettled(t, h, directBaselineDone, directBaselineResults, 8*time.Second)

	var directMessages []channelSendCall
	for _, call := range newChannelCalls(t, h.server, h.agent.ID, directBaselineCalls) {
		if call.Kind == "message" {
			directMessages = append(directMessages, call)
		}
	}
	if len(directMessages) != 1 || normalizeStatusNextFixture(directMessages[0].Text) != "OFFLINE CHAT ACKNOWLEDGED" {
		t.Fatalf("offline direct chat messages=%+v, want one durable acknowledgement", directMessages)
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBeforeDirect+1 {
		t.Fatalf("ordinary messages after offline direct reply=%d, want %d", got, ordinaryBeforeDirect+1)
	}

	// The next input is an autonomous scheduler event, not a chat turn. It
	// should replace monitoring status once and set the next wake without
	// adding an ordinary message or Inbox artifact.
	autonomousBaselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	autonomousBaselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	autonomousBaselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBeforeAutonomous := ordinaryAgentMessageCount(t, h.server, h.chatID)
	nextAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	event := strings.Join([]string{
		"[system] [scheduled]",
		"This is an autonomous scheduled event. No direct [chat] turn or requested chat outcome is pending, and no operator is connected.",
		"A meaningful multi-step daily CRM check has just completed. It inspected conversations, opportunities, and pipeline health.",
		"The CRM state is unchanged and there is no alert, approval, report, or operator decision to communicate.",
		"The nearest next responsibility is exactly: Run the next daily CRM check.",
		"That next check is due in 24 hours at " + nextAt + ".",
		"Apply the injected Channels guidance, record monitoring state if appropriate, and return idle.",
	}, "\n")
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main", event); err != nil {
		t.Fatalf("post autonomous event: %v", err)
	}
	waitForAgentTurnSettled(t, h, autonomousBaselineDone, autonomousBaselineResults, 8*time.Second)

	autonomousCalls := newChannelCalls(t, h.server, h.agent.ID, autonomousBaselineCalls)
	status := assertSingleStatusCall(t, autonomousCalls, "completed")
	if normalizeStatusNextFixture(status.Next) != "Run the next daily CRM check" || status.NextAt != nextAt {
		t.Fatalf("autonomous status next=%q next_at=%q, want daily check at %s: %+v",
			status.Next, status.NextAt, nextAt, status)
	}
	for _, call := range autonomousCalls {
		if call.Kind != "status" {
			t.Fatalf("autonomous no-change check emitted %s instead of remaining silent: %+v", call.Kind, autonomousCalls)
		}
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBeforeAutonomous {
		t.Fatalf("autonomous no-change check added ordinary chat: before=%d after=%d", ordinaryBeforeAutonomous, got)
	}
	assertSinglePaceSleep(t, newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", autonomousBaselineCalls), "24h")
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

// TestChannelChat_RealLLM_Codex_MainPersistsAutonomousStatus proves the
// main-thread monitoring path independently from dashboard conversations. A
// meaningful autonomous result must create one mutable status from main,
// persist it in the internal status sink, expose it through the status API,
// and leave the visible conversation list/history untouched.
func TestChannelChat_RealLLM_Codex_MainPersistsAutonomousStatus(t *testing.T) {
	h := setupRealChannelChatHarness(t, "main-status-codex-under-test",
		offlineAutonomousSilenceDirective(), `{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBefore := ordinaryAgentMessageCount(t, h.server, h.chatID)
	var visibleBefore int
	if err := h.server.store.db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_chats
		WHERE owner_user_id=? AND id <> printf('default-%d', agent_id)`, h.agent.UserID).Scan(&visibleBefore); err != nil {
		t.Fatalf("count visible conversations before status: %v", err)
	}

	nextAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	event := strings.Join([]string{
		"[system] [scheduled]",
		"This is an autonomous scheduled event on the agent's main thread; no direct chat request is pending.",
		"A meaningful multi-step daily CRM check has completed after inspecting conversations, opportunities, and pipeline health.",
		"The CRM state is unchanged and there is no alert, approval, report, or operator decision to communicate.",
		"The nearest next responsibility is exactly: Run the next daily CRM check.",
		"That next check is due at " + nextAt + ".",
		"Apply the injected Channels guidance, record monitoring state if appropriate, and return idle.",
	}, "\n")
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main", event); err != nil {
		t.Fatalf("post autonomous main event: %v", err)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	status := assertSingleStatusCall(t, newChannelCalls(t, h.server, h.agent.ID, baselineCalls), "completed")
	if normalizeStatusNextFixture(status.Next) != "Run the next daily CRM check" || status.NextAt != nextAt {
		t.Fatalf("main status next=%q next_at=%q, want daily check at %s: %+v", status.Next, status.NextAt, nextAt, status)
	}
	statusCallsOnMain := 0
	for _, telemetry := range newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls) {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(telemetry.Data, &data) == nil && isStatusToolName(data.Name) {
			if telemetry.ThreadID != "main" {
				t.Fatalf("status call originated from thread %q, want main", telemetry.ThreadID)
			}
			statusCallsOnMain++
		}
	}
	if statusCallsOnMain != 1 {
		t.Fatalf("main status calls=%d, want exactly one", statusCallsOnMain)
	}

	var persistedChatID, persistedThreadID, componentsJSON string
	if err := h.server.store.db.QueryRow(`
		SELECT chat_id, COALESCE(thread_id, ''), components_json
		FROM channel_chat_messages
		WHERE agent_id=? AND components_json LIKE '%"status-card"%'
		ORDER BY id DESC LIMIT 1`, h.agent.ID).Scan(&persistedChatID, &persistedThreadID, &componentsJSON); err != nil {
		t.Fatalf("read persisted main status: %v", err)
	}
	if persistedChatID != "default-"+itoa64(h.agent.ID) || persistedThreadID != "main" {
		t.Fatalf("persisted status chat=%q thread=%q, want internal default on main", persistedChatID, persistedThreadID)
	}
	if !strings.Contains(componentsJSON, `"state":"completed"`) || !strings.Contains(componentsJSON, nextAt) {
		t.Fatalf("persisted status missing completed state or next_at: %s", componentsJSON)
	}

	req, _ := http.NewRequest(http.MethodGet, h.url+"/apps/channel-chat/current-statuses", nil)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get current statuses: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("current statuses status=%d body=%s", resp.StatusCode, body)
	}
	var statuses []struct {
		AgentID int64  `json:"instance_id"`
		State   string `json:"state"`
		NextAt  string `json:"next_at"`
		Message struct {
			ChatID   string `json:"chat_id"`
			ThreadID string `json:"thread_id"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode current statuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].AgentID != h.agent.ID || statuses[0].State != "completed" ||
		statuses[0].NextAt != nextAt || statuses[0].Message.ChatID != persistedChatID || statuses[0].Message.ThreadID != "main" {
		t.Fatalf("current statuses API did not return persisted main status: %+v", statuses)
	}

	var visibleAfter int
	if err := h.server.store.db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_chats
		WHERE owner_user_id=? AND id <> printf('default-%d', agent_id)`, h.agent.UserID).Scan(&visibleAfter); err != nil {
		t.Fatalf("count visible conversations after status: %v", err)
	}
	if visibleAfter != visibleBefore {
		t.Fatalf("main status changed visible conversations: before=%d after=%d", visibleBefore, visibleAfter)
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBefore {
		t.Fatalf("main status leaked into chat history: before=%d after=%d", ordinaryBefore, got)
	}
}

// TestChannelChat_RealLLM_Codex_MainOwnsCentralReport proves that full Inbox
// reporting is available on main, persists in the hidden agent-output sink,
// and never leaks into a user conversation.
func TestChannelChat_RealLLM_Codex_MainOwnsCentralReport(t *testing.T) {
	h := setupRealChannelChatHarness(t, "main-report-codex-under-test",
		offlineAutonomousSilenceDirective(), `{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBefore := ordinaryAgentMessageCount(t, h.server, h.chatID)
	event := strings.Join([]string{
		"[system] [scheduled report]",
		"The agent directive explicitly requires its daily report now.",
		"Publish exactly one report titled Daily CRM summary.",
		"Its substantive content must say: Reviewed 12 conversations; 3 require follow-up tomorrow.",
		"Use period=today. This event requests only that central report: do not notify an external channel, update status, or send dashboard chat.",
	}, "\n")
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main", event); err != nil {
		t.Fatalf("post central report event: %v", err)
	}
	waitForChannelOutputSettled(t, h, baselineCalls, baselineResults, 8*time.Second)

	calls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	if len(calls) != 1 || calls[0].Kind != "report" ||
		calls[0].Title != "Daily CRM summary" ||
		!strings.Contains(calls[0].Text, "Reviewed 12 conversations") {
		t.Fatalf("main central report calls=%+v, want exactly one substantive report", calls)
	}
	var chatID, threadID string
	if err := h.server.store.db.QueryRow(`
		SELECT chat_id, COALESCE(thread_id, '')
		FROM channel_chat_messages
		WHERE agent_id=? AND components_json LIKE '%"report-card"%'
		ORDER BY id DESC LIMIT 1`, h.agent.ID).Scan(&chatID, &threadID); err != nil {
		t.Fatalf("read persisted central report: %v", err)
	}
	if chatID != "default-"+itoa64(h.agent.ID) || threadID != "main" {
		t.Fatalf("central report chat=%q thread=%q, want hidden default sink on main", chatID, threadID)
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBefore {
		t.Fatalf("central report leaked into conversation: before=%d after=%d", ordinaryBefore, got)
	}
}

// TestChannelChat_RealLLM_Codex_MainRecurringMonitorEmitsStatus reproduces
// the Personal Agent's hourly inbox cycle without explicitly asking Codex to
// update status. The recurring-monitor rule itself must cause one completed
// status on main, followed by the next hourly pace, with no chat or Inbox item.
func TestChannelChat_RealLLM_Codex_MainRecurringMonitorEmitsStatus(t *testing.T) {
	directive := strings.Join([]string{
		"# Role",
		"You keep the operator's Gmail inbox clean.",
		"# Hourly routine",
		"When idle, check unread Gmail messages about once every hour.",
		"Inspect and classify unread messages, mark processed routine messages read, and alert only when operator attention is needed.",
	}, "\n")
	h := setupRealChannelChatHarness(t, "main-recurring-status-codex-under-test", directive,
		`{"include_apteva_server":false,"include_channels":true}`)
	waitForInitialAgentTurn(t, h)

	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBefore := ordinaryAgentMessageCount(t, h.server, h.chatID)
	nextAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)

	event := strings.Join([]string{
		"[system] [scheduled]",
		"The due hourly Gmail inbox cycle has completed.",
		"It checked for unread messages and found none.",
		"The next hourly cycle is scheduled for " + nextAt + ".",
		"Continue the normal hourly monitoring routine.",
	}, "\n")
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main", event); err != nil {
		t.Fatalf("post recurring monitor event: %v", err)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	events := newTelemetryEvents(t, h.server, h.agent.ID, "tool.call", baselineCalls)
	calls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	if len(calls) == 0 {
		var completions []string
		for _, telemetry := range newTelemetryEvents(t, h.server, h.agent.ID, "llm.done", baselineDone) {
			var data struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(telemetry.Data, &data) == nil {
				completions = append(completions, data.Message)
			}
		}
		t.Logf("recurring monitor emitted no channel calls; tools=%v completions=%q", toolNames(events), completions)
	}
	status := assertSingleStatusCall(t, calls, "completed")
	if strings.TrimSpace(status.Detail) == "" {
		t.Fatalf("recurring monitor status has no concrete result: %+v", status)
	}
	next := strings.ToLower(strings.TrimSpace(status.Next))
	if next == "" || (!strings.Contains(next, "gmail") && !strings.Contains(next, "inbox")) {
		t.Fatalf("recurring monitor status does not describe the next inbox cycle: %+v", status)
	}
	if status.NextAt != nextAt {
		t.Fatalf("recurring monitor next_at=%q, want %q: %+v", status.NextAt, nextAt, status)
	}
	for _, call := range calls {
		if call.Kind != "status" {
			t.Fatalf("recurring no-change monitor emitted %s instead of remaining silent: %+v", call.Kind, calls)
		}
	}

	statusCallsOnMain := 0
	for _, telemetry := range events {
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(telemetry.Data, &data) == nil && isStatusToolName(data.Name) {
			if telemetry.ThreadID != "main" {
				t.Fatalf("recurring monitor status originated from thread %q, want main", telemetry.ThreadID)
			}
			statusCallsOnMain++
		}
	}
	if statusCallsOnMain != 1 {
		t.Fatalf("main recurring status calls=%d, want exactly one", statusCallsOnMain)
	}
	assertSinglePaceNearDuration(t, events, time.Hour, 2*time.Minute)
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBefore {
		t.Fatalf("recurring monitor leaked into chat: before=%d after=%d", ordinaryBefore, got)
	}

	req, _ := http.NewRequest(http.MethodGet, h.url+"/apps/channel-chat/current-statuses", nil)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get recurring current status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("recurring current statuses status=%d body=%s", resp.StatusCode, body)
	}
	var statuses []struct {
		AgentID int64  `json:"instance_id"`
		State   string `json:"state"`
		Next    string `json:"next"`
		NextAt  string `json:"next_at"`
		Message struct {
			ThreadID string `json:"thread_id"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode recurring current status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].AgentID != h.agent.ID ||
		statuses[0].State != "completed" || strings.TrimSpace(statuses[0].Next) == "" ||
		statuses[0].NextAt != nextAt || statuses[0].Message.ThreadID != "main" {
		t.Fatalf("persisted recurring status missing next action/time on main: %+v", statuses)
	}
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
		"On main, you maintain the agent's single operator-visible state while carrying out autonomous work.",
		"# Status protocol",
		"Apply the injected Channels capability guidance exactly.",
		"Use status only for meaningful operator-relevant work units, not your own administration or future-only scheduling.",
		"These monitoring events are internal main-thread events. Do not externally notify or publish Inbox artifacts.",
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

	t.Run("long-running-work-reports-working-progress", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A meaningful long-running CRM contact import is actively processing.",
			"It has imported 40 of 100 contacts successfully and is continuing with the remaining records.",
			"Update operator monitoring for this active work unit.",
			"The nearest next responsibility is exactly: Import the remaining 60 contacts.",
			"There is no deadline.",
		}, "\n"))
		call := assertSingleStatusCall(t, calls, "working")
		if call.Progress == nil || *call.Progress != 40 {
			t.Fatalf("working import progress=%v, want exactly 40: %+v", call.Progress, call)
		}
		if normalizeStatusNextFixture(call.Next) != "Import the remaining 60 contacts" || call.NextAt != "" {
			t.Fatalf("working import next=%q next_at=%q, want remaining work without invented time: %+v",
				call.Next, call.NextAt, call)
		}
		assertTitleDoesNotDescribeFutureOrWait(t, call.Title)
		if len(calls) != 1 || calls[0].Kind != "status" {
			t.Fatalf("main working progress calls=%+v, want exactly one global status and no conversation output", calls)
		}
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
		if normalizeStatusNextFixture(call.Next) != normalizeStatusNextFixture("Send requested notification.") || call.NextAt != "2026-07-20T10:00:00Z" {
			t.Fatalf("delayed status did not preserve exact next action/time: %+v", call)
		}
		assertTitleDoesNotDescribeFutureOrWait(t, call.Title)
	})

	t.Run("one-off-completion-clears-next", func(t *testing.T) {
		calls := runRealStatusSemanticTurn(t, h, strings.Join([]string{
			"A one-off cleanup of temporary test records just completed successfully.",
			"The previously requested delayed notification has been cancelled and is no longer pending.",
			"Update operator monitoring for the completed work. There is no follow-up responsibility or other planned work.",
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
	waitForInitialAgentTurn(t, h)
	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBefore := ordinaryAgentMessageCount(t, h.server, h.chatID)
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main",
		"[system] [monitoring]\n"+prompt); err != nil {
		t.Fatalf("post main status event: %v", err)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)

	calls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	status := assertSingleStatusCall(t, calls, "completed")
	if status.Progress == nil || *status.Progress != 100 {
		t.Fatalf("completed status progress=%v, want 100: %+v", status.Progress, status)
	}
	if normalizeStatusNextFixture(status.Next) != normalizeStatusNextFixture(wantNext) || status.NextAt != wantNextAt {
		t.Fatalf("status next=%q next_at=%q, want %q at %q: %+v", status.Next, status.NextAt, wantNext, wantNextAt, status)
	}
	for _, call := range calls {
		if call.Kind != "status" {
			t.Fatalf("main status-only event emitted %s: %+v", call.Kind, calls)
		}
	}

	var persistedState, persistedNext, persistedNextAt string
	var persistedProgress float64
	if err := h.server.store.db.QueryRow(`
		SELECT
			COALESCE(json_extract(components_json, '$[0].props.state'), ''),
			COALESCE(json_extract(components_json, '$[0].props.progress'), -1),
			COALESCE(json_extract(components_json, '$[0].props.next'), ''),
			COALESCE(json_extract(components_json, '$[0].props.next_at'), '')
		FROM channel_chat_messages
		WHERE chat_id=? AND components_json LIKE '%"status-card"%'
		LIMIT 1`, "default-"+itoa64(h.agent.ID)).Scan(
		&persistedState, &persistedProgress, &persistedNext, &persistedNextAt); err != nil {
		t.Fatalf("read persisted main status: %v", err)
	}
	if persistedState != "completed" || persistedProgress != 100 ||
		normalizeStatusNextFixture(persistedNext) != normalizeStatusNextFixture(wantNext) || persistedNextAt != wantNextAt {
		t.Fatalf("persisted status state=%q progress=%v next=%q next_at=%q",
			persistedState, persistedProgress, persistedNext, persistedNextAt)
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBefore {
		t.Fatalf("main status-only event leaked into conversation: before=%d after=%d", ordinaryBefore, got)
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
	waitForInitialAgentTurn(t, h)
	baselineCalls := telemetryEventIDs(t, h.server, h.agent.ID, "tool.call")
	baselineDone := telemetryEventIDs(t, h.server, h.agent.ID, "llm.done")
	baselineResults := telemetryEventIDs(t, h.server, h.agent.ID, "tool.result")
	ordinaryBefore := ordinaryAgentMessageCount(t, h.server, h.chatID)
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main",
		"[system] [scheduled]\n"+prompt); err != nil {
		t.Fatalf("post routine main event: %v", err)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
	calls := newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
	assertSingleStatusCall(t, calls, "completed")
	for _, call := range calls {
		if call.Kind != "status" {
			t.Fatalf("routine daytime work emitted %s instead of status only: %+v", call.Kind, calls)
		}
	}
	if got := ordinaryAgentMessageCount(t, h.server, h.chatID); got != ordinaryBefore {
		t.Fatalf("routine daytime work leaked into chat: before=%d after=%d", ordinaryBefore, got)
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
		"Run the user-conversation output coverage protocol now.",
		"Make exactly these three calls together in one parallel tool batch:",
		"1. channels_publish with kind=approval, title=Channel Coverage Approval, content=Approve the protocol fixture.",
		"2. channels_publish with kind=alert, title=Channel Coverage Alert, content=Protocol warning fixture, severity=warning.",
		"3. channels_send with channel=current, phase=final, text=CHANNEL COVERAGE COMPLETE.",
		"Global status and reports belong to main. Do not attempt either from this conversation.",
		"Do not send any other chat message. Do not repeat successful calls.",
	}, "\n"))

	// A provider turn can occasionally sit queued for more than two minutes
	// before emitting its first tool call. Keep the real-model deadline aligned
	// with the other multi-step channel tests so queue latency is not mistaken
	// for a protocol failure.
	deadline := time.Now().Add(5 * time.Minute)
	var settleUntil time.Time
	var snapshot channelCoverageSnapshot
	for time.Now().Before(deadline) {
		snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
		if snapshot.hasConversationOutputs() && settleUntil.IsZero() {
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
		t.Fatalf("real LLM did not produce every conversation-owned output before timeout: %s", snapshot.summary())
	}
	snapshot = readChannelCoverageSnapshot(t, h.server, h.agent.ID, h.chatID)
	if err := snapshot.validateConversationOutputsExactlyOnce(); err != nil {
		t.Fatal(err)
	}
}

type realChannelChatHarness struct {
	server     *Server
	agent      *Agent
	chatID     string
	url        string
	apiKey     string
	streamBody io.ReadCloser
}

func setupRealChannelChatHarness(t *testing.T, agentName, directive, config string) *realChannelChatHarness {
	t.Helper()
	providerState := loadOpenAICodexProviderState(t)
	return setupRealChannelChatHarnessWithProvider(t, agentName, directive, config,
		15, "llm", "OpenAI Codex", providerState)
}

func setupRealPlatformHelperChannelChatHarness(t *testing.T, agentName, directive, config string) *realChannelChatHarness {
	t.Helper()
	providerState := loadOpenAICodexProviderState(t)
	return setupRealChannelChatHarnessWithProviderPrepared(t, agentName, directive, config,
		15, "llm", "OpenAI Codex", providerState,
		func(s *Server, _ int64, agent *Agent) {
			agent.Kind = "platform_helper"
			if _, err := s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, agent.ID); err != nil {
				t.Fatalf("persist platform helper kind: %v", err)
			}
		})
}

func setupOpenCodePlatformHelperChannelChatHarness(t *testing.T, agentName, model, directive, config string) *realChannelChatHarness {
	t.Helper()
	providerState := map[string]any{
		"OPENCODE_GO_API_KEY": loadOpenCodeGoAPIKey(t),
		"model_large":         model,
		"model_medium":        model,
		"model_small":         model,
	}
	return setupRealChannelChatHarnessWithProviderPrepared(t, agentName, directive, config,
		13, "llm", "OpenCode Go", providerState,
		func(s *Server, _ int64, agent *Agent) {
			agent.Kind = "platform_helper"
			if _, err := s.store.db.Exec(`UPDATE agents SET kind='platform_helper' WHERE id=?`, agent.ID); err != nil {
				t.Fatalf("persist platform helper kind: %v", err)
			}
		})
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

	createBody, _ := json.Marshal(map[string]any{"agent_id": agent.ID, "title": "Test conversation"})
	createReq, _ := http.NewRequest(http.MethodPost, appSrv.URL+"/apps/channel-chat/chats", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+apiKey)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create explicit test conversation: %v", err)
	}
	if createResp.StatusCode != http.StatusOK {
		body, _ := readAll(createResp)
		t.Fatalf("create explicit test conversation status=%d body=%s", createResp.StatusCode, body)
	}
	var createdConversation struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&createdConversation); err != nil {
		_ = createResp.Body.Close()
		t.Fatalf("decode explicit test conversation: %v", err)
	}
	_ = createResp.Body.Close()
	chatID := createdConversation.ID
	if !strings.HasPrefix(chatID, "conv-") {
		t.Fatalf("explicit test conversation id=%q, want conv-*", chatID)
	}
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
	return &realChannelChatHarness{
		server: s, agent: agent, chatID: chatID, url: appSrv.URL, apiKey: apiKey, streamBody: streamResp.Body,
	}
}

func (h *realChannelChatHarness) closeChatStream(t *testing.T) {
	t.Helper()
	if h.streamBody == nil {
		return
	}
	if err := h.streamBody.Close(); err != nil {
		t.Fatalf("close chat stream: %v", err)
	}
	h.streamBody = nil
}

func (h *realChannelChatHarness) post(t *testing.T, content string) {
	t.Helper()
	h.postWithContext(t, content, map[string]any{
		"source": "dashboard-floating",
		"title":  "Test",
		"route":  "/agents/" + itoa64(h.agent.ID),
	})
}

func (h *realChannelChatHarness) postWithContext(t *testing.T, content string, context map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"content": content,
		"context": context,
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
	Phase    string
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
				Phase    string `json:"phase"`
				State    string `json:"state"`
				Text     string `json:"text"`
				Content  string `json:"content"`
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
	if err := postCoreEvent(t.Context(), h.server.agents.GetPort(h.agent.ID),
		h.server.agents.GetCoreAPIKey(h.agent.ID), "main",
		"[system] [monitoring]\n"+prompt); err != nil {
		t.Fatalf("post main status semantic event: %v", err)
	}
	waitForAgentTurnSettled(t, h, baselineDone, baselineResults, 8*time.Second)
	return newChannelCalls(t, h.server, h.agent.ID, baselineCalls)
}

func waitForInitialAgentTurn(t *testing.T, h *realChannelChatHarness) {
	t.Helper()
	waitForAgentTurnSettled(t, h, map[string]bool{}, map[string]bool{}, 3*time.Second)
}

func waitForAgentTurnSettled(t *testing.T, h *realChannelChatHarness, baselineDone, baselineResults map[string]bool, quietFor time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
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

// waitForChannelOutputSettled is for terminal main-owned output such as an
// explicitly requested report. Its durable tool receipt completes the work;
// unlike an acknowledgement or action result, it does not owe a later model
// round. Requiring llm.done after the receipt made a successful publication
// look hung when core correctly left main idle.
func waitForChannelOutputSettled(t *testing.T, h *realChannelChatHarness, baselineCalls, baselineResults map[string]bool, quietFor time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	var stableSince time.Time
	lastSignature := ""
	for time.Now().Before(deadline) {
		calls := newChannelCallEvents(t, h.server, h.agent.ID, baselineCalls)
		results := newChannelResultEvents(t, h.server, h.agent.ID, baselineResults)
		ready := len(calls) > 0 && len(results) >= len(calls)
		signature := telemetryEventSignature(calls) + ":" + telemetryEventSignature(results)
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
	t.Fatalf("main channel output did not settle: calls=%s results=%s",
		telemetryEventSignature(newChannelCallEvents(t, h.server, h.agent.ID, baselineCalls)),
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

func newChannelCallEvents(t *testing.T, s *Server, agentID int64, baseline map[string]bool) []TelemetryEvent {
	t.Helper()
	events := newTelemetryEvents(t, s, agentID, "tool.call", baseline)
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
				Phase    string `json:"phase"`
				State    string `json:"state"`
				Text     string `json:"text"`
				Content  string `json:"content"`
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
		text := data.Args.Text
		if text == "" {
			text = data.Args.Content
		}
		calls = append(calls, channelSendCall{
			ID: data.ID, Time: event.Time, Kind: kind, Phase: data.Args.Phase, State: data.Args.State, Text: text,
			Title: data.Args.Title, Detail: data.Args.Detail, Next: data.Args.Next, NextAt: data.Args.NextAt,
			Progress: numericStatusProgress(data.Args.Progress),
		})
	}
	return calls
}

func ordinaryAgentMessageCount(t *testing.T, s *Server, chatID string) int {
	t.Helper()
	var count int
	if err := s.store.db.QueryRow(`
		SELECT COUNT(*) FROM channel_chat_messages
		WHERE chat_id=? AND role='agent' AND COALESCE(components_json, '[]')='[]'`, chatID).Scan(&count); err != nil {
		t.Fatalf("count ordinary agent messages: %v", err)
	}
	return count
}

func assertSinglePaceSleep(t *testing.T, events []TelemetryEvent, wantSleep string) {
	t.Helper()
	var sleeps []string
	for _, event := range events {
		var data struct {
			Name string `json:"name"`
			Args struct {
				Sleep string `json:"sleep"`
			} `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Name == "pace" {
			sleeps = append(sleeps, data.Args.Sleep)
		}
	}
	if len(sleeps) != 1 || sleeps[0] != wantSleep {
		if len(sleeps) == 1 {
			gotDuration, gotErr := time.ParseDuration(sleeps[0])
			wantDuration, wantErr := time.ParseDuration(wantSleep)
			if gotErr == nil && wantErr == nil && gotDuration == wantDuration {
				return
			}
		}
		t.Fatalf("pace sleeps=%v, want exactly one duration equivalent to %s", sleeps, wantSleep)
	}
}

func assertSinglePaceNearDuration(t *testing.T, events []TelemetryEvent, want, tolerance time.Duration) {
	t.Helper()
	var sleeps []string
	for _, event := range events {
		var data struct {
			Name string `json:"name"`
			Args struct {
				Sleep string `json:"sleep"`
			} `json:"args"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Name == "pace" {
			sleeps = append(sleeps, data.Args.Sleep)
		}
	}
	if len(sleeps) != 1 {
		t.Fatalf("pace sleeps=%v, want exactly one duration within %s of %s", sleeps, tolerance, want)
	}
	got, err := time.ParseDuration(sleeps[0])
	if err != nil || got < want-tolerance || got > want+tolerance {
		t.Fatalf("pace sleep=%q, want one duration within %s of %s", sleeps[0], tolerance, want)
	}
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
	case name == "channels_publish" || strings.HasSuffix(name, "_channels_publish") || strings.HasSuffix(name, "_publish"):
		return advertisedKind, advertisedKind != ""
	case isStatusToolName(name):
		return "status", true
	default:
		return "", false
	}
}

func isStatusToolName(name string) bool {
	return name == "channels_set_status" ||
		strings.HasSuffix(name, "_channels_set_status") ||
		strings.HasSuffix(name, "_set_status")
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

func (s channelCoverageSnapshot) hasConversationOutputs() bool {
	return s.callCount("approval", "") >= 1 && s.callCount("alert", "") >= 1 &&
		s.markerCallCount() >= 1 && s.markerMessages >= 1 &&
		s.approvalRows >= 1 && s.alertRows >= 1
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

func (s channelCoverageSnapshot) validateConversationOutputsExactlyOnce() error {
	if s.failedResults != 0 {
		return fmt.Errorf("conversation output tools had %d failed result(s): %s", s.failedResults, s.summary())
	}
	if s.callCount("approval", "") != 1 || s.callCount("alert", "") != 1 || s.markerCallCount() != 1 {
		return fmt.Errorf("conversation approval, alert, or final reply was not exactly once: %s", s.summary())
	}
	if s.callCount("status", "") != 0 || s.callCount("report", "") != 0 {
		return fmt.Errorf("conversation attempted main-owned status or report: %s", s.summary())
	}
	if s.normalMessages != 1 || s.markerMessages != 1 || s.statusRows != 0 ||
		s.reportRows != 0 || s.approvalRows != 1 || s.alertRows != 1 {
		return fmt.Errorf("persisted conversation output rows were not exactly once and role-scoped: %s", s.summary())
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
