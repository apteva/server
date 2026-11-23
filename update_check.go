package main

// update_check.go — the database check a release runs on this installation
// before it is activated (`apteva-server --preflight`).
//
// It answers three questions without copying the whole live database:
//
//  1. Do this release's migrations succeed on this database?
//  2. Do they change anything? ("unchanged": apteva-host may hand over to the
//     release live, without migrating.)
//  3. Can the previous release still run on the migrated database?
//     ("compatible": a failed update can return to the previous release
//     automatically, and no backup is needed.)
//
// The check builds a reduced copy of the live database: every schema object,
// every small table in full, and the newest rows of each large table up to a
// byte budget. It runs the release's migrations on that copy and records every
// row they change through SQLite's pre-update hook. When a change touches a
// table that was only partly copied, the check runs again with that table
// copied in full, so the result never rests on a partial table that changed.
//
// A backup of the live database is written only when the caller's policy asks
// for one, typically when the release is not compatible.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// updateCheckTableBudget is how much of each table the reduced copy holds
	// before it stops copying older rows. Tables under it are copied in full.
	updateCheckTableBudget = int64(32 << 20)
	updateCheckChunkRows   = 2000
	// updateCheckFreeSpace is the free disk the reduced copy needs beyond the
	// backup reserve; the copy itself stays near the table budgets.
	updateCheckFreeSpace = int64(256 << 20)
)

// updateCheckSpaceFloor is the free disk the check leaves on the copy's and
// the live database's filesystems: the backup reserve, or a twentieth of the
// database when that is larger. A migration that
// rewrites a big table makes the check copy that table in full, and the live
// database's write-ahead log keeps growing while the copy holds its read
// transaction; the check stops before either can fill the disk.
func updateCheckSpaceFloor(dbBytes int64) int64 {
	floor := backupDiskReserve
	if share := dbBytes / 20; share > floor {
		floor = share
	}
	return floor
}

// updateCheckSpaceGuard reports an error once free space on any of dirs falls
// below floor.
func updateCheckSpaceGuard(floor int64, dirs ...string) func() error {
	return func() error {
		for _, dir := range dirs {
			free, err := runtimeStateDiskFree(dir)
			if err != nil {
				return fmt.Errorf("cannot read free disk space for %s: %w", dir, err)
			}
			if free < floor {
				return fmt.Errorf("stopped the database check: %s has %d bytes free, below the %d bytes it must keep for the running server; the update was not applied", dir, free, floor)
			}
		}
		return nil
	}
}

type updateCheckOptions struct {
	Source     string // live database
	Dir        string // private work directory for the reduced copy
	Report     string // where the JSON report is written
	Progress   string // optional progress file for the caller
	Backup     string // optional backup destination
	BackupWhen string // never, incompatible, or changed
	Budget     int64  // per-table byte budget; 0 copies every table in full
}

type updateRowChange struct {
	Inserted int64    `json:"inserted,omitempty"`
	Updated  int64    `json:"updated,omitempty"`
	Deleted  int64    `json:"deleted,omitempty"`
	Columns  []string `json:"columns,omitempty"`
	// Managed: every change is one each release makes at boot, so the
	// previous release copes with it.
	Managed bool `json:"managed,omitempty"`
}

