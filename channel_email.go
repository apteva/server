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
)

const agentMailBase = "https://api.agentmail.to/v0"

// EmailGateway manages a single AgentMail API key shared across all
// instances in a project. Each instance gets its own inbox.
type EmailGateway struct {
	apiKey string

	mu       sync.RWMutex
	mappings map[string]*emailMapping // inbox_id → mapping
}

type emailMapping struct {
	instanceID int64
	inboxID    string
	email      string // full address like agent-ops@agentmail.to
	registry   *ChannelRegistry
	sendEvent  func(text, threadID string)
}

func NewEmailGateway(apiKey string) *EmailGateway {
	return &EmailGateway{
		apiKey:   apiKey,
		mappings: make(map[string]*emailMapping),
	}
}

// Validate checks that the API key works.
func (g *EmailGateway) Validate() error {
	// List inboxes as a health check
	var result struct {
		Inboxes []any  `json:"inboxes"`
		Name    string `json:"name"`
	}
	err := g.apiCall("GET", "/inboxes?limit=1", nil, &result)
	if err != nil {
		return fmt.Errorf("agentmail auth failed: %w", err)
	}
	if result.Name != "" {
		return fmt.Errorf("agentmail: %s", result.Name)
	}
	return nil
}

// CreateInbox creates a new inbox for an instance.
func (g *EmailGateway) CreateInbox(displayName, clientID string) (inboxID, email string, err error) {
	body := map[string]string{
		"display_name": displayName,
		"client_id":    clientID,
	}
	var result struct {
		InboxID string `json:"inbox_id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := g.apiCall("POST", "/inboxes", body, &result); err != nil {
		return "", "", err
	}
	if result.InboxID == "" {
		msg := result.Message
		if msg == "" {
			msg = result.Name
		}
		return "", "", fmt.Errorf("agentmail create inbox: %s", msg)
	}
	return result.InboxID, result.Email, nil
}

// RegisterWebhook registers a webhook for inbound messages.
func (g *EmailGateway) RegisterWebhook(url, clientID string, inboxIDs []string) (webhookID, secret string, err error) {
	body := map[string]any{
		"url":         url,
		"event_types": []string{"message.received"},
		"client_id":   clientID,
	}
	if len(inboxIDs) > 0 {
		body["inbox_ids"] = inboxIDs
	}
	var result struct {
		WebhookID string `json:"webhook_id"`
		Secret    string `json:"secret"`
		Name      string `json:"name"`
		Message   string `json:"message"`
	}
	if err := g.apiCall("POST", "/webhooks", body, &result); err != nil {
		return "", "", err
	}
	if result.WebhookID == "" {
		msg := result.Message
		if msg == "" {
			msg = result.Name
		}
		return "", "", fmt.Errorf("agentmail create webhook: %s", msg)
	}
	return result.WebhookID, result.Secret, nil
}

// SendMessage sends an email from an inbox.
func (g *EmailGateway) SendMessage(inboxID, to, subject, text string) error {
	body := map[string]any{
		"to":      []string{to},
		"subject": subject,
		"text":    text,
	}
	var result struct {
		MessageID string `json:"message_id"`
		Name      string `json:"name"`
		Message   string `json:"message"`
	}
	path := fmt.Sprintf("/inboxes/%s/messages/send", inboxID)
	if err := g.apiCall("POST", path, body, &result); err != nil {
		return err
	}
	if result.MessageID == "" && result.Name != "" {
		return fmt.Errorf("agentmail send: %s — %s", result.Name, result.Message)
	}
	return nil
}

// MapInbox links an AgentMail inbox to an agent instance.
func (g *EmailGateway) MapInbox(instanceID int64, inboxID, email string, registry *ChannelRegistry, sendEvent func(string, string)) {
	g.mu.Lock()
	defer g.mu.Unlock()

	m := &emailMapping{
		instanceID: instanceID,
		inboxID:    inboxID,
		email:      email,
		registry:   registry,
		sendEvent:  sendEvent,
	}
	g.mappings[inboxID] = m

	ch := &EmailChannel{
		inboxID: inboxID,
		email:   email,
		gateway: g,
	}
	registry.Register(ch)

	log.Printf("[EMAIL] mapped %s (inbox %s) → instance %d", email, inboxID, instanceID)
}

// UnmapInstance removes all mappings for an instance.
func (g *EmailGateway) UnmapInstance(instanceID int64) {
	g.mu.Lock()
	var toRemove []string
	for id, m := range g.mappings {
		if m.instanceID == instanceID {
			m.registry.Unregister("email:" + m.email)
			toRemove = append(toRemove, id)
		}
	}
	for _, id := range toRemove {
		delete(g.mappings, id)
	}
	g.mu.Unlock()
}

// HandleInbound processes an inbound webhook from AgentMail.
func (g *EmailGateway) HandleInbound(payload json.RawMessage) {
	var event struct {
		EventType string `json:"event_type"`
		Message   struct {
			InboxID string `json:"inbox_id"`
			From    string `json:"from"`
			Subject string `json:"subject"`
			Text    string `json:"text"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	if event.EventType != "message.received" {
		return
	}

	g.mu.RLock()
	m, ok := g.mappings[event.Message.InboxID]
	g.mu.RUnlock()
	if !ok {
		return
	}

	text := event.Message.Text
	if text == "" {
		text = "(no text body)"
	}

	from := event.Message.From
	ev := fmt.Sprintf("[email:%s] %s", from, text)
	m.sendEvent(ev, "main")
}

func (g *EmailGateway) apiCall(method, path string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, agentMailBase+path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
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

// --- EmailChannel (Channel interface) ---

// EmailChannel implements Channel for a single AgentMail inbox.
// When Send is called with a "to" address embedded in the channel ID
// (email:user@example.com), it sends to that address. For broadcast
// sends the last known sender is used.
type EmailChannel struct {
	inboxID string
	email   string // inbox address
	gateway *EmailGateway

	mu         sync.Mutex
	lastSender string // last inbound sender for reply context
}

func (c *EmailChannel) ID() string {
	return "email:" + c.email
}

func (c *EmailChannel) Send(text string) error {
	c.mu.Lock()
	to := c.lastSender
	c.mu.Unlock()
	if to == "" {
		return fmt.Errorf("email channel: no recipient — nobody has emailed this inbox yet")
	}
	subject := emailSubject(text)
	return c.gateway.SendMessage(c.inboxID, to, subject, text)
}

// SendTo sends to a specific email address.
func (c *EmailChannel) SendTo(to, text string) error {
	subject := emailSubject(text)
	return c.gateway.SendMessage(c.inboxID, to, subject, text)
}

// SetLastSender records who last emailed this inbox, for reply context.
func (c *EmailChannel) SetLastSender(from string) {
	c.mu.Lock()
	c.lastSender = from
	c.mu.Unlock()
}

func (c *EmailChannel) Status(text, level string) error {
	return c.Send(fmt.Sprintf("[%s] %s", strings.ToUpper(level), text))
}

func (c *EmailChannel) Close() {}

// emailSubject extracts a subject from the message text — first line
// truncated to 80 chars.
func emailSubject(text string) string {
	line := strings.SplitN(text, "\n", 2)[0]
	line = strings.TrimSpace(line)
	if len(line) > 80 {
		line = line[:77] + "..."
	}
	if line == "" {
		line = "Agent message"
	}
	return line
}
