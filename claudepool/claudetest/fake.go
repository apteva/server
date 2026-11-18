// Package claudetest provides a fake `claude` CLI for tests. A test binary
// calls FakeMain from TestMain when CLAUDEPOOL_FAKE=1 and points the pool
// at its own executable; the fake then plays back stream-json the way
// Claude Code does and calls tools over the pool's MCP endpoint.
package claudetest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// A user message "CALL name {json} | name {json}" makes it call those
// tools; "AUTHFAIL" makes the turn fail as a lost login does; anything
// else is echoed back as "echo: <text>".

const (
	mcpServerName = "apteva"
	toolPrefix    = "mcp__" + mcpServerName + "__"
)

// FakeLog is one line of CLAUDEPOOL_FAKE_LOG: how a fake process started.
type FakeLog struct {
	Mode        string `json:"mode"`
	Session     string `json:"session"`
	Model       string `json:"model"`
	System      string `json:"system"`
	ToolTimeout string `json:"tool_timeout"`
	NoMDs       string `json:"no_mds"`
	Dir         string `json:"dir"`
	HasAPIKey   bool   `json:"has_api_key"`
}

// fakeAnswers are the replies to control requests that only read state,
// shaped like the real CLI's.
var fakeAnswers = map[string]string{
	"get_usage": `{"subscription_type":"max","rate_limits_available":true,"behaviors":null,
		"rate_limits":{"five_hour":{"utilization":45,"resets_at":"2030-01-01T10:00:00.5+00:00"},
		"seven_day":{"utilization":54,"resets_at":"2030-01-05T09:00:00+00:00"},
		"seven_day_opus":null,"seven_day_sonnet":null,
		"model_scoped":[{"display_name":"Fable","utilization":0,"resets_at":"2030-01-05T09:00:00+00:00"}],
		"extra_usage":{"is_enabled":false,"monthly_limit":4250,"used_credits":0,"utilization":0,"currency":"EUR"}}}`,
	"list_models": `{"models":[
		{"value":"default","resolvedModel":"claude-opus-5-5[1m]","displayName":"Default (recommended)","description":"Opus with 1M context"},
		{"value":"opus[1m]","resolvedModel":"claude-opus-5-5[1m]","displayName":"Opus (1M context)","description":"Opus with 1M context"},
		{"value":"claude-fable-5-1[1m]","resolvedModel":"claude-fable-5-1","displayName":"Fable","description":"Most capable"},
		{"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet","description":"Efficient"},
		{"value":"haiku","resolvedModel":"claude-haiku-4-5","displayName":"Haiku","description":"Fastest"},
		{"value":"locked","displayName":"Locked","description":"Not on this plan","disabled":true}]}`,
}

