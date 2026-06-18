package main

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

type integrationUsageSummary struct {
	Since  time.Time               `json:"since"`
	Rows   []integrationUsageRow   `json:"rows"`
	Totals []integrationUsageTotal `json:"totals"`
}

type integrationUsageRow struct {
	AppSlug            string     `json:"app_slug"`
	Tool               string     `json:"tool"`
	Unit               string     `json:"unit"`
	Direction          string     `json:"direction"`
	GrantID            string     `json:"grant_id,omitempty"`
	GrantResource      string     `json:"grant_resource,omitempty"`
	ConnectionID       int64      `json:"connection_id,omitempty"`
	ParentConnectionID int64      `json:"parent_connection_id,omitempty"`
	ChildInstallID     int64      `json:"child_install_id,omitempty"`
	ChildConnectionID  int64      `json:"child_connection_id,omitempty"`
	CallerInstallID    int64      `json:"caller_install_id,omitempty"`
	CallerAppName      string     `json:"caller_app_name,omitempty"`
	Quantity           int64      `json:"quantity"`
	Calls              int64      `json:"calls"`
	Errors             int64      `json:"errors"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
}

type integrationUsageTotal struct {
	AppSlug  string `json:"app_slug"`
	Unit     string `json:"unit"`
	Quantity int64  `json:"quantity"`
	Calls    int64  `json:"calls"`
	Errors   int64  `json:"errors"`
}

func (s *Server) handleIntegrationUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return
		}
	}
	since := integrationUsageSince(r.URL.Query().Get("period"))
	rows, err := s.listIntegrationUsageRows(userID, projectID, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totals, err := s.listIntegrationUsageTotals(userID, projectID, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, integrationUsageSummary{Since: since, Rows: rows, Totals: totals})
}

func integrationUsageSince(period string) time.Time {
	now := time.Now().UTC()
	switch strings.TrimSpace(strings.ToLower(period)) {
	case "24h", "1d", "day":
		return now.Add(-24 * time.Hour)
	case "30d", "month":
		return now.Add(-30 * 24 * time.Hour)
	case "90d":
		return now.Add(-90 * 24 * time.Hour)
	case "7d", "week", "":
		fallthrough
	default:
		return now.Add(-7 * 24 * time.Hour)
	}
}

func (s *Server) listIntegrationUsageRows(userID int64, projectID string, since time.Time) ([]integrationUsageRow, error) {
	sinceSQL := since.UTC().Format("2006-01-02 15:04:05")
	query := `
		SELECT
			e.app_slug, e.tool, e.unit, e.direction, e.grant_id, e.grant_resource,
			e.connection_id, e.parent_connection_id, e.child_install_id, e.child_connection_id,
			e.caller_install_id, e.caller_app_name,
			COALESCE(SUM(e.quantity), 0) AS quantity,
			COUNT(*) AS calls,
			COALESCE(SUM(CASE WHEN e.status = 'error' THEN 1 ELSE 0 END), 0) AS errors,
			MAX(e.created_at) AS last_used_at
		FROM integration_usage_events e
		JOIN connections c ON c.id = e.connection_id
		WHERE c.user_id = ? AND e.created_at >= ?`
	args := []any{userID, sinceSQL}
	if projectID != "" {
		query += " AND e.project_id = ?"
		args = append(args, projectID)
	}
	query += `
		GROUP BY e.app_slug, e.tool, e.unit, e.direction, e.grant_id, e.grant_resource,
			e.connection_id, e.parent_connection_id, e.child_install_id, e.child_connection_id,
			e.caller_install_id, e.caller_app_name
		ORDER BY MAX(e.created_at) DESC
		LIMIT 200`
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []integrationUsageRow{}
	for rows.Next() {
		var row integrationUsageRow
		var last sql.NullString
		if err := rows.Scan(
			&row.AppSlug, &row.Tool, &row.Unit, &row.Direction, &row.GrantID, &row.GrantResource,
			&row.ConnectionID, &row.ParentConnectionID, &row.ChildInstallID, &row.ChildConnectionID,
			&row.CallerInstallID, &row.CallerAppName, &row.Quantity, &row.Calls, &row.Errors, &last,
		); err != nil {
			return nil, err
		}
		if last.Valid {
			if t, ok := parseSQLiteTimestamp(last.String); ok {
				row.LastUsedAt = &t
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Server) listIntegrationUsageTotals(userID int64, projectID string, since time.Time) ([]integrationUsageTotal, error) {
	sinceSQL := since.UTC().Format("2006-01-02 15:04:05")
	query := `
		SELECT
			e.app_slug, e.unit,
			COALESCE(SUM(e.quantity), 0) AS quantity,
			COUNT(*) AS calls,
			COALESCE(SUM(CASE WHEN e.status = 'error' THEN 1 ELSE 0 END), 0) AS errors
		FROM integration_usage_events e
		JOIN connections c ON c.id = e.connection_id
		WHERE c.user_id = ? AND e.created_at >= ?`
	args := []any{userID, sinceSQL}
	if projectID != "" {
		query += " AND e.project_id = ?"
		args = append(args, projectID)
	}
	query += `
		GROUP BY e.app_slug, e.unit
		ORDER BY SUM(e.quantity) DESC, COUNT(*) DESC
		LIMIT 100`
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []integrationUsageTotal{}
	for rows.Next() {
		var total integrationUsageTotal
		if err := rows.Scan(&total.AppSlug, &total.Unit, &total.Quantity, &total.Calls, &total.Errors); err != nil {
			return nil, err
		}
		out = append(out, total)
	}
	return out, rows.Err()
}

func parseSQLiteTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
