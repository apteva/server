package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AgentActivityOptions struct {
	ProjectID       string
	AgentID         int64
	ThreadID        string
	Kind            string
	Status          string
	Query           string
	Period          string
	Since           time.Time
	Limit           int
	IncludePayloads bool
	IncludeRaw      bool
}

type AgentActivityResponse struct {
	Actions []AgentActivityAction `json:"actions"`
	Counts  map[string]int        `json:"counts"`
	Window  map[string]string     `json:"window"`
	Filters map[string]any        `json:"filters"`
}

type AgentActivityAction struct {
	ID         string           `json:"id"`
	Time       time.Time        `json:"time"`
	AgentID    int64            `json:"agent_id"`
	AgentName  string           `json:"agent_name"`
	ThreadID   string           `json:"thread_id"`
	Kind       string           `json:"kind"`   // thought, tool, thread, event, error
	Status     string           `json:"status"` // running, success, error, info
	Title      string           `json:"title"`
	Detail     string           `json:"detail,omitempty"`
	Thinking   string           `json:"thinking,omitempty"`
	Output     string           `json:"output,omitempty"`
	Args       string           `json:"args,omitempty"`
	Result     string           `json:"result,omitempty"`
	DurationMS int              `json:"duration_ms,omitempty"`
	Raw        []TelemetryEvent `json:"raw,omitempty"`
}

type agentActivityThoughtBuffer struct {
	Thinking string
	Output   string
}

func BuildAgentActivity(store *Store, userID int64, opts AgentActivityOptions) (AgentActivityResponse, error) {
	opts = normalizeAgentActivityOptions(opts)
	agents, err := resolveActivityAgents(store, userID, opts.ProjectID, opts.AgentID)
	if err != nil {
		return AgentActivityResponse{}, err
	}
	nameByID := map[int64]string{}
	var events []TelemetryEvent
	eventLimit := opts.Limit * 4
	if eventLimit < 100 {
		eventLimit = 100
	}
	if eventLimit > 1000 {
		eventLimit = 1000
	}
	for _, agent := range agents {
		nameByID[agent.ID] = agent.Name
		batch, err := store.QueryTelemetry(agent.ID, "", opts.Since, eventLimit, opts.ThreadID)
		if err != nil {
			return AgentActivityResponse{}, err
		}
		events = append(events, batch...)
	}
	actions := buildAgentActivityActions(events, nameByID, opts.IncludePayloads, opts.IncludeRaw)
	actions = filterAgentActivityActions(actions, opts)
	if len(actions) > opts.Limit {
		actions = actions[:opts.Limit]
	}
	counts := map[string]int{"total": len(actions)}
	for _, action := range actions {
		counts[action.Kind]++
		counts[action.Status]++
	}
	return AgentActivityResponse{
		Actions: actions,
		Counts:  counts,
		Window: map[string]string{
			"since":  opts.Since.UTC().Format(time.RFC3339),
			"until":  time.Now().UTC().Format(time.RFC3339),
			"period": opts.Period,
		},
		Filters: map[string]any{
			"project_id": opts.ProjectID,
			"agent_id":   opts.AgentID,
			"thread_id":  opts.ThreadID,
			"kind":       opts.Kind,
			"status":     opts.Status,
			"query":      opts.Query,
			"limit":      opts.Limit,
		},
	}, nil
}

func normalizeAgentActivityOptions(opts AgentActivityOptions) AgentActivityOptions {
	opts.Kind = strings.ToLower(strings.TrimSpace(opts.Kind))
	if opts.Kind == "" {
		opts.Kind = "all"
	}
	opts.Status = strings.ToLower(strings.TrimSpace(opts.Status))
	if opts.Status == "" {
		opts.Status = "all"
	}
	opts.Query = strings.ToLower(strings.TrimSpace(opts.Query))
	opts.ThreadID = strings.TrimSpace(opts.ThreadID)
	opts.ProjectID = strings.TrimSpace(opts.ProjectID)
	opts.Period = strings.TrimSpace(opts.Period)
	if opts.Period == "" {
		opts.Period = "24h"
	}
	if opts.Since.IsZero() {
		opts.Since = agentActivitySince(opts.Period)
	}
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Limit > 320 {
		opts.Limit = 320
	}
	return opts
}