type updateSchemaChange struct {
	Change string `json:"change"` // added, removed, modified
	Type   string `json:"type"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

type updateBackupInfo struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type updateCheckReport struct {
	Version string `json:"version"`
	// SchemaHash identifies the live schema the check ran against.
	SchemaHash    string `json:"schema_hash"`
	SchemaChanged bool   `json:"schema_changed"`
	// RollbackSafe keeps its original meaning for existing callers: the
	// migrations change nothing.
	RollbackSafe   bool                       `json:"rollback_safe"`
	Compatible     bool                       `json:"compatible"`
	Classification string                     `json:"classification"` // unchanged, compatible, incompatible
	Reasons        []string                   `json:"reasons,omitempty"`
	SchemaChanges  []updateSchemaChange       `json:"schema_changes,omitempty"`
	RowChanges     map[string]updateRowChange `json:"row_changes,omitempty"`
	Migrations     []string                   `json:"one_time_migrations,omitempty"`
	SampledTables  []string                   `json:"sampled_tables,omitempty"`
	FullTables     []string                   `json:"full_tables,omitempty"`
	DatabaseBytes  int64                      `json:"database_bytes"`
	ProbeBytes     int64                      `json:"probe_bytes"`
	Backup         *updateBackupInfo          `json:"backup,omitempty"`
	DurationMS     int64                      `json:"duration_ms"`
}

// runUpdateCheck runs the check and writes its report. The reduced copy is
// always removed; a partial backup is removed when the check does not finish.
func runUpdateCheck(ctx context.Context, opts updateCheckOptions) (err error) {
	started := time.Now()
	if !filepath.IsAbs(opts.Source) || !filepath.IsAbs(opts.Dir) || !filepath.IsAbs(opts.Report) {
		return fmt.Errorf("absolute database, work directory and report paths required")
	}
	if opts.Backup != "" && (!filepath.IsAbs(opts.Backup) || opts.Backup == opts.Source) {
		return fmt.Errorf("the backup path must be absolute and differ from the database")
	}
	switch opts.BackupWhen {
	case "never", "incompatible", "changed":
	default:
		return fmt.Errorf("unknown backup policy %q", opts.BackupWhen)
	}
	if opts.BackupWhen != "never" && opts.Backup == "" {
		return fmt.Errorf("backup policy %q needs a backup path", opts.BackupWhen)
	}
	info, err := os.Stat(opts.Source)
	if err != nil {
		return err
	}
	if opts.Backup != "" {
		if _, err := os.Lstat(opts.Backup); !os.IsNotExist(err) {
			return fmt.Errorf("backup already exists or cannot be inspected")
		}
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return err
	}
	if err := backupSpace(opts.Dir, updateCheckFreeSpace); err != nil {
		return fmt.Errorf("not enough free disk space for the update check: %w", err)
	}
	probe := filepath.Join(opts.Dir, "probe.db")
	defer removeSQLiteFiles(probe)
	progress := newUpdateProgress(opts.Progress)
	defer progress.close()

	dbBytes := info.Size()
	if wal, e := os.Stat(opts.Source + "-wal"); e == nil {
		dbBytes += wal.Size()
	}
	full := map[string]bool{}
	space := updateCheckSpaceGuard(updateCheckSpaceFloor(dbBytes), opts.Dir, filepath.Dir(opts.Source))
	var result updateProbeResult
	for {
		result, err = runUpdateProbe(ctx, opts.Source, probe, opts.Budget, full, progress, space)
		if err != nil {
			return err
		}
		var escalate []string
		for _, table := range result.build.sampled {
			if result.touches(table) {
				escalate = append(escalate, table)
			}
		}
		if len(escalate) == 0 {
			break
		}
		for _, table := range escalate {
			full[table] = true
		}
		progress.set("rechecking", fmt.Sprintf("Migrations change %s; checking again with %s copied in full", strings.Join(escalate, ", "), plural(len(escalate), "that table", "those tables")), 0, 0)
	}

	report := classifyUpdate(result.build.before, result.after, result.rows, result.build.sampled)
	report.Version = CLIVersion
	report.SchemaHash = result.build.before.hash
	report.DatabaseBytes = dbBytes
	report.ProbeBytes = result.build.bytes
	report.SampledTables = result.build.sampled
	for table := range full {
		report.FullTables = append(report.FullTables, table)
	}
	sort.Strings(report.FullTables)
	removeSQLiteFiles(probe)

	needBackup := (opts.BackupWhen == "incompatible" && !report.Compatible) ||
		(opts.BackupWhen == "changed" && !report.RollbackSafe)
	if needBackup {
		progress.set("backing_up", "Backing up the database", 0, dbBytes)
		err = onlineBackupPathProgress(ctx, opts.Source, opts.Backup, func(done, total int64) {
			progress.set("backing_up", fmt.Sprintf("Backing up the database (%s of %s)", formatUpdateBytes(done), formatUpdateBytes(total)), done, total)
		})
		if err != nil {
			return fmt.Errorf("database backup: %w", err)
		}
		if err := os.Chmod(opts.Backup, 0o600); err != nil {
			return err
		}
		backupInfo, err := os.Stat(opts.Backup)
		if err != nil {
			return err
		}
		report.Backup = &updateBackupInfo{Path: opts.Backup, Bytes: backupInfo.Size()}
	}
	report.DurationMS = time.Since(started).Milliseconds()
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := writeUpdateFileAtomic(opts.Report, raw); err != nil {
		return err
	}
	progress.set("done", updateCheckSummary(report), 1, 1)
	return nil
}

func updateCheckSummary(r updateCheckReport) string {
	switch r.Classification {
	case "unchanged":
		return "Database check passed: this release does not change stored data"
	case "compatible":
		return "Database check passed: this release adds to the database; the previous version can still use it"
	}
	if len(r.Reasons) > 0 {
		return "Database check passed: this release migrates the database (" + r.Reasons[0] + ")"
	}
	return "Database check passed: this release migrates the database"
}

type updateProbeResult struct {
	build  updateProbeBuild
	after  updateSchemaSnapshot
	rows   map[string]*updateCapturedTable
	writes map[string]bool // tables migrations ran UPDATE or DELETE on
}

// touches reports whether the migrations changed rows or schema objects of
// table, or ran an UPDATE or DELETE on it: on a partly copied table such a
// statement may match only rows the copy left out.
func (r updateProbeResult) touches(table string) bool {
	if t := r.rows[table]; t != nil && t.changes() > 0 {
		return true
	}
	if r.writes[strings.ToLower(table)] {
		return true
	}
	for key, after := range r.after.objects {
		before, ok := r.build.before.objects[key]
		if (after.Table == table || before.Table == table) && (!ok || before.SQL != after.SQL) {
			return true
		}
	}
	for key, before := range r.build.before.objects {
		if _, ok := r.after.objects[key]; !ok && before.Table == table {
			return true
		}
	}
	return false
}

func runUpdateProbe(ctx context.Context, source, probe string, budget int64, full map[string]bool, progress *updateProgress, space func() error) (updateProbeResult, error) {
	var result updateProbeResult
	removeSQLiteFiles(probe)
	build, err := buildUpdateProbe(ctx, source, probe, budget, full, progress, space)
	if err != nil {
		return result, err
	}
	result.build = build
	if err := ctx.Err(); err != nil {
		return result, err
	}
	progress.set("migrating", "Running this release's migrations on the copy", 0, 0)
	capture := startUpdateCapture(probe, build.before)
	store, err := NewStore(probe)
	if err == nil {
		app := channelchat.New(nil)
		if err = framework.RunMigrations(store.db, app.Manifest().Slug, app.Migrations()); err != nil {
			err = fmt.Errorf("built-in app migrations: %w", err)
		}
		if closeErr := store.Close(); err == nil {
			err = closeErr
		}
	}
	result.rows = capture.stop()
	result.writes = capture.written()
	if err != nil {
		return result, fmt.Errorf("this release's migrations failed on a copy of the database: %w", err)
	}
	result.after, err = readUpdateSchemaFile(ctx, probe)
	return result, err
}

// ---- reduced copy -------------------------------------------------------

type updateSchemaObject struct {
	Type, Name, Table, SQL string
}

type updateColumn struct {
	Name, Type string
	NotNull    bool
	Default    sql.NullString
	PK, Hidden int
}

type updateSchemaSnapshot struct {
	// hash covers every sqlite_master row, exactly as updateDatabaseSchema.
	hash    string
	objects map[string]updateSchemaObject // type + "\x00" + name
	columns map[string][]updateColumn     // table → columns, in table order
}

type updateProbeBuild struct {
	before  updateSchemaSnapshot
	sampled []string
	bytes   int64
}

func buildUpdateProbe(ctx context.Context, source, probe string, budget int64, full map[string]bool, progress *updateProgress, space func() error) (build updateProbeBuild, err error) {
	db, err := sql.Open("sqlite", sqliteFileURI(probe)+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return build, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return build, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return build, err
	}
	src := &url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, src.String()); err != nil {
		return build, fmt.Errorf("open the live database read-only: %w", err)
	}
	// One read transaction: every table comes from the same moment.
	if _, err = conn.ExecContext(ctx, `BEGIN`); err != nil {
		return build, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	before, err := readUpdateSchema(ctx, conn, "src")
	if err != nil {
		return build, err
	}
	build.before = before

	objects := make([]updateSchemaObject, 0, len(before.objects))
	for _, o := range before.objects {
		objects = append(objects, o)
	}
	// Creation order matters for views and triggers; sqlite_master order is
	// not preserved by the map, so order by type, then name.
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Type != objects[j].Type {
			return updateObjectRank(objects[i].Type) < updateObjectRank(objects[j].Type)
		}
		return objects[i].Name < objects[j].Name
	})
	virtual := map[string]bool{}
	for _, o := range objects {
		if o.Type == "table" && strings.HasPrefix(strings.ToUpper(o.SQL), "CREATE VIRTUAL TABLE") {
			virtual[o.Name] = true
		}
	}
	shadow := func(name string) bool {
		for v := range virtual {
			if name != v && strings.HasPrefix(name, v+"_") {
				return true
			}
		}
		return false
	}
	var tables []updateSchemaObject
	for _, o := range objects {
		if o.Type != "table" || strings.HasPrefix(o.Name, "sqlite_") || shadow(o.Name) {
			continue
		}
		if _, err = conn.ExecContext(ctx, o.SQL); err != nil {
			return build, fmt.Errorf("create %s in the copy: %w", o.Name, err)
		}
		if !virtual[o.Name] {
			tables = append(tables, o)
		}
	}
	var pageSize int64
	if err = conn.QueryRowContext(ctx, `PRAGMA main.page_size`).Scan(&pageSize); err != nil {
		return build, err
	}
	pages := func() (int64, error) {
		var n int64
		err := conn.QueryRowContext(ctx, `PRAGMA main.page_count`).Scan(&n)
		return n, err
	}
	for i, table := range tables {
		progress.set("copying", fmt.Sprintf("Copying tables (%d of %d)", i+1, len(tables)), int64(i), int64(len(tables)))
		limit := budget
		if full[table.Name] {
			limit = 0
		}
		sampled, err := copyUpdateProbeTable(ctx, conn, table, before.columns[table.Name], limit, pageSize, pages, space)
		if err != nil {
			return build, fmt.Errorf("copy %s: %w", table.Name, err)
		}
		if sampled {
			build.sampled = append(build.sampled, table.Name)
		}
	}
	if _, ok := before.objects["table\x00sqlite_sequence"]; ok {
		if _, err = conn.ExecContext(ctx, `DELETE FROM main.sqlite_sequence`); err != nil {
			return build, err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO main.sqlite_sequence(name, seq) SELECT name, seq FROM src.sqlite_sequence`); err != nil {
			return build, err
		}
	}
	// Indexes and triggers after the rows: triggers must not fire on copied
	// rows, and each index is built once.
	for _, o := range objects {
		if o.Type == "table" || o.SQL == "" || strings.HasPrefix(o.Name, "sqlite_") || shadow(o.Table) {
			continue
		}
		if _, err = conn.ExecContext(ctx, o.SQL); err != nil {
			return build, fmt.Errorf("create %s %s in the copy: %w", o.Type, o.Name, err)
		}
	}
	copied, err := readUpdateSchema(ctx, conn, "main")
	if err != nil {
		return build, err
	}
	if !reflect.DeepEqual(copied.objects, before.objects) {
		return build, errors.New("the reduced copy does not reproduce the live schema")
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return build, err
	}
	committed = true
	if _, err = conn.ExecContext(ctx, `DETACH DATABASE src`); err != nil {
		return build, err
	}
	n, err := pages()
	if err != nil {
		return build, err
	}
	build.bytes = n * pageSize
	sort.Strings(build.sampled)
	return build, nil
}

