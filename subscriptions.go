package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Subscription struct {
	ID                string    `json:"id"`
	UserID            int64     `json:"user_id"`
	InstanceID        int64     `json:"instance_id"`
	ConnectionID      int64     `json:"connection_id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	Description       string    `json:"description"`
	WebhookPath       string    `json:"webhook_path"`
	Enabled           bool      `json:"enabled"`
	ThreadID          string    `json:"thread_id,omitempty"`
	ProjectID         string    `json:"project_id,omitempty"`
	Events            []string  `json:"events"`
	ExternalWebhookID string    `json:"external_webhook_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// ListSubscriptionsByConnection returns every subscription bound to a
// given connection (all projects, all threads). Used by the connection
// delete cascade — before tearing down a connection we fetch its subs
// so we can unregister each upstream webhook and then remove the rows.
func (s *Store) ListSubscriptionsByConnection(userID, connectionID int64) ([]Subscription, error) {
	rows, err := s.db.Query(
		"SELECT id, instance_id, name, slug, webhook_path, COALESCE(external_webhook_id,'') FROM subscriptions WHERE user_id = ? AND connection_id = ?",
		userID, connectionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		rows.Scan(&sub.ID, &sub.InstanceID, &sub.Name, &sub.Slug, &sub.WebhookPath, &sub.ExternalWebhookID)
		sub.UserID = userID
		sub.ConnectionID = connectionID
		subs = append(subs, sub)
	}
	return subs, nil
}

// --- Store methods ---

func (s *Store) CreateSubscription(userID, instanceID, connectionID int64, name, slug, description, webhookPath, encryptedSecret, threadID, projectID string, events []string) (*Subscription, error) {
	id := generateID()
	eventsJSON := ""
	if len(events) > 0 {
		if b, merr := json.Marshal(events); merr == nil {
			eventsJSON = string(b)
		}
	}
	_, err := s.db.Exec(
		"INSERT INTO subscriptions (id, user_id, instance_id, connection_id, name, slug, description, webhook_path, encrypted_hmac_secret, thread_id, project_id, events) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, userID, instanceID, connectionID, name, slug, description, webhookPath, encryptedSecret, threadID, projectID, eventsJSON,
	)
	if err != nil {
		return nil, err
	}
	return &Subscription{ID: id, UserID: userID, InstanceID: instanceID, ConnectionID: connectionID, Name: name, Slug: slug, Description: description, WebhookPath: webhookPath, Enabled: true, ThreadID: threadID, ProjectID: projectID, Events: events, CreatedAt: time.Now()}, nil
}

func (s *Store) ListSubscriptions(userID int64, projectID ...string) ([]Subscription, error) {
	var rows *sql.Rows
	var err error
	const cols = "id, instance_id, connection_id, name, slug, description, webhook_path, enabled, COALESCE(thread_id,''), COALESCE(events,''), created_at"
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			"SELECT "+cols+" FROM subscriptions WHERE user_id = ? AND (project_id = ? OR project_id = '')", userID, projectID[0])
	} else {
		rows, err = s.db.Query(
			"SELECT "+cols+" FROM subscriptions WHERE user_id = ?", userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		var enabled int
		var createdAt, eventsJSON string
		rows.Scan(&sub.ID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &eventsJSON, &createdAt)
		sub.UserID = userID
		sub.Enabled = enabled == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		if eventsJSON != "" {
			json.Unmarshal([]byte(eventsJSON), &sub.Events)
		}
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

func (s *Store) SetSubscriptionExternalID(id, externalID string) {
	s.db.Exec("UPDATE subscriptions SET external_webhook_id = ? WHERE id = ?", externalID, id)
}

func (s *Store) GetSubscriptionExternalID(id string) string {
	var extID string
	s.db.QueryRow("SELECT COALESCE(external_webhook_id,'') FROM subscriptions WHERE id = ?", id).Scan(&extID)
	return extID
}

// GetSubscriptionByExternalID looks up the apteva subscription row whose
// external_webhook_id matches the given upstream id. Used by the
// Composio webhook ingress path to dispatch incoming trigger events to
// the right apteva subscription, and by the local webhook delete path
// when we only know the upstream id.
func (s *Store) GetSubscriptionByExternalID(userID int64, externalID string) (*Subscription, error) {
	const cols = "id, user_id, instance_id, connection_id, name, slug, description, webhook_path, enabled, COALESCE(thread_id,''), COALESCE(events,''), COALESCE(project_id,''), created_at"
	var (
		sub       Subscription
		enabled   int
		createdAt string
		eventsJSON string
	)
	var err error
	if userID > 0 {
		err = s.db.QueryRow(
			"SELECT "+cols+" FROM subscriptions WHERE external_webhook_id = ? AND user_id = ?",
			externalID, userID,
		).Scan(&sub.ID, &sub.UserID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &eventsJSON, &sub.ProjectID, &createdAt)
	} else {
		err = s.db.QueryRow(
			"SELECT "+cols+" FROM subscriptions WHERE external_webhook_id = ?",
			externalID,
		).Scan(&sub.ID, &sub.UserID, &sub.InstanceID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description, &sub.WebhookPath, &enabled, &sub.ThreadID, &eventsJSON, &sub.ProjectID, &createdAt)
	}
	if err != nil {
		return nil, err
	}
	sub.Enabled = enabled == 1
	sub.CreatedAt, _ = parseTime(createdAt)
	if eventsJSON != "" {
		json.Unmarshal([]byte(eventsJSON), &sub.Events)
	}
	sub.ExternalWebhookID = externalID
	return &sub, nil
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

// verifyStandardWebhook validates a payload signed per the Standard
// Webhooks spec (used by Composio, Svix, and others). The header format is:
//
//	webhook-id:        msg_xxx
//	webhook-timestamp: 1234567890  (unix seconds)
//	webhook-signature: v1,<base64(HMAC_SHA256(secret, id "." ts "." body))>
//
// webhook-signature may contain multiple space-separated versions; we
// accept if any v1 entry matches. Secret may be base64-encoded or raw
// bytes — we try both to tolerate both conventions.
func verifyStandardWebhook(body []byte, msgID, msgTS, sigHeader, secret string) bool {
	if secret == "" {
		return true
	}
	if msgID == "" || msgTS == "" || sigHeader == "" {
		return false
	}
	toSign := msgID + "." + msgTS + "." + string(body)
	// Try secret as raw bytes and as base64; Standard Webhooks typically
	// uses "whsec_<base64>" but we tolerate either form.
	secretBytes := []byte(secret)
	if stripped := strings.TrimPrefix(secret, "whsec_"); stripped != secret {
		if decoded, err := base64.StdEncoding.DecodeString(stripped); err == nil {
			secretBytes = decoded
		}
	}
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(toSign))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	// sigHeader may contain multiple versions: "v1,sig1 v1,sig2"
	for _, entry := range strings.Fields(sigHeader) {
		parts := strings.SplitN(entry, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		if hmac.Equal([]byte(parts[1]), []byte(expected)) {
			return true
		}
	}
	return false
}

// --- HTTP Handlers ---

// POST /webhooks/:token — unified webhook ingress.
//
// One endpoint handles every kind of incoming webhook, routed by what
// the opaque token matches in our DB:
//
//  1. Matches subscriptions.webhook_path  → per-subscription upstream
//     delivery. Used by local-template subs (SocialCast, Pushover, etc.)
//     that self-registered their own webhook with the upstream service
//     at create time. Validates HMAC with the per-subscription secret.
//
//  2. Matches providers.webhook_token     → provider-backed trigger
//     delivery. Used by Composio (today) and any other trigger backend
//     we add (Svix, n8n, etc.). Validates Standard Webhooks HMAC with
//     the per-provider signing secret stored in the encrypted blob,
//     then dispatches to a provider-specific delivery path that finds
//     the right apteva subscription by matching the inbound trigger id.
//
// Neither case uses authenticated sessions — these are public endpoints
// upstream services POST into, with HMAC as the only auth layer. The
// token in the URL is opaque random bytes (16 bytes / 32 hex chars),
// not a guessable id, so URL enumeration is not a concern.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log.Printf("[WEBHOOK-IN] %s %s remote=%s ua=%q content-type=%q content-length=%s",
		r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("User-Agent"),
		r.Header.Get("Content-Type"), r.Header.Get("Content-Length"))

	if r.Method != http.MethodPost {
		log.Printf("[WEBHOOK-IN] rejecting %s — POST only", r.Method)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimPrefix(r.URL.Path, "/webhooks/")
	if token == "" {
		log.Printf("[WEBHOOK-IN] empty token on path %q", r.URL.Path)
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	log.Printf("[WEBHOOK-IN] token=%s len=%d", token, len(token))

	// Dispatch 1: subscription-backed webhook. Try this first because
	// it's the common case for local templates and has a cheaper
	// lookup.
	sub, encSecret, err := s.store.GetSubscriptionByPath(token)
	if err == nil && sub != nil {
		log.Printf("[WEBHOOK-IN] matched subscription id=%s name=%q slug=%q enabled=%v", sub.ID, sub.Name, sub.Slug, sub.Enabled)
		s.handleSubscriptionWebhook(w, r, sub, encSecret)
		return
	}
	log.Printf("[WEBHOOK-IN] no subscription row for token=%s err=%v", token, err)

	// Dispatch 2: provider-backed trigger webhook. The token matches
	// providers.webhook_token; we find the provider, look up its
	// backend kind (Composio etc.), and route into the right delivery
	// flow.
	prov, encData, perr := s.store.FindProviderByWebhookToken(token)
	if perr == nil && prov != nil {
		log.Printf("[WEBHOOK-IN] matched provider id=%d name=%q", prov.ID, prov.Name)
		s.handleProviderTriggerWebhook(w, r, prov, encData)
		return
	}
	log.Printf("[WEBHOOK-IN] no provider row for token=%s err=%v", token, perr)

	// Neither matched.
	log.Printf("[WEBHOOK-IN] 404 token=%s — no subscription or provider row matched", token)
	http.Error(w, "unknown webhook token", http.StatusNotFound)
}

// handleSubscriptionWebhook is the delivery path for /webhooks/<token>
// when the token matches a subscription row. Factored out of the
// top-level handler so the unified entry point can dispatch cleanly
// between subscription-backed and provider-backed webhooks without
// nested early-returns.
func (s *Server) handleSubscriptionWebhook(w http.ResponseWriter, r *http.Request, sub *Subscription, encSecret string) {
	if sub == nil {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}

	if !sub.Enabled {
		log.Printf("[WEBHOOK] sub %s disabled — 403", sub.ID)
		http.Error(w, "subscription disabled", http.StatusForbidden)
		return
	}

	// Read body
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024)) // 1MB max
	if err != nil {
		log.Printf("[WEBHOOK] sub %s body read error: %v", sub.ID, err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	log.Printf("[WEBHOOK] sub %s received body len=%d", sub.ID, len(body))

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
			log.Printf("[WEBHOOK] sub %s HMAC check — sig header present=%v", sub.ID, sig != "")
			if !verifyHMAC(body, sig, secret) {
				log.Printf("[WEBHOOK] sub %s HMAC verification FAILED (sig=%q)", sub.ID, sig)
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
			log.Printf("[WEBHOOK] sub %s HMAC verified ok", sub.ID)
		} else if err != nil {
			log.Printf("[WEBHOOK] sub %s: failed to decrypt HMAC secret: %v — skipping verification", sub.ID, err)
		}
	} else {
		log.Printf("[WEBHOOK] sub %s has no HMAC secret — skipping verification", sub.ID)
	}

	// Find the target instance
	if sub.InstanceID == 0 {
		log.Printf("[WEBHOOK] sub %s: no instance configured", sub.ID)
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(sub.UserID, sub.InstanceID)
	if err != nil {
		log.Printf("[WEBHOOK] sub %s: instance %d not found: %v", sub.ID, sub.InstanceID, err)
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	port := s.instances.GetPort(inst.ID)
	if port == 0 {
		log.Printf("[WEBHOOK] sub %s: instance %d not running", sub.ID, inst.ID)
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[WEBHOOK] sub %s: delivering to instance %d port %d", sub.ID, inst.ID, port)

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
	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if ck := s.instances.GetCoreAPIKey(inst.ID); ck != "" {
		req.Header.Set("Authorization", "Bearer "+ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[WEBHOOK] deliver error: %v", err)
		http.Error(w, "failed to deliver", http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[WEBHOOK] core rejected %d: %s", resp.StatusCode, string(respBody))
		http.Error(w, fmt.Sprintf("core rejected: %d %s", resp.StatusCode, string(respBody)), http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]string{"status": "delivered", "subscription": sub.ID})
}

// POST /webhooks/composio/:project_id — unified ingress for every Composio
// trigger event in a given apteva project. Composio POSTs all triggers
// for a project to this one URL, signed with the per-project signing
// secret we stashed in the provider blob at subscription-create time.
//
// We look up the matching apteva subscription row by the trigger_nano_id
// field in the payload (stored as external_webhook_id on our side), then
// route through the same core /event delivery path real-service webhooks
// use — message prefix "[trigger:<slug>] …", optional thread targeting,
// same Bearer auth to core.
// handleProviderTriggerWebhook handles /webhooks/<token> deliveries when
// the token matches a providers.webhook_token. Today this is the
// Composio trigger ingress; future trigger backends (Svix, n8n, ...)
// will dispatch on prov.Name with their own validation + envelope
// shapes.
func (s *Server) handleProviderTriggerWebhook(w http.ResponseWriter, r *http.Request, prov *Provider, encData string) {
	userID := prov.UserID
	if userID == 0 {
		log.Printf("[PROVIDER-HOOK] provider row missing user_id")
		http.Error(w, "invalid provider row", http.StatusInternalServerError)
		return
	}

	// Only Composio for now. When we add more backends, switch on
	// prov.Name (or a dedicated provider_kind column) and route to the
	// right validator + envelope parser.
	if !strings.EqualFold(prov.Name, "Composio") {
		log.Printf("[PROVIDER-HOOK] unsupported provider %q", prov.Name)
		http.Error(w, "unsupported provider", http.StatusNotImplemented)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 5*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	plain, err := Decrypt(s.secret, encData)
	if err != nil {
		log.Printf("[COMPOSIO-HOOK] decrypt provider blob: %v", err)
		http.Error(w, "decrypt failed", http.StatusInternalServerError)
		return
	}
	var blob map[string]string
	_ = json.Unmarshal([]byte(plain), &blob)
	secret := blob["composio_webhook_secret"]
	if secret == "" {
		log.Printf("[COMPOSIO-HOOK] provider %d: no signing secret cached", prov.ID)
		http.Error(w, "webhook subscription not bootstrapped", http.StatusServiceUnavailable)
		return
	}

	msgID := r.Header.Get("webhook-id")
	msgTS := r.Header.Get("webhook-timestamp")
	sigHeader := r.Header.Get("webhook-signature")
	if !verifyStandardWebhook(body, msgID, msgTS, sigHeader, secret) {
		log.Printf("[COMPOSIO-HOOK] provider %d: invalid signature (msgID=%s)", prov.ID, msgID)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Parse the envelope. Composio V3 wraps every trigger in
	//   { "type": "trigger.event", "data": {
	//        "trigger_nano_id": "...",
	//        "trigger_slug": "GOOGLESHEETS_CELL_RANGE_VALUES_CHANGED",
	//        "connected_account_id": "...",
	//        "user_id": "...",
	//        "payload": { ...upstream event... }
	//   }}
	var envelope struct {
		Type string `json:"type"`
		Data struct {
			TriggerNanoID      string         `json:"trigger_nano_id"`
			TriggerID          string         `json:"trigger_id"`
			TriggerSlug        string         `json:"trigger_slug"`
			ConnectedAccountID string         `json:"connected_account_id"`
			UserID             string         `json:"user_id"`
			Payload            map[string]any `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		log.Printf("[COMPOSIO-HOOK] malformed envelope: %v", err)
		http.Error(w, "invalid envelope", http.StatusBadRequest)
		return
	}
	triggerID := envelope.Data.TriggerNanoID
	if triggerID == "" {
		triggerID = envelope.Data.TriggerID
	}
	if triggerID == "" {
		log.Printf("[COMPOSIO-HOOK] envelope missing trigger id; body=%s", string(body))
		http.Error(w, "no trigger id in envelope", http.StatusBadRequest)
		return
	}
	log.Printf("[COMPOSIO-HOOK] provider=%d trigger_slug=%s trigger_id=%s connected_account=%s",
		prov.ID, envelope.Data.TriggerSlug, triggerID, envelope.Data.ConnectedAccountID)

	// Look up the apteva subscription row whose external_webhook_id
	// matches the Composio trigger instance id.
	sub, err := s.store.GetSubscriptionByExternalID(userID, triggerID)
	if err != nil || sub == nil {
		log.Printf("[COMPOSIO-HOOK] no apteva subscription for trigger_id=%s — ignoring but 200-ing", triggerID)
		// Return 200 so Composio doesn't retry forever on an
		// orphaned instance. The row should exist if sub create
		// completed successfully; dangling ids are usually leftover
		// from interrupted sub creates.
		writeJSON(w, map[string]string{"status": "ignored"})
		return
	}

	if !sub.Enabled {
		log.Printf("[COMPOSIO-HOOK] sub %s disabled — ignoring", sub.ID)
		writeJSON(w, map[string]string{"status": "disabled"})
		return
	}

	// Find the target instance + its local core port + auth key.
	if sub.InstanceID == 0 {
		log.Printf("[COMPOSIO-HOOK] sub %s has no instance", sub.ID)
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
		log.Printf("[COMPOSIO-HOOK] instance %d not running", inst.ID)
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}

	// Format the event for the agent. Prefer the trigger's own payload
	// when present — that's the upstream app event the user actually
	// cares about. Fall back to the whole envelope if payload is empty.
	var payloadStr string
	if len(envelope.Data.Payload) > 0 {
		b, _ := json.Marshal(envelope.Data.Payload)
		payloadStr = string(b)
	} else {
		payloadStr = string(body)
	}
	if len(payloadStr) > 4000 {
		payloadStr = payloadStr[:4000] + "...[truncated]"
	}
	slug := envelope.Data.TriggerSlug
	if slug == "" {
		slug = sub.Slug
	}
	eventMsg := fmt.Sprintf("[trigger:%s] %s", slug, payloadStr)
	eventPayload := map[string]string{"message": eventMsg}
	if sub.ThreadID != "" {
		eventPayload["thread_id"] = sub.ThreadID
	}
	eventBody, _ := json.Marshal(eventPayload)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/event", port)
	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if ck := s.instances.GetCoreAPIKey(inst.ID); ck != "" {
		req.Header.Set("Authorization", "Bearer "+ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[COMPOSIO-HOOK] deliver error: %v", err)
		http.Error(w, "failed to deliver", http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[COMPOSIO-HOOK] core rejected %d: %s", resp.StatusCode, string(respBody))
		http.Error(w, fmt.Sprintf("core rejected: %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	log.Printf("[COMPOSIO-HOOK] delivered sub=%s trigger=%s", sub.ID, slug)
	writeJSON(w, map[string]string{"status": "delivered", "subscription": sub.ID})
}

// POST /subscriptions
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		InstanceID   int64          `json:"instance_id"`
		ConnectionID int64          `json:"connection_id"`
		Name         string         `json:"name"`
		Slug         string         `json:"slug"`
		Description  string         `json:"description"`
		HMACSecret   string         `json:"hmac_secret"`
		Events       []string       `json:"events"`
		ThreadID     string         `json:"thread_id"`
		ProjectID    string         `json:"project_id"`
		// Composio-source only: which Composio trigger template to
		// instantiate and its per-trigger config (e.g. spreadsheet_id,
		// range, channel_id). Ignored for local-source subscriptions.
		TriggerSlug   string         `json:"trigger_slug"`
		TriggerConfig map[string]any `json:"trigger_config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	// Short-circuit: Composio-source subscriptions go through their own
	// flow (webhook subscription bootstrap + trigger instance upsert).
	// They don't use the per-subscription HMAC secret or generated
	// webhook path — all deliveries funnel through the one project-level
	// /webhooks/composio/<project> URL validated with the provider-
	// level signing secret.
	if body.ConnectionID > 0 {
		if conn, _, cerr := s.store.GetConnection(userID, body.ConnectionID); cerr == nil && conn != nil && conn.Source == "composio" {
			s.createComposioSubscription(w, userID, body.InstanceID, body.ConnectionID, body.Name, body.Slug, body.Description, body.ThreadID, body.ProjectID, body.TriggerSlug, body.TriggerConfig, conn)
			return
		}
	}

	// Generate unique webhook path
	webhookPath := generateToken(16)

	// Auto-generate an HMAC secret when the caller didn't supply one —
	// we want HMAC validation to always be on. The plaintext is passed
	// to the upstream service during auto-registration so both sides
	// share the same secret; the encrypted copy is stored locally.
	if body.HMACSecret == "" {
		body.HMACSecret = generateToken(32)
	}
	encSecret, err := Encrypt(s.secret, body.HMACSecret)
	if err != nil {
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	sub, err := s.store.CreateSubscription(userID, body.InstanceID, body.ConnectionID, body.Name, body.Slug, body.Description, webhookPath, encSecret, body.ThreadID, body.ProjectID, body.Events)
	if err != nil {
		http.Error(w, "failed to create", http.StatusInternalServerError)
		return
	}

	webhookURL := s.webhookURL(webhookPath)
	log.Printf("[SUB-CREATE] sub=%s name=%q slug=%q conn=%d instance=%d webhook_url=%s events=%v",
		sub.ID, body.Name, body.Slug, body.ConnectionID, body.InstanceID, webhookURL, body.Events)

	// Auto-register webhook with the external service if it has registration config
	var autoRegistered bool
	if body.ConnectionID > 0 {
		conn, encCreds, err := s.store.GetConnection(userID, body.ConnectionID)
		if err != nil || conn == nil {
			log.Printf("[SUB-CREATE] skip auto-reg: connection %d lookup failed: err=%v conn=%v", body.ConnectionID, err, conn)
		} else {
			log.Printf("[SUB-CREATE] connection %d → app=%s name=%q", conn.ID, conn.AppSlug, conn.Name)
			app := s.catalog.Get(conn.AppSlug)
			switch {
			case app == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %q not found in catalog", conn.AppSlug)
			case app.Webhooks == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %s has no webhooks config", conn.AppSlug)
			case app.Webhooks.Registration == nil:
				log.Printf("[SUB-CREATE] skip auto-reg: app %s has no webhooks.registration config", conn.AppSlug)
			case app.Webhooks.Registration.ManualSetup != "":
				log.Printf("[SUB-CREATE] skip auto-reg: app %s requires manual setup (%s)", conn.AppSlug, app.Webhooks.Registration.ManualSetup)
			default:
				plain, derr := Decrypt(s.secret, encCreds)
				if derr != nil {
					log.Printf("[SUB-CREATE] skip auto-reg: decrypt creds failed: %v", derr)
				} else {
					reg := app.Webhooks.Registration

					headers := map[string]string{"Content-Type": "application/json"}
					for k, v := range app.Auth.Headers {
						headers[k] = resolveCredTemplate(v, plain)
					}

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

					// Redact auth header values for the log line
					logHeaders := make(map[string]string, len(headers))
					for k, v := range headers {
						if k == "Content-Type" {
							logHeaders[k] = v
						} else if len(v) > 8 {
							logHeaders[k] = v[:4] + "…" + v[len(v)-4:]
						} else {
							logHeaders[k] = "***"
						}
					}
					log.Printf("[SUB-CREATE] → %s %s headers=%v body=%s", reg.Method, regURL, logHeaders, string(regBody))

					req, rerr := http.NewRequest(reg.Method, regURL, strings.NewReader(string(regBody)))
					if rerr != nil {
						log.Printf("[SUB-CREATE] build request failed: %v", rerr)
					} else {
						for k, v := range headers {
							req.Header.Set(k, v)
						}
						resp, herr := http.DefaultClient.Do(req)
						if herr != nil {
							log.Printf("[SUB-CREATE] HTTP error: %v", herr)
						} else {
							respBody, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							log.Printf("[SUB-CREATE] ← %d %s", resp.StatusCode, string(respBody))
							if resp.StatusCode >= 200 && resp.StatusCode < 300 {
								autoRegistered = true
								if reg.IDField != "" {
									var respData map[string]any
									if json.Unmarshal(respBody, &respData) == nil {
										extID := extractJSONPath(respData, reg.IDField)
										log.Printf("[SUB-CREATE] extracted external_id=%q via path %q", extID, reg.IDField)
										if extID != "" {
											s.store.SetSubscriptionExternalID(sub.ID, extID)
										}
									} else {
										log.Printf("[SUB-CREATE] response body is not JSON, cannot extract id")
									}
								}
							}
						}
					}
				}
			}
		}
	} else {
		log.Printf("[SUB-CREATE] skip auto-reg: connection_id=0")
	}
	log.Printf("[SUB-CREATE] done sub=%s auto_registered=%v", sub.ID, autoRegistered)

	writeJSON(w, map[string]any{
		"subscription":    sub,
		"webhook_url":     webhookURL,
		"auto_registered": autoRegistered,
	})
}

// createComposioSubscription handles subscription creation when the
// connection's source is composio. The flow is independent of the
// local-app auto-register path because:
//
//  1. Delivery is project-level on Composio's side: every trigger event
//     for the whole (user, project) tuple lands on the one
//     /webhooks/composio/<project_id> URL. No per-sub webhook path.
//  2. HMAC validation happens at the project level with the
//     provider-stored signing secret, not with a per-subscription
//     secret. So we skip the per-sub secret generation entirely.
//  3. Events map 1:1 to trigger slugs. If the caller wants to react to
//     "cell changed" AND "new row added", they create two subscription
//     rows each with a distinct trigger_slug — matches Composio's own
//     trigger-per-event model.
func (s *Server) createComposioSubscription(
	w http.ResponseWriter,
	userID int64,
	instanceID int64,
	connectionID int64,
	name, slug, description, threadID, projectID, triggerSlug string,
	triggerConfig map[string]any,
	conn *Connection,
) {
	if triggerSlug == "" {
		http.Error(w, "trigger_slug required for composio-source subscriptions", http.StatusBadRequest)
		return
	}
	if conn.ProviderID == 0 {
		http.Error(w, "composio connection missing provider_id", http.StatusBadRequest)
		return
	}
	if conn.ExternalID == "" {
		http.Error(w, "composio connection missing external_id (connected_account_id)", http.StatusBadRequest)
		return
	}
	log.Printf("[SUB-CREATE] composio flow user=%d conn=%d trigger=%s account=%s",
		userID, connectionID, triggerSlug, conn.ExternalID)

	// 1. Ensure the project-level Composio webhook subscription exists
	//    and its signing secret is cached on the provider blob.
	if _, err := s.ensureComposioWebhookSubscription(userID, conn.ProviderID, projectID); err != nil {
		log.Printf("[SUB-CREATE] composio webhook bootstrap failed: %v", err)
		http.Error(w, "composio webhook bootstrap failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// 2. Upsert the trigger instance for this specific connected account.
	client, err := s.composioClientFor(userID, conn.ProviderID)
	if err != nil {
		http.Error(w, "composio client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	triggerID, err := client.UpsertTriggerInstance(triggerSlug, conn.ExternalID, triggerConfig)
	if err != nil {
		log.Printf("[SUB-CREATE] composio upsert trigger %s failed: %v", triggerSlug, err)
		http.Error(w, "composio trigger upsert: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("[SUB-CREATE] composio trigger upserted: slug=%s trigger_id=%s", triggerSlug, triggerID)

	// 3. Persist the apteva subscription row. No per-sub HMAC secret —
	//    validation happens at the project level via the ingress
	//    handler. We store an empty webhook_path (not used for
	//    composio) and set events to [trigger_slug] so the list view
	//    shows something meaningful.
	events := []string{triggerSlug}
	sub, err := s.store.CreateSubscription(
		userID,
		instanceID,
		connectionID,
		name,
		slug,
		description,
		"",   // webhook_path: unused for composio
		"",   // encrypted_hmac_secret: unused for composio
		threadID,
		projectID,
		events,
	)
	if err != nil {
		// Best-effort rollback upstream so we don't leak trigger
		// instances for rows that never committed locally.
		_ = client.DeleteTriggerInstance(triggerID)
		http.Error(w, "failed to create subscription: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 4. Bind the subscription to its Composio trigger instance id so
	//    the webhook ingress path can look it up on every event.
	s.store.SetSubscriptionExternalID(sub.ID, triggerID)
	sub.ExternalWebhookID = triggerID
	sub.Events = events

	// Resolve the project-level webhook URL from the provider's token
	// so the create response shows the user where deliveries will land.
	var webhookToken string
	s.store.db.QueryRow("SELECT COALESCE(webhook_token,'') FROM providers WHERE id = ?", conn.ProviderID).Scan(&webhookToken)
	webhookURL := s.publicBaseURL() + "/webhooks/" + webhookToken

	writeJSON(w, map[string]any{
		"subscription":    sub,
		"webhook_url":     webhookURL,
		"auto_registered": true,
		"trigger_id":      triggerID,
		"trigger_slug":    triggerSlug,
	})
}

// GET /subscriptions
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projectID := r.URL.Query().Get("project_id")
	subs, err := s.store.ListSubscriptions(userID, projectID)
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
			WebhookURL:   s.webhookURL(sub.WebhookPath),
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

	// Unregister from external service if we have an external webhook ID
	extID := s.store.GetSubscriptionExternalID(id)
	if extID != "" {
		sub, _ := s.store.GetSubscription(userID, id)
		if sub != nil && sub.ConnectionID > 0 {
			conn, encCreds, err := s.store.GetConnection(userID, sub.ConnectionID)
			if err == nil && conn != nil {
				// Composio-source: delete the Composio trigger
				// instance through the API instead of calling an
				// app template's delete_path.
				if conn.Source == "composio" {
					if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil {
						if derr := client.DeleteTriggerInstance(extID); derr != nil {
							log.Printf("[SUB-DELETE] composio delete trigger %s: %v", extID, derr)
						} else {
							log.Printf("[SUB-DELETE] composio trigger %s deleted", extID)
						}
					}
				} else {
					app := s.catalog.Get(conn.AppSlug)
					if app != nil && app.Webhooks != nil && app.Webhooks.Registration != nil && app.Webhooks.Registration.DeletePath != "" {
						plain, err := Decrypt(s.secret, encCreds)
						if err == nil {
							reg := app.Webhooks.Registration
							deletePath := strings.ReplaceAll(reg.DeletePath, "{id}", extID)
							deleteURL := strings.TrimSuffix(app.BaseURL, "/") + deletePath

							headers := map[string]string{}
							for k, v := range app.Auth.Headers {
								headers[k] = resolveCredTemplate(v, plain)
							}

							method := reg.DeleteMethod
							if method == "" {
								method = "DELETE"
							}

							req, err := http.NewRequest(method, deleteURL, nil)
							if err == nil {
								for k, v := range headers {
									req.Header.Set(k, v)
								}
								resp, err := http.DefaultClient.Do(req)
								if err == nil {
									resp.Body.Close()
								}
							}
						}
					}
				}
			}
		}
	}

	s.store.DeleteSubscription(userID, id)
	writeJSON(w, map[string]string{"status": "deleted"})
}

// unregisterUpstreamWebhook calls the app's delete_path upstream to remove
// an external webhook subscription. Best-effort — network errors and 4xx/5xx
// responses are logged but do not fail the caller, because the local DB row
// is the authoritative source of truth for us.
func (s *Server) unregisterUpstreamWebhook(conn *Connection, app *AppTemplate, externalID string) {
	if conn == nil || app == nil || app.Webhooks == nil || app.Webhooks.Registration == nil {
		return
	}
	reg := app.Webhooks.Registration
	if reg.DeletePath == "" || externalID == "" {
		return
	}
	_, encCreds, err := s.store.GetConnection(conn.UserID, conn.ID)
	if err != nil {
		return
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return
	}
	deletePath := strings.ReplaceAll(reg.DeletePath, "{id}", externalID)
	deleteURL := strings.TrimSuffix(app.BaseURL, "/") + deletePath
	headers := map[string]string{}
	for k, v := range app.Auth.Headers {
		headers[k] = resolveCredTemplate(v, plain)
	}
	method := reg.DeleteMethod
	if method == "" {
		method = "DELETE"
	}
	req, err := http.NewRequest(method, deleteURL, nil)
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SUB-UNREG] upstream delete error: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[SUB-UNREG] upstream delete %s → %d", deleteURL, resp.StatusCode)
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

	// Mirror the enable/disable on Composio's side so polling and
	// webhook delivery actually pause when the user disables the row.
	// Local-source subs don't have an upstream pause knob — their
	// registration just stays alive, and the ingress path drops
	// events for disabled rows.
	if sub, err := s.store.GetSubscription(userID, id); err == nil && sub != nil {
		extID := s.store.GetSubscriptionExternalID(id)
		if extID != "" && sub.ConnectionID > 0 {
			if conn, _, cerr := s.store.GetConnection(userID, sub.ConnectionID); cerr == nil && conn != nil && conn.Source == "composio" {
				if client, cerr := s.composioClientFor(userID, conn.ProviderID); cerr == nil {
					if perr := client.PatchTriggerInstance(extID, enable); perr != nil {
						log.Printf("[SUB-TOGGLE] composio patch %s enable=%v: %v", extID, enable, perr)
					}
				}
			}
		}
	}

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
	log.Printf("[SUB-TEST] start user=%d sub=%s", userID, id)

	sub, err := s.store.GetSubscription(userID, id)
	if err != nil {
		log.Printf("[SUB-TEST] subscription %s not found: %v", id, err)
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	log.Printf("[SUB-TEST] sub=%s name=%q slug=%q instance=%d thread=%q", sub.ID, sub.Name, sub.Slug, sub.InstanceID, sub.ThreadID)

	// Parse optional body: { "event": "content.created", "payload": { ... } }
	var reqBody struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	json.NewDecoder(r.Body).Decode(&reqBody) // ignore errors — all fields optional
	log.Printf("[SUB-TEST] request body event=%q custom_payload=%v", reqBody.Event, reqBody.Payload != nil)

	if sub.InstanceID == 0 {
		log.Printf("[SUB-TEST] sub=%s has no instance_id configured", sub.ID)
		http.Error(w, "no instance configured", http.StatusBadRequest)
		return
	}

	inst, err := s.store.GetInstance(sub.UserID, sub.InstanceID)
	if err != nil {
		log.Printf("[SUB-TEST] instance %d not found for user %d: %v", sub.InstanceID, sub.UserID, err)
		http.Error(w, "instance not found", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[SUB-TEST] instance %d → name=%q status=%q", inst.ID, inst.Name, inst.Status)
	testPort := s.instances.GetPort(inst.ID)
	if testPort == 0 {
		log.Printf("[SUB-TEST] instance %d has no local port — core not running or not tracked", inst.ID)
		http.Error(w, "instance not running", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[SUB-TEST] instance %d local port=%d", inst.ID, testPort)

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
	coreKey := s.instances.GetCoreAPIKey(inst.ID)
	log.Printf("[SUB-TEST] → POST %s thread=%q msg_len=%d has_auth=%v", targetURL, sub.ThreadID, len(eventMsg), coreKey != "")

	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(string(eventBody)))
	req.Header.Set("Content-Type", "application/json")
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SUB-TEST] HTTP error posting to core: %v", err)
		http.Error(w, "failed to deliver test event: "+err.Error(), http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("[SUB-TEST] ← core %d %s", resp.StatusCode, string(respBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("core rejected test event: %d %s", resp.StatusCode, string(respBody)), http.StatusBadGateway)
		return
	}

	log.Printf("[SUB-TEST] delivered sub=%s event=%q", sub.ID, eventType)
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

// extractJSONPath extracts a value at a dot-notation path from a map (e.g. "data.id")
func extractJSONPath(obj map[string]any, path string) string {
	parts := strings.Split(path, ".")
	var current any = obj
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[part]
	}
	if current == nil {
		return ""
	}
	return fmt.Sprintf("%v", current)
}

func (s *Server) webhookURL(path string) string {
	return s.publicBaseURL() + "/webhooks/" + path
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
