package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
	"golang.org/x/crypto/bcrypt"
)

// rateLimiter tracks attempts per IP.
type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

var loginLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}
var registerLimiter = &rateLimiter{attempts: make(map[string][]time.Time)}

func (rl *rateLimiter) allow(ip string, maxAttempts int, window time.Duration) bool {
	if ip == "" {
		return true // no IP = test or internal call
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	// Clean old entries
	var recent []time.Time
	for _, t := range rl.attempts[ip] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= maxAttempts {
		rl.attempts[ip] = recent
		return false
	}
	rl.attempts[ip] = append(recent, now)
	return true
}

func clientIP(r *http.Request) string {
	return resolvedClientIP(r)
}

func trustForwardedHeaders(r *http.Request) bool {
	if requestFromLoopback(r) || requestFromConfiguredProxy(r) {
		return true
	}
	// Backward compatibility for deployments that already firewall the server
	// behind one trusted proxy. New deployments should use the CIDR-scoped
	// APTEVA_TRUSTED_PROXY_CIDRS setting instead.
	return envTruthy(os.Getenv("APTEVA_TRUST_PROXY_HEADERS"))
}

func requestFromLoopback(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const sessionDuration = 7 * 24 * time.Hour
const cookieName = "session"

// crossOriginCookies is flipped on at server boot (see main.go) when
// the configured CORS mode permits credentialed cross-origin calls.
// When true, the session cookie goes out as SameSite=None; Secure so
// browsers will send it on cross-origin requests. Otherwise we keep
// the stricter SameSite=Lax default.
var crossOriginCookies bool

func generateToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// requestIsTLS reports whether the request came in over a TLS
// connection — either directly (r.TLS != nil) or through a reverse
// proxy that set X-Forwarded-Proto. Used to decide whether the
// session cookie can carry Secure.
func requestIsTLS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if trustForwardedHeaders(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

// setSessionCookie picks SameSite + Secure based on (1) the cross-
// origin policy and (2) the actual scheme the request came in on.
//
// The Secure attribute on a cookie is rejected by browsers over plain
// HTTP unless the host is localhost — so a LAN/hostname access over
// HTTP would silently lose the cookie if we always set Secure. Cross-
// origin policy still requires SameSite=None+Secure on HTTPS, but
// over HTTP we degrade to SameSite=Lax (same-origin only — which is
// the actual deployment shape for HTTP-only setups anyway).
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	setSessionCookieForDuration(w, r, token, sessionDuration)
}

func setSessionCookieForDuration(w http.ResponseWriter, r *http.Request, token string, duration time.Duration) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(duration.Seconds()),
		Secure:   requestIsTLS(r),
	}
	if crossOriginCookies && requestIsTLS(r) {
		c.SameSite = http.SameSiteNoneMode
		c.Secure = true
	}
	http.SetCookie(w, c)
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	}
	if crossOriginCookies && requestIsTLS(r) {
		c.SameSite = http.SameSiteNoneMode
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// authMiddleware extracts user from session cookie or API key.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// These identity headers are owned by the server. A network client
		// cannot select a user or app install by supplying them directly.
		r.Header.Del("X-User-ID")
		r.Header.Del("X-Apteva-App-Install-ID")
		for _, header := range []string{
			"X-Apteva-Project-ID", "X-Apteva-Issuer-App", "X-Apteva-Issuer-Install-ID",
			"X-Apteva-Subject-Type", "X-Apteva-Subject-ID", "X-Apteva-Subject-Email",
			"X-Apteva-Organization-ID", "X-Apteva-Organization-Slug", "X-Apteva-Scopes",
			"X-Apteva-Conversation-ID",
			sdk.HeaderBoundCallerInstallID,
		} {
			r.Header.Del(header)
		}
		// Try session cookie first
		if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
			if userID, err := s.store.GetSession(cookie.Value); err == nil {
				r.Header.Set("X-User-ID", itoa(userID))
				next(w, r)
				return
			}
		}

		// Internal gateway auth. The per-agent apteva-server MCP runs
		// as a local subprocess and needs to call the normal dashboard
		// API handlers so lifecycle logic stays centralized. Accept
		// the shared instance secret only from loopback, and only with
		// an explicit gateway user header supplied by the server-spawned
		// process.
		if s.instanceSecret != "" && requestFromLoopback(r) {
			got := r.Header.Get("X-Agent-Secret")
			if got == "" {
				got = r.Header.Get("X-Instance-Secret")
			}
			if got == s.instanceSecret {
				if uid, err := strconv.ParseInt(r.Header.Get("X-Apteva-MCP-User-ID"), 10, 64); err == nil && uid > 0 {
					r.Header.Set("X-User-ID", itoa(uid))
					next(w, r)
					return
				}
			}
		}

		// API key auth. Three carrier forms (first match wins):
		//   1. Authorization: Bearer <key>      — canonical
		//   2. X-API-Key: <key>                 — common alt header
		//   3. ?api_key=<key>                   — SSE/EventSource path
		//      (browsers can't set custom headers on EventSource, so
		//      the key must travel as a query param)
		token := ""
		if a := r.Header.Get("Authorization"); a != "" {
			token = strings.TrimPrefix(a, "Bearer ")
		}
		if token == "" {
			token = r.Header.Get("X-API-Key")
		}
		if token == "" && apiKeyQueryAllowed(r) {
			token = r.URL.Query().Get("api_key")
		} else if token == "" && appTokenQueryAllowed(r) {
			candidate := r.URL.Query().Get("api_key")
			if strings.HasPrefix(candidate, "app_") || strings.HasPrefix(candidate, "dev-") {
				token = candidate
			}
		}
		if token != "" {
			keyHash := HashAPIKey(token)
			if strings.HasPrefix(token, "uk_") {
				appName, appPath, ok := splitAppProxyPath(r.URL.Path)
				if !ok {
					http.Error(w, "delegated user keys are only valid for app routes", http.StatusUnauthorized)
					return
				}
				key, err := s.store.GetDelegatedUserAPIKey(keyHash)
				if err == nil {
					if !s.authorizeDelegatedAppRequest(w, r, key, appName) {
						return
					}
					if appPath == "/mcp" && r.Method == http.MethodPost {
						body, readErr := io.ReadAll(r.Body)
						if readErr != nil {
							http.Error(w, "invalid request body", http.StatusBadRequest)
							return
						}
						_ = r.Body.Close()
						restoreRequestBody(r, body)
						action, actionErr := publicClientMCPToolName(body)
						if actionErr != nil {
							http.Error(w, actionErr.Error(), http.StatusBadRequest)
							return
						}
						if !delegatedUserScopeAllows(key.Scopes, appName, action) {
							http.Error(w, "delegated user key is not allowed to call this app action", http.StatusForbidden)
							return
						}
					}
					setDelegatedUserPrincipalHeaders(r, key)
					r.Header.Set("X-User-ID", itoa(key.UserID))
					s.store.MarkAPIKeyUsed(key.ID, requestClientIP(r))
					next(w, r)
					return
				}
			}
			user, err := s.store.GetUserByAPIKey(keyHash)
			if err == nil {
				r.Header.Set("X-User-ID", itoa(user.ID))
				next(w, r)
				return
			}
		}

		// App-install token. Sidecars call back into the platform —
		// either /api/apps/<other>/* (cross-app) or /api/apps/callback/*
		// (PlatformClient) — using their APTEVA_APP_TOKEN, currently
		// formatted "dev-<install_id>". Resolve it to the install row's
		// installed_by user so downstream handlers see a normal user
		// id; the proxy then swaps the header to the destination
		// install's token before forwarding. Loopback-only dev tokens are
		// accepted temporarily for sidecars started by an older server build.
		if token != "" && appTokenRouteAllowed(r.URL.Path) {
			id, installedBy, status, appErr := s.appInstallForToken(token)
			if appErr != nil && strings.HasPrefix(token, "dev-") {
				id = legacyAppTokenInstallID(r, token)
				if id > 0 {
					appErr = s.store.db.QueryRow(
						`SELECT COALESCE(installed_by,0), status FROM app_installs WHERE id=?`, id,
					).Scan(&installedBy, &status)
				}
			}
			if appErr == nil && id > 0 {
				// Accept "running" plus any pre-running state. The
				// supervisor only flips status to "running" AFTER
				// the health check passes, but the sidecar's OnMount
				// (which has to run before it can be healthy) calls
				// back into the platform for WhoAmI, integration
				// bindings, and connection credentials. Gating this
				// on status="running" would 401 every callback during
				// boot — exactly when those callbacks are most
				// needed. "disabled" / "error" are still rejected so
				// a stopped install can't impersonate itself.
				if status != "disabled" && status != "error" {
					if installedBy == 0 {
						installedBy = 1 // global / built-in installs default to admin
					}
					r.Header.Set("X-User-ID", itoa(installedBy))
					r.Header.Set("X-Apteva-App-Install-ID", itoa(id))
					next(w, r)
					return
				}
			}
		}

		// Anonymous app-route fall-through is only allowed when the manifest
		// explicitly marks the matching route no_auth. Signed URLs must be
		// declared this way too; the sidecar still verifies their signature.
		if s.anonymousAppRouteAllowed(r) {
			next(w, r)
			return
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func apiKeyQueryAllowed(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	return r.URL.Path == "/telemetry/stream" || strings.HasPrefix(r.URL.Path, "/app-events/")
}

func appTokenQueryAllowed(r *http.Request) bool {
	if r == nil || !requestFromLoopback(r) {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/apps/") && appTokenRouteAllowed(r.URL.Path)
}

func (s *Server) anonymousAppRouteAllowed(r *http.Request) bool {
	appName, appPath, ok := splitAppProxyPath(r.URL.Path)
	if !ok || isAppManagementRoute(appName) || s == nil || s.installedApps == nil {
		return false
	}
	strippedPath, installID, hasInstallSelector, err := splitAppInstallSelector(appPath)
	if err != nil {
		return false
	}
	if hasInstallSelector {
		entry := s.installedApps.Get(installID)
		if entry == nil || entry.AppName != appName {
			return false
		}
		return appProxyRouteIsNoAuth(entry, strippedPath, r.Method)
	}
	return s.anonymousAppNoAuthRouteAllowed(r, appName, appPath)
}

func splitAppProxyPath(path string) (appName, appPath string, ok bool) {
	switch {
	case strings.HasPrefix(path, "/api/apps/"):
		path = strings.TrimPrefix(path, "/api/apps/")
	case strings.HasPrefix(path, "/apps/"):
		path = strings.TrimPrefix(path, "/apps/")
	default:
		return "", "", false
	}
	if path == "" {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	appName = parts[0]
	appPath = "/"
	if len(parts) == 2 {
		appPath = "/" + parts[1]
	}
	return appName, appPath, appName != ""
}

func isAppManagementRoute(first string) bool {
	switch first {
	case "installs", "callback", "preview", "install", "marketplace":
		return true
	default:
		return false
	}
}

func (s *Server) anonymousAppNoAuthRouteAllowed(r *http.Request, appName, appPath string) bool {
	if s == nil || s.installedApps == nil {
		return false
	}
	entry := s.installedAppForRequest(appName, r)
	if entry == nil {
		return false
	}
	return appProxyRouteIsNoAuth(entry, appPath, r.Method)
}

func (s *Server) installedAppForRequest(appName string, r *http.Request) *InstalledApp {
	q := r.URL.Query()
	if installIDRaw := q.Get("install_id"); installIDRaw != "" {
		installID, err := strconv.ParseInt(installIDRaw, 10, 64)
		if err == nil && installID > 0 {
			if entry := s.installedApps.Get(installID); entry != nil && entry.AppName == appName {
				return entry
			}
		}
		return nil
	}
	if installID := installIDFromDevAPIKey(q.Get("api_key")); installID > 0 {
		if entry := s.installedApps.Get(installID); entry != nil && entry.AppName == appName {
			return entry
		}
		return nil
	}
	if projectID := q.Get("project_id"); projectID != "" {
		return s.installedApps.GetByNameAndProject(appName, projectID)
	}
	return s.installedApps.GetByNameAndProject(appName, "")
}

func appRouteMatches(pattern, path string) bool {
	if pattern == "" {
		pattern = "/"
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	// Preserve the historical exact/subtree behavior for literal routes.
	// RouteSpec.Prefix also carries Go 1.22 ServeMux patterns from SDK apps,
	// however, so parameterized routes such as /v1/devices/{id}/test must be
	// evaluated with the same matcher the sidecar uses.
	if strings.Contains(pattern, "{") {
		matcher := cachedAppRouteMatcher(pattern)
		if matcher == nil {
			return false
		}
		_, matchedPattern := matcher.Handler(&http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: path},
		})
		return matchedPattern != ""
	}
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern)
	}
	return path == pattern
}

