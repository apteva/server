package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/apteva/server/apps/framework"
)

// buildSendDescription splices the AVAILABLE COMPONENTS catalog
// into the send tool's docs each turn. The agent learns what's
// renderable without a separate discovery call. This guards the
// happy path + the filtering rules.
func TestBuildSendDescription_BakesComponents(t *testing.T) {
	channels := []string{"apteva", "cli"}
	components := []componentEntry{
		// In scope — chat slot.
		{App: "storage", Name: "file-card", Slots: []string{"chat.message_attachment"}},
		// In scope — chat slot in addition to other slots.
		{App: "social", Name: "post-card", Slots: []string{"chat.message_attachment", "dashboard.project_sidebar"}, Description: "post status"},
		// Out of scope — only sidebar.
		{App: "media", Name: "usage-tile", Slots: []string{"dashboard.project_sidebar"}},
		// Out of scope — empty slots.
		{App: "weird", Name: "no-slots"},
	}
	desc := buildSendDescription(channels, components)

	// Catalog header + a line for each chat-eligible component.
	if !strings.Contains(desc, "AVAILABLE COMPONENTS") {
		t.Errorf("expected AVAILABLE COMPONENTS header in description")
	}
	if !strings.Contains(desc, `{app:"storage", name:"file-card"}`) {
		t.Errorf("expected storage/file-card in catalog: %s", desc)
	}
	if !strings.Contains(desc, `{app:"social", name:"post-card"}`) {
		t.Errorf("expected social/post-card in catalog")
	}
	if !strings.Contains(desc, "post status") {
		t.Errorf("expected per-component description text rendered")
	}
	// Out-of-scope components must not leak in. Match by the
	// rendered catalog line (which uses {app:"…", name:"…"}) so
	// we don't false-positive on the prose text that mentions
	// "media preview".
	if strings.Contains(desc, `{app:"media"`) || strings.Contains(desc, `{app:"weird"`) {
		t.Errorf("non-chat components leaked into catalog: %s", desc)
	}
}

func TestBuildSendDescription_EmptyCatalogOmitsBlock(t *testing.T) {
	desc := buildSendDescription([]string{"apteva"}, nil)
	// The header must not appear when there's nothing to list —
	// otherwise the agent thinks something is broken.
	if strings.Contains(desc, "AVAILABLE COMPONENTS") {
		t.Errorf("empty catalog should not render the AVAILABLE COMPONENTS header")
	}
	// Sanity — main description still renders.
	if !strings.Contains(desc, "KNOWN CHANNELS") {
		t.Errorf("main description missing")
	}
}

func TestBuildSendDescription_MessageWakesAgain(t *testing.T) {
	desc := buildSendDescription([]string{"apteva"}, nil)
	for _, want := range []string{
		"successful send wakes you again",
		"result is only the delivery receipt",
		"Never repeat it",
		"Continue only concrete unfinished work",
		"Every direct [chat] turn requires at least one successful call to this tool",
		"turn is incomplete until you send its visible answer",
		"read-only lookup results",
		"After any non-channel tool result used for the request",
		"never leave it only in thoughts or plain assistant output",
		"Use publish for approvals/reports/alerts",
		"set_status for mutable work state",
		"later outcome of work explicitly requested in that chat",
		"Do NOT send ordinary chat for autonomous or scheduled checks",
		"unchanged or no-op results",
		`"send a status update" or "update the status" means set_status`,
		"call pace for the next due check, and remain silent",
		"status next_at does not schedule a wake",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("respond description missing %q:\n%s", want, desc)
		}
	}
}

func TestChannelMCPSendAdvertisesWakeAlways(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	out := s.toolsList()
	tools, ok := out["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools payload has unexpected shape: %#v", out["tools"])
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name != "send" && name != "publish" && name != "set_status" {
			continue
		}
		seen[name] = true
		meta, ok := tool["_meta"].(map[string]any)
		if !ok {
			t.Fatalf("%s tool missing _meta: %#v", name, tool)
		}
		if got := meta["io.apteva/wakeOnResult"]; got != "always" {
			t.Fatalf("%s wakeOnResult=%v, want always", name, got)
		}
	}
	for _, name := range []string{"send", "publish", "set_status"} {
		if !seen[name] {
			t.Fatalf("tool %q not found", name)
		}
	}
}

