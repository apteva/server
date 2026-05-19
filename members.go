package main

// members.go — HTTP surface for project members, invites, and
// admin-only platform user management. Phase 1 of multi-user.
//
// Routes wired in main.go:
//
//   GET    /api/projects/:id/members                — list (viewer+)
//   POST   /api/projects/:id/members/invites        — create invite (owner)
//   GET    /api/projects/:id/members/invites        — list pending  (viewer+)
//   DELETE /api/projects/:id/members/invites/:tok   — revoke invite (owner)
//   PATCH  /api/projects/:id/members/:userId        — change role   (owner)
//   DELETE /api/projects/:id/members/:userId        — remove member (owner)
//
//   POST   /api/invites/:token/accept               — accept invite (any caller, email match)
//   GET    /api/invites/:token                      — preview invite for the login page (public)
//
//   GET    /api/admin/users                         — list users (admin)
//   PATCH  /api/admin/users/:id                     — set platform role (admin)
//
// /admin/users/:id/deactivate (kills sessions + revokes keys + drops
// memberships) is left as a Phase-2 follow-up — see the proposal §9.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// ─── Members ──────────────────────────────────────────────────────────

func (s *Server) handleProjectMembers(w http.ResponseWriter, r *http.Request) {
	// Path shape: /projects/<pid>/members[/<sub>]
	rest := strings.TrimPrefix(r.URL.Path, "/projects/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "members" {
		http.NotFound(w, r)
		return
	}
	pid := parts[0]
	if pid == "" {
		http.Error(w, "project id required", http.StatusBadRequest)
		return
	}

	// Routing inside /members:
	//   parts == [pid, "members"]                              → list / create-invite
	//   parts == [pid, "members", "invites"]                   → list pending invites
	//   parts == [pid, "members", "invites", "<token>"]        → revoke invite
	//   parts == [pid, "members", "<userId>"]                  → PATCH / DELETE member
	sub := ""
	if len(parts) >= 3 {
		sub = parts[2]
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.handleListMembers(w, r, pid)
		default:
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
		}
	case "invites":
		if len(parts) == 3 {
			switch r.Method {
			case http.MethodGet:
				s.handleListProjectInvites(w, r, pid)
			case http.MethodPost:
				s.handleCreateProjectInvite(w, r, pid)
			default:
				http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
			}
			return
		}
		// /invites/<token>
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		s.handleRevokeProjectInvite(w, r, pid, parts[3])
	default:
		// /members/<userId>
		uid, err := strconv.ParseInt(sub, 10, 64)
		if err != nil {
			http.Error(w, "bad member id", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			s.handleUpdateMember(w, r, pid, uid)
		case http.MethodDelete:
			s.handleRemoveMember(w, r, pid, uid)
		default:
			http.Error(w, "PATCH or DELETE", http.StatusMethodNotAllowed)
		}
	}
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, projectID string) {
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	members, err := s.store.ListProjectMembers(projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, members)
}

func (s *Server) handleUpdateMember(w http.ResponseWriter, r *http.Request, projectID string, targetUserID int64) {
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectOwner); !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	role := ProjectRole(body.Role)
	if !validProjectRole(role) {
		http.Error(w, "role must be viewer, editor, or owner", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateProjectMember(projectID, targetUserID, role); err != nil {
		if errors.Is(err, ErrLastOwner) {
			http.Error(w, ErrLastOwner.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, projectID string, targetUserID int64) {
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectOwner); !ok {
		return
	}
	if err := s.store.RemoveProjectMember(projectID, targetUserID); err != nil {
		if errors.Is(err, ErrLastOwner) {
			http.Error(w, ErrLastOwner.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "removed"})
}

// ─── Invites ──────────────────────────────────────────────────────────

func (s *Server) handleListProjectInvites(w http.ResponseWriter, r *http.Request, projectID string) {
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	invites, err := s.store.ListProjectInvites(projectID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, invites)
}

func (s *Server) handleCreateProjectInvite(w http.ResponseWriter, r *http.Request, projectID string) {
	uid, _, ok := s.requireProjectAccess(w, r, projectID, ProjectOwner)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	role := ProjectRole(body.Role)
	if role == "" {
		role = ProjectEditor
	}
	if !validProjectRole(role) {
		http.Error(w, "role must be viewer, editor, or owner", http.StatusBadRequest)
		return
	}
	// Auto-promote: if there's already a user with this email on this
	// install, skip the invite-link dance and add them as a member
	// directly. The wire shape carries a `kind` so the dashboard can
	// branch: "added" → refresh member list; "invited" → also refresh
	// pending invites + surface the copy-link button.
	if u, err := s.store.GetUserByEmail(strings.TrimSpace(strings.ToLower(body.Email))); err == nil && u != nil {
		if err := s.store.AddProjectMember(projectID, u.ID, role, uid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"kind":    "added",
			"user_id": u.ID,
			"email":   u.Email,
			"role":    string(role),
		})
		return
	}
	inv, err := s.store.CreateInvite(projectID, body.Email, role, uid, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"kind":   "invited",
		"invite": inv,
	})
}

func (s *Server) handleRevokeProjectInvite(w http.ResponseWriter, r *http.Request, projectID string, token string) {
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectOwner); !ok {
		return
	}
	if err := s.store.RevokeInvite(token); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "revoked"})
}

