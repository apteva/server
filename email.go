package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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
		"SELECT id, user_id, agent_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE type = 'email' AND status = 'active' AND agent_id > 0",
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

		ic := s.agents.GetChannels(instanceID)
		if ic == nil {
			continue
		}
		port := s.agents.GetPort(instanceID)
		if port == 0 {
			continue
		}
		coreKey := s.agents.GetCoreAPIKey(instanceID)
		sendEvent := makeSendEvent(port, coreKey)
		gw.MapInbox(instanceID, cfg["inbox_id"], cfg["email"], ic.registry, sendEvent)
	}
}

// restoreEmailForInstance re-maps email channels for a single instance.
func (s *Server) restoreEmailForInstance(inst *Agent) {
	records, err := s.store.ListChannels(inst.ID)
	if err != nil {
		return
	}
	ic := s.agents.GetChannels(inst.ID)
	if ic == nil {
		return
	}
	port := s.agents.GetPort(inst.ID)
	if port == 0 {
		return
	}
	coreKey := s.agents.GetCoreAPIKey(inst.ID)
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
			log.Printf("[EMAIL] webhook registered: %s", webhookID)
			// Store the webhook secret for verification
			if encrypted, err := Encrypt(s.secret, secret); err == nil {
				s.store.SetSetting("email_webhook_secret_"+body.ProjectID, "encrypted:"+encrypted)
			}
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

	ts, err := strconv.ParseInt(r.Header.Get("webhook-timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		http.Error(w, "invalid webhook timestamp", http.StatusUnauthorized)
		return
	}
	type destination struct {
		project string
		gateway *EmailGateway
	}
	targets := []destination{}
	emailMu.RLock()
	for project, gw := range emailGateways {
		targets = append(targets, destination{project, gw})
	}
	emailMu.RUnlock()
	verified := false
	for _, target := range targets {
		secret := s.store.GetSetting("email_webhook_secret_" + target.project)
		if strings.HasPrefix(secret, "encrypted:") {
			secret, err = Decrypt(s.secret, strings.TrimPrefix(secret, "encrypted:"))
			if err != nil {
				continue
			}
		}
		if secret == "" || !verifyStandardWebhook(body, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), secret) {
			continue
		}
		verified = true
		result, err := s.store.db.Exec("INSERT OR IGNORE INTO email_webhook_receipts(project_id,message_id,received_at) VALUES(?,?,?)", target.project, r.Header.Get("webhook-id"), time.Now().Unix())
		if err != nil {
			http.Error(w, "webhook storage unavailable", http.StatusServiceUnavailable)
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			continue
		}
		target.gateway.HandleInbound(json.RawMessage(body))
		s.store.db.Exec("DELETE FROM email_webhook_receipts WHERE received_at<?", time.Now().Add(-24*time.Hour).Unix())
	}
	if !verified {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}

	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]string{"status": "ok"})
}
