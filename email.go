package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
)

// --- Email gateway lifecycle (per-project) ---

var (
	emailMu       sync.RWMutex
	emailGateways = map[string]*EmailGateway{}
)

func getEmailGateway(projectID string) *EmailGateway {
	emailMu.RLock()
	defer emailMu.RUnlock()
	return emailGateways[projectID]
}

// initEmail starts email gateways for all projects that have an
// email_app channel config. Called once on server boot.
func (s *Server) initEmail() {
	rows, err := s.store.db.Query(
		"SELECT id, user_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE type = 'email_app' AND status = 'active'",
	)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, userID int64
		var projectID, enc string
		rows.Scan(&id, &userID, &projectID, &enc)

		plain, err := Decrypt(s.secret, enc)
		if err != nil {
			log.Printf("[EMAIL] failed to decrypt config for project %q: %v", projectID, err)
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg == nil || cfg["api_key"] == "" {
			continue
		}

		gw := NewEmailGateway(cfg["api_key"])
		if err := gw.Validate(); err != nil {
			log.Printf("[EMAIL] gateway validate failed for project %q: %v", projectID, err)
			continue
		}

		emailMu.Lock()
		emailGateways[projectID] = gw
		emailMu.Unlock()
		log.Printf("[EMAIL] gateway started for project %q", projectID)
	}

	s.restoreAllEmailMappings()
}

// restoreAllEmailMappings wires up persisted email channel mappings.
func (s *Server) restoreAllEmailMappings() {
	rows, err := s.store.db.Query(
		"SELECT id, user_id, instance_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE type = 'email' AND status = 'active' AND instance_id > 0",
	)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, userID, instanceID int64
		var projectID, enc string
		rows.Scan(&id, &userID, &instanceID, &projectID, &enc)

		gw := getEmailGateway(projectID)
		if gw == nil {
			continue
		}

		plain, err := Decrypt(s.secret, enc)
		if err != nil {
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg == nil || cfg["inbox_id"] == "" {
			continue
		}

		ic := s.instances.GetChannels(instanceID)
		if ic == nil {
			continue
		}
		port := s.instances.GetPort(instanceID)
		if port == 0 {
			continue
		}
		coreKey := s.instances.GetCoreAPIKey(instanceID)
		sendEvent := makeSendEvent(port, coreKey)
		gw.MapInbox(instanceID, cfg["inbox_id"], cfg["email"], ic.registry, sendEvent)
	}
}

// restoreEmailForInstance re-maps email channels for a single instance.
func (s *Server) restoreEmailForInstance(inst *Instance) {
	records, err := s.store.ListChannels(inst.ID)
	if err != nil {
		return
	}
	ic := s.instances.GetChannels(inst.ID)
	if ic == nil {
		return
	}
	port := s.instances.GetPort(inst.ID)
	if port == 0 {
		return
	}
	coreKey := s.instances.GetCoreAPIKey(inst.ID)
	sendEvent := makeSendEvent(port, coreKey)

	for _, r := range records {
		if r.Type != "email" {
			continue
		}
		gw := getEmailGateway(inst.ProjectID)
		if gw == nil {
			continue
		}
		enc, err := s.store.GetChannelConfig(r.ID)
		if err != nil || enc == "" {
			continue
		}
		plain, err := Decrypt(s.secret, enc)
		if err != nil {
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg == nil || cfg["inbox_id"] == "" {
			continue
		}
		gw.MapInbox(inst.ID, cfg["inbox_id"], cfg["email"], ic.registry, sendEvent)
	}
}

// --- HTTP Handlers ---

// POST /api/email/configure — set AgentMail API key for a project.
func (s *Server) handleEmailConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	var body struct {
		ProjectID string `json:"project_id"`
		APIKey    string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.APIKey == "" {
		http.Error(w, "api_key required", http.StatusBadRequest)
		return
	}

	gw := NewEmailGateway(body.APIKey)
	if err := gw.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Stop old gateway
	emailMu.Lock()
	emailGateways[body.ProjectID] = gw
	emailMu.Unlock()

	// Remove existing email_app config
	existing, _ := s.store.ListChannelsByProject(body.ProjectID, "email_app")
	for _, ch := range existing {
		s.store.DeleteChannel(ch.ID)
	}

	// Persist
	configJSON, _ := json.Marshal(map[string]string{"api_key": body.APIKey})
	encrypted, _ := Encrypt(s.secret, string(configJSON))
	s.store.CreateChannel(userID, 0, "email_app", "email", encrypted, body.ProjectID)

	// Register webhook for inbound emails
	publicURL := s.publicBaseURL()
	if publicURL != "" {
		webhookURL := publicURL + "/webhooks/email"
		clientID := "apteva-email-" + body.ProjectID
		webhookID, secret, err := gw.RegisterWebhook(webhookURL, clientID, nil)
		if err != nil {
			log.Printf("[EMAIL] webhook registration failed: %v", err)
		} else {
			log.Printf("[EMAIL] webhook registered: %s (secret: %s...)", webhookID, secret[:8])
			// Store the webhook secret for verification
			s.store.SetSetting("email_webhook_secret_"+body.ProjectID, secret)
		}
	}

	s.restoreAllEmailMappings()

	log.Printf("[EMAIL] configured for project %q", body.ProjectID)
	writeJSON(w, map[string]string{"status": "connected", "project_id": body.ProjectID})
}

// GET /api/email/status?project_id=X
func (s *Server) handleEmailStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	gw := getEmailGateway(projectID)
	writeJSON(w, map[string]bool{"connected": gw != nil})
}

// POST /webhooks/email — AgentMail inbound webhook (unauthenticated).
func (s *Server) handleEmailWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	log.Printf("[EMAIL-WEBHOOK] received %d bytes", len(body))

	// Parse to find inbox_id → project
	var event struct {
		EventType string `json:"event_type"`
		Message   struct {
			InboxID string `json:"inbox_id"`
			From    string `json:"from"`
		} `json:"message"`
	}
	json.Unmarshal(body, &event)

	// Try all email gateways — the one with the matching inbox_id will handle it
	emailMu.RLock()
	for _, gw := range emailGateways {
		gw.HandleInbound(json.RawMessage(body))
	}
	emailMu.RUnlock()

	// Update last sender on the channel for reply context
	if event.Message.InboxID != "" && event.Message.From != "" {
		emailMu.RLock()
		for _, gw := range emailGateways {
			gw.mu.RLock()
			if m, ok := gw.mappings[event.Message.InboxID]; ok {
				ch := m.registry.Get("email:" + m.email)
				if ec, ok := ch.(*EmailChannel); ok {
					ec.SetLastSender(event.Message.From)
				}
			}
			gw.mu.RUnlock()
		}
		emailMu.RUnlock()
	}

	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]string{"status": "ok"})
}