func updateObjectRank(kind string) int {
	switch kind {
	case "table":
		return 0
	case "index":
		return 1
	case "view":
		return 2
	}
	return 3 // trigger
}

// copyUpdateProbeTable copies a table newest-first, in rowid order, until the
// copy of the table has used limit bytes (0: no limit). It reports whether
// older rows were left out.
func copyUpdateProbeTable(ctx context.Context, conn *sql.Conn, table updateSchemaObject, columns []updateColumn, limit, pageSize int64, pages func() (int64, error), space func() error) (bool, error) {
	if space == nil {
		space = func() error { return nil }
	}
	quoted := quoteUpdateIdent(table.Name)
	var names []string
	rowidAlias := false
	for _, c := range columns {
		if c.Hidden != 0 {
			continue // generated columns cannot be inserted
		}
		names = append(names, quoteUpdateIdent(c.Name))
		if c.PK == 1 && strings.EqualFold(c.Type, "INTEGER") {
			rowidAlias = true
		}
	}
	if len(names) == 0 {
		return false, nil
	}
	withoutRowid := strings.Contains(strings.ToUpper(table.SQL), "WITHOUT ROWID")
	if withoutRowid {
		// No rowid to page by: copy whole. No such table exists today.
		if err := space(); err != nil {
			return false, err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO main.`+quoted+`(`+strings.Join(names, ",")+`) SELECT `+strings.Join(names, ",")+` FROM src.`+quoted)
		return false, err
	}
	target, selected := strings.Join(names, ","), strings.Join(names, ",")
	if !rowidAlias {
		// Keep rowids so the copy is an exact subset of the live table.
		target, selected = "rowid,"+target, "rowid,"+selected
	}
	start, err := pages()
	if err != nil {
		return false, err
	}
	cursor := int64(1<<63 - 1)
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := space(); err != nil {
			return false, err
		}
		var low sql.NullInt64
		var count int64
		bound := `rowid < ?`
		if first {
			bound = `rowid <= ?`
		}
		if err := conn.QueryRowContext(ctx, `SELECT min(r), count(*) FROM (SELECT rowid AS r FROM src.`+quoted+` WHERE `+bound+` ORDER BY rowid DESC LIMIT ?)`, cursor, updateCheckChunkRows).Scan(&low, &count); err != nil {
			return false, err
		}
		if count == 0 || !low.Valid {
			return false, nil
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO main.`+quoted+`(`+target+`) SELECT `+selected+` FROM src.`+quoted+` WHERE rowid >= ? AND `+bound, low.Int64, cursor); err != nil {
			return false, err
		}
		cursor, first = low.Int64, false
		if limit <= 0 {
			continue
		}
		now, err := pages()
		if err != nil {
			return false, err
		}
		if (now-start)*pageSize >= limit {
			var more bool
			if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM src.`+quoted+` WHERE rowid < ?)`, cursor).Scan(&more); err != nil {
				return false, err
			}
			return more, nil
		}
	}
}

type updateSchemaQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func readUpdateSchema(ctx context.Context, q updateSchemaQuerier, schema string) (updateSchemaSnapshot, error) {
	snap := updateSchemaSnapshot{objects: map[string]updateSchemaObject{}, columns: map[string][]updateColumn{}}
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql,'') FROM `+schema+`.sqlite_master ORDER BY type, name`)
	if err != nil {
		return snap, err
	}
	var text strings.Builder
	var tables []string
	for rows.Next() {
		var o updateSchemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			rows.Close()
			return snap, err
		}
		fmt.Fprintf(&text, "%s\x00%s\x00%s\n", o.Type, o.Name, o.SQL)
		// Planner statistics are not schema the release depends on.
		if strings.HasPrefix(o.Name, "sqlite_stat") {
			continue
		}
		snap.objects[o.Type+"\x00"+o.Name] = o
		if o.Type == "table" {
			tables = append(tables, o.Name)
		}
	}
	if err := rows.Close(); err != nil {
		return snap, err
	}
	snap.hash = fmt.Sprintf("%x", sha256.Sum256([]byte(text.String())))
	for _, table := range tables {
		cols, err := q.QueryContext(ctx, `PRAGMA `+schema+`.table_xinfo(`+quoteUpdateIdent(table)+`)`)
		if err != nil {
			return snap, err
		}
		for cols.Next() {
			var cid int
			var c updateColumn
			var notNull int
			if err := cols.Scan(&cid, &c.Name, &c.Type, &notNull, &c.Default, &c.PK, &c.Hidden); err != nil {
				cols.Close()
				return snap, err
			}
			c.NotNull = notNull != 0
			snap.columns[table] = append(snap.columns[table], c)
		}
		if err := cols.Close(); err != nil {
			return snap, err
		}
	}
	return snap, nil
}

