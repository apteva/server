package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SlackGateway manages a single Slack app connection shared across all
// instances. Uses Socket Mode (WebSocket) for receiving messages and
// the Web API for sending. One bot token, many channels — each Slack
// channel is mapped to a different agent instance.
type SlackGateway struct {
	botToken string // xoxb-... for Web API (send messages, list channels)
	appToken string // xapp-... for Socket Mode (receive events via WebSocket)

	mu       sync.RWMutex
	mappings map[string]*slackMapping // slack channel ID → mapping
	channels map[string]*SlackChannel // slack channel ID → Channel impl
	botID    string                   // our own bot user ID (filter self-messages)

	// User display name cache to avoid hitting users.info on every message.
	userMu    sync.Mutex
	userCache map[string]string // Slack user ID → display name

	stopCh  chan struct{}
	stopped bool
}

type slackMapping struct {
	instanceID int64
	channelID  string // Slack channel ID (C...)
	name       string // human-readable channel name
	registry   *ChannelRegistry
	sendEvent  func(text, threadID string)
}

func NewSlackGateway(botToken, appToken string) *SlackGateway {
	return &SlackGateway{
		botToken:  botToken,
		appToken:  appToken,
		mappings:  make(map[string]*slackMapping),
		channels:  make(map[string]*SlackChannel),
		userCache: make(map[string]string),
		stopCh:    make(chan struct{}),
	}
}

// Start authenticates with Slack and launches the Socket Mode loop.
func (g *SlackGateway) Start() error {
	var auth struct {
		OK     bool   `json:"ok"`
		UserID string `json:"user_id"`
		Error  string `json:"error"`
	}
	if err := g.apiCall("auth.test", nil, &auth); err != nil {
		return fmt.Errorf("slack auth.test failed: %w", err)
	}
	if !auth.OK {
		return fmt.Errorf("slack auth.test: %s", auth.Error)
	}
	g.botID = auth.UserID
	log.Printf("[SLACK] authenticated as bot user %s", g.botID)

	go g.socketLoop()
	return nil
}

func (g *SlackGateway) Stop() {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return
	}
	g.stopped = true
	close(g.stopCh)
	for _, ch := range g.channels {
		ch.Close()
	}
	g.mu.Unlock()
}

// MapChannel links a Slack channel to an agent instance so messages in
// that Slack channel route to the instance, and the instance can send
// back via respond(channel="slack:<id>").
func (g *SlackGateway) MapChannel(instanceID int64, slackChannelID, channelName, instanceName string, registry *ChannelRegistry, sendEvent func(string, string)) {
	g.mu.Lock()
	defer g.mu.Unlock()

	m := &slackMapping{
		instanceID: instanceID,
		channelID:  slackChannelID,
		name:       channelName,
		registry:   registry,
		sendEvent:  sendEvent,
	}
	g.mappings[slackChannelID] = m

	ch := &SlackChannel{
		channelID:    slackChannelID,
		channelName:  channelName,
		instanceName: instanceName,
		gateway:      g,
	}
	g.channels[slackChannelID] = ch
	registry.Register(ch)

	log.Printf("[SLACK] mapped #%s (%s) → instance %d", channelName, slackChannelID, instanceID)
}

// UnmapChannel removes a Slack channel mapping.
func (g *SlackGateway) UnmapChannel(slackChannelID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if m, ok := g.mappings[slackChannelID]; ok {
		m.registry.Unregister("slack:" + slackChannelID)
	}
	delete(g.mappings, slackChannelID)
	delete(g.channels, slackChannelID)
}

// UnmapInstance removes all mappings for a given instance.
func (g *SlackGateway) UnmapInstance(instanceID int64) {
	g.mu.Lock()
	var toRemove []string
	for chID, m := range g.mappings {
		if m.instanceID == instanceID {
			toRemove = append(toRemove, chID)
		}
	}
	g.mu.Unlock()

	for _, chID := range toRemove {
		g.UnmapChannel(chID)
	}
}

// --- Socket Mode (receive) ---

// socketLoop maintains the Socket Mode WebSocket connection, reconnecting
// on any error. Slack requires a fresh URL from apps.connections.open on
// each connect — the URL is single-use and expires after 30s.
func (g *SlackGateway) socketLoop() {
	backoff := time.Second
	for {
		select {
		case <-g.stopCh:
			return
		default:
		}

		wsURL, err := g.openConnection()
		if err != nil {
			log.Printf("[SLACK] socket mode connect error: %v — retrying in %s", err, backoff)
			select {
			case <-g.stopCh:
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second
		log.Printf("[SLACK] socket mode connected")
		g.readLoop(wsURL)
		log.Printf("[SLACK] socket mode disconnected — reconnecting")
	}
}

func (g *SlackGateway) openConnection() (string, error) {
	req, _ := http.NewRequest("POST", "https://slack.com/api/apps.connections.open", nil)
	req.Header.Set("Authorization", "Bearer "+g.appToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("apps.connections.open: %s", result.Error)
	}
	return result.URL, nil
}

func (g *SlackGateway) readLoop(wsURL string) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("[SLACK] websocket dial error: %v", err)
		return
	}
	defer conn.Close()

	// Send our own pings every 30s to keep the connection alive.
	// Slack doesn't use WebSocket-level pings — it relies on
	// application-level messages. If the channel is idle, no messages
	// flow and the read deadline would expire. Our pings trigger a
	// WebSocket pong from Slack's server which resets the deadline.
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-g.stopCh:
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	for {
		select {
		case <-g.stopCh:
			return
		default:
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[SLACK] websocket read error: %v", err)
			}
			return
		}
		// Reset deadline on any application message too.
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var envelope struct {
			EnvelopeID string          `json:"envelope_id"`
			Type       string          `json:"type"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}

		if envelope.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": envelope.EnvelopeID})
			conn.WriteMessage(websocket.TextMessage, ack)
		}

		switch envelope.Type {
		case "events_api":
			g.handleEvent(envelope.Payload)
		case "disconnect":
			log.Printf("[SLACK] received disconnect request — will reconnect")
			return
		}
	}
}

// handleEvent processes an events_api envelope payload.
func (g *SlackGateway) handleEvent(payload json.RawMessage) {
	var wrapper struct {
		Event struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			User    string `json:"user"`
			Text    string `json:"text"`
			BotID   string `json:"bot_id"`
			SubType string `json:"subtype"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return
	}

	ev := wrapper.Event
	if ev.Type != "message" || ev.Text == "" {
		return
	}
	// Skip bot messages (our own replies, other bots, edits, etc.)
	if ev.BotID != "" || ev.SubType != "" || ev.User == g.botID {
		return
	}

	g.mu.RLock()
	m, ok := g.mappings[ev.Channel]
	g.mu.RUnlock()
	if !ok {
		return
	}

	username := g.resolveUser(ev.User)
	event := fmt.Sprintf("[slack:%s:%s] %s", username, ev.Channel, ev.Text)
	m.sendEvent(event, "main")
}

