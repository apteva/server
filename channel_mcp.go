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
					"required": []string{"channel", "text"},
					"properties": map[string]any{
						"channel": map[string]any{"type": "string", "description": "Target channel. Use \"current\" to reply where the event originated, \"apteva\" for the durable Apteva operator chat, or an id returned by list_channels."},
						"text":    map[string]any{"type": "string", "description": "Complete user-visible message. Thoughts and plain assistant output are invisible; put the actual reply here."},
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
					},
				},
			},
			{
				"name":        "publish",
				"description": buildPublishDescription(),
				"_meta": map[string]any{
					"io.apteva/wakeOnResult": "always",
				},
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"kind", "title", "content"},
					"properties": map[string]any{
						"kind": map[string]any{
							"type":        "string",
							"enum":        []string{"approval", "report", "alert"},
							"description": "Inbox artifact type: approval, report, or alert.",
						},
						"title":   map[string]any{"type": "string", "description": "Short, specific operator-facing title."},
						"content": map[string]any{"type": "string", "description": "Required substantive content. For reports, summarize what actually happened and the outcome; for approvals, state the decision needed; for alerts, explain the problem and impact."},
						"severity": map[string]any{
							"type":        "string",
							"enum":        []string{"info", "warning", "error", "critical"},
							"description": "Optional alert severity: info, warning, error, or critical.",
						},
						"actions": map[string]any{
							"type":        "array",
							"description": "Optional approval buttons. Defaults to Approve and Deny.",
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
						"period": map[string]any{"type": "string", "description": "Optional report period such as today, yesterday, past_week, daily, weekly, or an ISO date range."},
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
						"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional report tags such as daily, weekly, milestone, incident, or activity."},
						"context": map[string]any{"type": "object", "description": "Optional structured context for follow-up."},
					},
				},
			},
			{
				"name":        "set_status",
				"description": buildSetStatusDescription(),
				"_meta": map[string]any{
					"io.apteva/wakeOnResult": "always",
				},
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"title", "state"},
					"properties": map[string]any{
						"title": map[string]any{"type": "string", "description": "Concise current work unit or completed outcome. Never use a future action, waiting condition, or internal agent administration as the title."},
						"state": map[string]any{
							"type":        "string",
							"enum":        []string{"working", "waiting", "blocked", "completed"},
							"description": "Required state. waiting is an expected pause with a known resume condition such as time, approval, or an external job. blocked is an unexpected failure, missing access, or missing capability requiring corrective action.",
						},
						"detail":   map[string]any{"type": "string", "description": "Optional concise current phase, concrete result, dependency, or blocker for this work unit."},
						"progress": map[string]any{"type": "number", "minimum": 0, "maximum": 100, "description": "Optional completion percentage for this work unit. Never use waiting with 100 percent."},
						"next":     map[string]any{"type": "string", "description": "Optional nearest distinct operator-relevant responsibility after this work or phase. Do not use placeholders such as No pending work, restate the title, or record internal pacing."},
						"next_at":  map[string]any{"type": "string", "description": "Optional explicit or externally derived RFC3339 deadline for next. Include only when the same call contains a non-empty next. Never estimate it from current time, expected duration, the current work phase, or internal pacing."},
					},
				},
			},
			{
				"name":        "list_channels",
				"description": "List communication channels and their capabilities. Use send for ordinary messages, publish for Apteva approval/report/alert Inbox artifacts, and set_status for mutable monitoring state.",
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
			"Status updates, errors, and pure conversation do NOT need a component.\n" +
			"Format: components=[{app:\"<app>\", name:\"<component-name>\", props:{<props>}}]. Send text AND components in one call — never a text-only message followed by a component-only message (that double-pings the user)."
	}

	return fmt.Sprintf(
		"Send one complete user-visible message through a communication channel. Thoughts and plain assistant output are INVISIBLE; only this tool delivers chat text. Use publish for approvals/reports/alerts and set_status for mutable work state.\n\n"+
			"channel=\"current\" replies where the event originated. For dashboard chat it resolves to apteva. channel=\"apteva\" is durable and saves messages even while the operator is offline.\n"+
			"Use this tool for a direct [chat] turn and for the later outcome of work explicitly requested in that chat, even if the operator disconnected before completion. Outside a direct [chat] request, send ordinary chat only when the operator or directive explicitly asks for a chat message at that time. Do NOT send ordinary chat for autonomous or scheduled checks, routine monitoring, unchanged or no-op results, idle updates, repeated progress, connect/disconnect events, or internal/system events. A request to \"send a status update\" or \"update the status\" means set_status unless it explicitly asks for a chat message. For a recurring autonomous check with no meaningful change, set_status at most once if appropriate, call pace for the next due check, and remain silent; status next_at does not schedule a wake.\n"+
			"For a direct [chat] turn that requires tools, including dashboard conversation threads, prefer a short visible acknowledgement before beginning tool work so the operator knows the concrete next action. This is strong guidance, not a hard requirement: skip it when the complete answer can be sent immediately or an acknowledgement would be empty or repetitive. If you acknowledge, wait for this send to succeed before action tools; never parallelize the acknowledgement with them. After the promised work, send exactly one final outcome; the acknowledgement never replaces it. A tool-work turn has either one final message or two intentional messages—acknowledgement then final—and never more. For a durable handoff, prefer acknowledgement before send(main); if it was skipped, at most one acknowledgement may follow the successful handoff receipt, never both.\n"+
			"Every direct [chat] turn requires at least one successful call to this tool before pace, done, or going idle. The turn is incomplete until you send its visible answer. This also applies to clarifications, read-only lookup results, blockers, failures, and no-op outcomes. After any non-channel tool result used for the request, send the outcome or next question here before pacing; never leave it only in thoughts or plain assistant output.\n"+
			"A successful send wakes you again, but its result is only the delivery receipt for that exact message. Never repeat it. Continue only concrete unfinished work explicitly promised in an acknowledgement and send one final outcome afterward; otherwise pace/done. Build each message as one complete call and never send placeholders or duplicates.\n\n"+
			"KNOWN CHANNELS (valid values for channel): [%s].\n"+
			"Routing — match the event prefix to the channel: %s.\n\n"+
			"If the gate rejects your message channel as unknown, retry with a channel from the list above — NOT to fall silent. Do NOT default to \"cli\" from training-data prior; use exactly the names listed.\n\n"+
			"Events tagged [admin], [system], [inject], or with no known live channel are directives, not user messages. Act on them without sending chat unless communication is useful.%s",
		connectedList, examplesLine, componentsBlock,
	)
}

