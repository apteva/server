package sshwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const storeFileName = "events.db"

const schema = `
CREATE TABLE IF NOT EXISTS events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	observed_at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	result TEXT NOT NULL DEFAULT '',
	source_ip TEXT NOT NULL DEFAULT '',
	alert_rules TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_source_time ON events(source_ip, observed_at);
CREATE INDEX IF NOT EXISTS events_time ON events(observed_at);
CREATE TABLE IF NOT EXISTS cursors (
	source TEXT PRIMARY KEY,
	cursor TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS alert_outbox (
	key TEXT PRIMARY KEY,
	event_seq INTEGER NOT NULL,
	payload TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	delivered_at INTEGER,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alert_outbox_pending ON alert_outbox(delivered_at, next_attempt_at);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS authorized_keys (
	path TEXT PRIMARY KEY,
	digest TEXT NOT NULL,
	generation INTEGER NOT NULL,
	keys TEXT NOT NULL
);
`

// Store is the durable SQLite (WAL) event store shared by every process that
// uses the same collector directory. Only the lock holder writes events.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens the store. create=false refuses to create a missing
// database, so a reader never creates files the collector user cannot write.
func OpenStore(path string, create bool) (*Store, error) {
	mode := "rw"
	if create {
		mode = "rwc"
	}
	u := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode + "&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, path: path}
	if create {
		if _, err = db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("security store schema: %w", err)
		}
	} else if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Tx is one atomic store update: events, outbox rows, cursors and metadata
// written through it commit or roll back together.
type Tx struct {
	tx *sql.Tx
}

func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(&Tx{tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// InsertEvent stores ev unless an event with the same ID exists. For a new
// event it evaluates the rules (inside this transaction, so burst counting
// sees earlier events of the same batch), stores the alerts with the event
// and enqueues one outbox row per alert.
func (t *Tx) InsertEvent(ev *Event, policy Policy) (bool, error) {
	if ev.ID == "" {
		return false, errors.New("event without id")
	}
	var one int
	switch err := t.tx.QueryRow(`SELECT 1 FROM events WHERE id = ?`, ev.ID).Scan(&one); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	alerts, err := Evaluate(*ev, policy, t)
	if err != nil {
		return false, err
	}
	ev.Alerts = alerts
	rules := ""
	if len(alerts) > 0 {
		names := make([]string, len(alerts))
		for i, a := range alerts {
			names[i] = a.Rule
		}
		rules = "," + strings.Join(names, ",") + ","
	}
	ev.Seq = 0
	data, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`INSERT INTO events(id, observed_at, kind, result, source_ip, alert_rules, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.ObservedAt.UnixNano(), ev.Kind, ev.Result, ev.SourceIP, rules, string(data))
	if err != nil {
		return false, err
	}
	if ev.Seq, err = res.LastInsertId(); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, a := range alerts {
		key := ev.ID + ":" + a.Rule
		payload, err := json.Marshal(AlertDelivery{IdempotencyKey: key, CreatedAt: now, MachineID: ev.MachineID, Hostname: ev.Hostname, Alert: a, Event: *ev})
		if err != nil {
			return false, err
		}
		if _, err = t.tx.Exec(`INSERT OR IGNORE INTO alert_outbox(key, event_seq, payload, created_at, next_attempt_at) VALUES (?, ?, ?, ?, ?)`,
			key, ev.Seq, string(payload), now.UnixNano(), now.UnixNano()); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (t *Tx) SetCursor(source, cursor string) error {
	_, err := t.tx.Exec(`INSERT INTO cursors(source, cursor, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`, source, cursor, time.Now().UnixNano())
	return err
}

func (t *Tx) cursorUpdatedAt(source string) (time.Time, error) {
	var ns int64
	err := t.tx.QueryRow(`SELECT updated_at FROM cursors WHERE source = ?`, source).Scan(&ns)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return time.Unix(0, ns).UTC(), nil
}

func (t *Tx) SetMeta(key, value string) error {
	_, err := t.tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (t *Tx) Meta(key string) (string, error) {
	var v string
	err := t.tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// FailuresSince implements History.
func (t *Tx) FailuresSince(ip string, since time.Time) (int, error) {
	var n int
	err := t.tx.QueryRow(`SELECT COUNT(*) FROM events WHERE source_ip = ? AND observed_at >= ? AND kind = ? AND result IN (?, ?)`,
		ip, since.UnixNano(), KindSSHAuth, ResultFailed, ResultInvalidUser).Scan(&n)
	return n, err
}

// BurstAlertedSince implements History.
func (t *Tx) BurstAlertedSince(ip string, since time.Time) (bool, error) {
	var one int
	err := t.tx.QueryRow(`SELECT 1 FROM events WHERE source_ip = ? AND observed_at >= ? AND alert_rules LIKE ? LIMIT 1`,
		ip, since.UnixNano(), "%,"+RuleFailureBurst+",%").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type keysSnapshot struct {
	Digest     string
	Generation int64
	Keys       []AuthorizedKey
}

func (t *Tx) authorizedKeys(path string) (keysSnapshot, bool, error) {
	var snap keysSnapshot
	var raw string
	err := t.tx.QueryRow(`SELECT digest, generation, keys FROM authorized_keys WHERE path = ?`, path).Scan(&snap.Digest, &snap.Generation, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, false, nil
	}
	if err != nil {
		return snap, false, err
	}
	err = json.Unmarshal([]byte(raw), &snap.Keys)
	return snap, true, err
}

func (t *Tx) setAuthorizedKeys(path string, snap keysSnapshot) error {
	keys, err := json.Marshal(snap.Keys)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT INTO authorized_keys(path, digest, generation, keys) VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET digest = excluded.digest, generation = excluded.generation, keys = excluded.keys`,
		path, snap.Digest, snap.Generation, string(keys))
	return err
}

func (s *Store) authorizedKeysPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM authorized_keys WHERE keys != '[]' AND keys != 'null' ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Cursor returns the stored position for a source ("" when none).
func (s *Store) Cursor(ctx context.Context, source string) (string, error) {
	var c string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM cursors WHERE source = ?`, source).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, values map[string]string) error {
	return s.Update(ctx, func(tx *Tx) error {
		for k, v := range values {
			if err := tx.SetMeta(k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Events returns up to limit events with Seq > after, in Seq order.
func (s *Store) Events(ctx context.Context, after int64, limit int) (EventsPage, error) {
	page := EventsPage{Events: []Event{}, NextAfter: after}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			return page, err
		}
		var ev Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return page, err
		}
		ev.Seq = seq
		page.Events = append(page.Events, ev)
		page.NextAfter = seq
	}
	return page, rows.Err()
}

type storeStats struct {
	count, lastSeq, backlog, gaps int64
	lastEvent, lastGap            time.Time
}

func (s *Store) stats(ctx context.Context) (storeStats, error) {
	var st storeStats
	var lastObserved, lastGap sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(seq), 0), MAX(observed_at) FROM events`).Scan(&st.count, &st.lastSeq, &lastObserved); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(observed_at) FROM events WHERE kind = ?`, KindCollectorGap).Scan(&st.gaps, &lastGap); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_outbox WHERE delivered_at IS NULL`).Scan(&st.backlog); err != nil {
		return st, err
	}
	if lastObserved.Valid {
		st.lastEvent = time.Unix(0, lastObserved.Int64).UTC()
	}
	if lastGap.Valid {
		st.lastGap = time.Unix(0, lastGap.Int64).UTC()
	}
	return st, nil
}

