package main

// store_roles.go — store helpers for the two-tier role system added in
// Phase 1 of multi-user. See store.go's migrate() for the underlying
// schema (users.role, project_members, project_invites).

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ─── Role enums (server-side) ─────────────────────────────────────────

// PlatformRole is the user-level role stored in users.role.
type PlatformRole string

const (
	PlatformUser  PlatformRole = "user"
	PlatformAdmin PlatformRole = "admin"
)

// ProjectRole is the per-project role stored in project_members.role.
// rank() expresses the partial order owner > editor > viewer so the
// authz helper can compare "have" against "need" with one int compare.
type ProjectRole string

const (
	ProjectViewer ProjectRole = "viewer"
	ProjectEditor ProjectRole = "editor"
	ProjectOwner  ProjectRole = "owner"
)

func (r ProjectRole) Rank() int {
	switch r {
	case ProjectOwner:
		return 3
	case ProjectEditor:
		return 2
	case ProjectViewer:
		return 1
	default:
		return 0
	}
}

// ─── Wire shapes (returned to the dashboard) ──────────────────────────

type ProjectMember struct {
	ProjectID string    `json:"project_id"`
	UserID    int64     `json:"user_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	AddedBy   int64     `json:"added_by,omitempty"`
	AddedAt   time.Time `json:"added_at"`
}

type ProjectInvite struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	InvitedBy  int64      `json:"invited_by"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ─── Errors ───────────────────────────────────────────────────────────

var (
	ErrLastAdmin = errors.New("at least one admin must remain")
	ErrLastOwner = errors.New("at least one owner must remain on the project")
	ErrInviteBad = errors.New("invite invalid, expired, or already accepted")
	ErrSelfRole  = errors.New("you cannot change your own platform role")
)

// ─── Platform role ────────────────────────────────────────────────────

// GetPlatformRole returns the user's platform role. Unknown users
// return PlatformUser as a safe default (callers should already have
// validated the user_id before reaching the authz layer).
func (s *Store) GetPlatformRole(userID int64) PlatformRole {
	var role string
	err := s.db.QueryRow(
		"SELECT COALESCE(role,'user') FROM users WHERE id = ?", userID,
	).Scan(&role)
	if err != nil {
		return PlatformUser
	}
	if role == string(PlatformAdmin) {
		return PlatformAdmin
	}
	return PlatformUser
}

// SetPlatformRole updates a user's platform role with the "at least one
// admin must remain" invariant. Pass userID==self to allow self-edits
// only if there's at least one OTHER admin left; the auth.go handler
// adds a stricter "you can't demote yourself at all" check on top.
func (s *Store) SetPlatformRole(userID int64, role PlatformRole) error {
	if role != PlatformAdmin && role != PlatformUser {
		return fmt.Errorf("invalid platform role: %s", role)
	}
	// Wrap in a transaction so the count-and-update is atomic.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role == PlatformUser {
		// Block any demotion that would leave zero admins.
		var others int
		if err := tx.QueryRow(
			"SELECT COUNT(*) FROM users WHERE role = 'admin' AND id != ?", userID,
		).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(
		"UPDATE users SET role = ? WHERE id = ?", string(role), userID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ─── Project role lookups ─────────────────────────────────────────────

// GetProjectRole returns the user's role on a project, or
// (sql.ErrNoRows, "") if they aren't a member. The authz helper
// checks admin status FIRST so this only matters for non-admin users.
func (s *Store) GetProjectRole(projectID string, userID int64) (ProjectRole, error) {
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	).Scan(&role)
	if err != nil {
		return "", err
	}
	return ProjectRole(role), nil
}

// ListProjectMembers returns every member of a project with their
// email joined in for direct rendering. Owners first, then editors,
// then viewers; within a tier ordered by added_at.
func (s *Store) ListProjectMembers(projectID string) ([]ProjectMember, error) {
	rows, err := s.db.Query(`
		SELECT m.project_id, m.user_id, u.email, m.role,
		       COALESCE(m.added_by, 0), m.added_at
		  FROM project_members m
		  JOIN users u ON u.id = m.user_id
		 WHERE m.project_id = ?
		 ORDER BY CASE m.role
		            WHEN 'owner'  THEN 0
		            WHEN 'editor' THEN 1
		            WHEN 'viewer' THEN 2
		          END, m.added_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		var addedAt string
		if err := rows.Scan(&m.ProjectID, &m.UserID, &m.Email, &m.Role, &m.AddedBy, &addedAt); err != nil {
			return nil, err
		}
		m.AddedAt, _ = parseTime(addedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddProjectMember inserts a membership row. Idempotent — re-adding
// an existing member updates their role.
func (s *Store) AddProjectMember(projectID string, userID int64, role ProjectRole, addedBy int64) error {
	if !validProjectRole(role) {
		return fmt.Errorf("invalid project role: %s", role)
	}
	_, err := s.db.Exec(`
		INSERT INTO project_members (project_id, user_id, role, added_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, user_id) DO UPDATE SET role = excluded.role`,
		projectID, userID, string(role), addedBy,
	)
	return err
}

// UpdateProjectMember changes a member's role, with the "at least one
// owner per project" invariant when demoting the last owner.
func (s *Store) UpdateProjectMember(projectID string, userID int64, role ProjectRole) error {
	if !validProjectRole(role) {
		return fmt.Errorf("invalid project role: %s", role)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role != ProjectOwner {
		// Block a demotion that would leave the project ownerless.
		var others int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM project_members
			  WHERE project_id = ? AND role = 'owner' AND user_id != ?`,
			projectID, userID,
		).Scan(&others); err != nil {
			return err
		}
		// Need to know the user's CURRENT role too — only block if
		// they're currently the sole owner.
		var current string
		_ = tx.QueryRow(
			`SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`,
			projectID, userID,
		).Scan(&current)
		if current == "owner" && others == 0 {
			return ErrLastOwner
		}
	}
	res, err := tx.Exec(
		`UPDATE project_members SET role = ? WHERE project_id = ? AND user_id = ?`,
		string(role), projectID, userID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// RemoveProjectMember drops a membership row. Same last-owner guard
// as UpdateProjectMember.
func (s *Store) RemoveProjectMember(projectID string, userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	_ = tx.QueryRow(
		`SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	).Scan(&role)
	if role == "owner" {
		var others int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM project_members
			  WHERE project_id = ? AND role = 'owner' AND user_id != ?`,
			projectID, userID,
		).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastOwner
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ListProjectsForUser returns every project visible to this user:
//   - explicit project_members rows (any role), AND
//   - if the user is platform admin, every project on the server.
//
// The single UNION query is cheaper than a per-call admin check in Go.
func (s *Store) ListProjectsForUser(userID int64) ([]Project, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.user_id, p.name, COALESCE(p.description,''),
		       COALESCE(p.color,''), p.created_at
		  FROM projects p
		 WHERE p.id IN (SELECT project_id FROM project_members WHERE user_id = ?)
		    OR (SELECT role FROM users WHERE id = ?) = 'admin'
		 ORDER BY p.created_at ASC`,
		userID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		var createdAt string
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Description, &p.Color, &createdAt); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = parseTime(createdAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── Invites ──────────────────────────────────────────────────────────

// CreateInvite mints an invite token + writes the row. Token is 32
// bytes hex (64 chars) — safe to embed in a URL slug.
func (s *Store) CreateInvite(projectID, email string, role ProjectRole, invitedBy int64, ttl time.Duration) (*ProjectInvite, error) {
	if !validProjectRole(role) {
		return nil, fmt.Errorf("invalid project role: %s", role)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, fmt.Errorf("email required")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	tokBytes := make([]byte, 16)
	if _, err := rand.Read(tokBytes); err != nil {
		return nil, err
	}
	id := "inv_" + hex.EncodeToString(tokBytes)
	expires := time.Now().Add(ttl).UTC()
	if _, err := s.db.Exec(`
		INSERT INTO project_invites (id, project_id, email, role, invited_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, email, string(role), invitedBy, expires,
	); err != nil {
		return nil, err
	}
	return &ProjectInvite{
		ID: id, ProjectID: projectID, Email: email, Role: string(role),
		InvitedBy: invitedBy, ExpiresAt: expires, CreatedAt: time.Now().UTC(),
	}, nil
}

// GetInviteByToken loads an invite if it's still valid (not expired,
// not yet accepted). Returns ErrInviteBad otherwise so callers don't
// have to disambiguate "missing" vs "stale".
func (s *Store) GetInviteByToken(token string) (*ProjectInvite, error) {
	var inv ProjectInvite
	var expires string
	var acceptedAt sql.NullString
	var createdAt string
	err := s.db.QueryRow(`
		SELECT id, project_id, email, role, invited_by, expires_at, accepted_at, created_at
		  FROM project_invites WHERE id = ?`, token,
	).Scan(&inv.ID, &inv.ProjectID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&expires, &acceptedAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInviteBad
		}
		return nil, err
	}
	inv.ExpiresAt, _ = parseTime(expires)
	inv.CreatedAt, _ = parseTime(createdAt)
	if acceptedAt.Valid {
		t, _ := parseTime(acceptedAt.String)
		inv.AcceptedAt = &t
	}
	if inv.AcceptedAt != nil || time.Now().UTC().After(inv.ExpiresAt) {
		return nil, ErrInviteBad
	}
	return &inv, nil
}

// AcceptInvite atomically marks an invite accepted AND adds the user
// as a member. Caller has already validated the user's email matches
// the invite's email (case-insensitive).
func (s *Store) AcceptInvite(token string, userID int64) (*ProjectInvite, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var inv ProjectInvite
	var expires string
	var acceptedAt sql.NullString
	var createdAt string
	if err := tx.QueryRow(`
		SELECT id, project_id, email, role, invited_by, expires_at, accepted_at, created_at
		  FROM project_invites WHERE id = ?`, token,
	).Scan(&inv.ID, &inv.ProjectID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&expires, &acceptedAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInviteBad
		}
		return nil, err
	}
	inv.ExpiresAt, _ = parseTime(expires)
	inv.CreatedAt, _ = parseTime(createdAt)
	if acceptedAt.Valid || time.Now().UTC().After(inv.ExpiresAt) {
		return nil, ErrInviteBad
	}
	if _, err := tx.Exec(`
		INSERT INTO project_members (project_id, user_id, role, added_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, user_id) DO UPDATE SET role = excluded.role`,
		inv.ProjectID, userID, inv.Role, inv.InvitedBy,
	); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`
		UPDATE project_invites SET accepted_at = CURRENT_TIMESTAMP WHERE id = ?`, token,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv.AcceptedAt = &now
	return &inv, nil
}

// ListProjectInvites returns pending (un-accepted, un-expired) invites
// for a project. Includes invitedBy so the UI can show "invited by X".
func (s *Store) ListProjectInvites(projectID string) ([]ProjectInvite, error) {
	rows, err := s.db.Query(`
		SELECT id, project_id, email, role, invited_by, expires_at, accepted_at, created_at
		  FROM project_invites
		 WHERE project_id = ? AND accepted_at IS NULL AND expires_at > CURRENT_TIMESTAMP
		 ORDER BY created_at DESC`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectInvite{}
	for rows.Next() {
		var inv ProjectInvite
		var expires, createdAt string
		var acceptedAt sql.NullString
		if err := rows.Scan(&inv.ID, &inv.ProjectID, &inv.Email, &inv.Role,
			&inv.InvitedBy, &expires, &acceptedAt, &createdAt); err != nil {
			return nil, err
		}
		inv.ExpiresAt, _ = parseTime(expires)
		inv.CreatedAt, _ = parseTime(createdAt)
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite deletes a pending invite by id. Idempotent — deleting
// an already-accepted or already-deleted invite is not an error.
func (s *Store) RevokeInvite(inviteID string) error {
	_, err := s.db.Exec(
		`DELETE FROM project_invites WHERE id = ? AND accepted_at IS NULL`,
		inviteID,
	)
	return err
}

// ─── Helpers ──────────────────────────────────────────────────────────

func validProjectRole(r ProjectRole) bool {
	return r == ProjectViewer || r == ProjectEditor || r == ProjectOwner
}
