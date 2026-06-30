package main

import (
	"fmt"
	"strings"
)

const environmentSubscriptionSourceAppEvent = "app_event"

type EnvironmentSubscriptionSpec struct {
	ID               string `json:"id,omitempty"`
	Source           string `json:"source"`
	App              string `json:"app,omitempty"`
	Topic            string `json:"topic,omitempty"`
	TargetAgentAlias string `json:"target_agent_alias,omitempty"`
	ThreadID         string `json:"thread_id,omitempty"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	Enabled          bool   `json:"enabled"`
}

type environmentSubscriptionInfo struct {
	EnvironmentSubscriptionSpec
	SubscriptionID string `json:"subscription_id,omitempty"`
	TargetAgentID  int64  `json:"target_agent_id,omitempty"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
}

func normalizeEnvironmentSubscriptionSpec(spec EnvironmentSubscriptionSpec) (EnvironmentSubscriptionSpec, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	spec.Source = strings.TrimSpace(spec.Source)
	if spec.Source == "" {
		spec.Source = environmentSubscriptionSourceAppEvent
	}
	if spec.Source != environmentSubscriptionSourceAppEvent {
		return spec, fmt.Errorf("environment subscription source %q not supported yet", spec.Source)
	}
	spec.App = strings.TrimSpace(spec.App)
	spec.Topic = strings.TrimSpace(spec.Topic)
	spec.TargetAgentAlias = strings.TrimSpace(spec.TargetAgentAlias)
	if spec.TargetAgentAlias == "" {
		spec.TargetAgentAlias = "main"
	}
	spec.ThreadID = strings.TrimSpace(spec.ThreadID)
	if spec.ThreadID == "" {
		spec.ThreadID = "main"
	}
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Description = strings.TrimSpace(spec.Description)
	if spec.App == "" || spec.Topic == "" {
		return spec, fmt.Errorf("app and topic required")
	}
	if strings.Contains(spec.App, ":") {
		return spec, fmt.Errorf("app must not contain ':'")
	}
	if spec.ID == "" {
		spec.ID = spec.App + ":" + spec.Topic + "->" + spec.TargetAgentAlias + ":" + spec.ThreadID
	}
	// Environment-owned subscriptions are active by default. The current JSON
	// shape does not distinguish omitted from false, so disabled declarations
	// can be added later with a separate field if we need author-time disabling.
	spec.Enabled = true
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("%s %s → %s/%s", spec.App, spec.Topic, spec.TargetAgentAlias, spec.ThreadID)
	}
	if spec.Description == "" {
		spec.Description = "Environment app-event subscription"
	}
	return spec, nil
}

func normalizeEnvironmentSubscriptionSpecs(specs []EnvironmentSubscriptionSpec) ([]EnvironmentSubscriptionSpec, error) {
	out := []EnvironmentSubscriptionSpec{}
	seen := map[string]bool{}
	for i, spec := range specs {
		normalized, err := normalizeEnvironmentSubscriptionSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("subscription %d: %w", i, err)
		}
		if seen[normalized.ID] {
			return nil, fmt.Errorf("subscription %d: duplicate id %q", i, normalized.ID)
		}
		seen[normalized.ID] = true
		out = append(out, normalized)
	}
	return out, nil
}

func upsertEnvironmentSubscriptionSpec(specs []EnvironmentSubscriptionSpec, spec EnvironmentSubscriptionSpec) []EnvironmentSubscriptionSpec {
	for i := range specs {
		if specs[i].ID == spec.ID {
			next := append([]EnvironmentSubscriptionSpec(nil), specs...)
			next[i] = spec
			return next
		}
	}
	next := append([]EnvironmentSubscriptionSpec(nil), specs...)
	return append(next, spec)
}

func (s *Server) installEnvironmentSubscriptionsForAgent(userID int64, environment *Environment, wa *EnvironmentAgent) error {
	if environment == nil || wa == nil {
		return nil
	}
	for _, spec := range environment.SubscriptionSpecs() {
		if spec.TargetAgentAlias != "" && spec.TargetAgentAlias != wa.Alias {
			continue
		}
		if err := s.installEnvironmentSubscription(userID, environment, wa, spec); err != nil {
			return err
		}
	}
	if s.appEventDispatcher != nil {
		return s.appEventDispatcher.Reconcile()
	}
	return nil
}

func (s *Server) installEnvironmentSubscription(userID int64, environment *Environment, wa *EnvironmentAgent, spec EnvironmentSubscriptionSpec) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("server store not configured")
	}
	if environment == nil || wa == nil {
		return fmt.Errorf("environment agent not running")
	}
	spec, err := normalizeEnvironmentSubscriptionSpec(spec)
	if err != nil {
		return err
	}
	if !spec.Enabled {
		return nil
	}
	if spec.Source != environmentSubscriptionSourceAppEvent {
		return fmt.Errorf("environment subscription source %q not supported yet", spec.Source)
	}
	slug := spec.App + ":" + spec.Topic
	var existing string
	_ = s.store.db.QueryRow(
		`SELECT id FROM subscriptions
		  WHERE source = 'app_event' AND project_id = ? AND agent_id = ? AND slug = ? AND COALESCE(thread_id, '') = ?
		  LIMIT 1`,
		environment.ID, wa.AgentID, slug, spec.ThreadID,
	).Scan(&existing)
	if existing != "" {
		return nil
	}
	_, err = s.store.CreateAppEventSubscription(userID, wa.AgentID, spec.Name, slug, spec.Description, spec.ThreadID, environment.ID, []string{spec.Topic})
	return err
}

