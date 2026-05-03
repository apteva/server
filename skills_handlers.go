package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Skill mirrors the skills table row, with metadata_json already
// parsed into a map. The dashboard reads this shape from
// GET /api/skills; the install pipeline writes one of these per
// app-shipped skill via insertOrReplaceAppSkill.
type Skill struct {
	ID          int64          `json:"id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Body        string         `json:"body"`
	Source      string         `json:"source"`     // app | user | builtin
	InstallID   *int64         `json:"install_id,omitempty"`
	ProjectID   string         `json:"project_id"`
	Command     string         `json:"command,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Enabled     bool           `json:"enabled"`
	Version     string         `json:"version"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	// AppName is set when source='app' so the dashboard can show
	// "From <app-name>" without a second join. Empty for user/builtin.
	AppName string `json:"app_name,omitempty"`
}

// slugRe is the canonical slug shape: lowercase letters, digits,
// dashes. Matches what manifests use for skill names + the prefix
// rules apps already follow for app names.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// commandRe enforces /<slug>. We surface a friendlier error from
// the validator when this fails, but the regex itself is the
// definition.
var commandRe = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]{0,63}$`)

// GET /api/skills?project_id=<id>
//
// Lists every skill visible to the project: project-scoped rows +
// globals (project_id=''). Joined to apps + app_installs to
// surface the AppName for source='app' rows. Returns enabled +
// disabled — the dashboard's filter pills decide what to show.
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	rows, err := s.store.db.Query(`
		SELECT sk.id, sk.slug, sk.name, sk.description, sk.body, sk.source,
		       sk.install_id, sk.project_id, sk.command, sk.metadata_json,
		       sk.enabled, sk.version, sk.created_at, sk.updated_at,
		       COALESCE(a.name, '')
		FROM skills sk
		LEFT JOIN app_installs i ON i.id = sk.install_id
		LEFT JOIN apps a ON a.id = i.app_id
		WHERE sk.project_id = '' OR sk.project_id = ?
		ORDER BY sk.source, sk.name`,
		projectID,
	)
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			continue
		}
		out = append(out, sk)
	}
	writeJSON(w, out)
}

// GET /api/skills/:id — full body included (the list view truncates
// for the row preview but the side panel needs the whole markdown).
func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseSkillIDFromPath(r.URL.Path, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	row := s.store.db.QueryRow(`
		SELECT sk.id, sk.slug, sk.name, sk.description, sk.body, sk.source,
		       sk.install_id, sk.project_id, sk.command, sk.metadata_json,
		       sk.enabled, sk.version, sk.created_at, sk.updated_at,
		       COALESCE(a.name, '')
		FROM skills sk
		LEFT JOIN app_installs i ON i.id = sk.install_id
		LEFT JOIN apps a ON a.id = i.app_id
		WHERE sk.id = ?`, id,
	)
	sk, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "skill not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, sk)
}

// POST /api/skills
//
// Body: {name, description, body, command?, project_id, metadata?}
//
// Always source='user'. Validates that name is slug-shaped, command
// (if set) is /<slug>-shaped and not colliding in this project.
// Slug = "user:<name>" so user-authored skills can't shadow app-shipped
// ones (which use "<app>:<name>").
func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Body        string         `json:"body"`
		Command     string         `json:"command"`
		ProjectID   string         `json:"project_id"`
		Metadata    map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Description = strings.TrimSpace(body.Description)
	body.Command = strings.TrimSpace(body.Command)
	if !slugRe.MatchString(body.Name) {
		http.Error(w, "name must be lowercase slug (a-z, 0-9, -)", http.StatusBadRequest)
		return
	}
	if body.Description == "" {
		http.Error(w, "description required", http.StatusBadRequest)
		return
	}
	if body.Command != "" && !commandRe.MatchString(body.Command) {
		http.Error(w, "command must look like /slug", http.StatusBadRequest)
		return
	}
	slug := "user:" + body.Name
	metaJSON, _ := json.Marshal(coalesceMetadata(body.Metadata))
	res, err := s.store.db.Exec(`
		INSERT INTO skills (slug, name, description, body, source, project_id, command, metadata_json)
		VALUES (?, ?, ?, ?, 'user', ?, ?, ?)`,
		slug, body.Name, body.Description, body.Body, body.ProjectID, body.Command, string(metaJSON),
	)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "name or command already in use in this project", http.StatusConflict)
			return
		}
		http.Error(w, "insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, map[string]any{"id": id})
}

