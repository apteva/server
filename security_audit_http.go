package main

// security_audit_http.go — attributes HTTP requests and MCP tool calls to a
// principal and records the security-relevant ones.
//
// securityAuditMiddleware wraps the /api mux. It allocates a requestAudit in
// the request context; authMiddleware fills in who authenticated and with
// which credential (it is the only code that knows), handlers may add a
// semantic action, and after the response the middleware records:
//
//   - every 401/403 (rate-limited per source so scanners cannot flood it);
//   - every mutating request to a security-sensitive route family
//     (credentials, permissions, deployments, exports, security settings);
//   - privileged MCP tool calls proxied to apps.
//
// Ordinary product traffic is not recorded here; that is telemetry's job.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestAuditKey struct{}

type requestAudit struct {
	mu            sync.Mutex
	correlationID string
	principalType string
	principalID   string
	credentialID  string
	installID     int64
	projectID     string
	presented     string // kind of credential presented but rejected
	action        string // semantic action recorded by a handler, suppresses the generic record
	mcpTool       string
	mcpApp        string
	skip          bool
}

func requestAuditFrom(r *http.Request) *requestAudit {
	if r == nil {
		return nil
	}
	ra, _ := r.Context().Value(requestAuditKey{}).(*requestAudit)
	return ra
}

// noteAuditPrincipal is called by authMiddleware once a credential validates.
func noteAuditPrincipal(r *http.Request, principalType, principalID, credentialID string, installID int64) {
	ra := requestAuditFrom(r)
	if ra == nil {
		return
	}
	ra.mu.Lock()
	ra.principalType, ra.principalID, ra.credentialID, ra.installID = principalType, principalID, credentialID, installID
	ra.mu.Unlock()
}

func noteAuditPresentedCredential(r *http.Request, token string) {
	ra := requestAuditFrom(r)
	if ra == nil || token == "" {
		return
	}
	ra.mu.Lock()
	ra.presented = credentialKind(token)
	ra.mu.Unlock()
}

// noteAuditHandled marks the request as already recorded semantically.
func noteAuditHandled(r *http.Request, action string) {
	if ra := requestAuditFrom(r); ra != nil {
		ra.mu.Lock()
		ra.action = action
		ra.mu.Unlock()
	}
}

func noteAuditMCPTool(r *http.Request, app, tool, projectID string) {
	if ra := requestAuditFrom(r); ra != nil {
		ra.mu.Lock()
		ra.mcpApp, ra.mcpTool = app, tool
		if projectID != "" {
			ra.projectID = projectID
		}
		ra.mu.Unlock()
	}
}

func requestCorrelationID(r *http.Request) string {
	if ra := requestAuditFrom(r); ra != nil {
		return ra.correlationID
	}
	return ""
}

// credentialKind names a presented credential's family without its value.
func credentialKind(token string) string {
	switch {
	case strings.HasPrefix(token, "sk-"):
		return "api_key"
	case strings.HasPrefix(token, "uk_"):
		return "delegated_key"
	case strings.HasPrefix(token, "core_"):
		return "core_key"
	case strings.HasPrefix(token, "nd_"):
		return "device_token"
	case strings.HasPrefix(token, "dev-"), strings.HasPrefix(token, "apt_"), strings.HasPrefix(token, "app_"):
		return "app_token"
	case token == "":
		return ""
	default:
		return "unknown_bearer"
	}
}

var correlationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,80}$`)

// statusRecorder keeps streaming/hijacking working for wrapped handlers.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// securityRouteCategory classifies route families whose mutations are always
// security-relevant. Paths are as seen inside the /api mux (prefix stripped).
func securityRouteCategory(path string) string {
	p := strings.ToLower(path)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(p, part) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(p, "/auth/"), strings.HasPrefix(p, "/security/"):
		if strings.HasPrefix(p, "/security/") {
			return "config"
		}
		return "auth"
	case has("/keys", "/api-keys", "/credentials", "/secrets", "/oauth", "/connections", "/provider", "/mfa", "/delegated", "/tokens"):
		return "credential"
	case has("/members", "/roles", "/admin/users", "/grants", "/project-grants", "/invites", "/permissions", "/access-policy", "/platform-role"):
		return "permission"
	case has("/backup", "/export", "/restore", "/snapshot", "/recovery", "/migrate", "/transfer"):
		return "export"
	case has("/platform-update", "/fleet-platform-update", "/apps/install", "/upgrade", "/uninstall", "/deployments", "/releases", "/runtime"):
		return "deployment"
	case has("/https", "/cors", "/ingress", "/listeners", "/trusted", "/settings", "/domains", "/tls", "/certificates"):
		return "config"
	}
	return ""
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// Tool names whose effect warrants an audit record on every call. Matching
// is on name segments, deterministic, and extendable via
// APTEVA_SECURITY_AUDIT_MCP_TOOLS (comma separated substrings).
var privilegedMCPToolWords = []string{"run_command", "exec", "shell", "ssh", "sudo", "root", "destroy", "delete", "revoke",
	"rotate", "export", "backup", "restore", "secret", "credential", "password", "token", "key", "grant", "permission",
	"role", "deploy", "release", "install", "uninstall", "upload_file", "download", "firewall", "dns", "domain", "cert",
	"migrate", "transfer", "admin", "impersonate", "reveal", "env"}

func mcpToolIsPrivileged(tool string) bool {
	t := strings.ToLower(tool)
	if t == "" {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_SECURITY_AUDIT_MCP")))
	switch mode {
	case "all":
		return true
	case "off":
		return false
	}
	words := append([]string(nil), privilegedMCPToolWords...)
	for _, extra := range strings.Split(os.Getenv("APTEVA_SECURITY_AUDIT_MCP_TOOLS"), ",") {
		if extra = strings.ToLower(strings.TrimSpace(extra)); extra != "" {
			words = append(words, extra)
		}
	}
	segments := strings.FieldsFunc(t, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == '/' })
	for _, w := range words {
		if strings.Contains(w, "_") {
			if strings.Contains(t, w) {
				return true
			}
			continue
		}
		for _, seg := range segments {
			if seg == w || seg == w+"s" {
				return true
			}
		}
	}
	return false
}

// deniedLimiter keeps a scanner from flooding the audit trail with 401s.
type deniedLimiter struct {
	mu      sync.Mutex
	window  time.Time
	counts  map[string]int
	limit   int
	dropped map[string]int
}

var auditDeniedLimiter = &deniedLimiter{counts: map[string]int{}, dropped: map[string]int{}, limit: 30}

// allow returns (record, suppressedSinceLastRecord).
func (d *deniedLimiter) allow(ip string) (bool, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if now.Sub(d.window) > time.Minute {
		d.window = now
		d.counts = map[string]int{}
	}
	d.counts[ip]++
	if d.counts[ip] > d.limit {
		d.dropped[ip]++
		return false, 0
	}
	suppressed := d.dropped[ip]
	delete(d.dropped, ip)
	return true, suppressed
}

func (s *Server) securityAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.securityAudit == nil || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ra := &requestAudit{correlationID: "req_" + generateToken(12)}
		clientCorrelation := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		r = r.WithContext(context.WithValue(r.Context(), requestAuditKey{}, ra))
		w.Header().Set("X-Apteva-Correlation-ID", ra.correlationID)
		rec := &statusRecorder{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		s.recordRequestAudit(r, ra, status, time.Since(started), clientCorrelation)
	})
}

func (s *Server) recordRequestAudit(r *http.Request, ra *requestAudit, status int, elapsed time.Duration, clientCorrelation string) {
	ra.mu.Lock()
	snapshot := requestAudit{correlationID: ra.correlationID, principalType: ra.principalType, principalID: ra.principalID,
		credentialID: ra.credentialID, installID: ra.installID, projectID: ra.projectID, presented: ra.presented,
		action: ra.action, mcpTool: ra.mcpTool, mcpApp: ra.mcpApp, skip: ra.skip}
	ra.mu.Unlock()
	if snapshot.skip || snapshot.action != "" {
		return
	}
	path := r.URL.Path
	ip := clientIP(r)
	result := securityResultSuccess
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result = securityResultDenied
	case status >= 500:
		result = securityResultError
	case status >= 400:
		result = securityResultFailure
	}
	ev := SecurityEvent{
		Result: result, PrincipalType: snapshot.principalType, PrincipalID: snapshot.principalID,
		CredentialID: snapshot.credentialID, SourceIP: ip, UserAgent: r.UserAgent(), InstallID: snapshot.installID,
		ProjectID: snapshot.projectID, CorrelationID: snapshot.correlationID, TargetType: "route", TargetID: r.Method + " " + path,
		Details: map[string]any{"status": status, "duration_ms": elapsed.Milliseconds()},
	}
	if clientCorrelation != "" && correlationIDPattern.MatchString(clientCorrelation) {
		ev.Details["client_request_id"] = clientCorrelation
	}
	if snapshot.installID > 0 {
		ev.OriginApp, _ = s.securityInstallApp(snapshot.installID)
	}
	if ev.PrincipalType == "" {
		ev.PrincipalType = "anonymous"
	}
	switch {
	case snapshot.mcpTool != "" && mcpToolIsPrivileged(snapshot.mcpTool):
		ev.Category, ev.Action = "privileged", "mcp.tools_call"
		ev.TargetType, ev.TargetID = "mcp_tool", snapshot.mcpApp+"/"+snapshot.mcpTool
		ev.Details["route"] = r.Method + " " + path
		if result == securityResultSuccess {
			ev.Severity = securitySeverityNotice
		}
	case result == securityResultDenied:
		if strings.HasPrefix(path, "/telemetry") {
			return
		}
		record, suppressed := auditDeniedLimiter.allow(ip)
		if !record {
			return
		}
		if suppressed > 0 {
			ev.Details["suppressed_since_last"] = suppressed
		}
		ev.Category, ev.Action = "auth", "http.denied"
		if snapshot.presented != "" && snapshot.principalType == "" {
			ev.Action = "auth.credential_rejected"
			ev.Details["credential_kind"] = snapshot.presented
		}
	case isMutatingMethod(r.Method):
		category := securityRouteCategory(path)
		if category == "" {
			return
		}
		ev.Category, ev.Action = category, "http."+strings.ToLower(r.Method)
	default:
		return
	}
	s.securityAudit.Record(ev)
}

// securityEventFromRequest pre-fills the request context of a semantic event.
func (s *Server) securityEventFromRequest(r *http.Request, category, action, result string) SecurityEvent {
	ev := SecurityEvent{Category: category, Action: action, Result: result}
	if r == nil {
		ev.PrincipalType = "system"
		return ev
	}
	ev.SourceIP = clientIP(r)
	ev.UserAgent = r.UserAgent()
	if ra := requestAuditFrom(r); ra != nil {
		ra.mu.Lock()
		ev.PrincipalType, ev.PrincipalID, ev.CredentialID = ra.principalType, ra.principalID, ra.credentialID
		ev.InstallID, ev.ProjectID, ev.CorrelationID = ra.installID, ra.projectID, ra.correlationID
		ra.mu.Unlock()
	}
	if ev.PrincipalType == "" {
		if uid := getUserID(r); uid > 0 {
			ev.PrincipalType, ev.PrincipalID = "user", strconv.FormatInt(uid, 10)
		} else {
			ev.PrincipalType = "anonymous"
		}
	}
	if ev.InstallID > 0 && s != nil {
		ev.OriginApp, _ = s.securityInstallApp(ev.InstallID)
	}
	return ev
}

// recordSecurity records a semantic event for this request and suppresses
// the generic per-route record.
func (s *Server) recordSecurity(r *http.Request, ev SecurityEvent) {
	if s == nil || s.securityAudit == nil {
		return
	}
	if r != nil && ev.Action != "" {
		noteAuditHandled(r, ev.Action)
	}
	s.securityAudit.Record(ev)
}

var securityInstallCache sync.Map // install id -> [2]string{app, project}

func (s *Server) securityInstallApp(installID int64) (app, project string) {
	if installID <= 0 || s == nil || s.store == nil {
		return "", ""
	}
	if v, ok := securityInstallCache.Load(installID); ok {
		pair := v.([2]string)
		return pair[0], pair[1]
	}
	_ = s.store.db.QueryRow(`SELECT COALESCE(a.name,''), COALESCE(i.project_id,'') FROM app_installs i JOIN apps a ON a.id = i.app_id WHERE i.id = ?`, installID).Scan(&app, &project)
	if app != "" {
		securityInstallCache.Store(installID, [2]string{app, project})
	}
	return app, project
}

// peekMCPToolName reads at most 1 MiB of a JSON-RPC body to find a
// tools/call name and puts every byte back, whatever the body size.
func peekMCPToolName(r *http.Request) string {
	if r == nil || r.Body == nil {
		return ""
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil {
		return ""
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(head, &rpc) != nil || rpc.Method != "tools/call" {
		return ""
	}
	return rpc.Params.Name
}

var credentialUseSeen sync.Map // "install:conn" -> time.Time

// recordCredentialUse records an app reading a connection's secret fields
// (brokered use, not an export). Repeated reads by the same install within
// an hour collapse into the first record so routine use cannot flood the
// trail.
func (s *Server) recordCredentialUse(r *http.Request, installID, connID int64, slug, role string, fields int) {
	if s.securityAudit == nil {
		return
	}
	key := strconv.FormatInt(installID, 10) + ":" + strconv.FormatInt(connID, 10)
	now := time.Now()
	if v, ok := credentialUseSeen.Load(key); ok && now.Sub(v.(time.Time)) < time.Hour {
		noteAuditHandled(r, "credential.use")
		return
	}
	credentialUseSeen.Store(key, now)
	ev := s.securityEventFromRequest(r, "credential", "credential.use", securityResultSuccess)
	ev.TargetType, ev.TargetID = "connection", strconv.FormatInt(connID, 10)+" ("+slug+")"
	ev.Details = map[string]any{"role": role, "fields": fields, "collapsed_window": "1h"}
	s.recordSecurity(r, ev)
}
