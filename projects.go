package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GET /projects
//
// Lists every project the caller can see. For non-admin users that's
// the projects they're an explicit member of (project_members); for
// admins, every project on the server. ListProjectsForUser handles
// both via a UNION in one query.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projects, err := s.store.ListProjectsForUser(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = []Project{}
	}
	writeJSON(w, projects)
}

// POST /projects
//
// Creates a project AND a project_members owner row for the creator,
// so the Members tab reflects ownership honestly even when the creator
// is a platform admin. See the design proposal: "creator always lands
// as owner of their project, regardless of platform role".
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	project, err := s.store.CreateProject(userID, body.Name, body.Description, body.Color)
	if err != nil {
		http.Error(w, "failed to create project", http.StatusInternalServerError)
		return
	}
	writeJSON(w, project)
}

// GET/PUT/DELETE /projects/:id
//
// GET requires viewer+; PUT requires editor+; DELETE requires owner.
// The admin short-circuit in requireProjectAccess gives admins owner
// effective role on every project.
//
// Also dispatches the member + invite sub-routes via prefix-match —
// /projects/<id>/members[/...] is handled by handleProjectMembers,
// since Go's net/http mux only matches by prefix and we can't
// register a separate pattern that's more specific.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/projects/")

	// Sub-route: /projects/<id>/members[/...] → members handler.
	if i := strings.Index(id, "/"); i >= 0 {
		seg := id[i+1:]
		if seg == "members" || strings.HasPrefix(seg, "members/") {
			s.handleProjectMembers(w, r)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		_, _, ok := s.requireProjectAccess(w, r, id, ProjectViewer)
		if !ok {
			return
		}
		// Use the admin-bypassing variant so admins viewing a project
		// they're not a member of still get the row back.
		project, err := s.store.GetProjectAny(id)
		if err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		writeJSON(w, project)

	case http.MethodPut:
		_, _, ok := s.requireProjectAccess(w, r, id, ProjectEditor)
		if !ok {
			return
		}
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Color       string `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateProjectAny(id, body.Name, body.Description, body.Color); err != nil {
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		project, _ := s.store.GetProjectAny(id)
		writeJSON(w, project)

	case http.MethodDelete:
		_, _, ok := s.requireProjectAccess(w, r, id, ProjectOwner)
		if !ok {
			return
		}
		s.store.DeleteProjectAny(id)
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}
