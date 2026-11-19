package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
)

// migratedTestDatabase is a live database at this release's schema.
func migratedTestDatabase(t *testing.T) (string, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	app := channelchat.New(nil)
	if err := framework.RunMigrations(store.db, app.Manifest().Slug, app.Migrations()); err != nil {
		t.Fatal(err)
	}
	return path, store
}

func runTestUpdateCheck(t *testing.T, source string, budget int64, backupWhen string) (updateCheckReport, updateCheckOptions) {
	t.Helper()
	root := t.TempDir()
	opts := updateCheckOptions{
		Source:     source,
		Dir:        filepath.Join(root, "check"),
		Backup:     filepath.Join(root, "backup", "apteva.db"),
		BackupWhen: backupWhen,
		Budget:     budget,
	}
	opts.Report = filepath.Join(opts.Dir, "report.json")
	opts.Progress = filepath.Join(opts.Dir, "progress.json")
	if err := os.MkdirAll(filepath.Dir(opts.Backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runUpdateCheck(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(opts.Report)
	if err != nil {
		t.Fatal(err)
	}
	var report updateCheckReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, "probe.db")); !os.IsNotExist(err) {
		t.Fatal("the reduced copy was left behind")
	}
	return report, opts
}

func TestUpdateCaptureCountsOnlyCommittedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	setup, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t(v) VALUES ('a'),('b'),('c')`); err != nil {
		t.Fatal(err)
	}
	setup.Close()
	before, err := readUpdateSchemaFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	capture := startUpdateCapture(path, before)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`UPDATE t SET v=v`,               // same values: no change
		`UPDATE t SET v='z' WHERE id=1`,  // one change
		`BEGIN; DELETE FROM t; ROLLBACK`, // rolled back
		`DELETE FROM t`,                  // the truncate path still counts rows
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got := capture.stop()["t"]
	if got == nil || got.updated != 1 || got.deleted != 3 || got.inserted != 0 || !got.columns[1] || got.columns[0] {
		t.Fatalf("captured %+v", got)
	}
	// Changes after stop are not captured.
	if _, err := db.Exec(`INSERT INTO t(v) VALUES ('after')`); err != nil {
		t.Fatal(err)
	}
	if got := capture.stop(); got["t"].inserted != 0 {
		t.Fatalf("captured after stop: %+v", got["t"])
	}
}

func TestUpdateCheckCurrentDatabaseIsUnchanged(t *testing.T) {
	source, store := migratedTestDatabase(t)
	if err := store.SetSetting("update-sentinel", "keep"); err != nil {
		t.Fatal(err)
	}
	schema, err := updateDatabaseSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	report, opts := runTestUpdateCheck(t, source, updateCheckTableBudget, "incompatible")
	if report.Classification != "unchanged" || !report.RollbackSafe || !report.Compatible || report.SchemaChanged {
		t.Fatalf("report: %+v", report)
	}
	if report.SchemaHash != schema || report.Version != CLIVersion {
		t.Fatalf("report identity: %s %s", report.SchemaHash, report.Version)
	}
	if report.Backup != nil {
		t.Fatal("an unchanged release must not back up the database")
	}
	if _, err := os.Stat(opts.Backup); !os.IsNotExist(err) {
		t.Fatal("backup file written")
	}
	if after, _ := updateDatabaseSchema(source); after != schema || store.GetSetting("update-sentinel") != "keep" {
		t.Fatal("the check modified the live database")
	}
	raw, err := os.ReadFile(opts.Progress)
	if err != nil {
		t.Fatal(err)
	}
	var progress updateProgressState
	if json.Unmarshal(raw, &progress) != nil || progress.Phase != "done" {
		t.Fatalf("progress: %s", raw)
	}
}

func TestUpdateCheckAddedColumnsAndTablesAreCompatible(t *testing.T) {
	source, store := migratedTestDatabase(t)
	// Roll the live database back to before a column and a table existed;
	// this release's migrations add both again.
	for _, q := range []string{
		`ALTER TABLE agents DROP COLUMN core_started_at`,
		`DROP TABLE project_invites`,
	} {
		if _, err := store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	report, opts := runTestUpdateCheck(t, source, updateCheckTableBudget, "incompatible")
	if report.Classification != "compatible" || !report.Compatible || report.RollbackSafe || !report.SchemaChanged {
		t.Fatalf("report: %+v", report)
	}
	var addedTable, addedColumn bool
	for _, c := range report.SchemaChanges {
		addedTable = addedTable || (c.Change == "added" && c.Type == "table" && c.Name == "project_invites")
		addedColumn = addedColumn || (c.Change == "modified" && c.Name == "agents" && strings.Contains(c.Detail, "core_started_at"))
	}
	if !addedTable || !addedColumn {
		t.Fatalf("schema changes: %+v", report.SchemaChanges)
	}
	if report.Backup != nil {
		t.Fatal("a compatible release must not back up the database")
	}
	if _, err := os.Stat(opts.Backup); !os.IsNotExist(err) {
		t.Fatal("backup file written")
	}
}

func insertUpdateCheckUsers(t *testing.T, store *Store, n int) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO users(email, password_hash, role) VALUES (?, 'x', 'admin')`, fmt.Sprintf("user%d@example.test", i)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCheckRowRewritesAreIncompatibleAndBackedUp(t *testing.T) {
	source, store := migratedTestDatabase(t)
	insertUpdateCheckUsers(t, store, 3)
	// Every boot promotes the first user to admin; leave it a plain user so
	// the migrations rewrite an existing row.
	if _, err := store.db.Exec(`UPDATE users SET role='user' WHERE id=(SELECT MIN(id) FROM users)`); err != nil {
		t.Fatal(err)
	}
	schema, _ := updateDatabaseSchema(source)
	report, opts := runTestUpdateCheck(t, source, updateCheckTableBudget, "incompatible")
	if report.Classification != "incompatible" || report.Compatible || report.RollbackSafe {
		t.Fatalf("report: %+v", report)
	}
	change := report.RowChanges["users"]
	if change.Updated != 1 || change.Managed || len(change.Columns) != 1 || change.Columns[0] != "role" {
		t.Fatalf("users change: %+v", change)
	}
	if report.Backup == nil || report.Backup.Path != opts.Backup || report.Backup.Bytes <= 0 {
		t.Fatalf("backup: %+v", report.Backup)
	}
	info, err := os.Stat(opts.Backup)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup must exist and be private: %v", err)
	}
	if backupSchema, _ := updateDatabaseSchema(opts.Backup); backupSchema != schema {
		t.Fatal("backup is not a copy of the live database")
	}
	var role string
	if err := store.db.QueryRow(`SELECT role FROM users WHERE id=(SELECT MIN(id) FROM users)`).Scan(&role); err != nil || role != "user" {
		t.Fatalf("live database changed: %q %v", role, err)
	}
	// The same release with backups disabled leaves no file.
	report, opts = runTestUpdateCheck(t, source, updateCheckTableBudget, "never")
	if report.Backup != nil {
		t.Fatal("backup written with backups disabled")
	}
	if _, err := os.Stat(opts.Backup); !os.IsNotExist(err) {
		t.Fatal("backup file written")
	}
}

