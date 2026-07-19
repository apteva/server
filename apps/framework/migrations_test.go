package framework

import (
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestRunMigrationsRollsBackSchemaAndLedgerTogether(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/migrations.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = RunMigrations(db, "atomic-app", []Migration{{
		Version: 1,
		Name:    "partial failure",
		SQL: `
			CREATE TABLE should_roll_back (id INTEGER PRIMARY KEY);
			INSERT INTO table_that_does_not_exist (id) VALUES (1);
		`,
	}})
	if err == nil {
		t.Fatal("expected migration failure")
	}

	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='should_roll_back'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("partially applied table count=%d, want 0", tables)
	}
	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM framework_app_versions WHERE app_slug='atomic-app'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("recorded versions=%d, want 0", versions)
	}
}

func TestRunMigrationsCustomApplyIsAtomicAndIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/custom.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	applyCalls := 0
	migrations := []Migration{{
		Version: 1,
		Name:    "custom apply",
		Apply: func(tx *MigrationTx) error {
			applyCalls++
			_, err := tx.Exec(`CREATE TABLE custom_apply (id INTEGER PRIMARY KEY)`)
			return err
		},
	}}
	if err := RunMigrations(db, "custom-app", migrations); err != nil {
		t.Fatal(err)
	}
	if err := RunMigrations(db, "custom-app", migrations); err != nil {
		t.Fatal(err)
	}
	if applyCalls != 1 {
		t.Fatalf("custom apply calls=%d, want 1", applyCalls)
	}

	failing := []Migration{{
		Version: 1,
		Name:    "custom failure",
		Apply: func(tx *MigrationTx) error {
			if _, err := tx.Exec(`CREATE TABLE custom_should_roll_back (id INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			return errors.New("stop")
		},
	}}
	if err := RunMigrations(db, "failing-custom-app", failing); err == nil {
		t.Fatal("expected custom migration failure")
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='custom_should_roll_back'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("custom migration left %d partial tables", tables)
	}
}

func TestRunMigrationsConcurrentRunnersApplyVersionOnce(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/concurrent.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	var applyCalls atomic.Int64
	migrations := []Migration{{
		Version: 1,
		Name:    "serialized apply",
		Apply: func(tx *MigrationTx) error {
			applyCalls.Add(1)
			time.Sleep(50 * time.Millisecond)
			_, err := tx.Exec(`CREATE TABLE concurrent_apply (id INTEGER PRIMARY KEY)`)
			return err
		},
	}}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- RunMigrations(db, "concurrent-app", migrations)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	if got := applyCalls.Load(); got != 1 {
		t.Fatalf("apply calls=%d, want 1", got)
	}
}
