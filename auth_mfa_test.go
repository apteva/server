package main

import (
	"bytes"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mfaJSONRequest(t *testing.T, method, path string, body any, cookie string) *http.Request {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	return req
}

func callAuthedMFA(t *testing.T, s *Server, handler http.HandlerFunc, body any, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := mfaJSONRequest(t, http.MethodPost, "/", body, cookie)
	w := httptest.NewRecorder()
	s.authMiddleware(handler)(w, req)
	return w
}

func registerAndLoginCookie(t *testing.T, s *Server) string {
	t.Helper()
	w := postJSON(t, s.handleRegister, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	w = postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	return getSessionCookie(w)
}

func enrollMFA(t *testing.T, s *Server, session string) (secret string, recovery []string) {
	t.Helper()
	w := callAuthedMFA(t, s, s.handleMFAEnroll, map[string]string{
		"current_password": "password123",
	}, session)
	if w.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	secret, _ = body["secret"].(string)
	if secret == "" || !strings.HasPrefix(body["otpauth_uri"].(string), "otpauth://totp/") {
		t.Fatalf("bad enrollment response: %s", w.Body.String())
	}
	code, err := totpCode(secret, time.Now().UTC().Unix()/totpPeriodSeconds)
	if err != nil {
		t.Fatal(err)
	}
	w = callAuthedMFA(t, s, s.handleMFAConfirm, map[string]string{"code": code}, session)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	body = decodeJSON(t, w)
	for _, value := range body["recovery_codes"].([]any) {
		recovery = append(recovery, value.(string))
	}
	if len(recovery) != recoveryCodeCount {
		t.Fatalf("recovery codes=%d, want %d", len(recovery), recoveryCodeCount)
	}
	return secret, recovery
}

func TestTOTPMatchesHOTPReferenceCounter(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	got, err := totpCode(secret, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "287082" {
		t.Fatalf("code=%q, want RFC HOTP value 287082", got)
	}
}

func TestMFAEnrollmentAndTwoStageLogin(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)

	wrong := callAuthedMFA(t, s, s.handleMFAEnroll, map[string]string{
		"current_password": "wrong-password",
	}, session)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d", wrong.Code)
	}

	secret, recovery := enrollMFA(t, s, session)
	user, err := s.store.GetUserByEmail("mfa@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if !userMFAEnabled(user) || user.MFASecretEncrypted == secret {
		t.Fatalf("MFA secret was not enabled and encrypted")
	}
	if strings.Contains(user.MFARecoveryHashes, normalizeRecoveryCode(recovery[0])) {
		t.Fatal("recovery code was stored in plaintext")
	}

	login := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	if login.Code != http.StatusOK || decodeJSON(t, login)["mfa_required"] != true {
		t.Fatalf("login did not require MFA: %d %s", login.Code, login.Body.String())
	}
	pending := getSessionCookie(login)
	if pending == "" {
		t.Fatal("missing pending MFA cookie")
	}

	protected := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: pending})
	s.authMiddleware(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(protected, req)
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("pending session reached protected route: %d", protected.Code)
	}

	bad := httptest.NewRecorder()
	s.handleMFAVerify(bad, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": "bad-code"}, pending))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad MFA code status=%d", bad.Code)
	}

	// Enrollment consumed the current time step. The accepted +1 skew step
	// proves login without sleeping while still exercising replay tracking.
	nextCode, err := totpCode(secret, time.Now().UTC().Unix()/totpPeriodSeconds+1)
	if err != nil {
		t.Fatal(err)
	}
	verified := httptest.NewRecorder()
	s.handleMFAVerify(verified, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": nextCode}, pending))
	if verified.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", verified.Code, verified.Body.String())
	}
	if _, err := s.store.GetSession(pending); err != nil {
		t.Fatalf("verified session is not active: %v", err)
	}

	// A TOTP time step is single-use even across separate login challenges.
	replayLogin := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	replayPending := getSessionCookie(replayLogin)
	replay := httptest.NewRecorder()
	s.handleMFAVerify(replay, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": nextCode}, replayPending))
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed TOTP status=%d", replay.Code)
	}
}

func TestMFARecoveryCodeIsSingleUse(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)
	_, recovery := enrollMFA(t, s, session)

	login := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	pending := getSessionCookie(login)
	verified := httptest.NewRecorder()
	s.handleMFAVerify(verified, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": recovery[0]}, pending))
	if verified.Code != http.StatusOK {
		t.Fatalf("recovery verify: %d %s", verified.Code, verified.Body.String())
	}
	if got := int(decodeJSON(t, verified)["recovery_codes_remaining"].(float64)); got != recoveryCodeCount-1 {
		t.Fatalf("remaining=%d", got)
	}

	login = postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	pending = getSessionCookie(login)
	reused := httptest.NewRecorder()
	s.handleMFAVerify(reused, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": recovery[0]}, pending))
	if reused.Code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code status=%d", reused.Code)
	}
}

