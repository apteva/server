package main

import (
	"fmt"
)

// An error is a failed outcome, counted once: a final LLM failure, a realtime
// failure, or a tool result reporting failure. Core reports tool failures as
// tool.result with success=false rather than a separate event, so the metrics
// projection carries them in is_error. llm.provider_error is one failed
// provider attempt that a retry or fallback may still recover, and
// event.error is a tracked execution ending after one of the failures above;
// both are reported on their own and never added to the error count.
const errorEventTypes = `'llm.error','tool.error','realtime.error'`

// windowErrors sums errors over metricsWindow rows.
const windowErrors = `CASE WHEN type IN (` + errorEventTypes + `) THEN samples ELSE errors END`

// metricErrors is 1 for a telemetry_metrics row that is an error.
const metricErrors = `(type IN (` + errorEventTypes + `) OR is_error=1)`

// toolResultFailed matches a raw tool.result payload that reports failure.
func toolResultFailed(doc string) string {
	return `(json_type(` + doc + `,'$.success')='false' OR (json_type(` + doc + `,'$.success')='integer' AND json_extract(` + doc + `,'$.success')=0))`
}

// migrateTelemetryErrors marks failed tool results as errors in the metrics
// projection and keeps an error count in the minute rollups.
func (s *Store) migrateTelemetryErrors() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRow("SELECT count(*) FROM server_schema_migrations WHERE version=8").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return tx.Commit()
	}
	projection := func(prefix string) string { return telemetryMetricsProjection(prefix, true) }
	insert, deletion := telemetryRollupTriggers(true)
	statements := []string{
		`DROP TRIGGER IF EXISTS telemetry_metrics_insert`,
		`DROP TRIGGER IF EXISTS telemetry_metrics_update`,
		`CREATE TRIGGER telemetry_metrics_insert AFTER INSERT ON telemetry BEGIN INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`CREATE TRIGGER telemetry_metrics_update AFTER UPDATE ON telemetry BEGIN DELETE FROM telemetry_metrics WHERE id=OLD.id; INSERT OR REPLACE INTO telemetry_metrics ` + projection("NEW.") + `; END`,
		`UPDATE telemetry_metrics SET is_error=1 WHERE type='tool.result' AND is_error=0 AND id IN
 (SELECT id FROM telemetry WHERE type='tool.result' AND json_valid(data) AND ` + toolResultFailed("data") + `)`,
		`ALTER TABLE telemetry_rollups ADD COLUMN errors INTEGER NOT NULL DEFAULT 0`,
		`DROP TRIGGER IF EXISTS telemetry_rollup_insert`,
		`DROP TRIGGER IF EXISTS telemetry_rollup_delete`,
		insert,
		deletion,
		`UPDATE telemetry_rollups SET errors=e.n
 FROM (SELECT agent_id,` + rollupBucket("") + ` AS bucket,thread_id,type,SUM(is_error) AS n FROM telemetry_metrics WHERE is_error=1 GROUP BY 1,2,3,4) AS e
 WHERE telemetry_rollups.agent_id=e.agent_id AND telemetry_rollups.bucket=e.bucket AND telemetry_rollups.thread_id=e.thread_id AND telemetry_rollups.type=e.type`,
		`INSERT INTO server_schema_migrations(version) VALUES(8)`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("telemetry error migration: %w", err)
		}
	}
	return tx.Commit()
}
