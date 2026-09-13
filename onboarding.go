package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
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
		"project_id":            projectID,
		"provider_configured":   len(s.GetProviderPool(userID, projectID)) > 0,
		"can_manage_provider":   s.capabilityAllowed(userID, "provider_management"),
		"starter_agent_id":      starterID,
		"workspace_preparation": s.interfacePreparationStatus(),
	})
}

// Expose a small product status, not build logs, credentials, or private app data.
func (s *Server) interfacePreparationStatus() map[string]string {
	var status, message string
	err := s.store.db.QueryRow(`SELECT i.status, COALESCE(i.status_message,'') FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name=? AND COALESCE(i.project_id,'')='' ORDER BY i.id LIMIT 1`, defaultConversationsApp).Scan(&status, &message)
	if err != nil {
		return map[string]string{"status": "preparing", "message": "Preparing your workspace…"}
	}
	text := "Preparing your workspace…"
	switch {
	case status == "running":
		text = "Your workspace is ready"
	case status == "error":
		text = "Workspace preparation needs a retry"
	case strings.HasPrefix(message, "Downloading"):
		text = "Downloading Conversations…"
	case strings.HasPrefix(message, "Verifying cached"):
		text = "Checking downloaded Conversations…"
	case strings.HasPrefix(message, "Verifying"), strings.HasPrefix(message, "Extracting"):
		text = "Unpacking Conversations…"
	case strings.HasPrefix(message, "Cloning"):
		text = "Fetching Conversations…"
	case strings.HasPrefix(message, "Compiling"), strings.HasPrefix(message, "Building"), strings.HasPrefix(message, "Linking"):
		text = "Building Conversations…"
	case strings.HasPrefix(message, "Queued"):
		text = "Waiting for workspace preparation…"
	case strings.HasPrefix(message, "Starting"), strings.HasPrefix(message, "Migrat"):
		text = "Starting Conversations…"
	}
	return map[string]string{"status": status, "message": text}
}
