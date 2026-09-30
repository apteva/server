package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Global dashboard widgets use the same atomic per-surface update as project
// widgets. A full preferences PUT can otherwise restore an older sidebar.
func (s *Server) handleUILayoutGlobalSurface(w http.ResponseWriter, r *http.Request) {
	surface := strings.TrimPrefix(r.URL.Path, "/ui-layout/global/surfaces/")
	if !validUILayoutSurface(surface) {
		http.Error(w, "invalid UI layout surface", http.StatusBadRequest)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
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
		layout, revision, err := s.store.PatchUserUILayoutGlobalSurface(userID, surface, body.Value)
		if errors.Is(err, errUILayoutConflict) {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "layout changed in another session", "ui_layout": layout, "revision": revision})
			return
		}
		if err != nil {
			http.Error(w, "failed to update layout", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"value": entries, "ui_layout": layout, "revision": revision})
	default:
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
	}
}

func (s *Store) PatchUserUILayoutGlobalSurface(userID int64, surface string, value json.RawMessage) (json.RawMessage, int64, error) {
	if len(value) > 64<<10 || !json.Valid(value) {
		return nil, 0, errors.New("surface layout must be valid JSON no larger than 64 KiB")
	}
	if _, err := s.db.Exec(`INSERT INTO user_preferences (user_id, ui_layout, ui_layout_revision, updated_at)
		VALUES (?, '{}', 0, CURRENT_TIMESTAMP) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return nil, 0, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, revision := s.GetUserUILayoutWithRevision(userID)
		var document map[string]any
		if err := json.Unmarshal(current, &document); err != nil || document == nil {
			document = map[string]any{}
		}
		global, _ := document["global"].(map[string]any)
		if global == nil {
			global = map[string]any{}
			document["global"] = global
		}
		slots, _ := global["slots"].(map[string]any)
		if slots == nil {
			slots = map[string]any{}
			global["slots"] = slots
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, revision, err
		}
		slots[surface] = decoded
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
	current, revision := s.GetUserUILayoutWithRevision(userID)
	return current, revision, errUILayoutConflict
}
