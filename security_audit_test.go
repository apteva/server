package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func openTestAudit(t *testing.T, opts securityAuditOptions) *securityAuditLog {
	t.Helper()
	l, err := openSecurityAuditLog(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func flushAudit(t *testing.T, l *securityAuditLog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityAuditRedactsSecretsBeforeStorage(t *testing.T) {
	l := openTestAudit(t, securityAuditOptions{})
	l.Record(SecurityEvent{Category: "credential", Action: "test.redaction", Details: map[string]any{
		"password":      "hunter2hunter2",
		"api_key":       "sk-abcdefabcdefabcdef",
		"command":       "curl -H 'Authorization: Bearer abcdefghijklmnop' https://x && export TOKEN=s3cr3tvalue",
		"note":          "leaked sk-0123456789abcdef0123 in logs",
		"key_prefix":    "sk-0123",
		"nested":        map[string]any{"client_secret": "zzz", "ok": "fine"},
		"private_block": "-----BEGIN OPENSSH PRIVATE KEY-----\nabc",
	}})
	flushAudit(t, l)
	events, err := l.Query(SecurityEventQuery{Action: "test.redaction"})
	if err != nil || len(events) != 1 {
		t.Fatalf("query: %v %d", err, len(events))
	}
	raw, _ := json.Marshal(events[0])
	for _, leaked := range []string{"hunter2hunter2", "sk-abcdefabcdefabcdef", "abcdefghijklmnop", "s3cr3tvalue", "sk-0123456789abcdef0123", "zzz", "BEGIN OPENSSH PRIVATE KEY"} {
		if bytes.Contains(raw, []byte(leaked)) {
			t.Fatalf("stored event leaks %q: %s", leaked, raw)
		}
	}
	if events[0].Details["key_prefix"] != "sk-0123" {
		t.Fatalf("identifier key was over-redacted: %v", events[0].Details)
	}
	if nested, _ := events[0].Details["nested"].(map[string]any); nested["ok"] != "fine" {
		t.Fatalf("non-secret nested value lost: %v", events[0].Details)
	}
}

func TestSecurityAuditHashChainDetectsTampering(t *testing.T) {
	l := openTestAudit(t, securityAuditOptions{})
	for i := 0; i < 5; i++ {
		l.Record(SecurityEvent{Category: "auth", Action: "auth.login", PrincipalType: "user", PrincipalID: "1", SourceIP: "198.51.100.7",
			Details: map[string]any{"n": i}})
	}
	flushAudit(t, l)
	status, err := l.VerifyChain()
	if err != nil || !status.OK || status.Checked != 5 {
		t.Fatalf("fresh chain should verify: %+v %v", status, err)
	}
	if _, err := l.db.Exec(`UPDATE security_events SET source_ip = '10.0.0.1' WHERE seq = 3`); err != nil {
		t.Fatal(err)
	}
	status, _ = l.VerifyChain()
	if status.OK || status.BrokenAt != 3 {
		t.Fatalf("edited row not detected: %+v", status)
	}
	if _, err := l.db.Exec(`UPDATE security_events SET source_ip = '198.51.100.7' WHERE seq = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`DELETE FROM security_events WHERE seq = 4`); err != nil {
		t.Fatal(err)
	}
	status, _ = l.VerifyChain()
	if status.OK || status.BrokenAt != 5 {
		t.Fatalf("deleted row not detected: %+v", status)
	}
}

func TestSecurityAuditRecordNeverBlocksAndReportsDrops(t *testing.T) {
	// A log whose writer is not draining: Record must return immediately and
	// count what it could not queue.
	stalled := &securityAuditLog{queue: make(chan SecurityEvent, 2)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			stalled.Record(SecurityEvent{Action: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full queue")
	}
	if got := stalled.dropped.Load(); got != 48 {
		t.Fatalf("dropped=%d, want 48", got)
	}

	l := openTestAudit(t, securityAuditOptions{})
	l.startAlerting("", "")
	l.dropped.Store(7)
	l.pendingDrops.Store(7)
	l.Record(SecurityEvent{Category: "auth", Action: "auth.login"})
	flushAudit(t, l)
	drops, _ := l.Query(SecurityEventQuery{Action: "audit.events_dropped"})
	if len(drops) != 1 || drops[0].Severity != securitySeverityCritical {
		t.Fatalf("drop gap not written back: %+v", drops)
	}
	if h := l.Health(); !h.Degraded || h.DroppedTotal != 7 {
		t.Fatalf("health must flag drops: %+v", h)
	}
	alerts, _ := l.Alerts(true, 10)
	if len(alerts) != 1 || alerts[0].Rule != "audit_events_dropped" {
		t.Fatalf("logging failure must alert: %+v", alerts)
	}
}

func TestSecurityAlertsDedupeAndDeliverOnceWithSignature(t *testing.T) {
	var mu sync.Mutex
	deliveries := map[string]int{}
	var badSignature atomic.Bool
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("alert-secret"))
		mac.Write(body)
		if r.Header.Get("X-Apteva-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			badSignature.Store(true)
		}
		mu.Lock()
		deliveries[r.Header.Get("Idempotency-Key")]++
		mu.Unlock()
	}))
	defer hook.Close()
	dir := t.TempDir()
	open := func() *securityAuditLog {
		l, err := openSecurityAuditLog(dir, securityAuditOptions{})
		if err != nil {
			t.Fatal(err)
		}
		l.startAlerting(hook.URL, "alert-secret")
		return l
	}
	l := open()
	login := func(id, ip string) SecurityEvent {
		return SecurityEvent{EventID: id, Category: "auth", Action: "auth.login", Result: securityResultSuccess,
			PrincipalType: "user", PrincipalID: "42", TargetID: "ops@example.test", SourceIP: ip}
	}
	ctx := context.Background()
	if _, err := l.RecordBatch(ctx, []SecurityEvent{login("ev-1", "198.51.100.10")}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordBatch(ctx, []SecurityEvent{login("ev-2", "198.51.100.10")}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordBatch(ctx, []SecurityEvent{login("ev-3", "203.0.113.99")}); err != nil {
		t.Fatal(err)
	}
	// Replays of the same event (collector redelivery) insert nothing.
	if n, _ := l.RecordBatch(ctx, []SecurityEvent{login("ev-3", "203.0.113.99")}); n != 0 {
		t.Fatalf("replayed event inserted %d rows", n)
	}
	alerts, _ := l.Alerts(false, 10)
	if len(alerts) != 1 || alerts[0].Rule != "login_new_source" || alerts[0].EventID != "ev-3" {
		t.Fatalf("expected one new-source alert for ev-3: %+v", alerts)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return deliveries["ev-3:login_new_source"] == 1 })
	l.Close()
	// Restart: nothing is delivered twice.
	l2 := open()
	defer l2.Close()
	l2.signalAlerts()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if deliveries["ev-3:login_new_source"] != 1 || len(deliveries) != 1 {
		t.Fatalf("deliveries after restart: %v", deliveries)
	}
	if badSignature.Load() {
		t.Fatal("alert signature did not verify")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestSecurityAuditForwardsToIndependentDestinationExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	fail := atomic.Bool{}
	fail.Store(true)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer append-only-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Events []SecurityEvent `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, ev := range body.Events {
			seen[ev.EventID]++
		}
		mu.Unlock()
	}))
	defer dest.Close()
	dir := t.TempDir()
	opts := securityAuditOptions{ForwardURL: dest.URL, ForwardToken: "append-only-token", ForwardEvery: 20 * time.Millisecond}
	l, err := openSecurityAuditLog(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		l.Record(SecurityEvent{Category: "auth", Action: "auth.login"})
	}
	flushAudit(t, l)
	waitFor(t, func() bool { return l.Health().ForwardLastErr != "" })
	if h := l.Health(); !h.Degraded || h.ForwardLagCount != 3 {
		t.Fatalf("failing destination must degrade health with lag 3: %+v", h)
	}
	fail.Store(false)
	waitFor(t, func() bool { return l.Health().ForwardLagCount == 0 })
	l.Close()
	l2, err := openSecurityAuditLog(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	l2.Record(SecurityEvent{Category: "auth", Action: "auth.logout"})
	flushAudit(t, l2)
	waitFor(t, func() bool { return l2.Health().ForwardLagCount == 0 })
	l2.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("forwarded %d distinct events, want 4: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s forwarded %d times", id, n)
		}
	}
}

