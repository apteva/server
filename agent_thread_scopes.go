package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// BindAgentThreadScope records the server-validated project in which an
// app-spawned agent thread executes. A durable thread cannot be rebound to a
// different project: doing so would carry its existing history across project
// boundaries. Callers must delete the thread before creating it in a new scope.
func (s *Store) BindAgentThreadScope(agentID int64, threadID, projectID string, sourceInstallID int64) (bool, error) {
	threadID = strings.TrimSpace(threadID)
	projectID = strings.TrimSpace(projectID)
	if agentID <= 0 || threadID == "" || projectID == "" {
		return false, fmt.Errorf("agent_id, thread_id, and project_id required")
	}
	if len(threadID) > 256 || strings.ContainsAny(threadID, "\r\n\x00") {
		return false, fmt.Errorf("invalid thread_id")
	}
	if len(projectID) > 256 || strings.ContainsAny(projectID, "\r\n\x00") {
		return false, fmt.Errorf("invalid project_id")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRow(`SELECT project_id FROM agent_thread_scopes WHERE agent_id=? AND thread_id=?`, agentID, threadID).Scan(&existing)
	switch {
	case err == nil:
		if existing != projectID {
			return false, fmt.Errorf("thread %q is already scoped to another project", threadID)
		}
		if _, err := tx.Exec(`UPDATE agent_thread_scopes SET source_install_id=?, updated_at=CURRENT_TIMESTAMP WHERE agent_id=? AND thread_id=?`, sourceInstallID, agentID, threadID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	case err != sql.ErrNoRows:
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO agent_thread_scopes(agent_id,thread_id,project_id,source_install_id) VALUES(?,?,?,?)`, agentID, threadID, projectID, sourceInstallID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AgentThreadProjectForUser resolves a scope only when the caller agent still
// belongs to the authenticated app owner's user. This keeps an app token plus
// spoofed X-Apteva-Caller-Agent header from selecting another user's scope.
func (s *Store) AgentThreadProjectForUser(userID, agentID int64, threadID string) (string, error) {
	if userID <= 0 || agentID <= 0 || strings.TrimSpace(threadID) == "" {
		return "", nil
	}
	var projectID string
	err := s.db.QueryRow(`
		SELECT ats.project_id
		FROM agent_thread_scopes ats
		JOIN agents a ON a.id=ats.agent_id
		WHERE ats.agent_id=? AND ats.thread_id=? AND a.user_id=?`,
		agentID, strings.TrimSpace(threadID), userID,
	).Scan(&projectID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return projectID, err
}

func (s *Store) DeleteAgentThreadScope(agentID int64, threadID string) error {
	_, err := s.db.Exec(`DELETE FROM agent_thread_scopes WHERE agent_id=? AND thread_id=?`, agentID, strings.TrimSpace(threadID))
	return err
}

// appMCPThreadProject resolves only the identity that Core and the server's
// MCP body rewriter established. Client-supplied project headers are stripped
// by authMiddleware and model-supplied _project_id arguments are ignored.
func (s *Server) appMCPThreadProject(r *http.Request) (string, error) {
	if s == nil || s.store == nil || r == nil {
		return "", nil
	}
	agentID, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Agent")), 10, 64)
	if err != nil || agentID <= 0 {
		return "", nil
	}
	threadID := strings.TrimSpace(r.Header.Get("X-Apteva-Caller-Thread"))
	if threadID == "" {
		return "", nil
	}
	return s.store.AgentThreadProjectForUser(getUserID(r), agentID, threadID)
}