func readUpdateSchemaFile(ctx context.Context, path string) (updateSchemaSnapshot, error) {
	db, err := sql.Open("sqlite", sqliteFileURI(path)+"?mode=ro")
	if err != nil {
		return updateSchemaSnapshot{}, err
	}
	defer db.Close()
	return readUpdateSchema(ctx, db, "main")
}

// ---- change capture -----------------------------------------------------

// updateCaptureRule marks changes every release makes when it boots, which
// the previous release therefore also copes with.
type updateCaptureRule struct {
	ignore    bool            // derived data, not reported
	all       bool            // any change to this table
	ledger    bool            // a migration ledger: report recorded versions
	columns   map[string]bool // updates confined to these columns
	rowColumn string          // rows whose rowColumn equals rowValue
	rowValue  string
}

func updateColumnSet(names ...string) map[string]bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return set
}

var updateCaptureRules = map[string]updateCaptureRule{
	// Cache epoch that triggers bump on every app or MCP write, even no-ops.
	"app_metadata_generation": {ignore: true},
	// Each release reasserts its provider catalogue at boot.
	"provider_types": {all: true},
	// Boot resets of transient state.
	"preset_setup_steps": {columns: updateColumnSet("status", "error")},
	"agents":             {columns: updateColumnSet("port", "pid", "core_api_key")},
	"mcp_servers":        {columns: updateColumnSet("name", "description")},
	// Built-in starter templates are upserted by each release at boot.
	"agent_templates": {rowColumn: "source", rowValue: "builtin"},
	// ...and get a new version when a release changed their content.
	"agent_template_versions": {rowColumn: "source", rowValue: "builtin"},
	// Ledgers of one-time migrations; a release ignores versions it does not
	// know. What the migration itself changed is captured separately.
	"server_schema_migrations": {all: true, ledger: true},
	"framework_app_versions":   {all: true, ledger: true},
}

