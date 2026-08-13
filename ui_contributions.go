package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	sdk "github.com/apteva/app-sdk"
)

type uiContributionEligibility struct {
	App        string `json:"app"`
	Component  string `json:"component"`
	Visibility string `json:"visibility"`
	Eligible   bool   `json:"eligible"`
	Attached   bool   `json:"attached"`
}

// GET /api/ui/contributions resolves contextual placement without teaching the
// UI or apps about Core thread kinds. thread_id is deliberately opaque: it is
// passed as render context, while eligibility depends only on project + agent
// attachment and the component's manifest visibility policy.
func (s *Server) handleUIContributions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	surface := r.URL.Query().Get("surface")
	if projectID == "" || !validUILayoutSurface(surface) {
		http.Error(w, "project_id and a valid surface are required", http.StatusBadRequest)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	if r.URL.Query().Get("agent_id") != "" && (err != nil || agentID <= 0) {
		http.Error(w, "invalid agent_id", http.StatusBadRequest)
		return
	}
	if agentID > 0 {
		agent, err := s.store.GetAgentByID(agentID)
		if err != nil || agent.ProjectID != projectID {
			http.Error(w, "agent not found in project", http.StatusNotFound)
			return
		}
	}

	attached := map[int64]bool{}
	if agentID > 0 {
		rows, err := s.store.db.Query(`SELECT install_id FROM app_agent_bindings WHERE agent_id=? AND enabled=1`, agentID)
		if err == nil {
			for rows.Next() {
				var installID int64
				if rows.Scan(&installID) == nil {
					attached[installID] = true
				}
			}
			rows.Close()
		}
	}

	rows, err := s.store.db.Query(`
		SELECT i.id, a.name, COALESCE(NULLIF(i.manifest_json, ''), a.manifest_json)
		FROM app_installs i JOIN apps a ON a.id=i.app_id
		WHERE (i.project_id='' OR i.project_id=?) AND i.status='running'
		ORDER BY a.name`, projectID)
	if err != nil {
		http.Error(w, "unable to resolve contributions", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	result := make([]uiContributionEligibility, 0)
	seen := map[string]bool{}
	for rows.Next() {
		var installID int64
		var appName, manifestJSON string
		if rows.Scan(&installID, &appName, &manifestJSON) != nil {
			continue
		}
		var manifest sdk.Manifest
		if json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
			continue
		}
		for _, component := range manifest.Provides.UIComponents {
			if !containsString(component.Slots, surface) {
				continue
			}
			visibility := component.Visibility
			if visibility == "" {
				if surface == sdk.UIComponentSlotDashboardHome {
					visibility = sdk.UIComponentVisibilityProject
				} else {
					visibility = sdk.UIComponentVisibilityAttached
				}
			}
			isAttached := attached[installID]
			eligible := agentID == 0 || visibility == sdk.UIComponentVisibilityProject || isAttached
			key := appName + ":" + component.Name
			seen[key] = true
			result = append(result, uiContributionEligibility{
				App: appName, Component: component.Name, Visibility: visibility,
				Eligible: eligible, Attached: isAttached,
			})
		}
	}

	// Integration components are project connections, not agent-bound app
	// installs. Keep them project-visible while using the same response shape.
	if s.catalog != nil {
		connectionRows, err := s.store.db.Query(`SELECT DISTINCT app_slug FROM connections WHERE project_id=? AND status!='disabled'`, projectID)
		if err == nil {
			defer connectionRows.Close()
			for connectionRows.Next() {
				var slug string
				if connectionRows.Scan(&slug) != nil {
					continue
				}
				template := s.catalog.Get(slug)
				if template == nil {
					continue
				}
				for _, component := range template.UIComponents {
					key := slug + ":" + component.Name
					if seen[key] || !containsString(component.Slots, surface) {
						continue
					}
					result = append(result, uiContributionEligibility{
						App: slug, Component: component.Name,
						Visibility: sdk.UIComponentVisibilityProject, Eligible: true,
					})
				}
			}
		}
	}

	writeJSON(w, map[string]any{
		"project_id":    projectID,
		"agent_id":      agentID,
		"thread_id":     r.URL.Query().Get("thread_id"),
		"surface":       surface,
		"contributions": result,
	})
}
