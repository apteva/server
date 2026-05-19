package main

// authz.go — project + platform authorization helpers, the single
// chokepoint every project-scoped handler routes its access check
// through. See store_roles.go for the underlying schema + roles.

import (
	"net/http"
)

// requireProjectAccess gates a request on (a) the caller's platform
// role and (b) their project membership.
//
//   - Platform admins always pass with effective role = owner.
//   - Otherwise, the caller's project_members row must have a role at
//     or above the required rank.
//
// Returns the caller's user_id, their effective role on the project,
// and ok=true on success. On failure the handler must NOT proceed —
// requireProjectAccess writes the response (401 if no caller, 403 if
// insufficient role) and returns ok=false.
func (s *Server) requireProjectAccess(w http.ResponseWriter, r *http.Request, projectID string, need ProjectRole) (uid int64, role ProjectRole, ok bool) {
	uid = getUserID(r)
	if uid == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return 0, "", false
	}
	if s.store.GetPlatformRole(uid) == PlatformAdmin {
		return uid, ProjectOwner, true
	}
	have, err := s.store.GetProjectRole(projectID, uid)
	if err != nil {
		http.Error(w, "not a project member", http.StatusForbidden)
		return uid, "", false
	}
	if have.Rank() < need.Rank() {
		http.Error(w, "insufficient role on project", http.StatusForbidden)
		return uid, have, false
	}
	return uid, have, true
}

// requirePlatformAdmin gates a request on the caller being a platform
// admin. Used by /admin/users and any other server-wide management
// endpoint that should never be reachable as a regular user.
func (s *Server) requirePlatformAdmin(w http.ResponseWriter, r *http.Request) (uid int64, ok bool) {
	uid = getUserID(r)
	if uid == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return 0, false
	}
	if s.store.GetPlatformRole(uid) != PlatformAdmin {
		http.Error(w, "admin only", http.StatusForbidden)
		return uid, false
	}
	return uid, true
}

// effectiveRoleOnProject returns the caller's effective role on the
// project (with the admin short-circuit), without writing HTTP errors.
// Used by read-only handlers that want to expose the role in their
// response payload (e.g. so the dashboard can disable Edit buttons for
// viewers) but don't want to fail the whole request — these handlers
// pair this with requireProjectAccess(... need=ProjectViewer) to gate
// access first, then call effectiveRoleOnProject for the payload role.
func (s *Server) effectiveRoleOnProject(userID int64, projectID string) ProjectRole {
	if s.store.GetPlatformRole(userID) == PlatformAdmin {
		return ProjectOwner
	}
	role, err := s.store.GetProjectRole(projectID, userID)
	if err != nil {
		return ""
	}
	return role
}
