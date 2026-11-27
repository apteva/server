package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// newPasswordResetHarness is the security harness with the user routes and
// the public reset routes mounted.
func newPasswordResetHarness(t *testing.T) *securityHarness {
	t.Helper()
	h := newSecurityHarness(t)
	passwordResetLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}
	s := h.s
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/password", s.authMiddleware(s.handleChangePassword))
	mux.HandleFunc("/auth/password-reset", s.handlePasswordReset)
	mux.HandleFunc("/auth/password-reset/check", s.handlePasswordResetCheck)
	mux.HandleFunc("/users/", s.authMiddleware(s.handleUserByID))
	mux.HandleFunc("/whoami", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"user_id": getUserID(r)})
	}))
	h.h = s.securityAuditMiddleware(mux)
	return h
}

func createResetLink(t *testing.T, h *securityHarness, admin string, userID int64) (token string, url string) {
	t.Helper()
	w := h.do(t, harnessReq{method: "POST", path: "/users/" + itoa(userID) + "/password-reset-link", cookie: admin})
	if w.Code != http.StatusOK {
		t.Fatalf("create reset link: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil || expires.Sub(time.Now()) < 23*time.Hour || expires.Sub(time.Now()) > 25*time.Hour {
		t.Fatalf("expires_at=%q, want about 24h from now", out.ExpiresAt)
	}
	if out.Token == "" || !strings.HasSuffix(out.URL, "/reset-password#"+out.Token) {
		t.Fatalf("token=%q url=%q", out.Token, out.URL)
	}
	return out.Token, out.URL
}

func TestPasswordResetLinkSetsPasswordOnceAndEndsSessions(t *testing.T) {
	h := newPasswordResetHarness(t)
	admin := h.login(t, "admin@example.test", "198.51.100.10:1")
	userSession := h.login(t, "user@example.test", "198.51.100.11:1")
	token, _ := createResetLink(t, h, admin, h.userID)

	var stored int
	_ = h.s.store.db.QueryRow(`SELECT COUNT(*) FROM password_reset_tokens WHERE token_hash = ?`, token).Scan(&stored)
	if stored != 0 {
		t.Fatal("raw reset token is stored in the database")
	}

	check := h.do(t, harnessReq{method: "POST", path: "/auth/password-reset/check", body: map[string]string{"token": token}})
	if check.Code != http.StatusOK || !strings.Contains(check.Body.String(), `"email":"user@example.test"`) {
		t.Fatalf("check: %d %s", check.Code, check.Body.String())
	}
	short := h.do(t, harnessReq{method: "POST", path: "/auth/password-reset", body: map[string]string{"token": token, "new_password": "short"}})
	if short.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d %s", short.Code, short.Body.String())
	}
	reset := h.do(t, harnessReq{method: "POST", path: "/auth/password-reset", body: map[string]string{"token": token, "new_password": "a brand new password"}})
	if reset.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", reset.Code, reset.Body.String())
	}
	if getSessionCookie(reset) != "" {
		t.Fatal("reset signed the caller in; it must leave sign-in (and MFA) to the login page")
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", cookie: userSession}); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session still works after reset: %d", w.Code)
	}
	u, _ := h.s.store.GetUserByEmail("user@example.test")
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("a brand new password")) != nil {
		t.Fatal("new password not stored")
	}
	again := h.do(t, harnessReq{method: "POST", path: "/auth/password-reset", body: map[string]string{"token": token, "new_password": "another new password"}})
	if again.Code != http.StatusGone {
		t.Fatalf("link worked twice: %d %s", again.Code, again.Body.String())
	}

	created := h.events(t, SecurityEventQuery{Action: "auth.password_reset_link", Result: securityResultSuccess})
	if len(created) != 1 || created[0].PrincipalID != itoa(h.adminID) || created[0].TargetID != "user@example.test" {
		t.Fatalf("link creation not attributed: %+v", created)
	}
	used := h.events(t, SecurityEventQuery{Action: "auth.password_reset", Result: securityResultSuccess})
	if len(used) != 1 || used[0].TargetID != "user@example.test" {
		t.Fatalf("reset not recorded: %+v", used)
	}
	raw, _ := json.Marshal(append(created, used...))
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("security events contain the reset token")
	}
}

func TestPasswordResetLinkNeedsAdminWithRecentConfirmation(t *testing.T) {
	h := newPasswordResetHarness(t)
	user := h.login(t, "user@example.test", "198.51.100.12:1")
	if w := h.do(t, harnessReq{method: "POST", path: "/users/" + itoa(h.adminID) + "/password-reset-link", cookie: user}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin created a reset link: %d %s", w.Code, w.Body.String())
	}
	admin := h.login(t, "admin@example.test", "198.51.100.13:1")
	_, _ = h.s.store.db.Exec(`UPDATE sessions SET reauth_at = ? WHERE token = ?`, time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05"), sessionKey(admin))
	w := h.do(t, harnessReq{method: "POST", path: "/users/" + itoa(h.userID) + "/password-reset-link", cookie: admin})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") {
		t.Fatalf("stale admin session created a reset link: %d %s", w.Code, w.Body.String())
	}
	var n int
	_ = h.s.store.db.QueryRow(`SELECT COUNT(*) FROM password_reset_tokens`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d reset links exist after refused requests", n)
	}
}

