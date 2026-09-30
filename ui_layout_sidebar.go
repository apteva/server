package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func validSidebarAppName(name string) bool {
	return name != "" && len(name) <= 120 && !strings.ContainsAny(name, "/\r\n\x00")
}

func sidebarNames(raw any) []string {
	entries, _ := raw.([]any)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, ok := entry.(string)
		if ok {
			names = append(names, name)
		}
	}
	return uniqueSidebarNames(names)
}

func uniqueSidebarNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !validSidebarAppName(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// PatchUserUILayoutSidebarApp changes one favorite against the latest saved
// layout. Other favorites, projects, and widget slots remain untouched.
func (s *Store) PatchUserUILayoutSidebarApp(userID int64, projectID, appName string, pinned bool, defaults []string) (json.RawMessage, int64, []string, error) {
	if _, err := s.db.Exec(`INSERT INTO user_preferences (user_id, ui_layout, ui_layout_revision, updated_at)
		VALUES (?, '{}', 0, CURRENT_TIMESTAMP) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return nil, 0, nil, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, revision := s.GetUserUILayoutWithRevision(userID)
		var document map[string]any
		if err := json.Unmarshal(current, &document); err != nil || document == nil {
			document = map[string]any{}
		}
		projects, _ := document["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
			document["projects"] = projects
		}
		project, _ := projects[projectID].(map[string]any)
		if project == nil {
			project = map[string]any{}
			projects[projectID] = project
		}
		var names []string
		if stored, ok := project["sidebar"]; ok {
			names = sidebarNames(stored)
		} else {
			names = uniqueSidebarNames(defaults)
		}
		found := false
		for _, name := range names {
			if name == appName {
				found = true
				break
			}
		}
		if pinned && !found {
			names = append(names, appName)
		} else if !pinned && found {
			kept := make([]string, 0, len(names)-1)
			for _, name := range names {
				if name != appName {
					kept = append(kept, name)
				}
			}
			names = kept
		}
		project["sidebar"] = names
		next, err := json.Marshal(document)
		if err != nil {
			return nil, revision, nil, err
		}
		if len(next) > 128<<10 {
			return nil, revision, nil, errors.New("ui_layout must be no larger than 128 KiB")
		}
		result, err := s.db.Exec(`UPDATE user_preferences SET ui_layout=?, ui_layout_revision=ui_layout_revision+1,
			updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND ui_layout_revision=?`, string(next), userID, revision)
		if err != nil {
			return nil, revision, nil, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			return json.RawMessage(next), revision + 1, names, nil
		}
	}
	current, revision := s.GetUserUILayoutWithRevision(userID)
	return current, revision, nil, errUILayoutConflict
}

func (s *Server) handleUILayoutSidebar(w http.ResponseWriter, r *http.Request, projectID string) {
	if projectID == "" || strings.Contains(projectID, "/") {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	var body struct {
		App      string   `json:"app"`
		Pinned   *bool    `json:"pinned"`
		Defaults []string `json:"defaults"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validSidebarAppName(body.App) || body.Pinned == nil || len(body.Defaults) > 100 {
		http.Error(w, "invalid sidebar update", http.StatusBadRequest)
		return
	}
	for _, name := range body.Defaults {
		if !validSidebarAppName(name) {
			http.Error(w, "invalid sidebar default", http.StatusBadRequest)
			return
		}
	}
	layout, revision, names, err := s.store.PatchUserUILayoutSidebarApp(getUserID(r), projectID, body.App, *body.Pinned, body.Defaults)
	if errors.Is(err, errUILayoutConflict) {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "layout changed in another session", "ui_layout": layout, "revision": revision})
		return
	}
	if err != nil {
		http.Error(w, "failed to update sidebar", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"sidebar": names, "ui_layout": layout, "revision": revision})
}