func TestPrivilegedMCPToolClassification(t *testing.T) {
	t.Setenv("APTEVA_SECURITY_AUDIT_MCP", "")
	for tool, want := range map[string]bool{
		"instance_run_command": true, "tenant_reveal_api_key": true, "files_delete": true, "deploy_release": true,
		"connections_rotate": true, "tenant_list": false, "tasks_create": false, "search_web": false, "keyboard_type": false,
	} {
		if got := mcpToolIsPrivileged(tool); got != want {
			t.Fatalf("%s: got %v want %v", tool, got, want)
		}
	}
	t.Setenv("APTEVA_SECURITY_AUDIT_MCP", "off")
	if mcpToolIsPrivileged("instance_run_command") {
		t.Fatal("off mode must not classify")
	}
}

// ─── end-to-end through the HTTP stack ────────────────────────────────────

type securityHarness struct {
	s       *Server
	h       http.Handler
	adminID int64
	userID  int64
}

const harnessPassword = "correct horse battery"

func newSecurityHarness(t *testing.T) *securityHarness {
	t.Helper()
	s := newTestServer(t)
	audit := openTestAudit(t, securityAuditOptions{})
	audit.startAlerting("", "")
	s.securityAudit = audit
	auditDeniedLimiter = &deniedLimiter{counts: map[string]int{}, dropped: map[string]int{}, limit: 30}
	hash, _ := bcrypt.GenerateFromPassword([]byte(harnessPassword), bcrypt.MinCost)
	admin, err := s.store.CreateUser("admin@example.test", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.store.SetPlatformRole(admin.ID, PlatformAdmin)
	user, err := s.store.CreateUser("user@example.test", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/auth/keys", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleCreateKey(w, r)
			return
		}
		s.handleListKeys(w, r)
	}))
	mux.HandleFunc("/auth/keys/", s.authMiddleware(s.handleDeleteKey))
	mux.HandleFunc("/whoami", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"user_id": getUserID(r)})
	}))
	mux.HandleFunc("/connections/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			s.handleGetConnectionCredentials(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	mux.HandleFunc("/admin/users/", s.authMiddleware(s.handleAdminUsers))
	s.registerSecurityRoutes(mux)
	return &securityHarness{s: s, h: s.securityAuditMiddleware(mux), adminID: admin.ID, userID: user.ID}
}

