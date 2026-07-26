package channelchat

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Streamer turns the LLM's incremental tool-argument deltas (emitted
// as `llm.tool_chunk` telemetry events when the model is composing a
// `channels_send` or legacy `channels_respond` call
// on a chat-capable thread) into ephemeral
// "streaming" frames on the chat SSE stream.
//
// Why this exists:
//   - The agent calls channels_send(channel="apteva",
//     text="long answer") to
//     reply. Without streaming, the user waits for the entire LLM
//     generation to finish, then sees the message land all at once.
//   - The LLM's provider already streams the tool-call arguments
//     token-by-token, and core surfaces those as telemetry events
//     (see core/thinker.go onToolChunk publisher). We just hadn't
//     plumbed them to the chat UI.
//
// What this does NOT do:
//   - Touch the DB. Streaming frames are pure pubsub. The DB row
//     (the "final" message) is still written by the channels MCP's
//     respond handler when the tool actually executes. That ordering
//     is the whole point — the prior streaming attempt mixed
//     streams into the message store and produced ordering bugs.
//
// Disabled? Set CHANNELCHAT_STREAMING=0 on the server. The telemetry
// tap (server/telemetry.go) gates on that env var and skips Ingest
// entirely when it's "0". This file remains harmless dead code in
// that mode.
type Streamer struct {
	hub *hub

	// Per (threadID, callID) accumulator. tool_chunk events deliver
	// the args incrementally; we accumulate and extract `text` after
	// each chunk. Cleared when tool.call or tool.result for the same
	// id arrives — that's when the final args are known and the DB
	// row will land momentarily.
	mu       sync.Mutex
	buffers  map[string]*streamState
	lastEmit map[string]string // last text we emitted, to skip no-op republishes
}

type streamState struct {
	buf       strings.Builder
	chatID    string // resolved once from threadID prefix
	threadID  string
	callID    string
	tool      string
	createdAt time.Time
}

func newStreamer(h *hub) *Streamer {
	return &Streamer{
		hub:      h,
		buffers:  make(map[string]*streamState),
		lastEmit: make(map[string]string),
	}
}

// StreamFrame is the ephemeral payload pushed to chat SSE subscribers
// while a respond tool call is still being composed by the LLM.
//
//	ChatID:   the chat the streaming message belongs to.
//	ThreadID: the core thread that's emitting (e.g. "main" or
//	          "chat-default-13").
//	CallID:   the tool-call id — stable across chunks for the same
//	          pending respond call.
//	Text:     the current best-effort extraction of the text arg.
//	          Grows monotonically; UI replaces the bubble each frame.
//	Done:     true on the closing frame that tells the UI to drop the
//	          bubble (the final message is about to / has landed).
type StreamFrame struct {
	Type      string    `json:"type"` // always "stream"
	ChatID    string    `json:"chat_id"`
	ThreadID  string    `json:"thread_id"`
	CallID    string    `json:"call_id"`
	Text      string    `json:"text"`
	Phase     string    `json:"phase,omitempty"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
}

// Ingest is the entry point called by the server-side telemetry hook
// for every live event. Filters internally — non-relevant events are
// no-ops, so the caller can pipe the full firehose without per-event
// gating. dataJSON is the event's raw `data` payload; callID + tool
// name are parsed out of it as needed.
func (s *Streamer) Ingest(eventType string, agentID int64, threadID, dataJSON string, eventTime time.Time) {
	if s == nil {
		return
	}
	chatID := ""
	switch {
	case strings.HasPrefix(threadID, "chat-"):
		// Per-chat thread mode: the thread id encodes the chat id.
		chatID = strings.TrimPrefix(threadID, "chat-")
	case (threadID == "main" || threadID == "") && !perThreadEnabled():
		// Legacy rollback mode routes the default chat through main. In normal
		// per-thread mode, main cannot emit internal chat and must not leak a
		// transient tool-argument stream into the default conversation.
		if agentID > 0 {
			chatID = defaultChatID(agentID)
		}
	}
	if chatID == "" {
		return
	}
	switch eventType {
	case "llm.tool_chunk":
		s.onChunk(agentID, threadID, chatID, dataJSON, eventTime)
	case "tool.call":
		// LLM finished composing args, executor is about to run.
		// We still want the user to see the (now complete) text
		// momentarily — the DB row lands within the tool call's
		// execution time, which is usually <100ms but can be more.
		s.onFinalArgs(agentID, threadID, chatID, dataJSON, eventTime)
	case "tool.result":
		// Tool finished. The DB row exists or is about to; tell the
		// UI to drop the streaming bubble. The next real message
		// arriving for this chat would do the same, but this is the
		// authoritative signal.
		s.onToolEnd(agentID, threadID, chatID, dataJSON, eventTime)
	}
}

// onChunk accumulates a streaming delta and republishes if the
// extracted text changed.
func (s *Streamer) onChunk(agentID int64, threadID, chatID, dataJSON string, ts time.Time) {
	// Telemetry payload for llm.tool_chunk is {"tool": "...", "id":
	// "...", "chunk": "..."}. callID is the event's own correlator —
	// we re-extract from the data because the tap delivers the
	// outer event with thread_id, but the call id lives in data.
	var d struct {
		Tool       string `json:"tool"`
		Name       string `json:"name"`
		ID         string `json:"id"`
		CallID     string `json:"call_id"`
		ToolCallID string `json:"tool_call_id"`
		Chunk      string `json:"chunk"`
		Delta      string `json:"delta"`
		Text       string `json:"text"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &d); err != nil {
		return
	}
	tool := firstNonEmpty(d.Tool, d.Name)
	callID := firstNonEmpty(d.ID, d.CallID, d.ToolCallID)
	chunk := firstNonEmpty(d.Chunk, d.Delta, d.Text)
	if !isVisibleChatTool(tool) {
		return
	}
	if callID == "" {
		return
	}
	key := streamCallKey(agentID, threadID, callID)
	s.mu.Lock()
	st, ok := s.buffers[key]
	if !ok {
		st = &streamState{
			chatID:    chatID,
			threadID:  threadID,
			callID:    callID,
			tool:      tool,
			createdAt: ts,
		}
		s.buffers[key] = st
	}
	st.buf.WriteString(chunk)
	current := st.buf.String()
	text, _ := extractTextField(current)
	last := s.lastEmit[key]
	s.mu.Unlock()
	if text == "" || text == last {
		return
	}
	s.mu.Lock()
	s.lastEmit[key] = text
	s.mu.Unlock()
	s.hub.publishStream(StreamFrame{
		Type:      "stream",
		ChatID:    chatID,
		ThreadID:  threadID,
		CallID:    callID,
		Text:      text,
		CreatedAt: ts,
	})
}

