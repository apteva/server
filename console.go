package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ANSI color codes
const (
	reset   = "\033[0m"
	dim     = "\033[2m"
	bold    = "\033[1m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
	white   = "\033[37m"
)

// ConsoleLogger renders live telemetry events to stderr with colors.
type ConsoleLogger struct {
	broadcaster *TelemetryBroadcaster
	store       *Store
	mu          sync.Mutex
	names       map[int64]string // instanceID → name cache
	lastThread  string           // last printed thread header
}

func NewConsoleLogger(b *TelemetryBroadcaster, store *Store) *ConsoleLogger {
	return &ConsoleLogger{
		broadcaster: b,
		store:       store,
		names:       make(map[int64]string),
	}
}

func (c *ConsoleLogger) Run() {
	ch := c.broadcaster.SubscribeAll()
	for ev := range ch {
		c.render(ev)
	}
}

func (c *ConsoleLogger) instanceName(id int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name, ok := c.names[id]; ok {
		return name
	}
	name, err := c.store.GetInstanceName(id)
	if err != nil || name == "" {
		name = fmt.Sprintf("instance-%d", id)
	}
	c.names[id] = name
	return name
}

// ClearInstanceName removes a cached name so it gets re-fetched.
func (c *ConsoleLogger) ClearInstanceName(id int64) {
	c.mu.Lock()
	delete(c.names, id)
	c.mu.Unlock()
}

