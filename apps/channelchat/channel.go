package channelchat

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/apteva/server/apps/framework"
)

// chatChannel is the Channel implementation the agent reaches via
// channels_send(channel="apteva", text=...). It writes the agent's
// reply as a new `role=agent` row and pushes to the hub so every
// connected dashboard tab sees the new message immediately.
//
// Unlike CLIBridge, Send is NOT a no-op: the DB row is the source of
// truth for the conversation. Unlike SlackChannel, there's no remote
// API — everything is local.
type chatChannel struct {
	chatID   string
	threadID string
	agentID  int64
	userID   int64 // owner of the instance — drives wildcard hub fanout
	store    *store
	hub      *hub
	bus      *framework.AppBus
}

// ID returns the canonical internal Apteva operator channel. Older
// agents may still call channel="chat"; the channels MCP aliases that
// to "apteva" for compatibility.
func (c *chatChannel) ID() string { return "apteva" }

func (c *chatChannel) ForConversationContext(contextID string) framework.Channel {
	if c.store == nil || strings.TrimSpace(contextID) == "" {
		return c
	}
	chat, err := c.store.ChatForAgentThread(c.agentID, contextID)
	if err != nil {
		// A chat-* caller context claims ownership of one concrete durable
		// conversation. Never fall it back to the primary chat: the Channels
		// MCP will return a visible tool error for deleted, forged, or stale
		// contexts instead of crossing conversation boundaries.
		if strings.HasPrefix(strings.TrimSpace(contextID), "chat-") {
			return nil
		}
		// Non-chat workers legitimately have no conversation mapping. Preserve
		// the historical artifact/status behavior for non-conversational tools;
		// ordinary channels_send is independently blocked at the MCP boundary.
		copy := *c
		copy.threadID = contextID
		return &copy
	}
	copy := *c
	copy.chatID = chat.ID
	copy.threadID = contextID
	if chat.OwnerUserID != 0 {
		copy.userID = chat.OwnerUserID
	}
	return &copy
}

// Send inserts a final agent message and fans it out.
func (c *chatChannel) Send(text string) error {
	return c.SendWithComponents(text, nil)
}

// SendWithComponents writes the agent's reply with optional rich
// attachments. Implements framework.RichSender — the channels MCP
// looks for this method when the agent's respond call carries a
// `components` arg.
func (c *chatChannel) SendWithComponents(text string, components []framework.ChatComponent) error {
	_, err := c.SendWithReceipt(text, components)
	return err
}

// SendWithReceipt exposes whether the durable row was newly inserted or an
// immediate exact retry was suppressed. The ordinary Channel methods retain
// their error-only compatibility for external channel consumers.
func (c *chatChannel) SendWithReceipt(text string, components []framework.ChatComponent) (framework.MessageDeliveryReceipt, error) {
	return c.SendWithReceiptAndPhase(text, components, "final")
}

// SendWithReceiptAndPhase persists the visible message lifecycle phase in the
// existing metadata_json column. Keeping this as an optional framework
// capability avoids changing external channel implementations.
func (c *chatChannel) SendWithReceiptAndPhase(text string, components []framework.ChatComponent, phase string) (framework.MessageDeliveryReceipt, error) {
	if c.store == nil {
		return framework.MessageDeliveryReceipt{}, fmt.Errorf("channel-chat: store not initialised")
	}
	phase = normalizeMessagePhase(phase)
	m, inserted, err := c.store.AppendAgentMessageOnceWithMetadata(
		c.chatID,
		text,
		c.threadID,
		c.agentID,
		components,
		map[string]any{"phase": phase},
	)
	if err != nil {
		log.Printf("[CHAT] Send DB append failed chatID=%s err=%v", c.chatID, err)
		return framework.MessageDeliveryReceipt{}, err
	}
	receipt := framework.MessageDeliveryReceipt{MessageID: m.ID, Inserted: inserted}
	if !inserted {
		log.Printf("[CHAT-DEBUG] suppressed immediate duplicate chat=%s messageID=%d", c.chatID, m.ID)
		return receipt, nil
	}
	chatSubs, userSubs := c.hub.subscriberCounts(c.chatID, c.userID)
	log.Printf("[CHAT-DEBUG] Send chat=%s user=%d msgID=%d components=%d chatSubs=%d userSubs=%d",
		c.chatID, c.userID, m.ID, len(components), chatSubs, userSubs)
	c.hub.publish(*m)
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.message", "channel-chat", *m)
	}
	return receipt, nil
}

func normalizeMessagePhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "acknowledgement":
		return "acknowledgement"
	case "progress":
		return "progress"
	default:
		return "final"
	}
}

func (c *chatChannel) RequestApproval(req framework.ApprovalRequest) (framework.ApprovalResult, error) {
	if c.store == nil {
		return framework.ApprovalResult{}, fmt.Errorf("channel-chat: store not initialised")
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Body = strings.TrimSpace(req.Body)
	if req.Title == "" {
		return framework.ApprovalResult{}, fmt.Errorf("title required")
	}
	if req.Body == "" {
		req.Body = req.Title
	}
	if len(req.Actions) == 0 {
		req.Actions = []framework.ApprovalAction{
			{ID: "approve", Label: "Approve", Style: "primary"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		}
	}
	props := map[string]any{
		"title":   req.Title,
		"body":    req.Body,
		"status":  "pending",
		"actions": req.Actions,
	}
	if req.Context != nil {
		props["context"] = req.Context
	}
	components := []framework.ChatComponent{{
		App:   "channel-chat",
		Name:  "approval-card",
		Props: props,
	}}
	content := "Approval requested: " + req.Title
	m, err := c.store.AppendAgentArtifact(c.chatID, content, c.agentID, c.threadID, components)
	if err != nil {
		return framework.ApprovalResult{}, err
	}
	m.Components[0].Props["message_id"] = m.ID
	m, err = c.store.UpdateMessageComponents(m.ID, m.Components)
	if err != nil {
		return framework.ApprovalResult{}, err
	}
	c.hub.publish(*m)
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.message", "channel-chat", *m)
	}
	return framework.ApprovalResult{MessageID: m.ID, ChatID: m.ChatID, Status: "pending"}, nil
}

func (c *chatChannel) SendReport(req framework.ReportRequest) (framework.ReportResult, error) {
	if c.store == nil {
		return framework.ReportResult{}, fmt.Errorf("channel-chat: store not initialised")
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Summary = strings.TrimSpace(req.Summary)
	req.Period = strings.TrimSpace(req.Period)
	if req.Title == "" {
		return framework.ReportResult{}, fmt.Errorf("title required")
	}
	cleanSections := make([]framework.ReportSection, 0, len(req.Sections))
	for _, section := range req.Sections {
		title := strings.TrimSpace(section.Title)
		body := strings.TrimSpace(section.Body)
		if title == "" && body == "" {
			continue
		}
		cleanSections = append(cleanSections, framework.ReportSection{Title: title, Body: body})
	}
	if req.Summary == "" && len(cleanSections) == 0 {
		return framework.ReportResult{}, fmt.Errorf("summary or sections required")
	}
	cleanTags := make([]string, 0, len(req.Tags))
	for _, tag := range req.Tags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			cleanTags = append(cleanTags, tag)
		}
	}
	props := map[string]any{
		"title":      req.Title,
		"summary":    req.Summary,
		"period":     req.Period,
		"sections":   cleanSections,
		"tags":       cleanTags,
		"inbox_only": true,
		"status":     "sent",
	}
	if req.Context != nil {
		props["context"] = req.Context
	}
	components := []framework.ChatComponent{{
		App:   "channel-chat",
		Name:  "report-card",
		Props: props,
	}}
	content := "Report: " + req.Title
	m, err := c.store.AppendAgentArtifact(c.chatID, content, c.agentID, c.threadID, components)
	if err != nil {
		return framework.ReportResult{}, err
	}
	m.Components[0].Props["message_id"] = m.ID
	m, err = c.store.UpdateMessageComponents(m.ID, m.Components)
	if err != nil {
		return framework.ReportResult{}, err
	}
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.report", "channel-chat", *m)
	}
	return framework.ReportResult{MessageID: m.ID, ChatID: m.ChatID, Status: "sent"}, nil
}

// Status writes an alert artifact into the Apteva channel. It is
// filtered out of normal chat history and shown through the inbox view.
func (c *chatChannel) Status(text, level string) error {
	if level == "" {
		level = "info"
	}
	props := map[string]any{
		"title":    firstNonEmptyString(alertTitle(text), "Alert"),
		"body":     strings.TrimSpace(text),
		"severity": strings.TrimSpace(level),
		"status":   "sent",
	}
	components := []framework.ChatComponent{{
		App:   "channel-chat",
		Name:  "alert-card",
		Props: props,
	}}
	m, err := c.store.AppendAgentArtifact(c.chatID, "Alert: "+props["title"].(string), c.agentID, c.threadID, components)
	if err != nil {
		return err
	}
	m.Components[0].Props["message_id"] = m.ID
	m, err = c.store.UpdateMessageComponents(m.ID, m.Components)
	if err != nil {
		return err
	}
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.alert", "channel-chat", *m)
	}
	return nil
}