// tables whose earlier releases insert rows without naming columns, so a new
// column there would break the previous release.
var updateColumnOrderTables = map[string]bool{
	"app_metadata_generation":         true,
	"app_component_declarations":      true,
	"app_runtime_target_catalog":      true,
	"app_runtime_install_receipts":    true,
	"app_event_receipts":              true,
	"user_notification_preferences":   true,
	"user_notification_subscriptions": true,
}

type updateCapturedTable struct {
	inserted, updated, deleted int64
	unmanaged                  int64
	columns                    map[int]bool
	ledger                     []string
}

func (t *updateCapturedTable) changes() int64 {
	if t == nil {
		return 0
	}
	return t.inserted + t.updated + t.deleted
}

type resolvedCaptureRule struct {
	updateCaptureRule
	columnIndexes map[int]bool
	row           int   // index of rowColumn, or -1
	ledgerColumns []int // columns that describe a ledger row
}

type updateCapture struct {
	path    string
	rules   map[string]resolvedCaptureRule
	mu      sync.Mutex
	stopped bool
	tables  map[string]*updateCapturedTable
	// writes: tables (lowercase) an UPDATE or DELETE statement named,
	// whether or not it changed a row of the copy (update_check_sql.go).
	writes map[string]bool
}

var (
	updateCaptureOnce   sync.Once
	updateCaptureActive sync.Map // cleaned probe path → *updateCapture
)

