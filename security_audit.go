package main

// security_audit.go — the unified security audit trail.
//
// Security evidence is kept apart from the main database and from model
// telemetry:
//
//   - its own SQLite file (<DATA_DIR>/security/security-audit.db, 0600), so
//     recording an event never competes with the platform's write lock;
//   - a bounded in-memory queue drained by one writer goroutine; a request
//     never blocks on the audit log, and an overflow is counted and written
//     back as an explicit "audit.events_dropped" event instead of vanishing;
//   - a hash chain over every row, so deletion or edits are detectable;
//   - its own retention (APTEVA_SECURITY_RETENTION_DAYS, default 400 days),
//     independent of telemetry's 30-day default;
//   - optional forwarding of every row to an independent append-only
//     destination (APTEVA_SECURITY_FORWARD_URL + _TOKEN);
//   - details are redacted before they are queued: raw credentials and
//     secret-looking values are never stored.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	securityResultSuccess = "success"
	securityResultFailure = "failure"
	securityResultDenied  = "denied"
	securityResultError   = "error"

	securitySeverityInfo     = "info"
	securitySeverityNotice   = "notice"
	securitySeverityWarning  = "warning"
	securitySeverityCritical = "critical"
)

// SecurityEvent is one attributable security-relevant action.
type SecurityEvent struct {
	Seq           int64          `json:"seq"`
	EventID       string         `json:"event_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	RecordedAt    time.Time      `json:"recorded_at"`
	Category      string         `json:"category"`
	Action        string         `json:"action"`
	Result        string         `json:"result"`
	Severity      string         `json:"severity"`
	PrincipalType string         `json:"principal_type,omitempty"`
	PrincipalID   string         `json:"principal_id,omitempty"`
	CredentialID  string         `json:"credential_id,omitempty"`
	SourceIP      string         `json:"source_ip,omitempty"`
	UserAgent     string         `json:"user_agent,omitempty"`
	OriginApp     string         `json:"origin_app,omitempty"`
	InstallID     int64          `json:"install_id,omitempty"`
	ProjectID     string         `json:"project_id,omitempty"`
	TargetType    string         `json:"target_type,omitempty"`
	TargetID      string         `json:"target_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	MachineID     string         `json:"machine_id,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
	PrevHash      string         `json:"prev_hash,omitempty"`
	Hash          string         `json:"hash,omitempty"`
}

type securityAuditOptions struct {
	QueueSize     int
	Retention     time.Duration
	ForwardURL    string
	ForwardToken  string
	ForwardEvery  time.Duration
	LowDiskBytes  int64
	PruneInterval time.Duration
}

func securityAuditOptionsFromEnv() securityAuditOptions {
	opts := securityAuditOptions{
		QueueSize:     8192,
		Retention:     400 * 24 * time.Hour,
		ForwardURL:    strings.TrimSpace(os.Getenv("APTEVA_SECURITY_FORWARD_URL")),
		ForwardToken:  strings.TrimSpace(os.Getenv("APTEVA_SECURITY_FORWARD_TOKEN")),
		ForwardEvery:  5 * time.Second,
		LowDiskBytes:  1 << 30,
		PruneInterval: time.Hour,
	}
	if days, err := strconv.Atoi(strings.TrimSpace(os.Getenv("APTEVA_SECURITY_RETENTION_DAYS"))); err == nil && days >= 30 {
		opts.Retention = time.Duration(days) * 24 * time.Hour
	}
	return opts
}

type securityWriteBatch struct {
	events []SecurityEvent
	done   chan securityWriteResult
}

type securityWriteResult struct {
	inserted int
	err      error
}

type securityAuditLog struct {
	db    *sql.DB
	dir   string
	opts  securityAuditOptions
	queue chan SecurityEvent
	sync  chan securityWriteBatch
	stop  chan struct{}
	wg    sync.WaitGroup

	dropped       atomic.Uint64
	pendingDrops  atomic.Uint64
	closed        atomic.Bool
	mu            sync.Mutex
	lastHash      string
	lastWriteAt   time.Time
	lastError     string
	lastErrorAt   time.Time
	fwdLastOK     time.Time
	fwdLastError  string
	fwdLastErrAt  time.Time
	diskFree      int64
	diskCheckedAt time.Time
	alertHook     func(SecurityEvent)
	alertWake     chan struct{}
}

