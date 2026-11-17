package main

// store_credential_security.go — credential storage and revocation records.
//
//   - Session tokens are stored as "sh_"+sha256(token). A database read no
//     longer yields usable dashboard sessions. Existing rows are converted
//     once on upgrade, so nobody is logged out.
//   - Sessions keep their creation source (IP, user agent) and are revoked,
//     not deleted, with actor, time and reason. Expired/revoked rows are
//     purged after sessionRecordRetention.
//   - Private API keys are revoked, not deleted, with actor and reason.
//   - Users can be disabled. Every credential path (session, private key,
//     delegated key, app user-session callbacks) refuses a disabled user, so
//     disabling takes effect on already-issued credentials immediately.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sessionKeyPrefix       = "sh_"
	sessionRecordRetention = 90 * 24 * time.Hour
	sessionTouchInterval   = 5 * time.Minute
)

var errUserDisabled = errors.New("account is disabled")

func sessionKey(token string) string {
	if token == "" || strings.HasPrefix(token, sessionKeyPrefix) {
		// Already a key (or empty). Raw tokens are 64 hex chars and never
		// start with the prefix, so this cannot be confused with a raw token.
		return token
	}
	sum := sha256.Sum256([]byte(token))
	return sessionKeyPrefix + hex.EncodeToString(sum[:])
}

func (s *Store) migrateCredentialSecurity() error {
	for _, col := range []struct{ table, name, ddl string }{
		{"sessions", "created_ip", "ALTER TABLE sessions ADD COLUMN created_ip TEXT NOT NULL DEFAULT ''"},
		{"sessions", "created_user_agent", "ALTER TABLE sessions ADD COLUMN created_user_agent TEXT NOT NULL DEFAULT ''"},
		{"sessions", "last_seen_at", "ALTER TABLE sessions ADD COLUMN last_seen_at DATETIME"},
		{"sessions", "last_seen_ip", "ALTER TABLE sessions ADD COLUMN last_seen_ip TEXT NOT NULL DEFAULT ''"},
		{"sessions", "revoked_at", "ALTER TABLE sessions ADD COLUMN revoked_at DATETIME"},
		{"sessions", "revoked_by", "ALTER TABLE sessions ADD COLUMN revoked_by INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "revoke_reason", "ALTER TABLE sessions ADD COLUMN revoke_reason TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "revoked_by", "ALTER TABLE api_keys ADD COLUMN revoked_by INTEGER NOT NULL DEFAULT 0"},
		{"api_keys", "revoke_reason", "ALTER TABLE api_keys ADD COLUMN revoke_reason TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "created_ip", "ALTER TABLE api_keys ADD COLUMN created_ip TEXT NOT NULL DEFAULT ''"},
		{"users", "disabled_at", "ALTER TABLE users ADD COLUMN disabled_at DATETIME"},
		{"users", "disabled_by", "ALTER TABLE users ADD COLUMN disabled_by INTEGER NOT NULL DEFAULT 0"},
		{"users", "disabled_reason", "ALTER TABLE users ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''"},
		{"sessions", "reauth_at", "ALTER TABLE sessions ADD COLUMN reauth_at DATETIME"},
	} {
		if !columnExists(s.db, col.table, col.name) {
			if _, err := s.db.Exec(col.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("%s.%s: %w", col.table, col.name, err)
			}
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id, revoked_at)`); err != nil {
		return err
	}
	return s.hashLegacySessionTokens()
}

// hashLegacySessionTokens converts raw session tokens in place. Idempotent:
// already-hashed rows carry the prefix and are skipped.
func (s *Store) hashLegacySessionTokens() error {
	rows, err := s.db.Query(`SELECT token FROM sessions WHERE token NOT LIKE 'sh\_%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	var raw []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			raw = append(raw, t)
		}
	}
	rows.Close()
	if len(raw) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, t := range raw {
		if _, err := tx.Exec(`UPDATE OR IGNORE sessions SET token = ? WHERE token = ?`, sessionKey(t), t); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func sqliteNow() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

// CreateSessionWithContext records where a session was created.
func (s *Store) CreateSessionWithContext(token string, userID int64, expiresAt time.Time, ip, userAgent string) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at, created_ip, created_user_agent, reauth_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionKey(token), userID, expiresAt.UTC().Format("2006-01-02 15:04:05"), truncateAuditString(ip, 64), truncateAuditString(userAgent, 256), sqliteNow(),
	)
	return err
}

