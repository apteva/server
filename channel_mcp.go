package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apteva/server/apps/framework"
)

// channelMCPServer is an HTTP MCP server exposing unified channel tools for core.
// Runs per-instance in the server process.
type channelMCPServer struct {
	port     int
	listener net.Listener
	registry *ChannelRegistry
	ic       *AgentChannels // parent — for listing available channels

	// componentCatalog returns the UI components installed apps in
	// this instance's project declare. Used to enumerate them in the
	// `respond` tool description (so the agent learns what's
	// renderable without a separate discovery call) and to back the
	// `components_list` tool. Optional — when nil the description
	// degrades to a generic "available components depend on installed
	// apps" line, same as v1.
	componentCatalog func() []componentEntry

	mu     sync.Mutex
	closed bool
}

// componentEntry is the flat (app, name, slots, description) row
// the chat MCP advertises to the agent. Decoupled from sdk.UIComponent
// so we can also expose human-readable display_name/description from
// the surrounding manifest without leaking the whole manifest type.
type componentEntry struct {
	App         string         `json:"app"`
	Name        string         `json:"name"`
	Slots       []string       `json:"slots"`
	Description string         `json:"description,omitempty"`
	PropsSchema map[string]any `json:"props_schema,omitempty"`
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
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    256 << 10,
	}
	_ = server.Serve(s.listener)
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
	// Use RegisteredChannels, NOT AvailableChannels. MCP clients cache
	// tools/list from the initialize handshake and never re-fetch — if
	// we emit a description that says "CONNECTED CHANNELS: [none]" at
	// boot (before any dashboard has opened chat), that cached line
	// follows the agent forever. The call-time gate in handleToolCall
	// still rejects dead channels with a clean error, so nothing is
	// lost on correctness; we just stop lying to the agent about
	// which channels exist as targets.
	var channelIDs []string
	if s.ic != nil {
		channelIDs = s.ic.RegisteredChannels()
	} else {
		for _, ch := range s.registry.List() {
			channelIDs = append(channelIDs, ch.ID())
		}
	}
	var components []componentEntry
	if s.componentCatalog != nil {
		components = s.componentCatalog()
	}

	return map[string]any{
		"tools": []map[string]any{
			{
				"name":        "send",
				"description": buildSendDescription(channelIDs, components),
				"_meta": map[string]any{
					"io.apteva/wakeOnResult": "always",
				},
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"kind", "channel"},
					"properties": map[string]any{
						"kind":    map[string]any{"type": "string", "description": "Payload kind: message, status, approval, report, or alert."},
						"channel": map[string]any{"type": "string", "description": "Target channel. Use \"current\" for the channel that triggered the event, \"apteva\" for Apteva operator messages/statuses/approvals/reports/alerts, or Telegram/Slack/etc ids from list_channels. Legacy \"chat\" is accepted as an alias for \"apteva\"."},
						"text":    map[string]any{"type": "string", "description": "User-visible message body for kind=message. Plain assistant output/thoughts are internal and invisible."},
						"title":   map[string]any{"type": "string", "description": "Short title for kind=status, kind=approval, kind=report, or kind=alert."},
						"body":    map[string]any{"type": "string", "description": "Body/details for kind=approval or kind=alert."},
						"summary": map[string]any{"type": "string", "description": "Compact outcome summary for kind=report."},
						"severity": map[string]any{
							"type":        "string",
							"description": "Optional alert severity: info, warning, error, or critical.",
						},
						"state": map[string]any{
							"type":        "string",
							"description": "Current status state for kind=status: working, waiting, blocked, or completed.",
						},
						"detail":   map[string]any{"type": "string", "description": "Optional concise detail for kind=status."},
						"progress": map[string]any{"type": "number", "minimum": 0, "maximum": 100, "description": "Optional completion percentage for kind=status."},
						"components": map[string]any{
							"type":        "array",
							"description": "Optional rich attachments for kind=message — see AVAILABLE COMPONENTS in the description. External channels ignore this field; the Apteva channel renders attachments.",
							"items": map[string]any{
								"type":     "object",
								"required": []string{"app", "name"},
								"properties": map[string]any{
									"app":   map[string]any{"type": "string", "description": "Installed app's slug, e.g. \"storage\"."},
									"name":  map[string]any{"type": "string", "description": "Component name from that app's manifest, e.g. \"file-card\"."},
									"props": map[string]any{"type": "object", "description": "Forwarded to the component verbatim."},
								},
							},
						},
						"actions": map[string]any{
							"type":        "array",
							"description": "Optional button list for kind=approval. Defaults to Approve and Deny.",
							"items": map[string]any{
								"type":     "object",
								"required": []string{"id", "label"},
								"properties": map[string]any{
									"id":    map[string]any{"type": "string", "description": "Stable action id, e.g. approve or deny."},
									"label": map[string]any{"type": "string", "description": "Button label."},
									"style": map[string]any{"type": "string", "description": "Optional visual style: primary, danger, neutral."},
								},
							},
						},
						"period": map[string]any{"type": "string", "description": "Optional period such as today, yesterday, past_week, daily, weekly, or an ISO date range."},
						"sections": map[string]any{
							"type":        "array",
							"description": "Optional expanded details for the report modal. Each item is {title, body}. Use sections for completed work, findings, risks, next steps, metrics, evidence, links, or chronology. Do not rely on sections to replace the summary.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"title": map[string]any{"type": "string"},
									"body":  map[string]any{"type": "string"},
								},
							},
						},
						"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional tags like daily, weekly, milestone, incident, activity."},
						"context": map[string]any{"type": "object", "description": "Optional structured context for approval/report/alert follow-up."},
					},
				},
			},
			{
				"name":        "list_channels",
				"description": "List communication channels and their capabilities. For normal Apteva operator replies, send kind=message to channel=\"current\" or channel=\"apteva\". Use kind=status for current monitoring state, or kind=approval/report/alert for structured inbox artifacts, on channel=\"apteva\".",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
}

