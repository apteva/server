// Package claudepool runs agent turns through the owner's own, signed-in
// Claude Code CLI ("claude -p"), sharing a small number of processes
// between every agent thread.
//
// Claude Code keeps each conversation as a session on disk and can reopen
// it with --resume, so a session does not need a running process. The pool
// keeps one session per conversation and at most MaxProcs processes,
// closing the least recently used idle process when another conversation
// needs one.
//
// Agent tools reach Claude through an MCP endpoint served by the pool.
// When Claude calls a tool, the pool ends the current reply with the tool
// calls and holds the MCP request open until the caller sends the tool
// results with its next request; Claude then continues the same turn.
//
// The pool never reads, stores, refreshes or forwards Claude credentials.
// It runs the unmodified binary, which uses whatever login the user made
// with `claude auth login`, and it strips every ANTHROPIC_* variable from
// the child environment so usage is never billed to an API key by
// accident.
package claudepool

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Message is one conversation entry in provider-neutral form.
type Message struct {
	Role       string     `json:"role"` // "user", "assistant" or "tool"
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool
	IsError    bool       `json:"is_error,omitempty"`     // tool
}

// ToolCall is a tool call made by the assistant.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool the caller offers to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Request is one model call: the caller's full conversation so far.
type Request struct {
	// Owner keys the conversations of one caller, such as one agent.
	// Sessions are matched only among the same owner's sessions.
	Owner string
	// OwnerLabel is shown in the pool status.
	OwnerLabel string
	Model      string
	System     string
	Messages   []Message
	Tools      []Tool
}

// EventKind identifies a streamed piece of a reply.
type EventKind int

const (
	EventText EventKind = iota
	EventThinking
	EventToolStart
	EventToolArgs
	EventDone
)

// Event is one streamed piece of a reply.
type Event struct {
	Kind      EventKind
	Text      string
	ToolIndex int
	ToolID    string
	ToolName  string
	// FinishReason is set on EventDone: "stop", "tool_calls" or "length".
	FinishReason string
	Usage        Usage
	// Model is set on EventDone: the model that wrote the reply, as Claude
	// reports it.
	Model string
}

// Usage is the token usage of one reply.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

var (
	// ErrNotSignedIn means Claude Code has no usable login.
	ErrNotSignedIn = errors.New("Claude Code is not signed in; run `claude auth login`")
	// ErrNotInstalled means no claude binary was found.
	ErrNotInstalled = errors.New("Claude Code is not installed")
	// ErrPoolBusy means every process stayed busy for the whole queue wait.
	ErrPoolBusy = errors.New("every Claude process is busy")
	// ErrLimited means the subscription's usage limit was reached.
	ErrLimited = errors.New("Claude subscription usage limit reached")
	// ErrClosed means the pool was shut down.
	ErrClosed = errors.New("Claude pool is closed")
)

// RetryError wraps an error the caller should retry after a delay.
type RetryError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", e.Err, e.RetryAfter.Round(time.Second))
}

func (e *RetryError) Unwrap() error { return e.Err }

// LimitInfo is the latest subscription usage limit state Claude Code
// reported in its rate_limit_event messages.
type LimitInfo struct {
	Status      string     `json:"status,omitempty"` // "allowed", "allowed_warning", "rejected"
	Type        string     `json:"type,omitempty"`   // "five_hour", "seven_day", ...
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	Utilization *float64   `json:"utilization,omitempty"` // fraction of the window used, when reported
	UpdatedAt   time.Time  `json:"updated_at,omitempty"`
}

// Limits are the adjustable pool limits.
type Limits struct {
	MaxProcs     int
	IdleTimeout  time.Duration
	QueueTimeout time.Duration
}

// ProcessStatus describes one running process.
type ProcessStatus struct {
	Session    string    `json:"session"`
	Owner      string    `json:"owner"`
	OwnerLabel string    `json:"owner_label,omitempty"`
	Model      string    `json:"model,omitempty"`
	PID        int       `json:"pid"`
	State      string    `json:"state"` // starting, replying, waiting_tool, finishing, idle
	Detail     string    `json:"detail,omitempty"`
	Since      time.Time `json:"since"`
	StartedAt  time.Time `json:"started_at"`
	Turns      int       `json:"turns"`
}

// Status is a snapshot of the pool.
type Status struct {
	MaxProcs     int             `json:"max_processes"`
	Running      int             `json:"running"`
	Busy         int             `json:"busy"`
	Waiting      int             `json:"waiting"`
	Sessions     int             `json:"sessions"`
	IdleTimeout  time.Duration   `json:"-"`
	QueueTimeout time.Duration   `json:"-"`
	Processes    []ProcessStatus `json:"processes"`
	Limit        *LimitInfo      `json:"limit,omitempty"`
}