func TestPasswordResetLinkIsReplacedExpiresAndStopsForDisabledAccounts(t *testing.T) {
	h := newPasswordResetHarness(t)
	admin := h.login(t, "admin@example.test", "198.51.100.14:1")
	checkCode := func(token string) int {
		return h.do(t, harnessReq{method: "POST", path: "/auth/password-reset/check", body: map[string]string{"token": token}}).Code
	}

	first, _ := createResetLink(t, h, admin, h.userID)
	second, _ := createResetLink(t, h, admin, h.userID)
	if checkCode(first) != http.StatusGone || checkCode(second) != http.StatusOK {
		t.Fatal("a new link must replace the earlier one")
	}
	_, _ = h.s.store.db.Exec(`UPDATE password_reset_tokens SET expires_at = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	if checkCode(second) != http.StatusGone {
		t.Fatal("expired link still accepted")
	}

	third, _ := createResetLink(t, h, admin, h.userID)
	_, _ = h.s.store.db.Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, sqliteNow(), h.userID)
	if checkCode(third) != http.StatusGone {
		t.Fatal("link of a disabled account still accepted")
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/users/" + itoa(h.userID) + "/password-reset-link", cookie: admin}); w.Code != http.StatusConflict {
		t.Fatalf("created a link for a disabled account: %d %s", w.Code, w.Body.String())
	}
	for _, token := range []string{"", "not-a-real-token"} {
		if checkCode(token) != http.StatusGone {
			t.Fatalf("token %q accepted", token)
		}
	}
}

func TestOtherPasswordChangesCancelPendingResetLinks(t *testing.T) {
	h := newPasswordResetHarness(t)
	admin := h.login(t, "admin@example.test", "198.51.100.15:1")
	checkCode := func(token string) int {
		return h.do(t, harnessReq{method: "POST", path: "/auth/password-reset/check", body: map[string]string{"token": token}}).Code
	}

	viaAdmin, _ := createResetLink(t, h, admin, h.userID)
	if w := h.do(t, harnessReq{method: "PATCH", path: "/users/" + itoa(h.userID) + "/password", cookie: admin, body: map[string]string{"new_password": "set by an admin"}}); w.Code != http.StatusOK {
		t.Fatalf("admin reset: %d %s", w.Code, w.Body.String())
	}
	if checkCode(viaAdmin) != http.StatusGone {
		t.Fatal("link survived an admin password reset")
	}

	viaSelf, _ := createResetLink(t, h, admin, h.adminID)
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/password", cookie: admin, body: map[string]string{"current_password": harnessPassword, "new_password": "changed by myself"}}); w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if checkCode(viaSelf) != http.StatusGone {
		t.Fatal("link survived a password change")
	}
}

func TestResetPasswordModeWorksFromTheDatabaseFile(t *testing.T) {
	s := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("forgotten password"), bcrypt.MinCost)
	admin, err := s.store.CreateUser("Owner@Example.test", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.store.SetPlatformRole(admin.ID, PlatformAdmin)
	if err := s.store.CreateSession("session-token", admin.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	link, _, err := s.store.CreatePasswordResetToken(admin.ID, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := s.store.path

	var out bytes.Buffer
	if err := runResetPassword(dbPath, "owner@example.TEST", strings.NewReader("remembered now\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Owner@Example.test") {
		t.Fatalf("output %q does not name the account", out.String())
	}
	u, _ := s.store.GetUserByEmail("Owner@Example.test")
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("remembered now")) != nil {
		t.Fatal("new password not stored")
	}
	if _, err := s.store.GetSession("session-token"); err == nil {
		t.Fatal("existing session still valid after reset")
	}
	if _, _, _, err := s.store.PasswordResetTarget(t.Context(), link); err == nil {
		t.Fatal("pending reset link survived the reset")
	}

	err = runResetPassword(dbPath, "nobody@example.test", strings.NewReader("whatever123\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "admin accounts: Owner@Example.test") {
		t.Fatalf("unknown email: %v", err)
	}
	if err := runResetPassword(dbPath, "owner@example.test", strings.NewReader("short\n"), &out); err == nil {
		t.Fatal("short password accepted")
	}
	if err := runResetPassword(dbPath, "owner@example.test", strings.NewReader(""), &out); err == nil {
		t.Fatal("empty stdin accepted")
	}
	missing := dbPath + ".missing"
	if err := runResetPassword(missing, "owner@example.test", strings.NewReader("long enough\n"), &out); err == nil || !strings.Contains(err.Error(), "no Apteva database") {
		t.Fatalf("missing database: %v", err)
	}
}
