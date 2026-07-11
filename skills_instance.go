package main

// Per-instance skill assignment endpoints. The journal is the source
// of truth for "which skills are assigned to which agent" — these
// handlers are the thin REST surface that translates dashboard
// actions into journal pushes/drops.
//
//   GET    /api/instances/:id/skills              list assigned skills + drift status
//   POST   /api/instances/:id/skills/:skill_id    assign (push to journal)
//   DELETE /api/instances/:id/skills/:skill_id    unassign (tombstone)

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// instanceSkillView is the row shape returned by GET — one entry per
// (catalog skill ∪ orphan journal record). The dashboard renders
// these as cards with a status badge.
type instanceSkillView struct {
	SkillID     int64     `json:"skill_id"` // 0 for orphaned journal entries
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Source      string    `json:"source"` // app | user | builtin | "" for orphan
	AppName     string    `json:"app_name,omitempty"`
	MemoryID    string    `json:"memory_id,omitempty"` // present when status != missing
	PushedAt    time.Time `json:"pushed_at,omitempty"`
	Status      string    `json:"status"` // synced | stale | missing | orphaned
}

// handleInstanceSkills dispatches GET /skills, POST /skills/:id, DELETE /skills/:id.
func (s *Server) handleInstanceSkills(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/instances/")

	// Split into <instance-id>/skills[/<skill-id>].
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "skills" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	instanceID, err := atoi64(parts[0])
	if err != nil {
		http.Error(w, "invalid instance ID", http.StatusBadRequest)
		return
	}
	need := ProjectViewer
	if r.Method != http.MethodGet {
		need = ProjectEditor
	}
	inst, ok := s.requireAgentAccess(w, r, instanceID, need)
	if !ok {
		return
	}

	switch len(parts) {
	case 2:
		// /instances/:id/skills — list only
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.handleListInstanceSkills(w, r, inst)
		return

	case 3:
		// /instances/:id/skills/:skill_id — assign / unassign
		skillID, err := atoi64(parts[2])
		if err != nil {
			http.Error(w, "invalid skill ID", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.handleAssignInstanceSkill(w, r, inst, skillID)
		case http.MethodDelete:
			s.handleUnassignInstanceSkill(w, r, inst, skillID)
		default:
			http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
		}
		return
	}
	http.Error(w, "bad path", http.StatusBadRequest)
}

// GET /instances/:id/skills
//
// Returns one row per skill visible to the project + one row per
// orphaned journal record. Reads the journal from disk (works for
// running and stopped instances; the runtime is single-writer per
// file so a concurrent-append race at worst returns N-1 rows).
func (s *Server) handleListInstanceSkills(w http.ResponseWriter, r *http.Request, inst *Agent) {
	dir := s.agents.instanceDir(inst.ID)
	journalPath := filepath.Join(dir, "memory.jsonl")

	active, err := journalActiveSkillRecords(journalPath)
	if err != nil {
		http.Error(w, "read journal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cat, err := s.listProjectSkills(inst.ProjectID)
	if err != nil {
		http.Error(w, "list catalog: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, computeInstanceSkillView(cat, active))
}

// computeInstanceSkillView is the pure status-computation step pulled
// out of the handler for testability. Given a catalog (enabled skills
// the project can see) and the active skill-records on the agent's
// journal (keyed by slug), produces the instanceSkillView slice with
// per-skill status:
//
//	synced   — record present, hash tag matches current body hash
//	stale    — record present, hash differs (catalog body changed)
//	missing  — catalog row exists, no record on agent
//	orphaned — record on agent, no matching catalog row
func computeInstanceSkillView(cat []Skill, active map[string]journalRecord) []instanceSkillView {
	out := make([]instanceSkillView, 0, len(cat))
	seen := map[string]bool{}
	for _, sk := range cat {
		row := instanceSkillView{
			SkillID:     sk.ID,
			Slug:        sk.Slug,
			Name:        sk.Name,
			Description: sk.Description,
			Source:      sk.Source,
			AppName:     sk.AppName,
		}
		rec, ok := active[sk.Slug]
		if !ok {
			row.Status = "missing"
		} else {
			row.MemoryID = rec.ID
			row.PushedAt = rec.TS
			seen[sk.Slug] = true
			if extractTag(rec.Tags, SkillHashTagPrefix) == skillBodyHash(sk.Body) {
				row.Status = "synced"
			} else {
				row.Status = "stale"
			}
		}
		out = append(out, row)
	}
	for slug, rec := range active {
		if seen[slug] {
			continue
		}
		out = append(out, instanceSkillView{
			Slug:     slug,
			Name:     slug,
			MemoryID: rec.ID,
			PushedAt: rec.TS,
			Status:   "orphaned",
			Source:   extractTag(rec.Tags, SkillSourceTagPrefix),
			AppName:  extractTag(rec.Tags, SkillAppTagPrefix),
		})
	}
	return out
}

// POST /instances/:id/skills/:skill_id — assign.
func (s *Server) handleAssignInstanceSkill(w http.ResponseWriter, r *http.Request, inst *Agent, skillID int64) {
	sk, err := s.getSkillForProject(skillID, inst.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "skill not found in this project", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !sk.Enabled {
		http.Error(w, "skill is disabled at the catalog level — enable it before assigning", http.StatusConflict)
		return
	}
	if err := s.PushSkillToInstance(inst.ID, sk); err != nil {
		log.Printf("[SKILLS] assign agent=%d skill=%d failed: %v", inst.ID, skillID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[SKILLS] assigned skill=%d (%s) to agent=%d", sk.ID, sk.Slug, inst.ID)
	writeJSON(w, map[string]any{
		"ok":        true,
		"memory_id": skillMemoryID(sk.ID),
		"slug":      sk.Slug,
	})
}

// DELETE /instances/:id/skills/:skill_id — unassign.
func (s *Server) handleUnassignInstanceSkill(w http.ResponseWriter, r *http.Request, inst *Agent, skillID int64) {
	// We don't require the skill to still exist in the catalog —
	// unassign should work for orphaned records too. Best-effort
	// look-up just for logging.
	slug := ""
	if sk, err := s.getSkillForProject(skillID, inst.ProjectID); err == nil {
		slug = sk.Slug
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "unassigned via dashboard"
	}
	if err := s.RemoveSkillFromInstance(inst.ID, skillID, reason); err != nil {
		log.Printf("[SKILLS] unassign agent=%d skill=%d failed: %v", inst.ID, skillID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[SKILLS] unassigned skill=%d (%s) from agent=%d", skillID, slug, inst.ID)
	writeJSON(w, map[string]any{"ok": true})
}

// ---- catalog queries (lookups skills_handlers.go doesn't already expose) ----

// listProjectSkills returns every enabled skill visible to the project:
// project-scoped rows (project_id == projectID) + globals (project_id == ”).
// Joined with apps for the AppName field.
func (s *Server) listProjectSkills(projectID string) ([]Skill, error) {
	rows, err := s.store.db.Query(`
		SELECT sk.id, sk.slug, sk.name, sk.description, sk.body, sk.source,
		       sk.install_id, sk.project_id, sk.command, sk.metadata_json,
		       sk.enabled, sk.version, sk.created_at, sk.updated_at,
		       COALESCE(a.name, '')
		FROM skills sk
		LEFT JOIN app_installs i ON i.id = sk.install_id
		LEFT JOIN apps a ON a.id = i.app_id
		WHERE sk.enabled = 1 AND (sk.project_id = '' OR sk.project_id = ?)
		ORDER BY sk.source, sk.name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			continue
		}
		out = append(out, sk)
	}
	return out, nil
}

// getSkillForProject loads a skill by id and verifies it's visible to
// the project (own row or a global). Returns sql.ErrNoRows if not.
func (s *Server) getSkillForProject(skillID int64, projectID string) (Skill, error) {
	row := s.store.db.QueryRow(`
		SELECT sk.id, sk.slug, sk.name, sk.description, sk.body, sk.source,
		       sk.install_id, sk.project_id, sk.command, sk.metadata_json,
		       sk.enabled, sk.version, sk.created_at, sk.updated_at,
		       COALESCE(a.name, '')
		FROM skills sk
		LEFT JOIN app_installs i ON i.id = sk.install_id
		LEFT JOIN apps a ON a.id = i.app_id
		WHERE sk.id = ? AND (sk.project_id = '' OR sk.project_id = ?)`,
		skillID, projectID,
	)
	sk, err := scanSkill(row)
	if err != nil {
		return Skill{}, err
	}
	if sk.ID == 0 {
		return Skill{}, sql.ErrNoRows
	}
	return sk, nil
}

// listInstancesInProject returns every instance the user owns whose
// project_id matches. Used by skill-level fan-out hooks (catalog edits
// can re-push to every assigned instance) and by cleanup hooks.
func (s *Server) listInstancesInProject(userID int64, projectID string) ([]Agent, error) {
	insts, err := s.store.ListAgents(userID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return insts, nil
}
