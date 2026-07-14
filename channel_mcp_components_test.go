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
		"continue with the needed tools or pace",
		"send the actual outcome before going idle",
		"Use publish for approvals/reports/alerts",
		"set_status for mutable work state",
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

func TestChannelMCPRespondResultDoesNotForbidFinalOutcome(t *testing.T) {
	reg := NewChannelRegistry()
	reg.Register(&captureChannel{id: "apteva"})
	s := &channelMCPServer{registry: reg}
	params, _ := json.Marshal(map[string]any{
		"name": "respond",
		"arguments": map[string]any{
			"channel": "chat",
			"text":    "On it.",
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("handleToolCall rpcErr: %#v", rpcErr)
	}
	b, _ := json.Marshal(out)
	got := string(b)
	if strings.Contains(got, "do NOT send another respond") {
		t.Fatalf("respond result forbids final outcome: %s", got)
	}
	if !strings.Contains(got, "Continue promised work") {
		t.Fatalf("respond result missing continuation reminder: %s", got)
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
			"channel": "current",
			"text":    "On it.",
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
		},
	})
	out, rpcErr := s.handleToolCall(params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %#v", rpcErr)
	}
	if ch.currentStatus.Title != "Rendering clips" || ch.currentStatus.State != "working" ||
		ch.currentStatus.Progress == nil || *ch.currentStatus.Progress != 60 {
		t.Fatalf("status request = %#v", ch.currentStatus)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "current status updated") {
		t.Fatalf("unexpected result: %s", b)
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
		`{"kind":"report","title":"Import completed","content":"Imported 842 contacts; 17 invalid rows were skipped and saved for review."`,
		"what actually happened",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("publish description missing %q:\n%s", want, desc)
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