var appRouteMatcherCache sync.Map

func cachedAppRouteMatcher(pattern string) (matcher *http.ServeMux) {
	if cached, ok := appRouteMatcherCache.Load(pattern); ok {
		matcher, _ = cached.(*http.ServeMux)
		return matcher
	}
	defer func() {
		if recover() != nil {
			matcher = nil
		}
	}()
	compiled := http.NewServeMux()
	compiled.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	actual, _ := appRouteMatcherCache.LoadOrStore(pattern, compiled)
	matcher, _ = actual.(*http.ServeMux)
	return matcher
}

func getUserID(r *http.Request) int64 {
	id, _ := atoi64(r.Header.Get("X-User-ID"))
	return id
}

// GET /auth/status — public, returns the server's current registration mode
// so the dashboard can decide whether to render a setup screen, a normal
// login, or a locked-down "no signups" page. No auth required.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"reg_mode":    s.regMode,
		"needs_setup": s.regMode == "setup",
	})
}

// POST /auth/register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Rate limit: 3 registrations per IP per hour
	if !registerLimiter.allow(clientIP(r), 3, time.Hour) {
		http.Error(w, "too many registration attempts", http.StatusTooManyRequests)
		return
	}

	// Check registration mode
	switch s.regMode {
	case "locked":
		// Require a valid project invite token. The token is delivered
		// either via X-Invite-Token header (programmatic) or
		// ?invite=<token> on the URL (dashboard flow). Both must
		// resolve to a non-expired, non-accepted project_invites row
		// whose email matches the registration email (case-insensitive)
		// — that proves possession of the link AND limits use to the
		// addressee. The actual project membership is added in
		// handleInviteAccept after the user has a session; this
		// handler only verifies the invite is currently valid for the
		// registering email.
		invite := r.Header.Get("X-Invite-Token")
		if invite == "" {
			invite = r.URL.Query().Get("invite")
		}
		if invite == "" {
			http.Error(w, "registration locked — invite token required", http.StatusForbidden)
			return
		}
		inv, err := s.store.GetInviteByToken(invite)
		if err != nil {
			http.Error(w, "invite invalid or expired", http.StatusForbidden)
			return
		}
		// Email comparison must happen post-body-decode; we do it
		// after the JSON parse below by stashing the invite for that
		// step. Inline the body decode here to keep ordering tight.
		var preBody struct {
			Email string `json:"email"`
		}
		// Read & restore body so the later DecodeJSON still works.
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &preBody)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if !strings.EqualFold(strings.TrimSpace(preBody.Email), strings.TrimSpace(inv.Email)) {
			http.Error(w, "invite was issued to a different email", http.StatusForbidden)
			return
		}
	case "setup":
		// Require setup token (first user)
		token := r.Header.Get("X-Setup-Token")
		if token == "" || token != s.setupToken {
			http.Error(w, "setup token required for first registration", http.StatusForbidden)
			return
		}
		// "open" — no restriction
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Email == "" || body.Password == "" {
		http.Error(w, "username and password required", http.StatusBadRequest)
		return
	}
	if len(body.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	user, err := s.store.CreateUser(body.Email, string(hash))
	if err != nil {
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}

	// First registered user becomes platform admin. Subsequent users
	// stay 'user' and the admin can promote from /admin/users if
	// needed. We rely on HasUsers() being false at the start of this
	// handler in setup mode, but check via id == 1 as a safety net
	// in case of any concurrency weirdness.
	if s.regMode == "setup" || user.ID == 1 {
		_ = s.store.SetPlatformRole(user.ID, PlatformAdmin)
	}

	// Lock registration after first user (if was in setup mode)
	if s.regMode == "setup" {
		s.regMode = "locked"
		s.setupToken = ""
	}

	// Auto-create a default project for the new user, plus the
	// owner membership row so the new project_members-driven authz
	// lookup finds them.
	project, err := s.store.CreateProject(user.ID, "Default", "Default project", "#6366f1")
	if err == nil && project != nil {
		_ = s.store.AddProjectMember(project.ID, user.ID, ProjectOwner, user.ID)
	}

	writeJSON(w, map[string]any{"id": user.ID, "email": user.Email})
}

