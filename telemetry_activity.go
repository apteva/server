package main

import (
	"net/http"
	"strconv"
	"time"
)

type agentLastActive struct {
	AgentID      int64     `json:"agent_id"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// One aggregate query gives the fleet cards a useful activity timestamp
// without fetching telemetry or thread details separately for each agent.
func (s *Server) handleAgentsLastActive(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	visible := agents[:0]
	for _, agent := range agents {
		if project == "" || agent.ProjectID == project {
			visible = append(visible, agent)
		}
	}
	if len(visible) == 0 {
		writeJSON(w, []agentLastActive{})
		return
	}
	ids, args := metricIDs(visible)
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT agent_id, MAX(time) FROM telemetry WHERE agent_id IN (`+ids+`) AND type IN ('llm.start','tool.call','tool.result','event.received','thread.done','error') GROUP BY agent_id`, args...)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []agentLastActive{}
	for rows.Next() {
		var agentID int64
		var rawTime string
		if err := rows.Scan(&agentID, &rawTime); err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		at, err := parseTime(rawTime)
		if err == nil {
			out = append(out, agentLastActive{AgentID: agentID, LastActiveAt: at})
		}
	}
	if rows.Err() != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

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
	types := "'tool.call','tool.result','event.received','thread.done','error'"
	if r.URL.Query().Get("view") == "runtime" {
		// The composable activity feed uses the same completed thoughts and
		// thread events as agent details. Streaming chunks arrive over SSE;
		// they do not crowd completed records out of the history window. Keep
		// llm.start here as well: an event-driven turn can be active between
		// polls, and without its start record the activity feed appears idle
		// until the eventual llm.done arrives.
		types += ",'llm.start','llm.done','llm.error','thread.spawn','thread.message','realtime.user','realtime.assistant'"
	}
	args = append(args, limit)
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT id,agent_id,thread_id,type,time,data FROM telemetry WHERE agent_id IN (`+ids+`) AND type IN (`+types+`)`+condition+` ORDER BY time DESC,id DESC LIMIT ?`, args...)
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
	writeJSON(w, publicTelemetryEvents(out))
}
