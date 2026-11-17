package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiKeyUsageCapacity        = 4096
	apiKeyUsageBatchSize       = 128
	apiKeyUsageInterval        = time.Second
	apiKeyUsageBusyMS          = 100
	apiKeyUsageWriteTimeout    = 500 * time.Millisecond
	apiKeyUsageShutdownTimeout = time.Second
)

type apiKeyUsageUpdate struct {
	id    int64
	at    time.Time
	ip    string
	setIP bool
}

// One worker per Store, with a bounded map rather than a goroutine per request.
// Its reserved connection has a short busy timeout and cannot consume the
// remaining request connections while waiting for SQLite's writer lock.
// Usage metadata is intentionally eventually consistent, never authorization.
type apiKeyUsageTracker struct {
	db      *sql.DB
	conn    *sql.Conn
	mu      sync.Mutex
	pending map[int64]apiKeyUsageUpdate
	stopped bool
	dropped atomic.Uint64
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func newAPIKeyUsageTracker(db *sql.DB) (*apiKeyUsageTracker, error) {
	conn, err := openAPIKeyUsageConn(context.Background(), db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &apiKeyUsageTracker{db: db, conn: conn, pending: make(map[int64]apiKeyUsageUpdate), wake: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	go w.run(ctx)
	return w, nil
}

func openAPIKeyUsageConn(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", apiKeyUsageBusyMS)); err != nil {
		// A partially initialized connection must not rejoin the request pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (w *apiKeyUsageTracker) releaseConn() {
	if w.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiKeyUsageWriteTimeout)
	defer cancel()
	// Restore before returning this connection to the shared pool; discard it
	// if a timeout/driver error made that impossible.
	if _, err := w.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMS)); err != nil {
		_ = w.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = w.conn.Close()
	w.conn = nil
}

func (w *apiKeyUsageTracker) record(id int64, ip string, setIP bool) {
	if w == nil || id <= 0 {
		return
	}
	// Bound memory even if an unexpected caller supplies an oversized value.
	if len(ip) > 128 {
		ip = strings.Clone(ip[:128])
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	previous, exists := w.pending[id]
	if !exists && len(w.pending) >= apiKeyUsageCapacity {
		w.dropped.Add(1)
		w.mu.Unlock()
		return
	}
	u := apiKeyUsageUpdate{id: id, at: time.Now().UTC(), ip: ip, setIP: setIP}
	if !setIP && exists {
		u.ip, u.setIP = previous.ip, previous.setIP
	}
	w.pending[id] = u
	ready := len(w.pending) >= apiKeyUsageBatchSize
	w.mu.Unlock()
	if ready {
		w.signal()
	}
}

func (w *apiKeyUsageTracker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *apiKeyUsageTracker) take() []apiKeyUsageUpdate {
	w.mu.Lock()
	defer w.mu.Unlock()
	batch := make([]apiKeyUsageUpdate, 0, apiKeyUsageBatchSize)
	for id, u := range w.pending {
		batch = append(batch, u)
		delete(w.pending, id)
		if len(batch) == apiKeyUsageBatchSize {
			break
		}
	}
	return batch
}

func (w *apiKeyUsageTracker) retry(batch []apiKeyUsageUpdate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, u := range batch {
		if newer, ok := w.pending[u.id]; ok {
			// Requests queued during the write win; retain the older IP only
			// when the newer request did not supply one.
			if !newer.setIP {
				newer.ip, newer.setIP = u.ip, u.setIP
				w.pending[u.id] = newer
			}
		} else if len(w.pending) < apiKeyUsageCapacity {
			w.pending[u.id] = u
		} else {
			w.dropped.Add(1)
		}
	}
}

func (w *apiKeyUsageTracker) flush(ctx context.Context) (int, error) {
	batch := w.take()
	if len(batch) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, apiKeyUsageWriteTimeout)
	defer cancel()
	var err error
	if w.conn == nil {
		w.conn, err = openAPIKeyUsageConn(ctx, w.db)
	}
	var tx *sql.Tx
	if err == nil {
		tx, err = w.conn.BeginTx(ctx, nil)
	}
	if err == nil {
		for _, u := range batch {
			at := u.at.Format("2006-01-02 15:04:05")
			_, err = tx.ExecContext(ctx, `UPDATE api_keys SET
 last_used=CASE WHEN last_used IS NULL OR last_used<? THEN ? ELSE last_used END,
 last_used_ip=CASE WHEN ? AND (last_used IS NULL OR last_used<=?) THEN ? ELSE last_used_ip END
 WHERE id=? AND revoked_at IS NULL`, at, at, u.setIP, at, u.ip, u.id)
			if err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		// Also releases a transaction when Commit fails.
		_ = tx.Rollback()
	}
	if err != nil {
		// SQLite can invalidate an interrupted connection. The next retry
		// obtains a fresh one instead of retaining a dead worker connection.
		if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) || ctx.Err() != nil {
			w.releaseConn()
		}
		w.retry(batch)
		return 0, err
	}
	return len(batch), nil
}

func (w *apiKeyUsageTracker) pendingCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending)
}

func (w *apiKeyUsageTracker) run(ctx context.Context) {
	defer close(w.done)
	defer w.releaseConn()
	ticker := time.NewTicker(apiKeyUsageInterval)
	defer ticker.Stop()
	lastErrorLog := time.Time{}
	for {
		select {
		case <-ctx.Done():
			w.shutdownFlush()
			return
		case <-ticker.C:
		case <-w.wake:
		}
		if dropped := w.dropped.Swap(0); dropped > 0 {
			log.Printf("[API-KEY-USAGE] dropped %d metadata updates: queue full", dropped)
		}
		_, err := w.flush(ctx)
		if err != nil {
			if ctx.Err() == nil && time.Since(lastErrorLog) >= time.Minute {
				log.Printf("[API-KEY-USAGE] flush deferred: %v", err)
				lastErrorLog = time.Now()
			}
		} else if w.pendingCount() >= apiKeyUsageBatchSize {
			w.signal()
		}
	}
}

func (w *apiKeyUsageTracker) shutdownFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), apiKeyUsageShutdownTimeout)
	defer cancel()
	for w.pendingCount() > 0 && ctx.Err() == nil {
		if _, err := w.flush(ctx); err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if remaining := w.pendingCount(); remaining > 0 {
		log.Printf("[API-KEY-USAGE] shutdown deadline: %d metadata updates not saved", remaining)
	}
}

func (w *apiKeyUsageTracker) Close() {
	w.once.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.mu.Unlock()
		w.cancel()
		<-w.done
	})
}
