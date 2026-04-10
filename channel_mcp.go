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
	var channelIDs []string
	if s.ic != nil {
		channelIDs = s.ic.AvailableChannels()
	} else {
		for _, ch := range s.registry.List() {
			channelIDs = append(channelIDs, ch.ID())
		}
	}
	channelList := strings.Join(channelIDs, ", ")
	if channelList == "" {
		channelList = "cli"
	}

	return map[string]any{
		"tools": []map[string]any{
			{
				"name": "respond",
				"description": fmt.Sprintf(
					"Send a message to a user on a channel. Every user message MUST get a response via this tool. "+
						"IMPORTANT: Send ONE complete response per user message. Include ALL information in a single call — do NOT split across multiple calls or follow up with a second message repeating the same content. "+
						"Connected channels: [%s]. "+
						"Match the channel from the event prefix: [cli] → channel=\"cli\", [telegram:@john:12345] → channel=\"telegram:12345\".",
					channelList,
				),
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
				"description": "List all currently connected communication channels.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
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
		if text == "" {
			return nil, &mcpRPCError{Code: -32602, Message: "text required"}
		}
		if channel == "" {
			channel = "cli"
		}
		if err := s.registry.Send(channel, text); err != nil {
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
		if level == "" {
			level = "info"
		}
		ch := s.registry.Get(channel)
		if ch == nil {
			return nil, &mcpRPCError{Code: -32602, Message: fmt.Sprintf("channel %q not found", channel)}
		}
		ch.Status(line, level)
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
