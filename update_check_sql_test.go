package main

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func TestUpdateWriteTargets(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{"UPDATE agents SET port=0 WHERE status='stopped'", []string{"agents"}},
		{"update OR replace \"Odd Name\" set x=1", []string{"odd name"}},
		{"DELETE FROM main.telemetry WHERE ts < ?", []string{"telemetry"}},
		{"WITH old AS (SELECT id FROM users) DELETE FROM [sessions] WHERE user_id IN old", []string{"sessions"}},
		{"INSERT INTO t(a) VALUES (1) ON CONFLICT(a) DO UPDATE SET a=excluded.a", nil},
		{"CREATE TABLE c (p INTEGER REFERENCES p(id) ON DELETE CASCADE ON UPDATE CASCADE)", nil},
		{"CREATE TRIGGER tr AFTER UPDATE OF status ON agents BEGIN UPDATE audit SET n=n+1; END", []string{"audit"}},
		{"CREATE TRIGGER tr2 AFTER DELETE ON agents BEGIN SELECT 1; END", nil},
		{"SELECT 'UPDATE users SET x=1' -- DELETE FROM users\n/* UPDATE agents */", nil},
		{"UPDATE users SET a=1; DELETE FROM sessions; UPDATE `apps` SET b=2", []string{"apps", "sessions", "users"}},
	} {
		got := []string{}
		for table := range updateWriteTargets(tc.sql) {
			got = append(got, table)
		}
		sort.Strings(got)
		want := tc.want
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", tc.sql, got, want)
		}
	}
}

// A boot UPDATE that matches only rows the reduced copy left out changes
// nothing in the copy. The check must still see that the release writes to
// the table, copy it in full, and find the change.
func TestUpdateCheckSeesWritesToRowsOutsideTheCopy(t *testing.T) {
	source, store := migratedTestDatabase(t)
	insertUpdateCheckUsers(t, store, 1)
	insertUpdateCheckAgents(t, store, updateCheckChunkRows+500)
	// Only the oldest agent, far outside the newest chunk the copy keeps,
	// is a stopped agent still holding a port: every boot clears it.
	if _, err := store.db.Exec(`UPDATE agents SET status='stopped', port=4321, pid=99 WHERE id=(SELECT MIN(id) FROM agents)`); err != nil {
		t.Fatal(err)
	}
	report, _ := runTestUpdateCheck(t, source, 1, "never")
	full := false
	for _, table := range report.FullTables {
		full = full || table == "agents"
	}
	if !full {
		t.Fatalf("agents not copied in full: full %v, sampled %v", report.FullTables, report.SampledTables)
	}
	if change := report.RowChanges["agents"]; change.Updated != 1 {
		t.Fatalf("the boot reset of the old agent was missed: %+v (classification %s)", report.RowChanges, report.Classification)
	}
	if report.Classification == "unchanged" || report.RollbackSafe {
		t.Fatalf("a release that changes rows was reported unchanged: %+v", report)
	}
}

func insertUpdateCheckAgents(t *testing.T, store *Store, n int) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	var stmt *sql.Stmt
	if stmt, err = tx.Prepare(`INSERT INTO agents(user_id, name, status, port, pid) VALUES ((SELECT MIN(id) FROM users), ?, 'running', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err = stmt.Exec(fmt.Sprintf("agent-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