func agentActivitySince(period string) time.Time {
	if d, err := time.ParseDuration(period); err == nil {
		return time.Now().Add(-d)
	}
	return parsePeriod(period)
}

func resolveActivityAgents(store *Store, userID int64, projectID string, agentID int64) ([]Agent, error) {
	if agentID != 0 {
		agent, err := store.GetAgent(userID, agentID)
		if err != nil {
			return nil, fmt.Errorf("agent not found")
		}
		if projectID != "" && agent.ProjectID != projectID {
			return nil, fmt.Errorf("agent %d is not in project %q", agentID, projectID)
		}
		return []Agent{*agent}, nil
	}
	agents, err := store.ListAgents(userID, projectID)
	if err != nil {
		return nil, err
	}
	return agents, nil
}

func buildAgentActivityActions(events []TelemetryEvent, nameByID map[int64]string, includePayloads, includeRaw bool) []AgentActivityAction {
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	rows := map[string]AgentActivityAction{}
	order := []string{}
	activeThoughtByThread := map[string]string{}
	thoughtBuffers := map[string]*agentActivityThoughtBuffer{}

	put := func(key string, patch AgentActivityAction, raw TelemetryEvent) {
		current, ok := rows[key]
		if !ok {
			if patch.ID == "" {
				patch.ID = key
			}
			if patch.Time.IsZero() {
				patch.Time = raw.Time
			}
			if patch.AgentName == "" {
				patch.AgentName = nameByID[patch.AgentID]
				if patch.AgentName == "" && patch.AgentID != 0 {
					patch.AgentName = fmt.Sprintf("agent %d", patch.AgentID)
				}
			}
			if patch.ThreadID == "" {
				patch.ThreadID = "main"
			}
			if patch.Kind == "" {
				patch.Kind = "event"
			}
			if patch.Status == "" {
				patch.Status = "info"
			}
			if patch.Title == "" {
				patch.Title = "event"
			}
			if includeRaw && raw.ID != "" {
				patch.Raw = []TelemetryEvent{raw}
			}
			rows[key] = patch
			order = append(order, key)
			return
		}
		if !patch.Time.IsZero() && patch.Time.After(current.Time) {
			current.Time = patch.Time
		}
		if patch.Kind != "" {
			current.Kind = patch.Kind
		}
		if patch.Status != "" {
			current.Status = patch.Status
		}
		if patch.Title != "" {
			current.Title = patch.Title
		}
		if patch.Detail != "" {
			current.Detail = patch.Detail
		}
		if patch.Thinking != "" {
			current.Thinking = patch.Thinking
		}
		if patch.Output != "" {
			current.Output = patch.Output
		}
		if patch.Args != "" {
			current.Args = patch.Args
		}
		if patch.Result != "" {
			current.Result = patch.Result
		}
		if patch.DurationMS != 0 {
			current.DurationMS = patch.DurationMS
		}
		if includeRaw && raw.ID != "" {
			current.Raw = append(current.Raw, raw)
			if len(current.Raw) > 8 {
				current.Raw = current.Raw[len(current.Raw)-8:]
			}
		}
		rows[key] = current
	}

	for _, ev := range events {
		data := agentActivityData(ev)
		agentName := nameByID[ev.AgentID]
		if agentName == "" {
			agentName = fmt.Sprintf("agent %d", ev.AgentID)
		}
		threadID := ev.ThreadID
		if threadID == "" {
			threadID = "main"
		}
		iteration := agentActivityString(data["iteration"])
		threadKey := fmt.Sprintf("%d:%s", ev.AgentID, threadID)

		switch ev.Type {
		case "llm.start":
			key := agentActivityThoughtKey(ev.AgentID, threadID, activityFirstNonEmpty(iteration, ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10)))
			activeThoughtByThread[threadKey] = key
			if thoughtBuffers[key] == nil {
				thoughtBuffers[key] = &agentActivityThoughtBuffer{}
			}
			put(key, AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thought", Status: "running", Title: "Thinking",
				Detail: compactActivityText(activityFirstNonEmpty(agentActivityString(data["model"]), agentActivityString(data["provider"]))),
			}, ev)
		case "llm.thinking", "llm.chunk":
			key := activeThoughtByThread[threadKey]
			if iteration != "" {
				key = agentActivityThoughtKey(ev.AgentID, threadID, iteration)
			}
			if key == "" {
				key = agentActivityThoughtKey(ev.AgentID, threadID, "live")
			}
			activeThoughtByThread[threadKey] = key
			buf := thoughtBuffers[key]
			if buf == nil {
				buf = &agentActivityThoughtBuffer{}
				thoughtBuffers[key] = buf
			}
			chunk := agentActivityString(firstExisting(data, "text", "chunk", "delta"))
			if ev.Type == "llm.thinking" {
				buf.Thinking += chunk
			} else {
				buf.Output += chunk
			}
			action := AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thought", Status: "running", Title: "Thinking",
				Detail: shortActivityText(compactActivityText(activityFirstNonEmpty(buf.Output, buf.Thinking)), 260),
			}
			if includePayloads {
				action.Thinking = strings.TrimSpace(buf.Thinking)
				action.Output = strings.TrimSpace(buf.Output)
			}
			put(key, action, ev)
		case "llm.done":
			key := activeThoughtByThread[threadKey]
			if iteration != "" {
				key = agentActivityThoughtKey(ev.AgentID, threadID, iteration)
			}
			if key == "" {
				key = agentActivityThoughtKey(ev.AgentID, threadID, activityFirstNonEmpty(ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10)))
			}
			buf := thoughtBuffers[key]
			if buf == nil {
				buf = &agentActivityThoughtBuffer{}
				thoughtBuffers[key] = buf
			}
			message := compactActivityText(firstExisting(data, "message", "summary", "model"))
			if strings.TrimSpace(buf.Output) == "" && message != "" {
				buf.Output = message
			}
			delete(activeThoughtByThread, threadKey)
			detail := strings.Join(nonEmptyStrings(shortActivityText(activityFirstNonEmpty(message, compactActivityText(activityFirstNonEmpty(buf.Output, buf.Thinking))), 260), activityTokenSummary(data)), " - ")
			action := AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thought", Status: "success", Title: "Reasoning step", Detail: detail,
				DurationMS: intFromAny(data["duration_ms"]),
			}
			if includePayloads {
				action.Thinking = strings.TrimSpace(buf.Thinking)
				action.Output = strings.TrimSpace(buf.Output)
			}
			put(key, action, ev)
		case "llm.error", "error":
			put(fmt.Sprintf("error:%d:%s:%s", ev.AgentID, threadID, activityFirstNonEmpty(ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10))), AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "error", Status: "error", Title: activityFirstNonEmpty(map[bool]string{true: "LLM error", false: "Error"}[ev.Type == "llm.error"]),
				Detail: compactActivityText(firstExisting(data, "error", "message")),
			}, ev)
		case "llm.tool_chunk":
			tool := agentActivityString(firstExisting(data, "tool", "name"))
			if isHiddenActivityTool(tool) {
				continue
			}
			callID := agentActivityCallID(data, ev)
			chunk := agentActivityString(firstExisting(data, "chunk", "delta", "text"))
			action := AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "tool", Status: "running", Title: "Preparing " + shortActivityToolName(tool), Detail: tool,
			}
			if includePayloads {
				action.Args = formatActivityPayload(chunk, 4000)
			}
			put(fmt.Sprintf("tool:%d:%s:%s", ev.AgentID, threadID, callID), action, ev)
		case "tool.call":
			tool := agentActivityString(firstExisting(data, "name", "tool"))
			if isHiddenActivityTool(tool) {
				continue
			}
			callID := agentActivityCallID(data, ev)
			reason := compactActivityText(data["reason"])
			action := AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "tool", Status: "running", Title: activityFirstNonEmpty(reason, "Running "+shortActivityToolName(tool)), Detail: tool,
			}
			if includePayloads {
				action.Args = formatActivityPayload(activityArgsValue(data), 5000)
			}
			put(fmt.Sprintf("tool:%d:%s:%s", ev.AgentID, threadID, callID), action, ev)
		case "tool.result":
			tool := agentActivityString(firstExisting(data, "name", "tool"))
			if isHiddenActivityTool(tool) {
				continue
			}
			callID := agentActivityCallID(data, ev)
			failed := boolFromAny(data["is_error"]) || data["success"] == false
			action := AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "tool", Status: map[bool]string{true: "error", false: "success"}[failed],
				Title:      shortActivityToolName(tool) + map[bool]string{true: " failed", false: " completed"}[failed],
				Detail:     activityFirstNonEmpty(compactActivityText(firstExisting(data, "error", "message")), tool),
				DurationMS: intFromAny(data["duration_ms"]),
			}
			if includePayloads {
				action.Result = formatActivityPayload(activityResultValue(data), 5000)
			}
			put(fmt.Sprintf("tool:%d:%s:%s", ev.AgentID, threadID, callID), action, ev)
		case "thread.spawn":
			spawned := agentActivityString(firstExisting(data, "thread_id", "id", "name"))
			title := "Spawned thread"
			if strings.TrimSpace(spawned) != "" {
				title = "Spawned " + spawned
			}
			put(fmt.Sprintf("thread:%d:%s:spawn:%s", ev.AgentID, threadID, activityFirstNonEmpty(spawned, ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10))), AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thread", Status: "running", Title: title,
				Detail: compactActivityText(firstExisting(data, "directive", "prompt")),
			}, ev)
		case "thread.message":
			put(fmt.Sprintf("thread:%d:%s:message:%s", ev.AgentID, threadID, activityFirstNonEmpty(ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10))), AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thread", Status: "info", Title: "Thread message",
				Detail: compactActivityText(firstExisting(data, "message", "text")),
			}, ev)
		case "thread.done":
			put(fmt.Sprintf("thread:%d:%s:done:%s", ev.AgentID, threadID, activityFirstNonEmpty(ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10))), AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "thread", Status: "success", Title: "Thread done",
				Detail: compactActivityText(firstExisting(data, "result", "message")),
			}, ev)
		case "event.received":
			put(fmt.Sprintf("event:%d:%s:%s", ev.AgentID, threadID, activityFirstNonEmpty(ev.ID, strconv.FormatInt(ev.Time.UnixNano(), 10))), AgentActivityAction{
				AgentID: ev.AgentID, AgentName: agentName, ThreadID: threadID, Time: ev.Time,
				Kind: "event", Status: "info", Title: activityFirstNonEmpty(compactActivityText(data["source"]), "Incoming event"),
				Detail: compactActivityText(firstExisting(data, "message", "text", "event")),
			}, ev)
		}
	}

	out := make([]AgentActivityAction, 0, len(order))
	for _, key := range order {
		if row, ok := rows[key]; ok {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func filterAgentActivityActions(actions []AgentActivityAction, opts AgentActivityOptions) []AgentActivityAction {
	out := actions[:0]
	for _, action := range actions {
		if opts.Kind != "" && opts.Kind != "all" && action.Kind != opts.Kind {
			continue
		}
		if opts.Status != "" && opts.Status != "all" && action.Status != opts.Status {
			continue
		}
		if opts.Query != "" {
			haystack := strings.ToLower(strings.Join([]string{
				action.AgentName, action.ThreadID, action.Kind, action.Status, action.Title, action.Detail, action.Args, action.Result, action.Thinking, action.Output,
			}, " "))
			if !strings.Contains(haystack, opts.Query) {
				continue
			}
		}
		out = append(out, action)
	}
	return out
}

func agentActivityData(ev TelemetryEvent) map[string]any {
	var data map[string]any
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data
}

func isHiddenActivityTool(tool string) bool {
	tool = strings.ToLower(strings.TrimSpace(tool))
	switch tool {
	case "pace", "done", "channels_status", "channels_respond", "channels_send", "channels_publish", "channels_set_status":
		return true
	default:
		return strings.HasSuffix(tool, "_set_status") ||
			strings.HasSuffix(tool, "_publish") ||
			strings.HasSuffix(tool, "_notify") ||
			strings.HasSuffix(tool, "_list_channels")
	}
}

func agentActivityCallID(data map[string]any, ev TelemetryEvent) string {
	return activityFirstNonEmpty(
		agentActivityString(data["id"]),
		agentActivityString(data["call_id"]),
		agentActivityString(data["tool_call_id"]),
		agentActivityString(data["index"]),
		ev.ID,
		ev.Type+":"+ev.Time.UTC().Format(time.RFC3339Nano),
	)
}

func agentActivityThoughtKey(agentID int64, threadID, suffix string) string {
	return fmt.Sprintf("thought:%d:%s:%s", agentID, threadID, suffix)
}

func firstExisting(data map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			return value
		}
	}
	return nil
}

func activityFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nonEmptyStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func agentActivityString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func compactActivityText(value any) string {
	return strings.Join(strings.Fields(agentActivityString(value)), " ")
}

func shortActivityText(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max-1] + "..."
}

func shortActivityToolName(tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return "tool"
	}
	parts := strings.FieldsFunc(tool, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == '/' })
	if len(parts) == 0 {
		return tool
	}
	last := parts[len(parts)-1]
	if last == "" {
		return tool
	}
	return strings.ToUpper(last[:1]) + last[1:]
}

func activityTokenSummary(data map[string]any) string {
	in := firstExisting(data, "tokens_in", "prompt_tokens")
	out := firstExisting(data, "tokens_out", "completion_tokens")
	cost, hasCost := floatFromAny(data["cost_usd"])
	tokens := ""
	if in != nil || out != nil {
		tokens = fmt.Sprintf("%d in / %d out", intFromAny(in), intFromAny(out))
	}
	if hasCost {
		if tokens != "" {
			return fmt.Sprintf("%s $%.4f", tokens, cost)
		}
		return fmt.Sprintf("$%.4f", cost)
	}
	return tokens
}

func activityArgsValue(data map[string]any) any {
	for _, key := range []string{"args", "arguments", "input", "params"} {
		if value, ok := data[key]; ok {
			return value
		}
	}
	return nil
}

