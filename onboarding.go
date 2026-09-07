package main

import (
	"database/sql"
	"fmt"
	"net/http"
)

// Onboarding uses the user's own workspace and the same provider resolution as
// agent startup. Managed/shared credentials remain server-side.
func (s *Server) handleOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	projects, err := s.store.ListProjects(userID)
	if err != nil {
		http.Error(w, "Could not load your workspace", http.StatusInternalServerError)
		return
	}
	if len(projects) == 0 {
		http.Error(w, "Your workspace is not available. Please contact your administrator.", http.StatusConflict)
		return
	}
	projectID := projects[0].ID
	// Reuse the starter on retries, including accounts whose one-agent quota
	// is now full. The regular create endpoint enforces quota before replay.
	var starterID int64
	err = s.store.db.QueryRow(`SELECT a.id FROM agent_creation_keys k
		JOIN agents a ON a.id=k.agent_id
		WHERE k.project_id=? AND k.scope_user_id=0 AND k.idempotency_key=?
		AND a.user_id=? AND a.project_id=?`, projectID, fmt.Sprintf("onboarding-starter:%d", userID), userID, projectID).Scan(&starterID)
	if err != nil && err != sql.ErrNoRows {
		http.Error(w, "Could not load your assistant", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"project_id":          projectID,
		"provider_configured": len(s.GetProviderPool(userID, projectID)) > 0,
		"can_manage_provider": s.capabilityAllowed(userID, "provider_management"),
		"starter_agent_id":    starterID,
	})
}
