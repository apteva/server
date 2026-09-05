package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A compact projection is maintained in the same transaction as raw events.
// There is no timer/TTL: inserts, restores, corrections and deletes are visible
// to the very next query, including writes by another server process.
func (s *Store) migrateTelemetryMetrics() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRow("SELECT count(*) FROM server_schema_migrations WHERE version=2").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return tx.Commit()
	}
	schema := `CREATE TABLE telemetry_metrics(
 id TEXT PRIMARY KEY,agent_id INTEGER NOT NULL,thread_id TEXT NOT NULL,type TEXT NOT NULL,time TEXT NOT NULL,
 valid INTEGER NOT NULL,tokens_in INTEGER NOT NULL,tokens_out INTEGER NOT NULL,tokens_cached INTEGER NOT NULL,cost REAL NOT NULL,cost_present INTEGER NOT NULL,duration REAL,
 tool_name TEXT NOT NULL,is_error INTEGER NOT NULL);
 CREATE INDEX idx_metrics_agent_time ON telemetry_metrics(agent_id,time);
 CREATE INDEX idx_metrics_agent_type_time ON telemetry_metrics(agent_id,type,time);`
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	numeric := func(field string, integer bool) string {
		value := "json_extract(doc,'$." + field + "')"
		if integer {
			value = "CAST(" + value + " AS INTEGER)"
		}
		return "CASE WHEN json_type(doc,'$." + field + "') IN ('integer','real') THEN " + value + " ELSE 0 END"
	}
	usage := func(normal, text, audio string) string {
		return "CASE WHEN kind='realtime.usage' THEN (" + numeric(text, true) + "+" + numeric(audio, true) + ") ELSE " + numeric(normal, true) + " END"
	}
	projection := func(prefix string) string {
		return `SELECT event_id,agent_id,thread_id,kind,substr(time,1,19)||'.'||CASE WHEN substr(time,20,1)='.' THEN substr(substr(time,21,length(time)-21)||'000000000',1,9) ELSE '000000000' END||'Z',valid,` +
			usage("tokens_in", "text_input_tokens", "audio_input_tokens") + `,` +
			usage("tokens_out", "text_output_tokens", "audio_output_tokens") + `,` +
			usage("tokens_cached", "text_cached_tokens", "audio_cached_tokens") + `,
  CASE WHEN kind='realtime.usage' THEN ` + numeric("cost", false) + ` ELSE ` + numeric("cost_usd", false) + ` END,
  CASE WHEN kind='realtime.usage' THEN valid ELSE COALESCE(json_type(doc,'$.cost_usd') IN ('integer','real'),0) END,
  CASE WHEN json_type(doc,'$.duration_ms') IN ('integer','real') THEN json_extract(doc,'$.duration_ms') ELSE NULL END,
  CASE WHEN json_type(doc,'$.name')='text' AND json_extract(doc,'$.name')!='' THEN json_extract(doc,'$.name') WHEN json_type(doc,'$.tool')='text' THEN json_extract(doc,'$.tool') ELSE '' END,
  COALESCE(json_type(doc,'$.is_error')='true',0)
  FROM (SELECT ` + prefix + `id AS event_id,` + prefix + `agent_id AS agent_id,` + prefix + `thread_id AS thread_id,` + prefix + `type AS kind,` + prefix + `time AS time,
   CASE WHEN json_valid(` + prefix + `data) THEN ` + prefix + `data ELSE '{}' END AS doc,
   CASE WHEN json_valid(` + prefix + `data) THEN (json_type(` + prefix + `data) IN ('object','null')) ELSE 0 END AS valid` + func() string {
			if prefix == "" {
				return " FROM telemetry"
			}
			return ""
		}() + `)`
	}
	if _, err := tx.Exec("INSERT INTO telemetry_metrics " + projection("")); err != nil {
		return fmt.Errorf("backfill metrics: %w", err)
	}
	statements := []string{
		`CREATE TRIGGER telemetry_metrics_insert AFTER INSERT ON telemetry BEGIN INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`CREATE TRIGGER telemetry_metrics_update AFTER UPDATE ON telemetry BEGIN DELETE FROM telemetry_metrics WHERE id=OLD.id; INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`CREATE TRIGGER telemetry_metrics_delete AFTER DELETE ON telemetry BEGIN DELETE FROM telemetry_metrics WHERE id=OLD.id; END`,
		`INSERT INTO server_schema_migrations(version) VALUES(2)`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("metrics trigger: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) TelemetryStats(id int64, since time.Time) (*TelemetryStats, error) {
	return s.TelemetryStatsContext(context.Background(), id, since)
}
func (s *Store) TelemetryStatsContext(ctx context.Context, id int64, since time.Time) (*TelemetryStats, error) {
	result := &TelemetryStats{}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(samples),0),
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN valid_samples ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_in ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_out ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN cost ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN duration_sum ELSE 0 END)/NULLIF(SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN duration_samples ELSE 0 END),0),0),
 COALESCE(SUM(CASE WHEN type='thread.spawn' THEN samples ELSE 0 END),0),COALESCE(SUM(CASE WHEN type='thread.done' THEN samples ELSE 0 END),0),COALESCE(SUM(CASE WHEN type='tool.call' THEN samples ELSE 0 END),0),COALESCE(SUM(CASE WHEN type LIKE '%.error' THEN samples ELSE 0 END),0)
 FROM `+metricsWindow(since)+` WHERE agent_id=? AND time>=?`, id, since.UTC().Format(metricTimeLayout)).Scan(&result.TotalEvents, &result.LLMCalls, &result.TotalTokensIn, &result.TotalTokensOut, &result.TotalCost, &result.AvgDurationMs, &result.ThreadsSpawned, &result.ThreadsDone, &result.ToolCalls, &result.Errors)
	return result, err
}

func (s *Store) TelemetryTimeline(id int64, since time.Time, minutes int) ([]TimelineBucket, error) {
	return s.TelemetryTimelineContext(context.Background(), id, since, minutes)
}
func (s *Store) TelemetryTimelineContext(ctx context.Context, id int64, since time.Time, minutes int) ([]TimelineBucket, error) {
	if minutes <= 0 {
		return nil, fmt.Errorf("bucket width must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT (unixepoch(time)/?)*? AS bucket,CASE WHEN type='llm.done' THEN thread_id ELSE '' END AS thread,
 SUM(CASE WHEN type='llm.done' THEN samples ELSE 0 END),SUM(CASE WHEN type='tool.call' THEN samples ELSE 0 END),SUM(CASE WHEN type='llm.error' THEN samples ELSE 0 END),SUM(CASE WHEN type='llm.done' THEN tokens_in ELSE 0 END),SUM(CASE WHEN type='llm.done' THEN tokens_out ELSE 0 END),SUM(CASE WHEN type='llm.done' THEN cost ELSE 0 END)
 FROM `+metricsWindow(since)+` WHERE agent_id=? AND time>=? GROUP BY bucket,thread ORDER BY bucket`, minutes*60, minutes*60, id, since.UTC().Format(metricTimeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := map[int64]*TimelineBucket{}
	out := []TimelineBucket{}
	for rows.Next() {
		var epoch int64
		var thread string
		var calls, tools, errors, in, outTokens int
		var cost float64
		if err := rows.Scan(&epoch, &thread, &calls, &tools, &errors, &in, &outTokens, &cost); err != nil {
			return nil, err
		}
		b := buckets[epoch]
		if b == nil {
			b = &TimelineBucket{Time: time.Unix(epoch, 0).UTC().Format(time.RFC3339), Threads: map[string]int{}}
			buckets[epoch] = b
		}
		b.LLMCalls += calls
		b.ToolCalls += tools
		b.Errors += errors
		b.TokensIn += in
		b.TokensOut += outTokens
		b.Cost += cost
		if calls > 0 {
			b.Threads[thread] += calls
		}
	}
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, rows.Err()
}

