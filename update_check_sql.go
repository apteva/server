package main

// update_check_sql.go — the statements a release runs on the update check's
// copy.
//
// The pre-update hook (update_check.go) records rows a migration changes in
// the copy. A table the copy holds only part of can hide a migration whose
// UPDATE or DELETE matches only rows that were left out: the copy shows no
// change, and the check would call the release unchanged. Recording each
// statement's text as well lets the check see that the migration writes to
// the table at all and copy that table in full.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
)

const updateCheckDriverName = "sqlite-update-check"

var updateCheckDriverOnce sync.Once

// updateCheckDriver returns the driver name openStore uses for path: the
// recording driver while an update check runs migrations on that copy, the
// plain SQLite driver otherwise.
func updateCheckDriver(path string) string {
	if _, ok := updateCaptureActive.Load(filepath.Clean(path)); !ok {
		return "sqlite"
	}
	updateCheckDriverOnce.Do(func() {
		// The registered driver carries the connection hooks the pre-update
		// capture attaches through; a zero sqlite.Driver would not.
		db, err := sql.Open("sqlite", "")
		if err != nil {
			panic(err)
		}
		base := db.Driver()
		_ = db.Close()
		sql.Register(updateCheckDriverName, recordingSQLDriver{base: base})
	})
	return updateCheckDriverName
}

type recordingSQLDriver struct{ base driver.Driver }

// updateCheckSQLConn is the driver interface set database/sql uses on a
// modernc connection.
type updateCheckSQLConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

func (d recordingSQLDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.base.Open(dsn)
	if err != nil {
		return nil, err
	}
	full, ok := c.(updateCheckSQLConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("SQLite driver cannot record statements for the update check")
	}
	path, _, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
	if unescaped, err := url.PathUnescape(path); err == nil {
		path = unescaped
	}
	record := func(string) {}
	if value, ok := updateCaptureActive.Load(filepath.Clean(path)); ok {
		record = value.(*updateCapture).statement
	}
	return recordingSQLConn{updateCheckSQLConn: full, record: record}, nil
}

type recordingSQLConn struct {
	updateCheckSQLConn
	record func(string)
}

func (c recordingSQLConn) Prepare(query string) (driver.Stmt, error) {
	c.record(query)
	return c.updateCheckSQLConn.Prepare(query)
}

func (c recordingSQLConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.record(query)
	return c.updateCheckSQLConn.PrepareContext(ctx, query)
}

func (c recordingSQLConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.record(query)
	return c.updateCheckSQLConn.ExecContext(ctx, query, args)
}

func (c recordingSQLConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.record(query)
	return c.updateCheckSQLConn.QueryContext(ctx, query, args)
}

// statement records the tables query updates or deletes from.
func (c *updateCapture) statement(query string) {
	targets := updateWriteTargets(query)
	if len(targets) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if c.writes == nil {
		c.writes = map[string]bool{}
	}
	for table := range targets {
		c.writes[table] = true
	}
}

// updateWriteTargets returns the tables query's UPDATE and DELETE statements
// write to. It reads SQL text only: comments and string literals are skipped,
// identifiers are unquoted and lowercased, and a schema prefix is dropped.
// UPDATE and DELETE that follow ON (foreign key actions) or DO (upsert), and
// trigger events (UPDATE OF c ON t), do not name a written table and are
// ignored.
func updateWriteTargets(query string) map[string]bool {
	tokens := updateSQLTokens(query)
	targets := map[string]bool{}
	for i := 0; i < len(tokens); i++ {
		word := strings.ToUpper(tokens[i].text)
		if tokens[i].quoted || (word != "UPDATE" && word != "DELETE") {
			continue
		}
		if i > 0 && !tokens[i-1].quoted {
			if prev := strings.ToUpper(tokens[i-1].text); prev == "ON" || prev == "DO" || prev == "OF" {
				continue
			}
		}
		j := i + 1
		if j < len(tokens) && !tokens[j].quoted {
			// Trigger events: UPDATE OF col ON t, DELETE ON t.
			if next := strings.ToUpper(tokens[j].text); next == "ON" || next == "OF" {
				continue
			}
		}
		if word == "UPDATE" && j+1 < len(tokens) && !tokens[j].quoted && strings.EqualFold(tokens[j].text, "OR") {
			j += 2 // UPDATE OR REPLACE|IGNORE|...
		}
		if word == "DELETE" {
			if j >= len(tokens) || tokens[j].quoted || !strings.EqualFold(tokens[j].text, "FROM") {
				continue
			}
			j++
		}
		if j >= len(tokens) {
			continue
		}
		name := tokens[j]
		if j+2 < len(tokens) && tokens[j+1].text == "." && !tokens[j+1].quoted {
			name = tokens[j+2] // schema.table
		}
		if name.text == "" || (!name.quoted && !updateSQLIdentStart(name.text[0])) {
			continue
		}
		targets[strings.ToLower(name.text)] = true
	}
	return targets
}

type updateSQLToken struct {
	text   string
	quoted bool // a quoted identifier; its text is unquoted
}

func updateSQLIdentStart(b byte) bool {
	return b == '_' || (b|0x20 >= 'a' && b|0x20 <= 'z')
}

func updateSQLTokens(query string) []updateSQLToken {
	var tokens []updateSQLToken
	for i := 0; i < len(query); {
		b := query[i]
		switch {
		case b == '-' && i+1 < len(query) && query[i+1] == '-':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case b == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return tokens
			}
			i += end + 4
		case b == '\'':
			// String literal: skipped; '' escapes a quote.
			i++
			for i < len(query) {
				if query[i] == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case b == '"' || b == '`' || b == '[':
			closer := b
			if b == '[' {
				closer = ']'
			}
			var sb strings.Builder
			i++
			for i < len(query) {
				if query[i] == closer {
					if closer != ']' && i+1 < len(query) && query[i+1] == closer {
						sb.WriteByte(closer)
						i += 2
						continue
					}
					break
				}
				sb.WriteByte(query[i])
				i++
			}
			i++
			tokens = append(tokens, updateSQLToken{text: sb.String(), quoted: true})
		case updateSQLIdentStart(b):
			start := i
			for i < len(query) && (updateSQLIdentStart(query[i]) || (query[i] >= '0' && query[i] <= '9') || query[i] == '$') {
				i++
			}
			tokens = append(tokens, updateSQLToken{text: query[start:i]})
		case b == '.':
			tokens = append(tokens, updateSQLToken{text: "."})
			i++
		default:
			i++
		}
	}
	return tokens
}
