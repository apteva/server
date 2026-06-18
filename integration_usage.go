package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type integrationUsageEvent struct {
	ProjectID          string
	CallerInstallID    int64
	CallerAppName      string
	ConnectionID       int64
	ParentConnectionID int64
	AppSlug            string
	Tool               string
	GrantID            string
	GrantResource      string
	ChildInstallID     int64
	ChildConnectionID  int64
	Direction          string
	Quantity           int
	Unit               string
	Status             string
	Error              string
	ProviderRequestID  string
	Metadata           map[string]any
}

func (s *Server) recordIntegrationUsage(ev integrationUsageEvent) {
	if s == nil || s.store == nil || s.store.db == nil {
		return
	}
	recordIntegrationUsageDB(s.store.db, ev)
}

func recordIntegrationUsageDB(db *sql.DB, ev integrationUsageEvent) {
	if db == nil {
		return
	}
	if ev.Quantity <= 0 {
		ev.Quantity = 1
	}
	if strings.TrimSpace(ev.Unit) == "" {
		ev.Unit = "request"
	}
	if strings.TrimSpace(ev.Direction) == "" {
		ev.Direction = "local"
	}
	if strings.TrimSpace(ev.GrantResource) == "" && strings.TrimSpace(ev.GrantID) != "" {
		ev.GrantResource = "provider.connection"
	}
	if ev.ParentConnectionID == 0 {
		ev.ParentConnectionID = ev.ConnectionID
	}
	meta := "{}"
	if len(ev.Metadata) > 0 {
		if raw, err := json.Marshal(ev.Metadata); err == nil {
			meta = truncate(string(raw), 4000)
		}
	}
	_, _ = db.Exec(`
		INSERT INTO integration_usage_events
			(project_id, caller_install_id, caller_app_name, connection_id, parent_connection_id,
			 app_slug, tool, grant_id, grant_resource, child_install_id, child_connection_id,
			 direction, quantity, unit, status, error, provider_request_id, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, strings.TrimSpace(ev.ProjectID), ev.CallerInstallID, strings.TrimSpace(ev.CallerAppName),
		ev.ConnectionID, ev.ParentConnectionID, strings.TrimSpace(ev.AppSlug), strings.TrimSpace(ev.Tool),
		strings.TrimSpace(ev.GrantID), strings.TrimSpace(ev.GrantResource), ev.ChildInstallID,
		ev.ChildConnectionID, strings.TrimSpace(ev.Direction), ev.Quantity, strings.TrimSpace(ev.Unit),
		strings.TrimSpace(ev.Status), truncate(ev.Error, 1000), strings.TrimSpace(ev.ProviderRequestID), meta)
}

func (s *Server) callerAppName(installID int64) string {
	if installID <= 0 || s == nil || s.store == nil || s.store.db == nil {
		return ""
	}
	var name string
	_ = s.store.db.QueryRow(`
		SELECT COALESCE(a.name, '')
		FROM app_installs i JOIN apps a ON a.id = i.app_id
		WHERE i.id = ?
	`, installID).Scan(&name)
	return name
}

func (s *Server) projectForConnection(connID int64) string {
	if connID <= 0 || s == nil || s.store == nil || s.store.db == nil {
		return ""
	}
	var projectID string
	_ = s.store.db.QueryRow(`SELECT COALESCE(project_id, '') FROM connections WHERE id = ?`, connID).Scan(&projectID)
	return projectID
}

func integrationUsageFromResult(conn *Connection, installID int64, callerApp, tool string, input map[string]any, result *ExecuteResult, err error) integrationUsageEvent {
	status := "success"
	errText := ""
	if err != nil {
		status = "error"
		errText = err.Error()
	} else if result != nil && (!result.Success || result.Status >= 400) {
		status = "error"
		errText = truncate(fmt.Sprintf("%v", result.Data), 500)
	}
	qty, unit, metadata := integrationUsageMetric(conn, tool, input, result)
	ev := integrationUsageEvent{
		CallerInstallID: installID,
		CallerAppName:   callerApp,
		Tool:            tool,
		Quantity:        qty,
		Unit:            unit,
		Status:          status,
		Error:           errText,
		Direction:       "local",
		Metadata:        metadata,
	}
	if conn != nil {
		ev.ProjectID = conn.ProjectID
		ev.ConnectionID = conn.ID
		ev.ParentConnectionID = conn.ID
		ev.AppSlug = conn.AppSlug
	}
	return ev
}

func integrationUsageMetric(conn *Connection, tool string, input map[string]any, result *ExecuteResult) (int, string, map[string]any) {
	appSlug := ""
	if conn != nil {
		appSlug = conn.AppSlug
	}
	if strings.EqualFold(appSlug, "aws-ses") {
		if strings.EqualFold(tool, "send_email") || strings.EqualFold(tool, "send_bulk_email") {
			return usageQuantity(tool, input), "recipient", nil
		}
		return 1, "request", nil
	}
	if strings.EqualFold(appSlug, "deepgram") {
		if seconds := deepgramDurationSeconds(result); seconds > 0 {
			return seconds, "second", map[string]any{"duration_seconds": seconds}
		}
	}
	return 1, "request", nil
}

func usageUnit(appSlug, tool string) string {
	if strings.EqualFold(appSlug, "aws-ses") && (strings.EqualFold(tool, "send_email") || strings.EqualFold(tool, "send_bulk_email")) {
		return "recipient"
	}
	return "request"
}

func deepgramDurationSeconds(result *ExecuteResult) int {
	if result == nil || result.Data == nil {
		return 0
	}
	var data any = result.Data
	switch v := result.Data.(type) {
	case json.RawMessage:
		_ = json.Unmarshal(v, &data)
	case []byte:
		_ = json.Unmarshal(v, &data)
	}
	m, ok := data.(map[string]any)
	if !ok {
		return 0
	}
	for _, path := range [][]string{
		{"metadata", "duration"},
		{"metadata", "duration_seconds"},
		{"duration"},
		{"duration_seconds"},
	} {
		if v, ok := nestedMapValue(m, path...); ok {
			if seconds := numericSeconds(v); seconds > 0 {
				return seconds
			}
		}
	}
	return 0
}

func nestedMapValue(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, key := range path {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = next[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func numericSeconds(v any) int {
	switch x := v.(type) {
	case float64:
		return int(math.Ceil(x))
	case float32:
		return int(math.Ceil(float64(x)))
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		f, _ := x.Float64()
		return int(math.Ceil(f))
	default:
		return 0
	}
}
