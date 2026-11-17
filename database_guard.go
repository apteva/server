package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

const databaseMetadataTimeout = 5 * time.Second
const databaseReadinessTimeout = 500 * time.Millisecond

var errDatabaseUnavailable = errors.New("database unavailable")

type databaseOperationStats struct{ Count, Errors, Timeouts, DurationNS, MaxNS atomic.Uint64 }
type databaseGuard struct {
	ctx        context.Context
	cancel     context.CancelFunc
	timeout    time.Duration
	operations [4]databaseOperationStats // query, row, exec, transaction
}

func (s *Store) guard() *databaseGuard {
	s.databaseOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.databaseGuard = &databaseGuard{ctx: ctx, cancel: cancel, timeout: databaseMetadataTimeout}
	})
	return s.databaseGuard
}
func (s *Store) databaseContext(parent context.Context) (context.Context, context.CancelFunc) {
	g := s.guard()
	if parent == nil {
		return context.WithTimeout(g.ctx, g.timeout)
	}
	ctx, cancel := context.WithTimeout(parent, g.timeout)
	stop := context.AfterFunc(g.ctx, cancel)
	stopCleanup := context.AfterFunc(ctx, func() { stop() })
	if g.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stopCleanup(); stop(); cancel() }
}
func (s *Store) observeDatabase(op int, start time.Time, err error) {
	stat := &s.guard().operations[op]
	elapsed := uint64(time.Since(start))
	stat.Count.Add(1)
	stat.DurationNS.Add(elapsed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		stat.Errors.Add(1)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		stat.Timeouts.Add(1)
	}
	for old := stat.MaxNS.Load(); elapsed > old; old = stat.MaxNS.Load() {
		if stat.MaxNS.CompareAndSwap(old, elapsed) {
			break
		}
	}
}

type databaseRow struct {
	row  *sql.Row
	done func(error)
}

func (r *databaseRow) Scan(out ...any) error {
	err := r.row.Scan(out...)
	r.done(err)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", errDatabaseUnavailable, err)
	}
	return err
}
func (r *databaseRow) Err() error {
	err := r.row.Err()
	if err != nil {
		r.done(err)
	}
	return err
}
func (s *Store) dbQueryRow(query string, args ...any) *databaseRow {
	return s.dbQueryRowContext(nil, query, args...)
}
func (s *Store) dbQueryRowContext(parent context.Context, query string, args ...any) *databaseRow {
	start := time.Now()
	if s.databaseBootstrapping {
		return &databaseRow{s.db.QueryRow(query, args...), func(error) {}}
	}
	ctx, cancel := s.databaseContext(parent)
	return &databaseRow{s.db.QueryRowContext(ctx, query, args...), func(err error) { cancel(); s.observeDatabase(1, start, err) }}
}
func (s *Store) dbQuery(query string, args ...any) (*sql.Rows, error) {
	return s.dbQueryContext(nil, query, args...)
}
func (s *Store) dbQueryContext(parent context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.databaseBootstrapping {
		return s.db.Query(query, args...)
	}
	ctx, cancel := s.databaseContext(parent)
	start := time.Now()
	rows, err := s.db.QueryContext(ctx, query, args...)
	s.observeDatabase(0, start, err)
	// A successful cursor needs its context while being consumed. Explicit read
	// helpers also own/cancel their parent deadline. Legacy calls retain at most
	// this five-second timer, which also cancels on Store shutdown.
	if err != nil {
		cancel()
	}
	return rows, err
}
func (s *Store) dbExec(query string, args ...any) (sql.Result, error) {
	return s.dbExecContext(nil, query, args...)
}
func (s *Store) dbExecContext(parent context.Context, query string, args ...any) (sql.Result, error) {
	if s.databaseBootstrapping {
		return s.db.Exec(query, args...)
	}
	ctx, cancel := s.databaseContext(parent)
	defer cancel()
	start := time.Now()
	result, err := s.db.ExecContext(ctx, query, args...)
	s.observeDatabase(2, start, err)
	return result, err
}
func (s *Store) dbBegin() (*sql.Tx, error) { return s.dbBeginTx(nil, nil) }
func (s *Store) dbBeginTx(parent context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	if s.databaseBootstrapping {
		return s.db.BeginTx(context.Background(), options)
	}
	ctx, cancel := s.databaseContext(parent)
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, options)
	s.observeDatabase(3, start, err)
	if err != nil {
		cancel()
	}
	return tx, err
}

// databaseHandle adapts helpers shared by *sql.Tx and *sql.DB. Transaction
// callers continue passing their transaction; request callers use this bounded
// handle so a helper cannot silently wait forever on the request pool.
type databaseHandle struct {
	store *Store
	ctx   context.Context
}

func (s *Store) boundedDatabase(ctx context.Context) databaseHandle {
	return databaseHandle{store: s, ctx: ctx}
}
func (d databaseHandle) Query(query string, args ...any) (*sql.Rows, error) {
	return d.store.dbQueryContext(d.ctx, query, args...)
}
func (d databaseHandle) QueryRow(query string, args ...any) *sql.Row {
	ctx, cancel := d.store.databaseContext(d.ctx)
	row := d.store.db.QueryRowContext(ctx, query, args...)
	// The shared interface requires a native *sql.Row. Its short context remains
	// alive through Scan and cancels at the deadline or on Store shutdown.
	if row.Err() != nil {
		cancel()
	}
	return row
}
func (d databaseHandle) Exec(query string, args ...any) (sql.Result, error) {
	return d.store.dbExecContext(d.ctx, query, args...)
}

func databaseUnavailable(err error) bool {
	return errors.Is(err, errDatabaseUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrConnDone) || err != nil && err.Error() == "sql: database is closed"
}
func databaseHTTPFailure(w http.ResponseWriter, err error) bool {
	if !databaseUnavailable(err) {
		return false
	}
	http.Error(w, "database temporarily unavailable", http.StatusServiceUnavailable)
	return true
}

func (s *Store) databaseMetrics() map[string]any {
	stats := s.db.Stats()
	operations := map[string]any{}
	for i, name := range []string{"query", "row", "exec", "transaction"} {
		v := &s.guard().operations[i]
		operations[name] = map[string]any{"count": v.Count.Load(), "errors": v.Errors.Load(), "timeouts": v.Timeouts.Load(), "duration_ms": float64(v.DurationNS.Load()) / 1e6, "max_ms": float64(v.MaxNS.Load()) / 1e6}
	}
	return map[string]any{"max_open": stats.MaxOpenConnections, "open": stats.OpenConnections, "in_use": stats.InUse, "idle": stats.Idle, "wait_count": stats.WaitCount, "wait_duration_ms": float64(stats.WaitDuration) / float64(time.Millisecond), "operations": operations}
}
func (s *Server) handleDatabaseReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), databaseReadinessTimeout)
	defer cancel()
	var one int
	err := s.store.dbQueryRowContext(ctx, "SELECT 1").Scan(&one)
	status := http.StatusOK
	ready := err == nil && s.ready.Load()
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSONStatus(w, status, map[string]any{"ok": ready, "database_ok": err == nil, "database": s.store.databaseMetrics()})
}
