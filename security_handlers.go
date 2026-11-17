package main

// security_handlers.go — admin-only Security API, step-up authorization for
// privileged operations, account disable/enable and incident containment.
//
//   POST /auth/reauth                          confirm password (+MFA) on this session
//   GET  /auth/sessions, DELETE /auth/sessions/<ref>   a user's own sessions
//   GET  /security/overview                    health, alerts, coverage, exposure
//   GET  /security/events                      timeline (filters: category, action, principal_*, source_ip, result, since, before_seq, limit)
//   GET  /security/alerts, POST /security/alerts/<id>/ack
//   GET  /security/chain                       tamper check of the trail
//   GET  /security/inventory                   management exposure + security config of this server and Fleet tenants
//   GET  /security/containment                 every live credential (optionally ?user_id=)
//   POST /security/containment/revoke          revoke, with explicit per-target results
//   POST /security/users/<id>/disable|enable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func privilegedReauthWindow() time.Duration {
	if m, err := strconv.Atoi(strings.TrimSpace(os.Getenv("APTEVA_SECURITY_REAUTH_MINUTES"))); err == nil && m >= 1 && m <= 240 {
		return time.Duration(m) * time.Minute
	}
	return 15 * time.Minute
}

// requestCredential reports how the current request authenticated:
// ("session", rawToken) for a dashboard session, ("api_key", "") for a
// private key, or another principal kind.
func (s *Server) requestCredential(r *http.Request) (kind, sessionToken string) {
	if ra := requestAuditFrom(r); ra != nil {
		ra.mu.Lock()
		cred, ptype := ra.credentialID, ra.principalType
		ra.mu.Unlock()
		switch {
		case strings.HasPrefix(cred, "session:"):
			if c, err := r.Cookie(cookieName); err == nil {
				return "session", c.Value
			}
			return "session", ""
		case strings.HasPrefix(cred, "api_key:") && ptype == "user":
			return "api_key", ""
		case ptype != "":
			return ptype, ""
		}
	}
	// Direct handler calls (tests, internal) carry no audit context.
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if uid, err := s.store.GetSession(c.Value); err == nil && uid == getUserID(r) {
			return "session", c.Value
		}
	}
	return "unknown", ""
}

// containmentKeyScope lets a private API key call incident containment
// (POST /security/containment/revoke) without an interactive session. It
// unlocks that one action only; every other privileged operation still
// needs a reauthenticated dashboard session. Fleet mints its tenant control
// keys with this scope so it can contain a tenant during an incident.
const containmentKeyScope = "security.containment"

// privilegedActionKeyScopes maps the privileged actions an API key may
// perform when it carries the listed scope.
var privilegedActionKeyScopes = map[string]string{
	"containment.revoke": containmentKeyScope,
}

// privilegedKeyScopesIn returns the privileged key scopes (values of
// privilegedActionKeyScopes) named anywhere in a requested scopes document.
func privilegedKeyScopesIn(raw json.RawMessage) []string {
	privileged := map[string]bool{}
	for _, scope := range privilegedActionKeyScopes {
		privileged[scope] = true
	}
	var doc any
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if name := strings.TrimSpace(t); privileged[name] && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(doc)
	sort.Strings(out)
	return out
}