// RevokeSession marks one session revoked. actor 0 = the session owner/system.
func (s *Store) RevokeSession(token string, actor int64, reason string) error {
	_, err := s.db.Exec(`UPDATE sessions SET revoked_at = COALESCE(revoked_at, ?), revoked_by = CASE WHEN revoked_at IS NULL THEN ? ELSE revoked_by END,
		revoke_reason = CASE WHEN revoked_at IS NULL THEN ? ELSE revoke_reason END WHERE token = ?`,
		sqliteNow(), actor, reason, sessionKey(token))
	return err
}

// RevokeUserSessions revokes every live session of a user except keepToken
// (pass "" to revoke all). Returns how many were revoked.
func (s *Store) RevokeUserSessions(userID int64, keepToken string, actor int64, reason string) (int64, error) {
	res, err := s.db.Exec(`UPDATE sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL AND token != ?`, sqliteNow(), actor, reason, userID, sessionKey(keepToken))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeSessionByRef revokes a session identified by its public reference
// (credentialRef of the raw token is unavailable server-side, so sessions are
// addressed by the first 16 hex chars of their stored key).
func (s *Store) RevokeSessionByRef(userID int64, ref string, actor int64, reason string) (bool, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "session:")
	if len(ref) < 12 {
		return false, errors.New("invalid session reference")
	}
	res, err := s.db.Exec(`UPDATE sessions SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL AND substr(token, 4, ?) = ?`, sqliteNow(), actor, reason, userID, len(ref), ref)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

var (
	sessionTouches    sync.Map // session key -> time.Time
	sessionTouchCount atomic.Int64
)

// TouchSession records last activity at most every sessionTouchInterval so
// ordinary requests do not take the database write lock.
func (s *Store) TouchSession(token, ip string) {
	key := sessionKey(token)
	now := time.Now()
	if v, ok := sessionTouches.Load(key); ok && now.Sub(v.(time.Time)) < sessionTouchInterval {
		return
	}
	sessionTouches.Store(key, now)
	if sessionTouchCount.Add(1)%1024 == 0 {
		// Keep the throttle map bounded by live sessions, not every session
		// ever seen by this process.
		sessionTouches.Range(func(k, v any) bool {
			if now.Sub(v.(time.Time)) >= sessionTouchInterval {
				sessionTouches.Delete(k)
			}
			return true
		})
	}
	go func() {
		_, _ = s.db.Exec(`UPDATE sessions SET last_seen_at = ?, last_seen_ip = ? WHERE token = ?`, sqliteNow(), truncateAuditString(ip, 64), key)
	}()
}

// SessionRecord is the operator-visible view of a session (no token).
type SessionRecord struct {
	Ref              string `json:"ref"`
	UserID           int64  `json:"user_id"`
	CreatedAt        string `json:"created_at"`
	CreatedIP        string `json:"created_ip,omitempty"`
	CreatedUserAgent string `json:"created_user_agent,omitempty"`
	LastSeenAt       string `json:"last_seen_at,omitempty"`
	LastSeenIP       string `json:"last_seen_ip,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	AuthState        string `json:"auth_state"`
	RevokedAt        string `json:"revoked_at,omitempty"`
	RevokedBy        int64  `json:"revoked_by,omitempty"`
	RevokeReason     string `json:"revoke_reason,omitempty"`
	Active           bool   `json:"active"`
}

func (s *Store) ListSessionRecords(userID int64, includeInactive bool) ([]SessionRecord, error) {
	query := `SELECT token, user_id, COALESCE(created_at,''), created_ip, created_user_agent, COALESCE(last_seen_at,''), last_seen_ip,
		expires_at, COALESCE(auth_state,'active'), COALESCE(revoked_at,''), revoked_by, revoke_reason FROM sessions WHERE 1=1`
	var args []any
	if userID > 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if !includeInactive {
		query += ` AND revoked_at IS NULL AND expires_at > ?`
		args = append(args, sqliteNow())
	}
	query += ` ORDER BY created_at DESC LIMIT 1000`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRecord
	now := time.Now().UTC()
	for rows.Next() {
		var rec SessionRecord
		var key string
		if err := rows.Scan(&key, &rec.UserID, &rec.CreatedAt, &rec.CreatedIP, &rec.CreatedUserAgent, &rec.LastSeenAt, &rec.LastSeenIP,
			&rec.ExpiresAt, &rec.AuthState, &rec.RevokedAt, &rec.RevokedBy, &rec.RevokeReason); err != nil {
			return nil, err
		}
		rec.Ref = "session:" + strings.TrimPrefix(key, sessionKeyPrefix)[:16]
		exp, _ := parseTime(rec.ExpiresAt)
		rec.Active = rec.RevokedAt == "" && exp.After(now) && rec.AuthState == "active"
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RevokeAPIKey replaces hard deletion: the row stays as evidence.
func (s *Store) RevokeAPIKey(userID, keyID, actor int64, reason string) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, sqliteNow(), actor, reason, keyID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeUserAPIKeys revokes every live key (private, public client and
// delegated) owned by a user. Returns the count.
func (s *Store) RevokeUserAPIKeys(userID, actor int64, reason string) (int64, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at = ?, revoked_by = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL`, sqliteNow(), actor, reason, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// APIKeyRecord is the containment view of a key (no hash).
type APIKeyRecord struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	Name         string `json:"name"`
	KeyPrefix    string `json:"key_prefix"`
	Kind         string `json:"kind"`
	Access       string `json:"access"`
	ProjectID    string `json:"project_id,omitempty"`
	IssuerApp    string `json:"issuer_app,omitempty"`
	CreatedAt    string `json:"created_at"`
	CreatedIP    string `json:"created_ip,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	LastUsed     string `json:"last_used,omitempty"`
	LastUsedIP   string `json:"last_used_ip,omitempty"`
	RevokedAt    string `json:"revoked_at,omitempty"`
	RevokedBy    int64  `json:"revoked_by,omitempty"`
	RevokeReason string `json:"revoke_reason,omitempty"`
	Active       bool   `json:"active"`
}

func (s *Store) ListAPIKeyRecords(userID int64, includeInactive bool) ([]APIKeyRecord, error) {
	if s.apiKeyUsage != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.apiKeyUsage.flush(ctx)
		cancel()
	}
	query := `SELECT id, user_id, name, key_prefix, COALESCE(kind,'private'), COALESCE(access,'read_write'), COALESCE(project_id,''),
		COALESCE(issuer_app,''), COALESCE(created_at,''), created_ip, COALESCE(expires_at,''), COALESCE(last_used,''), COALESCE(last_used_ip,''),
		COALESCE(revoked_at,''), revoked_by, revoke_reason,
		(revoked_at IS NULL AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)) FROM api_keys WHERE 1=1`
	var args []any
	if userID > 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if !includeInactive {
		query += ` AND revoked_at IS NULL AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)`
	}
	query += ` ORDER BY id DESC LIMIT 5000`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		var k APIKeyRecord
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.Kind, &k.Access, &k.ProjectID, &k.IssuerApp, &k.CreatedAt,
			&k.CreatedIP, &k.ExpiresAt, &k.LastUsed, &k.LastUsedIP, &k.RevokedAt, &k.RevokedBy, &k.RevokeReason, &k.Active); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DisableUser blocks every credential of the user immediately and revokes
// live sessions. Keys are blocked through the disabled check (and revoked
// explicitly by containment), so re-enabling restores service keys.
func (s *Store) DisableUser(userID, actor int64, reason string) (sessions int64, err error) {
	res, err := s.db.Exec(`UPDATE users SET disabled_at = COALESCE(disabled_at, ?), disabled_by = ?, disabled_reason = ? WHERE id = ?`,
		sqliteNow(), actor, reason, userID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return s.RevokeUserSessions(userID, "", actor, "account disabled: "+reason)
}

func (s *Store) EnableUser(userID int64) error {
	res, err := s.db.Exec(`UPDATE users SET disabled_at = NULL, disabled_by = 0, disabled_reason = '' WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UserDisabled(userID int64) bool {
	var disabled sql.NullString
	if err := s.db.QueryRow(`SELECT disabled_at FROM users WHERE id = ?`, userID).Scan(&disabled); err != nil {
		return false
	}
	return disabled.Valid && disabled.String != ""
}

// MarkSessionReauthenticated records a fresh password (and MFA) confirmation
// on one session, for step-up checks on privileged operations. It is
// per-session so a stolen cookie elsewhere does not inherit the owner's
// recent confirmation.
func (s *Store) MarkSessionReauthenticated(token string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET reauth_at = ? WHERE token = ? AND revoked_at IS NULL`, at.UTC().Format("2006-01-02 15:04:05"), sessionKey(token))
	return err
}

func (s *Store) SessionReauthenticatedAt(token string) time.Time {
	var raw sql.NullString
	if s.db.QueryRow(`SELECT reauth_at FROM sessions WHERE token = ? AND revoked_at IS NULL`, sessionKey(token)).Scan(&raw) != nil || !raw.Valid {
		return time.Time{}
	}
	t, _ := parseTime(raw.String)
	return t
}
