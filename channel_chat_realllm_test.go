package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	requireRealLLMTests(t)
	key := strings.TrimSpace(os.Getenv("OPENCODE_GO_API_KEY"))
	if key == "" {
		t.Skip("set OPENCODE_GO_API_KEY to run the OpenCode Go channel test")
	}
	providerState := map[string]any{
		"OPENCODE_GO_API_KEY": key,
		"model_large":         model,
		"model_medium":        model,
		"model_small":         model,
	}
	return setupRealChannelChatHarnessWithProvider(t, agentName, channelCoverageDirective(),
		`{"include_apteva_server":false,"include_channels":true}`,
		13, "llm", "OpenCode Go", providerState)
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
	t.Helper()
	corePath := findCoreBinary(t)
	s, userID, agent := setupRealServerWithProviderState(t, corePath, agentName, directive,
		providerTypeID, providerType, providerName, providerState)
	agent.Config = config
	if err := s.store.UpdateAgent(agent); err != nil {
		t.Fatalf("update agent config: %v", err)
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
	if err := s.agents.Start(agent, providerEnv, s.port, s.GetProviderPool(userID, agent.ProjectID), s.instanceSecret); err != nil {
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
	ID    string
	Time  time.Time
	Kind  string
	State string
	Text  string
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
				Kind  string `json:"kind"`
				State string `json:"state"`
				Text  string `json:"text"`
			} `json:"args"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			kind, ok := channelCoverageToolKind(data.Name, data.Args.Kind)
			if !ok {
				continue
			}
			out.calls = append(out.calls, channelSendCall{
				ID: data.ID, Time: ev.Time, Kind: kind, State: data.Args.State, Text: data.Args.Text,
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