func metricIDs(agents []Agent) (string, []any) {
	slots := make([]string, len(agents))
	args := make([]any, len(agents))
	for i, a := range agents {
		slots[i] = "?"
		args[i] = a.ID
	}
	return strings.Join(slots, ","), args
}
func (s *Store) TelemetryStatsByProject(userID int64, project string, since time.Time) ([]InstanceStats, error) {
	return s.TelemetryStatsByProjectContext(context.Background(), userID, project, since)
}
func (s *Store) TelemetryStatsByProjectContext(ctx context.Context, userID int64, project string, since time.Time) ([]InstanceStats, error) {
	agents, err := s.listAgentsForTelemetry(userID, project)
	if err != nil {
		return nil, err
	}
	out := []InstanceStats{}
	if len(agents) == 0 {
		return out, nil
	}
	byID := map[int64]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	ids, args := metricIDs(agents)
	args = append(args, since.UTC().Format(metricTimeLayout))
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN samples ELSE 0 END),
 SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_in ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_out ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_cached ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN cost ELSE 0 END),
 SUM(CASE WHEN type IN ('llm.error','tool.error','realtime.error') THEN samples ELSE 0 END),SUM(CASE WHEN type='tool.call' THEN samples ELSE 0 END),COALESCE(SUM(CASE WHEN type='llm.done' THEN duration_sum ELSE 0 END)/NULLIF(SUM(CASE WHEN type='llm.done' THEN duration_samples ELSE 0 END),0),0),COUNT(DISTINCT CASE WHEN type IN ('llm.done','realtime.usage') AND thread_id!='' THEN thread_id END)
 FROM `+metricsWindow(since)+` WHERE agent_id IN (`+ids+`) AND time>=? AND type IN ('llm.done','realtime.usage','tool.call','llm.error','tool.error','realtime.error') GROUP BY agent_id ORDER BY 6 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a InstanceStats
		if err := rows.Scan(&a.AgentID, &a.LLMCalls, &a.TokensIn, &a.TokensOut, &a.TokensCached, &a.Cost, &a.Errors, &a.ToolCalls, &a.AvgDurationMs, &a.DistinctThreads); err != nil {
			return nil, err
		}
		a.Name = byID[a.AgentID].Name
		a.Status = byID[a.AgentID].Status
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) TelemetryTimelineByProject(userID int64, project string, since time.Time, minutes int) ([]ProjectTimelineBucket, error) {
	return s.TelemetryTimelineByProjectContext(context.Background(), userID, project, since, minutes)
}
func (s *Store) TelemetryTimelineByProjectContext(ctx context.Context, userID int64, project string, since time.Time, minutes int) ([]ProjectTimelineBucket, error) {
	if minutes <= 0 {
		return nil, fmt.Errorf("bucket width must be positive")
	}
	agents, err := s.listAgentsForTelemetry(userID, project)
	if err != nil {
		return nil, err
	}
	out := []ProjectTimelineBucket{}
	if len(agents) == 0 {
		return out, nil
	}
	ids, args := metricIDs(agents)
	args = append([]any{minutes * 60, minutes * 60}, args...)
	args = append(args, since.UTC().Format(metricTimeLayout))
	rows, err := s.db.QueryContext(ctx, `SELECT (unixepoch(time)/?)*? AS bucket,agent_id,SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN samples ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_in ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN tokens_out ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN cost ELSE 0 END),SUM(CASE WHEN type IN ('llm.error','tool.error','realtime.error') THEN samples ELSE 0 END),SUM(CASE WHEN type IN ('llm.done','realtime.usage') THEN cost_samples ELSE 0 END)
 FROM `+metricsWindow(since)+` WHERE agent_id IN (`+ids+`) AND time>=? AND type IN ('llm.done','realtime.usage','llm.error','tool.error','realtime.error') GROUP BY bucket,agent_id ORDER BY bucket`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := map[int64]*ProjectTimelineBucket{}
	for rows.Next() {
		var epoch, id int64
		var calls, in, outTokens, errors, present int
		var cost float64
		if err := rows.Scan(&epoch, &id, &calls, &in, &outTokens, &cost, &errors, &present); err != nil {
			return nil, err
		}
		b := buckets[epoch]
		if b == nil {
			b = &ProjectTimelineBucket{Time: time.Unix(epoch, 0).UTC().Format(time.RFC3339), CostByInstance: map[string]float64{}, CallsByInstance: map[string]int{}}
			buckets[epoch] = b
		}
		b.LLMCalls += calls
		b.TokensIn += in
		b.TokensOut += outTokens
		b.Cost += cost
		b.Errors += errors
		key := strconv.FormatInt(id, 10)
		if calls > 0 {
			b.CallsByInstance[key] += calls
		}
		if present > 0 {
			b.CostByInstance[key] += cost
		}
	}
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, rows.Err()
}
