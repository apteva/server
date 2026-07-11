package main

import (
	"database/sql"
	"net/http"
)

// requireScopedProjectAccess applies the platform-wide rule for resources that
// may be global. Global mutations are admin-only; project resources use the
// normal membership roles.
func (s *Server) requireScopedProjectAccess(w http.ResponseWriter, r *http.Request, projectID string, need ProjectRole) bool {
	if projectID == "" {
		_, ok := s.requirePlatformAdmin(w, r)
		return ok
	}
	_, _, ok := s.requireProjectAccess(w, r, projectID, need)
	return ok
}

func (s *Server) requireAppInstallAccess(w http.ResponseWriter, r *http.Request, installID int64, need ProjectRole) (string, bool) {
	var projectID string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID).Scan(&projectID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "install not found", http.StatusNotFound)
		} else {
			http.Error(w, "authorization lookup failed", http.StatusInternalServerError)
		}
		return "", false
	}
	return projectID, s.requireScopedProjectAccess(w, r, projectID, need)
}

func (s *Server) requireSkillAccess(w http.ResponseWriter, r *http.Request, skillID int64, need ProjectRole) (string, bool) {
	var projectID string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM skills WHERE id=?`, skillID).Scan(&projectID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "skill not found", http.StatusNotFound)
		} else {
			http.Error(w, "authorization lookup failed", http.StatusInternalServerError)
		}
		return "", false
	}
	return projectID, s.requireScopedProjectAccess(w, r, projectID, need)
}

func (s *Server) requireAgentAccess(w http.ResponseWriter, r *http.Request, agentID int64, need ProjectRole) (*Agent, bool) {
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return nil, false
	}
	if agent.ProjectID == "" {
		uid := getUserID(r)
		if uid == 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		if uid != agent.UserID && s.store.GetPlatformRole(uid) != PlatformAdmin {
			http.Error(w, "agent not found", http.StatusNotFound)
			return nil, false
		}
	} else if _, _, ok := s.requireProjectAccess(w, r, agent.ProjectID, need); !ok {
		return nil, false
	}
	return agent, true
}