func TestMFARecoveryCodeDoesNotDependOnDecryptingTOTPSecret(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)
	_, recovery := enrollMFA(t, s, session)
	user, err := s.store.GetUserByEmail("mfa@test.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec("UPDATE users SET mfa_secret_encrypted='unreadable' WHERE id=?", user.ID); err != nil {
		t.Fatal(err)
	}

	login := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	pending := getSessionCookie(login)
	verified := httptest.NewRecorder()
	s.handleMFAVerify(verified, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": recovery[0]}, pending))
	if verified.Code != http.StatusOK {
		t.Fatalf("recovery verify with unreadable TOTP secret: %d %s", verified.Code, verified.Body.String())
	}
}

func TestMFAChallengeLocksAfterMaximumAttempts(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)
	_, _ = enrollMFA(t, s, session)

	login := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	pending := getSessionCookie(login)
	for i := 0; i < mfaMaxAttempts; i++ {
		w := httptest.NewRecorder()
		s.handleMFAVerify(w, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": "000000"}, pending))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d", i+1, w.Code)
		}
	}
	if _, _, err := s.store.GetPendingMFASession(pending); err == nil {
		t.Fatal("MFA challenge survived the maximum number of attempts")
	}
}

func TestMFARecoveryRegenerationAndDisable(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)
	_, original := enrollMFA(t, s, session)

	regenerated := callAuthedMFA(t, s, s.handleMFARecoveryCodes, map[string]string{
		"current_password": "password123",
		"code":             original[0],
	}, session)
	if regenerated.Code != http.StatusOK {
		t.Fatalf("regenerate: %d %s", regenerated.Code, regenerated.Body.String())
	}
	var replacement []string
	for _, value := range decodeJSON(t, regenerated)["recovery_codes"].([]any) {
		replacement = append(replacement, value.(string))
	}
	if len(replacement) != recoveryCodeCount {
		t.Fatalf("replacement recovery codes=%d", len(replacement))
	}

	login := postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	pending := getSessionCookie(login)
	oldCode := httptest.NewRecorder()
	s.handleMFAVerify(oldCode, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": original[1]}, pending))
	if oldCode.Code != http.StatusUnauthorized {
		t.Fatalf("old recovery code survived regeneration: %d", oldCode.Code)
	}
	newCode := httptest.NewRecorder()
	s.handleMFAVerify(newCode, mfaJSONRequest(t, http.MethodPost, "/auth/mfa/verify", map[string]string{"code": replacement[0]}, pending))
	if newCode.Code != http.StatusOK {
		t.Fatalf("replacement recovery code: %d %s", newCode.Code, newCode.Body.String())
	}

	disabled := callAuthedMFA(t, s, s.handleMFADisable, map[string]string{
		"current_password": "password123",
		"code":             replacement[1],
	}, pending)
	if disabled.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", disabled.Code, disabled.Body.String())
	}
	login = postJSON(t, s.handleLogin, map[string]string{
		"email": "mfa@test.com", "password": "password123",
	})
	if login.Code != http.StatusOK || decodeJSON(t, login)["mfa_required"] != false {
		t.Fatalf("login still requires MFA after disable: %d %s", login.Code, login.Body.String())
	}
}

func TestMFAKeepsAPIKeysIndependentAndManagementInteractive(t *testing.T) {
	s := newTestServer(t)
	session := registerAndLoginCookie(t, s)
	_, _ = enrollMFA(t, s, session)
	user, err := s.store.GetUserByEmail("mfa@test.com")
	if err != nil {
		t.Fatal(err)
	}
	rawKey := "apt_test_mfa_api_key"
	if _, err := s.store.CreateAPIKey(user.ID, "MFA test", HashAPIKey(rawKey), "apt_test"); err != nil {
		t.Fatal(err)
	}

	protected := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	s.authMiddleware(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(protected, req)
	if protected.Code != http.StatusNoContent {
		t.Fatalf("MFA unexpectedly blocked API-key auth: %d", protected.Code)
	}

	enroll := httptest.NewRecorder()
	req = mfaJSONRequest(t, http.MethodPost, "/auth/mfa/enroll", map[string]string{"current_password": "password123"}, "")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	s.authMiddleware(s.handleMFAEnroll)(enroll, req)
	if enroll.Code != http.StatusUnauthorized {
		t.Fatalf("API key managed MFA without an interactive session: %d", enroll.Code)
	}
}

func TestMFAMigrationsUseExistingTables(t *testing.T) {
	s := newTestServer(t)
	for table := range map[string]bool{"auth_factors": true, "mfa_challenges": true} {
		var count int
		if err := s.store.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("unexpected MFA table %q", table)
		}
	}
	for table, column := range map[string]string{
		"users": "mfa_secret_encrypted", "sessions": "auth_state",
	} {
		if !columnExists(s.store.db, table, column) {
			t.Fatalf("missing %s.%s", table, column)
		}
	}
}

func TestMFAMigrationKeepsLegacySessionsActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			expires_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email, password_hash) VALUES (1, 'legacy@test.com', 'hash');
		INSERT INTO sessions (token, user_id, expires_at) VALUES ('legacy-session', 1, ?);
	`, expires); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if userID, err := store.GetSession("legacy-session"); err != nil || userID != 1 {
		t.Fatalf("legacy session was not preserved as active: user=%d err=%v", userID, err)
	}
	for table, column := range map[string]string{
		"users": "mfa_type", "sessions": "auth_state",
	} {
		if !columnExists(store.db, table, column) {
			t.Fatalf("missing migrated %s.%s", table, column)
		}
	}
}