// POST /auth/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Rate limit: 5 login attempts per IP per minute
	if !loginLimiter.allow(clientIP(r), 5, time.Minute) {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	user, err := s.store.GetUserByEmail(body.Email)
	if err != nil {
		// Back-compat: older CLI setups silently appended "@local" to
		// plain usernames at registration time. If the typed value has
		// no "@" and the direct lookup failed, try the legacy variant
		// so those accounts remain loginable without re-running setup.
		if !strings.Contains(body.Email, "@") {
			if u2, err2 := s.store.GetUserByEmail(body.Email + "@local"); err2 == nil {
				user = u2
				err = nil
			}
		}
	}
	if err != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)); err != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token := generateToken(32)
	if userMFAEnabled(user) {
		if err := s.store.CreatePendingMFASession(token, user.ID, time.Now().Add(mfaPendingDuration)); err != nil {
			http.Error(w, "failed to create MFA challenge", http.StatusInternalServerError)
			return
		}
		setSessionCookieForDuration(w, r, token, mfaPendingDuration)
		writeJSON(w, map[string]any{
			"mfa_required": true,
			"email":        user.Email,
		})
		return
	}
	if err := s.store.CreateSession(token, user.ID, time.Now().Add(sessionDuration)); err != nil {
		http.Error(w, "failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, r, token)
	writeJSON(w, map[string]any{
		"user_id":      user.ID,
		"email":        user.Email,
		"mfa_required": false,
	})
}

// POST /auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		_ = s.store.DeleteSession(cookie.Value)
	}
	clearSessionCookie(w, r)
	writeJSON(w, map[string]string{"status": "ok"})
}

