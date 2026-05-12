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
