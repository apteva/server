package main

import (
	"fmt"
	"sync"
)

// CLIBridge implements Channel for the CLI.
// Send/Status are no-ops when a client is connected — the CLI receives
// messages via SSE telemetry. When no client is connected, Send returns
// an error so the agent knows nobody is listening.
type CLIBridge struct {
	mu        sync.Mutex
	connected int
}

func NewCLIBridge() *CLIBridge {
	return &CLIBridge{}
}

func (c *CLIBridge) ID() string { return "cli" }

// Send returns an error when no client is connected so the agent knows
// nobody will see the message. When connected, it's a no-op — the CLI
// receives the text via SSE telemetry streaming.
func (c *CLIBridge) Send(text string) error {
	c.mu.Lock()
	n := c.connected
	c.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("cli channel: no user connected — message not delivered")
	}
	return nil
}

// Connect increments the connected client count.
func (c *CLIBridge) Connect() {
	c.mu.Lock()
	c.connected++
	c.mu.Unlock()
}

// Disconnect decrements the connected client count.
func (c *CLIBridge) Disconnect() {
	c.mu.Lock()
	if c.connected > 0 {
		c.connected--
	}
	c.mu.Unlock()
}

// IsConnected returns true if at least one client is listening.
func (c *CLIBridge) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected > 0
}

// Status is a no-op when connected. Returns error when no client is listening.
func (c *CLIBridge) Status(text, level string) error {
	c.mu.Lock()
	n := c.connected
	c.mu.Unlock()
	if n == 0 {
		return fmt.Errorf("cli channel: no user connected")
	}
	return nil
}

func (c *CLIBridge) Close() {}