// FakeMain runs the fake CLI with the given arguments and returns its
// exit code.
func FakeMain(args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		if os.Getenv("CLAUDEPOOL_FAKE_LOGGEDOUT") == "1" {
			fmt.Println(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`)
			return 1
		}
		fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`)
		return 0
	}

	var mcpFile, session, mode, model, systemFile string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--mcp-config":
			mcpFile = args[i+1]
		case "--session-id":
			mode, session = "new", args[i+1]
		case "--resume":
			mode, session = "resume", args[i+1]
		case "--model":
			model = args[i+1]
		case "--system-prompt-file":
			systemFile = args[i+1]
		}
	}
	for _, arg := range args {
		if arg == "--no-session-persistence" {
			mode = "probe"
		}
	}
	system, _ := os.ReadFile(systemFile)
	dir, _ := os.Getwd()
	entry := FakeLog{
		Mode: mode, Session: session, Model: model, System: string(system),
		ToolTimeout: os.Getenv("MCP_TOOL_TIMEOUT"), NoMDs: os.Getenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS"),
		Dir: dir, HasAPIKey: os.Getenv("ANTHROPIC_API_KEY") != "",
	}
	if path := os.Getenv("CLAUDEPOOL_FAKE_LOG"); path != "" {
		line, _ := json.Marshal(entry)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}

	var mcp struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(mcpFile)
	_ = json.Unmarshal(raw, &mcp)
	server := mcp.MCPServers[mcpServerName]

	out := bufio.NewWriter(os.Stdout)
	var outMu, modelMu sync.Mutex
	emit := func(v any) {
		line, _ := json.Marshal(v)
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = out.Write(append(line, '\n'))
		_ = out.Flush()
	}
	currentModel := func() string {
		modelMu.Lock()
		defer modelMu.Unlock()
		return model
	}
	event := func(ev map[string]any) {
		emit(map[string]any{"type": "stream_event", "event": ev})
	}
	reply := func(text string) {
		event(map[string]any{"type": "message_start", "message": map[string]any{"model": currentModel(), "usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 5, "output_tokens": 1}}})
		event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}})
		event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "hmm"}})
		event(map[string]any{"type": "content_block_stop", "index": 0})
		event(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}})
		half := len(text) / 2
		event(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": text[:half]}})
		event(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": text[half:]}})
		event(map[string]any{"type": "content_block_stop", "index": 1})
		event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}})
		event(map[string]any{"type": "message_stop"})
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant"}})
	}

	// Control requests are answered at once, even while a tool call is
	// pending, as the real CLI does.
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			var ctl struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string  `json:"subtype"`
					Model   *string `json:"model"`
				} `json:"request"`
			}
			if json.Unmarshal(line, &ctl) != nil || ctl.Type != "control_request" {
				lines <- line
				continue
			}
			if body, ok := fakeAnswers[ctl.Request.Subtype]; ok {
				if os.Getenv("CLAUDEPOOL_FAKE_ASK_FAIL") == "1" {
					emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": ctl.RequestID, "error": "usage endpoint unavailable"}})
					continue
				}
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": ctl.RequestID, "response": json.RawMessage(body)}})
				continue
			}
			if ctl.Request.Subtype != "set_model" {
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": ctl.RequestID, "error": "unsupported"}})
				continue
			}
			modelMu.Lock()
			model = ""
			if ctl.Request.Model != nil {
				model = *ctl.Request.Model
			}
			modelMu.Unlock()
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": ctl.RequestID}})
		}
	}()

	toolN := 0
	for raw := range lines {
		var msg struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		var parts []string
		for _, block := range msg.Message.Content {
			parts = append(parts, block.Text)
		}
		text := strings.Join(parts, "\n")
		emit(map[string]any{"type": "system", "subtype": "init", "model": model, "mcp_servers": []map[string]any{{"name": mcpServerName, "status": "connected"}}})
		emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed", "resetsAt": time.Now().Add(time.Hour).Unix(), "rateLimitType": "five_hour", "utilization": 0.42}})

		switch {
		case strings.HasPrefix(text, "AUTHFAIL"):
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Failed to authenticate. API Error: 401 OAuth token has expired"})
			continue
		case strings.HasPrefix(text, "CALL "):
			type call struct{ id, name, args string }
			var calls []call
			for _, spec := range strings.Split(strings.TrimPrefix(text, "CALL "), " | ") {
				name, args, _ := strings.Cut(strings.TrimSpace(spec), " ")
				toolN++
				calls = append(calls, call{id: fmt.Sprintf("toolu_%d_%d", os.Getpid(), toolN), name: name, args: args})
			}
			event(map[string]any{"type": "message_start", "message": map[string]any{"model": currentModel(), "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}}})
			for i, c := range calls {
				event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": c.id, "name": toolPrefix + c.name}})
				half := len(c.args) / 2
				event(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args[:half]}})
				event(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args[half:]}})
				event(map[string]any{"type": "content_block_stop", "index": i})
			}
			event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 12}})
			event(map[string]any{"type": "message_stop"})
			emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant"}})
			var results []string
			for _, c := range calls {
				results = append(results, fakeMCPCall(server.URL, server.Headers, c.id, c.name, c.args))
			}
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user"}})
			reply("got: " + strings.Join(results, " | "))
		default:
			reply("echo: " + text)
		}
		time.Sleep(30 * time.Millisecond)
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done"})
	}
	return 0
}

func fakeMCPCall(url string, headers map[string]string, id, name, args string) string {
	post := func(body any) (map[string]any, error) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, data)
		}
		return out, nil
	}
	list, err := post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if err != nil {
		return "LISTFAIL " + err.Error()
	}
	found := false
	result, _ := list["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	for _, tool := range tools {
		if m, _ := tool.(map[string]any); m["name"] == name {
			found = true
		}
	}
	if !found {
		return "NOTLISTED " + name
	}
	var arguments any
	_ = json.Unmarshal([]byte(args), &arguments)
	resp, err := post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": name, "arguments": arguments, "_meta": map[string]any{"claudecode/toolUseId": id, "progressToken": 2},
	}})
	if err != nil {
		return "CALLFAIL " + err.Error()
	}
	result, _ = resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	text := ""
	if len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		return "ERR " + text
	}
	return text
}