// GET /auth/me — returns the authenticated user's profile (id + email +
// created_at). Accepts either a session cookie or an API key so
// programmatic clients can introspect their own identity without
// scraping /auth/keys. Matches the carrier rules in authMiddleware.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	var userID int64
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		if uid, err := s.store.GetSession(cookie.Value); err == nil {
			userID = uid
		} else {
			// Expired or invalid cookie — clear it so the browser stops
			// sending a bad one on every request.
			clearSessionCookie(w, r)
		}
	}
	if userID == 0 {
		// Fall back to API-key auth: Authorization Bearer, X-API-Key, or ?api_key.
		token := ""
		if a := r.Header.Get("Authorization"); a != "" {
			token = strings.TrimPrefix(a, "Bearer ")
		}
		if token == "" {
			token = r.Header.Get("X-API-Key")
		}
		if token == "" {
			token = r.URL.Query().Get("api_key")
		}
		if token != "" {
			if u, err := s.store.GetUserByAPIKey(HashAPIKey(token)); err == nil {
				userID = u.ID
			}
		}
	}
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u, err := s.store.GetUserByID(userID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	role := u.Role
	if role == "" {
		role = string(PlatformUser)
	}
	uiLayout, uiLayoutRevision := s.store.GetUserUILayoutWithRevision(u.ID)
	resp := map[string]any{
		"user_id":            u.ID,
		"email":              u.Email,
		"role":               role,
		"created_at":         u.CreatedAt.UTC().Format(time.RFC3339),
		"onboarded":          u.OnboardedAt != nil,
		"language":           normalizedDashboardLanguage(s.store.GetUserLanguage(u.ID)),
		"ui_layout":          uiLayout,
		"ui_layout_revision": uiLayoutRevision,
		"mfa_enabled":        userMFAEnabled(u),
		"mfa_type": func() string {
			if userMFAEnabled(u) {
				return "totp"
			}
			return ""
		}(),
		"mfa_recovery_codes_remaining": recoveryHashCount(u.MFARecoveryHashes),
	}
	if u.OnboardedAt != nil {
		resp["onboarded_at"] = u.OnboardedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, resp)
}

