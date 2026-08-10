package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// handleSetInstallAgentDefault controls the creation-time policy for one app
// installation. It never mutates existing agents: their app attachments are
// deliberate snapshots which remain editable from the agent detail page.
func (s *Server) handleSetInstallAgentDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apps/installs/")
	idText := strings.TrimSuffix(rest, "/agent-default")
	installID, err := atoi64(idText)
	if err != nil || installID <= 0 {
		http.Error(w, "invalid install id", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled *bool `json:"default_for_new_agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, "default_for_new_agents is required", http.StatusBadRequest)
		return
	}
	if *body.Enabled {
		var eligible int
		err := s.store.db.QueryRow(`
			SELECT CASE WHEN EXISTS (
				SELECT 1 FROM mcp_servers m WHERE m.upstream_id=?
			) OR EXISTS (
				SELECT 1 FROM skills sk WHERE sk.install_id=? AND sk.enabled=1
			) THEN 1 ELSE 0 END
			FROM app_installs i WHERE i.id=?`,
			appMCPUpstreamID(installID), installID, installID,
		).Scan(&eligible)
		if err == sql.ErrNoRows {
			http.Error(w, "install not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to inspect app", http.StatusInternalServerError)
			return
		}
		if eligible == 0 {
			http.Error(w, "app has no agent tools or skills", http.StatusBadRequest)
			return
		}
	}
	enabled := 0
	if *body.Enabled {
		enabled = 1
	}
	res, err := s.store.db.Exec(`UPDATE app_installs SET default_for_new_agents=? WHERE id=?`, enabled, installID)
	if err != nil {
		http.Error(w, "failed to update app default", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"install_id":             installID,
		"default_for_new_agents": *body.Enabled,
	})
}

// defaultAppInstallIDsForProject resolves the defaults visible to a new agent.
// A project installation wins over a global installation of the same app.
// Only running apps which can actually contribute an MCP surface or skill are
// eligible; UI-only apps are intentionally absent from an agent's config.
func (s *Server) defaultAppInstallIDsForProject(projectID string) ([]int64, error) {
	rows, err := s.store.db.Query(`
		SELECT i.id, a.name, COALESCE(i.project_id,'')
		FROM app_installs i
		JOIN apps a ON a.id=i.app_id
		WHERE i.default_for_new_agents=1
		  AND i.status='running'
		  AND (COALESCE(i.project_id,'')='' OR i.project_id=?)
		  AND (
			EXISTS (SELECT 1 FROM mcp_servers m WHERE m.upstream_id='app:' || i.id)
			OR EXISTS (SELECT 1 FROM skills sk WHERE sk.install_id=i.id AND sk.enabled=1)
		  )
		ORDER BY a.name,
			CASE WHEN i.project_id=? AND ?<>'' THEN 0 ELSE 1 END,
			i.id`, projectID, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list default apps: %w", err)
	}
	defer rows.Close()
	ids := []int64{}
	seenApps := map[string]bool{}
	for rows.Next() {
		var id int64
		var name, installProject string
		if err := rows.Scan(&id, &name, &installProject); err != nil {
			return nil, fmt.Errorf("scan default app: %w", err)
		}
		if projectID == "" && installProject != "" {
			continue
		}
		if seenApps[name] {
			continue
		}
		seenApps[name] = true
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list default apps: %w", err)
	}
	return ids, nil
}
