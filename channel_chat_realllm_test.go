package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	providerState := loadOpenAICodexProviderState(t)
	corePath := findCoreBinary(t)

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
	s, userID, agent := setupRealServerWithProviderState(t, corePath, "chat-action-under-test", directive, 15, "llm", "OpenAI Codex", providerState)
	agent.Config = fmt.Sprintf(`{"include_apteva_server":false,"include_channels":true,"mcp_servers":[{"name":"todo","transport":"http","url":%q}]}`, todoMCP.URL)
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

	apiKey := "apt_test_chat_action"
	if _, err := s.store.CreateAPIKey(userID, "chat-action-test", HashAPIKey(apiKey), apiKey[:8]); err != nil {
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
	time.Sleep(100 * time.Millisecond) // let the SSE handler register the chat subscriber

	postBody := `{"content":"Mark todo alpha done now. Do not just acknowledge; complete it and tell me the result.","context":{"source":"dashboard-floating","title":"Test","route":"/agents/` + itoa64(agent.ID) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, appSrv.URL+"/apps/channel-chat/messages?chat_id="+chatID, bytes.NewBufferString(postBody))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post chat: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("post chat status=%d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

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