// onFinalArgs is the "tool.call" event: the LLM has finished writing
// args. Telemetry payload includes the parsed args, so this is where
// we can guarantee a clean final text extraction.
func (s *Streamer) onFinalArgs(agentID int64, threadID, chatID, dataJSON string, ts time.Time) {
	var d struct {
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		ID         string          `json:"id"`
		CallID     string          `json:"call_id"`
		ToolCallID string          `json:"tool_call_id"`
		Args       json.RawMessage `json:"args"`
		Arguments  json.RawMessage `json:"arguments"`
		Input      json.RawMessage `json:"input"`
		Params     json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &d); err != nil {
		return
	}
	name := firstNonEmpty(d.Name, d.Tool)
	callID := firstNonEmpty(d.ID, d.CallID, d.ToolCallID)
	if !isVisibleChatTool(name) || callID == "" {
		return
	}
	argsRaw := firstRaw(d.Args, d.Arguments, d.Input, d.Params)
	channel, text, phase, ok := decodeRespondArgs(argsRaw)
	if !ok {
		return
	}
	if channel != "" && channel != "chat" && channel != "apteva" && channel != "current" {
		// Tool call targeted a different channel — not our problem.
		return
	}
	key := streamCallKey(agentID, threadID, callID)
	s.mu.Lock()
	signature := text + "\x00" + phase
	last := s.lastEmit[key]
	s.mu.Unlock()
	if text == "" || signature == last {
		return
	}
	s.mu.Lock()
	s.lastEmit[key] = signature
	s.mu.Unlock()
	s.hub.publishStream(StreamFrame{
		Type:      "stream",
		ChatID:    chatID,
		ThreadID:  threadID,
		CallID:    callID,
		Text:      text,
		Phase:     phase,
		CreatedAt: ts,
	})
}

// onToolEnd clears state when the respond tool has run. The real DB
// message landing on the normal SSE path is still what the UI swaps
// in; Done is only a cleanup signal for stale ephemeral bubbles.
func (s *Streamer) onToolEnd(agentID int64, threadID, chatID, dataJSON string, ts time.Time) {
	var d struct {
		Name       string `json:"name"`
		Tool       string `json:"tool"`
		ID         string `json:"id"`
		CallID     string `json:"call_id"`
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &d); err != nil {
		return
	}
	name := firstNonEmpty(d.Name, d.Tool)
	callID := firstNonEmpty(d.ID, d.CallID, d.ToolCallID)
	if !isVisibleChatTool(name) || callID == "" {
		// Not our tool, or the provider didn't surface the call id —
		// leave any stale buffer; it gets superseded on the next
		// tool_chunk for a new call id.
		return
	}
	key := streamCallKey(agentID, threadID, callID)
	s.mu.Lock()
	delete(s.buffers, key)
	delete(s.lastEmit, key)
	s.mu.Unlock()
	s.hub.publishStream(StreamFrame{
		Type:      "stream",
		ChatID:    chatID,
		ThreadID:  threadID,
		CallID:    callID,
		Done:      true,
		CreatedAt: ts,
	})
}

func streamCallKey(agentID int64, threadID, callID string) string {
	// Provider call IDs are only guaranteed inside one model response. Scope
	// them by agent and thread before touching shared streamer state so two
	// cores using the same call ID cannot suppress, append to, or clear each
	// other's visible chat stream.
	return strconv.FormatInt(agentID, 10) + "\x00" + threadID + "\x00" + callID
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isVisibleChatTool(name string) bool {
	switch name {
	case "channels_respond", "channels_send":
		return true
	default:
		return false
	}
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(strings.TrimSpace(string(v))) > 0 && strings.TrimSpace(string(v)) != "null" {
			return v
		}
	}
	return nil
}

func decodeRespondArgs(raw json.RawMessage) (channel, text, phase string, ok bool) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", "", "", false
	}
	var args struct {
		Channel string `json:"channel"`
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		Phase   string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &args); err == nil {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if kind != "" && kind != "message" {
			return "", "", "", false
		}
		return args.Channel, args.Text, normalizeMessagePhase(args.Phase), args.Channel != "" || args.Text != ""
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil && encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &args); err == nil {
			kind := strings.ToLower(strings.TrimSpace(args.Kind))
			if kind != "" && kind != "message" {
				return "", "", "", false
			}
			return args.Channel, args.Text, normalizeMessagePhase(args.Phase), args.Channel != "" || args.Text != ""
		}
	}
	return "", "", "", false
}

// extractTextField pulls the value of the JSON object's `text` key
// out of a possibly-truncated input. The LLM's tool-arg JSON streams
// in piecewise, so by the time we want to render a partial bubble
// we may only have e.g. `{"channel":"chat","text":"Hello wor`.
//
// Returns (text, ok). ok=true means we found the key; the returned
// string is the cleanest reconstruction up to the truncation point.
// Bare value-only matches ({"text": ...}) are supported; nested
// objects are NOT (the respond schema has no nested objects, so we
// keep the scanner flat).
func extractTextField(buf string) (string, bool) {
	i := 0
	for i < len(buf) {
		switch buf[i] {
		case ' ', '\t', '\n', '\r', '{', ',':
			i++
			continue
		case '"':
			// Read key
			i++
			keyStart := i
			for i < len(buf) && buf[i] != '"' {
				if buf[i] == '\\' && i+1 < len(buf) {
					i += 2
					continue
				}
				i++
			}
			if i >= len(buf) {
				return "", false
			}
			key := buf[keyStart:i]
			i++ // past closing quote
			// Skip whitespace + colon
			for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t' || buf[i] == '\n' || buf[i] == ':') {
				i++
			}
			if i >= len(buf) {
				return "", false
			}
			if key == "text" {
				if buf[i] != '"' {
					return "", false
				}
				return readStringContent(buf, i+1)
			}
			// Skip the value for non-text keys
			if buf[i] == '"' {
				i++
				for i < len(buf) && buf[i] != '"' {
					if buf[i] == '\\' && i+1 < len(buf) {
						i += 2
						continue
					}
					i++
				}
				if i >= len(buf) {
					return "", false
				}
				i++
			} else {
				for i < len(buf) && buf[i] != ',' && buf[i] != '}' {
					i++
				}
			}
		default:
			i++
		}
	}
	return "", false
}

// readStringContent reads a JSON string body starting at i (one past
// the opening quote) and returns the unescaped text. If the buffer
// ends mid-string, returns what we have so far — that's the whole
// point: we render partials.
func readStringContent(buf string, i int) (string, bool) {
	var sb strings.Builder
	for i < len(buf) {
		c := buf[i]
		if c == '\\' {
			if i+1 >= len(buf) {
				// Incomplete escape — stop and return what we have.
				return sb.String(), true
			}
			next := buf[i+1]
			switch next {
			case '"', '\\', '/':
				sb.WriteByte(next)
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'u':
				if i+6 > len(buf) {
					return sb.String(), true
				}
				hex := buf[i+2 : i+6]
				if r, err := strconv.ParseUint(hex, 16, 32); err == nil {
					sb.WriteRune(rune(r))
				}
				i += 6
				continue
			default:
				sb.WriteByte(next)
			}
			i += 2
		} else if c == '"' {
			return sb.String(), true
		} else {
			sb.WriteByte(c)
			i++
		}
	}
	return sb.String(), true
}
