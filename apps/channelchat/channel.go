package channelchat

import (
	"fmt"
	"log"
	"strings"

	"github.com/apteva/server/apps/framework"
)

// chatChannel is the Channel implementation the agent reaches via
// channels_send(channel="apteva", kind="message", text=...). It writes the agent's
// reply as a new `role=agent` row and pushes to the hub so every
// connected dashboard tab sees the new message immediately.
//
// Unlike CLIBridge, Send is NOT a no-op: the DB row is the source of
// truth for the conversation. Unlike SlackChannel, there's no remote
// API — everything is local.
type chatChannel struct {
	chatID   string
	threadID string
	userID   int64 // owner of the instance — drives wildcard hub fanout
	store    *store
	hub      *hub
	bus      *framework.AppBus
}

// ID returns the canonical internal Apteva operator channel. Older
// agents may still call channel="chat"; the channels MCP aliases that
// to "apteva" for compatibility.
func (c *chatChannel) ID() string { return "apteva" }

// Send inserts a final agent message and fans it out.
func (c *chatChannel) Send(text string) error {
	return c.SendWithComponents(text, nil)
}

// SendWithComponents writes the agent's reply with optional rich
// attachments. Implements framework.RichSender — the channels MCP
// looks for this method when the agent's respond call carries a
// `components` arg.
func (c *chatChannel) SendWithComponents(text string, components []framework.ChatComponent) error {
	if c.store == nil {
		return fmt.Errorf("channel-chat: store not initialised")
	}
	m, err := c.store.Append(c.chatID, "agent", text, nil, c.threadID, "final", components)
	if err != nil {
		log.Printf("[CHAT] Send DB append failed chatID=%s err=%v", c.chatID, err)
		return err
	}
	chatSubs, userSubs := c.hub.subscriberCounts(c.chatID, c.userID)
	log.Printf("[CHAT-DEBUG] Send chat=%s user=%d msgID=%d components=%d chatSubs=%d userSubs=%d",
		c.chatID, c.userID, m.ID, len(components), chatSubs, userSubs)
	c.hub.publish(*m)
	c.hub.publishToUser(c.userID, *m)
	if c.bus != nil {
		c.bus.Publish("chat.message", "channel-chat", *m)
	}
	return nil
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
	m, err := c.store.Append(c.chatID, "agent", content, nil, c.threadID, "final", components)
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
	m, err := c.store.Append(c.chatID, "agent", content, nil, c.threadID, "final", components)
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
	m, err := c.store.Append(c.chatID, "agent", "Alert: "+props["title"].(string), nil, c.threadID, "final", components)
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

// Close is a no-op — nothing to tear down. The Channel interface
// requires it; the per-instance registry calls it on detach.
func (c *chatChannel) Close() {}

// IsActive is always true for the Apteva operator channel. Presence is
// a UI concern; agents can always leave durable messages for operators.
func (c *chatChannel) IsActive() bool { return true }

// --- Factory ----------------------------------------------------------

// chatChannelFactory builds one chatChannel per instance with the
// default chat id. Multi-chat per instance would return multiple
// factories or one factory that returns multiple channels; for v1 we
// ship the single-default case.
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
		chatID: chat.ID,
		userID: inst.UserID,
		store:  f.store,
		hub:    f.hub,
		bus:    f.bus,
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
