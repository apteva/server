package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Password reset links.
//
// An admin creates a one-time link for a user who forgot their password and
// passes it on; whoever opens it sets a new password. The server sends no
// email. The token travels in the URL fragment (/reset-password#<token>), so
// loading the page never sends it to the server or into access logs; the page
// posts it in a JSON body instead. Only a SHA-256 hash of the token is stored.
// A user has at most one live link: creating a new one replaces the old one,
// and using any link removes every link for that user.

const passwordResetLinkTTL = 24 * time.Hour

var errPasswordResetInvalid = errors.New("reset link is invalid or has expired")

// passwordResetLimiter bounds token guessing and reset attempts per IP. A
// token has 256 random bits, so this is about load, not about guessability.
var passwordResetLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}

func (s *Store) migratePasswordResetTokens() error {
	_, err := s.dbExec(`CREATE TABLE IF NOT EXISTS password_reset_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_by INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_user ON password_reset_tokens(user_id)`)
	return err
}

func passwordResetTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreatePasswordResetToken returns a new link token for the user, replacing
// any earlier one, and clears expired tokens.
func (s *Store) CreatePasswordResetToken(userID, createdBy int64, ttl time.Duration) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now().UTC()
	expires := now.Add(ttl)
	tx, err := s.dbBegin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM password_reset_tokens WHERE user_id = ? OR expires_at <= ?`,
		userID, now.Format(time.RFC3339)); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(`INSERT INTO password_reset_tokens (token_hash, user_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, passwordResetTokenHash(token), userID, createdBy,
		now.Format(time.RFC3339), expires.Format(time.RFC3339)); err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

const livePasswordResetTokenQuery = `SELECT t.user_id, u.email, t.expires_at, u.disabled_at
	FROM password_reset_tokens t JOIN users u ON u.id = t.user_id
	WHERE t.token_hash = ?`

// scanLivePasswordResetToken reads a livePasswordResetTokenQuery row. Expired
// tokens, unknown tokens and tokens of disabled accounts are all invalid.
func scanLivePasswordResetToken(row interface{ Scan(...any) error }) (int64, string, time.Time, error) {
	var userID int64
	var email, expiresRaw string
	var disabled sql.NullString
	err := row.Scan(&userID, &email, &expiresRaw, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", time.Time{}, errPasswordResetInvalid
	}
	if err != nil {
		return 0, "", time.Time{}, err
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || !time.Now().Before(expires) || disabled.Valid {
		return 0, "", time.Time{}, errPasswordResetInvalid
	}
	return userID, email, expires, nil
}

// PasswordResetTarget returns the account a live token belongs to.
func (s *Store) PasswordResetTarget(ctx context.Context, token string) (int64, string, time.Time, error) {
	if strings.TrimSpace(token) == "" {
		return 0, "", time.Time{}, errPasswordResetInvalid
	}
	return scanLivePasswordResetToken(s.dbQueryRowContext(ctx, livePasswordResetTokenQuery, passwordResetTokenHash(token)))
}

// ConsumePasswordResetToken sets the new password and removes every link for
// that user in one transaction, so a link works once. The caller revokes the
// user's sessions afterwards.
func (s *Store) ConsumePasswordResetToken(ctx context.Context, token, newHash string) (int64, string, error) {
	if strings.TrimSpace(token) == "" {
		return 0, "", errPasswordResetInvalid
	}
	tx, err := s.dbBeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	userID, email, _, err := scanLivePasswordResetToken(tx.QueryRowContext(ctx, livePasswordResetTokenQuery, passwordResetTokenHash(token)))
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(`DELETE FROM password_reset_tokens WHERE user_id = ?`, userID); err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, newHash, userID); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return userID, email, nil
}