// buildSendDescription emits a send-tool description whose
// routing examples are filtered to ONLY the currently connected
// channels. The previous static description listed every possible
// channel (cli, apteva, telegram, slack, email) even when only one
// was live — LLMs treat examples as strong priors and would call
// respond(channel="cli") even with Apteva as the sole connected
// channel, because "cli" appeared right there in the tool doc.
// Dynamic examples kill that failure mode: if the agent sees only
// [apteva] as a valid channel in the docs, it calls channel="apteva".
// propsSchemaHint renders a component's props_schema as a compact
// inline string the agent can read at a glance, e.g.
// "{file_id*: integer, compact?: boolean}". Required keys get a `*`,
// optional keys get a `?`. Returns "" when the schema is empty or
// shaped unexpectedly — degrades gracefully so a malformed schema
// just hides the hint instead of polluting the description.
func propsSchemaHint(schema map[string]any) string {
	if schema == nil {
		return ""
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	if reqArr, ok := schema["required"].([]any); ok {
		for _, r := range reqArr {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		typ := ""
		if def, ok := props[k].(map[string]any); ok {
			if t, ok := def["type"].(string); ok {
				typ = t
			}
		}
		marker := "?"
		if required[k] {
			marker = "*"
		}
		entry := k + marker
		if typ != "" {
			entry += ": " + typ
		}
		parts = append(parts, entry)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func buildSendDescription(channelIDs []string, components []componentEntry) string {
	var examples []string
	for _, id := range channelIDs {
		// Strip cosmetic telegram suffixes for the example.
		raw := id
		if i := strings.Index(raw, " "); i > 0 {
			raw = raw[:i]
		}
		raw = normalizeChannelID(raw)
		switch {
		case raw == "cli":
			examples = append(examples, `[cli] → channel="cli"`)
		case raw == "apteva":
			examples = append(examples, `[apteva] → channel="apteva"`)
		case strings.HasPrefix(raw, "telegram"):
			examples = append(examples, `[telegram:@user:12345] → channel="telegram:12345" (digits only)`)
		case strings.HasPrefix(raw, "slack:"):
			examples = append(examples, fmt.Sprintf(`[slack:user:%s] → channel="%s" (C-prefixed id only, not the username)`, raw[len("slack:"):], raw))
		case strings.HasPrefix(raw, "email:"):
			examples = append(examples, fmt.Sprintf(`[email:user@example.com] → channel="%s"`, raw))
		default:
			examples = append(examples, fmt.Sprintf(`channel="%s"`, raw))
		}
	}
	connectedList := strings.Join(canonicalChannelIDs(channelIDs), ", ")
	if connectedList == "" {
		connectedList = "none"
	}
	examplesLine := strings.Join(examples, "; ")
	if examplesLine == "" {
		examplesLine = "(none — no channels currently accept responses; see DIRECTIVES rule below)"
	}

	// AVAILABLE COMPONENTS — surfaced to the agent the same way
	// channel routing is. Each turn the description is regenerated
	// with the live catalog, so the agent never has to call a
	// separate discovery tool. Filtered to components that include
	// chat.message_attachment in their slot allowlist (the only
	// slot `respond` can render into).
	var componentLines []string
	for _, c := range components {
		allowed := false
		for _, slot := range c.Slots {
			if slot == "chat.message_attachment" {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		line := fmt.Sprintf("  - {app:%q, name:%q}", c.App, c.Name)
		if propsHint := propsSchemaHint(c.PropsSchema); propsHint != "" {
			line += " props=" + propsHint
		}
		if c.Description != "" {
			line += " — " + c.Description
		}
		componentLines = append(componentLines, line)
	}
	componentsBlock := ""
	if len(componentLines) > 0 {
		componentsBlock = "\n\nAVAILABLE COMPONENTS for the optional `components` arg (Apteva channel only — external channels strip them):\n" +
			strings.Join(componentLines, "\n") +
			"\nWHEN TO ATTACH (default ON for these cases — do NOT wait to be asked):\n" +
			"  - The reply is about a specific file, image, video, or media item the user can view → attach the matching card with the item's id in props.\n" +
			"  - You looked up an entity (file, post, document) and are reporting metadata about it → attach the card alongside or instead of a text dump.\n" +
			"  - The user said \"show\", \"display\", \"preview\", \"render\" — always attach.\n" +
			"Plain status updates, error messages, and pure conversation do NOT need a component.\n" +
			"Format: components=[{app:\"<app>\", name:\"<component-name>\", props:{<props>}}]. Send the text AND components in the same kind=\"message\" call — never a text-only message followed by a component-only message (that double-pings the user)."
	}

	return fmt.Sprintf(
		"Send typed communication through Apteva Channels. Text in your thoughts is INVISIBLE — only this tool delivers messages or durable channel artifacts.\n\n"+
			"KINDS:\n"+
			"- kind=\"message\": visible user-facing message to a channel such as current/apteva/telegram. Use this for live conversation, immediate progress, and final answers while the operator is actively chatting.\n"+
			"- kind=\"status\": current monitoring headline for what you are doing now. Use channel=\"apteva\". Set working before meaningful multi-step work and before any substantive external action, even a single create/update/delete/send/publish/trigger tool call. When work can start immediately, call working status and the first action tool in the same parallel tool-call batch; do not wait for the status result. Never parallelize past a required approval or prerequisite. Update status at phase changes, when waiting/blocked, and set completed after the action result or work finishes. It replaces your previous status and never appears in chat or Inbox. Skip status for read-only lookups, brief answers, internal pacing, and individual tool calls within one stated phase.\n"+
			"- kind=\"approval\": Apteva approval request with buttons. Use channel=\"apteva\".\n"+
			"- kind=\"report\": Apteva dashboard report. Use channel=\"apteva\". Use this for requested delayed/background checks, scheduled summaries, significant completed work, and anything the operator asked to review later, especially after a dashboard chat disconnect.\n"+
			"- kind=\"alert\": Apteva operator alert. Use channel=\"apteva\".\n\n"+
			"APTEVA OPERATOR CHANNEL:\n"+
			"- channel=\"apteva\" is the internal Apteva channel for talking to operators. It is durable: messages are saved even when the user is offline.\n"+
			"- channel=\"current\" resolves to the channel that triggered the event. For dashboard chat events, current resolves to apteva.\n"+
			"- Legacy channel=\"chat\" is accepted as an alias for channel=\"apteva\", but prefer apteva.\n"+
			"- Use kind=\"message\" for normal conversation, followups, and completion notes while the operator is currently chatting.\n"+
			"- If you receive a chat disconnect before finishing delayed work, do not send the completed check as kind=\"message\". Send a kind=\"report\" artifact unless the user explicitly requested a normal message.\n"+
			"- Use approval/report/alert for structured inbox artifacts. Reports should state what actually happened and omit routine chat/connect/disconnect/idle events.\n"+
			"- A successful message send wakes you again, so visible messages do not end the work loop.\n"+
			"- If you promised work, continue after the send result: call the needed tools, schedule yourself with pace, or explain why blocked.\n"+
			"- If the request is fully answered, use the follow-up turn to pace/done/wait normally.\n"+
			"- After action tool results arrive, send another kind=\"message\" with the actual outcome before pace/done/wait. \"Done\" alone is not enough; include what you did or found.\n\n"+
			"KNOWN CHANNELS (valid values for channel): [%s].\n"+
			"Routing — match the event prefix to the channel: %s.\n\n"+
			"If the gate rejects your message channel as unknown, retry with a channel from the list above — NOT to fall silent. Do NOT default to \"cli\" from training-data prior; use exactly the names listed.\n\n"+
			"DIRECTIVES vs MESSAGES: events whose tag does NOT correspond to a known live channel above — e.g. [admin], [system], [inject], or a bare untagged event — are DIRECTIVES from an operator, not user messages. Act on them but do NOT send a live message unless the task needs a durable Apteva artifact.%s",
		connectedList, examplesLine, componentsBlock,
	)
}

func canonicalChannelIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		canonical := normalizeChannelID(id)
		if canonical == "" || seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}

func channelLabel(id string) string {
	switch id {
	case "apteva":
		return "Apteva"
	default:
		return id
	}
}

// extractComponents pulls a []ChatComponent out of the agent's
// `respond` arguments. Tolerant of missing / wrong shapes — any
// entry that doesn't have both `app` and `name` is silently
// dropped so a malformed component doesn't tank the whole
// respond call (the text still goes through).
func extractComponents(raw any) []framework.ChatComponent {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]framework.ChatComponent, 0, len(arr))
	for _, v := range arr {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		app, _ := obj["app"].(string)
		name, _ := obj["name"].(string)
		if app == "" || name == "" {
			continue
		}
		props, _ := obj["props"].(map[string]any)
		out = append(out, framework.ChatComponent{
			App:   app,
			Name:  name,
			Props: props,
		})
	}
	return out
}

// channelInList reports whether `channel` (normalized) matches any
// entry in the available-channels list, after trimming the display
// suffix telegram channels carry (e.g. "telegram (bot @foo)" → "telegram").
// Needed so the gate in the respond handler can accept channel="chat"
// when AvailableChannels returned ["chat"] verbatim, and channel="telegram:123"
// when it returned "telegram (bot @mybot)".
func channelInList(channel string, available []string) bool {
	channel = normalizeChannelID(channel)
	for _, a := range available {
		a = normalizeChannelID(a)
		if a == channel {
			return true
		}
		if i := strings.Index(a, " "); i > 0 && a[:i] == channel {
			return true
		}
		// Accept the "telegram:123" vs "telegram" prefix case.
		if strings.HasPrefix(channel, a+":") {
			return true
		}
	}
	return false
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
	textToolError := func(text string) any {
		return map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": true,
		}
	}

	sendVisibleMessage := func(text, channel string, components []framework.ChatComponent) any {
		rawChannel := channel
		if channel == "current" {
			channel = s.resolveCurrentChannel()
			rawChannel = "current"
		}
		if text == "" {
			return textToolError("text required")
		}
		// Gate by the active channels list BEFORE attempting Send.
		// This makes the feedback loop loud when the agent picks a
		// channel that isn't in the connected list (e.g. defaulting
		// to "cli" from training). The error tells it exactly what
		// the valid options are, so the next turn's tool_result
		// becomes the correction signal the LLM needs.
		var connected []string
		if s.ic != nil {
			connected = s.ic.AvailableChannels()
		} else {
			for _, ch := range s.registry.List() {
				connected = append(connected, ch.ID())
			}
		}
		normalized := normalizeChannelID(channel)
		if channel == "" || !channelInList(normalized, connected) {
			// "Stay silent" only applies when the connected list is
			// EMPTY (no user reachable on any surface). When the list
			// has channels but the agent picked the wrong name, the
			// fix is to retry with a valid name — silence here would
			// strand the user with a question that never gets answered.
			var msg string
			if len(connected) == 0 {
				msg = fmt.Sprintf(
					"channel %q is invalid and no channel is currently connected — no user is reachable. "+
						"Treat the originating event as a directive and act silently for now.",
					rawChannel,
				)
			} else {
				msg = fmt.Sprintf(
					"channel %q is not in the currently connected channels %v. "+
						"Retry with a name from that list. Do NOT fall silent — the user is waiting for your reply.",
					rawChannel, connected,
				)
			}
			return textToolError(msg)
		}
		ch := s.registry.Get(normalized)
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", normalized))
		}
		if rich, ok := ch.(framework.RichSender); ok && len(components) > 0 {
			if err := rich.SendWithComponents(text, components); err != nil {
				return textToolError(err.Error())
			}
		} else {
			if err := ch.Send(text); err != nil {
				return textToolError(err.Error())
			}
		}
		return nil
	}

	resolveArtifactChannel := func(channel string) string {
		switch strings.TrimSpace(channel) {
		case "", "current", "apteva":
			return "apteva"
		default:
			return normalizeChannelID(channel)
		}
	}

	sendApproval := func(channel string) any {
		title, _ := call.Arguments["title"].(string)
		body, _ := call.Arguments["body"].(string)
		ch := s.registry.Get(resolveArtifactChannel(channel))
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", channel))
		}
		requester, ok := ch.(framework.ApprovalRequester)
		if !ok {
			return textToolError(fmt.Sprintf("channel %q does not support approval cards", channel))
		}
		result, err := requester.RequestApproval(framework.ApprovalRequest{
			Title:   title,
			Body:    body,
			Actions: extractApprovalActions(call.Arguments["actions"]),
			Context: extractObject(call.Arguments["context"]),
		})
		if err != nil {
			return textToolError(err.Error())
		}
		raw, _ := json.Marshal(result)
		return textResult("approval sent to Apteva channel: " + string(raw))
	}

	sendReport := func(channel string) any {
		title, _ := call.Arguments["title"].(string)
		summary, _ := call.Arguments["summary"].(string)
		period, _ := call.Arguments["period"].(string)
		ch := s.registry.Get(resolveArtifactChannel(channel))
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", channel))
		}
		sender, ok := ch.(framework.ReportSender)
		if !ok {
			return textToolError(fmt.Sprintf("channel %q does not support reports", channel))
		}
		result, err := sender.SendReport(framework.ReportRequest{
			Title:    title,
			Summary:  summary,
			Period:   period,
			Sections: extractReportSections(call.Arguments["sections"]),
			Tags:     extractStringList(call.Arguments["tags"]),
			Context:  extractObject(call.Arguments["context"]),
		})
		if err != nil {
			return textToolError(err.Error())
		}
		raw, _ := json.Marshal(result)
		return textResult("report sent to Apteva channel: " + string(raw))
	}

	sendAlert := func(channel string) any {
		title, _ := call.Arguments["title"].(string)
		body, _ := call.Arguments["body"].(string)
		severity, _ := call.Arguments["severity"].(string)
		if title == "" {
			title = "Alert"
		}
		if body == "" {
			body = title
		}
		if severity == "" {
			severity = "info"
		}
		ch := s.registry.Get(resolveArtifactChannel(channel))
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", channel))
		}
		if err := ch.Status(title+": "+body, severity); err != nil {
			return textToolError(err.Error())
		}
		return textResult("alert sent to Apteva channel")
	}

	sendStatus := func(channel string) any {
		title, _ := call.Arguments["title"].(string)
		detail, _ := call.Arguments["detail"].(string)
		state, _ := call.Arguments["state"].(string)
		var progress *float64
		if value, ok := call.Arguments["progress"].(float64); ok {
			progress = &value
		}
		ch := s.registry.Get(resolveArtifactChannel(channel))
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", channel))
		}
		sender, ok := ch.(framework.CurrentStatusSender)
		if !ok {
			return textToolError(fmt.Sprintf("channel %q does not support current status", channel))
		}
		result, err := sender.SetCurrentStatus(framework.CurrentStatusRequest{
			Title: title, Detail: detail, State: state, Progress: progress,
		})
		if err != nil {
			return textToolError(err.Error())
		}
		raw, _ := json.Marshal(result)
		return textResult("current status updated: " + string(raw))
	}

	switch call.Name {
	case "send":
		kind, _ := call.Arguments["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" {
			kind = "message"
		}
		channel, _ := call.Arguments["channel"].(string)
		switch kind {
		case "message":
			text, _ := call.Arguments["text"].(string)
			if errResult := sendVisibleMessage(text, channel, extractComponents(call.Arguments["components"])); errResult != nil {
				return errResult, nil
			}
			return textResult("delivered. Continue promised work, schedule with pace if needed, or pace/done if the request is complete."), nil
		case "status":
			return sendStatus(channel), nil
		case "approval":
			return sendApproval(channel), nil
		case "report":
			return sendReport(channel), nil
		case "alert":
			return sendAlert(channel), nil
		default:
			return textToolError(fmt.Sprintf("unknown channels send kind %q", kind)), nil
		}

	case "respond":
		text, _ := call.Arguments["text"].(string)
		channel, _ := call.Arguments["channel"].(string)
		// Extract components if the agent attached any. When present
		// AND the channel implements framework.RichSender (channelchat
		// does; cli/slack/email/telegram don't), deliver them
		// alongside the text. Otherwise fall back to plain Send so
		// channels without rich rendering still get the text.
		components := extractComponents(call.Arguments["components"])
		if errResult := sendVisibleMessage(text, channel, components); errResult != nil {
			return errResult, nil
		}
		return textResult("delivered. Continue promised work, schedule with pace if needed, or pace/done if the request is complete."), nil

	case "request_approval":
		channel, _ := call.Arguments["channel"].(string)
		return sendApproval(channel), nil

	case "send_report":
		channel, _ := call.Arguments["channel"].(string)
		return sendReport(channel), nil

	case "list_channels":
		raw, _ := json.Marshal(s.channelCapabilityList())
		return textResult(string(raw)), nil

	default:
		return nil, &mcpRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", call.Name)}
	}
}

