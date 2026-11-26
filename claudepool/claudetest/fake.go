// Package claudetest provides a fake `claude` CLI for tests. A test binary
// calls FakeMain from TestMain when CLAUDEPOOL_FAKE=1 and points the pool
// at its own executable; the fake then plays back stream-json the way
// Claude Code does and calls tools over the pool's MCP endpoint.
package claudetest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// tools; " ;; " separates the model's messages, so "CALL a {} ;; b {}"
// calls a, then b once a's result is back. A "raw:" name is called without
// the MCP prefix. "AUTHFAIL" makes the turn fail as a lost login does;
// anything else is echoed back as "echo: <text>". Content blocks are joined
// with newlines, and images, in user content and in tool results, are shown
// as ImageMarker.
// CLAUDEPOOL_FAKE_RESUME_FAIL=1 makes --resume fail as it does for an
// unknown session.
//
// Like Claude Code, it reads the tool list when it starts and again only
// on notifications/tools/list_changed (from a server that declared
// listChanged), and answers a call to a tool not in that list itself,
// with "NOTLISTED <name>", without reaching the server.
// CLAUDEPOOL_FAKE_IGNORE_LIST_CHANGED=1 makes it ignore those
// notifications.

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
	IdleTimeout string `json:"idle_timeout"`
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
		{"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku","description":"Fastest"},
		{"value":"locked","displayName":"Locked","description":"Not on this plan","disabled":true}]}`,
}

// FakeMain runs the fake CLI with the given arguments and returns its
// exit code.
func FakeMain(args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "login" {
		return fakeLogin()
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
		ToolTimeout: os.Getenv("MCP_TOOL_TIMEOUT"), IdleTimeout: os.Getenv("CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT"),
		NoMDs: os.Getenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS"),
		Dir:   dir, HasAPIKey: os.Getenv("ANTHROPIC_API_KEY") != "",
	}
	if path := os.Getenv("CLAUDEPOOL_FAKE_LOG"); path != "" {
		line, _ := json.Marshal(entry)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	if mode == "resume" && os.Getenv("CLAUDEPOOL_FAKE_RESUME_FAIL") == "1" {
		fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", session)
		return 1
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
	client := &fakeMCP{url: server.URL, headers: server.Headers}
	if client.url != "" {
		client.connect()
	}

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
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		var parts []string
		for _, block := range msg.Message.Content {
			if block.Type == "image" {
				parts = append(parts, ImageMarker(block.Source.MediaType, block.Source.Data))
				continue
			}
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
			var results []string
			for _, step := range strings.Split(strings.TrimPrefix(text, "CALL "), " ;; ") {
				var calls []call
				for _, spec := range strings.Split(step, " | ") {
					name, args, _ := strings.Cut(strings.TrimSpace(spec), " ")
					toolN++
					calls = append(calls, call{id: fmt.Sprintf("toolu_%d_%d", os.Getpid(), toolN), name: name, args: args})
				}
				event(map[string]any{"type": "message_start", "message": map[string]any{"model": currentModel(), "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}}})
				for i, c := range calls {
					called := toolPrefix + c.name
					if bare, ok := strings.CutPrefix(c.name, "raw:"); ok {
						called = bare
					}
					event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": c.id, "name": called}})
					half := len(c.args) / 2
					event(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args[:half]}})
					event(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.args[half:]}})
					event(map[string]any{"type": "content_block_stop", "index": i})
				}
				event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 12}})
				event(map[string]any{"type": "message_stop"})
				emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant"}})
				for _, c := range calls {
					if strings.HasPrefix(c.name, "raw:") || !client.has(c.name) {
						// Claude Code: "No such tool available".
						results = append(results, "NOTLISTED "+strings.TrimPrefix(c.name, "raw:"))
						continue
					}
					results = append(results, client.call(c.id, c.name, c.args))
				}
				emit(map[string]any{"type": "user", "message": map[string]any{"role": "user"}})
			}
			reply("got: " + strings.Join(results, " | "))
		default:
			reply("echo: " + text)
		}
		time.Sleep(30 * time.Millisecond)
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done"})
	}
	return 0
}

// fakeMCP is the fake's connection to the pool's MCP server.
type fakeMCP struct {
	url         string
	headers     map[string]string
	listChanged bool // the server declared tools.listChanged

	mu    sync.Mutex
	tools map[string]bool // the list it last read
}

// connect initializes the connection and reads the tool list, as Claude
// Code does when it starts.
func (m *fakeMCP) connect() {
	init, err := fakePost(context.Background(), m.url, m.headers, rpc(0, "initialize", map[string]any{"protocolVersion": "2025-06-18"}), nil, nil)
	if err == nil {
		result, _ := init["result"].(map[string]any)
		caps, _ := result["capabilities"].(map[string]any)
		tools, _ := caps["tools"].(map[string]any)
		m.listChanged, _ = tools["listChanged"].(bool)
	}
	m.list()
}

func (m *fakeMCP) list() {
	names := map[string]bool{}
	if resp, err := fakePost(context.Background(), m.url, m.headers, rpc(1, "tools/list", nil), nil, nil); err == nil {
		result, _ := resp["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		for _, tool := range tools {
			if t, _ := tool.(map[string]any); t != nil {
				name, _ := t["name"].(string)
				names[name] = true
			}
		}
	}
	m.mu.Lock()
	m.tools = names
	m.mu.Unlock()
}

func (m *fakeMCP) has(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tools[name]
}

// notified handles a server notification other than progress.
func (m *fakeMCP) notified(method string) {
	if method == "notifications/tools/list_changed" && m.listChanged && os.Getenv("CLAUDEPOOL_FAKE_IGNORE_LIST_CHANGED") != "1" {
		m.list()
	}
}

func rpc(id int, method string, params any) []byte {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	return raw
}

func (m *fakeMCP) call(id, name, args string) string {
	var arguments any
	_ = json.Unmarshal([]byte(args), &arguments)
	raw := rpc(2, "tools/call", map[string]any{
		"name": name, "arguments": arguments, "_meta": map[string]any{"claudecode/toolUseId": id, "progressToken": 2},
	})
	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		out map[string]any
		err error
	}
	done := make(chan answer, 1)
	progress := make(chan struct{}, 64)
	go func() {
		out, err := fakePost(ctx, m.url, m.headers, raw, progress, m.notified)
		done <- answer{out, err}
	}()
	// Like Claude Code: give up on a call after a silence (no response
	// and no progress) and carry on with an error result. Claude Code
	// leaves the request itself open; CLAUDEPOOL_FAKE_DROP_CLOSES=1
	// closes it instead.
	silence := time.Duration(0)
	if d, err := time.ParseDuration(os.Getenv("CLAUDEPOOL_FAKE_DROP_AFTER")); err == nil && d > 0 {
		silence = d
	}
	if silence > 0 {
		timer := time.NewTimer(silence)
		defer timer.Stop()
		for {
			select {
			case a := <-done:
				cancel()
				return callText(a.out, a.err)
			case <-progress:
				timer.Reset(silence)
			case <-timer.C:
				if os.Getenv("CLAUDEPOOL_FAKE_DROP_CLOSES") == "1" {
					cancel()
				} else {
					go func() { <-done; cancel() }() // open until it ends
				}
				return callText(nil, fmt.Errorf("no response or progress for %s", silence))
			}
		}
	}
	a := <-done
	cancel()
	return callText(a.out, a.err)
}

// callText renders a tool result: its text items, and an ImageMarker for
// each image, one per line.
func callText(resp map[string]any, err error) string {
	if err != nil {
		return "CALLFAIL " + err.Error()
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var parts []string
	for _, item := range content {
		item, _ := item.(map[string]any)
		switch item["type"] {
		case "image":
			mimeType, _ := item["mimeType"].(string)
			data, _ := item["data"].(string)
			parts = append(parts, ImageMarker(mimeType, data))
		default:
			text, _ := item["text"].(string)
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n")
	if isErr, _ := result["isError"].(bool); isErr {
		return "ERR " + text
	}
	return text
}

// ImageMarker is how the fake shows an image it received, in user content
// or a tool result: its media type and the start of the SHA-256 of its
// base64 data.
func ImageMarker(mediaType, data string) string {
	sum := sha256.Sum256([]byte(data))
	return "[image " + mediaType + " " + hex.EncodeToString(sum[:4]) + "]"
}

// fakePost sends one MCP request and returns the response, reading an event
// stream when the server answers with one; each progress notification is
// signalled on progress, and other notifications go to notified.
func fakePost(ctx context.Context, url string, headers map[string]string, raw []byte, progress chan<- struct{}, notified func(method string)) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, data)
		}
		return out, nil
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(data), &out); err != nil {
			return nil, err
		}
		if method, _ := out["method"].(string); method != "" {
			if method == "notifications/progress" {
				select {
				case progress <- struct{}{}:
				default:
				}
			} else if notified != nil {
				notified(method)
			}
			continue
		}
		return out, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

// FakeLoginCode is the code the fake sign-in accepts.
const FakeLoginCode = "good-code#fake-state"

// fakeLogin plays Claude Code's sign-in on a machine without a browser: it
// prints the link and a prompt without a newline, reads one line, and
// accepts only FakeLoginCode. CLAUDEPOOL_FAKE_LOGIN_URL overrides the link;
// CLAUDEPOOL_FAKE_LOGIN_RECEIVED records what it read.
func fakeLogin() int {
	link := os.Getenv("CLAUDEPOOL_FAKE_LOGIN_URL")
	if link == "" {
		link = "https://claude.com/cai/oauth/authorize?code=true&client_id=fake&state=fake-state"
	}
	fmt.Println("Opening browser to sign in…")
	fmt.Fprintf(os.Stderr, "If the browser didn't open, visit: %s\n", link)
	fmt.Print("Paste code here if prompted > ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if path := os.Getenv("CLAUDEPOOL_FAKE_LOGIN_RECEIVED"); path != "" {
		_ = os.WriteFile(path, []byte(line), 0o600)
	}
	if err != nil {
		return 1
	}
	time.Sleep(100 * time.Millisecond) // the real CLI checks the code with Anthropic
	if strings.TrimSpace(line) != FakeLoginCode {
		fmt.Println("Couldn't log in: That code wasn't accepted. Sign in again to get a new code.")
		return 1
	}
	fmt.Println("Login successful.")
	return 0
}