var securityAuditSchema = []string{
	`CREATE TABLE IF NOT EXISTS security_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT NOT NULL UNIQUE,
		occurred_at TEXT NOT NULL,
		recorded_at TEXT NOT NULL,
		category TEXT NOT NULL,
		action TEXT NOT NULL,
		result TEXT NOT NULL,
		severity TEXT NOT NULL,
		principal_type TEXT NOT NULL DEFAULT '',
		principal_id TEXT NOT NULL DEFAULT '',
		credential_id TEXT NOT NULL DEFAULT '',
		source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		origin_app TEXT NOT NULL DEFAULT '',
		install_id INTEGER NOT NULL DEFAULT 0,
		project_id TEXT NOT NULL DEFAULT '',
		target_type TEXT NOT NULL DEFAULT '',
		target_id TEXT NOT NULL DEFAULT '',
		correlation_id TEXT NOT NULL DEFAULT '',
		machine_id TEXT NOT NULL DEFAULT '',
		details TEXT NOT NULL DEFAULT '{}',
		prev_hash TEXT NOT NULL DEFAULT '',
		hash TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_security_events_time ON security_events(occurred_at)`,
	`CREATE INDEX IF NOT EXISTS idx_security_events_category ON security_events(category, occurred_at)`,
	`CREATE INDEX IF NOT EXISTS idx_security_events_principal ON security_events(principal_type, principal_id, occurred_at)`,
	`CREATE INDEX IF NOT EXISTS idx_security_events_source ON security_events(source_ip, occurred_at)`,
	`CREATE TABLE IF NOT EXISTS security_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS security_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		alert_key TEXT NOT NULL UNIQUE,
		event_id TEXT NOT NULL,
		rule TEXT NOT NULL,
		severity TEXT NOT NULL,
		message TEXT NOT NULL,
		machine_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		acknowledged_at TEXT,
		acknowledged_by INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_security_alerts_open ON security_alerts(acknowledged_at, created_at)`,
}

func openSecurityAuditLog(dir string, opts securityAuditOptions) (*securityAuditLog, error) {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 8192
	}
	if opts.Retention <= 0 {
		opts.Retention = 400 * 24 * time.Hour
	}
	if opts.ForwardEvery <= 0 {
		opts.ForwardEvery = 5 * time.Second
	}
	if opts.PruneInterval <= 0 {
		opts.PruneInterval = time.Hour
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0700)
	path := filepath.Join(dir, "security-audit.db")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600); err == nil {
		_ = f.Close()
	}
	_ = os.Chmod(path, 0600)
	// _txlock=immediate takes the write lock at BEGIN, so the chain head read
	// inside write() cannot race a second server slot sharing this file.
	db, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	for _, stmt := range securityAuditSchema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("security audit schema: %w", err)
		}
	}
	l := &securityAuditLog{db: db, dir: dir, opts: opts, queue: make(chan SecurityEvent, opts.QueueSize),
		sync: make(chan securityWriteBatch), stop: make(chan struct{}), alertWake: make(chan struct{}, 1)}
	_ = db.QueryRow(`SELECT hash FROM security_events ORDER BY seq DESC LIMIT 1`).Scan(&l.lastHash)
	l.refreshDisk()
	l.wg.Add(1)
	go l.writer()
	if opts.ForwardURL != "" {
		l.wg.Add(1)
		go l.forwarder()
	}
	return l, nil
}

func (l *securityAuditLog) Close() {
	if l == nil || !l.closed.CompareAndSwap(false, true) {
		return
	}
	close(l.stop)
	l.wg.Wait()
	_ = l.db.Close()
}

func newSecurityEventID() string {
	return "sev_" + generateToken(16)
}