func (c *chatChannel) SetCurrentStatus(req framework.CurrentStatusRequest) (framework.CurrentStatusResult, error) {
	if c.store == nil {
		return framework.CurrentStatusResult{}, fmt.Errorf("channel-chat: store not initialised")
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Detail = strings.TrimSpace(req.Detail)
	req.State = strings.ToLower(strings.TrimSpace(req.State))
	req.Next = strings.TrimSpace(req.Next)
	req.NextAt = strings.TrimSpace(req.NextAt)
	if req.Title == "" {
		return framework.CurrentStatusResult{}, fmt.Errorf("title required")
	}
	if req.State == "" {
		req.State = "working"
	}
	switch req.State {
	case "working", "waiting", "blocked", "completed":
	default:
		return framework.CurrentStatusResult{}, fmt.Errorf("state must be working, waiting, blocked, or completed")
	}
	if req.Progress != nil && (*req.Progress < 0 || *req.Progress > 100) {
		return framework.CurrentStatusResult{}, fmt.Errorf("progress must be between 0 and 100")
	}
	if req.NextAt != "" && req.Next == "" {
		return framework.CurrentStatusResult{}, fmt.Errorf("next_at requires next")
	}
	if req.NextAt != "" {
		nextAt, err := time.Parse(time.RFC3339, req.NextAt)
		if err != nil {
			return framework.CurrentStatusResult{}, fmt.Errorf("next_at must be an RFC3339 timestamp")
		}
		req.NextAt = nextAt.UTC().Format(time.RFC3339)
	}
	props := map[string]any{
		"agent_id":   c.agentID,
		"title":      req.Title,
		"detail":     req.Detail,
		"state":      req.State,
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if req.Progress != nil {
		props["progress"] = *req.Progress
	}
	if req.Next != "" {
		props["next"] = req.Next
	}
	if req.NextAt != "" {
		props["next_at"] = req.NextAt
	}
	components := []framework.ChatComponent{{App: "channel-chat", Name: "status-card", Props: props}}
	m, err := c.store.UpsertCurrentStatus(c.chatID, c.agentID, c.threadID, "Status: "+req.Title, components)
	if err != nil {
		return framework.CurrentStatusResult{}, err
	}
	m.Components[0].Props["message_id"] = m.ID
	m, err = c.store.UpdateMessageComponents(m.ID, m.Components)
	if err != nil {
		return framework.CurrentStatusResult{}, err
	}
	// Status is intentionally absent from the per-chat hub. The user stream
	// drives monitoring surfaces while chat panels remain untouched.
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.status", "channel-chat", *m)
	}
	return framework.CurrentStatusResult{MessageID: m.ID, ChatID: m.ChatID, State: req.State}, nil
}

// Close is a no-op — nothing to tear down. The Channel interface
// requires it; the per-instance registry calls it on detach.
func (c *chatChannel) Close() {}

// IsActive is always true for the Apteva operator channel. Presence is
// a UI concern; agents can always leave durable messages for operators.
func (c *chatChannel) IsActive() bool { return true }

// --- Factory ----------------------------------------------------------

// chatChannelFactory builds the internal operator channel for an agent. Its
// default-* storage record is an inbox/status sink for main, not a dashboard
// conversation. Explicit conv-* conversations get scoped channel contexts
// through the conversation delivery path.
type chatChannelFactory struct {
	store *store
	hub   *hub
	bus   *framework.AppBus
}

func (f *chatChannelFactory) ChannelID(_ framework.InstanceInfo) string {
	return "apteva"
}

func (f *chatChannelFactory) Build(_ *framework.AppCtx, inst framework.InstanceInfo) (framework.Channel, error) {
	chat, err := f.store.EnsureDefaultChat(inst.ID)
	if err != nil {
		return nil, err
	}
	return &chatChannel{
		chatID:  chat.ID,
		agentID: inst.ID,
		userID:  inst.UserID,
		store:   f.store,
		hub:     f.hub,
		bus:     f.bus,
	}, nil
}

func alertTitle(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if i := strings.Index(text, ":"); i > 0 && i <= 80 {
		return strings.TrimSpace(text[:i])
	}
	if len(text) > 80 {
		return strings.TrimSpace(text[:80])
	}
	return text
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
