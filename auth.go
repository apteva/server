package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionDuration.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// authMiddleware extracts user from session cookie or API key.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try session cookie first
		if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
			if userID, err := s.store.GetSession(cookie.Value); err == nil {
				r.Header.Set("X-User-ID", itoa(userID))
				next(w, r)
				return
			}
		}

		// Try Authorization header (API key)
		auth := r.Header.Get("Authorization")
		if auth != "" {
			token := strings.TrimPrefix(auth, "Bearer ")
			keyHash := HashAPIKey(token)
			user, err := s.store.GetUserByAPIKey(keyHash)
			if err == nil {
				r.Header.Set("X-User-ID", itoa(user.ID))
				next(w, r)
				return
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func getUserID(r *http.Request) int64 {
	id, _ := atoi64(r.Header.Get("X-User-ID"))
	return id
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

	setSessionCookie(w, token)
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
	clearSessionCookie(w)
	writeJSON(w, map[string]string{"status": "ok"})
}

// GET /auth/me — check if session is valid
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	userID, err := s.store.GetSession(cookie.Value)
	if err != nil {
		clearSessionCookie(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"user_id": userID})
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
