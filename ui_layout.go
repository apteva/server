package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// /api/ui-layout/projects/:project/surfaces/:surface stores one surface at a
// time. Layouts remain user preferences, but per-surface optimistic revisions
// avoid two open dashboard tabs replacing each other's unrelated changes.
func (s *Server) handleUILayoutSurface(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ui-layout/projects/")
	parts := strings.SplitN(rest, "/surfaces/", 2)
	if len(parts) != 2 || parts[0] == "" || !validUILayoutSurface(parts[1]) {
		http.Error(w, "invalid UI layout path", http.StatusBadRequest)
		return
	}
	projectID, surface := parts[0], parts[1]
	if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		layout, revision := s.store.GetUserUILayoutWithRevision(userID)
		writeUILayoutSurface(w, layout, revision, projectID, surface)
	case http.MethodPatch:
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Value == nil {
			http.Error(w, "value is required", http.StatusBadRequest)
			return
		}
		var entries []any
		if err := json.Unmarshal(body.Value, &entries); err != nil {
			http.Error(w, "surface value must be an array", http.StatusBadRequest)
			return
		}
		var expected *int64
		if raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); raw != "" && raw != "*" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				http.Error(w, "invalid If-Match revision", http.StatusBadRequest)
				return
			}
			expected = &value
		}
		layout, revision, err := s.store.PatchUserUILayoutSurface(userID, projectID, surface, body.Value, expected)
		if errors.Is(err, errUILayoutConflict) {
			w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision, 10)))
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":     "layout changed in another session",
				"ui_layout": layout,
				"revision":  revision,
			})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeUILayoutSurface(w, layout, revision, projectID, surface)
	default:
		http.Error(w, "GET or PATCH only", http.StatusMethodNotAllowed)
	}
}

func validUILayoutSurface(surface string) bool {
	if surface == "" || len(surface) > 80 || strings.Contains(surface, "/") {
		return false
	}
	for _, r := range surface {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func writeUILayoutSurface(w http.ResponseWriter, layout json.RawMessage, revision int64, projectID, surface string) {
	value := json.RawMessage(`[]`)
	var document struct {
		Projects map[string]struct {
			Slots map[string]json.RawMessage `json:"slots"`
		} `json:"projects"`
	}
	if json.Unmarshal(layout, &document) == nil {
		if project, ok := document.Projects[projectID]; ok {
			if stored, ok := project.Slots[surface]; ok && json.Valid(stored) {
				value = stored
			}
		}
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision, 10)))
	writeJSON(w, map[string]any{
		"value":     value,
		"ui_layout": layout,
		"revision":  revision,
	})
}
