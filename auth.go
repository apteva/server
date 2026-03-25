package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Session tokens: map token → userID (in-memory, lost on restart)
var sessions = map[string]int64{}

func generateToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// authMiddleware extracts user from session token or API key.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")

		// Try session token first
		if userID, ok := sessions[token]; ok {
			r.Header.Set("X-User-ID", itoa(userID))
			next(w, r)
			return
		}

		// Try API key
		keyHash := HashAPIKey(token)
		user, err := s.store.GetUserByAPIKey(keyHash)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-User-ID", itoa(user.ID))
		next(w, r)
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

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Email == "" || body.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
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
		http.Error(w, "email already registered", http.StatusConflict)
		return
	}

	writeJSON(w, map[string]any{"id": user.ID, "email": user.Email})
}

// POST /auth/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
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
	sessions[token] = user.ID

	writeJSON(w, map[string]any{
		"token":      token,
		"user_id":    user.ID,
		"email":      user.Email,
		"expires_in": int((24 * time.Hour).Seconds()),
	})
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