func TestChannelMCPAdvertisesUnconditionalSchemas(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	tools := s.toolsList()["tools"].([]map[string]any)
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		byName[name] = tool
	}
	assertRequired := func(name string, want []string) map[string]any {
		t.Helper()
		schema := byName[name]["inputSchema"].(map[string]any)
		got := schema["required"].([]string)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s required=%v, want %v", name, got, want)
		}
		return schema["properties"].(map[string]any)
	}
	sendProps := assertRequired("send", []string{"channel", "text"})
	if _, exists := sendProps["kind"]; exists {
		t.Fatal("new send schema must not advertise legacy kind")
	}
	sendDescription, _ := byName["send"]["description"].(string)
	for _, want := range []string{
		"including dashboard conversation threads",
		"prefer a short visible acknowledgement before beginning tool work",
		"strong guidance, not a hard requirement",
		"wait for this send to succeed before action tools",
		"never parallelize the acknowledgement with them",
		"the acknowledgement never replaces it",
		"tool-work turn has either one final message or two intentional messages",
		"acknowledgement then final",
		"never more",
		"prefer acknowledgement before send(main)",
		"never both",
	} {
		if !strings.Contains(sendDescription, want) {
			t.Fatalf("send description missing %q: %s", want, sendDescription)
		}
	}
	publishProps := assertRequired("publish", []string{"kind", "title", "content"})
	statusProps := assertRequired("set_status", []string{"title", "state"})
	assertEnum := func(properties map[string]any, field string, want []string) {
		t.Helper()
		definition := properties[field].(map[string]any)
		got := definition["enum"].([]string)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s enum=%#v, want %#v", field, got, want)
		}
	}
	assertEnum(publishProps, "kind", []string{"approval", "report", "alert"})
	assertEnum(statusProps, "state", []string{"working", "waiting", "blocked", "completed"})
	if _, ok := statusProps["next"]; !ok {
		t.Fatal("set_status schema missing optional next field")
	}
	nextAt, ok := statusProps["next_at"].(map[string]any)
	if !ok {
		t.Fatalf("set_status next_at schema = %#v, want object", statusProps["next_at"])
	}
	if _, exists := nextAt["format"]; exists {
		t.Fatalf("set_status next_at schema unexpectedly advertises a format hint: %#v", nextAt)
	}
	for _, legacy := range []string{"next_action", "next_action_due_at", "planned_action", "planned_action_deadline"} {
		if _, exists := statusProps[legacy]; exists {
			t.Fatalf("set_status schema still advertises legacy field %q", legacy)
		}
	}
	statusDescription, _ := byName["set_status"]["description"].(string)
	titleDescription, _ := statusProps["title"].(map[string]any)["description"].(string)
	progressDescription, _ := statusProps["progress"].(map[string]any)["description"].(string)
	nextDescription, _ := statusProps["next"].(map[string]any)["description"].(string)
	nextAtDescription, _ := nextAt["description"].(string)
	for _, want := range []string{
		"meaningful operator-relevant work",
		"multi-step, long-running, or cannot currently continue",
		"always call this tool at meaningful phase changes",
		"do not merely describe the state in thoughts or chat",
		"even when no other action tool remains",
		"expected pause in that same unfinished work unit",
		"operator approval",
		"do not use blocked for ordinary approval or a scheduled delay",
		"future recurring task does not make completed work waiting",
		"never a future action or waiting/blocking condition",
		`title="Customer update publication" over "Waiting for approval"`,
		`title="CRM contact import" over "CRM import blocked"`,
		"never use waiting with 100 percent",
		"directive or memory edits",
		"channel messages or publications",
		"merely sleeping until future recurring work",
		"nearest distinct operator-relevant responsibility",
		"must not replace the current title or detail",
		"No pending work",
		"completed recurring task may remain completed",
		`"state":"completed"`,
		`"state":"waiting"`,
		`"state":"blocked"`,
		"SAME tool call also contains a non-empty next",
		"Never invent or estimate it from [CURRENT TIME]",
	} {
		if !strings.Contains(statusDescription, want) {
			t.Fatalf("set_status description missing %q: %s", want, statusDescription)
		}
	}
	for _, want := range []string{
		"same call contains a non-empty next",
		"RFC3339 deadline for next",
		"estimate it from current time",
	} {
		if !strings.Contains(nextAtDescription, want) {
			t.Fatalf("set_status next_at description missing %q: %s", want, nextAtDescription)
		}
	}
	for label, pair := range map[string][2]string{
		"title":    {titleDescription, "Never use a future action, waiting condition, or internal agent administration"},
		"progress": {progressDescription, "Never use waiting with 100 percent"},
		"next":     {nextDescription, "No pending work"},
	} {
		if !strings.Contains(pair[0], pair[1]) {
			t.Fatalf("set_status %s description missing %q: %s", label, pair[1], pair[0])
		}
	}
	if strings.Contains(statusDescription, "substantive external action") {
		t.Fatalf("set_status description still treats isolated external actions as status-worthy: %s", statusDescription)
	}
}

