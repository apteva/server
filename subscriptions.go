package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Subscription struct {
	ID           string    `json:"id"`
	UserID       int64     `json:"user_id"`
	InstanceID   int64     `json:"instance_id"`
	ConnectionID int64     `json:"connection_id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Description  string    `json:"description"`
	WebhookPath  string    `json:"webhook_path"`
	Enabled      bool      `json:"enabled"`
	ThreadID     string    `json:"thread_id,omitempty"`
	ProjectID    string    `json:"project_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// --- Store methods ---

func (s *Store) CreateSubscription(userID, instanceID, connectionID int64, name, slug, description, webhookPath, encryptedSecret, threadID string) (*Subscription, error) {
	id := generateID()
	_, err := s.db.Exec(
		"INSERT INTO subscriptions (id, user_id, instance_id, connection_id, name, slug, description, webhook_path, encrypted_hmac_secret, thread_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, userID, instanceID, connectionID, name, slug, description, webhookPath, encryptedSecret, threadID,
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{ID: id, UserID: userID, InstanceID: instanceID, ConnectionID: connectionID, Name: name, Slug: slug, Description: description, WebhookPath: webhookPath, Enabled: true, ThreadID: threadID, CreatedAt: time.Now()}, nil
}

func (s *Store) ListSubscriptions(userID int64) ([]Subscription, error) {
	rows, err := s.db.Query(
		"SELECT id, instance_id, connection_id, name, slug, description, webhook_path, enabled, COALESCE(thread_id,''), created_at FROM subscriptions WHERE user_id = ?", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		var enabled int
		var createdAt string
		rows.Scan(&sub.ID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &createdAt)
		sub.UserID = userID
		sub.Enabled = enabled == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		subs = append(subs, sub)
	}
	return subs, nil
}

func (s *Store) GetSubscription(userID int64, id string) (*Subscription, error) {
	var sub Subscription
	var enabled int
	var createdAt string
	err := s.db.QueryRow(
		"SELECT id, instance_id, connection_id, name, slug, description, webhook_path, enabled, COALESCE(thread_id,''), created_at FROM subscriptions WHERE id = ? AND user_id = ?",
		id, userID,
	).Scan(&sub.ID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &createdAt)
	if err != nil {
		return nil, err
	}
	sub.UserID = userID
	sub.Enabled = enabled == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	return &sub, nil
}

func (s *Store) GetSubscriptionByPath(webhookPath string) (*Subscription, string, error) {
	var sub Subscription
	var enabled int
	var encSecret, createdAt string
	err := s.db.QueryRow(
		"SELECT id, user_id, instance_id, connection_id, name, slug, description, webhook_path, encrypted_hmac_secret, enabled, COALESCE(thread_id,''), created_at FROM subscriptions WHERE webhook_path = ?",
		webhookPath,
	).Scan(&sub.ID, &sub.UserID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &encSecret, &enabled, &sub.ThreadID, &createdAt)
	if err != nil {
		return nil, "", err
	}
	sub.Enabled = enabled == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	return &sub, encSecret, nil
}

func (s *Store) DeleteSubscription(userID int64, id string) error {
	_, err := s.db.Exec("DELETE FROM subscriptions WHERE id = ? AND user_id = ?", id, userID)
	return err
}

func (s *Store) SetSubscriptionEnabled(userID int64, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec("UPDATE subscriptions SET enabled = ? WHERE id = ? AND user_id = ?", v, id, userID)
	return err
}

// --- HMAC verification ---

func verifyHMAC(body []byte, signature string, secret string) bool {
	if secret == "" || signature == "" {
		return true // no HMAC configured
	}
	// Strip "sha256=" prefix
	sig := strings.TrimPrefix(signature, "sha256=")
	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}

// --- HTTP Handlers ---

// POST /webhooks/:id — receives incoming webhooks from external services
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	subID := strings.TrimPrefix(r.URL.Path, "/webhooks/")
	if subID == "" {
		http.Error(w, "subscription ID required", http.StatusBadRequest)
		return
	}

	sub, encSecret, err := s.store.GetSubscriptionByPath(subID)
	if err != nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	if !sub.Enabled {
		http.Error(w, "subscription disabled", http.StatusForbidden)
		return
	}

	// Read body
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024)) // 1MB max
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Verify HMAC if configured
	if encSecret != "" {
		secret, err := Decrypt(s.secret, encSecret)
		if err == nil && secret != "" {
			sig := r.Header.Get("x-hub-signature-256")
			if sig == "" {
				sig = r.Header.Get("x-signature-256")
			}
			if sig == "" {
				sig = r.Header.Get("x-webhook-signature")
			}
			if !verifyHMAC(body, sig, secret) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
		}
	}

	// Find the target instance
	if sub.InstanceID == 0 {
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(sub.UserID, sub.InstanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	port := s.instances.GetPort(inst.ID)
	if port == 0 {
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}

	// Format and inject the event into core
	var payload any
	json.Unmarshal(body, &payload)
	payloadStr := string(body)
	if len(payloadStr) > 2000 {
		payloadStr = payloadStr[:2000] + "...[truncated]"
	}

	eventMsg := fmt.Sprintf("[webhook:%s] %s", sub.Slug, payloadStr)

	// POST to core's /event endpoint with optional thread targeting
	eventPayload := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		eventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(eventPayload)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	resp, err := http.Post(targetURL, "application/json", strings.NewReader(string(eventBody)))
	if err != nil {
		http.Error(w, "failed to deliver", http.StatusBadGateway)
		return
	}
	resp.Body.Close()

	writeJSON(w, map[string]string{"status": "delivered", "subscription": sub.ID})
}

// POST /subscriptions
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		InstanceID   int64    `json:"instance_id"`
		ConnectionID int64    `json:"connection_id"`
		Name         string   `json:"name"`
		Slug         string   `json:"slug"`
		Description  string   `json:"description"`
		HMACSecret   string   `json:"hmac_secret"`
		Events       []string `json:"events"`
		ThreadID     string   `json:"thread_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	// Generate unique webhook path
	webhookPath := generateToken(16)

	// Encrypt HMAC secret if provided
	encSecret := ""
	if body.HMACSecret != "" {
		enc, err := Encrypt(s.secret, body.HMACSecret)
		if err != nil {
			http.Error(w, "encryption failed", http.StatusInternalServerError)
			return
		}
		encSecret = enc
	}

	sub, err := s.store.CreateSubscription(userID, body.InstanceID, body.ConnectionID, body.Name, body.Slug, body.Description, webhookPath, encSecret, body.ThreadID)
	if err != nil {
		http.Error(w, "failed to create", http.StatusInternalServerError)
		return
	}

	webhookURL := fmt.Sprintf("http://127.0.0.1:%s/webhooks/%s", s.port, webhookPath)

	// Auto-register webhook with the external service if it has registration config
	var autoRegistered bool
	if body.ConnectionID > 0 {
		// Look up app from the connection's app_slug
		conn, encCreds, err := s.store.GetConnection(userID, body.ConnectionID)
		if err == nil && conn != nil {
			app := s.catalog.Get(conn.AppSlug)
			if app != nil && app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.ManualSetup == "" {
				plain, err := Decrypt(s.secret, encCreds)
				if err == nil {
					reg := app.Webhooks.Registration

					// Build auth headers from app config
					headers := map[string]string{"Content-Type": "application/json"}
					for k, v := range app.Auth.Headers {
						headers[k] = resolveCredTemplate(v, plain)
					}

					// Build request body
					reqBody := map[string]any{}
					if reg.Extra != nil {
						for k, v := range reg.Extra {
							reqBody[k] = v
						}
					}
					setField(reqBody, reg.URLField, webhookURL)
					if reg.SecretField != "" && body.HMACSecret != "" {
						setField(reqBody, reg.SecretField, body.HMACSecret)
					}
					if reg.EventsField != "" && len(body.Events) > 0 {
						setField(reqBody, reg.EventsField, body.Events)
					}

					regURL := strings.TrimSuffix(app.BaseURL, "/") + reg.Path
					regBody, _ := json.Marshal(reqBody)

					req, err := http.NewRequest(reg.Method, regURL, strings.NewReader(string(regBody)))
					if err == nil {
						for k, v := range headers {
							req.Header.Set(k, v)
						}
						resp, err := http.DefaultClient.Do(req)
						if err == nil {
							resp.Body.Close()
							if resp.StatusCode >= 200 && resp.StatusCode < 300 {
								autoRegistered = true
							}
						}
					}
				}
			}
		}
	}

	writeJSON(w, map[string]any{
		"subscription":    sub,
		"webhook_url":     webhookURL,
		"auto_registered": autoRegistered,
	})
}

// GET /subscriptions
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	subs, err := s.store.ListSubscriptions(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if subs == nil {
		subs = []Subscription{}
	}

	// Enrich with webhook URLs
	type subWithURL struct {
		Subscription
		WebhookURL string `json:"webhook_url"`
	}
	var enriched []subWithURL
	for _, sub := range subs {
		enriched = append(enriched, subWithURL{
			Subscription: sub,
			WebhookURL:   fmt.Sprintf("http://127.0.0.1:%s/webhooks/%s", s.port, sub.WebhookPath),
		})
	}
	writeJSON(w, enriched)
}

// DELETE /subscriptions/:id
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	id := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	if strings.HasSuffix(id, "/enable") || strings.HasSuffix(id, "/disable") {
		return // handled elsewhere
	}
	s.store.DeleteSubscription(userID, id)
	writeJSON(w, map[string]string{"status": "deleted"})
}

// POST /subscriptions/:id/enable or /disable
func (s *Server) handleToggleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")

	var id string
	var enable bool
	if strings.HasSuffix(path, "/enable") {
		id = strings.TrimSuffix(path, "/enable")
		enable = true
	} else if strings.HasSuffix(path, "/disable") {
		id = strings.TrimSuffix(path, "/disable")
		enable = false
	} else {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	s.store.SetSubscriptionEnabled(userID, id, enable)
	writeJSON(w, map[string]any{"status": "ok", "enabled": enable})
}

// POST /subscriptions/:id/test — send a fake test event to the instance
func (s *Server) handleTestSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
	id := strings.TrimSuffix(path, "/test")

	sub, err := s.store.GetSubscription(userID, id)
	if err != nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	// Parse optional body: { "event": "content.created", "payload": { ... } }
	var reqBody struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	json.NewDecoder(r.Body).Decode(&reqBody) // ignore errors — all fields optional

	if sub.InstanceID == 0 {
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(sub.UserID, sub.InstanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	testPort := s.instances.GetPort(inst.ID)
	if testPort == 0 {
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}

	// Use provided event type, or fall back to first from app config
	eventType := reqBody.Event
	if eventType == "" {
		eventType = "test.event"
		if app := s.catalog.Get(sub.Slug); app != nil && app.Webhooks != nil && len(app.Webhooks.Events) > 0 {
			eventType = app.Webhooks.Events[0].Name
		}
	}

	testPayload := map[string]any{
		"_test":     true,
		"event":     eventType,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	// Use provided payload or default
	if reqBody.Payload != nil {
		testPayload["data"] = reqBody.Payload
	} else {
		testPayload["data"] = map[string]any{
			"message":           "This is a test event. Your subscription is working correctly.",
			"subscription_id":   sub.ID,
			"subscription_name": sub.Name,
		}
	}

	payloadBytes, _ := json.Marshal(testPayload)

	eventMsg := fmt.Sprintf("[webhook:%s] %s", sub.Slug, string(payloadBytes))
	testEventPayload := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		testEventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(testEventPayload)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/event", testPort)

	resp, err := http.Post(targetURL, "application/json", strings.NewReader(string(eventBody)))
	if err != nil {
		http.Error(w, "failed to deliver test event", http.StatusBadGateway)
		return
	}
	resp.Body.Close()

	writeJSON(w, map[string]any{
		"status":  "delivered",
		"event":   eventType,
		"payload": testPayload,
	})
}

// resolveCredTemplate replaces {{key}} placeholders with credential values
func resolveCredTemplate(template string, credsJSON string) string {
	var creds map[string]string
	json.Unmarshal([]byte(credsJSON), &creds)
	result := template
	for k, v := range creds {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result
}

// setField sets a value at a dot-notation path in a map
func setField(obj map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := obj
	for i := 0; i < len(parts)-1; i++ {
		if _, ok := current[parts[i]]; !ok {
			current[parts[i]] = map[string]any{}
		}
		current = current[parts[i]].(map[string]any)
	}
	current[parts[len(parts)-1]] = value
}