type harnessReq struct {
	method, path string
	body         any
	cookie       string
	bearer       string
	remote       string
	headers      map[string]string
}

func (h *securityHarness) do(t *testing.T, req harnessReq) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if req.body != nil {
		b, _ := json.Marshal(req.body)
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(req.method, req.path, body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if req.remote != "" {
		r.RemoteAddr = req.remote
	}
	if req.cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: req.cookie})
	}
	if req.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+req.bearer)
	}
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *securityHarness) login(t *testing.T, email, remote string) string {
	t.Helper()
	w := h.do(t, harnessReq{method: "POST", path: "/auth/login", body: map[string]string{"email": email, "password": harnessPassword}, remote: remote})
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", email, w.Code, w.Body.String())
	}
	token := getSessionCookie(w)
	if token == "" {
		t.Fatal("no session cookie")
	}
	return token
}

func (h *securityHarness) events(t *testing.T, q SecurityEventQuery) []SecurityEvent {
	t.Helper()
	flushAudit(t, h.s.securityAudit)
	events, err := h.s.securityAudit.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestSecurityLoginRecordsSourceAndHashesSessionTokens(t *testing.T) {
	h := newSecurityHarness(t)
	token := h.login(t, "user@example.test", "198.51.100.23:51000")
	var stored int
	_ = h.s.store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token = ?`, token).Scan(&stored)
	if stored != 0 {
		t.Fatal("raw session token is stored in the database")
	}
	recs, _ := h.s.store.ListSessionRecords(h.userID, false)
	if len(recs) != 1 || recs[0].CreatedIP != "198.51.100.23" {
		t.Fatalf("session source not recorded: %+v", recs)
	}
	logins := h.events(t, SecurityEventQuery{Action: "auth.login"})
	if len(logins) != 1 || logins[0].SourceIP != "198.51.100.23" || logins[0].CredentialID != recs[0].Ref || logins[0].PrincipalID != itoa(h.userID) {
		t.Fatalf("login event incomplete: %+v (session ref %s)", logins, recs[0].Ref)
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/login", body: map[string]string{"email": "user@example.test", "password": "wrong"}, remote: "198.51.100.24:1"}); w.Code != 401 {
		t.Fatalf("bad password: %d", w.Code)
	}
	failed := h.events(t, SecurityEventQuery{Action: "auth.login", Result: securityResultFailure})
	if len(failed) != 1 || failed[0].Details["reason"] != "bad_password" || failed[0].SourceIP != "198.51.100.24" {
		t.Fatalf("failed login not recorded: %+v", failed)
	}
}

func TestSecurityLegacyRawSessionTokensAreMigrated(t *testing.T) {
	s := newTestServer(t)
	u, _ := s.store.CreateUser("legacy@example.test", "x")
	raw := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := s.store.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`, raw, u.ID, time.Now().Add(time.Hour).UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if err := s.store.hashLegacySessionTokens(); err != nil {
		t.Fatal(err)
	}
	var plain int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token = ?`, raw).Scan(&plain)
	if plain != 0 {
		t.Fatal("legacy raw token still stored")
	}
	if uid, err := s.store.GetSession(raw); err != nil || uid != u.ID {
		t.Fatalf("migrated session must keep working: %d %v", uid, err)
	}
}

func TestSecurityForgedIdentityAndForwardingHeaders(t *testing.T) {
	h := newSecurityHarness(t)
	// A client cannot select a user by sending server-owned identity headers.
	w := h.do(t, harnessReq{method: "GET", path: "/whoami", remote: "203.0.113.50:4000", headers: map[string]string{
		"X-User-ID": itoa(h.adminID), "X-Apteva-Operator-ID": itoa(h.adminID), "X-Apteva-App-Install-ID": "1",
		"X-Forwarded-For": "10.9.9.9", "X-Real-IP": "10.9.9.9",
	}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged identity headers authenticated: %d %s", w.Code, w.Body.String())
	}
	denied := h.events(t, SecurityEventQuery{Category: "auth", Result: securityResultDenied})
	if len(denied) != 1 || denied[0].SourceIP != "203.0.113.50" {
		t.Fatalf("denial must record the real peer, not the forged forwarding header: %+v", denied)
	}
	// Forwarded headers from an untrusted peer never become the login source.
	h.login(t, "user@example.test", "203.0.113.51:4000")
	h2 := h.do(t, harnessReq{method: "POST", path: "/auth/login", remote: "203.0.113.52:4000",
		body: map[string]string{"email": "user@example.test", "password": harnessPassword}, headers: map[string]string{"X-Forwarded-For": "8.8.8.8"}})
	if h2.Code != 200 {
		t.Fatal(h2.Code)
	}
	logins := h.events(t, SecurityEventQuery{Action: "auth.login", Result: securityResultSuccess})
	for _, ev := range logins {
		if ev.SourceIP == "8.8.8.8" {
			t.Fatalf("forged X-Forwarded-For accepted from untrusted peer: %+v", ev)
		}
	}
	// A key belonging to one user cannot act as another through headers.
	raw := "sk-" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.userID, "k", HashAPIKey(raw), raw[:11]); err != nil {
		t.Fatal(err)
	}
	w = h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: raw, headers: map[string]string{"X-User-ID": itoa(h.adminID)}})
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || int64(got["user_id"].(float64)) != h.userID {
		t.Fatalf("API key principal overridden by header: %d %v", w.Code, got)
	}
}

func TestSecurityPrivateAPIKeysRecordSourceAndRevocationIsKept(t *testing.T) {
	h := newSecurityHarness(t)
	raw := "sk-" + generateToken(24)
	key, err := h.s.store.CreateAPIKey(h.userID, "ci", HashAPIKey(raw), raw[:11])
	if err != nil {
		t.Fatal(err)
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: raw, remote: "192.0.2.77:999"}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	recs, _ := h.s.store.ListAPIKeyRecords(h.userID, true)
	if len(recs) != 1 || recs[0].LastUsedIP != "192.0.2.77" {
		t.Fatalf("private key use must record its source: %+v", recs)
	}
	token := h.login(t, "user@example.test", "192.0.2.78:1")
	if w := h.do(t, harnessReq{method: "DELETE", path: "/auth/keys/" + itoa(key.ID), cookie: token}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: raw}); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", w.Code)
	}
	recs, _ = h.s.store.ListAPIKeyRecords(h.userID, true)
	if len(recs) != 1 || recs[0].RevokedAt == "" || recs[0].RevokedBy != h.userID || recs[0].Active {
		t.Fatalf("revocation record not preserved: %+v", recs)
	}
	revokes := h.events(t, SecurityEventQuery{Action: "api_key.revoke"})
	if len(revokes) != 1 || revokes[0].TargetID != itoa(key.ID) {
		t.Fatalf("revocation not audited: %+v", revokes)
	}
}

func TestSecurityDisablingAccountRejectsExistingCredentialsEverywhere(t *testing.T) {
	h := newSecurityHarness(t)
	userSession := h.login(t, "user@example.test", "198.51.100.1:1")
	raw := "sk-" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.userID, "k", HashAPIKey(raw), raw[:11]); err != nil {
		t.Fatal(err)
	}
	delegated := "uk_" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.userID, "d", HashAPIKey(delegated), delegated[:11], APIKeyCreateOptions{Kind: "delegated_user", Scopes: "[]"}); err != nil {
		t.Fatal(err)
	}
	for _, cred := range []harnessReq{{cookie: userSession}, {bearer: raw}} {
		cred.method, cred.path = "GET", "/whoami"
		if w := h.do(t, cred); w.Code != 200 {
			t.Fatalf("precondition: credential should work: %d", w.Code)
		}
	}
	admin := h.login(t, "admin@example.test", "198.51.100.2:1")
	w := h.do(t, harnessReq{method: "POST", path: "/security/users/" + itoa(h.userID) + "/disable", cookie: admin, body: map[string]string{"reason": "suspicious access"}})
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	for name, cred := range map[string]harnessReq{"session": {cookie: userSession}, "private key": {bearer: raw}} {
		cred.method, cred.path = "GET", "/whoami"
		if w := h.do(t, cred); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s still accepted after disable: %d", name, w.Code)
		}
	}
	if _, err := h.s.store.GetDelegatedUserAPIKey(HashAPIKey(delegated)); err == nil {
		t.Fatal("delegated key still resolves for a disabled account")
	}
	// App callbacks carrying the user's browser session are rejected too.
	if _, err := h.s.store.GetSession(userSession); err == nil {
		t.Fatal("app user-session validation still accepts the disabled account's session")
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/login", body: map[string]string{"email": "user@example.test", "password": harnessPassword}}); w.Code != http.StatusForbidden {
		t.Fatalf("disabled account can log in: %d", w.Code)
	}
	recs, _ := h.s.store.ListSessionRecords(h.userID, true)
	if len(recs) == 0 || recs[0].RevokedBy != h.adminID || !strings.Contains(recs[0].RevokeReason, "suspicious access") {
		t.Fatalf("session revocation record lacks actor/reason: %+v", recs)
	}
	ev := h.events(t, SecurityEventQuery{Action: "account.disable"})
	if len(ev) != 1 || ev[0].PrincipalID != itoa(h.adminID) || ev[0].TargetID != itoa(h.userID) {
		t.Fatalf("disable not attributed: %+v", ev)
	}
}

func TestSecurityPrivilegedOperationsRequireRecentReauth(t *testing.T) {
	h := newSecurityHarness(t)
	enc, _ := Encrypt(h.s.secret, `{"api_key":"provider-secret-value"}`)
	conn, err := h.s.store.CreateConnection(h.adminID, "openai", "OpenAI", "prod", "api_key", enc, "")
	if err != nil {
		t.Fatal(err)
	}
	admin := h.login(t, "admin@example.test", "198.51.100.9:1")
	path := "/connections/" + itoa(conn.ID) + "/credentials"
	if w := h.do(t, harnessReq{method: "GET", path: path, cookie: admin}); w.Code != 200 {
		t.Fatalf("fresh login should allow reveal: %d %s", w.Code, w.Body.String())
	}
	// Age the confirmation past the window.
	_, _ = h.s.store.db.Exec(`UPDATE sessions SET reauth_at = ? WHERE token = ?`, time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05"), sessionKey(admin))
	w := h.do(t, harnessReq{method: "GET", path: path, cookie: admin})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") {
		t.Fatalf("stale session revealed a secret: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/reauth", cookie: admin, body: map[string]string{"password": "nope"}}); w.Code != 401 {
		t.Fatalf("bad reauth accepted: %d", w.Code)
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/reauth", cookie: admin, body: map[string]string{"password": harnessPassword}}); w.Code != 200 {
		t.Fatalf("reauth: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(t, harnessReq{method: "GET", path: path, cookie: admin}); w.Code != 200 {
		t.Fatalf("reveal after reauth: %d", w.Code)
	}
	// API keys (and therefore agents/automation) cannot export secrets.
	raw := "sk-" + generateToken(24)
	_, _ = h.s.store.CreateAPIKey(h.adminID, "k", HashAPIKey(raw), raw[:11])
	if w := h.do(t, harnessReq{method: "GET", path: path, bearer: raw}); w.Code != http.StatusForbidden {
		t.Fatalf("API key exported a secret: %d", w.Code)
	}
	exports := h.events(t, SecurityEventQuery{Action: "credential.export", Result: securityResultSuccess})
	if len(exports) != 2 {
		t.Fatalf("each export must be audited: %d", len(exports))
	}
	raws, _ := json.Marshal(h.events(t, SecurityEventQuery{Limit: 500}))
	if bytes.Contains(raws, []byte("provider-secret-value")) || bytes.Contains(raws, []byte(raw)) || bytes.Contains(raws, []byte(admin)) {
		t.Fatal("audit trail contains a secret, key or session token")
	}
	denied := h.events(t, SecurityEventQuery{Category: "privileged", Result: securityResultDenied})
	if len(denied) != 2 {
		t.Fatalf("refused privileged attempts must be audited: %+v", denied)
	}
	alerts, _ := h.s.securityAudit.Alerts(false, 50)
	foundExport := false
	for _, a := range alerts {
		foundExport = foundExport || a.Rule == "credential_exported"
	}
	if !foundExport {
		t.Fatalf("export should alert: %+v", alerts)
	}
}

func TestSecurityContainmentRevokesAndReportsFleetGaps(t *testing.T) {
	h := newSecurityHarness(t)
	userA := h.login(t, "user@example.test", "198.51.100.30:1")
	userB := h.login(t, "user@example.test", "198.51.100.31:1")
	raw := "sk-" + generateToken(24)
	k1, _ := h.s.store.CreateAPIKey(h.userID, "a", HashAPIKey(raw), raw[:11])
	keep := "sk-" + generateToken(24)
	k2, _ := h.s.store.CreateAPIKey(h.userID, "service", HashAPIKey(keep), keep[:11])
	// A fake Fleet sidecar with two tenants.
	fleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"tenants": []map[string]any{
			{"id": 7, "slug": "acme", "base_url": "https://acme.example.test", "current_version": "0.62.0"},
			{"id": 9, "slug": "beta", "base_url": "https://beta.example.test", "current_version": "0.88.1"},
		}})
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}})
	}))
	defer fleet.Close()
	h.s.installedApps = NewInstalledAppsRegistry()
	h.s.installedApps.Add(&InstalledApp{InstallID: 5, AppName: "fleet", SidecarURL: fleet.URL, Token: "fleet-token"})

	admin := h.login(t, "admin@example.test", "198.51.100.32:1")
	report := h.do(t, harnessReq{method: "GET", path: "/security/containment?user_id=" + itoa(h.userID), cookie: admin})
	var view containmentReportView
	_ = json.Unmarshal(report.Body.Bytes(), &view)
	if len(view.Sessions) != 2 || len(view.APIKeys) != 2 || len(view.Fleet) != 2 || view.Fleet[0].Status != "unconfirmed" {
		t.Fatalf("containment view incomplete: %s", report.Body.String())
	}
	w := h.do(t, harnessReq{method: "POST", path: "/security/containment/revoke", cookie: admin, body: map[string]any{
		"user_id": h.userID, "sessions": true, "api_keys": true, "keep_key_ids": []int64{k2.ID}, "include_fleet": true, "reason": "incident 42"}})
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Complete bool                `json:"complete"`
		Counts   map[string]int      `json:"counts"`
		Results  []containmentTarget `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Complete || res.Counts["revoked"] != 3 || res.Counts["kept"] != 1 || res.Counts["unconfirmed"] != 2 {
		t.Fatalf("revocation must be explicit about partial coverage: %s", w.Body.String())
	}
	for _, tok := range []string{userA, userB} {
		if w := h.do(t, harnessReq{method: "GET", path: "/whoami", cookie: tok}); w.Code != 401 {
			t.Fatal("revoked session still works")
		}
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: raw}); w.Code != 401 {
		t.Fatal("revoked key still works")
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: keep}); w.Code != 200 {
		t.Fatal("kept key was revoked")
	}
	_ = k1
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", cookie: admin}); w.Code != 200 {
		t.Fatal("operator's own session must survive containment of another user")
	}
	ev := h.events(t, SecurityEventQuery{Action: "containment.revoke"})
	if len(ev) != 1 || ev[0].Result != securityResultFailure || ev[0].Details["complete"] != false {
		t.Fatalf("containment audit must record incompleteness: %+v", ev)
	}
}

func TestSecurityAPIIsAdminOnly(t *testing.T) {
	h := newSecurityHarness(t)
	user := h.login(t, "user@example.test", "198.51.100.40:1")
	for _, p := range []string{"/security/overview", "/security/events", "/security/containment", "/security/inventory"} {
		if w := h.do(t, harnessReq{method: "GET", path: p, cookie: user}); w.Code != http.StatusForbidden {
			t.Fatalf("%s reachable by non-admin: %d", p, w.Code)
		}
	}
	admin := h.login(t, "admin@example.test", "198.51.100.41:1")
	w := h.do(t, harnessReq{method: "GET", path: "/security/overview", cookie: admin})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "evidence_coverage") {
		t.Fatalf("overview: %d %s", w.Code, w.Body.String())
	}
}
