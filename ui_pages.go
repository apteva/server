package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type workspacePage struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope"`
	Kind        string `json:"kind"`
	Pinned      bool   `json:"pinned"`
	Layout      string `json:"layout,omitempty"`
	CreatedAt   string `json:"created_at"`
}

var errPageMissing = errors.New("page not found")

// Pages are personal layouts, using the same project access rules as widgets.
// Mutate one page against the latest document so unrelated widgets and favorites
// cannot be overwritten by a stale Settings tab.
func (s *Server) handleUIPages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		http.Error(w, "POST, PATCH or DELETE only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		ProjectID   string `json:"project_id"`
		Scope       string `json:"scope"`
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Pinned      bool   `json:"pinned"`
		Layout      string `json:"layout"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body) != nil {
		http.Error(w, "invalid page", http.StatusBadRequest)
		return
	}
	if body.Scope != "global" && body.Scope != "project" {
		http.Error(w, "scope must be project or global", http.StatusBadRequest)
		return
	}
	if body.Scope == "project" {
		if body.ProjectID == "" {
			http.Error(w, "project_id required", http.StatusBadRequest)
			return
		}
		if _, _, ok := s.requireProjectAccess(w, r, body.ProjectID, ProjectViewer); !ok {
			return
		}
	} else if body.ProjectID != "" {
		http.Error(w, "global pages cannot specify project_id", http.StatusBadRequest)
		return
	}
	if body.Layout != "" && body.Layout != "grid" && body.Layout != "workspace" {
		http.Error(w, "layout must be grid or workspace", http.StatusBadRequest)
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	body.Description = strings.TrimSpace(body.Description)
	if r.Method != http.MethodDelete && (body.Title == "" || len(body.Title) > 160 || len(body.Description) > 2000) {
		http.Error(w, "title is required (160 bytes max); description must be at most 2000 bytes", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			http.Error(w, "could not create page", 500)
			return
		}
		body.ID = hex.EncodeToString(id[:])
	} else if !validUILayoutSurface(body.ID) || len(body.ID) > 64 {
		http.Error(w, "invalid page id", http.StatusBadRequest)
		return
	}
	page := workspacePage{ID: body.ID, Title: body.Title, Description: body.Description, Scope: body.Scope, Kind: "custom", Pinned: body.Pinned, Layout: body.Layout, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	layout, revision, err := s.store.mutateUIPage(userID, body.ProjectID, page, r.Method)
	if errors.Is(err, errPageMissing) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if errors.Is(err, errUILayoutConflict) {
		http.Error(w, "layout changed; please retry", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"page": page, "ui_layout": layout, "revision": revision})
}

func (s *Store) mutateUIPage(userID int64, projectID string, page workspacePage, method string) (json.RawMessage, int64, error) {
	if _, err := s.db.Exec(`INSERT INTO user_preferences (user_id, ui_layout, ui_layout_revision, updated_at)
 VALUES (?, '{}', 0, CURRENT_TIMESTAMP) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return nil, 0, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, revision := s.GetUserUILayoutWithRevision(userID)
		var document map[string]any
		if json.Unmarshal(current, &document) != nil || document == nil {
			document = map[string]any{}
		}
		var target map[string]any
		if page.Scope == "global" {
			target, _ = document["global"].(map[string]any)
			if target == nil {
				target = map[string]any{}
				document["global"] = target
			}
		} else {
			projects, _ := document["projects"].(map[string]any)
			if projects == nil {
				projects = map[string]any{}
				document["projects"] = projects
			}
			target, _ = projects[projectID].(map[string]any)
			if target == nil {
				target = map[string]any{}
				projects[projectID] = target
			}
		}
		pages, _ := target["pages"].([]any)
		index := -1
		for i, raw := range pages {
			if item, ok := raw.(map[string]any); ok && item["id"] == page.ID {
				index = i
				break
			}
		}
		if method == http.MethodPost {
			if len(pages) >= 100 {
				return nil, revision, errors.New("at most 100 pages per scope")
			}
			pages = append(pages, page)
		} else {
			if index < 0 {
				return nil, revision, errPageMissing
			}
			if method == http.MethodDelete {
				pages = append(pages[:index], pages[index+1:]...)
				if slots, ok := target["slots"].(map[string]any); ok {
					delete(slots, "page."+page.ID)
				}
			} else {
				// Preserve creation metadata and future fields while editing known fields.
				item := pages[index].(map[string]any)
				item["title"] = page.Title
				item["description"] = page.Description
				item["pinned"] = page.Pinned
				if page.Layout != "" {
					item["layout"] = page.Layout
				}
			}
		}
		if pages == nil {
			pages = []any{}
		}
		target["pages"] = pages
		next, err := json.Marshal(document)
		if err != nil {
			return nil, revision, err
		}
		if len(next) > 128<<10 {
			return nil, revision, errors.New("ui_layout must be no larger than 128 KiB")
		}
		result, err := s.db.Exec(`UPDATE user_preferences SET ui_layout=?, ui_layout_revision=ui_layout_revision+1,
   updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND ui_layout_revision=?`, string(next), userID, revision)
		if err != nil {
			return nil, revision, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			return json.RawMessage(next), revision + 1, nil
		}
	}
	return nil, 0, errUILayoutConflict
}