func activityResultValue(data map[string]any) any {
	for _, key := range []string{"result", "output", "error", "message"} {
		if value, ok := data[key]; ok {
			return value
		}
	}
	return nil
}

func formatActivityPayload(value any, max int) string {
	if value == nil {
		return ""
	}
	sanitized := sanitizeActivityPayload(value, 0)
	var text string
	if s, ok := sanitized.(string); ok {
		text = s
	} else if data, err := json.MarshalIndent(sanitized, "", "  "); err == nil {
		text = string(data)
	} else {
		text = fmt.Sprintf("%v", sanitized)
	}
	if max > 0 && len(text) > max {
		return fmt.Sprintf("%s\n... (%s chars total)", text[:max], strconv.Itoa(len(text)))
	}
	return text
}

func sanitizeActivityPayload(value any, depth int) any {
	if s, ok := value.(string); ok {
		var parsed any
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			if json.Unmarshal([]byte(trimmed), &parsed) == nil {
				return sanitizeActivityPayload(parsed, depth+1)
			}
		}
		if len(s) > 600 {
			return fmt.Sprintf("%s... (%s chars)", s[:600], strconv.Itoa(len(s)))
		}
		return s
	}
	if value == nil {
		return nil
	}
	if depth > 5 {
		return "[nested object]"
	}
	switch v := value.(type) {
	case []any:
		limit := len(v)
		if limit > 20 {
			limit = 20
		}
		out := make([]any, 0, limit+1)
		for i := 0; i < limit; i++ {
			out = append(out, sanitizeActivityPayload(v[i], depth+1))
		}
		if len(v) > limit {
			out = append(out, fmt.Sprintf("... %d more items", len(v)-limit))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for key, val := range v {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "base64") || strings.Contains(lower, "screenshot") || strings.Contains(lower, "image") || strings.Contains(lower, "audio") || strings.Contains(lower, "blob") {
				if str, ok := val.(string); ok {
					out[key] = fmt.Sprintf("[%s chars omitted]", strconv.Itoa(len(str)))
				} else {
					out[key] = sanitizeActivityPayload(val, depth+1)
				}
				continue
			}
			out[key] = sanitizeActivityPayload(val, depth+1)
		}
		return out
	default:
		return value
	}
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := strconv.Atoi(v.String())
		return i
	default:
		return 0
	}
}

func floatFromAny(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := strconv.ParseFloat(v.String(), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func boolFromAny(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}
