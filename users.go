package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// User administration — list / create / get / delete / admin-reset-password.
//
// Mounted at /users; authenticated via the shared authMiddleware (the
// same cookie-or-API-key path every other /api route uses). Admin
// enforcement is per-handler rather than a wrapping middleware so each
// policy is visible at the call site.
//
// Admin = user_id 1 (the first registered account). No schema change.
// Once you need richer roles, bump this to an `is_admin` column.

// isAdmin treats user_id=1 as the server's admin. The first-registered
// user always lands at id=1 because the users table is autoincrementing
// and setup mode auto-locks registration after that row lands.
func (s *Server) isAdmin(userID int64) bool { return userID == 1 }

// GET /users — admin lists every user; non-admin gets 403.
// POST /users — admin creates a new user directly (email + initial
// password, no invite/setup token needed — admin IS the gate).
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	caller := getUserID(r)
	if !s.isAdmin(caller) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		users, err := s.store.ListUsers()
		if err != nil {
			http.Error(w, "failed to list users", http.StatusInternalServerError)
			return
		}
		type row struct {
			User
			UserResourceCounts
			IsAdmin bool `json:"is_admin"`
			IsSelf  bool `json:"is_self"`
		}
		out := make([]row, 0, len(users))
		for _, u := range users {
			out = append(out, row{
				User:               u,
				UserResourceCounts: s.store.CountUserResources(u.ID),
				IsAdmin:            s.isAdmin(u.ID),
				IsSelf:             u.ID == caller,
			})
		}
		writeJSON(w, out)

	case http.MethodPost:
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		body.Email = strings.TrimSpace(body.Email)
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
		u, err := s.store.CreateUser(body.Email, string(hash))
		if err != nil {
			http.Error(w, "email already taken", http.StatusConflict)
			return
		}
		// Auto-create a default project so the new user has somewhere
		// to land on first login, matching the normal register flow.
		s.store.CreateProject(u.ID, "Default", "Default project", "#6366f1")
		log.Printf("[USERS] admin=%d created user id=%d email=%s", caller, u.ID, u.Email)
		writeJSON(w, u)

	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleUserByID dispatches /users/:id and /users/:id/password.
// Self can GET themselves; admin can GET anyone, DELETE non-admin non-self,
// and PATCH password on anyone.
func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	caller := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/users/")
	parts := strings.SplitN(path, "/", 2)
	targetID, err := atoi64(parts[0])
	if err != nil || targetID <= 0 {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	// Sub-resource routing. Only /password is supported today.
	if sub == "password" {
		s.handleUserPasswordReset(w, r, caller, targetID)
		return
	}
	if sub != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if caller != targetID && !s.isAdmin(caller) {
			// 404 not 403 so we don't leak which ids exist to a
			// non-admin probing the endpoint.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		u, err := s.store.GetUserByID(targetID)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{
			"id":         u.ID,
			"email":      u.Email,
			"created_at": u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"counts":     s.store.CountUserResources(u.ID),
			"is_admin":   s.isAdmin(u.ID),
			"is_self":    u.ID == caller,
		})

	case http.MethodDelete:
		if !s.isAdmin(caller) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		if targetID == caller {
			http.Error(w, "cannot delete your own account", http.StatusBadRequest)
			return
		}
		if s.isAdmin(targetID) {
			http.Error(w, "cannot delete the admin account", http.StatusBadRequest)
			return
		}
		// dry_run=1 → return what would be deleted, don't actually delete.
		// Lets the UI show a blast-radius confirmation before the user
		// clicks through.
		if r.URL.Query().Get("dry_run") == "1" {
			u, err := s.store.GetUserByID(targetID)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{
				"user":        map[string]any{"id": u.ID, "email": u.Email},
				"would_delete": s.store.CountUserResources(u.ID),
			})
			return
		}

		// Stop any running cores owned by the target user BEFORE
		// deleting DB rows — otherwise the reaper will try to update
		// an instances row that no longer exists. ListInstances
		// scoped to the target user across all projects is good
		// enough (empty projectID = all projects).
		insts, _ := s.store.ListInstances(targetID, "")
		for _, inst := range insts {
			s.instances.Stop(inst.ID)
		}

		if err := s.store.DeleteUser(targetID); err != nil {
			log.Printf("[USERS] admin=%d delete user=%d failed: %v", caller, targetID, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		log.Printf("[USERS] admin=%d deleted user=%d", caller, targetID)
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}

// PATCH /users/:id/password — admin resets a target user's password
// without needing to know the current one. Revokes every session for
// that user so they're forced to log in with the new password.
// Separate from /auth/password (self-service) which requires the
// current password — admin-reset doesn't, which is the whole point.
func (s *Server) handleUserPasswordReset(w http.ResponseWriter, r *http.Request, caller, targetID int64) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	if !s.isAdmin(caller) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var body struct {
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
	if _, err := s.store.GetUserByID(targetID); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateUserPassword(targetID, string(hash)); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.DeleteSessionsForUser(targetID); err != nil {
		log.Printf("[USERS] admin=%d reset pw for user=%d but session sweep failed: %v", caller, targetID, err)
	}
	log.Printf("[USERS] admin=%d reset password for user=%d", caller, targetID)
	writeJSON(w, map[string]string{"status": "ok"})
}