func buildPublishDescription() string {
	return `Publish one durable structured artifact to the Apteva operator Inbox. This is not chat and not current work status.

Every call requires kind, title, and content. Build one complete call; never publish placeholders or duplicates. The content field is always the artifact's substantive operator-facing text:
- report: a periodic digest of meaningful work across its reporting period, with concrete outcomes, important evidence or metrics, blockers, and next steps. Reports are not action receipts. Do not publish a report after each check, tool call, cleanup, or completed task; use set_status for work state and send for a requested task's direct outcome. Omit greetings, chat/connect/disconnect events, idle time, and internal reasoning. Add sections for useful detail, but content must still stand alone.
- approval: the exact decision needed, why it is needed, and the consequence of approving or denying it.
- alert: what went wrong, its impact, and the relevant next action.

Correct report example: {"kind":"report","title":"Daily work summary","content":"Imported 842 contacts, cleared 12 routine inbox items, and preserved 3 messages requiring review. Seventeen invalid contact rows remain for follow-up.","period":"today"}
Correct approval example: {"kind":"approval","title":"Approve production deploy","content":"Deploy version 1.4 now; this will restart two API workers."}
Correct alert example: {"kind":"alert","title":"CRM authentication expired","content":"Lead synchronization stopped after the CRM token expired; reconnect the integration to resume.","severity":"error"}

Follow an explicit operator request or directive when it defines report timing. Otherwise publish at most one unsolicited report per day, near the end of the operator's day, and only when meaningful work was completed since the previous report. Combine that day's work into one digest and use period=today. If no meaningful work was done, publish no report. Use approvals only when a decision is genuinely required. Use alerts only for important problems requiring attention.`
}

