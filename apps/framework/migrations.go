package framework

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// SQLite's busy timeout is connection-local and does not reliably serialize
// simultaneous schema setup across every supported driver version. Migration
// work is rare and startup-bound, so serialize runners inside the process;
// BEGIN IMMEDIATE remains the cross-process safety boundary.
var migrationRunnerMu sync.Mutex

// MigrationTx is a transaction pinned to one SQLite connection. Migrations
// begin with BEGIN IMMEDIATE so schema inspection followed by DDL cannot lose
// a lock-upgrade race to other server startup writers.
type MigrationTx struct {
	ctx  context.Context
	conn *sql.Conn
}

func (tx *MigrationTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(tx.ctx, query, args...)
}

func (tx *MigrationTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(tx.ctx, query, args...)
}

func (tx *MigrationTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(tx.ctx, query, args...)
}

// RunMigrations applies every migration for the app in version order,
// skipping any that are already applied. Idempotent.
//
// Tracks applied versions in the shared framework_app_versions table
// (created on first use). Apps declare versions starting at 1 and
// incrementing; the runner refuses to go backwards and skips anything
// ≤ the highest applied version.
func RunMigrations(db *sql.DB, slug string, migs []Migration) error {
	migrationRunnerMu.Lock()
	defer migrationRunnerMu.Unlock()

	if err := ensureVersionsTable(db); err != nil {
		return err
	}
	applied, err := highestApplied(db, slug)
	if err != nil {
		return err
	}
	// Enforce monotonicity on the supplied slice.
	for i := 1; i < len(migs); i++ {
		if migs[i].Version <= migs[i-1].Version {
			return fmt.Errorf("%s: migrations not strictly increasing at index %d (%d then %d)",
				slug, i, migs[i-1].Version, migs[i].Version)
		}
	}
	for _, m := range migs {
		if m.Version <= applied {
			continue
		}
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("%s acquire connection for v%d: %w", slug, m.Version, err)
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			_ = conn.Close()
			return fmt.Errorf("%s begin v%d: %w", slug, m.Version, err)
		}
		tx := &MigrationTx{ctx: ctx, conn: conn}
		var current sql.NullInt64
		if err := tx.QueryRow(
			`SELECT MAX(version) FROM framework_app_versions WHERE app_slug = ?`, slug,
		).Scan(&current); err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			_ = conn.Close()
			return fmt.Errorf("%s recheck v%d: %w", slug, m.Version, err)
		}
		if current.Valid && int(current.Int64) >= m.Version {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			_ = conn.Close()
			applied = int(current.Int64)
			continue
		}
		if m.Apply != nil {
			err = m.Apply(tx)
		} else {
			_, err = tx.Exec(m.SQL)
		}
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			_ = conn.Close()
			return fmt.Errorf("%s migration v%d (%s): %w", slug, m.Version, m.Name, err)
		}
		_, err = tx.Exec(
			`INSERT INTO framework_app_versions (app_slug, version, name) VALUES (?, ?, ?)`,
			slug, m.Version, m.Name,
		)
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			_ = conn.Close()
			return fmt.Errorf("%s record v%d: %w", slug, m.Version, err)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			_ = conn.Close()
			return fmt.Errorf("%s commit v%d: %w", slug, m.Version, err)
		}
		if err := conn.Close(); err != nil {
			return fmt.Errorf("%s close migration connection v%d: %w", slug, m.Version, err)
		}
		applied = m.Version
	}
	return nil
}

func ensureVersionsTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS framework_app_versions (
			app_slug   TEXT    NOT NULL,
			version    INTEGER NOT NULL,
			name       TEXT    NOT NULL,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (app_slug, version)
		)
	`)
	return err
}

func highestApplied(db *sql.DB, slug string) (int, error) {
	var v sql.NullInt64
	err := db.QueryRow(
		`SELECT MAX(version) FROM framework_app_versions WHERE app_slug = ?`,
		slug,
	).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