// startUpdateCapture records committed row changes made by connections that
// open the probe. Connections to any other database are untouched.
func startUpdateCapture(probe string, before updateSchemaSnapshot) *updateCapture {
	updateCaptureOnce.Do(func() {
		sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
			path, _, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
			if unescaped, err := url.PathUnescape(path); err == nil {
				path = unescaped
			}
			value, ok := updateCaptureActive.Load(filepath.Clean(path))
			if !ok {
				return nil
			}
			hooks, ok := conn.(sqlite.HookRegisterer)
			if !ok {
				return errors.New("SQLite driver cannot record changes for the update check")
			}
			value.(*updateCapture).attach(hooks)
			return nil
		})
	})
	c := &updateCapture{path: filepath.Clean(probe), rules: map[string]resolvedCaptureRule{}, tables: map[string]*updateCapturedTable{}}
	for table, rule := range updateCaptureRules {
		cols, ok := before.columns[table]
		if !ok {
			continue
		}
		resolved := resolvedCaptureRule{updateCaptureRule: rule, row: -1}
		index := map[string]int{}
		for i, col := range cols {
			index[col.Name] = i
		}
		if rule.columns != nil {
			resolved.columnIndexes = map[int]bool{}
			for name := range rule.columns {
				if i, ok := index[name]; ok {
					resolved.columnIndexes[i] = true
				}
			}
		}
		if rule.rowColumn != "" {
			if i, ok := index[rule.rowColumn]; ok {
				resolved.row = i
			}
		}
		if rule.ledger {
			for _, name := range []string{"app_slug", "version"} {
				if i, ok := index[name]; ok {
					resolved.ledgerColumns = append(resolved.ledgerColumns, i)
				}
			}
		}
		c.rules[table] = resolved
	}
	updateCaptureActive.Store(c.path, c)
	return c
}

// written returns the tables statements wrote to; call after stop.
func (c *updateCapture) written() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.writes))
	for table := range c.writes {
		out[table] = true
	}
	return out
}

func (c *updateCapture) stop() map[string]*updateCapturedTable {
	updateCaptureActive.Delete(c.path)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true // connections still open keep their hooks; ignore them
	out := map[string]*updateCapturedTable{}
	for name, t := range c.tables {
		if t.changes() > 0 {
			out[name] = t
		}
	}
	return out
}

func (c *updateCapture) attach(hooks sqlite.HookRegisterer) {
	// Hooks of one connection run one at a time on its goroutine.
	pending := map[string]*updateCapturedTable{}
	hooks.RegisterPreUpdateHook(func(d sqlite.SQLitePreUpdateData) {
		if d.DatabaseName == "main" {
			c.record(pending, d)
		}
	})
	hooks.RegisterCommitHook(func() int32 {
		c.merge(pending)
		clear(pending)
		return 0
	})
	hooks.RegisterRollbackHook(func() { clear(pending) })
}

func (c *updateCapture) record(pending map[string]*updateCapturedTable, d sqlite.SQLitePreUpdateData) {
	rule, known := c.rules[d.TableName]
	if !known {
		// A table without a rule, or one the migrations just created.
		rule = resolvedCaptureRule{updateCaptureRule: updateCaptureRules[d.TableName], row: -1}
	}
	if rule.ignore {
		return
	}
	t := pending[d.TableName]
	if t == nil {
		t = &updateCapturedTable{columns: map[int]bool{}}
		pending[d.TableName] = t
	}
	n := d.Count()
	row := func(read func(...any) error) []any {
		values := make([]any, n)
		if read(values...) != nil {
			return nil
		}
		return values
	}
	var oldRow, newRow []any
	var changed []int
	switch d.Op {
	case sqlite3.SQLITE_INSERT:
		if rule.row >= 0 || rule.ledgerColumns != nil {
			newRow = row(d.New)
		}
		t.inserted++
		if rule.ledgerColumns != nil && newRow != nil {
			var parts []string
			for _, i := range rule.ledgerColumns {
				if i < len(newRow) {
					parts = append(parts, updateValueText(newRow[i]))
				}
			}
			t.ledger = append(t.ledger, strings.Join(parts, " "))
		}
	case sqlite3.SQLITE_DELETE:
		if rule.row >= 0 {
			oldRow = row(d.Old)
		}
		t.deleted++
	case sqlite3.SQLITE_UPDATE:
		oldRow, newRow = row(d.Old), row(d.New)
		if oldRow == nil || newRow == nil || d.OldRowID != d.NewRowID {
			t.updated++
			t.unmanaged++
			return
		}
		for i := range oldRow {
			if !sameUpdateValue(oldRow[i], newRow[i]) {
				changed = append(changed, i)
				t.columns[i] = true
			}
		}
		if len(changed) == 0 {
			return // rewritten with the same values
		}
		t.updated++
	default:
		t.unmanaged++
		return
	}
	if !rule.manages(d.Op, oldRow, newRow, changed) {
		t.unmanaged++
	}
}

