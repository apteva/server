package channelchat

import (
	"fmt"
	"log"

	"github.com/apteva/server/apps/framework"
)

// chatChannel is the Channel implementation the agent reaches via
// channels_respond(channel="chat", text=...). It writes the agent's
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

// ID returns the string the agent uses in channels_respond(channel=...).
// One chat per instance → id is always "chat".
func (c *chatChannel) ID() string { return "chat" }

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

// Status writes a system-role message so status lines show up in the
// chat transcript but are visually distinguishable from agent replies.
// Level (info/warn/alert) is prefixed onto the content; the dashboard
// renders each role differently. System messages do NOT publish to the
// wildcard hub — they're noise for the notifications tray, which only
// surfaces user-addressable agent replies.
func (c *chatChannel) Status(text, level string) error {
	if level == "" {
		level = "info"
	}
	body := "[" + level + "] " + text
	m, err := c.store.Append(c.chatID, "system", body, nil, c.threadID, "final", nil)
	if err != nil {
		return err
	}
	c.hub.publish(*m)
	return nil
}

// Close is a no-op — nothing to tear down. The Channel interface
// requires it; the per-instance registry calls it on detach.
func (c *chatChannel) Close() {}

// IsActive tells the channels MCP whether to advertise this channel
// as a place the agent can respond to right now. For chat, "active"
// means at least one SSE subscriber is connected to the stream — i.e.
// a dashboard / CLI has the chat open. When nobody's listening, the
// agent should see the channel as absent (same way CLIBridge reports
// IsConnected == false) so it doesn't reflexively reply to every
// inbound event on chat. The apteva-server's AvailableChannels
// function reads this method via the activeChannel interface.
func (c *chatChannel) IsActive() bool {
	if c.hub == nil {
		return false
	}
	// No log here — AvailableChannels already logs per-channel decisions
	// with the gated/ungated reason. Logging here too would double up.
	return c.hub.hasSubscribers(c.chatID)
}

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
	return "chat"
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
