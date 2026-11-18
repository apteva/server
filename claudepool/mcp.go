package claudepool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// toolPrefix is how Claude Code names the tools of the pool's MCP server.
const toolPrefix = "mcp__" + mcpServerName + "__"

// maxExposedName keeps prefixed tool names within the 64 characters the
// model accepts.
const maxExposedName = 64 - len(toolPrefix)

var invalidToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// exposedName is the name a caller's tool has on the MCP server. Names
// that are too long or use other characters get a stable short alias.
func exposedName(name string) string {
	if len(name) <= maxExposedName && !invalidToolChars.MatchString(name) && name != "" {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	clean := invalidToolChars.ReplaceAllString(name, "_")
	keep := maxExposedName - 7
	if len(clean) > keep {
		clean = clean[:keep]
	}
	return clean + "_" + hex.EncodeToString(sum[:3])
}

// originalToolName maps a tool name Claude used back to the caller's name.
func (p *Pool) originalToolName(sess *session, name string) string {
	short := strings.TrimPrefix(name, toolPrefix)
	p.mu.Lock()
	tools := sess.tools
	p.mu.Unlock()
	for _, tool := range tools {
		if exposedName(tool.Name) == short {
			return tool.Name
		}
	}
	return short
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP serves the MCP endpoint through which Claude Code calls the
// caller's tools. Each session authenticates with its own bearer token.
func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var sess *session
	if ok && token != "" {
		p.mu.Lock()
		sess = p.byToken[token]
		p.mu.Unlock()
	}
	if sess == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// A notification needs no reply.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": "1"},
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": p.listTools(sess)}, nil)
	case "tools/call":
		// Held calls can last minutes; lift any server write deadline.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		result, ok := p.callTool(r.Context(), sess, req.Params)
		if !ok {
			return
		}
		writeRPC(w, req.ID, result, nil)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
	}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0"}
	if len(id) > 0 {
		msg["id"] = id
	} else {
		msg["id"] = nil
	}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

func (p *Pool) listTools(sess *session) []map[string]any {
	p.mu.Lock()
	tools := sess.tools
	p.mu.Unlock()
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := tool.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"name":        exposedName(tool.Name),
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	return out
}

// callTool holds a tool call until the caller sends its result.
func (p *Pool) callTool(ctx context.Context, sess *session, raw json.RawMessage) (map[string]any, bool) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      map[string]any  `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &params)
	id, _ := params.Meta["claudecode/toolUseId"].(string)
	if id == "" {
		id = p.guessCall(ctx, sess, params.Name, params.Arguments)
	}
	if id == "" {
		return toolReply(toolResult{text: "This tool call was not expected.", isError: true}), true
	}
	result, ch := sess.await(id)
	if ch != nil {
		// The reaper stops the process once ToolTimeout passes; this
		// is a backstop.
		timer := time.NewTimer(p.cfg.ToolTimeout + 30*time.Second)
		defer timer.Stop()
		select {
		case result = <-ch:
		case <-timer.C:
			sess.forget(id, ch)
			result = toolResult{text: "The tool did not return a result in time.", isError: true}
		case <-ctx.Done():
			sess.forget(id, ch)
			return nil, false
		case <-p.stop:
			sess.forget(id, ch)
			result = toolResult{text: "Apteva is shutting down.", isError: true}
		}
	}
	return toolReply(result), true
}

func toolReply(result toolResult) map[string]any {
	text := result.text
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": result.isError,
	}
}

// guessCall finds the pending call a request without a tool use id is
// for, by tool name and arguments. Claude Code normally sends the id.
func (p *Pool) guessCall(ctx context.Context, sess *session, name string, args json.RawMessage) string {
	name = strings.TrimPrefix(name, toolPrefix)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		awaiting := sess.awaiting
		p.mu.Unlock()
		var byName string
		for _, call := range awaiting {
			if exposedName(call.Name) != name {
				continue
			}
			if sameJSON(call.Arguments, string(args)) {
				return call.ID
			}
			if byName == "" {
				byName = call.ID
			}
		}
		if byName != "" {
			return byName
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}