func TestChannelMCPSetStatusDescriptionSeparatesCurrentWorkFromFutureSchedule(t *testing.T) {
	desc := buildSetStatusDescription()
	for _, want := range []string{
		"Status answers: what meaningful operator-relevant work",
		"Use waiting only for an expected pause in that same unfinished work unit",
		"A future recurring task does not make completed work waiting",
		"title names the current work unit or completed outcome",
		"never a future action or waiting/blocking condition",
		"never use waiting with 100 percent",
		"Skip status for directive or memory edits",
		"status maintenance itself",
		"isolated quick actions",
		"merely sleeping until future recurring work",
		"Do not set a status just to announce what you may do later",
		"A completed recurring task may remain completed while next and next_at describe its next scheduled run",
		`"send a status update" or "update the status"`,
		"must not also create an ordinary chat message",
		"call pace for the next due check",
		"next_at is display metadata and does not schedule a wake",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("set_status description missing %q:\n%s", want, desc)
		}
	}
}

func TestChannelMCPAdvertisesSeparatedTools(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	out := s.toolsList()
	tools, ok := out["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools payload has unexpected shape: %#v", out["tools"])
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		seen[name] = true
		if name != "send" && name != "publish" && name != "set_status" && name != "list_channels" {
			t.Fatalf("unexpected advertised tool %q: %#v", name, tool)
		}
	}
	for _, name := range []string{"send", "publish", "set_status", "list_channels"} {
		if !seen[name] {
			t.Fatalf("missing advertised tool %q: %#v", name, seen)
		}
	}
}

func TestLegacyChannelMCPRespondResultClosesDeliveredReply(t *testing.T) {
	reg := NewChannelRegistry()
	reg.Register(&captureChannel{id: "apteva"})
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "respond",
		"arguments": map[string]any{
			"channel":                "chat",
			"text":                   "On it.",
			"_apteva_caller_context": "chat-default-1",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("handleToolCall rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	got := string(b)
	if !strings.Contains(got, "satisfies the current chat turn") || !strings.Contains(got, "Do not repeat") {
		t.Fatalf("respond result missing unambiguous delivery receipt: %s", got)
	}
}

func TestChannelMCPSendReportsSuppressedDuplicateAsSuccessfulReceipt(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &receiptCaptureChannel{
		captureChannel: captureChannel{id: "apteva", active: true},
		receipt:        framework.MessageDeliveryReceipt{MessageID: 42, Inserted: false},
	}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"channel":                "current",
			"text":                   "Understood — I’ll wait.",
			"_apteva_caller_context": "chat-default-1",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, _ := json.Marshal(out)
	got := string(raw)
	if !strings.Contains(got, "duplicate_suppressed") || !strings.Contains(got, "message_id=42") || !strings.Contains(got, "Do not send it again") {
		t.Fatalf("duplicate receipt was ambiguous: %s", got)
	}
	if strings.Contains(got, `"isError":true`) {
		t.Fatalf("idempotent suppression must remain a successful tool result: %s", got)
	}
}

func TestChannelMCPSendMessageCurrentUsesLiveChat(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva", active: true}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"channel":                "current",
			"text":                   "On it.",
			"_apteva_caller_context": "chat-default-1",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("handleToolCall rpcErr: %#v", rpcErr)
	}
	if ch.sent != "On it." {
		t.Fatalf("sent=%q", ch.sent)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "delivered") {
		t.Fatalf("unexpected result: %s", string(b))
	}
}

func TestChannelMCPSendRejectsInternalAptevaReplyFromMain(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva", active: true}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"channel":                "current",
			"text":                   "This must not enter primary chat",
			"_apteva_caller_context": "main",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, _ := json.Marshal(out)
	got := string(raw)
	if !strings.Contains(got, `"isError":true`) || !strings.Contains(got, "originating conversation thread") {
		t.Fatalf("main rejection=%s", got)
	}
	if ch.sent != "" {
		t.Fatalf("main internal message was delivered: %q", ch.sent)
	}
}

func TestChannelMCPSendStillAllowsExplicitExternalChannelFromMain(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "slack", active: true}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"channel":                "slack",
			"text":                   "External notification",
			"_apteva_caller_context": "main",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("external main send failed: %s", raw)
	}
	if ch.sent != "External notification" {
		t.Fatalf("external message=%q", ch.sent)
	}
}

