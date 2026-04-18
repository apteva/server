package framework

import (
	"database/sql"
	"fmt"
)

// RunMigrations applies every migration for the app in version order,
// skipping any that are already applied. Idempotent.
//
// Tracks applied versions in the shared framework_app_versions table
// (created on first use). Apps declare versions starting at 1 and
// incrementing; the runner refuses to go backwards and skips anything
// ≤ the highest applied version.
func RunMigrations(db *sql.DB, slug string, migs []Migration) error {
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
		if _, err := db.Exec(m.SQL); err != nil {
			return fmt.Errorf("%s migration v%d (%s): %w", slug, m.Version, m.Name, err)
		}
		_, err := db.Exec(
			`INSERT INTO framework_app_versions (app_slug, version, name) VALUES (?, ?, ?)`,
			slug, m.Version, m.Name,
		)
		if err != nil {
			return fmt.Errorf("%s record v%d: %w", slug, m.Version, err)
		}
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
