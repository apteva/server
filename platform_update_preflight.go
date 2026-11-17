package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot the live database consistently, then run the candidate's real
// migrations on a disposable copy. The original backup remains untouched.
func preflightUpdateDatabase(source, backup string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(backup) || source == backup {
		return fmt.Errorf("absolute, distinct database paths required")
	}
	if _, err := os.Stat(source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		return fmt.Errorf("backup already exists or cannot be inspected")
	}
	if err := vacuumIntoFromPath(source, backup); err != nil {
		return err
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return err
	}
	clone := backup + ".preflight"
	in, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(clone, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(clone + suffix)
		}
	}()
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	before, err := updateDatabaseSchema(backup)
	if err != nil {
		return err
	}
	beforeData, err := updateDatabaseFingerprint(backup)
	if err != nil {
		return err
	}
	store, err := NewStore(clone)
	if err != nil {
		return fmt.Errorf("candidate migrations: %w", err)
	}
	if err := store.Close(); err != nil {
		return err
	}
	after, err := updateDatabaseSchema(clone)
	if err != nil {
		return err
	}
	afterData, err := updateDatabaseFingerprint(clone)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"version": CLIVersion, "schema_changed": before != after, "rollback_safe": before == after && beforeData == afterData})
	if err != nil {
		return err
	}
	return os.WriteFile(backup+".json", raw, 0600)
}

func updateDatabaseSchema(path string) (string, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
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
	return schema.String(), rows.Err()
}

// Schema equality alone does not prove rollback safety: migrations may rewrite
// data. Compare a deterministic stream of all stored rows too. Fail closed on
// unsupported values; never load the whole database into memory.
func updateDatabaseFingerprint(path string) (string, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, name := range tables {
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		columns, err := db.Query("SELECT * FROM " + quoted + " LIMIT 0")
		if err != nil {
			return "", err
		}
		names, err := columns.Columns()
		columns.Close()
		if err != nil {
			return "", err
		}
		if err := encoder.Encode([]any{name, names}); err != nil {
			return "", err
		}
		order := make([]string, len(names))
		for i := range names {
			order[i] = fmt.Sprint(i + 1)
		}
		rows, err := db.Query("SELECT * FROM " + quoted + " ORDER BY " + strings.Join(order, ","))
		if err != nil {
			return "", err
		}
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
			if err := encoder.Encode(values); err != nil {
				rows.Close()
				return "", err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", err
		}
		rows.Close()
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
