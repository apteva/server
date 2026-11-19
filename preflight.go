package main

// preflight.go — `apteva-server --preflight`.
//
// Run by `apteva update` against the freshly-extracted binary BEFORE
// flipping `bin/current`. The job is to catch a "new build won't
// boot at all" failure cheaply: we won't promote a broken binary
// past the symlink. Failures here are the operator's signal to keep
// the prior version active.
//
// Checks, in this order:
//   0. When the caller asks for it, the database update check
//      (update_check.go): this release's migrations run on a reduced
//      copy of the live database, never on the live database itself.
//   1. APTEVA_HOME exists and is writable.
//   2. The DB at $DB_PATH (or APTEVA_HOME/apteva.db) opens
//      read-only — exercises the schema decoder against on-disk
//      bytes WITHOUT applying migrations. A schema-incompatible
//      build wedges here.
//   3. Bind an ephemeral high port; this proves the binary can
//      reach the network stack and isn't blocked by AppArmor/
//      SELinux/seccomp filters added since last upgrade.
//
// Callers request the database check through the environment:
//
//   APTEVA_UPDATE_CHECK_DIR    private work directory; report.json and
//                              progress.json are written there
//   APTEVA_UPDATE_BACKUP       optional backup destination
//   APTEVA_UPDATE_BACKUP_WHEN  never | incompatible (default) | changed
//   APTEVA_UPDATE_CHECK_FULL   1: copy every table in full
//
// Earlier callers set APTEVA_UPDATE_PREFLIGHT=<backup path> instead; they
// get the report at <path>.json and a backup whenever the release changes
// anything, which is when they would point an operator at it.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

func runPreflight() int {
	if opts, ok, err := updateCheckOptionsFromEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "database update check: %v\n", err)
		return 1
	} else if ok {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		go func() {
			// Migrations on the copy cannot be interrupted midway. After a
			// grace period, remove the copy and any partial backup and exit.
			<-ctx.Done()
			time.Sleep(10 * time.Second)
			removeSQLiteFiles(filepath.Join(opts.Dir, "probe.db"))
			if opts.Backup != "" {
				if _, err := os.Stat(opts.Report); os.IsNotExist(err) {
					removeSQLiteFiles(opts.Backup)
				}
			}
			os.Exit(1)
		}()
		err := runUpdateCheck(ctx, opts)
		if opts.Dir != "" && opts.Progress == "" {
			_ = os.RemoveAll(opts.Dir) // the earlier contract's private directory
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "database update check: %v\n", err)
			return 1
		}
	}
	home := os.Getenv("APTEVA_HOME")
	if home == "" {
		// Fall back to the same default the CLI uses.
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".apteva")
		}
	}
	if home != "" {
		if err := os.MkdirAll(home, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: home %s not writable: %v\n", home, err)
			return 1
		}
	}

	// DB sanity-check. We run a single PRAGMA against the existing
	// DB; opening + reading the schema header is enough to catch
	// "new build expects a schema that doesn't exist on this DB"
	// without applying any migrations. A missing DB is OK — fresh
	// installs have no DB yet, and the boot path will create one.
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" && home != "" {
		dbPath = filepath.Join(home, "apteva.db")
	}
	if dbPath != "" {
		if _, err := os.Stat(dbPath); err == nil {
			db, err := sql.Open("sqlite", dbPath+"?mode=ro")
			if err != nil {
				fmt.Fprintf(os.Stderr, "preflight: open db: %v\n", err)
				return 1
			}
			defer db.Close()
			row := db.QueryRow("PRAGMA schema_version")
			var sv int
			if err := row.Scan(&sv); err != nil {
				fmt.Fprintf(os.Stderr, "preflight: db schema_version: %v\n", err)
				return 1
			}
		}
	}

	// Network sanity-check: bind an ephemeral high port. We don't
	// touch the configured PORT — bumping that with a daemon
	// already on it would fail for an unrelated reason and mask a
	// real preflight failure.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: bind ephemeral port: %v\n", err)
		return 1
	}
	_ = l.Close()

	fmt.Fprintln(os.Stderr, "preflight: ok")
	return 0
}

// updateCheckOptionsFromEnv reads the caller's request for the database
// update check. ok is false when none was made.
func updateCheckOptionsFromEnv() (opts updateCheckOptions, ok bool, err error) {
	opts = updateCheckOptions{Source: os.Getenv("DB_PATH"), Budget: updateCheckTableBudget}
	if os.Getenv("APTEVA_UPDATE_CHECK_FULL") == "1" {
		opts.Budget = 0
	}
	if dir := os.Getenv("APTEVA_UPDATE_CHECK_DIR"); dir != "" {
		opts.Dir = dir
		opts.Report = filepath.Join(dir, "report.json")
		opts.Progress = filepath.Join(dir, "progress.json")
		opts.Backup = os.Getenv("APTEVA_UPDATE_BACKUP")
		opts.BackupWhen = os.Getenv("APTEVA_UPDATE_BACKUP_WHEN")
		if opts.BackupWhen == "" {
			opts.BackupWhen = "never"
			if opts.Backup != "" {
				opts.BackupWhen = "incompatible"
			}
		}
		return opts, true, nil
	}
	if backup := os.Getenv("APTEVA_UPDATE_PREFLIGHT"); backup != "" {
		if !filepath.IsAbs(backup) {
			return opts, false, fmt.Errorf("absolute backup path required")
		}
		opts.Dir = backup + ".check"
		opts.Report = backup + ".json"
		opts.Backup = backup
		opts.BackupWhen = "changed"
		return opts, true, nil
	}
	return opts, false, nil
}
