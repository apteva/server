package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

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
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.Split(fwd, ",")[0]
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
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
	rand.Read(b)
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
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
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
	c := &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionDuration.Seconds()),
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
		// Signed-URL passthrough for app routes. Storage (and any
		// future app) mints time-limited URLs with `?sig=…&exp=…`
		// for cases where the consumer can't carry session/API auth
		// (Facebook's CDN fetching a posted image, a public link
		// shared by the user, etc.). The app's own handler verifies
		// the signature; the platform proxy just needs to let the
		// request reach it. We require BOTH params and an app-route
		// prefix so this can't be used to bypass auth on management
		// routes (which never need signing).
		if strings.HasPrefix(r.URL.Path, "/api/apps/") || strings.HasPrefix(r.URL.Path, "/apps/") {
			q := r.URL.Query()
			if q.Get("sig") != "" && q.Get("exp") != "" {
				next(w, r)
				return
			}
		}
		// Try session cookie first
		if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
			if userID, err := s.store.GetSession(cookie.Value); err == nil {
				r.Header.Set("X-User-ID", itoa(userID))
				next(w, r)
				return
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
		if token == "" {
			token = r.URL.Query().Get("api_key")
		}
		if token != "" {
			keyHash := HashAPIKey(token)
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
		// install's token before forwarding.
		if token != "" && strings.HasPrefix(token, "dev-") {
			if id, perr := atoi64(strings.TrimPrefix(token, "dev-")); perr == nil && id > 0 {
				var (
					installedBy int64
					status      string
				)
				err := s.store.db.QueryRow(
					`SELECT installed_by, status FROM app_installs WHERE id = ?`, id,
				).Scan(&installedBy, &status)
				if err == nil && status == "running" {
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

		// Anonymous app-route fall-through. Apps decide for themselves
		// whether GET requests need auth. Storage's visibility=public
		// uses this — the file's URL is the same as for authenticated
		// requests, the app handler just doesn't require credentials.
		// X-User-ID is intentionally NOT set so the app can tell this
		// is an anonymous request and refuse private resources.
		//
		// Scoped to GET (and HEAD) on /api/apps/<name>/...; never the
		// management surfaces under /api/apps/installs, /api/apps/
		// callback, /api/apps/preview etc., which always require auth.
		//
		// Note: apiMux is wrapped in http.StripPrefix("/api"), so the
		// path here is /apps/<name>/... not /api/apps/<name>/.... We
		// match either form for safety in case of routing changes.
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			path := ""
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/apps/"):
				path = strings.TrimPrefix(r.URL.Path, "/api/apps/")
			case strings.HasPrefix(r.URL.Path, "/apps/"):
				path = strings.TrimPrefix(r.URL.Path, "/apps/")
			}
			if path != "" {
				first := path
				if i := strings.Index(path, "/"); i >= 0 {
					first = path[:i]
				}
				switch first {
				case "installs", "callback", "preview", "install", "marketplace":
					// management routes — fall through to 401
				default:
					next(w, r)
					return
				}
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
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
		// Require invite token
		invite := r.Header.Get("X-Invite-Token")
		if invite == "" {
			http.Error(w, "registration locked — invite token required", http.StatusForbidden)
			return
		}
		// TODO: validate invite token against DB
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

	// Lock registration after first user (if was in setup mode)
	if s.regMode == "setup" {
		s.regMode = "locked"
		s.setupToken = ""
	}

	// Auto-create a default project for the new user
	s.store.CreateProject(user.ID, "Default", "Default project", "#6366f1")

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
	if err := s.store.CreateSession(token, user.ID, time.Now().Add(sessionDuration)); err != nil {
		http.Error(w, "failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, r, token)
	writeJSON(w, map[string]any{
		"user_id": user.ID,
		"email":   user.Email,
	})
}

// POST /auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
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
	writeJSON(w, map[string]any{
		"user_id":    u.ID,
		"email":      u.Email,
		"created_at": u.CreatedAt.UTC().Format(time.RFC3339),
	})
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
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "default"
	}

	// Generate key: sk-<random>
	raw := "sk-" + generateToken(24)
	keyHash := HashAPIKey(raw)
	keyPrefix := raw[:11] // "sk-" + first 8 hex chars

	key, err := s.store.CreateAPIKey(userID, body.Name, keyHash, keyPrefix)
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