type outboxRow struct {
	Key      string
	Payload  []byte
	Attempts int
}

func (s *Store) dueAlerts(ctx context.Context, now time.Time, limit int) ([]outboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, payload, attempts FROM alert_outbox
		WHERE delivered_at IS NULL AND next_attempt_at <= ? ORDER BY event_seq, key LIMIT ?`, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload string
		if err := rows.Scan(&r.Key, &payload, &r.Attempts); err != nil {
			return nil, err
		}
		r.Payload = []byte(payload)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) markDelivered(ctx context.Context, key string, now time.Time) error {
	return s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.tx.Exec(`UPDATE alert_outbox SET delivered_at = ?, attempts = attempts + 1, last_error = '' WHERE key = ? AND delivered_at IS NULL`, now.UnixNano(), key); err != nil {
			return err
		}
		if err := tx.SetMeta(metaAlertLastDelivered, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return tx.SetMeta(metaAlertLastError, "")
	})
}

func (s *Store) markFailed(ctx context.Context, key string, next time.Time, msg string) error {
	return s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.tx.Exec(`UPDATE alert_outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE key = ? AND delivered_at IS NULL`, next.UnixNano(), msg, key); err != nil {
			return err
		}
		return tx.SetMeta(metaAlertLastError, msg)
	})
}

// Prune removes events older than maxAge and the oldest events beyond maxRows,
// plus outbox rows of removed events. It deletes in bounded chunks.
func (s *Store) Prune(ctx context.Context, now time.Time, maxAge time.Duration, maxRows int) (int64, error) {
	var total int64
	chunk := func(query string, args ...any) error {
		for {
			res, err := s.db.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 5000 {
				return nil
			}
		}
	}
	if maxAge > 0 {
		cutoff := now.Add(-maxAge).UnixNano()
		if err := chunk(`DELETE FROM events WHERE seq IN (SELECT seq FROM events WHERE observed_at < ? ORDER BY seq LIMIT 5000)`, cutoff); err != nil {
			return total, err
		}
	}
	if maxRows > 0 {
		var boundary sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1 OFFSET ?`, maxRows).Scan(&boundary)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return total, err
		}
		if boundary.Valid {
			if err := chunk(`DELETE FROM events WHERE seq IN (SELECT seq FROM events WHERE seq <= ? ORDER BY seq LIMIT 5000)`, boundary.Int64); err != nil {
				return total, err
			}
		}
	}
	if total > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_outbox WHERE NOT EXISTS (SELECT 1 FROM events WHERE events.seq = alert_outbox.event_seq)`); err != nil {
			return total, err
		}
	}
	// Delivered rows are only bookkeeping once the event exists (event
	// de-duplication already prevents re-enqueueing).
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_outbox WHERE delivered_at IS NOT NULL AND delivered_at < ?`, now.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return total, err
	}
	return total, nil
}

const (
	metaHeartbeat          = "heartbeat_at"
	metaStartedAt          = "started_at"
	metaStoppedAt          = "stopped_at"
	metaCollectorPID       = "collector_pid"
	metaSource             = "source"
	metaSourceError        = "source_error"
	metaMachineID          = "machine_id"
	metaHostname           = "hostname"
	metaPolicyDigest       = "policy_digest"
	metaAlertLastError     = "alert_last_error"
	metaAlertLastDelivered = "alert_last_delivered_at"
	metaKeysBaseline       = "authorized_keys_baseline"
	metaKeysError          = "authorized_keys_error"
)

func parseMetaTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func parseMetaInt(v string) int {
	n, _ := strconv.Atoi(v)
	return n
}