// PUT /api/skills/:id
//
// Body: {name?, description?, body?, command?, metadata?}
//
// Only updates provided fields. Rejects edits on source='app' /
// 'builtin' rows (those track their source-of-truth elsewhere).
// Slug is immutable.
func (s *Server) handleUpdateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseSkillIDFromPath(r.URL.Path, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var src string
	if err := s.store.db.QueryRow(`SELECT source FROM skills WHERE id = ?`, id).Scan(&src); err != nil {
		http.Error(w, "skill not found", http.StatusNotFound)
		return
	}
	if src != "user" {
		http.Error(w, fmt.Sprintf("cannot edit skill with source=%q — managed by the platform", src), http.StatusForbidden)
		return
	}
	var body struct {
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Body        *string         `json:"body"`
		Command     *string         `json:"command"`
		Metadata    *map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	sets := []string{}
	args := []any{}
	if body.Name != nil {
		n := strings.TrimSpace(*body.Name)
		if !slugRe.MatchString(n) {
			http.Error(w, "name must be lowercase slug", http.StatusBadRequest)
			return
		}
		sets = append(sets, "name = ?")
		args = append(args, n)
	}
	if body.Description != nil {
		d := strings.TrimSpace(*body.Description)
		if d == "" {
			http.Error(w, "description cannot be empty", http.StatusBadRequest)
			return
		}
		sets = append(sets, "description = ?")
		args = append(args, d)
	}
	if body.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *body.Body)
	}
	if body.Command != nil {
		c := strings.TrimSpace(*body.Command)
		if c != "" && !commandRe.MatchString(c) {
			http.Error(w, "command must look like /slug", http.StatusBadRequest)
			return
		}
		sets = append(sets, "command = ?")
		args = append(args, c)
	}
	if body.Metadata != nil {
		mj, _ := json.Marshal(coalesceMetadata(*body.Metadata))
		sets = append(sets, "metadata_json = ?")
		args = append(args, string(mj))
	}
	if len(sets) == 0 {
		writeJSON(w, map[string]any{"updated": 0})
		return
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, id)
	if _, err := s.store.db.Exec(
		`UPDATE skills SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...,
	); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "name or command already in use in this project", http.StatusConflict)
			return
		}
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"updated": 1})
}

// DELETE /api/skills/:id
//
// User skills only — app-shipped rows are removed via uninstall.
func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseSkillIDFromPath(r.URL.Path, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var src string
	if err := s.store.db.QueryRow(`SELECT source FROM skills WHERE id = ?`, id).Scan(&src); err != nil {
		http.Error(w, "skill not found", http.StatusNotFound)
		return
	}
	if src != "user" {
		http.Error(w, fmt.Sprintf("cannot delete skill with source=%q — uninstall the owning app instead", src), http.StatusForbidden)
		return
	}
	if _, err := s.store.db.Exec(`DELETE FROM skills WHERE id = ?`, id); err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": 1})
}

// PUT /api/skills/:id/enabled
//
// Body: {enabled: bool}
//
// Works on every source — operators can disable an app-shipped skill
// without uninstalling the app, same way npm scripts let you disable
// a hook without removing the package.
func (s *Server) handleSetSkillEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := parseSkillIDFromPath(r.URL.Path, "/enabled")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	v := 0
	if body.Enabled {
		v = 1
	}
	if _, err := s.store.db.Exec(
		`UPDATE skills SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		v, id,
	); err != nil {
		http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"enabled": body.Enabled})
}

// --- helpers --------------------------------------------------------

// scanSkill works against either *sql.Row or *sql.Rows (both expose
// Scan in the same shape) so handleGetSkill and handleListSkills can
// share the same column-list-to-Skill mapping. The metadata_json
// column is parsed into a map; failures fall back to an empty map
// rather than aborting the request.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSkill(r rowScanner) (Skill, error) {
	var sk Skill
	var installID sql.NullInt64
	var metaJSON string
	var enabledInt int
	if err := r.Scan(&sk.ID, &sk.Slug, &sk.Name, &sk.Description, &sk.Body, &sk.Source,
		&installID, &sk.ProjectID, &sk.Command, &metaJSON,
		&enabledInt, &sk.Version, &sk.CreatedAt, &sk.UpdatedAt, &sk.AppName); err != nil {
		return sk, err
	}
	if installID.Valid {
		v := installID.Int64
		sk.InstallID = &v
	}
	sk.Enabled = enabledInt != 0
	if metaJSON != "" {
		_ = json.Unmarshal([]byte(metaJSON), &sk.Metadata)
	}
	if sk.Metadata == nil {
		sk.Metadata = map[string]any{}
	}
	return sk, nil
}

// parseSkillIDFromPath walks /skills/<id>[/suffix]. Suffix is "" for
// the primary endpoints, "/enabled" for the toggle. Anything else
// 404s — callers must pass the exact suffix they expect.
func parseSkillIDFromPath(urlPath, suffix string) (int64, error) {
	rest := strings.TrimPrefix(urlPath, "/skills/")
	if suffix != "" {
		rest = strings.TrimSuffix(rest, suffix)
	}
	if rest == "" || strings.Contains(rest, "/") {
		return 0, errors.New("invalid path")
	}
	return strconv.ParseInt(rest, 10, 64)
}

// coalesceMetadata defends against nil maps so the JSON marshaller
// emits `{}` instead of `null`. Cosmetic, but the dashboard reads
// metadata as a map and would crash on null.
func coalesceMetadata(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// isUniqueViolation lets handlers translate UNIQUE INDEX failures
// into 409 Conflict without a vendor-specific check elsewhere.
// SQLite reports unique violations through error strings; not the
// cleanest API but stable across the modernc.org/sqlite driver
// versions we use.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}