func (r resolvedCaptureRule) manages(op int32, oldRow, newRow []any, changed []int) bool {
	switch {
	case r.all:
		return true
	case r.row >= 0:
		match := func(values []any) bool {
			return values != nil && r.row < len(values) && updateValueText(values[r.row]) == r.rowValue
		}
		switch op {
		case sqlite3.SQLITE_INSERT:
			return match(newRow)
		case sqlite3.SQLITE_DELETE:
			return match(oldRow)
		}
		return match(oldRow) && match(newRow)
	case r.columnIndexes != nil && op == sqlite3.SQLITE_UPDATE:
		for _, i := range changed {
			if !r.columnIndexes[i] {
				return false
			}
		}
		return len(changed) > 0
	}
	return false
}

func (c *updateCapture) merge(pending map[string]*updateCapturedTable) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	for name, p := range pending {
		t := c.tables[name]
		if t == nil {
			t = &updateCapturedTable{columns: map[int]bool{}}
			c.tables[name] = t
		}
		t.inserted += p.inserted
		t.updated += p.updated
		t.deleted += p.deleted
		t.unmanaged += p.unmanaged
		for i := range p.columns {
			t.columns[i] = true
		}
		t.ledger = append(t.ledger, p.ledger...)
	}
}

func sameUpdateValue(a, b any) bool {
	if ab, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		return ok && bytes.Equal(ab, bb)
	}
	return reflect.DeepEqual(a, b)
}

func updateValueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// ---- classification -----------------------------------------------------

func classifyUpdate(before, after updateSchemaSnapshot, rows map[string]*updateCapturedTable, sampled []string) updateCheckReport {
	r := updateCheckReport{SchemaChanged: before.hash != after.hash}
	incompatible := func(reason string) { r.Reasons = append(r.Reasons, reason) }
	newTables := map[string]bool{}
	for key, o := range after.objects {
		if _, ok := before.objects[key]; !ok && o.Type == "table" {
			newTables[o.Name] = true
		}
	}

	keys := map[string]bool{}
	for key := range before.objects {
		keys[key] = true
	}
	for key := range after.objects {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		b, inBefore := before.objects[key]
		a, inAfter := after.objects[key]
		switch {
		case inBefore && inAfter && b.SQL == a.SQL:
			continue
		case !inBefore:
			if newTables[a.Table] && a.Type == "index" && a.SQL == "" {
				continue // automatic index of a new table
			}
			r.SchemaChanges = append(r.SchemaChanges, updateSchemaChange{Change: "added", Type: a.Type, Name: a.Name})
			switch a.Type {
			case "trigger":
				incompatible("adds trigger " + a.Name)
			case "index":
				if updateUniqueIndex(a.SQL) && !newTables[a.Table] {
					incompatible("adds unique index " + a.Name + " on " + a.Table)
				}
			}
		case !inAfter:
			r.SchemaChanges = append(r.SchemaChanges, updateSchemaChange{Change: "removed", Type: b.Type, Name: b.Name})
			if b.Type != "index" {
				incompatible("removes " + b.Type + " " + b.Name)
			}
		default:
			change := updateSchemaChange{Change: "modified", Type: a.Type, Name: a.Name}
			switch a.Type {
			case "table":
				added, ok := updateAddedColumns(before.columns[a.Name], after.columns[a.Name], b.SQL, a.SQL)
				switch {
				case !ok:
					incompatible("changes the definition of table " + a.Name)
				case updateColumnOrderTables[a.Name]:
					change.Detail = "adds " + strings.Join(added, ", ")
					incompatible("adds columns to " + a.Name + ", which earlier releases write without naming columns")
				default:
					change.Detail = "adds " + strings.Join(added, ", ")
				}
			case "index":
				if updateUniqueIndex(a.SQL) {
					incompatible("changes unique index " + a.Name)
				}
			default:
				incompatible("changes " + a.Type + " " + a.Name)
			}
			r.SchemaChanges = append(r.SchemaChanges, change)
		}
	}

	sampledSet := map[string]bool{}
	for _, t := range sampled {
		sampledSet[t] = true
	}
	changedRows := false
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := rows[name]
		if t.changes() == 0 || newTables[name] {
			continue
		}
		changedRows = true
		change := updateRowChange{Inserted: t.inserted, Updated: t.updated, Deleted: t.deleted, Managed: t.unmanaged == 0}
		cols := after.columns[name]
		for i := range t.columns {
			if i < len(cols) {
				change.Columns = append(change.Columns, cols[i].Name)
			}
		}
		sort.Strings(change.Columns)
		if r.RowChanges == nil {
			r.RowChanges = map[string]updateRowChange{}
		}
		r.RowChanges[name] = change
		if t.unmanaged > 0 {
			incompatible(fmt.Sprintf("changes %d existing %s in %s", t.unmanaged, plural(int(t.unmanaged), "row", "rows"), name))
		}
		for _, entry := range t.ledger {
			label := entry
			if name == "server_schema_migrations" {
				label = "server " + entry
			}
			r.Migrations = append(r.Migrations, label)
		}
	}
	if len(r.Migrations) > 0 && len(sampled) > 0 {
		// A one-time migration may rewrite rows the reduced copy left out.
		incompatible("runs one-time migrations (" + strings.Join(r.Migrations, ", ") + ") that were checked on the newest rows of large tables only")
	}
	r.RollbackSafe = !r.SchemaChanged && !changedRows
	r.Compatible = len(r.Reasons) == 0
	switch {
	case r.RollbackSafe:
		r.Classification = "unchanged"
	case r.Compatible:
		r.Classification = "compatible"
	default:
		r.Classification = "incompatible"
	}
	return r
}

