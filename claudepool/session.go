package claudepool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// session is one Claude Code conversation, persisted by Claude Code and
// reopened with --resume whenever it has no running process.
type session struct {
	id         string // Claude Code session id (a UUID)
	owner      string
	ownerLabel string
	mcpToken   string
	createdAt  time.Time

	// Guarded by Pool.mu.
	model      string
	system     string
	toolsKey   string
	tools      []Tool
	history    []Message // the conversation as the caller sees it
	started    bool      // Claude Code has saved this session
	dirty      bool      // history no longer known to match Claude's copy
	proc       *process
	busy       bool // a turn is in progress (replying, waiting for tools, finishing)
	turnHeld   bool // a request holds the turn lock
	lastUsed   time.Time
	state      string
	detail     string
	stateSince time.Time
	turns      int
	awaiting   []ToolCall // tool calls of the last reply, waiting for results
	waitMoves  int64      // proc.moves when the session started waiting for them
	retire     bool       // drop the session once its current turn ends

	// turn serializes requests on this session.
	turn chan struct{}

	// Held tool calls, guarded by callsMu.
	callsMu sync.Mutex
	held    map[string]chan toolResult
	results map[string]toolResult
}

type toolResult struct {
	text    string
	isError bool
}

func newSession(owner, label string) *session {
	return &session{
		id:         newUUID(),
		owner:      owner,
		ownerLabel: label,
		mcpToken:   randomToken(),
		createdAt:  time.Now(),
		lastUsed:   time.Now(),
		state:      "new",
		stateSince: time.Now(),
		turn:       make(chan struct{}, 1),
		held:       map[string]chan toolResult{},
		results:    map[string]toolResult{},
	}
}

func (s *session) setState(state, detail string) {
	if s.state != state || s.detail != detail {
		s.state, s.detail, s.stateSince = state, detail, time.Now()
	}
}

// deliver hands a tool result to the waiting MCP call, or keeps it until
// the call arrives (Claude Code makes the calls of one reply in turn).
func (s *session) deliver(id string, result toolResult) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	if ch, ok := s.held[id]; ok {
		delete(s.held, id)
		ch <- result
		return
	}
	s.results[id] = result
}

// await returns the result for a tool call, waiting for it if needed.
func (s *session) await(id string) (toolResult, chan toolResult) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	if result, ok := s.results[id]; ok {
		delete(s.results, id)
		return result, nil
	}
	ch := make(chan toolResult, 1)
	s.held[id] = ch
	return toolResult{}, ch
}

func (s *session) forget(id string, ch chan toolResult) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	if s.held[id] == ch {
		delete(s.held, id)
	}
}

// awaitsLocked reports whether the caller still owes the result of call
// id. The pool lock must be held.
func (s *session) awaitsLocked(id string) bool {
	for _, call := range s.awaiting {
		if call.ID == id {
			return true
		}
	}
	return false
}

// failHeld answers every held call, used when a session is reset.
func (s *session) failHeld(text string) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	for id, ch := range s.held {
		ch <- toolResult{text: text, isError: true}
		delete(s.held, id)
	}
	s.results = map[string]toolResult{}
}

// plan is what a request needs to send to a session.
type plan struct {
	toolResults []Message // results for the session's awaiting calls
	userText    []string  // new user content
	reseed      bool      // start a fresh Claude session from the history
}

// matchHistory reports whether history is a prefix of messages.
func matchHistory(history, messages []Message) bool {
	if len(history) > len(messages) {
		return false
	}
	for i := range history {
		if !sameMessage(history[i], messages[i]) {
			return false
		}
	}
	return true
}

// sameMessage compares two conversation entries. Assistant replies are
// compared by their tool call ids when they have any, since callers may
// reformat the text or arguments they store.
func sameMessage(a, b Message) bool {
	if a.Role != b.Role {
		return false
	}
	switch a.Role {
	case "assistant":
		if len(a.ToolCalls) > 0 || len(b.ToolCalls) > 0 {
			return sameIDs(a.ToolCalls, b.ToolCalls)
		}
		return normalize(a.Text) == normalize(b.Text)
	case "tool":
		return a.ToolCallID == b.ToolCallID
	default:
		return normalize(a.Text) == normalize(b.Text)
	}
}

func sameIDs(a, b []ToolCall) bool {
	if len(a) != len(b) {
		return false
	}
	ids := map[string]bool{}
	for _, call := range a {
		ids[call.ID] = true
	}
	for _, call := range b {
		if !ids[call.ID] {
			return false
		}
	}
	return true
}

func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// planFor works out how to bring the session up to messages, which the
// caller has already checked starts with the session's history.
func (s *session) planFor(messages []Message) plan {
	suffix := messages[len(s.history):]
	if len(suffix) == 0 {
		return plan{reseed: true}
	}
	var p plan
	i := 0
	if len(s.awaiting) > 0 {
		for i < len(suffix) && suffix[i].Role == "tool" {
			p.toolResults = append(p.toolResults, suffix[i])
			i++
		}
		if len(p.toolResults) == 0 {
			// The caller moved on without returning the tool results.
			return plan{reseed: true}
		}
	}
	for ; i < len(suffix); i++ {
		switch suffix[i].Role {
		case "user":
			p.userText = append(p.userText, suffix[i].Text)
		case "tool":
			// Results for calls this session is not waiting on, e.g.
			// after its process was restarted mid-turn.
			p.userText = append(p.userText, renderToolResult(suffix[i]))
		default:
			return plan{reseed: true}
		}
	}
	return p
}

// renderTranscript turns a conversation into the first message of a new
// Claude session.
func renderTranscript(messages []Message) string {
	if len(messages) == 1 && messages[0].Role == "user" {
		return messages[0].Text
	}
	last := len(messages) - 1
	for last >= 0 && messages[last].Role == "user" {
		last--
	}
	var b strings.Builder
	b.WriteString("This conversation continues from an earlier session. Here is everything so far:\n\n<conversation>\n")
	for _, msg := range messages[:last+1] {
		switch msg.Role {
		case "user":
			fmt.Fprintf(&b, "[user]\n%s\n\n", msg.Text)
		case "assistant":
			if strings.TrimSpace(msg.Text) != "" {
				fmt.Fprintf(&b, "[assistant]\n%s\n\n", msg.Text)
			}
			for _, call := range msg.ToolCalls {
				fmt.Fprintf(&b, "[assistant called tool %s, id %s]\n%s\n\n", call.Name, call.ID, call.Arguments)
			}
		case "tool":
			b.WriteString(renderToolResult(msg))
			b.WriteString("\n\n")
		}
	}
	b.WriteString("</conversation>\n\n")
	if last == len(messages)-1 {
		b.WriteString("Continue from the last tool results above.")
		return b.String()
	}
	for _, msg := range messages[last+1:] {
		b.WriteString(msg.Text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

func renderToolResult(msg Message) string {
	label := "result"
	if msg.IsError {
		label = "error"
	}
	return fmt.Sprintf("[tool %s for call %s]\n%s", label, msg.ToolCallID, msg.Text)
}

func toolsKey(tools []Tool) string {
	sorted := append([]Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, tool := range sorted {
		h.Write([]byte(tool.Name))
		h.Write([]byte{0})
		h.Write([]byte(tool.Description))
		h.Write([]byte{0})
		h.Write(tool.InputSchema)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