func TestChannelMCPSendScopesAptevaChannelToCallerConversation(t *testing.T) {
	reg := NewChannelRegistry()
	target := &captureChannel{id: "apteva", active: true}
	base := &scopedCaptureChannel{captureChannel: captureChannel{id: "apteva", active: true}, target: target}
	reg.Register(base)
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"channel":                "current",
			"text":                   "Room answer",
			"_apteva_caller_context": "chat-conv-123",
		},
	})
	if _, rpcErr := s.handleToolCall(params); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if base.contextID != "chat-conv-123" {
		t.Fatalf("context=%q", base.contextID)
	}
	if base.sent != "" {
		t.Fatalf("base channel received scoped message: %q", base.sent)
	}
	if target.sent != "Room answer" {
		t.Fatalf("target sent=%q", target.sent)
	}
}

func TestChannelMCPPublishApprovalAndReportUseAptevaChannel(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	for _, tc := range []struct {
		kind string
		args map[string]any
		want string
	}{
		{"approval", map[string]any{"title": "Approve deploy", "content": "Deploy now?"}, "approval sent to Apteva channel"},
		{"report", map[string]any{"title": "Daily report", "content": "Imported 42 contacts."}, "report sent to Apteva channel"},
	} {
		params, _ := json.Marshal(map[string]any{
			"name": "publish",
			"arguments": map[string]any{
				"kind":    tc.kind,
				"title":   tc.args["title"],
				"content": tc.args["content"],
			},
		})
		out, rpcErr := s.handleToolCall(params)
		if rpcErr != nil {
			t.Fatalf("%s rpcErr: %#v", tc.kind, rpcErr)
		}
		b, _ := json.Marshal(out)
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("%s result missing %q: %s", tc.kind, tc.want, string(b))
		}
	}
	if ch.approvals != 1 || ch.reports != 1 {
		t.Fatalf("approvals=%d reports=%d", ch.approvals, ch.reports)
	}
}