func updateUniqueIndex(definition string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(definition)), "CREATE UNIQUE ")
}

// updateAddedColumns reports whether after is before plus appended columns,
// as ALTER TABLE ADD COLUMN produces, and names the added columns.
func updateAddedColumns(before, after []updateColumn, beforeSQL, afterSQL string) ([]string, bool) {
	if len(after) <= len(before) || !reflect.DeepEqual(before, after[:len(before)]) {
		return nil, false
	}
	// ADD COLUMN inserts ", <definition>" after the last column definition,
	// ahead of any table constraints. Anything else changed the table.
	if !updateSQLInsertion(beforeSQL, afterSQL) {
		return nil, false
	}
	var added []string
	for _, c := range after[len(before):] {
		if c.Hidden != 0 || (c.NotNull && !c.Default.Valid) || c.PK != 0 {
			return nil, false
		}
		added = append(added, c.Name)
	}
	return added, true
}

// updateSQLInsertion reports whether after is before with one contiguous
// piece of text inserted.
func updateSQLInsertion(before, after string) bool {
	if len(after) <= len(before) {
		return false
	}
	p := 0
	for p < len(before) && before[p] == after[p] {
		p++
	}
	q := 0
	for q < len(before)-p && before[len(before)-1-q] == after[len(after)-1-q] {
		q++
	}
	return p+q == len(before)
}

// ---- progress -----------------------------------------------------------

type updateProgressState struct {
	Phase     string    `json:"phase"`
	Message   string    `json:"message"`
	Done      int64     `json:"done"`
	Total     int64     `json:"total"`
	UpdatedAt time.Time `json:"updated_at"`
}

// updateProgress keeps a small JSON file current for the caller: on every
// phase change, at most twice a second within a phase, and every few seconds
// as a heartbeat.
type updateProgress struct {
	path    string
	mu      sync.Mutex
	state   updateProgressState
	written time.Time
	stop    chan struct{}
	once    sync.Once
}

func newUpdateProgress(path string) *updateProgress {
	p := &updateProgress{path: path, stop: make(chan struct{})}
	if path == "" {
		return p
	}
	p.set("starting", "Starting the database check", 0, 0)
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.writeLocked()
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *updateProgress) set(phase, message string, done, total int64) {
	if p == nil || p.path == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := phase != p.state.Phase
	p.state.Phase, p.state.Message, p.state.Done, p.state.Total = phase, message, done, total
	if changed || time.Since(p.written) >= 500*time.Millisecond {
		p.writeLocked()
	}
}

func (p *updateProgress) writeLocked() {
	p.state.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(p.state)
	if err == nil && writeUpdateFileAtomic(p.path, raw) == nil {
		p.written = time.Now()
	}
}

func (p *updateProgress) close() {
	if p == nil || p.path == "" {
		return
	}
	p.once.Do(func() {
		close(p.stop)
		p.mu.Lock()
		p.writeLocked()
		p.mu.Unlock()
	})
}

// ---- helpers ------------------------------------------------------------

func writeUpdateFileAtomic(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".update-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(raw); err == nil {
			err = f.Sync()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func quoteUpdateIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func formatUpdateBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

// updateDatabaseSchema hashes every sqlite_master row; apteva-host compares it
// to confirm the live schema is the one a check ran against.
func updateDatabaseSchema(path string) (string, error) {
	db, err := sql.Open("sqlite", sqliteFileURI(path)+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT type,name,COALESCE(sql,'') FROM sqlite_master ORDER BY type,name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var schema strings.Builder
	for rows.Next() {
		var kind, name, definition string
		if err := rows.Scan(&kind, &name, &definition); err != nil {
			return "", err
		}
		fmt.Fprintf(&schema, "%s\x00%s\x00%s\n", kind, name, definition)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(schema.String()))), nil
}

func sqliteFileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }
