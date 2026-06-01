package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func (s *Server) notifySubscriptionCreated(sub *Subscription) {
	s.notifySubscriptionChange("Subscription created", sub)
}

func (s *Server) notifySubscriptionEnabled(sub *Subscription) {
	s.notifySubscriptionChange("Subscription enabled", sub)
}

func (s *Server) notifySubscriptionDisabled(sub *Subscription) {
	s.notifySubscriptionChange("Subscription disabled", sub)
}

func (s *Server) notifySubscriptionDeleted(sub *Subscription) {
	s.notifySubscriptionChange("Subscription deleted", sub)
}

func (s *Server) notifyAgentSubscriptionStartup(inst *Agent) {
	if s == nil || inst == nil || inst.ID == 0 {
		return
	}
	go func() {
		summary, count, err := s.activeSubscriptionSummary(inst.UserID, inst.ID)
		if err != nil {
			log.Printf("[SUB-NOTIFY] startup summary agent=%d: %v", inst.ID, err)
			return
		}
		if count == 0 {
			return
		}
		msg := "[platform:subscriptions] Agent started. " + summary
		s.postAgentSubscriptionEvent(inst.ID, "", msg)
	}()
}

func (s *Server) notifySubscriptionChange(action string, sub *Subscription) {
	if s == nil || sub == nil || sub.UserID == 0 || sub.AgentID == 0 || !sub.NotifyAgent {
		return
	}
	subCopy := *sub
	go func() {
		changed := formatSubscriptionChangedLine(&subCopy)
		summary, _, err := s.activeSubscriptionSummary(subCopy.UserID, subCopy.AgentID)
		if err != nil {
			log.Printf("[SUB-NOTIFY] summary agent=%d sub=%s: %v", subCopy.AgentID, subCopy.ID, err)
			summary = "Current active subscriptions targeting this agent: unavailable."
		}
		msg := fmt.Sprintf("[platform:subscriptions] %s: %s\n\n%s", action, changed, summary)
		s.postAgentSubscriptionEvent(subCopy.AgentID, subCopy.ThreadID, msg)
	}()
}

func (s *Server) activeSubscriptionSummary(userID, agentID int64) (string, int, error) {
	subs, err := s.store.ListSubscriptionsForAgent(userID, agentID)
	if err != nil {
		return "", 0, err
	}
	var lines []string
	for _, sub := range subs {
		if !sub.Enabled || !sub.NotifyAgent {
			continue
		}
		lines = append(lines, "- "+formatSubscriptionSummaryLine(&sub))
	}
	if len(lines) == 0 {
		return "Current active subscriptions targeting this agent: none.", 0, nil
	}
	return "Current active subscriptions targeting this agent:\n" + strings.Join(lines, "\n"), len(lines), nil
}

func formatSubscriptionChangedLine(sub *Subscription) string {
	if sub == nil {
		return "unknown subscription"
	}
	return formatSubscriptionSummaryLine(sub)
}

func formatSubscriptionSummaryLine(sub *Subscription) string {
	if sub == nil {
		return "unknown subscription"
	}
	kind := subscriptionKind(sub)
	label := strings.TrimSpace(sub.Name)
	if label == "" {
		label = strings.TrimSpace(sub.Slug)
	}
	if label == "" {
		label = sub.ID
	}
	parts := []string{fmt.Sprintf("%s: %s", kind, label)}
	if sub.Slug != "" && sub.Slug != label {
		parts = append(parts, "slug: "+sub.Slug)
	}
	if len(sub.Events) > 0 {
		parts = append(parts, "events: "+strings.Join(sub.Events, ", "))
	}
	if sub.ThreadID != "" {
		parts = append(parts, "thread: "+sub.ThreadID)
	} else {
		parts = append(parts, "thread: main")
	}
	return strings.Join(parts, " | ")
}

func subscriptionKind(sub *Subscription) string {
	if sub == nil {
		return "subscription"
	}
	if sub.Source == "app_event" {
		return "app event"
	}
	if sub.ConnectionID > 0 && sub.WebhookPath == "" {
		return "trigger"
	}
	return "webhook"
}

func (s *Server) postAgentSubscriptionEvent(agentID int64, threadID, message string) {
	if s == nil || s.agents == nil || agentID == 0 || strings.TrimSpace(message) == "" {
		return
	}
	body := map[string]string{"message": message}
	if threadID != "" {
		body["thread_id"] = threadID
	}
	bodyJSON, _ := json.Marshal(body)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		port := s.agents.GetPort(agentID)
		if port == 0 {
			lastErr = fmt.Errorf("agent not running")
			time.Sleep(250 * time.Millisecond)
			continue
		}
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/event", port), strings.NewReader(string(bodyJSON)))
		req.Header.Set("Content-Type", "application/json")
		if key := s.agents.GetCoreAPIKey(agentID); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("[SUB-NOTIFY] delivered agent=%d thread=%q", agentID, threadID)
			return
		}
		lastErr = fmt.Errorf("core rejected http %d: %s", resp.StatusCode, string(respBody))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	log.Printf("[SUB-NOTIFY] skipped agent=%d thread=%q: %v", agentID, threadID, lastErr)
}
