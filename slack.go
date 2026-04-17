package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

// --- Slack gateway lifecycle (per-project) ---

// slackGateways maps project_id → running SlackGateway.
// Protected by slackMu. Lazily populated on boot and on configure.
var (
	slackMu       sync.RWMutex
	slackGateways = map[string]*SlackGateway{}
)

// getSlackGateway returns the running gateway for a project, or nil.
func getSlackGateway(projectID string) *SlackGateway {
	slackMu.RLock()
	defer slackMu.RUnlock()
	return slackGateways[projectID]
}

// initSlack starts Slack gateways for all projects that have a
// slack_app channel config. Called once on server boot.
func (s *Server) initSlack() {
	rows, err := s.store.db.Query(
		"SELECT id, user_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE type = 'slack_app' AND status = 'active'",
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
			log.Printf("[SLACK] failed to decrypt config for project %q: %v", projectID, err)
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg == nil || cfg["bot_token"] == "" || cfg["app_token"] == "" {
			continue
		}

		gw := NewSlackGateway(cfg["bot_token"], cfg["app_token"])
		if err := gw.Start(); err != nil {
			log.Printf("[SLACK] gateway start failed for project %q: %v", projectID, err)
			continue
		}

		slackMu.Lock()
		slackGateways[projectID] = gw
		slackMu.Unlock()
		log.Printf("[SLACK] gateway started for project %q", projectID)
	}

	// Restore per-instance channel mappings
	s.restoreAllSlackMappings()
}

// restoreAllSlackMappings wires up persisted slack channel mappings
// for all running instances across all projects.
func (s *Server) restoreAllSlackMappings() {
	rows, err := s.store.db.Query(
		"SELECT id, user_id, instance_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE type = 'slack' AND status = 'active' AND instance_id > 0",
	)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, userID, instanceID int64
		var projectID, enc string
		rows.Scan(&id, &userID, &instanceID, &projectID, &enc)

		gw := getSlackGateway(projectID)
		if gw == nil {
			continue
		}

		plain, err := Decrypt(s.secret, enc)
		if err != nil {
			continue
		}
		var cfg map[string]string
		json.Unmarshal([]byte(plain), &cfg)
		if cfg == nil || cfg["slack_channel_id"] == "" {
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

		// Look up instance name for display
		instName := ""
		if inst, err := s.store.GetInstanceByID(instanceID); err == nil {
			instName = inst.Name
		}

		coreKey := s.instances.GetCoreAPIKey(instanceID)
		sendEvent := makeSendEvent(port, coreKey)
		gw.MapChannel(instanceID, cfg["slack_channel_id"], cfg["channel_name"], instName, ic.registry, sendEvent)
	}
}

// restoreSlackForInstance re-maps slack channels for a single instance
// that just started.
func (s *Server) restoreSlackForInstance(inst *Instance) {
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
		if r.Type != "slack" {
			continue
		}
		gw := getSlackGateway(inst.ProjectID)
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
		if cfg == nil || cfg["slack_channel_id"] == "" {
			continue
		}
		gw.MapChannel(inst.ID, cfg["slack_channel_id"], cfg["channel_name"], inst.Name, ic.registry, sendEvent)
	}
}

func makeSendEvent(port int, coreKey string) func(string, string) {
	return func(text, threadID string) {
		body, _ := json.Marshal(map[string]any{"message": text, "thread_id": threadID})
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", port), strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if coreKey != "" {
			req.Header.Set("Authorization", "Bearer "+coreKey)
		}
		http.DefaultClient.Do(req)
	}
}

// --- HTTP Handlers ---

