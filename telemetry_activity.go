package main

import (
	"net/http"
	"strconv"
)

// One project history query replaces a request per agent. Stream updates keep
// the feed live after this initial/cursor-based catch-up.
func (s *Server) handleProjectActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		if _, _, ok := s.requireProjectAccess(w, r, project, ProjectViewer); !ok {
			return
		}
	}
	agents, err := s.store.ListVisibleAgents(getUserID(r))
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	visible := agents[:0]
	for _, a := range agents {
		if project == "" || a.ProjectID == project {
			visible = append(visible, a)
		}
	}
	if len(visible) == 0 {
		writeJSON(w, []TelemetryEvent{})
		return
	}
	ids, args := metricIDs(visible)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 80
	}
	condition := ""
	if before := r.URL.Query().Get("before"); before != "" {
		if id := r.URL.Query().Get("before_id"); id != "" {
			condition = " AND (time<? OR (time=? AND id<?))"
			args = append(args, before, before, id)
		} else {
			condition = " AND time<?"
			args = append(args, before)
		}
	}
	args = append(args, limit)
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT id,agent_id,thread_id,type,time,data FROM telemetry WHERE agent_id IN (`+ids+`) AND type IN ('tool.call','tool.result','event.received','thread.done','error')`+condition+` ORDER BY time DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	out := []TelemetryEvent{}
	for rows.Next() {
		var e TelemetryEvent
		var ts, raw string
		if err := rows.Scan(&e.ID, &e.AgentID, &e.ThreadID, &e.Type, &ts, &raw); err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		e.Time, _ = parseTime(ts)
		e.Data = []byte(raw)
		out = append(out, e)
	}
	if rows.Err() != nil {
		http.Error(w, "query failed", 500)
		return
	}
	writeJSON(w, out)
}
