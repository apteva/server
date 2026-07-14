package main

// Phase 1 rename migration — instance → agent (server-internal).
//
// We bypass NewStore (which would create the post-rename schema
// directly via CREATE TABLE IF NOT EXISTS) and hand-roll a tiny DB
// with the pre-rename schema, then invoke the rename pass. The test
// asserts the agent-flavoured table + columns exist afterwards, the
// old names are gone, and the rename is idempotent on a fresh DB.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedPreRenameDB writes the pre-Phase-1 schema (`instances` table +
// `instance_id` columns in subscriptions / channels / telemetry /
// app_grants + the `app_instance_bindings` join table). Returns a
// connected *sql.DB the test can hand into a Store wrapper.
func seedPreRenameDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE instances (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`,
		`CREATE TABLE subscriptions (id INTEGER PRIMARY KEY, instance_id INTEGER)`,
		`CREATE TABLE channels (id INTEGER PRIMARY KEY, instance_id INTEGER)`,
		`CREATE TABLE telemetry (id INTEGER PRIMARY KEY, instance_id INTEGER, time DATETIME)`,
		`CREATE TABLE app_grants (id INTEGER PRIMARY KEY, instance_id INTEGER)`,
		`CREATE TABLE app_instance_bindings (install_id INTEGER, instance_id INTEGER, enabled INTEGER DEFAULT 1)`,
		`CREATE INDEX idx_telem_instance_time ON telemetry(instance_id, time)`,
		`INSERT INTO instances (id, name) VALUES (1, 'legacy-agent')`,
		`INSERT INTO subscriptions (id, instance_id) VALUES (10, 1)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	return db
}

func TestRenameMigration_HandlesPreRenameSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := seedPreRenameDB(t, path)
	t.Cleanup(func() { db.Close() })

	s := &Store{db: db}
	s.renameInstanceTablesToAgents()

	if !tableExists(db, "agents") {
		t.Fatal("agents table missing after rename")
	}
	if tableExists(db, "instances") {
		t.Error("instances table should have been renamed away")
	}
	if !tableExists(db, "app_agent_bindings") {
		t.Error("app_agent_bindings missing after rename")
	}
	if tableExists(db, "app_instance_bindings") {
		t.Error("app_instance_bindings should have been renamed away")
	}
	for _, tbl := range []string{"subscriptions", "channels", "telemetry", "app_grants", "app_agent_bindings"} {
		if !columnExists(db, tbl, "agent_id") {
			t.Errorf("%s.agent_id missing after rename", tbl)
		}
		if columnExists(db, tbl, "instance_id") {
			t.Errorf("%s.instance_id should have been renamed away", tbl)
		}
	}
	// Seeded data survives.
	var name string
	if err := db.QueryRow(`SELECT name FROM agents WHERE id=1`).Scan(&name); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	if name != "legacy-agent" {
		t.Errorf("row data corrupted: got %q", name)
	}
}

func TestRenameMigration_IsIdempotent(t *testing.T) {
	// Second call on an already-migrated schema must be a no-op (no
	// errors about destination column existing, etc.).
	path := filepath.Join(t.TempDir(), "second.db")
	db := seedPreRenameDB(t, path)
	t.Cleanup(func() { db.Close() })
	s := &Store{db: db}
	s.renameInstanceTablesToAgents()
	s.renameInstanceTablesToAgents() // should not panic or corrupt
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil {
		t.Fatalf("second pass broke agents: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row in agents, got %d", n)
	}
}

func TestRenameMigration_FreshDBUnaffected(t *testing.T) {
	// Calling the rename on a brand-new DB (no `instances`, no
	// `app_instance_bindings`) must be a clean no-op.
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Store{db: db}
	s.renameInstanceTablesToAgents() // no-op
	if tableExists(db, "agents") {
		t.Error("rename should not create tables on a fresh DB")
	}
}

func TestRepairLegacyAgentForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-agent-fks.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	statements := []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE agents (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE app_installs (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE app_agent_bindings (install_id INTEGER NOT NULL REFERENCES app_installs(id), agent_id INTEGER NOT NULL REFERENCES instances(id), enabled INTEGER NOT NULL DEFAULT 1, PRIMARY KEY (install_id, agent_id))`,
		`CREATE TABLE app_grants (id INTEGER PRIMARY KEY AUTOINCREMENT, install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE, agent_id INTEGER NOT NULL REFERENCES instances(id) ON DELETE CASCADE, effect TEXT NOT NULL, permission TEXT NOT NULL, resource TEXT NOT NULL DEFAULT '*', created_at DATETIME DEFAULT CURRENT_TIMESTAMP, created_by TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE app_grant_defaults (install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE, agent_id INTEGER NOT NULL REFERENCES instances(id) ON DELETE CASCADE, default_effect TEXT NOT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (install_id, agent_id))`,
		`INSERT INTO agents(id) VALUES (7)`,
		`INSERT INTO app_installs(id) VALUES (11)`,
		`INSERT INTO app_agent_bindings(install_id,agent_id,enabled) VALUES (11,7,1)`,
		`INSERT INTO app_grants(install_id,agent_id,effect,permission) VALUES (11,7,'allow','tools.call')`,
		`INSERT INTO app_grant_defaults(install_id,agent_id,default_effect) VALUES (11,7,'deny')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	s := &Store{db: db}
	if err := s.repairLegacyAgentForeignKeys(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"app_agent_bindings", "app_grants", "app_grant_defaults"} {
		var schema string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(schema), "references instances") || !strings.Contains(strings.ToLower(schema), "references agents") {
			t.Errorf("%s foreign key was not repaired: %s", table, schema)
		}
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Errorf("%s data was not preserved: count=%d err=%v", table, count, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM app_agent_bindings WHERE install_id=11`); err != nil {
		t.Fatalf("binding delete still broken after repair: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM app_installs WHERE id=11`); err != nil {
		t.Fatalf("install delete still broken after repair: %v", err)
	}
}
