package main

import (
	"fmt"
	"sync"
	"time"
)

// CLIBridge implements Channel for the CLI.
// Send/Status are no-ops — the CLI receives messages via SSE telemetry
// (llm.tool_chunk for streaming, tool.result for completion).
// Ask blocks until the CLI posts a reply via HTTP.
type CLIBridge struct {
	mu        sync.Mutex
	pendingAsk *pendingAsk
}

type pendingAsk struct {
	question string
	replyCh  chan string
}

func NewCLIBridge() *CLIBridge {
	return &CLIBridge{}
}

func (c *CLIBridge) ID() string { return "cli" }

// Send is a no-op — the CLI receives the text via SSE telemetry streaming.
func (c *CLIBridge) Send(text string) error {
	return nil
}

// Ask stores the question and blocks until the CLI replies via HTTP POST.
// The question text reaches the CLI via SSE tool argument streaming.
func (c *CLIBridge) Ask(question string) (string, error) {
	replyCh := make(chan string, 1)
	c.mu.Lock()
	c.pendingAsk = &pendingAsk{question: question, replyCh: replyCh}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.pendingAsk = nil
		c.mu.Unlock()
	}()

	select {
	case reply := <-replyCh:
		return reply, nil
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("ask timeout: no reply from CLI within 5 minutes")
	}
}

// SubmitReply delivers the CLI user's answer to a pending Ask call.
// Called by the HTTP handler for POST /instances/:id/channels/cli/reply.
func (c *CLIBridge) SubmitReply(text string) error {
	c.mu.Lock()
	pa := c.pendingAsk
	c.mu.Unlock()
	if pa == nil {
		return fmt.Errorf("no pending question")
	}
	select {
	case pa.replyCh <- text:
		return nil
	default:
		return fmt.Errorf("reply already submitted")
	}
}

// HasPendingAsk returns true if the agent is waiting for a CLI reply.
func (c *CLIBridge) HasPendingAsk() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingAsk != nil
}

// Status is a no-op — the CLI receives status via SSE tool call telemetry.
func (c *CLIBridge) Status(text, level string) error {
	return nil
}

func (c *CLIBridge) Close() {}
