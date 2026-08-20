package main

// apps_telemetry.go — the generic, permission-gated telemetry feed for
// sidecar apps: GET /api/apps/callback/telemetry.
//
// Until now the only app receiving telemetry was channel-chat, through
// a hard-coded in-process hook (apps_wire.go liveTelemetryHook). This
// endpoint replaces that special case with a capability any app can
// declare: platform.telemetry.read. The conversations app's streaming
// bubbles are the first consumer; once channel-chat is retired, the
// hook goes with it.
//
// The stream is EPHEMERAL by design — no cursor, no replay. Token
// deltas are worthless after the fact; a reconnect starts fresh.
// Durable facts always travel their own durable paths.
//
// Three server-side filters, all enforced here rather than trusted to
// the client:
//   - event types  (?events=llm.tool_chunk,…) — apps get only what
//     they name; an unfiltered subscription is refused outright since
//     the firehose is dominated by token deltas.
//   - ownership    — only agents owned by the install's acting user,
//     scoped to the install's project when it has one (the same rule
//     callbackAgentForInstall applies to thread spawns). Refreshed
//     periodically so newly created agents join a live stream.
//   - thread prefix (?thread_prefix=chat-) — optional, so a
//     conversation app never receives other threads' content.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// telemetryOwnedAgentsRefresh is how often the eligible-agent set is
// recomputed on a live stream. Agents created after subscribe join
// within this window.
var telemetryOwnedAgentsRefresh = 60 * time.Second

type telemetryFilter struct {
	events       map[string]bool
	threadPrefix string
	agentID      int64
	ownedAgents  map[int64]bool
}

func (f *telemetryFilter) allows(ev TelemetryEvent) bool {
	if !f.events[ev.Type] {
		return false
	}
	if f.agentID != 0 && ev.AgentID != f.agentID {
		return false
	}
	if !f.ownedAgents[ev.AgentID] {
		return false
	}
	if f.threadPrefix != "" && !strings.HasPrefix(ev.ThreadID, f.threadPrefix) {
		return false
	}
	return true
}

// ownedAgentIDsForInstall computes the agent set an install may observe:
// the acting user's agents, narrowed to the install's project when the
// install is project-scoped. Mirrors callbackAgentForInstall, evaluated
// as a set so the stream loop never does per-event DB work.
func (s *Server) ownedAgentIDsForInstall(userID, installID int64) (map[int64]bool, error) {
	var installProject string
	if err := s.store.db.QueryRow(
		`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID,
	).Scan(&installProject); err != nil {
		return nil, fmt.Errorf("app installation not found")
	}
	rows, err := s.store.db.Query(
		`SELECT id, COALESCE(project_id,'') FROM agents WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := map[int64]bool{}
	for rows.Next() {
		var id int64
		var project string
		if err := rows.Scan(&id, &project); err != nil {
			continue
		}
		if installProject != "" && project != installProject {
			continue
		}
		owned[id] = true
	}
	return owned, rows.Err()
}

// handleCallbackTelemetry serves the SSE stream. Auth (install token →
// user headers) has already run in the callback middleware; permission
// and filters are enforced here.
func (s *Server) handleCallbackTelemetry(w http.ResponseWriter, r *http.Request, installID int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !installHasPermission(s, installID, sdk.PermTelemetryRead) {
		http.Error(w, "missing permission: "+string(sdk.PermTelemetryRead), http.StatusForbidden)
		return
	}

	eventsParam := strings.TrimSpace(r.URL.Query().Get("events"))
	if eventsParam == "" {
		// The firehose is mostly token deltas; an app that wants
		// everything must say so per type, not by omission.
		http.Error(w, "events filter required (e.g. ?events=llm.tool_chunk)", http.StatusBadRequest)
		return
	}
	filter := &telemetryFilter{events: map[string]bool{}}
	for _, event := range strings.Split(eventsParam, ",") {
		if event = strings.TrimSpace(event); event != "" {
			filter.events[event] = true
		}
	}
	filter.threadPrefix = r.URL.Query().Get("thread_prefix")
	if raw := r.URL.Query().Get("agent_id"); raw != "" {
		id, err := atoi64(raw)
		if err != nil {
			http.Error(w, "invalid agent_id", http.StatusBadRequest)
			return
		}
		filter.agentID = id
	}

	userID := getUserID(r)
	owned, err := s.ownedAgentIDsForInstall(userID, installID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.ownedAgents = owned

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher.Flush()

	ch := s.broadcaster.SubscribeAll()
	defer s.broadcaster.UnsubscribeAll(ch)

	refresh := time.NewTicker(telemetryOwnedAgentsRefresh)
	defer refresh.Stop()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-refresh.C:
			if next, err := s.ownedAgentIDsForInstall(userID, installID); err == nil {
				filter.ownedAgents = next
			}
		case <-heartbeat.C:
			// Comment line keeps proxies from idling the connection out;
			// SDK clients skip non-data lines.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			if !filter.allows(ev) {
				continue
			}
			encoded, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			flusher.Flush()
		}
	}
}