// POST /api/slack/configure — set Slack app tokens for a project.
// Body: {"project_id": "proj_abc", "bot_token": "xoxb-...", "app_token": "xapp-..."}
func (s *Server) handleSlackConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	var body struct {
		ProjectID string `json:"project_id"`
		BotToken  string `json:"bot_token"`
		AppToken  string `json:"app_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.BotToken == "" || body.AppToken == "" {
		http.Error(w, "bot_token and app_token required", http.StatusBadRequest)
		return
	}
	if body.ProjectID == "" {
		http.Error(w, "project_id required", http.StatusBadRequest)
		return
	}

	// Verify the caller actually owns this project. Without this check,
	// an authenticated user could push tokens for any project_id and
	// either DoS the real owner or (worse) substitute their own Slack
	// app to silently intercept the victim's inbound messages.
	// GetProject already filters by user_id and returns an error if the
	// row doesn't exist or isn't owned — surface that as 404 without
	// revealing which case it was.
	if _, err := s.store.GetProject(userID, body.ProjectID); err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	// Validate by starting a gateway
	gw := NewSlackGateway(body.BotToken, body.AppToken)
	if err := gw.Start(); err != nil {
		http.Error(w, "slack authentication failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Stop old gateway for this project if running
	slackMu.Lock()
	if old, ok := slackGateways[body.ProjectID]; ok {
		old.Stop()
	}
	slackGateways[body.ProjectID] = gw
	slackMu.Unlock()

	// Remove existing slack_app config for this project
	existing, _ := s.store.ListChannelsByProject(body.ProjectID, "slack_app")
	for _, ch := range existing {
		s.store.DeleteChannel(ch.ID)
	}

	// Persist as a project-level channel (instance_id=0)
	configJSON, _ := json.Marshal(map[string]string{
		"bot_token": body.BotToken,
		"app_token": body.AppToken,
	})
	encrypted, _ := Encrypt(s.secret, string(configJSON))
	s.store.CreateChannel(userID, 0, "slack_app", "slack", encrypted, body.ProjectID)

	// Restore any existing per-instance mappings
	s.restoreAllSlackMappings()

	log.Printf("[SLACK] configured for project %q", body.ProjectID)
	writeJSON(w, map[string]string{"status": "connected", "project_id": body.ProjectID})
}

// GET /api/slack/status?project_id=X — check if Slack is configured for a project.
func (s *Server) handleSlackStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	gw := getSlackGateway(projectID)
	writeJSON(w, map[string]bool{"connected": gw != nil})
}

// GET /api/slack/channels?project_id=X — list Slack channels the bot can see.
func (s *Server) handleSlackListChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	gw := getSlackGateway(projectID)
	if gw == nil {
		http.Error(w, "slack not configured for this project", http.StatusBadRequest)
		return
	}
	channels, err := gw.ListSlackChannels()
	if err != nil {
		http.Error(w, "failed to list channels: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, channels)
}

// --- Unified channel management ---

// GET /api/channels?instance_id=X or ?project_id=X
func (s *Server) handleChannelList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	q := r.URL.Query()

	type channelInfo struct {
		ID         int64  `json:"id"`
		InstanceID int64  `json:"instance_id"`
		Type       string `json:"type"`
		Name       string `json:"name"`
		Status     string `json:"status"`
	}

	// By instance
	if idStr := q.Get("instance_id"); idStr != "" {
		instanceID, _ := atoi64(idStr)
		inst, err := s.store.GetInstance(userID, instanceID)
		if err != nil {
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}
		records, _ := s.store.ListChannels(inst.ID)
		ic := s.instances.GetChannels(inst.ID)

		var out []channelInfo
		// CLI is always present
		cliStatus := "disconnected"
		if ic != nil && ic.cli != nil && ic.cli.IsConnected() {
			cliStatus = "connected"
		}
		out = append(out, channelInfo{Type: "cli", Name: "CLI / Dashboard", Status: cliStatus})

		for _, r := range records {
			status := "connected"
			if r.Type == "slack" && getSlackGateway(inst.ProjectID) == nil {
				status = "disconnected"
			} else if r.Type == "email" && getEmailGateway(inst.ProjectID) == nil {
				status = "disconnected"
			}
			out = append(out, channelInfo{ID: r.ID, InstanceID: r.InstanceID, Type: r.Type, Name: r.Name, Status: status})
		}
		writeJSON(w, out)
		return
	}

	// By project — return all non-cli channels across all instances
	projectID := q.Get("project_id")
	instances, err := s.store.ListInstances(userID, projectID)
	if err != nil {
		writeJSON(w, []channelInfo{})
		return
	}
	var out []channelInfo
	for _, inst := range instances {
		records, _ := s.store.ListChannels(inst.ID)
		for _, r := range records {
			status := "connected"
			if r.Type == "slack" && getSlackGateway(inst.ProjectID) == nil {
				status = "disconnected"
			} else if r.Type == "email" && getEmailGateway(inst.ProjectID) == nil {
				status = "disconnected"
			}
			out = append(out, channelInfo{ID: r.ID, InstanceID: r.InstanceID, Type: r.Type, Name: r.Name, Status: status})
		}
	}
	writeJSON(w, out)
}

// POST /api/channels/connect
// POST /api/channels/connect
// Body: {"instance_id": 5, "type": "telegram", "token": "..."}
//   or: {"instance_id": 5, "type": "slack", "channel_id": "C123", "channel_name": "ops"}
//   or: {"instance_id": 5, "type": "email"}
func (s *Server) handleChannelConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	var body struct {
		InstanceID  int64  `json:"instance_id"`
		Type        string `json:"type"`
		Token       string `json:"token"`        // telegram
		ChannelID   string `json:"channel_id"`    // slack
		ChannelName string `json:"channel_name"`  // slack
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetInstance(userID, body.InstanceID)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	switch body.Type {
	case "telegram":
		if body.Token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		botName, err := s.instances.StartTelegram(inst.ID, body.Token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Remove existing telegram channel for this instance
		if existing, _ := s.store.ListChannels(inst.ID); existing != nil {
			for _, ch := range existing {
				if ch.Type == "telegram" {
					s.store.DeleteChannel(ch.ID)
				}
			}
		}
		configJSON, _ := json.Marshal(map[string]string{"bot_token": body.Token, "bot_name": botName})
		encrypted, _ := Encrypt(s.secret, string(configJSON))
		s.store.CreateChannel(userID, inst.ID, "telegram", "@"+botName, encrypted, inst.ProjectID)
		// Notify core
		port := s.instances.GetPort(inst.ID)
		coreKey := s.instances.GetCoreAPIKey(inst.ID)
		if port > 0 {
			ev := fmt.Sprintf("[telegram] gateway connected. Bot @%s online.", botName)
			makeSendEvent(port, coreKey)(ev, "main")
		}
		writeJSON(w, map[string]string{"status": "connected", "type": "telegram", "bot_name": botName})

	case "slack":
		if body.ChannelID == "" {
			http.Error(w, "channel_id required", http.StatusBadRequest)
			return
		}
		gw := getSlackGateway(inst.ProjectID)
		if gw == nil {
			http.Error(w, "slack not configured for this project", http.StatusBadRequest)
			return
		}
		if body.ChannelName == "" {
			body.ChannelName = body.ChannelID
		}
		// Auto-join
		if err := gw.JoinChannel(body.ChannelID); err != nil {
			log.Printf("[SLACK] auto-join #%s failed: %v", body.ChannelName, err)
		}
		// Remove existing slack mapping for this instance
		if existing, _ := s.store.ListChannels(inst.ID); existing != nil {
			for _, ch := range existing {
				if ch.Type == "slack" {
					s.store.DeleteChannel(ch.ID)
				}
			}
		}
		gw.UnmapInstance(inst.ID)
		configJSON, _ := json.Marshal(map[string]string{
			"slack_channel_id": body.ChannelID,
			"channel_name":     body.ChannelName,
		})
		encrypted, _ := Encrypt(s.secret, string(configJSON))
		s.store.CreateChannel(userID, inst.ID, "slack", "#"+body.ChannelName, encrypted, inst.ProjectID)
		// Wire up live mapping
		ic := s.instances.GetChannels(inst.ID)
		port := s.instances.GetPort(inst.ID)
		if ic != nil && port > 0 {
			coreKey := s.instances.GetCoreAPIKey(inst.ID)
			sendEvent := makeSendEvent(port, coreKey)
			gw.MapChannel(inst.ID, body.ChannelID, body.ChannelName, inst.Name, ic.registry, sendEvent)
			sendEvent(fmt.Sprintf("[slack] channel #%s connected.", body.ChannelName), "main")
		}
		writeJSON(w, map[string]string{"status": "connected", "type": "slack", "channel": "#" + body.ChannelName})

	case "email":
		gw := getEmailGateway(inst.ProjectID)
		if gw == nil {
			http.Error(w, "email not configured for this project — call POST /api/email/configure first", http.StatusBadRequest)
			return
		}
		// Remove existing email channel for this instance
		if existing, _ := s.store.ListChannels(inst.ID); existing != nil {
			for _, ch := range existing {
				if ch.Type == "email" {
					s.store.DeleteChannel(ch.ID)
				}
			}
		}
		gw.UnmapInstance(inst.ID)
		// Create inbox
		clientID := fmt.Sprintf("apteva-%d", inst.ID)
		displayName := inst.Name
		inboxID, email, err := gw.CreateInbox(displayName, clientID)
		if err != nil {
			http.Error(w, "failed to create inbox: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Persist
		configJSON, _ := json.Marshal(map[string]string{
			"inbox_id": inboxID,
			"email":    email,
		})
		encrypted, _ := Encrypt(s.secret, string(configJSON))
		s.store.CreateChannel(userID, inst.ID, "email", email, encrypted, inst.ProjectID)
		// Wire up live
		ic := s.instances.GetChannels(inst.ID)
		port := s.instances.GetPort(inst.ID)
		if ic != nil && port > 0 {
			coreKey := s.instances.GetCoreAPIKey(inst.ID)
			sendEvent := makeSendEvent(port, coreKey)
			gw.MapInbox(inst.ID, inboxID, email, ic.registry, sendEvent)
			sendEvent(fmt.Sprintf("[email] inbox %s created.", email), "main")
		}
		writeJSON(w, map[string]string{"status": "connected", "type": "email", "email": email})

	default:
		http.Error(w, fmt.Sprintf("unsupported channel type: %s", body.Type), http.StatusBadRequest)
	}
}

// DELETE /api/channels/disconnect/:id
func (s *Server) handleChannelDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/channels/disconnect/")
	channelID, err := atoi64(idStr)
	if err != nil {
		http.Error(w, "invalid channel ID", http.StatusBadRequest)
		return
	}

	// Scope the lookup to the authenticated user. Without the user_id
	// filter any authenticated caller could delete another tenant's
	// channel by guessing ids (IDOR). A missing row — whether because
	// the id doesn't exist OR because it belongs to someone else — is
	// reported as 404 so we don't reveal which one is the case.
	userID := getUserID(r)
	var chType, enc string
	var instanceID int64
	var projectID string
	err = s.store.db.QueryRow(
		"SELECT type, instance_id, COALESCE(project_id,''), encrypted_config FROM channels WHERE id = ? AND user_id = ?",
		channelID, userID,
	).Scan(&chType, &instanceID, &projectID, &enc)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	// Clean up live state
	switch chType {
	case "slack":
		if gw := getSlackGateway(projectID); gw != nil {
			plain, _ := Decrypt(s.secret, enc)
			var cfg map[string]string
			json.Unmarshal([]byte(plain), &cfg)
			if cfg != nil && cfg["slack_channel_id"] != "" {
				gw.UnmapChannel(cfg["slack_channel_id"])
			}
		}
	case "telegram":
		if ic := s.instances.GetChannels(instanceID); ic != nil && ic.telegram != nil {
			ic.telegram.Stop()
			ic.telegram = nil
		}
	case "email":
		if gw := getEmailGateway(projectID); gw != nil {
			gw.UnmapInstance(instanceID)
		}
	}

	s.store.DeleteChannel(channelID)
	writeJSON(w, map[string]string{"status": "disconnected"})
}
