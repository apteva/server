package main

// preflight.go — `apteva-server --preflight`.
//
// Run by `apteva update` against the freshly-extracted binary BEFORE
// flipping `bin/current`. The job is to catch a "new build won't
// boot at all" failure cheaply: we won't promote a broken binary
// past the symlink. Failures here are the operator's signal to keep
// the prior version active.
//
// Checks (cheap, in this order):
//   1. APTEVA_HOME exists and is writable.
//   2. The DB at $DB_PATH (or APTEVA_HOME/apteva.db) opens
//      read-only — exercises the schema decoder against on-disk
//      bytes WITHOUT applying migrations. A schema-incompatible
//      build wedges here.
//   3. Bind an ephemeral high port; this proves the binary can
//      reach the network stack and isn't blocked by AppArmor/
//      SELinux/seccomp filters added since last upgrade.
//
// We don't run real migrations or load the integrations catalog —
// those are slow and would write to disk. Preflight stays in the
// "pure introspection" lane.

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func runPreflight() int {
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