func normalizedDashboardLanguage(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fr", "fr-fr", "fr-ca":
		return "fr"
	case "es", "es-es", "es-mx", "es-419":
		return "es"
	default:
		return "en"
	}
}

// PUT /auth/preferences — update lightweight user preferences that should
// follow the account across browsers. Omitted fields are left unchanged.
func (s *Server) handleAuthPreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		Language *string         `json:"language"`
		UILayout json.RawMessage `json:"ui_layout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Language != nil {
		language := normalizedDashboardLanguage(*body.Language)
		if err := s.store.SetUserLanguage(userID, language); err != nil {
			http.Error(w, "failed to save preferences", http.StatusInternalServerError)
			return
		}
	}
	if body.UILayout != nil {
		if err := s.store.SetUserUILayout(userID, body.UILayout); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	uiLayout, uiLayoutRevision := s.store.GetUserUILayoutWithRevision(userID)
	writeJSON(w, map[string]any{
		"language":           normalizedDashboardLanguage(s.store.GetUserLanguage(userID)),
		"ui_layout":          uiLayout,
		"ui_layout_revision": uiLayoutRevision,
	})
}

// POST /auth/onboarding/complete — flips users.onboarded_at to now for
// the authenticated user. Idempotent at the store level (IS NULL guard
// prevents overwriting the original timestamp on retry).
func (s *Server) handleCompleteOnboarding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.store.MarkUserOnboarded(userID); err != nil {
		http.Error(w, "failed to mark onboarded", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /auth/password — change the logged-in user's password. Requires
// the CURRENT password to be presented (auth still enforced by the
// middleware-populated X-User-ID header). On success every OTHER active
// session for this user is wiped, so a leaked cookie on another device
// is instantly neutralised. The session doing the change keeps its cookie.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.CurrentPassword == "" || body.NewPassword == "" {
		http.Error(w, "current_password and new_password required", http.StatusBadRequest)
		return
	}
	if len(body.NewPassword) < 8 {
		http.Error(w, "new password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	if body.CurrentPassword == body.NewPassword {
		http.Error(w, "new password must differ from current", http.StatusBadRequest)
		return
	}

	u, err := s.store.GetUserByID(userID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.CurrentPassword)); err != nil {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateUserPassword(userID, string(newHash)); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}

	// Keep the current session alive, revoke every other one.
	currentToken := ""
	if c, err := r.Cookie(cookieName); err == nil {
		currentToken = c.Value
	}
	if err := s.store.DeleteSessionsForUserExcept(userID, currentToken); err != nil {
		log.Printf("[AUTH] password changed user=%d but session sweep failed: %v", userID, err)
	}

	log.Printf("[AUTH] password changed user=%d remote=%s", userID, r.RemoteAddr)
	writeJSON(w, map[string]any{"status": "ok"})
}

// POST /auth/keys — create API key
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)

	var body struct {
		Name               string          `json:"name"`
		Kind               string          `json:"kind"`
		ProjectID          string          `json:"project_id"`
		Scopes             json.RawMessage `json:"scopes"`
		AllowedOrigins     []string        `json:"allowed_origins"`
		RateLimitPerMinute int             `json:"rate_limit_per_minute"`
		ExpiresAt          string          `json:"expires_at"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "default"
	}
	kind := strings.TrimSpace(body.Kind)
	if kind == "" {
		kind = "private"
	}
	if kind != "private" && kind != "public_client" {
		http.Error(w, "kind must be private or public_client", http.StatusBadRequest)
		return
	}
	scopesJSON := "[]"
	if len(body.Scopes) > 0 && string(body.Scopes) != "null" {
		if !json.Valid(body.Scopes) {
			http.Error(w, "scopes must be valid JSON", http.StatusBadRequest)
			return
		}
		scopesJSON = string(body.Scopes)
	}
	originsJSON := "[]"
	if len(body.AllowedOrigins) > 0 {
		var origins []string
		for i := range body.AllowedOrigins {
			origin := strings.TrimSpace(body.AllowedOrigins[i])
			if origin != "" {
				origins = append(origins, origin)
			}
		}
		if raw, err := json.Marshal(origins); err == nil {
			originsJSON = string(raw)
		}
	}
	projectID := strings.TrimSpace(body.ProjectID)
	if kind == "public_client" {
		if projectID == "" {
			http.Error(w, "project_id required for public_client keys", http.StatusBadRequest)
			return
		}
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectEditor); !ok {
			return
		}
	}
	if strings.TrimSpace(body.ExpiresAt) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ExpiresAt)); err != nil {
			http.Error(w, "expires_at must be RFC3339", http.StatusBadRequest)
			return
		}
	}

	prefix := "sk-"
	if kind == "public_client" {
		prefix = "pk_"
	}
	raw := prefix + generateToken(24)
	keyHash := HashAPIKey(raw)
	keyPrefix := raw[:11] // prefix + first 8 hex chars

	key, err := s.store.CreateAPIKey(userID, body.Name, keyHash, keyPrefix, APIKeyCreateOptions{
		Kind:               kind,
		ProjectID:          projectID,
		Scopes:             scopesJSON,
		AllowedOrigins:     originsJSON,
		RateLimitPerMinute: body.RateLimitPerMinute,
		ExpiresAt:          strings.TrimSpace(body.ExpiresAt),
	})
	if err != nil {
		http.Error(w, "failed to create key", http.StatusInternalServerError)
		return
	}

	// Return the full key ONCE — it can't be retrieved later
	writeJSON(w, map[string]any{
		"id":      key.ID,
		"name":    key.Name,
		"key":     raw,
		"prefix":  keyPrefix,
		"kind":    key.Kind,
		"message": "Save this key — it won't be shown again",
	})
}

// GET /auth/keys — list keys
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	keys, err := s.store.ListAPIKeys(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []APIKey{}
	}
	writeJSON(w, keys)
}

// DELETE /auth/keys/:id
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	keyID, err := atoi64(strings.TrimPrefix(r.URL.Path, "/auth/keys/"))
	if err != nil {
		http.Error(w, "invalid key ID", http.StatusBadRequest)
		return
	}
	s.store.DeleteAPIKey(userID, keyID)
	writeJSON(w, map[string]string{"status": "deleted"})
}
