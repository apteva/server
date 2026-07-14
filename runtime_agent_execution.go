package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func isPacingToolName(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	return name == "pace" || strings.HasSuffix(name, ".pace")
}

type runtimeThreadMessage struct {
	Role        string              `json:"role"`
	Content     string              `json:"content"`
	Parts       []runtimeThreadPart `json:"parts"`
	ToolCalls   []runtimeToolCall   `json:"tool_calls,omitempty"`
	ToolResults []runtimeToolResult `json:"tool_results,omitempty"`
}

type runtimeThreadPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type runtimeToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type runtimeToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

func (m runtimeThreadMessage) text() string {
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func (s *Server) waitRuntimeAgent(ctx context.Context, agent *EnvironmentAgent, req sdk.RuntimeAgentWaitRequest) (*sdk.RuntimeAgentExecution, error) {
	if agent == nil {
		return nil, fmt.Errorf("runtime agent required")
	}
	threadID := strings.TrimSpace(req.ThreadID)
	if threadID == "" {
		threadID = "main"
	}
	if strings.Contains(threadID, "/") {
		return nil, fmt.Errorf("invalid thread id")
	}
	timeout := boundedRuntimeWait(req.TimeoutSeconds, 10*time.Minute, 5*time.Second, 30*time.Minute)
	idle := boundedRuntimeWait(req.IdleSeconds, 5*time.Second, time.Second, time.Minute)
	postToolIdle := boundedRuntimeWait(req.PostToolIdleSeconds, 30*time.Second, idle, 2*time.Minute)
	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}
	if maxTurns > 100 {
		return nil, fmt.Errorf("max_turns must be at most 100")
	}

	started := time.Now().UTC()
	deadline := started.Add(timeout)
	trace := []sdk.RuntimeTraceEvent{}
	pending := map[string]int{}
	historyCursor := int64(0)
	historySupported := true
	contextCount := 0
	turns := 0
	meaningful := false
	lastActivity := started
	lastToolActivity := time.Time{}
	fetchErrors := 0
	status, reason := "completed", "idle"

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			status, reason = "timeout", "timeout"
			break
		}

		messages, next, supported, err := s.runtimeAgentMessages(ctx, agent, threadID, historyCursor, contextCount, historySupported)
		if !supported {
			historySupported = false
		}
		if err != nil {
			fetchErrors++
			if fetchErrors >= 3 && (s.agents == nil || !s.agents.IsRunning(agent.AgentID)) {
				status, reason = "failed", "agent_stopped"
				break
			}
		} else {
			fetchErrors = 0
			if historySupported {
				historyCursor = next
			} else {
				contextCount += len(messages)
			}
		}

		if len(messages) > 0 {
			lastActivity = time.Now()
			for _, message := range messages {
				text := runtimeTraceText(message.text())
				switch message.Role {
				case "assistant":
					if text != "" {
						trace = append(trace, sdk.RuntimeTraceEvent{Index: len(trace), ThreadID: threadID, Role: "agent", Content: text})
						meaningful = true
					}
					for _, call := range message.ToolCalls {
						tool := &sdk.RuntimeToolCall{ID: call.ID, Name: call.Name, Input: append(json.RawMessage(nil), call.Args...)}
						trace = append(trace, sdk.RuntimeTraceEvent{Index: len(trace), ThreadID: threadID, Role: "tool", ToolCall: tool})
						pending[call.ID] = len(trace) - 1
						if !isPacingToolName(call.Name) {
							meaningful = true
							lastToolActivity = time.Now()
						}
					}
					if text != "" || len(message.ToolCalls) > 0 {
						turns++
					}
				case "user":
					if text != "" && !strings.HasPrefix(text, "(no events)") {
						trace = append(trace, sdk.RuntimeTraceEvent{Index: len(trace), ThreadID: threadID, Role: "user", Content: text})
					}
				}
				for _, result := range message.ToolResults {
					if index, ok := pending[result.CallID]; ok && index >= 0 && index < len(trace) && trace[index].ToolCall != nil {
						trace[index].ToolCall.Output = runtimeTraceText(result.Content)
						trace[index].ToolCall.IsError = result.IsError
						delete(pending, result.CallID)
						lastToolActivity = time.Now()
					} else {
						trace = append(trace, sdk.RuntimeTraceEvent{Index: len(trace), ThreadID: threadID, Role: "tool_result", Content: runtimeTraceText(result.Content)})
					}
				}
			}
			if turns >= maxTurns {
				reason = "max_turns"
				break
			}
		} else if turns > 0 && len(pending) == 0 && (!req.RequireActivity || meaningful) {
			window := idle
			if !lastToolActivity.IsZero() && time.Since(lastToolActivity) < postToolIdle {
				window = postToolIdle
			}
			if time.Since(lastActivity) >= window {
				break
			}
		}

		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	finished := time.Now().UTC()
	metrics := s.runtimeAgentMetrics(agent, agent.CreatedAt)
	return &sdk.RuntimeAgentExecution{Status: status, Reason: reason, ThreadID: threadID, Turns: turns, StartedAt: started, FinishedAt: finished, Trace: trace, Metrics: metrics}, nil
}