// requestAPIKeyID returns the id of the private API key that authenticated
// this request, or 0.
func requestAPIKeyID(r *http.Request) int64 {
	ra := requestAuditFrom(r)
	if ra == nil {
		return 0
	}
	ra.mu.Lock()
	cred := ra.credentialID
	ra.mu.Unlock()
	if !strings.HasPrefix(cred, "api_key:") {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(cred, "api_key:"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// apiKeyHasScope reports whether an active (unrevoked, unexpired) key
// carries scope in its scopes JSON array.
func (s *Server) apiKeyHasScope(keyID int64, scope string) bool {
	if keyID <= 0 || scope == "" {
		return false
	}
	var raw string
	err := s.store.db.QueryRow(`SELECT COALESCE(scopes,'[]') FROM api_keys
		WHERE id=? AND revoked_at IS NULL AND COALESCE(kind,'private')='private' AND (expires_at IS NULL OR expires_at = '' OR datetime(expires_at) > CURRENT_TIMESTAMP)`, keyID).Scan(&raw)
	if err != nil {
		return false
	}
	var scopes []string
	if json.Unmarshal([]byte(raw), &scopes) != nil {
		return false
	}
	for _, have := range scopes {
		if strings.TrimSpace(have) == scope {
			return true
		}
	}
	return false
}

// requirePrivilegedAuth gates dangerous operations: the caller must be an
// interactive dashboard session that confirmed its password (and MFA, when
// enrolled) within privilegedReauthWindow. App tokens, agents, the platform
// gateway and API keys are refused, except private keys when the operator
// sets APTEVA_SECURITY_ALLOW_KEY_PRIVILEGED=1 for automation, and private
// keys carrying the scope privilegedActionKeyScopes lists for this action.
func (s *Server) requirePrivilegedAuth(w http.ResponseWriter, r *http.Request, action string) bool {
	return s.requirePrivilegedAuthWithScope(w, r, action, privilegedActionKeyScopes[action])
}

// requirePrivilegedAuthWithScope is requirePrivilegedAuth with the key scope
// that may stand in for step-up named explicitly ("" = none). Key creation
// uses it to gate granting a privileged scope: only a caller that could
// itself use the scope may mint a key carrying it.
func (s *Server) requirePrivilegedAuthWithScope(w http.ResponseWriter, r *http.Request, action, keyScope string) bool {
	kind, token := s.requestCredential(r)
	allowed := false
	reason := ""
	switch kind {
	case "session":
		at := s.store.SessionReauthenticatedAt(token)
		allowed = !at.IsZero() && time.Since(at) <= privilegedReauthWindow()
		if !allowed {
			reason = "reauthentication_required"
		}
	case "api_key":
		allowed = envTruthy(os.Getenv("APTEVA_SECURITY_ALLOW_KEY_PRIVILEGED"))
		if !allowed {
			if keyScope != "" && s.apiKeyHasScope(requestAPIKeyID(r), keyScope) {
				allowed = true
			}
		}
		if !allowed {
			reason = "interactive_session_required"
		}
	default:
		reason = "interactive_session_required"
	}
	if allowed {
		return true
	}
	ev := s.securityEventFromRequest(r, "privileged", action, securityResultDenied)
	ev.Details = map[string]any{"reason": reason, "credential_kind": kind}
	if keyScope != "" {
		ev.Details["key_scope"] = keyScope
	}
	s.recordSecurity(r, ev)
	body := map[string]any{
		"error":          reason,
		"message":        "This operation requires recent password confirmation from a dashboard session.",
		"reauth_url":     "/api/auth/reauth",
		"window_minutes": int(privilegedReauthWindow() / time.Minute),
	}
	if keyScope != "" && kind == "api_key" {
		body["required_scope"] = keyScope
	}
	writeJSONStatus(w, http.StatusForbidden, body)
	return false
}

// POST /auth/reauth {password, code?}
func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	kind, token := s.requestCredential(r)
	if kind != "session" || token == "" {
		http.Error(w, "interactive session required", http.StatusUnauthorized)
		return
	}
	uid := getUserID(r)
	if !mfaLimiter.allow(fmt.Sprintf("reauth:%d:%s", uid, clientIP(r)), 5, 5*time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUserByID(uid)
	fail := func(reason string) {
		ev := s.securityEventFromRequest(r, "auth", "auth.reauth", securityResultFailure)
		ev.Details = map[string]any{"reason": reason}
		s.recordSecurity(r, ev)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		fail("bad_password")
		return
	}
	if userMFAEnabled(user) {
		if _, _, err := s.verifyUserMFACode(user, body.Code); err != nil {
			fail("bad_mfa_code")
			return
		}
	}
	if err := s.store.MarkSessionReauthenticated(token, time.Now()); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.recordSecurity(r, s.securityEventFromRequest(r, "auth", "auth.reauth", securityResultSuccess))
	writeJSON(w, map[string]any{"status": "ok", "valid_for_minutes": int(privilegedReauthWindow() / time.Minute)})
}

// GET /auth/sessions, DELETE /auth/sessions/<ref>
func (s *Server) handleOwnSessions(w http.ResponseWriter, r *http.Request) {
	uid := getUserID(r)
	ref := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/auth/sessions"), "/")
	switch {
	case r.Method == http.MethodGet && ref == "":
		records, err := s.store.ListSessionRecords(uid, false)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_, current := s.requestCredential(r)
		currentRef := credentialRef("session", current)
		out := make([]map[string]any, 0, len(records))
		for _, rec := range records {
			b, _ := json.Marshal(rec)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			m["current"] = rec.Ref == currentRef
			out = append(out, m)
		}
		writeJSON(w, out)
	case r.Method == http.MethodDelete && ref != "":
		ok, err := s.store.RevokeSessionByRef(uid, ref, uid, "revoked by owner")
		ev := s.securityEventFromRequest(r, "credential", "session.revoke", securityResultSuccess)
		ev.TargetType, ev.TargetID = "session", ref
		if err != nil || !ok {
			ev.Result = securityResultFailure
			s.recordSecurity(r, ev)
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		s.recordSecurity(r, ev)
		writeJSON(w, map[string]string{"status": "revoked"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) registerSecurityRoutes(apiMux *http.ServeMux) {
	apiMux.HandleFunc("/auth/reauth", s.authMiddleware(s.handleReauth))
	apiMux.HandleFunc("/auth/sessions", s.authMiddleware(s.handleOwnSessions))
	apiMux.HandleFunc("/auth/sessions/", s.authMiddleware(s.handleOwnSessions))
	apiMux.HandleFunc("/security/", s.authMiddleware(s.handleSecurityAPI))
}

func (s *Server) handleSecurityAPI(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requirePlatformAdmin(w, r)
	if !ok {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/security/"), "/")
	parts := strings.Split(path, "/")
	switch {
	case path == "overview" && r.Method == http.MethodGet:
		writeJSON(w, s.securityOverview(r.Context()))
	case path == "events" && r.Method == http.MethodGet:
		s.handleSecurityEvents(w, r)
	case path == "alerts" && r.Method == http.MethodGet:
		alerts, err := s.securityAudit.Alerts(r.URL.Query().Get("open") == "1", atoiDefault(r.URL.Query().Get("limit"), 200))
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if alerts == nil {
			alerts = []SecurityAlert{}
		}
		writeJSON(w, alerts)
	case len(parts) == 3 && parts[0] == "alerts" && parts[2] == "ack" && r.Method == http.MethodPost:
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		done, err := s.securityAudit.AcknowledgeAlert(id, caller)
		if err != nil || !done {
			http.Error(w, "alert not found or already acknowledged", http.StatusNotFound)
			return
		}
		ev := s.securityEventFromRequest(r, "config", "security.alert_ack", securityResultSuccess)
		ev.TargetType, ev.TargetID = "alert", parts[1]
		s.recordSecurity(r, ev)
		writeJSON(w, map[string]string{"status": "acknowledged"})
	case path == "chain" && r.Method == http.MethodGet:
		status, err := s.securityAudit.VerifyChain()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, status)
	case path == "inventory" && r.Method == http.MethodGet:
		writeJSON(w, s.securityInventory(r.Context()))
	case path == "containment" && r.Method == http.MethodGet:
		userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
		writeJSON(w, s.containmentReport(r.Context(), userID))
	case path == "containment/revoke" && r.Method == http.MethodPost:
		s.handleContainmentRevoke(w, r, caller)
	case len(parts) == 3 && parts[0] == "users" && (parts[2] == "disable" || parts[2] == "enable") && r.Method == http.MethodPost:
		s.handleUserDisable(w, r, caller, parts[1], parts[2] == "disable")
	case path == "host" || strings.HasPrefix(path, "host/"):
		s.handleSecurityHost(w, r, strings.TrimPrefix(strings.TrimPrefix(path, "host"), "/"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func atoiDefault(raw string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return n
	}
	return def
}

func (s *Server) handleSecurityEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := SecurityEventQuery{Category: q.Get("category"), Action: q.Get("action"), PrincipalType: q.Get("principal_type"),
		PrincipalID: q.Get("principal_id"), SourceIP: q.Get("source_ip"), Result: q.Get("result"), Limit: atoiDefault(q.Get("limit"), 200)}
	query.BeforeSeq, _ = strconv.ParseInt(q.Get("before_seq"), 10, 64)
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			query.Since = t
		}
	}
	events, err := s.securityAudit.Query(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if events == nil {
		events = []SecurityEvent{}
	}
	writeJSON(w, events)
}

// ─── accounts ─────────────────────────────────────────────────────────────

func (s *Server) handleUserDisable(w http.ResponseWriter, r *http.Request, caller int64, idRaw string, disable bool) {
	action := "account.enable"
	if disable {
		action = "account.disable"
	}
	if !s.requirePrivilegedAuth(w, r, action) {
		return
	}
	target, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || target <= 0 {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	body.Reason = truncateAuditString(strings.TrimSpace(body.Reason), 300)
	ev := s.securityEventFromRequest(r, "permission", action, securityResultSuccess)
	ev.TargetType, ev.TargetID = "user", idRaw
	ev.Severity = securitySeverityWarning
	if disable {
		if target == caller {
			http.Error(w, "you cannot disable your own account", http.StatusBadRequest)
			return
		}
		if s.store.GetPlatformRole(target) == PlatformAdmin && s.activeAdminCount() <= 1 {
			http.Error(w, "cannot disable the last active admin", http.StatusConflict)
			return
		}
		revoked, err := s.store.DisableUser(target, caller, body.Reason)
		if err != nil {
			ev.Result = securityResultError
			s.recordSecurity(r, ev)
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.stopUserAgentsForContainment(target)
		ev.Details = map[string]any{"reason": body.Reason, "sessions_revoked": revoked}
		s.recordSecurity(r, ev)
		writeJSON(w, map[string]any{"status": "disabled", "sessions_revoked": revoked,
			"note": "Sessions, API keys, delegated keys and app user-sessions of this account are rejected immediately."})
		return
	}
	if err := s.store.EnableUser(target); err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	ev.Details = map[string]any{"reason": body.Reason}
	s.recordSecurity(r, ev)
	writeJSON(w, map[string]string{"status": "enabled"})
}

func (s *Server) activeAdminCount() int {
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE COALESCE(role,'user') = 'admin' AND disabled_at IS NULL`).Scan(&n)
	return n
}

// stopUserAgentsForContainment stops running cores owned by a disabled user
// so a core API key cannot keep acting for them. Best effort.
func (s *Server) stopUserAgentsForContainment(userID int64) {
	if s.agents == nil {
		return
	}
	rows, err := s.store.db.Query(`SELECT id FROM agents WHERE user_id = ?`, userID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.agents.Stop(id)
	}
}

// ─── containment ──────────────────────────────────────────────────────────

type containmentTarget struct {
	Scope  string `json:"scope"`
	Target string `json:"target"`
	Status string `json:"status"` // revoked | disabled_locally | already_inactive | kept | unsupported | unconfirmed | error
	Detail string `json:"detail,omitempty"`
}

type fleetTenantCoverage struct {
	TenantID string `json:"tenant_id"`
	Slug     string `json:"slug,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Version  string `json:"version,omitempty"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

type containmentReportView struct {
	GeneratedAt string                `json:"generated_at"`
	UserID      int64                 `json:"user_id,omitempty"`
	Sessions    []SessionRecord       `json:"sessions"`
	APIKeys     []APIKeyRecord        `json:"api_keys"`
	AppInstalls []map[string]any      `json:"app_installs"`
	Fleet       []fleetTenantCoverage `json:"fleet_tenants"`
	FleetError  string                `json:"fleet_error,omitempty"`
	Gaps        []string              `json:"coverage_gaps"`
}

func (s *Server) containmentReport(ctx context.Context, userID int64) containmentReportView {
	view := containmentReportView{GeneratedAt: time.Now().UTC().Format(time.RFC3339), UserID: userID,
		Sessions: []SessionRecord{}, APIKeys: []APIKeyRecord{}, AppInstalls: []map[string]any{}, Fleet: []fleetTenantCoverage{}}
	if recs, err := s.store.ListSessionRecords(userID, false); err == nil && recs != nil {
		view.Sessions = recs
	}
	if keys, err := s.store.ListAPIKeyRecords(userID, false); err == nil && keys != nil {
		view.APIKeys = keys
	}
	if s.installedApps != nil {
		for _, app := range s.installedApps.List() {
			view.AppInstalls = append(view.AppInstalls, map[string]any{"install_id": app.InstallID, "app": app.AppName,
				"project_id": app.ProjectID, "has_token": app.Token != ""})
		}
	}
	view.Fleet, view.FleetError = s.fleetTenantCoverage(ctx)
	view.Gaps = append(view.Gaps,
		"App install tokens are rotated by reinstalling/upgrading the app; they are listed, not revoked, here.",
		"Sessions and tokens issued by the Auth app (end-user identities) are revoked in the Auth app.")
	if view.FleetError != "" {
		view.Gaps = append(view.Gaps, "Fleet tenants could not be enumerated: "+view.FleetError)
	}
	for _, t := range view.Fleet {
		if t.Status != "covered" {
			view.Gaps = append(view.Gaps, fmt.Sprintf("Fleet tenant %s: %s", firstNonEmpty(t.Slug, t.TenantID), t.Detail))
		}
	}
	return view
}

// callInstalledAppTool invokes a tool on a locally installed app sidecar with
// that install's own token (server-internal; used for Fleet coverage).
func (s *Server) callInstalledAppTool(ctx context.Context, appName, tool string, args map[string]any) (json.RawMessage, error) {
	if s.installedApps == nil {
		return nil, errors.New("apps not loaded")
	}
	app := s.installedApps.GetByName(appName)
	if app == nil || app.SidecarURL == "" {
		return nil, fmt.Errorf("%s app is not installed or not running", appName)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(app.SidecarURL, "/")+"/mcp", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if app.Token != "" {
		req.Header.Set("Authorization", "Bearer "+app.Token)
	}
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(app.InstallID, 10))
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for len(raw) < 4<<20 {
		n, rerr := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d", appName, tool, resp.StatusCode)
	}
	if appErr := appCallProtocolError(raw); appErr != nil {
		return nil, fmt.Errorf("%s %s: %s", appName, tool, appErr.Message)
	}
	return json.RawMessage(unwrapAppCallResult(raw)), nil
}

// fleetTenantCoverage lists Fleet tenants and states, per tenant, whether
// containment can reach them. This server does not drive tenant containment
// itself (Fleet's tenant_contain / tenant_contain_all do), so every tenant is
// reported as unconfirmed rather than silently treated as covered.
func (s *Server) fleetTenantCoverage(ctx context.Context) ([]fleetTenantCoverage, string) {
	if s.installedApps == nil || s.installedApps.GetByName("fleet") == nil {
		return []fleetTenantCoverage{}, ""
	}
	raw, err := s.callInstalledAppTool(ctx, "fleet", "tenant_list", map[string]any{})
	if err != nil {
		return []fleetTenantCoverage{}, err.Error()
	}
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	var items []any
	switch t := decoded.(type) {
	case []any:
		items = t
	case map[string]any:
		for _, key := range []string{"tenants", "items", "data"} {
			if list, ok := t[key].([]any); ok {
				items = list
				break
			}
		}
	}
	out := []fleetTenantCoverage{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		t := fleetTenantCoverage{TenantID: fmt.Sprint(firstPresent(m, "id", "tenant_id")), Slug: fmt.Sprint(firstPresent(m, "slug", "name")),
			BaseURL: fmt.Sprint(firstPresent(m, "base_url", "public_url", "url")), Version: fmt.Sprint(firstPresent(m, "current_version", "version", "apteva_version")),
			Status: "unconfirmed", Detail: "Not contained from here: run Fleet's tenant_contain (or tenant_contain_all), which rotates Fleet's control key and revokes sessions and keys on the tenant, then confirm here."}
		for _, f := range []*string{&t.TenantID, &t.Slug, &t.BaseURL, &t.Version} {
			if *f == "<nil>" {
				*f = ""
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, ""
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && fmt.Sprint(v) != "" {
			return v
		}
	}
	return ""
}

type containmentRequest struct {
	UserID             int64   `json:"user_id"`
	Sessions           bool    `json:"sessions"`
	APIKeys            bool    `json:"api_keys"`
	DisableUser        bool    `json:"disable_user"`
	KeepCurrentSession *bool   `json:"keep_current_session"`
	KeepKeyIDs         []int64 `json:"keep_key_ids"`
	Reason             string  `json:"reason"`
	IncludeFleet       bool    `json:"include_fleet"`
	ConfirmAllUsers    bool    `json:"confirm_all_users"`
}

// POST /security/containment/revoke
func (s *Server) handleContainmentRevoke(w http.ResponseWriter, r *http.Request, caller int64) {
	if !s.requirePrivilegedAuth(w, r, "containment.revoke") {
		return
	}
	var req containmentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Reason = truncateAuditString(strings.TrimSpace(req.Reason), 300)
	if req.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	if req.UserID == 0 && !req.ConfirmAllUsers {
		http.Error(w, "user_id is required (or confirm_all_users=true for every account)", http.StatusBadRequest)
		return
	}
	if req.DisableUser && (req.UserID == 0 || req.UserID == caller) {
		http.Error(w, "disable_user needs a specific user other than yourself", http.StatusBadRequest)
		return
	}
	keepCurrent := req.KeepCurrentSession == nil || *req.KeepCurrentSession
	_, currentToken := s.requestCredential(r)
	keep := map[int64]bool{}
	for _, id := range req.KeepKeyIDs {
		keep[id] = true
	}
	var results []containmentTarget
	complete := true
	add := func(t containmentTarget) {
		if t.Status == "error" || t.Status == "unsupported" || t.Status == "unconfirmed" {
			complete = false
		}
		results = append(results, t)
	}

	if req.DisableUser {
		if _, err := s.store.DisableUser(req.UserID, caller, req.Reason); err != nil {
			add(containmentTarget{Scope: "account", Target: strconv.FormatInt(req.UserID, 10), Status: "error", Detail: err.Error()})
		} else {
			s.stopUserAgentsForContainment(req.UserID)
			add(containmentTarget{Scope: "account", Target: strconv.FormatInt(req.UserID, 10), Status: "disabled_locally"})
		}
	}
	if req.Sessions {
		recs, err := s.store.ListSessionRecords(req.UserID, false)
		if err != nil {
			add(containmentTarget{Scope: "sessions", Target: "*", Status: "error", Detail: err.Error()})
		}
		currentRef := credentialRef("session", currentToken)
		for _, rec := range recs {
			if keepCurrent && rec.Ref == currentRef {
				add(containmentTarget{Scope: "session", Target: rec.Ref, Status: "kept", Detail: "current session"})
				continue
			}
			ok, err := s.store.RevokeSessionByRef(rec.UserID, rec.Ref, caller, "containment: "+req.Reason)
			switch {
			case err != nil:
				add(containmentTarget{Scope: "session", Target: rec.Ref, Status: "error", Detail: err.Error()})
			case ok:
				add(containmentTarget{Scope: "session", Target: rec.Ref, Status: "revoked"})
			default:
				add(containmentTarget{Scope: "session", Target: rec.Ref, Status: "already_inactive"})
			}
		}
	}
	if req.APIKeys {
		keys, err := s.store.ListAPIKeyRecords(req.UserID, false)
		if err != nil {
			add(containmentTarget{Scope: "api_keys", Target: "*", Status: "error", Detail: err.Error()})
		}
		for _, k := range keys {
			target := fmt.Sprintf("api_key:%d (%s)", k.ID, k.KeyPrefix)
			if keep[k.ID] {
				add(containmentTarget{Scope: "api_key", Target: target, Status: "kept"})
				continue
			}
			ok, err := s.store.RevokeAPIKey(k.UserID, k.ID, caller, "containment: "+req.Reason)
			switch {
			case err != nil:
				add(containmentTarget{Scope: "api_key", Target: target, Status: "error", Detail: err.Error()})
			case ok:
				add(containmentTarget{Scope: "api_key", Target: target, Status: "revoked"})
			default:
				add(containmentTarget{Scope: "api_key", Target: target, Status: "already_inactive"})
			}
		}
	}
	var fleetErr string
	if req.IncludeFleet {
		tenants, ferr := s.fleetTenantCoverage(r.Context())
		fleetErr = ferr
		if ferr != "" {
			add(containmentTarget{Scope: "fleet", Target: "*", Status: "error", Detail: ferr})
		}
		for _, t := range tenants {
			add(containmentTarget{Scope: "fleet_tenant", Target: firstNonEmpty(t.Slug, t.TenantID), Status: "unconfirmed", Detail: t.Detail})
		}
	}
	counts := map[string]int{}
	for _, t := range results {
		counts[t.Status]++
	}
	result := securityResultSuccess
	if !complete {
		result = securityResultFailure
	}
	ev := s.securityEventFromRequest(r, "containment", "containment.revoke", result)
	ev.TargetType, ev.TargetID = "user", strconv.FormatInt(req.UserID, 10)
	ev.Severity = securitySeverityWarning
	ev.Details = map[string]any{"reason": req.Reason, "counts": counts, "complete": complete, "include_fleet": req.IncludeFleet, "fleet_error": fleetErr}
	s.recordSecurity(r, ev)
	writeJSON(w, map[string]any{"complete": complete, "counts": counts, "results": results})
}

// ─── inventory / exposure ─────────────────────────────────────────────────

type listenerExposure struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Loopback bool   `json:"loopback"`
	Public   bool   `json:"public"`
	Note     string `json:"note,omitempty"`
}

func addressIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// managementListenerAddress reports where the management API is reachable:
// the address main actually bound when known, otherwise the address main
// would choose from the same environment (APTEVA_HOST_BACKEND_ADDR, then
// APTEVA_BIND, then 0.0.0.0 for a root service install, else loopback).
func (s *Server) managementListenerAddress() string {
	if s.managementListenAddr != "" {
		return s.managementListenAddr
	}
	if backend := strings.TrimSpace(os.Getenv("APTEVA_HOST_BACKEND_ADDR")); backend != "" {
		return backend
	}
	bind := strings.TrimSpace(os.Getenv("APTEVA_BIND"))
	if bind == "" {
		bind = "127.0.0.1"
		if os.Getenv("APTEVA_HOME") != "" && os.Geteuid() == 0 {
			bind = "0.0.0.0"
		}
	}
	return net.JoinHostPort(bind, s.port)
}

func (s *Server) securityInventory(ctx context.Context) map[string]any {
	var listeners []listenerExposure
	mgmt := s.managementListenerAddress()
	listeners = append(listeners, listenerExposure{Name: "management", Address: mgmt, Loopback: addressIsLoopback(mgmt), Public: !addressIsLoopback(mgmt)})
	for _, env := range []string{"APTEVA_HTTP_LISTEN_ADDR", "APTEVA_HTTPS_LISTEN_ADDR", "APTEVA_TLS_LISTEN_ADDR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			listeners = append(listeners, listenerExposure{Name: strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(env, "APTEVA_"), "_LISTEN_ADDR")) + " ingress",
				Address: v, Loopback: addressIsLoopback(v), Public: !addressIsLoopback(v), Note: "intended public ingress"})
		}
	}
	var admins, adminsWithMFA, disabled, activeKeys, activeSessions int
	_ = s.store.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN COALESCE(mfa_type,'') != '' THEN 1 ELSE 0 END),0) FROM users WHERE COALESCE(role,'user') = 'admin' AND disabled_at IS NULL`).Scan(&admins, &adminsWithMFA)
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE disabled_at IS NOT NULL`).Scan(&disabled)
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND (expires_at IS NULL OR datetime(expires_at) > CURRENT_TIMESTAMP)`).Scan(&activeKeys)
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL AND expires_at > ?`, sqliteNow()).Scan(&activeSessions)
	childMode := firstNonEmpty(strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_CHILD_ENV_MODE"))), "allowlist")
	tenants, fleetErr := s.fleetTenantCoverage(ctx)
	findings := []string{}
	for _, l := range listeners {
		if l.Name == "management" && l.Public {
			findings = append(findings, "Management API listens on a non-loopback address ("+l.Address+"); bind it to 127.0.0.1 behind the ingress.")
		}
	}
	if admins > adminsWithMFA {
		findings = append(findings, fmt.Sprintf("%d of %d admin accounts have no MFA.", admins-adminsWithMFA, admins))
	}
	if childMode == "inherit" {
		findings = append(findings, "APTEVA_CHILD_ENV_MODE=inherit passes the server environment to apps.")
	}
	if mode := appIsolationMode(); mode == "off" {
		findings = append(findings, "App processes run as the server's own OS user (APTEVA_APP_ISOLATION is off).")
	}
	if os.Getenv("SERVER_SECRET") != "" {
		findings = append(findings, "SERVER_SECRET is set in the environment; it is withheld from apps, but prefer the 0600 .secret file.")
	}
	if strings.TrimSpace(os.Getenv("APTEVA_SECURITY_FORWARD_URL")) == "" {
		findings = append(findings, "Security events are not forwarded to an independent destination (APTEVA_SECURITY_FORWARD_URL).")
	}
	if strings.TrimSpace(os.Getenv("APTEVA_SECURITY_ALERT_WEBHOOK")) == "" {
		findings = append(findings, "No alert webhook is configured (APTEVA_SECURITY_ALERT_WEBHOOK).")
	}
	if len(tenants) > 0 {
		findings = append(findings, fmt.Sprintf("%d Fleet tenant(s) are outside automatic containment coverage; review their keys and versions.", len(tenants)))
	}
	return map[string]any{
		"server": map[string]any{"version": Version, "listeners": listeners, "hosted": os.Getenv("APTEVA_HOST_SOCKET") != "",
			"registration_mode": s.regMode, "child_env_mode": childMode, "app_isolation": appIsolationMode(),
			"trusted_proxy_cidrs": os.Getenv("APTEVA_TRUSTED_PROXY_CIDRS"), "trust_all_proxy_headers": envTruthy(os.Getenv("APTEVA_TRUST_PROXY_HEADERS")),
			"admins": admins, "admins_with_mfa": adminsWithMFA, "disabled_users": disabled, "active_api_keys": activeKeys,
			"active_sessions": activeSessions, "privileged_reauth_minutes": int(privilegedReauthWindow() / time.Minute)},
		"fleet_tenants": tenants,
		"fleet_error":   fleetErr,
		"findings":      findings,
	}
}

func (s *Server) securityOverview(ctx context.Context) map[string]any {
	health := s.securityAudit.Health()
	open, _ := s.securityAudit.Alerts(true, 1000)
	bySeverity := map[string]int{}
	for _, a := range open {
		bySeverity[a.Severity]++
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	recentDenied, _ := s.securityAudit.Query(SecurityEventQuery{Result: securityResultDenied, Since: since, Limit: 1000})
	recentLogins, _ := s.securityAudit.Query(SecurityEventQuery{Action: "auth.login", Since: since, Limit: 1000})
	return map[string]any{
		"audit":             health,
		"open_alerts":       len(open),
		"open_by_severity":  bySeverity,
		"denied_24h":        len(recentDenied),
		"login_events_24h":  len(recentLogins),
		"host":              s.securityHostStatus(ctx),
		"inventory":         s.securityInventory(ctx),
		"evidence_coverage": securityEvidenceCoverage(),
	}
}

// securityEvidenceCoverage states plainly what each evidence source proves,
// so the page never implies command-level evidence that does not exist.
func securityEvidenceCoverage() []map[string]string {
	return []map[string]string{
		{"source": "Platform audit trail", "proves": "Dashboard/API authentication, credential and permission changes, exports, deployments, privileged MCP tool calls, containment actions.", "does_not_prove": "Actions taken directly on the host outside Apteva."},
		{"source": "Host SSH collector", "proves": "SSH logins (user, source, method, key fingerprint), session open/close, authorized_keys changes.", "does_not_prove": "Commands executed inside an SSH session; that needs auditd or session recording."},
		{"source": "Provider consoles", "proves": "Out-of-band access (rescue mode, web console).", "does_not_prove": "Nothing is collected from them by Apteva."},
	}
}