// ─── Public-ish invite endpoints ──────────────────────────────────────
//
// GET /api/invites/:token — preview the invite for the login page so
// the banner can say "You've been invited to <Project> by <inviter>".
// Returns the project name + inviter email + role. Does NOT require
// authentication: the token itself is the credential. We don't leak
// project_id details beyond what the inviter clearly intended.
//
// POST /api/invites/:token/accept — accept an invite. Requires a
// logged-in user; their email must match the invite's email
// (case-insensitive) so a leaked link can't be used by someone else.

func (s *Server) handleInvitePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/invites/")
	if strings.Contains(token, "/") {
		// /invites/:token/accept handled by handleInviteAccept
		http.NotFound(w, r)
		return
	}
	inv, err := s.store.GetInviteByToken(token)
	if err != nil {
		http.Error(w, "invite invalid or expired", http.StatusNotFound)
		return
	}
	project, _ := s.store.GetProjectAny(inv.ProjectID)
	inviter, _ := s.store.GetUserByID(inv.InvitedBy)
	resp := map[string]any{
		"email": inv.Email,
		"role":  inv.Role,
	}
	if project != nil {
		resp["project_id"] = project.ID
		resp["project_name"] = project.Name
	}
	if inviter != nil {
		resp["inviter_email"] = inviter.Email
	}
	writeJSON(w, resp)
}

func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Path: /invites/:token/accept
	rest := strings.TrimPrefix(r.URL.Path, "/invites/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "accept" {
		http.NotFound(w, r)
		return
	}
	token := parts[0]

	uid := getUserID(r)
	if uid == 0 {
		http.Error(w, "must be logged in to accept an invite", http.StatusUnauthorized)
		return
	}
	user, err := s.store.GetUserByID(uid)
	if err != nil {
		http.Error(w, "user lookup failed", http.StatusInternalServerError)
		return
	}
	inv, err := s.store.GetInviteByToken(token)
	if err != nil {
		http.Error(w, "invite invalid or expired", http.StatusNotFound)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(inv.Email)) {
		http.Error(w, "invite was issued to a different email", http.StatusForbidden)
		return
	}
	accepted, err := s.store.AcceptInvite(token, uid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Invite-accepting users are joining an existing project on an
	// existing Apteva install — the "Welcome to Apteva" flow doesn't
	// apply. Mark onboarded so OnboardingGate sends them to the
	// dashboard, not /onboarding. Idempotent for users who were
	// already onboarded.
	_ = s.store.MarkUserOnboarded(uid)
	writeJSON(w, map[string]any{
		"status":     "accepted",
		"project_id": accepted.ProjectID,
		"role":       accepted.Role,
	})
}

// ─── Admin users ──────────────────────────────────────────────────────

// GET /api/admin/users — list every user with platform role.
// PATCH /api/admin/users/:id — change platform role.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requirePlatformAdmin(w, r)
	if !ok {
		return
	}
	// Either /admin/users (list) or /admin/users/<id> (patch).
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		users, err := s.store.ListUsers()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, users)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	targetID, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return
	}
	// Refuse self-edit to avoid an admin nuking their own access.
	if targetID == caller {
		http.Error(w, ErrSelfRole.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	role := PlatformRole(body.Role)
	if role != PlatformAdmin && role != PlatformUser {
		http.Error(w, "role must be 'user' or 'admin'", http.StatusBadRequest)
		return
	}
	if err := s.store.SetPlatformRole(targetID, role); err != nil {
		if errors.Is(err, ErrLastAdmin) {
			http.Error(w, ErrLastAdmin.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