// --- Web API (send) ---

func (g *SlackGateway) sendMessage(channelID, text, displayName string) error {
	for len(text) > 0 {
		chunk := text
		if len(chunk) > 3900 {
			idx := strings.LastIndex(chunk[:3900], "\n")
			if idx < 0 {
				idx = 3900
			}
			chunk = text[:idx]
			text = text[idx:]
		} else {
			text = ""
		}

		payload := map[string]any{
			"channel": channelID,
			"text":    chunk,
		}
		if displayName != "" {
			payload["username"] = displayName
		}

		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		err := g.apiCall("chat.postMessage", payload, &result)
		if err != nil {
			return err
		}
		if !result.OK {
			return fmt.Errorf("chat.postMessage: %s", result.Error)
		}
	}
	return nil
}

func (g *SlackGateway) resolveUser(userID string) string {
	g.userMu.Lock()
	if name, ok := g.userCache[userID]; ok {
		g.userMu.Unlock()
		return name
	}
	g.userMu.Unlock()

	var result struct {
		OK   bool `json:"ok"`
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := g.apiCall("users.info", map[string]any{"user": userID}, &result); err != nil || !result.OK {
		return userID
	}

	name := result.User.Profile.DisplayName
	if name == "" {
		name = result.User.Profile.RealName
	}
	if name == "" {
		name = result.User.Name
	}
	if name == "" {
		name = userID
	}

	g.userMu.Lock()
	g.userCache[userID] = name
	// Cap cache at 200 entries
	if len(g.userCache) > 200 {
		for k := range g.userCache {
			delete(g.userCache, k)
			break
		}
	}
	g.userMu.Unlock()

	return name
}

func (g *SlackGateway) apiCall(method string, params map[string]any, result any) error {
	url := "https://slack.com/api/" + method
	var body io.Reader
	if params != nil {
		data, _ := json.Marshal(params)
		body = bytes.NewReader(data)
	}
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Authorization", "Bearer "+g.botToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// ListSlackChannels lists Slack channels the bot can see.
func (g *SlackGateway) ListSlackChannels() ([]SlackChannelInfo, error) {
	var result struct {
		OK       bool               `json:"ok"`
		Channels []SlackChannelInfo `json:"channels"`
		Error    string             `json:"error"`
	}
	err := g.apiCall("conversations.list", map[string]any{
		"types":            "public_channel,private_channel",
		"exclude_archived": true,
		"limit":            200,
	}, &result)
	if err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("conversations.list: %s", result.Error)
	}
	return result.Channels, nil
}

// JoinChannel makes the bot join a Slack channel. Silently succeeds
// if already a member. Only works for public channels — private
// channels require a manual invite.
func (g *SlackGateway) JoinChannel(channelID string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	err := g.apiCall("conversations.join", map[string]any{
		"channel": channelID,
	}, &result)
	if err != nil {
		return err
	}
	if !result.OK && result.Error != "already_in_channel" {
		return fmt.Errorf("conversations.join: %s", result.Error)
	}
	return nil
}

// SlackChannelInfo is the shape returned by conversations.list.
type SlackChannelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsMember   bool   `json:"is_member"`
	IsPrivate  bool   `json:"is_private"`
	NumMembers int    `json:"num_members"`
}

// --- SlackChannel (Channel interface) ---

// SlackChannel implements Channel for a single Slack channel.
type SlackChannel struct {
	channelID    string
	channelName  string
	instanceName string
	gateway      *SlackGateway
}

func (c *SlackChannel) ID() string {
	return "slack:" + c.channelID
}

func (c *SlackChannel) Send(text string) error {
	return c.gateway.sendMessage(c.channelID, text, c.instanceName)
}

func (c *SlackChannel) Status(text, level string) error {
	prefix := ""
	switch level {
	case "warn":
		prefix = ":warning: "
	case "alert":
		prefix = ":rotating_light: "
	}
	return c.gateway.sendMessage(c.channelID, prefix+text, c.instanceName)
}

func (c *SlackChannel) Close() {}