func (s *Server) runtimeAgentMessages(ctx context.Context, agent *EnvironmentAgent, threadID string, cursor int64, contextCount int, history bool) ([]runtimeThreadMessage, int64, bool, error) {
	if history {
		path := fmt.Sprintf("/threads/%s/history?after=%d&limit=500", url.PathEscape(threadID), cursor)
		raw, err := runtimeCoreRequest(ctx, agent, http.MethodGet, path, nil)
		if err == nil {
			var payload struct {
				Entries    []runtimeThreadMessage `json:"entries"`
				NextCursor int64                  `json:"next_cursor"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, cursor, true, err
			}
			return payload.Entries, payload.NextCursor, true, nil
		}
		if !strings.Contains(err.Error(), "core http 404") {
			return nil, cursor, true, err
		}
	}
	raw, err := runtimeCoreRequest(ctx, agent, http.MethodGet, "/threads/"+url.PathEscape(threadID)+"/context", nil)
	if err != nil {
		return nil, cursor, false, err
	}
	var payload struct {
		Messages []runtimeThreadMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, cursor, false, err
	}
	if contextCount >= len(payload.Messages) {
		return nil, cursor, false, nil
	}
	return payload.Messages[contextCount:], cursor, false, nil
}

func (s *Server) runtimeAgentMetrics(agent *EnvironmentAgent, since time.Time) sdk.RuntimeAgentMetrics {
	metrics := sdk.RuntimeAgentMetrics{Provider: agent.Provider, Model: agent.Model}
	if s == nil || s.store == nil {
		return metrics
	}
	events, err := s.store.QueryTelemetry(agent.AgentID, "", since, 1000)
	if err != nil {
		return metrics
	}
	for _, event := range events {
		var data map[string]any
		_ = json.Unmarshal(event.Data, &data)
		switch event.Type {
		case "llm.done":
			metrics.LLMCalls++
			metrics.TokensIn += runtimeInt(data["tokens_in"])
			metrics.TokensOut += runtimeInt(data["tokens_out"])
			metrics.TokensCached += runtimeInt(data["tokens_cached"])
			metrics.LLMDurationMS += runtimeInt(data["duration_ms"])
			metrics.CostUSD += runtimeFloat(data["cost_usd"])
			if value := strings.TrimSpace(fmt.Sprint(data["provider"])); value != "" && value != "<nil>" {
				metrics.Provider = value
			}
			if value := strings.TrimSpace(fmt.Sprint(data["model"])); value != "" && value != "<nil>" {
				metrics.Model = value
			}
		case "tool.call":
			metrics.ToolCalls++
		case "llm.error", "tool.error", "error":
			metrics.Errors++
		case "tool.result":
			if failed, _ := data["is_error"].(bool); failed {
				metrics.Errors++
			}
		}
	}
	return metrics
}

func boundedRuntimeWait(seconds int, fallback, min, max time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	value := time.Duration(seconds) * time.Second
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func runtimeTraceText(value string) string {
	const max = 128 << 10
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

func runtimeInt(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		v, _ := n.Int64()
		return int(v)
	default:
		return 0
	}
}

func runtimeFloat(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		v, _ := n.Float64()
		return v
	default:
		return 0
	}
}