// DeletePasswordResetTokensForUser removes any pending link, used when the
// password changes some other way. A database from before this table existed
// has nothing to remove.
func (s *Store) DeletePasswordResetTokensForUser(userID int64) error {
	_, err := s.dbExec(`DELETE FROM password_reset_tokens WHERE user_id = ?`, userID)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

// POST /users/:id/password-reset-link — admin creates a one-time link for a
// user. Like other account-takeover-capable actions it needs a recently
// confirmed dashboard session.
func (s *Server) handleUserPasswordResetLink(w http.ResponseWriter, r *http.Request, caller, targetID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.isAdmin(caller) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !s.requirePrivilegedAuth(w, r, "account.password_reset_link") {
		return
	}
	user, err := s.store.GetUserByIDContext(r.Context(), targetID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if s.store.UserDisabled(targetID) {
		http.Error(w, "account is disabled; enable it before creating a reset link", http.StatusConflict)
		return
	}
	token, expires, err := s.store.CreatePasswordResetToken(targetID, caller, passwordResetLinkTTL)
	if err != nil {
		http.Error(w, "could not create reset link", http.StatusInternalServerError)
		return
	}
	ev := s.securityEventFromRequest(r, "credential", "auth.password_reset_link", securityResultSuccess)
	ev.Severity = securitySeverityWarning
	ev.TargetType, ev.TargetID = "account", user.Email
	ev.Details = map[string]any{"user_id": targetID, "expires_at": expires.Format(time.RFC3339)}
	s.recordSecurity(r, ev)
	log.Printf("[USERS] admin=%d created a password reset link for user=%d", caller, targetID)
	writeJSON(w, map[string]any{
		"token":      token,
		"url":        s.publicBaseURL() + "/reset-password#" + token,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// POST /auth/password-reset/check {token} — who the link is for and when it
// expires, so the page can show it before asking for a new password.
func (s *Server) handlePasswordResetCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.allowPasswordResetAttempt(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	_, email, expires, err := s.store.PasswordResetTarget(r.Context(), body.Token)
	if err != nil {
		s.passwordResetFailed(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"email": email, "expires_at": expires.Format(time.RFC3339)})
}

// POST /auth/password-reset {token, new_password} — set the new password,
// end every session of that account, and spend the link. It does not sign
// the caller in, so an account with MFA still needs its second factor.
func (s *Server) handlePasswordReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.allowPasswordResetAttempt(w, r) {
		return
	}
	var body struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(body.NewPassword) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	userID, email, err := s.store.ConsumePasswordResetToken(r.Context(), body.Token, string(hash))
	if err != nil {
		s.passwordResetFailed(w, r, err)
		return
	}
	if err := s.store.DeleteSessionsForUser(userID); err != nil {
		log.Printf("[AUTH] password reset by link for user=%d but session sweep failed: %v", userID, err)
	}
	s.closeUserDeviceLinks(userID, "account sessions revoked")
	ev := s.securityEventFromRequest(r, "credential", "auth.password_reset", securityResultSuccess)
	ev.Severity = securitySeverityWarning
	ev.TargetType, ev.TargetID = "account", email
	ev.Details = map[string]any{"user_id": userID, "method": "link"}
	s.recordSecurity(r, ev)
	writeJSON(w, map[string]any{"status": "ok", "email": email})
}

func (s *Server) allowPasswordResetAttempt(w http.ResponseWriter, r *http.Request) bool {
	if passwordResetLimiter.allow(clientIP(r), 10, time.Minute) {
		return true
	}
	ev := s.securityEventFromRequest(r, "auth", "auth.password_reset", securityResultDenied)
	ev.Details = map[string]any{"reason": "rate_limited"}
	s.recordSecurity(r, ev)
	http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
	return false
}

func (s *Server) passwordResetFailed(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, errPasswordResetInvalid) {
		log.Printf("[AUTH] password reset lookup failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ev := s.securityEventFromRequest(r, "auth", "auth.password_reset", securityResultFailure)
	ev.Details = map[string]any{"reason": "invalid_or_expired_link"}
	s.recordSecurity(r, ev)
	http.Error(w, "This reset link is invalid, already used, or expired. Ask an admin for a new one.", http.StatusGone)
}