func TestUpdateCheckManagedBootResetsAreCompatible(t *testing.T) {
	source, store := migratedTestDatabase(t)
	insertUpdateCheckUsers(t, store, 1)
	if _, err := store.db.Exec(`INSERT INTO agents(user_id, name, directive, config, status, port, pid) VALUES ((SELECT MIN(id) FROM users), 'a', '', '{}', 'stopped', 4321, 99)`); err != nil {
		t.Fatal(err)
	}
	report, _ := runTestUpdateCheck(t, source, updateCheckTableBudget, "incompatible")
	if report.Classification != "compatible" || report.RollbackSafe {
		t.Fatalf("report: %+v", report)
	}
	if change := report.RowChanges["agents"]; change.Updated != 1 || !change.Managed {
		t.Fatalf("agents change: %+v", change)
	}
}

func TestUpdateCheckRechecksPartlyCopiedTablesInFull(t *testing.T) {
	source, store := migratedTestDatabase(t)
	insertUpdateCheckUsers(t, store, updateCheckChunkRows+500)
	// With a one-byte budget each large table keeps only its newest chunk.
	// Make the migration's rewrite visible in that chunk and outside it.
	var low int64
	if err := store.db.QueryRow(`SELECT id FROM users ORDER BY id DESC LIMIT 1 OFFSET ?`, updateCheckChunkRows-1).Scan(&low); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE users SET role='user' WHERE id IN ((SELECT MIN(id) FROM users), ?)`, low); err != nil {
		t.Fatal(err)
	}
	report, _ := runTestUpdateCheck(t, source, 1, "never")
	if len(report.FullTables) != 1 || report.FullTables[0] != "users" {
		t.Fatalf("full tables: %v (sampled %v)", report.FullTables, report.SampledTables)
	}
	for _, table := range report.SampledTables {
		if table == "users" {
			t.Fatal("users still reported as partly copied")
		}
	}
	// In full, the first user is the one promoted.
	if change := report.RowChanges["users"]; change.Updated != 1 || report.Classification != "incompatible" {
		t.Fatalf("report: %+v", report)
	}
}

func TestLegacyPreflightContract(t *testing.T) {
	source, store := migratedTestDatabase(t)
	home := t.TempDir()
	backup := filepath.Join(home, "update-backups", "job", "apteva.db")
	t.Setenv("APTEVA_HOME", home)
	t.Setenv("DB_PATH", source)
	t.Setenv("APTEVA_UPDATE_CHECK_DIR", "")
	t.Setenv("APTEVA_UPDATE_PREFLIGHT", backup)
	if code := runPreflight(); code != 0 {
		t.Fatalf("preflight exit %d", code)
	}
	var report struct {
		Version       string `json:"version"`
		SchemaChanged bool   `json:"schema_changed"`
		RollbackSafe  bool   `json:"rollback_safe"`
	}
	raw, err := os.ReadFile(backup + ".json")
	if err != nil || json.Unmarshal(raw, &report) != nil {
		t.Fatalf("legacy report: %v", err)
	}
	if report.Version != CLIVersion || report.SchemaChanged || !report.RollbackSafe {
		t.Fatalf("legacy report: %+v", report)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("unchanged release backed up for an earlier caller")
	}
	if _, err := os.Stat(backup + ".check"); !os.IsNotExist(err) {
		t.Fatal("work directory left behind")
	}
	// Earlier callers point the operator at the backup whenever anything
	// changes, so it must then exist.
	if _, err := store.db.Exec(`ALTER TABLE agents DROP COLUMN core_started_at`); err != nil {
		t.Fatal(err)
	}
	backup = filepath.Join(home, "update-backups", "job2", "apteva.db")
	t.Setenv("APTEVA_UPDATE_PREFLIGHT", backup)
	if code := runPreflight(); code != 0 {
		t.Fatalf("preflight exit %d", code)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("changed release not backed up for an earlier caller")
	}
	raw, _ = os.ReadFile(backup + ".json")
	if json.Unmarshal(raw, &report) != nil || !report.SchemaChanged || report.RollbackSafe {
		t.Fatalf("legacy report: %s", raw)
	}
}

func TestUpdateCheckRefusesExistingBackup(t *testing.T) {
	source, _ := migratedTestDatabase(t)
	root := t.TempDir()
	backup := filepath.Join(root, "apteva.db")
	if err := os.WriteFile(backup, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runUpdateCheck(context.Background(), updateCheckOptions{Source: source, Dir: filepath.Join(root, "check"), Report: filepath.Join(root, "check", "report.json"), Backup: backup, BackupWhen: "incompatible", Budget: updateCheckTableBudget})
	if err == nil {
		t.Fatal("an existing backup was accepted")
	}
	if raw, _ := os.ReadFile(backup); string(raw) != "keep" {
		t.Fatal("existing backup overwritten")
	}
}

func TestUpdateAddedColumnsMatchesSQLiteAlterTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alter.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT NOT NULL, b TEXT, UNIQUE(a, b))`); err != nil {
		t.Fatal(err)
	}
	before, err := readUpdateSchemaFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE t ADD COLUMN c INTEGER NOT NULL DEFAULT 0; ALTER TABLE t ADD COLUMN d TEXT`); err != nil {
		t.Fatal(err)
	}
	after, err := readUpdateSchemaFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	key := "table\x00t"
	added, ok := updateAddedColumns(before.columns["t"], after.columns["t"], before.objects[key].SQL, after.objects[key].SQL)
	if !ok || strings.Join(added, ",") != "c,d" {
		t.Fatalf("added %v ok %v\nbefore %s\nafter  %s", added, ok, before.objects[key].SQL, after.objects[key].SQL)
	}
	if _, ok := updateAddedColumns(after.columns["t"], before.columns["t"], after.objects[key].SQL, before.objects[key].SQL); ok {
		t.Fatal("removed columns accepted as additions")
	}
}

func TestClassifyUpdateRules(t *testing.T) {
	table := func(name, def string) updateSchemaObject {
		return updateSchemaObject{Type: "table", Name: name, Table: name, SQL: def}
	}
	index := func(name, on, def string) updateSchemaObject {
		return updateSchemaObject{Type: "index", Name: name, Table: on, SQL: def}
	}
	snap := func(objects ...updateSchemaObject) updateSchemaSnapshot {
		s := updateSchemaSnapshot{objects: map[string]updateSchemaObject{}, columns: map[string][]updateColumn{}}
		var text strings.Builder
		for _, o := range objects {
			s.objects[o.Type+"\x00"+o.Name] = o
			text.WriteString(o.Type + o.Name + o.SQL)
		}
		s.hash = fmt.Sprintf("%x", sha256.Sum256([]byte(text.String())))
		return s
	}
	base := []updateSchemaObject{table("x", "CREATE TABLE x(id INTEGER PRIMARY KEY)"), index("x_old", "x", "CREATE INDEX x_old ON x(id)")}
	cases := []struct {
		name   string
		after  []updateSchemaObject
		rows   map[string]*updateCapturedTable
		sample []string
		want   string
	}{
		{"nothing", base, nil, nil, "unchanged"},
		{"plain index", append(base[:2:2], index("x_new", "x", "CREATE INDEX x_new ON x(id)")), nil, nil, "compatible"},
		{"unique index", append(base[:2:2], index("x_u", "x", "CREATE UNIQUE INDEX x_u ON x(id)")), nil, nil, "incompatible"},
		{"index removed", base[:1], nil, nil, "compatible"},
		{"table removed", base[1:], nil, nil, "incompatible"},
		{"trigger", append(base[:2:2], updateSchemaObject{Type: "trigger", Name: "tr", Table: "x", SQL: "CREATE TRIGGER tr AFTER INSERT ON x BEGIN SELECT 1; END"}), nil, nil, "incompatible"},
		{"managed rows", base, map[string]*updateCapturedTable{"x": {updated: 2}}, nil, "compatible"},
		{"unmanaged rows", base, map[string]*updateCapturedTable{"x": {updated: 2, unmanaged: 1}}, nil, "incompatible"},
		{"ledger, all copied", base, map[string]*updateCapturedTable{"x": {inserted: 1, ledger: []string{"9"}}}, nil, "compatible"},
		{"ledger, large tables sampled", base, map[string]*updateCapturedTable{"x": {inserted: 1, ledger: []string{"9"}}}, []string{"big"}, "incompatible"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyUpdate(snap(base...), snap(c.after...), c.rows, c.sample)
			if got.Classification != c.want {
				t.Fatalf("got %s (%v), want %s", got.Classification, got.Reasons, c.want)
			}
		})
	}
	// A new column on a table earlier releases write without column names.
	cols := []updateColumn{{Name: "a", Type: "TEXT"}}
	before := snap(table("app_event_receipts", "CREATE TABLE app_event_receipts(a TEXT)"))
	before.columns["app_event_receipts"] = cols
	after := snap(table("app_event_receipts", "CREATE TABLE app_event_receipts(a TEXT, b TEXT)"))
	after.columns["app_event_receipts"] = append(cols, updateColumn{Name: "b", Type: "TEXT"})
	if got := classifyUpdate(before, after, nil, nil); got.Classification != "incompatible" {
		t.Fatalf("column-order table: %s", got.Classification)
	}
}

// Earlier releases must still run on a database a newer release added
// columns to; that needs every statement to name its columns.
func TestSQLNamesColumns(t *testing.T) {
	insert := regexp.MustCompile(`(?i)\binsert\s+(or\s+\w+\s+)?into\s+[a-z_][a-z0-9_]*\s+values\b`)
	selectAll := regexp.MustCompile(`(?i)\bselect\s+\*\s+from\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob("apps/*/*.go")
	files = append(files, more...)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if insert.MatchString(line) || selectAll.MatchString(line) {
				t.Errorf("%s:%d: name the columns: %s", file, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// updateDatabaseFingerprint hashes every stored row; tests use it to prove a
// code path changed no data.
func updateDatabaseFingerprint(path string) (string, error) {
	db, err := sql.Open("sqlite", sqliteFileURI(path)+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name!='app_metadata_generation' ORDER BY name`)
	if err != nil {
		return "", err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		tables = append(tables, name)
	}
	rows.Close()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, name := range tables {
		rows, err := db.Query("SELECT * FROM " + quoteUpdateIdent(name) + " ORDER BY rowid")
		if err != nil {
			return "", err
		}
		names, _ := rows.Columns()
		_ = encoder.Encode([]any{name, names})
		for rows.Next() {
			values := make([]any, len(names))
			dest := make([]any, len(names))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return "", err
			}
			_ = encoder.Encode(values)
		}
		rows.Close()
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
