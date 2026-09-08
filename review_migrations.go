package main

import "fmt"

// New migrations are versioned and fail atomically. Older compatibility
// migrations remain intact so existing deployments can still upgrade.
func (s *Store) migrateReviewFixes() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS server_schema_migrations(version INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS email_webhook_receipts(project_id TEXT NOT NULL,message_id TEXT NOT NULL,received_at INTEGER NOT NULL,PRIMARY KEY(project_id,message_id))`,
		`CREATE INDEX IF NOT EXISTS idx_email_receipt_time ON email_webhook_receipts(received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_telemetry_retention_time ON telemetry(time)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_project ON agents(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status)`,
		`INSERT OR IGNORE INTO server_schema_migrations(version) VALUES(1)`,
		`CREATE TABLE IF NOT EXISTS agent_behavior_state(agent_id INTEGER PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE, revision INTEGER NOT NULL DEFAULT 0, main_revision INTEGER NOT NULL DEFAULT 0, applied_revision INTEGER NOT NULL DEFAULT 0, version INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '')`,
		`INSERT OR IGNORE INTO agent_behavior_state(agent_id) SELECT id FROM agents`,
		`CREATE TRIGGER IF NOT EXISTS agent_behavior_insert AFTER INSERT ON agents BEGIN INSERT INTO agent_behavior_state(agent_id) VALUES(NEW.id); END`,
		`CREATE TRIGGER IF NOT EXISTS agent_behavior_update AFTER UPDATE OF directive,mode ON agents WHEN NEW.directive IS NOT OLD.directive OR NEW.mode IS NOT OLD.mode BEGIN INSERT INTO agent_behavior_state(agent_id,revision,version) VALUES(NEW.id,1,1) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1,version=1,last_error=''; END`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("server review migration: %w", err)
		}
	}
	return tx.Commit()
}