func TestChannelMCPSetStatusUsesAptevaCurrentStatus(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status",
		"arguments": map[string]any{
			"title":  "Rendering clips",
			"detail": "Three of five complete", "state": "working", "progress": 60,
			"next": "Publish the finished reel", "next_at": "2026-07-20T09:00:00Z",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	if ch.currentStatus.Title != "Rendering clips" || ch.currentStatus.State != "working" ||
		ch.currentStatus.Progress == nil || *ch.currentStatus.Progress != 60 ||
		ch.currentStatus.Next != "Publish the finished reel" || ch.currentStatus.NextAt != "2026-07-20T09:00:00Z" {
		t.Fatalf("status request = %#v", ch.currentStatus)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "current status updated") {
		t.Fatalf("unexpected result: %s", b)
	}
}

func TestChannelMCPSetStatusAcceptsLegacyNextAliases(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status",
		"arguments": map[string]any{
			"title": "Legacy status", "state": "waiting",
			"next": "Resume import", "next_at": "2026-07-20T09:00:00Z",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	if ch.currentStatus.Next != "Resume import" || ch.currentStatus.NextAt != "2026-07-20T09:00:00Z" {
		t.Fatalf("legacy status request = %#v", ch.currentStatus)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "current status updated") {
		t.Fatalf("unexpected legacy result: %s", b)
	}
}

func TestChannelMCPSetStatusAcceptsPreviousNextActionAliases(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status",
		"arguments": map[string]any{
			"title": "Previous schema status", "state": "waiting",
			"next_action": "Resume import", "next_action_due_at": "2026-07-20T09:00:00Z",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	if ch.currentStatus.Next != "Resume import" || ch.currentStatus.NextAt != "2026-07-20T09:00:00Z" {
		t.Fatalf("previous-schema status request = %#v", ch.currentStatus)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "current status updated") {
		t.Fatalf("unexpected previous-schema result: %s", b)
	}
}

func TestChannelMCPSetStatusAcceptsPreviousPlannedActionAliases(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status",
		"arguments": map[string]any{
			"title": "Experimental schema status", "state": "waiting",
			"planned_action": "Resume import", "planned_action_deadline": "2026-07-20T09:00:00Z",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	if ch.currentStatus.Next != "Resume import" || ch.currentStatus.NextAt != "2026-07-20T09:00:00Z" {
		t.Fatalf("experimental-schema status request = %#v", ch.currentStatus)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "current status updated") {
		t.Fatalf("unexpected experimental-schema result: %s", b)
	}
}

func TestChannelMCPSetStatusRejectsConflictingRenamedAliases(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status",
		"arguments": map[string]any{
			"title": "Conflicting status", "state": "waiting",
			"planned_action": "New action", "next": "Legacy action",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "conflicting next and planned_action values") {
		t.Fatalf("unexpected conflict result: %s", b)
	}
}

func TestChannelMCPSetStatusRejectsMissingStateWithTargetedRetry(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	params, _ := json.Marshal(map[string]any{
		"name": "set_status", "arguments": map[string]any{"title": "Incomplete status"},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	for _, want := range []string{"requires state", "Retry only this failed status call"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing targeted error %q: %s", want, b)
		}
	}
}

func TestChannelMCPLegacyTypedSendStillWorks(t *testing.T) {
	reg := NewChannelRegistry()
	ch := &captureChannel{id: "apteva"}
	reg.Register(ch)
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "send",
		"arguments": map[string]any{
			"kind": "report", "channel": "apteva", "title": "Legacy report", "summary": "Still accepted.",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "report sent to Apteva channel") || ch.reports != 1 {
		t.Fatalf("legacy report was not delivered: result=%s reports=%d", b, ch.reports)
	}
}

func TestChannelMCPPublishDescriptionRequiresSubstantiveReportContent(t *testing.T) {
	desc := buildPublishDescription()
	for _, want := range []string{
		"Every call requires kind, title, and content",
		"content must still stand alone",
		`{"kind":"report","title":"Daily work summary","content":"Imported 842 contacts, cleared 12 routine inbox items`,
		"concrete outcomes",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("publish description missing %q:\n%s", want, desc)
		}
	}
}

func TestChannelMCPPublishDescriptionDefaultsReportsToOneDailyDigest(t *testing.T) {
	desc := buildPublishDescription()
	for _, want := range []string{
		"Reports are not action receipts",
		"Do not publish a report after each check, tool call, cleanup, or completed task",
		"use set_status for work state and send for a requested task's direct outcome",
		"Follow an explicit operator request or directive when it defines report timing",
		"at most one unsolicited report per day",
		"near the end of the operator's day",
		"Combine that day's work into one digest",
		"use period=today",
		"If no meaningful work was done, publish no report",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("publish description missing daily-digest guidance %q:\n%s", want, desc)
		}
	}
	for _, unwanted := range []string{
		"scheduled summaries, significant completed work",
		`"title":"Import completed"`,
	} {
		if strings.Contains(desc, unwanted) {
			t.Fatalf("publish description retained action-report guidance %q:\n%s", unwanted, desc)
		}
	}
}

func TestChannelMCPPublishRejectsMissingContentWithTargetedRetry(t *testing.T) {
	s := &channelMCPServer{registry: NewChannelRegistry()}
	params, _ := json.Marshal(map[string]any{
		"name":      "publish",
		"arguments": map[string]any{"kind": "report", "title": "Incomplete report"},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	for _, want := range []string{"requires substantive content", "what actually happened", "Retry only this failed publication"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing targeted error %q: %s", want, b)
		}
	}
}

func TestChannelMCPListChannelsIncludesAptevaCapabilities(t *testing.T) {
	reg := NewChannelRegistry()
	reg.Register(&captureChannel{id: "apteva", active: true})
	s := &channelMCPServer{registry: reg, ic: &AgentChannels{registry: reg}}
	params, _ := json.Marshal(map[string]any{"name": "list_channels", "arguments": map[string]any{}})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("handleToolCall rpcErr: %#v", rpcErr)
	}
	outMap, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result shape: %#v", out)
	}
	var content []map[string]string
	switch v := outMap["content"].(type) {
	case []map[string]string:
		content = v
	case []map[string]any:
		for _, item := range v {
			row := map[string]string{}
			for key, value := range item {
				if s, ok := value.(string); ok {
					row[key] = s
				}
			}
			content = append(content, row)
		}
	}
	if len(content) != 1 {
		t.Fatalf("unexpected content shape: %#v", outMap["content"])
	}
	text := content[0]["text"]
	var channels []map[string]any
	if err := json.Unmarshal([]byte(text), &channels); err != nil {
		t.Fatalf("list_channels returned invalid JSON %q: %v", text, err)
	}
	seen := map[string]map[string]any{}
	for _, ch := range channels {
		id, _ := ch["id"].(string)
		seen[id] = ch
	}
	for _, id := range []string{"apteva"} {
		if seen[id] == nil {
			t.Fatalf("missing channel %q in %#v", id, channels)
		}
	}
	for _, want := range []string{"message", "approval", "report", "alert", "buttons", "components"} {
		if !capabilityPresent(seen["apteva"]["capabilities"], want) {
			t.Fatalf("apteva channel missing capability %q: %#v", want, seen["apteva"])
		}
	}
	if seen["chat"] != nil {
		t.Fatalf("legacy chat alias should not be advertised: %#v", channels)
	}
}

func capabilityPresent(raw any, want string) bool {
	arr, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, v := range arr {
		if s, _ := v.(string); s == want {
			return true
		}
	}
	return false
}

type captureChannel struct {
	id            string
	sent          string
	statusText    string
	active        bool
	approvals     int
	reports       int
	currentStatus framework.CurrentStatusRequest
}

type scopedCaptureChannel struct {
	captureChannel
	contextID string
	target    framework.Channel
}

type receiptCaptureChannel struct {
	captureChannel
	receipt framework.MessageDeliveryReceipt
}

func (c *receiptCaptureChannel) SendWithReceipt(text string, _ []framework.ChatComponent) (framework.MessageDeliveryReceipt, error) {
	c.sent = text
	return c.receipt, nil
}

func (c *scopedCaptureChannel) ForConversationContext(contextID string) framework.Channel {
	c.contextID = contextID
	return c.target
}

func (c *captureChannel) ID() string { return c.id }
func (c *captureChannel) Send(text string) error {
	c.sent = text
	return nil
}
func (c *captureChannel) Status(text, level string) error {
	c.statusText = text
	return nil
}
func (c *captureChannel) Close()         {}
func (c *captureChannel) IsActive() bool { return c.active }
func (c *captureChannel) RequestApproval(req framework.ApprovalRequest) (framework.ApprovalResult, error) {
	c.approvals++
	return framework.ApprovalResult{MessageID: int64(c.approvals), ChatID: c.id, Status: "pending"}, nil
}
func (c *captureChannel) SendReport(req framework.ReportRequest) (framework.ReportResult, error) {
	c.reports++
	return framework.ReportResult{MessageID: int64(c.reports), ChatID: c.id, Status: "sent"}, nil
}
func (c *captureChannel) SetCurrentStatus(req framework.CurrentStatusRequest) (framework.CurrentStatusResult, error) {
	c.currentStatus = req
	return framework.CurrentStatusResult{MessageID: 1, ChatID: c.id, State: req.State}, nil
}
