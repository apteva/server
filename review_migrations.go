package main

import (
	"encoding/json"
	"fmt"
)

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
	var proactivityMigrated int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM server_schema_migrations WHERE version=4`).Scan(&proactivityMigrated); err != nil {
		return err
	}
	if proactivityMigrated == 0 {
		for _, q := range []string{
			`ALTER TABLE agents ADD COLUMN proactivity INTEGER NOT NULL DEFAULT 25 CHECK(typeof(proactivity)='integer' AND proactivity BETWEEN 0 AND 100)`,
			`DROP TRIGGER IF EXISTS agent_behavior_update`,
			`CREATE TRIGGER agent_behavior_update AFTER UPDATE OF directive,mode,proactivity ON agents WHEN NEW.directive IS NOT OLD.directive OR NEW.mode IS NOT OLD.mode OR NEW.proactivity IS NOT OLD.proactivity BEGIN INSERT INTO agent_behavior_state(agent_id,revision,version) VALUES(NEW.id,1,2) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1,version=2,last_error=''; END`,
			`INSERT INTO server_schema_migrations(version) VALUES(4)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("agent proactivity migration: %w", err)
			}
		}
	}
	var sharperPolicyMigrated int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM server_schema_migrations WHERE version=5`).Scan(&sharperPolicyMigrated); err != nil {
		return err
	}
	if sharperPolicyMigrated == 0 {
		for _, q := range []string{
			`DROP TRIGGER IF EXISTS agent_behavior_update`,
			`CREATE TRIGGER agent_behavior_update AFTER UPDATE OF directive,mode,proactivity ON agents WHEN NEW.directive IS NOT OLD.directive OR NEW.mode IS NOT OLD.mode OR NEW.proactivity IS NOT OLD.proactivity BEGIN INSERT INTO agent_behavior_state(agent_id,revision,version) VALUES(NEW.id,1,3) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1,version=3,last_error=''; END`,
			`INSERT INTO server_schema_migrations(version) VALUES(5)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("agent initiative policy migration: %w", err)
			}
		}
	}
	var genericControlsMigrated int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM server_schema_migrations WHERE version=6`).Scan(&genericControlsMigrated); err != nil {
		return err
	}
	if genericControlsMigrated == 0 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS agent_control_assignments(
				agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
				control_key TEXT NOT NULL,
				value_json TEXT NOT NULL,
				source TEXT NOT NULL DEFAULT 'agent',
				locked INTEGER NOT NULL DEFAULT 0 CHECK(locked IN (0,1)),
				revision INTEGER NOT NULL DEFAULT 1,
				updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY(agent_id,control_key)
			)`,
			`INSERT OR REPLACE INTO agent_control_assignments(agent_id,control_key,value_json,source,locked,revision)
			 SELECT id,'execution.approval',json_quote(COALESCE(NULLIF(mode,''),'autonomous')),'agent',0,1 FROM agents`,
			`INSERT OR REPLACE INTO agent_control_assignments(agent_id,control_key,value_json,source,locked,revision)
			 SELECT id,'initiative.level',CAST(proactivity AS TEXT),'agent',0,1 FROM agents`,
			`CREATE TRIGGER IF NOT EXISTS agent_controls_insert AFTER INSERT ON agents BEGIN
			 INSERT INTO agent_control_assignments(agent_id,control_key,value_json) VALUES(NEW.id,'execution.approval',json_quote(COALESCE(NULLIF(NEW.mode,''),'autonomous')));
			 INSERT INTO agent_control_assignments(agent_id,control_key,value_json) VALUES(NEW.id,'initiative.level',CAST(NEW.proactivity AS TEXT));
			 END`,
			`CREATE TRIGGER IF NOT EXISTS agent_controls_update AFTER UPDATE OF mode,proactivity ON agents
			 WHEN NEW.mode IS NOT OLD.mode OR NEW.proactivity IS NOT OLD.proactivity BEGIN
			 INSERT INTO agent_control_assignments(agent_id,control_key,value_json,revision,updated_at) VALUES(NEW.id,'execution.approval',json_quote(COALESCE(NULLIF(NEW.mode,''),'autonomous')),1,CURRENT_TIMESTAMP)
			 ON CONFLICT(agent_id,control_key) DO UPDATE SET value_json=excluded.value_json,revision=agent_control_assignments.revision+1,updated_at=CURRENT_TIMESTAMP;
			 INSERT INTO agent_control_assignments(agent_id,control_key,value_json,revision,updated_at) VALUES(NEW.id,'initiative.level',CAST(NEW.proactivity AS TEXT),1,CURRENT_TIMESTAMP)
			 ON CONFLICT(agent_id,control_key) DO UPDATE SET value_json=excluded.value_json,revision=agent_control_assignments.revision+1,updated_at=CURRENT_TIMESTAMP;
			 END`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("generic agent controls migration: %w", err)
			}
		}

		rows, err := tx.Query(`SELECT id,directive,config FROM agents`)
		if err != nil {
			return fmt.Errorf("read legacy compiled directives: %w", err)
		}
		type directiveMigration struct {
			id        int64
			directive string
			config    string
			changed   bool
		}
		var directives []directiveMigration
		for rows.Next() {
			var entry directiveMigration
			if err := rows.Scan(&entry.id, &entry.directive, &entry.config); err != nil {
				rows.Close()
				return err
			}
			clean := withoutAgentControls(entry.directive)
			if clean != entry.directive {
				entry.directive = clean
				entry.changed = true
			}
			var config map[string]any
			if behaviorSection.MatchString(entry.config) && json.Unmarshal([]byte(entry.config), &config) == nil && config != nil {
				before := entry.config
				sanitizeDirectiveFields(config)
				if encoded, encodeErr := json.Marshal(config); encodeErr == nil && string(encoded) != before {
					entry.config = string(encoded)
					entry.changed = true
				}
			}
			if entry.changed {
				directives = append(directives, entry)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, entry := range directives {
			if _, err := tx.Exec(`UPDATE agents SET directive=?,config=? WHERE id=?`, entry.directive, entry.config, entry.id); err != nil {
				return fmt.Errorf("clean legacy compiled directive agent %d: %w", entry.id, err)
			}
		}

		for _, q := range []string{
			`DROP TRIGGER IF EXISTS agent_behavior_update`,
			`CREATE TRIGGER agent_behavior_update AFTER UPDATE OF directive,mode,proactivity ON agents WHEN NEW.directive IS NOT OLD.directive OR NEW.mode IS NOT OLD.mode OR NEW.proactivity IS NOT OLD.proactivity BEGIN INSERT INTO agent_behavior_state(agent_id,revision,version) VALUES(NEW.id,1,4) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1,version=4,last_error=''; END`,
			`INSERT INTO server_schema_migrations(version) VALUES(6)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("generic agent controls migration: %w", err)
			}
		}
	}
	return tx.Commit()
}