func (c *ConsoleLogger) render(ev TelemetryEvent) {
	// Skip events that produce no console output
	switch ev.Type {
	case "llm.start", "llm.done", "llm.error", "tool.call", "tool.result", "tool.pending", "tool.approved", "tool.rejected",
		"thread.spawn", "thread.done", "event.received", "instance.paused", "instance.resumed", "mode.changed":
		// These produce output — continue
	default:
		return
	}

	name := c.instanceName(ev.InstanceID)
	threadLabel := ev.ThreadID
	if threadLabel == "" {
		threadLabel = "main"
	}

	// Print thread header when switching context
	header := fmt.Sprintf("%s/%s", name, threadLabel)
	c.mu.Lock()
	needHeader := header != c.lastThread
	c.lastThread = header
	c.mu.Unlock()

	if needHeader {
		fmt.Fprintf(os.Stderr, "\n  %s┊%s %s%s%s\n", dim, reset, cyan, name, reset)
		if threadLabel != "main" {
			fmt.Fprintf(os.Stderr, "  %s│%s %s%s%s\n", dim, reset, dim, threadLabel, reset)
		}
	}

	// Parse event data
	var data map[string]any
	if ev.Data != nil {
		json.Unmarshal(ev.Data, &data)
	}
	if data == nil {
		data = map[string]any{}
	}

	ts := ev.Time.Format("15:04:05")

	switch ev.Type {
	case "llm.start":
		model := getString(data, "model")
		fmt.Fprintf(os.Stderr, "  %s│%s %s⟳%s %sthinking%s  %s%s  %s%s\n",
			dim, reset,
			yellow, reset,
			white, reset,
			dim, model, ts, reset,
		)

	case "llm.done":
		msg := truncate(getString(data, "message"), 60)
		dur := getFloat(data, "duration_ms")
		tokIn := getInt(data, "tokens_in")
		tokOut := getInt(data, "tokens_out")
		fmt.Fprintf(os.Stderr, "  %s│%s %s✓%s %s%-60s%s  %s%.1fs  ↑%d ↓%d%s\n",
			dim, reset,
			green, reset,
			white, msg, reset,
			dim, dur/1000, tokIn, tokOut, reset,
		)

	case "llm.error":
		// Provider errors carry the actual failure detail in a JSON
		// body (context too long, schema mismatch, quota, invalid
		// model, …). A 70-char prefix masks all of it. Print the
		// full message — this is the one place in the console log
		// where truncation defeats the purpose.
		errMsg := getString(data, "error")
		fmt.Fprintf(os.Stderr, "  %s│%s %s✗ %s%s  %s%s%s\n",
			dim, reset,
			red, errMsg, reset,
			dim, ts, reset,
		)

	case "tool.call":
		toolName := getString(data, "name")
		args := formatToolArgs(data)
		if args != "" {
			fmt.Fprintf(os.Stderr, "  %s│%s %s⚡%s %s%s%s(%s)%s\n",
				dim, reset,
				yellow, reset,
				white, toolName, dim, args, reset,
			)
		} else {
			fmt.Fprintf(os.Stderr, "  %s│%s %s⚡%s %s%s%s\n",
				dim, reset,
				yellow, reset,
				white, toolName, reset,
			)
		}

	case "tool.result":
		toolName := getString(data, "name")
		dur := getFloat(data, "duration_ms")
		success := getBool(data, "success")
		if !success {
			result := truncate(getString(data, "result"), 50)
			fmt.Fprintf(os.Stderr, "  %s│%s %s✗ %s failed: %s%s  %s%.1fs%s\n",
				dim, reset,
				red, toolName, result, reset,
				dim, dur/1000, reset,
			)
		} else {
			result := truncate(getString(data, "result"), 60)
			fmt.Fprintf(os.Stderr, "  %s│%s %s↳%s %s%s%s  %s%.1fs%s",
				dim, reset,
				green, reset,
				white, toolName, reset,
				dim, dur/1000, reset,
			)
			if result != "" {
				fmt.Fprintf(os.Stderr, "  %s%s%s", dim, result, reset)
			}
			fmt.Fprintln(os.Stderr)
		}

	case "tool.pending":
		toolName := getString(data, "name")
		fmt.Fprintf(os.Stderr, "  %s│%s %s⏳ %s awaiting approval%s\n",
			dim, reset,
			magenta, toolName, reset,
		)

	case "tool.approved":
		toolName := getString(data, "name")
		fmt.Fprintf(os.Stderr, "  %s│%s %s✓ %s approved%s\n",
			dim, reset,
			green, toolName, reset,
		)

	case "tool.rejected":
		toolName := getString(data, "name")
		fmt.Fprintf(os.Stderr, "  %s│%s %s✗ %s rejected%s\n",
			dim, reset,
			red, toolName, reset,
		)

	case "thread.spawn":
		childID := getString(data, "id")
		if childID == "" {
			childID = ev.ThreadID
		}
		directive := truncate(getString(data, "directive"), 50)
		fmt.Fprintf(os.Stderr, "  %s│%s %s⚙%s  thread %s%s%s spawned",
			dim, reset,
			cyan, reset,
			bold, childID, reset,
		)
		if directive != "" {
			fmt.Fprintf(os.Stderr, "  %s— %s%s", dim, directive, reset)
		}
		fmt.Fprintln(os.Stderr)

	case "thread.done":
		childID := getString(data, "id")
		if childID == "" {
			childID = ev.ThreadID
		}
		result := truncate(getString(data, "result"), 50)
		fmt.Fprintf(os.Stderr, "  %s│%s %s✓%s  thread %s%s%s done",
			dim, reset,
			cyan, reset,
			bold, childID, reset,
		)
		if result != "" {
			fmt.Fprintf(os.Stderr, "  %s— %s%s", dim, result, reset)
		}
		fmt.Fprintln(os.Stderr)

	case "instance.paused":
		fmt.Fprintf(os.Stderr, "  %s│%s %s⏸  paused%s\n",
			dim, reset,
			yellow, reset,
		)

	case "instance.resumed":
		fmt.Fprintf(os.Stderr, "  %s│%s %s▶  resumed%s\n",
			dim, reset,
			green, reset,
		)

	case "event.received":
		source := getString(data, "source")
		msg := truncate(getString(data, "message"), 60)
		icon := "▶"
		if source == "thread" {
			icon = "⇄"
		} else if source == "webhook" {
			icon = "⚑"
		}
		fmt.Fprintf(os.Stderr, "  %s│%s %s%s%s %s[%s]%s %s%s\n",
			dim, reset,
			cyan, icon, reset,
			dim, source, reset,
			msg, reset,
		)

	case "mode.changed":
		mode := getString(data, "mode")
		fmt.Fprintf(os.Stderr, "  %s│%s %s◆%s  mode → %s%s%s\n",
			dim, reset,
			blue, reset,
			bold, mode, reset,
		)
	}
}

// --- helpers ---

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func getString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func getFloat(data map[string]any, key string) float64 {
	if v, ok := data[key].(float64); ok {
		return v
	}
	return 0
}

func getInt(data map[string]any, key string) int {
	if v, ok := data[key].(float64); ok {
		return int(v)
	}
	return 0
}

func getBool(data map[string]any, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}

func formatToolArgs(data map[string]any) string {
	args, ok := data["args"]
	if !ok {
		return ""
	}
	// Structured args: map[string]any from JSON
	if m, ok := args.(map[string]any); ok {
		parts := make([]string, 0, len(m))
		for k, v := range m {
			val := fmt.Sprintf("%v", v)
			if len(val) > 40 {
				val = val[:40] + "…"
			}
			parts = append(parts, k+"="+val)
		}
		result := strings.Join(parts, ", ")
		if len(result) > 80 {
			return result[:80] + "…"
		}
		return result
	}
	// Legacy string args
	if s, ok := args.(string); ok {
		if len(s) > 80 {
			return s[:80] + "…"
		}
		return s
	}
	return ""
}

