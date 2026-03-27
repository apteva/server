package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GET /projects
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	projects, err := s.store.ListProjects(userID)
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
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	id := strings.TrimPrefix(r.URL.Path, "/projects/")

	switch r.Method {
	case http.MethodGet:
		project, err := s.store.GetProject(userID, id)
		if err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		writeJSON(w, project)

	case http.MethodPut:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Color       string `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateProject(userID, id, body.Name, body.Description, body.Color); err != nil {
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		project, _ := s.store.GetProject(userID, id)
		writeJSON(w, project)

	case http.MethodDelete:
		s.store.DeleteProject(userID, id)
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}
