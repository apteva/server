package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const telemetryReadColumns = `rowid,id,agent_id,thread_id,type,time,data`

// Historical reads share filtering and row decoding. Paging, projection and
// authorization belong to their callers; raw internal readers never grant access.
func telemetryHistoryFilter(agentIDs []int64, q sdk.TelemetryQuery) (string, []any) {
	if len(agentIDs) == 0 {
		return "0", nil
	}
	marks := make([]string, 0, len(agentIDs))
	args := make([]any, 0, len(agentIDs))
	for _, id := range agentIDs {
		marks = append(marks, "?")
		args = append(args, id)
	}
	where := `agent_id IN (` + strings.Join(marks, ",") + `)`
	if q.ThreadID != "" {
		where += ` AND thread_id=?`
		args = append(args, q.ThreadID)
	}
	if q.ThreadPrefix != "" {
		where += ` AND substr(thread_id,1,length(?))=?`
		args = append(args, q.ThreadPrefix, q.ThreadPrefix)
	}
	if !q.Since.IsZero() {
		where += ` AND julianday(time)>=julianday(?)`
		args = append(args, q.Since.UTC().Format(time.RFC3339Nano))
	}
	if !q.Until.IsZero() {
		where += ` AND julianday(time)<=julianday(?)`
		args = append(args, q.Until.UTC().Format(time.RFC3339Nano))
	}
	if len(q.Events) > 0 {
		parts := make([]string, 0, len(q.Events))
		for _, event := range q.Events {
			if strings.Contains(event, ".") {
				parts = append(parts, "type=?")
				args = append(args, event)
			} else {
				parts = append(parts, `(type=? OR type LIKE ? ESCAPE '\')`)
				escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(event)
				args = append(args, event, escaped+".%")
			}
		}
		where += ` AND (` + strings.Join(parts, " OR ") + `)`
	}
	return where, args
}

func scanTelemetryRow(rows *sql.Rows) (int64, TelemetryEvent, error) {
	var row int64
	var event TelemetryEvent
	var timestamp, data string
	if err := rows.Scan(&row, &event.ID, &event.AgentID, &event.ThreadID, &event.Type, &timestamp, &data); err != nil {
		return 0, event, err
	}
	var err error
	event.Time, err = parseTime(timestamp)
	if err != nil {
		return 0, event, fmt.Errorf("invalid stored telemetry timestamp: %w", err)
	}
	event.Data = json.RawMessage(data)
	return row, event, nil
}

func (s *Store) QueryTelemetryContext(ctx context.Context, agentID int64, eventType string, since time.Time, limit int, threadID ...string) ([]TelemetryEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := sdk.TelemetryQuery{Since: since}
	if eventType != "" {
		q.Events = []string{eventType}
	}
	if len(threadID) > 0 {
		q.ThreadID = threadID[0]
	}
	where, args := telemetryHistoryFilter([]int64{agentID}, q)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+telemetryReadColumns+` FROM telemetry WHERE `+where+` ORDER BY time DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []TelemetryEvent
	for rows.Next() {
		_, event, err := scanTelemetryRow(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// Compatibility routes keep their array responses and original authorization,
// while sharing cancellation, history filters and public-content sanitization.
func (s *Server) queryPublicTelemetry(ctx context.Context, agentID int64, eventType string, since time.Time, limit int, threadID string) ([]TelemetryEvent, error) {
	events, err := s.store.QueryTelemetryContext(ctx, agentID, eventType, since, limit, threadID)
	if err != nil {
		return nil, err
	}
	return publicTelemetryEvents(events), nil
}