func (s *channelMCPServer) resolveCurrentChannel() string {
	var available []string
	if s.ic != nil {
		available = s.ic.AvailableChannels()
	} else {
		for _, ch := range s.registry.List() {
			available = append(available, ch.ID())
		}
	}
	for _, id := range available {
		if normalizeChannelID(id) == "apteva" {
			return "apteva"
		}
	}
	if len(available) == 1 {
		id := available[0]
		if i := strings.Index(id, " "); i > 0 {
			id = id[:i]
		}
		return id
	}
	return ""
}

func (s *channelMCPServer) channelCapabilityList() []map[string]any {
	available := map[string]bool{}
	var availableList []string
	if s.ic != nil {
		availableList = s.ic.AvailableChannels()
	} else {
		for _, ch := range s.registry.List() {
			availableList = append(availableList, ch.ID())
		}
	}
	for _, id := range availableList {
		id = normalizeChannelID(id)
		available[id] = true
		if i := strings.Index(id, " "); i > 0 {
			available[id[:i]] = true
		}
	}
	var ids []string
	if s.ic != nil {
		ids = s.ic.RegisteredChannels()
	} else {
		for _, ch := range s.registry.List() {
			ids = append(ids, ch.ID())
		}
	}
	sort.Strings(ids)
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = normalizeChannelID(id)
		if seen[id] {
			continue
		}
		seen[id] = true
		caps := []string{"message"}
		typ := "external"
		durable := false
		var online any
		if id == "apteva" {
			typ = "internal"
			durable = true
			online = available[id]
			caps = []string{"message", "status", "approval", "report", "alert", "buttons", "components"}
		}
		isAvailable := available[id]
		if id == "apteva" {
			isAvailable = true
		}
		out = append(out, map[string]any{
			"id":           id,
			"label":        channelLabel(id),
			"type":         typ,
			"available":    isAvailable,
			"online":       online,
			"durable":      durable,
			"capabilities": caps,
		})
	}
	return out
}