func (s *Server) deleteEnvironmentSubscriptionRows(environmentID string) error {
	if s == nil || s.store == nil || strings.TrimSpace(environmentID) == "" {
		return nil
	}
	_, err := s.store.db.Exec(`DELETE FROM subscriptions WHERE project_id = ? AND source = 'app_event'`, environmentID)
	if err == nil && s.appEventDispatcher != nil {
		_ = s.appEventDispatcher.Reconcile()
	}
	return err
}

func (s *Server) deleteEnvironmentSubscriptionRow(environmentID, subscriptionID string) error {
	if s == nil || s.store == nil || environmentID == "" || subscriptionID == "" {
		return nil
	}
	_, err := s.store.db.Exec(`DELETE FROM subscriptions WHERE id = ? AND project_id = ? AND source = 'app_event'`, subscriptionID, environmentID)
	if err == nil && s.appEventDispatcher != nil {
		_ = s.appEventDispatcher.Reconcile()
	}
	return err
}

func (s *Server) deleteEnvironmentSubscriptionRowsForSpec(environmentID string, spec EnvironmentSubscriptionSpec) error {
	if s == nil || s.store == nil || environmentID == "" {
		return nil
	}
	spec, err := normalizeEnvironmentSubscriptionSpec(spec)
	if err != nil {
		return err
	}
	slug := spec.App + ":" + spec.Topic
	_, err = s.store.db.Exec(
		`DELETE FROM subscriptions
		  WHERE project_id = ? AND source = 'app_event' AND slug = ? AND COALESCE(thread_id, '') = ?`,
		environmentID, slug, spec.ThreadID,
	)
	if err == nil && s.appEventDispatcher != nil {
		_ = s.appEventDispatcher.Reconcile()
	}
	return err
}

func (s *Server) environmentSubscriptionInfos(environment *Environment, rec *EnvironmentRecord) []environmentSubscriptionInfo {
	specs := []EnvironmentSubscriptionSpec{}
	if environment != nil {
		specs = environment.SubscriptionSpecs()
	} else if rec != nil {
		specs = decodePersistedEnvironmentSpec(rec).Subscriptions
	}
	rows := s.environmentSubscriptionRows(firstNonEmpty(environmentID(environment), recordID(rec)))
	rowByKey := map[string]Subscription{}
	for _, row := range rows {
		app, topic, ok := splitAppEventSlug(row.Slug)
		if !ok {
			continue
		}
		key := environmentSubscriptionKey(app, topic, row.AgentID, row.ThreadID)
		rowByKey[key] = row
	}
	out := []environmentSubscriptionInfo{}
	for _, spec := range specs {
		info := environmentSubscriptionInfo{EnvironmentSubscriptionSpec: spec, Status: "waiting_for_agent"}
		if !spec.Enabled {
			info.Status = "disabled"
		}
		if environment != nil {
			wa := environment.AgentByAlias(spec.TargetAgentAlias)
			if wa != nil {
				info.TargetAgentID = wa.AgentID
				key := environmentSubscriptionKey(spec.App, spec.Topic, wa.AgentID, spec.ThreadID)
				if row, ok := rowByKey[key]; ok {
					info.SubscriptionID = row.ID
					info.Status = "active"
				} else if spec.Enabled {
					info.Status = "pending"
				}
			}
		}
		out = append(out, info)
	}
	return out
}

func (s *Server) environmentSubscriptionRows(environmentID string) []Subscription {
	if s == nil || s.store == nil || environmentID == "" {
		return nil
	}
	rows, err := s.store.db.Query(
		`SELECT id, user_id, agent_id, connection_id, name, slug, description, webhook_path,
		        enabled, COALESCE(notify_agent,0), COALESCE(thread_id,''), COALESCE(events,''),
		        COALESCE(project_id,''), COALESCE(source,'webhook'), COALESCE(last_seq_delivered,0), created_at
		   FROM subscriptions
		  WHERE project_id = ? AND source = 'app_event'
		  ORDER BY created_at, id`,
		environmentID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		var sub Subscription
		var enabled, notifyAgent int
		var eventsJSON, createdAt string
		if err := rows.Scan(
			&sub.ID, &sub.UserID, &sub.AgentID, &sub.ConnectionID, &sub.Name, &sub.Slug, &sub.Description,
			&sub.WebhookPath, &enabled, &notifyAgent, &sub.ThreadID, &eventsJSON, &sub.ProjectID,
			&sub.Source, &sub.LastSeqDelivered, &createdAt,
		); err != nil {
			continue
		}
		sub.Enabled = enabled == 1
		sub.NotifyAgent = notifyAgent == 1
		sub.CreatedAt, _ = parseTime(createdAt)
		out = append(out, sub)
	}
	return out
}

func environmentSubscriptionKey(app, topic string, agentID int64, threadID string) string {
	return fmt.Sprintf("%s:%s:%d:%s", app, topic, agentID, threadID)
}

func environmentID(environment *Environment) string {
	if environment == nil {
		return ""
	}
	return environment.ID
}

func recordID(rec *EnvironmentRecord) string {
	if rec == nil {
		return ""
	}
	return rec.ID
}
