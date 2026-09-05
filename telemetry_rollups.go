package main

import (
	"fmt"
	"strconv"
	"time"
)

func (s *Store) migrateTelemetryRollups() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRow("SELECT count(*) FROM server_schema_migrations WHERE version=3").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return tx.Commit()
	}
	schema := `CREATE TABLE telemetry_rollups (
 bucket INTEGER NOT NULL,agent_id INTEGER NOT NULL,thread_id TEXT NOT NULL,type TEXT NOT NULL,
 samples INTEGER NOT NULL,valid_samples INTEGER NOT NULL,tokens_in INTEGER NOT NULL,tokens_out INTEGER NOT NULL,tokens_cached INTEGER NOT NULL,cost REAL NOT NULL,cost_samples INTEGER NOT NULL,duration_sum REAL NOT NULL,duration_samples INTEGER NOT NULL,
 PRIMARY KEY(agent_id,bucket,thread_id,type));
 CREATE INDEX idx_rollups_bucket ON telemetry_rollups(bucket);`
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	cols := "bucket,agent_id,thread_id,type,samples,valid_samples,tokens_in,tokens_out,tokens_cached,cost,cost_samples,duration_sum,duration_samples"
	if _, err := tx.Exec(`INSERT INTO telemetry_rollups (` + cols + `) SELECT (COALESCE(unixepoch(substr(time,1,19)||'Z'),0)/60)*60,agent_id,thread_id,type,COUNT(*),SUM(valid),SUM(tokens_in),SUM(tokens_out),SUM(tokens_cached),SUM(cost),SUM(cost_present),COALESCE(SUM(duration),0),COUNT(duration) FROM telemetry_metrics GROUP BY agent_id,(COALESCE(unixepoch(substr(time,1,19)||'Z'),0)/60)*60,thread_id,type`); err != nil {
		return err
	}
	insert := `CREATE TRIGGER telemetry_rollup_insert AFTER INSERT ON telemetry_metrics BEGIN
 INSERT INTO telemetry_rollups (` + cols + `) VALUES((COALESCE(unixepoch(substr(NEW.time,1,19)||'Z'),0)/60)*60,NEW.agent_id,NEW.thread_id,NEW.type,1,NEW.valid,NEW.tokens_in,NEW.tokens_out,NEW.tokens_cached,NEW.cost,NEW.cost_present,COALESCE(NEW.duration,0),NEW.duration IS NOT NULL)
 ON CONFLICT(agent_id,bucket,thread_id,type) DO UPDATE SET samples=samples+1,valid_samples=valid_samples+NEW.valid,tokens_in=tokens_in+NEW.tokens_in,tokens_out=tokens_out+NEW.tokens_out,tokens_cached=tokens_cached+NEW.tokens_cached,cost=cost+NEW.cost,cost_samples=cost_samples+NEW.cost_present,duration_sum=duration_sum+COALESCE(NEW.duration,0),duration_samples=duration_samples+(NEW.duration IS NOT NULL); END`
	deletion := `CREATE TRIGGER telemetry_rollup_delete AFTER DELETE ON telemetry_metrics BEGIN
 UPDATE telemetry_rollups SET samples=samples-1,valid_samples=valid_samples-OLD.valid,tokens_in=tokens_in-OLD.tokens_in,tokens_out=tokens_out-OLD.tokens_out,tokens_cached=tokens_cached-OLD.tokens_cached,cost=cost-OLD.cost,cost_samples=cost_samples-OLD.cost_present,duration_sum=duration_sum-COALESCE(OLD.duration,0),duration_samples=duration_samples-(OLD.duration IS NOT NULL)
 WHERE agent_id=OLD.agent_id AND bucket=(COALESCE(unixepoch(substr(OLD.time,1,19)||'Z'),0)/60)*60 AND thread_id=OLD.thread_id AND type=OLD.type;
 DELETE FROM telemetry_rollups WHERE agent_id=OLD.agent_id AND bucket=(COALESCE(unixepoch(substr(OLD.time,1,19)||'Z'),0)/60)*60 AND thread_id=OLD.thread_id AND type=OLD.type AND samples=0; END`
	for _, q := range []string{insert, deletion, "INSERT INTO server_schema_migrations(version) VALUES(3)"} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("telemetry rollup migration: %w", err)
		}
	}
	return tx.Commit()
}

// Whole minutes use rollups, including the current minute: their triggers run
// with ingestion, so there is no refresh delay. Only the partial first minute
// reads compact facts to preserve the exact time-window boundary.
const metricTimeLayout = "2006-01-02T15:04:05.000000000Z"

func metricsWindow(since time.Time) string {
	since = since.UTC()
	boundary := since.Truncate(time.Minute)
	if boundary.Before(since) {
		boundary = boundary.Add(time.Minute)
	}
	unix := strconv.FormatInt(boundary.Unix(), 10)
	return `(SELECT agent_id,thread_id,type,strftime('%Y-%m-%dT%H:%M:%S.000000000Z',bucket,'unixepoch') AS time,samples,valid_samples,tokens_in,tokens_out,tokens_cached,cost,cost_samples,duration_sum,duration_samples
 FROM telemetry_rollups WHERE bucket>=` + unix + `
 UNION ALL SELECT agent_id,thread_id,type,time,1,valid,tokens_in,tokens_out,tokens_cached,cost,cost_present,COALESCE(duration,0),duration IS NOT NULL
 FROM telemetry_metrics WHERE time>='` + since.Format(metricTimeLayout) + `' AND time<'` + boundary.Format(metricTimeLayout) + `')`
}