func extractReportSections(raw any) []framework.ReportSection {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]framework.ReportSection, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title, _ := obj["title"].(string)
		body, _ := obj["body"].(string)
		title = strings.TrimSpace(title)
		body = strings.TrimSpace(body)
		if title == "" && body == "" {
			continue
		}
		out = append(out, framework.ReportSection{Title: title, Body: body})
	}
	return out
}

func extractStringList(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func extractApprovalActions(raw any) []framework.ApprovalAction {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]framework.ApprovalAction, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := obj["id"].(string)
		label, _ := obj["label"].(string)
		style, _ := obj["style"].(string)
		id = strings.TrimSpace(id)
		label = strings.TrimSpace(label)
		if id == "" || label == "" {
			continue
		}
		out = append(out, framework.ApprovalAction{ID: id, Label: label, Style: strings.TrimSpace(style)})
	}
	return out
}

func extractObject(raw any) map[string]any {
	if obj, ok := raw.(map[string]any); ok {
		return obj
	}
	return nil
}

// normalizeChannelID strips extra prefix parts that agents include from
// event format: slack:user:C123 → slack:C123, telegram:@user:123 → telegram:123
func normalizeChannelID(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "chat" {
		return "apteva"
	}
	if strings.HasPrefix(channel, "slack:") {
		parts := strings.Split(channel, ":")
		if len(parts) == 3 {
			return "slack:" + parts[2]
		}
	}
	if strings.HasPrefix(channel, "telegram:") {
		parts := strings.Split(channel, ":")
		if len(parts) == 3 {
			return "telegram:" + parts[2]
		}
	}
	return channel
}