func buildSetStatusDescription() string {
	return `Set the agent's single mutable current-work status. Status is operational state: it replaces the previous status and never appears in chat or the Inbox. Every call requires both title and state.

When an autonomous instruction says to "send a status update" or "update the status" without explicitly requesting a chat message, use this tool rather than send. A recurring check with no meaningful change may replace status at most once for that completed phase and must not also create an ordinary chat message. After recording the completed recurring work, call pace for the next due check; next_at is display metadata and does not schedule a wake.

Status answers: what meaningful operator-relevant work is this agent actively doing, waiting on, blocked by, or most recently completed? Use it only for a work unit that is multi-step, long-running, or cannot currently continue. When qualifying work can start immediately, call set_status and the first action tool in the same parallel batch.

For qualifying work, always call this tool at meaningful phase changes; do not merely describe the state in thoughts or chat. If an event reports that qualifying work you performed has completed, begun waiting, or become blocked, update status even when no other action tool remains.

Use working while the work unit is actively executing. Use waiting only for an expected pause in that same unfinished work unit whose resume condition is known, including a scheduled time, operator approval, or an external job. Use blocked for an unexpected failure, missing access, or missing capability that requires corrective action before work can resume; do not use blocked for ordinary approval or a scheduled delay. Use completed after the meaningful work unit finishes. A future recurring task does not make completed work waiting.

The title names the current work unit or completed outcome, never a future action or waiting/blocking condition. Put the current phase, concrete result, dependency, or blocker in detail. Prefer title="Customer update publication" over "Waiting for approval", title="Delayed notification" over "Notification scheduled", and title="CRM contact import" over "CRM import blocked". Progress measures only this work unit; never use waiting with 100 percent.

Use next for only the nearest distinct operator-relevant responsibility after this work or phase. It is secondary metadata and must not replace the current title or detail. Never use placeholders such as "No pending work", restate the title, or record internal pacing. next_at is its optional deadline and is valid only when the SAME tool call also contains a non-empty next; never send next_at alone. Use it only for an RFC3339 deadline explicitly stated by the operator or directive, or obtained from an external system. Never invent or estimate it from [CURRENT TIME], expected duration, the current work phase, or internal pacing. A completed recurring task may remain completed while next and next_at describe its next scheduled run. If the responsibility has no known deadline, send next without next_at. If nothing meaningful is planned, omit both. Replacing a status without them clears the previous next action.

Examples:
- Completed recurring work: {"title":"Daily CRM check completed","state":"completed","detail":"No unresolved conversations found.","progress":100,"next":"Run the next daily CRM check.","next_at":"2026-07-20T09:00:00Z"}
- Expected approval: {"title":"Customer update publication","state":"waiting","detail":"Draft is ready and requires operator approval.","progress":70,"next":"Publish customer update after approval."}
- Corrective failure: {"title":"CRM contact import","state":"blocked","detail":"Authentication expired; reconnect the integration to resume."}

Emit at most one status per work phase. If the request already specifies a status call, that call satisfies this rule; never add a separate preliminary status. Skip status for directive or memory edits, thread management, internal configuration, planning, pacing, retries, tool discovery, chat connectivity, channel messages or publications, status maintenance itself, read-only lookups, brief answers, isolated quick actions, individual tools within one phase, and merely sleeping until future recurring work. Do not set a status just to announce what you may do later.`
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
	callerContext, _ := call.Arguments["_apteva_caller_context"].(string)
	delete(call.Arguments, "_apteva_caller_context")
	scopeChannel := func(ch Channel) Channel {
		if ch == nil || strings.TrimSpace(callerContext) == "" {
			return ch
		}
		if scoped, ok := ch.(framework.ConversationScopedChannel); ok {
			return scoped.ForConversationContext(callerContext)
		}
		return ch
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

	sendVisibleMessage := func(text, channel string, components []framework.ChatComponent) (framework.MessageDeliveryReceipt, any) {
		rawChannel := channel
		if channel == "current" {
			channel = s.resolveCurrentChannel()
			rawChannel = "current"
		}
		if text == "" {
			return framework.MessageDeliveryReceipt{}, textToolError("text required")
		}
		// Ordinary internal chat is conversation-owned. Main and generic
		// workers must send their result to the originating conversation with
		// core send; only that conversation may create the visible Apteva row.
		// The caller context is injected by core after model-visible telemetry,
		// so it is a trusted runtime capability rather than a prompt claim.
		if normalizeChannelID(channel) == "apteva" && !strings.HasPrefix(strings.TrimSpace(callerContext), "chat-") {
			return framework.MessageDeliveryReceipt{}, textToolError(
				"internal Apteva chat replies require an originating conversation thread. Main and worker threads must use send(id=\"<conversation-thread>\", message=\"...\") so that conversation can reply; channels_send cannot fall back to the primary chat",
			)
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
			return framework.MessageDeliveryReceipt{}, textToolError(msg)
		}
		ch := scopeChannel(s.registry.Get(normalized))
		if ch == nil {
			return framework.MessageDeliveryReceipt{}, textToolError(fmt.Sprintf("channel %q not found", normalized))
		}
		if sender, ok := ch.(framework.ReceiptSender); ok {
			receipt, err := sender.SendWithReceipt(text, components)
			if err != nil {
				return framework.MessageDeliveryReceipt{}, textToolError(err.Error())
			}
			return receipt, nil
		}
		if rich, ok := ch.(framework.RichSender); ok && len(components) > 0 {
			if err := rich.SendWithComponents(text, components); err != nil {
				return framework.MessageDeliveryReceipt{}, textToolError(err.Error())
			}
		} else {
			if err := ch.Send(text); err != nil {
				return framework.MessageDeliveryReceipt{}, textToolError(err.Error())
			}
		}
		return framework.MessageDeliveryReceipt{Inserted: true}, nil
	}

	deliveryResultText := func(receipt framework.MessageDeliveryReceipt) string {
		if !receipt.Inserted && receipt.MessageID > 0 {
			return fmt.Sprintf("duplicate_suppressed message_id=%d. This exact visible message was already delivered. Do not send it again. If it explicitly acknowledged concrete unfinished work, continue that work and send one final outcome afterward; otherwise pace/done.", receipt.MessageID)
		}
		if receipt.MessageID > 0 {
			return fmt.Sprintf("delivered message_id=%d. This exact visible message is complete. Do not repeat it after this result. If it explicitly acknowledged concrete unfinished work, continue that work and send one final outcome afterward; otherwise it satisfies the current chat turn and you should pace/done.", receipt.MessageID)
		}
		return "delivered. This exact visible message is complete. Do not repeat it after this result. If it explicitly acknowledged concrete unfinished work, continue that work and send one final outcome afterward; otherwise it satisfies the current chat turn and you should pace/done."
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
		ch := scopeChannel(s.registry.Get(resolveArtifactChannel(channel)))
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
		ch := scopeChannel(s.registry.Get(resolveArtifactChannel(channel)))
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
		ch := scopeChannel(s.registry.Get(resolveArtifactChannel(channel)))
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
		resolveRenamedArg := func(current string, aliases ...string) (string, string) {
			selectedName := ""
			selectedValue := ""
			for _, name := range append([]string{current}, aliases...) {
				value, _ := call.Arguments[name].(string)
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				if selectedValue != "" && value != selectedValue {
					return "", fmt.Sprintf("conflicting %s and %s values", selectedName, name)
				}
				selectedName = name
				selectedValue = value
			}
			return selectedValue, ""
		}
		next, aliasErr := resolveRenamedArg("next", "next_action", "planned_action")
		if aliasErr != "" {
			return textToolError(aliasErr)
		}
		nextAt, aliasErr := resolveRenamedArg("next_at", "next_action_due_at", "planned_action_deadline")
		if aliasErr != "" {
			return textToolError(aliasErr)
		}
		var progress *float64
		if value, ok := call.Arguments["progress"].(float64); ok {
			progress = &value
		}
		ch := scopeChannel(s.registry.Get(resolveArtifactChannel(channel)))
		if ch == nil {
			return textToolError(fmt.Sprintf("channel %q not found", channel))
		}
		sender, ok := ch.(framework.CurrentStatusSender)
		if !ok {
			return textToolError(fmt.Sprintf("channel %q does not support current status", channel))
		}
		result, err := sender.SetCurrentStatus(framework.CurrentStatusRequest{
			Title: title, Detail: detail, State: state, Progress: progress, Next: next, NextAt: nextAt,
		})
		if err != nil {
			return textToolError(err.Error())
		}
		raw, _ := json.Marshal(result)
		return textResult("current status updated: " + string(raw))
	}

	switch call.Name {
	case "send":
		// New clients use send only for ordinary messages. Keep the kind
		// dispatch below for older cores that cached the previous tools/list
		// schema before this server was upgraded.
		kind, _ := call.Arguments["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" {
			kind = "message"
		}
		channel, _ := call.Arguments["channel"].(string)
		switch kind {
		case "message":
			text, _ := call.Arguments["text"].(string)
			receipt, errResult := sendVisibleMessage(text, channel, extractComponents(call.Arguments["components"]))
			if errResult != nil {
				return errResult, nil
			}
			return textResult(deliveryResultText(receipt)), nil
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

	case "publish":
		kind, _ := call.Arguments["kind"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		title, _ := call.Arguments["title"].(string)
		content, _ := call.Arguments["content"].(string)
		title = strings.TrimSpace(title)
		content = strings.TrimSpace(content)
		if title == "" {
			return textToolError("publish requires a non-empty title; retry only this failed publication and do not repeat successful sibling calls"), nil
		}
		if content == "" {
			return textToolError("publish requires substantive content; for a report, summarize what actually happened and the outcome. Retry only this failed publication and do not repeat successful sibling calls"), nil
		}
		call.Arguments["title"] = title
		switch kind {
		case "approval":
			call.Arguments["body"] = content
			return sendApproval("apteva"), nil
		case "report":
			call.Arguments["summary"] = content
			return sendReport("apteva"), nil
		case "alert":
			call.Arguments["body"] = content
			return sendAlert("apteva"), nil
		default:
			return textToolError(fmt.Sprintf("publish kind %q is invalid; use approval, report, or alert", kind)), nil
		}

	case "set_status":
		state, _ := call.Arguments["state"].(string)
		state = strings.ToLower(strings.TrimSpace(state))
		if state == "" {
			return textToolError("set_status requires state: working, waiting, blocked, or completed. Retry only this failed status call and do not repeat successful sibling calls"), nil
		}
		call.Arguments["state"] = state
		return sendStatus("apteva"), nil

	case "respond":
		text, _ := call.Arguments["text"].(string)
		channel, _ := call.Arguments["channel"].(string)
		// Extract components if the agent attached any. When present
		// AND the channel implements framework.RichSender (channelchat
		// does; cli/slack/email/telegram don't), deliver them
		// alongside the text. Otherwise fall back to plain Send so
		// channels without rich rendering still get the text.
		components := extractComponents(call.Arguments["components"])
		receipt, errResult := sendVisibleMessage(text, channel, components)
		if errResult != nil {
			return errResult, nil
		}
		return textResult(deliveryResultText(receipt)), nil

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
			caps = []string{"message", "approval", "report", "alert", "buttons", "components"}
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