func (l *securityAuditLog) normalize(ev SecurityEvent) SecurityEvent {
	now := time.Now().UTC()
	if ev.EventID == "" {
		ev.EventID = newSecurityEventID()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = now
	}
	ev.OccurredAt = ev.OccurredAt.UTC()
	if ev.Result == "" {
		ev.Result = securityResultSuccess
	}
	if ev.Severity == "" {
		ev.Severity = securitySeverityInfo
		if ev.Result == securityResultFailure || ev.Result == securityResultDenied {
			ev.Severity = securitySeverityNotice
		}
	}
	if ev.Category == "" {
		ev.Category = "other"
	}
	ev.UserAgent = truncateAuditString(ev.UserAgent, 256)
	ev.SourceIP = truncateAuditString(ev.SourceIP, 64)
	ev.Details = redactSecurityDetails(ev.Details)
	return ev
}

// Record queues an event without blocking. A nil log is a no-op so call sites
// never need to guard (tests and early boot run without one).
func (l *securityAuditLog) Record(ev SecurityEvent) {
	if l == nil || l.closed.Load() {
		return
	}
	ev = l.normalize(ev)
	select {
	case l.queue <- ev:
	default:
		l.dropped.Add(1)
		l.pendingDrops.Add(1)
	}
}

// RecordBatch writes events synchronously and idempotently (event_id is
// unique). Used for ingestion from the host collector, where the caller must
// know the events are durable before advancing its own cursor.
func (l *securityAuditLog) RecordBatch(ctx context.Context, events []SecurityEvent) (int, error) {
	if l == nil {
		return 0, errors.New("security audit log unavailable")
	}
	if len(events) == 0 {
		return 0, nil
	}
	normalized := make([]SecurityEvent, len(events))
	for i, ev := range events {
		normalized[i] = l.normalize(ev)
	}
	req := securityWriteBatch{events: normalized, done: make(chan securityWriteResult, 1)}
	select {
	case l.sync <- req:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-l.stop:
		return 0, errors.New("security audit log closed")
	}
	select {
	case res := <-req.done:
		return res.inserted, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (l *securityAuditLog) writer() {
	defer l.wg.Done()
	prune := time.NewTicker(l.opts.PruneInterval)
	defer prune.Stop()
	l.pruneOnce()
	for {
		select {
		case <-l.stop:
			// Drain what is already queued so a clean shutdown loses nothing.
			var rest []SecurityEvent
			for {
				select {
				case ev := <-l.queue:
					rest = append(rest, ev)
					continue
				default:
				}
				break
			}
			if len(rest) > 0 {
				l.write(rest)
			}
			return
		case req := <-l.sync:
			n, err := l.write(req.events)
			req.done <- securityWriteResult{inserted: n, err: err}
		case ev := <-l.queue:
			batch := []SecurityEvent{ev}
		fill:
			for len(batch) < 256 {
				select {
				case next := <-l.queue:
					batch = append(batch, next)
				default:
					break fill
				}
			}
			if drops := l.pendingDrops.Swap(0); drops > 0 {
				batch = append(batch, l.normalize(SecurityEvent{Category: "audit", Action: "audit.events_dropped",
					Result: securityResultError, Severity: securitySeverityCritical, PrincipalType: "system",
					Details: map[string]any{"dropped": drops, "reason": "queue_full"}}))
				log.Printf("[SECURITY] audit queue overflow: %d event(s) dropped", drops)
			}
			_, _ = l.write(batch)
		case <-prune.C:
			l.pruneOnce()
			l.refreshDisk()
		}
	}
}

func securityEventHash(prev string, ev SecurityEvent) string {
	ev.Seq, ev.Hash, ev.PrevHash = 0, "", ""
	ev.RecordedAt = time.Time{}
	b, _ := json.Marshal(ev)
	sum := sha256.Sum256(append([]byte(prev+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

func (l *securityAuditLog) write(events []SecurityEvent) (int, error) {
	tx, err := l.db.Begin()
	if err != nil {
		l.noteError(err)
		return 0, err
	}
	prev := ""
	if err := tx.QueryRow(`SELECT hash FROM security_events ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		l.noteError(err)
		return 0, err
	}
	inserted := 0
	var hooks []SecurityEvent
	now := time.Now().UTC()
	for _, ev := range events {
		ev.RecordedAt = now
		ev.PrevHash = prev
		ev.Hash = securityEventHash(prev, ev)
		details, _ := json.Marshal(ev.Details)
		if len(details) == 0 || string(details) == "null" {
			details = []byte("{}")
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO security_events(event_id, occurred_at, recorded_at, category, action, result, severity,
			principal_type, principal_id, credential_id, source_ip, user_agent, origin_app, install_id, project_id,
			target_type, target_id, correlation_id, machine_id, details, prev_hash, hash)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ev.EventID, ev.OccurredAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), ev.Category, ev.Action, ev.Result, ev.Severity,
			ev.PrincipalType, ev.PrincipalID, ev.CredentialID, ev.SourceIP, ev.UserAgent, ev.OriginApp, ev.InstallID, ev.ProjectID,
			ev.TargetType, ev.TargetID, ev.CorrelationID, ev.MachineID, string(details), ev.PrevHash, ev.Hash)
		if err != nil {
			_ = tx.Rollback()
			l.noteError(err)
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			prev = ev.Hash
			inserted++
			hooks = append(hooks, ev)
		}
	}
	if err := tx.Commit(); err != nil {
		l.noteError(err)
		return 0, err
	}
	l.mu.Lock()
	l.lastHash = prev
	l.lastWriteAt = now
	hook := l.alertHook
	l.mu.Unlock()
	if hook != nil {
		for _, ev := range hooks {
			hook(ev)
		}
	}
	return inserted, nil
}

func (l *securityAuditLog) noteError(err error) {
	l.mu.Lock()
	l.lastError = err.Error()
	l.lastErrorAt = time.Now().UTC()
	l.mu.Unlock()
	log.Printf("[SECURITY] audit write failed: %v", err)
}

func (l *securityAuditLog) pruneOnce() {
	cutoff := time.Now().UTC().Add(-l.opts.Retention).Format(time.RFC3339Nano)
	if _, err := l.db.Exec(`DELETE FROM security_events WHERE occurred_at < ?`, cutoff); err != nil {
		l.noteError(err)
	}
	_, _ = l.db.Exec(`DELETE FROM security_alerts WHERE created_at < ? AND acknowledged_at IS NOT NULL`, cutoff)
}

func (l *securityAuditLog) refreshDisk() {
	free, err := runtimeFilesystemFree(l.dir)
	l.mu.Lock()
	if err == nil {
		l.diskFree = free
	}
	l.diskCheckedAt = time.Now().UTC()
	l.mu.Unlock()
}

// Flush waits until everything queued so far is durable. Tests and shutdown
// paths use it; request handlers never do.
func (l *securityAuditLog) Flush(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for len(l.queue) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The writer handles one request at a time, so an empty synchronous batch
	// completes only after the batch it may currently be committing.
	req := securityWriteBatch{done: make(chan securityWriteResult, 1)}
	select {
	case l.sync <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.stop:
		return errors.New("security audit log closed")
	}
	select {
	case res := <-req.done:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SecurityEventQuery filters the timeline.
type SecurityEventQuery struct {
	Category      string
	Action        string
	PrincipalType string
	PrincipalID   string
	SourceIP      string
	Result        string
	Since         time.Time
	Until         time.Time
	BeforeSeq     int64
	Limit         int
}

func (l *securityAuditLog) Query(q SecurityEventQuery) ([]SecurityEvent, error) {
	if l == nil {
		return nil, errors.New("security audit log unavailable")
	}
	var where []string
	var args []any
	add := func(clause string, v any) { where = append(where, clause); args = append(args, v) }
	if q.Category != "" {
		add("category = ?", q.Category)
	}
	if q.Action != "" {
		add("action = ?", q.Action)
	}
	if q.PrincipalType != "" {
		add("principal_type = ?", q.PrincipalType)
	}
	if q.PrincipalID != "" {
		add("principal_id = ?", q.PrincipalID)
	}
	if q.SourceIP != "" {
		add("source_ip = ?", q.SourceIP)
	}
	if q.Result != "" {
		add("result = ?", q.Result)
	}
	if !q.Since.IsZero() {
		add("occurred_at >= ?", q.Since.UTC().Format(time.RFC3339Nano))
	}
	if !q.Until.IsZero() {
		add("occurred_at < ?", q.Until.UTC().Format(time.RFC3339Nano))
	}
	if q.BeforeSeq > 0 {
		add("seq < ?", q.BeforeSeq)
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	query := `SELECT seq, event_id, occurred_at, recorded_at, category, action, result, severity, principal_type, principal_id,
		credential_id, source_ip, user_agent, origin_app, install_id, project_id, target_type, target_id, correlation_id,
		machine_id, details, prev_hash, hash FROM security_events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY seq DESC LIMIT ?"
	args = append(args, q.Limit)
	return l.scanEvents(query, args...)
}

func (l *securityAuditLog) scanEvents(query string, args ...any) ([]SecurityEvent, error) {
	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecurityEvent
	for rows.Next() {
		var ev SecurityEvent
		var occurred, recorded, details string
		if err := rows.Scan(&ev.Seq, &ev.EventID, &occurred, &recorded, &ev.Category, &ev.Action, &ev.Result, &ev.Severity,
			&ev.PrincipalType, &ev.PrincipalID, &ev.CredentialID, &ev.SourceIP, &ev.UserAgent, &ev.OriginApp, &ev.InstallID,
			&ev.ProjectID, &ev.TargetType, &ev.TargetID, &ev.CorrelationID, &ev.MachineID, &details, &ev.PrevHash, &ev.Hash); err != nil {
			return nil, err
		}
		ev.OccurredAt, _ = time.Parse(time.RFC3339Nano, occurred)
		ev.RecordedAt, _ = time.Parse(time.RFC3339Nano, recorded)
		_ = json.Unmarshal([]byte(details), &ev.Details)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SecurityChainStatus reports whether the stored rows still form an unbroken
// hash chain (detects edited or deleted rows inside the retention window).
type SecurityChainStatus struct {
	OK        bool   `json:"ok"`
	Checked   int    `json:"checked"`
	BrokenAt  int64  `json:"broken_at_seq,omitempty"`
	Reason    string `json:"reason,omitempty"`
	FirstSeq  int64  `json:"first_seq,omitempty"`
	LastSeq   int64  `json:"last_seq,omitempty"`
	CheckedAt string `json:"checked_at"`
}

func (l *securityAuditLog) VerifyChain() (SecurityChainStatus, error) {
	status := SecurityChainStatus{OK: true, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if l == nil {
		return status, errors.New("security audit log unavailable")
	}
	events, err := l.scanEvents(`SELECT seq, event_id, occurred_at, recorded_at, category, action, result, severity, principal_type, principal_id,
		credential_id, source_ip, user_agent, origin_app, install_id, project_id, target_type, target_id, correlation_id,
		machine_id, details, prev_hash, hash FROM security_events ORDER BY seq ASC`)
	if err != nil {
		return status, err
	}
	prev := ""
	for i, ev := range events {
		if i == 0 {
			// Retention prunes from the start; the oldest kept row anchors the chain.
			prev = ev.PrevHash
			status.FirstSeq = ev.Seq
		}
		if ev.PrevHash != prev {
			return SecurityChainStatus{OK: false, Checked: i, BrokenAt: ev.Seq, Reason: "previous hash mismatch (row removed or reordered)", FirstSeq: status.FirstSeq, CheckedAt: status.CheckedAt}, nil
		}
		// Details round-trip through JSON; numbers decode as float64, which is
		// also how they were hashed when the original map held JSON numbers.
		if securityEventHash(prev, normalizeForHash(ev)) != ev.Hash {
			return SecurityChainStatus{OK: false, Checked: i, BrokenAt: ev.Seq, Reason: "row content does not match its hash (row edited)", FirstSeq: status.FirstSeq, CheckedAt: status.CheckedAt}, nil
		}
		prev = ev.Hash
		status.Checked++
		status.LastSeq = ev.Seq
	}
	return status, nil
}

// normalizeForHash makes a stored event hash-identical to the in-memory event
// that was hashed at insert time (JSON-typed details, UTC times).
func normalizeForHash(ev SecurityEvent) SecurityEvent {
	if ev.Details != nil {
		b, _ := json.Marshal(ev.Details)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		ev.Details = m
	}
	ev.OccurredAt = ev.OccurredAt.UTC()
	return ev
}

// SecurityAuditHealth is surfaced on the Security page; any degraded field is
// itself an alert condition (logging failures must be visible).
type SecurityAuditHealth struct {
	Available       bool     `json:"available"`
	QueueDepth      int      `json:"queue_depth"`
	QueueCapacity   int      `json:"queue_capacity"`
	DroppedTotal    uint64   `json:"dropped_total"`
	LastWriteAt     string   `json:"last_write_at,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	LastErrorAt     string   `json:"last_error_at,omitempty"`
	RetentionDays   int      `json:"retention_days"`
	DiskFreeBytes   int64    `json:"disk_free_bytes"`
	DiskLow         bool     `json:"disk_low"`
	ForwardEnabled  bool     `json:"forward_enabled"`
	ForwardLastOK   string   `json:"forward_last_ok_at,omitempty"`
	ForwardLastErr  string   `json:"forward_last_error,omitempty"`
	ForwardLagCount int64    `json:"forward_lag_events"`
	Degraded        bool     `json:"degraded"`
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
}

func (l *securityAuditLog) Health() SecurityAuditHealth {
	if l == nil {
		return SecurityAuditHealth{Degraded: true, DegradedReasons: []string{"security audit log is not running"}}
	}
	l.mu.Lock()
	h := SecurityAuditHealth{Available: !l.closed.Load(), QueueDepth: len(l.queue), QueueCapacity: cap(l.queue),
		DroppedTotal: l.dropped.Load(), LastError: l.lastError, RetentionDays: int(l.opts.Retention / (24 * time.Hour)),
		DiskFreeBytes: l.diskFree, ForwardEnabled: l.opts.ForwardURL != "", ForwardLastErr: l.fwdLastError}
	if !l.lastWriteAt.IsZero() {
		h.LastWriteAt = l.lastWriteAt.Format(time.RFC3339)
	}
	if !l.lastErrorAt.IsZero() {
		h.LastErrorAt = l.lastErrorAt.Format(time.RFC3339)
	}
	if !l.fwdLastOK.IsZero() {
		h.ForwardLastOK = l.fwdLastOK.Format(time.RFC3339)
	}
	recentError := !l.lastErrorAt.IsZero() && l.lastErrorAt.After(l.lastWriteAt)
	fwdFailing := l.fwdLastError != "" && l.fwdLastErrAt.After(l.fwdLastOK)
	l.mu.Unlock()
	h.DiskLow = l.opts.LowDiskBytes > 0 && h.DiskFreeBytes > 0 && h.DiskFreeBytes < l.opts.LowDiskBytes
	if h.ForwardEnabled {
		h.ForwardLagCount = l.forwardLag()
	}
	if recentError {
		h.DegradedReasons = append(h.DegradedReasons, "audit writes are failing")
	}
	if h.DroppedTotal > 0 {
		h.DegradedReasons = append(h.DegradedReasons, "audit events were dropped since start")
	}
	if h.DiskLow {
		h.DegradedReasons = append(h.DegradedReasons, "low disk space for security logs")
	}
	if fwdFailing {
		h.DegradedReasons = append(h.DegradedReasons, "forwarding to the independent destination is failing")
	}
	if h.QueueCapacity > 0 && h.QueueDepth*10 > h.QueueCapacity*8 {
		h.DegradedReasons = append(h.DegradedReasons, "audit queue is almost full")
	}
	h.Degraded = len(h.DegradedReasons) > 0
	return h
}

// ─── forwarding to an independent destination ────────────────────────────

func (l *securityAuditLog) metaInt(key string) int64 {
	var raw string
	if l.db.QueryRow(`SELECT value FROM security_meta WHERE key = ?`, key).Scan(&raw) != nil {
		return 0
	}
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

func (l *securityAuditLog) setMetaInt(key string, v int64) error {
	_, err := l.db.Exec(`INSERT INTO security_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, strconv.FormatInt(v, 10))
	return err
}

func (l *securityAuditLog) forwardLag() int64 {
	var n int64
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM security_events WHERE seq > ?`, l.metaInt("forward_cursor")).Scan(&n)
	return n
}

func (l *securityAuditLog) forwarder() {
	defer l.wg.Done()
	client := &http.Client{Timeout: 15 * time.Second}
	backoff := l.opts.ForwardEvery
	for {
		select {
		case <-l.stop:
			return
		case <-time.After(backoff):
		}
		if err := l.forwardOnce(client); err != nil {
			l.mu.Lock()
			l.fwdLastError = err.Error()
			l.fwdLastErrAt = time.Now().UTC()
			l.mu.Unlock()
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = l.opts.ForwardEvery
	}
}

func (l *securityAuditLog) forwardOnce(client *http.Client) error {
	for {
		cursor := l.metaInt("forward_cursor")
		events, err := l.scanEvents(`SELECT seq, event_id, occurred_at, recorded_at, category, action, result, severity, principal_type, principal_id,
			credential_id, source_ip, user_agent, origin_app, install_id, project_id, target_type, target_id, correlation_id,
			machine_id, details, prev_hash, hash FROM security_events WHERE seq > ? ORDER BY seq ASC LIMIT 200`, cursor)
		if err != nil || len(events) == 0 {
			if err == nil {
				l.mu.Lock()
				l.fwdLastOK = time.Now().UTC()
				l.mu.Unlock()
			}
			return err
		}
		body, _ := json.Marshal(map[string]any{"source": "apteva-server", "events": events})
		req, err := http.NewRequest(http.MethodPost, l.opts.ForwardURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", events[0].EventID+".."+events[len(events)-1].EventID)
		if l.opts.ForwardToken != "" {
			req.Header.Set("Authorization", "Bearer "+l.opts.ForwardToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("forward destination returned HTTP %d", resp.StatusCode)
		}
		if err := l.setMetaInt("forward_cursor", events[len(events)-1].Seq); err != nil {
			return err
		}
		l.mu.Lock()
		l.fwdLastOK = time.Now().UTC()
		l.fwdLastError = ""
		l.mu.Unlock()
	}
}

// ─── redaction ────────────────────────────────────────────────────────────

var secretKeyPattern = regexp.MustCompile(`(?i)(secret|token|password|passwd|passphrase|api[_-]?key|private[_-]?key|authorization|cookie|credential|session|bearer|signature|otp|totp|recovery)`)

// Values that look like credentials regardless of their key.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|pk|rk|uk|core|ak|tok)[-_][A-Za-z0-9_\-]{12,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-~+/=]{8,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key)\s*[=:]\s*\S+`),
	regexp.MustCompile(`\b[a-fA-F0-9]{40,}\b`),
}

const redacted = "[redacted]"

func redactSecurityValue(v string) string {
	for _, p := range secretValuePatterns {
		v = p.ReplaceAllString(v, redacted)
	}
	return truncateAuditString(v, 512)
}

func truncateAuditString(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max] + "…"
}

func redactSecurityDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i >= 64 {
			out["_truncated_keys"] = len(keys) - 64
			break
		}
		if secretKeyPattern.MatchString(k) && !securityDetailKeyAllowed(k) {
			out[k] = redacted
			continue
		}
		out[k] = redactSecurityAny(in[k], 0)
	}
	return out
}

// Keys that match the secret pattern but carry identifiers, not secrets.
func securityDetailKeyAllowed(k string) bool {
	switch strings.ToLower(k) {
	case "session_id", "credential_id", "credential_type", "token_type", "key_id", "key_prefix", "session_count",
		"revoked_sessions", "revoked_keys", "revoked_tokens", "session_ref", "sessions_revoked", "keys_revoked":
		return true
	}
	return false
}

func redactSecurityAny(v any, depth int) any {
	if depth > 4 {
		return "[nested]"
	}
	switch t := v.(type) {
	case string:
		return redactSecurityValue(t)
	case map[string]any:
		return redactSecurityDetails(t)
	case []string:
		out := make([]any, 0, len(t))
		for i, s := range t {
			if i >= 50 {
				break
			}
			out = append(out, redactSecurityValue(s))
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for i, s := range t {
			if i >= 50 {
				break
			}
			out = append(out, redactSecurityAny(s, depth+1))
		}
		return out
	case nil, bool, int, int32, int64, uint, uint32, uint64, float32, float64:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%T", t)
		}
		var decoded any
		if json.Unmarshal(b, &decoded) != nil {
			return redactSecurityValue(string(b))
		}
		return redactSecurityAny(decoded, depth+1)
	}
}

// credentialRef returns a non-reversible reference for a bearer secret so
// events can be correlated without storing the credential.
func credentialRef(kind, secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return kind + ":" + hex.EncodeToString(sum[:])[:16]
}
