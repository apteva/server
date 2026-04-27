package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// channelMCPServer is an HTTP MCP server exposing unified channel tools for core.
// Runs per-instance in the server process.
type channelMCPServer struct {
	port     int
	listener net.Listener
	registry *ChannelRegistry
	ic       *InstanceChannels // parent — for listing available channels

	mu     sync.Mutex
	closed bool
}

func newChannelMCPServer(registry *ChannelRegistry) (*channelMCPServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &channelMCPServer{
		port:     ln.Addr().(*net.TCPAddr).Port,
		listener: ln,
		registry: registry,
	}, nil
}

func (s *channelMCPServer) url() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

func (s *channelMCPServer) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	http.Serve(s.listener, mux)
}

func (s *channelMCPServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.listener.Close()
	}
}

type mcpRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpRPCError     `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *channelMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req mcpRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result any
	var rpcErr *mcpRPCError

	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "apteva-channels", "version": "1.0.0"},
		}
	case "tools/list":
		result = s.toolsList()
	case "tools/call":
		result, rpcErr = s.handleToolCall(req.Params)
	default:
		rpcErr = &mcpRPCError{Code: -32601, Message: "method not found"}
	}

	resp := mcpRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *channelMCPServer) toolsList() map[string]any {
	// Use RegisteredChannels, NOT AvailableChannels. MCP clients cache
	// tools/list from the initialize handshake and never re-fetch — if
	// we emit a description that says "CONNECTED CHANNELS: [none]" at
	// boot (before any dashboard has opened chat), that cached line
	// follows the agent forever. The call-time gate in handleToolCall
	// still rejects dead channels with a clean error, so nothing is
	// lost on correctness; we just stop lying to the agent about
	// which channels exist as targets.
	var channelIDs []string
	if s.ic != nil {
		channelIDs = s.ic.RegisteredChannels()
	} else {
		for _, ch := range s.registry.List() {
			channelIDs = append(channelIDs, ch.ID())
		}
	}
	channelList := strings.Join(channelIDs, ", ")
	if channelList == "" {
		channelList = "none — no channels configured"
	}

	return map[string]any{
		"tools": []map[string]any{
			{
				"name": "respond",
				"description": buildRespondDescription(channelIDs),
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"text", "channel"},
					"properties": map[string]any{
						"text":    map[string]any{"type": "string", "description": "The message to send"},
						"channel": map[string]any{"type": "string", "description": "Target channel ID, e.g. \"cli\", \"telegram:12345\""},
					},
				},
			},
			{
				"name":        "status",
				"description": "Send a status update to a specific channel.",
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"line", "channel"},
					"properties": map[string]any{
						"line":    map[string]any{"type": "string", "description": "Status text"},
						"channel": map[string]any{"type": "string", "description": "Target channel ID"},
						"level":   map[string]any{"type": "string", "description": "Severity: info, warn, or alert", "enum": []string{"info", "warn", "alert"}},
					},
				},
			},
			{
				"name":        "list_channels",
				"description": "List currently connected channels. RARELY NEEDED: the `respond` tool's description already lists the connected channels on every turn (that listing IS authoritative). Call this tool ONLY if you need to introspect channel availability for some out-of-band reason — never as a precondition to calling respond.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
}

// buildRespondDescription emits a respond-tool description whose
// routing examples are filtered to ONLY the currently connected
// channels. The previous static description listed every possible
// channel (cli, chat, telegram, slack, email) even when only one
// was live — LLMs treat examples as strong priors and would call
// respond(channel="cli") even with chat as the sole connected
// channel, because "cli" appeared right there in the tool doc.
// Dynamic examples kill that failure mode: if the agent sees only
// [chat] as a valid channel in the docs, it calls channel="chat".
func buildRespondDescription(channelIDs []string) string {
	var examples []string
	for _, id := range channelIDs {
		// Strip cosmetic telegram suffixes for the example.
		raw := id
		if i := strings.Index(raw, " "); i > 0 {
			raw = raw[:i]
		}
		switch {
		case raw == "cli":
			examples = append(examples, `[cli] → channel="cli"`)
		case raw == "chat":
			examples = append(examples, `[chat] → channel="chat"`)
		case strings.HasPrefix(raw, "telegram"):
			examples = append(examples, `[telegram:@user:12345] → channel="telegram:12345" (digits only)`)
		case strings.HasPrefix(raw, "slack:"):
			examples = append(examples, fmt.Sprintf(`[slack:user:%s] → channel="%s" (C-prefixed id only, not the username)`, raw[len("slack:"):], raw))
		case strings.HasPrefix(raw, "email:"):
			examples = append(examples, fmt.Sprintf(`[email:user@example.com] → channel="%s"`, raw))
		default:
			examples = append(examples, fmt.Sprintf(`channel="%s"`, raw))
		}
	}
	connectedList := strings.Join(channelIDs, ", ")
	if connectedList == "" {
		connectedList = "none"
	}
	examplesLine := strings.Join(examples, "; ")
	if examplesLine == "" {
		examplesLine = "(none — no channels currently accept responses; see DIRECTIVES rule below)"
	}

	return fmt.Sprintf(
		"Send a message to a user on a channel. Text in your thoughts is INVISIBLE — only this tool delivers messages.\n\n"+
			"REPLY CONTRACT (every user message must satisfy this):\n"+
			"- Acknowledging early (\"on it\", \"checking\") is OPTIONAL.\n"+
			"- The FINAL respond before you go idle (pace/done/wait) MUST contain the actual outcome — what you did, what you found, the answer. \"Done\" alone is not enough; include the substance.\n"+
			"- The early acknowledge does NOT satisfy the contract. If you sent \"on it\" and then completed the work, you owe a SECOND respond with the result. The \"don't repeat yourself\" rule applies to the SAME content — not to acknowledge vs. result, which are different content.\n"+
			"- If the work spans multiple iterations, the final iteration must include a respond before any pace/done.\n\n"+
			"KNOWN CHANNELS (valid values for the `channel` parameter): [%s].\n"+
			"Routing — match the event prefix to the channel: %s.\n\n"+
			"If the gate rejects your channel as unknown, the right move is to retry with a channel from the list above — NOT to fall silent. Do NOT default to \"cli\" from training-data prior; use exactly the names listed.\n\n"+
			"DIRECTIVES vs MESSAGES: events whose tag does NOT correspond to a known channel above — e.g. [admin], [system], [inject], or a bare untagged event — are DIRECTIVES from an operator, not user messages. Act on them but do NOT call respond.",
		connectedList, examplesLine,
	)
}

// channelInList reports whether `channel` (normalized) matches any
// entry in the available-channels list, after trimming the display
// suffix telegram channels carry (e.g. "telegram (bot @foo)" → "telegram").
// Needed so the gate in the respond handler can accept channel="chat"
// when AvailableChannels returned ["chat"] verbatim, and channel="telegram:123"
// when it returned "telegram (bot @mybot)".
func channelInList(channel string, available []string) bool {
	for _, a := range available {
		if a == channel {
			return true
		}
		if i := strings.Index(a, " "); i > 0 && a[:i] == channel {
			return true
		}
		// Accept the "telegram:123" vs "telegram" prefix case.
		if strings.HasPrefix(channel, a+":") {
			return true
		}
	}
	return false
}

func (s *channelMCPServer) handleToolCall(params json.RawMessage) (any, *mcpRPCError) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "invalid params"}
	}

	textResult := func(text string) any {
		return map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
		}
	}

	switch call.Name {
	case "respond":
		text, _ := call.Arguments["text"].(string)
		channel, _ := call.Arguments["channel"].(string)
		rawChannel := channel
		if text == "" {
			return nil, &mcpRPCError{Code: -32602, Message: "text required"}
		}
		// Gate by the active channels list BEFORE attempting Send.
		// This makes the feedback loop loud when the agent picks a
		// channel that isn't in the connected list (e.g. defaulting
		// to "cli" from training). The error tells it exactly what
		// the valid options are, so the next turn's tool_result
		// becomes the correction signal the LLM needs.
		var connected []string
		if s.ic != nil {
			connected = s.ic.AvailableChannels()
		} else {
			for _, ch := range s.registry.List() {
				connected = append(connected, ch.ID())
			}
		}
		normalized := normalizeChannelID(channel)
		if channel == "" || !channelInList(normalized, connected) {
			// "Stay silent" only applies when the connected list is
			// EMPTY (no user reachable on any surface). When the list
			// has channels but the agent picked the wrong name, the
			// fix is to retry with a valid name — silence here would
			// strand the user with a question that never gets answered.
			var msg string
			if len(connected) == 0 {
				msg = fmt.Sprintf(
					"channel %q is invalid and no channel is currently connected — no user is reachable. "+
						"Treat the originating event as a directive and act silently for now.",
					rawChannel,
				)
			} else {
				msg = fmt.Sprintf(
					"channel %q is not in the currently connected channels %v. "+
						"Retry with a name from that list. Do NOT fall silent — the user is waiting for your reply.",
					rawChannel, connected,
				)
			}
			return nil, &mcpRPCError{Code: -32602, Message: msg}
		}
		if err := s.registry.Send(normalized, text); err != nil {
			return nil, &mcpRPCError{Code: -32602, Message: err.Error()}
		}
		return textResult("delivered — do NOT send another respond for this same user message"), nil

	case "status":
		line, _ := call.Arguments["line"].(string)
		channel, _ := call.Arguments["channel"].(string)
		level, _ := call.Arguments["level"].(string)
		if channel == "" {
			channel = "cli"
		}
		channel = normalizeChannelID(channel)
		if level == "" {
			level = "info"
		}
		ch := s.registry.Get(channel)
		if ch == nil {
			return nil, &mcpRPCError{Code: -32602, Message: fmt.Sprintf("channel %q not found", channel)}
		}
		if err := ch.Status(line, level); err != nil {
			return nil, &mcpRPCError{Code: -32602, Message: err.Error()}
		}
		return textResult("ok"), nil

	case "list_channels":
		var ids []string
		if s.ic != nil {
			ids = s.ic.AvailableChannels()
		} else {
			for _, ch := range s.registry.List() {
				ids = append(ids, ch.ID())
			}
		}
		return textResult(fmt.Sprintf("Connected channels: %s", strings.Join(ids, ", "))), nil

	default:
		return nil, &mcpRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", call.Name)}
	}
}

// normalizeChannelID strips extra prefix parts that agents include from
// event format: slack:user:C123 → slack:C123, telegram:@user:123 → telegram:123
func normalizeChannelID(channel string) string {
	if strings.HasPrefix(channel, "slack:") {
		parts := strings.Split(channel, ":")
		if len(parts) == 3 {
			return "slack:" + parts[2]
		}
	}
	if strings.HasPrefix(channel, "telegram:") {
		parts := strings.Split(channel, ":")
		if len(parts) == 3 {
			return "telegram:" + parts[2]
		}
	}
	return channel
}
