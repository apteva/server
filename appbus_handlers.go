package main

// HTTP surface for AppEventBus.
//
//   POST /api/app-events/internal/emit
//     Auth: sidecar APTEVA_APP_TOKEN. The auth middleware resolves
//     it into X-Apteva-App-Install-ID; we look up app_name +
//     project_id from the install row so the bus key is always
//     server-stamped, never client-claimed.
//     Body: {topic, data?}
//
//   GET /api/app-events/<app>?project_id=<x>&since=<seq>
//     Auth: user (cookie/API key). SSE stream of events for the
//     given (app, project_id). Replays from ring on connect when
//     since= is given, then live-tails. 15s keepalive ping.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// POST /api/app-events/internal/emit
func (s *Server) handleAppEventEmit(w http.ResponseWriter, r *http.Request) {
	installIDStr := r.Header.Get("X-Apteva-App-Install-ID")
	if installIDStr == "" {
		http.Error(w, "sidecar token required", http.StatusUnauthorized)
		return
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil || installID <= 0 {
		http.Error(w, "bad install id", http.StatusUnauthorized)
		return
	}
	var body struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Topic) == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}
	// Look up the install — derive app name + project_id server-side
	// so the sidecar can never spoof a different app's lane.
	var (
		appName   string
		projectID string
	)
	err = s.store.db.QueryRow(
		`SELECT a.name, COALESCE(i.project_id, '')
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&appName, &projectID)
	if err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	data := body.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	ev := s.appBus.Publish(appName, projectID, installID, body.Topic, data)
	writeJSON(w, map[string]any{
		"ok":  true,
		"seq": ev.Seq,
	})
}

// GET /api/app-events/<app>?project_id=<x>&since=<seq>
func (s *Server) handleAppEventStream(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimPrefix(r.URL.Path, "/app-events/")
	app = strings.TrimSuffix(app, "/")
	if app == "" || strings.Contains(app, "/") {
		http.Error(w, "path: /app-events/<app>", http.StatusBadRequest)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	// project_id is required because subscriptions are scoped per
	// project. A global subscription would need a different auth
	// boundary — punted to a follow-up if a real use case shows up.
	if projectID == "" {
		http.Error(w, "project_id required", http.StatusBadRequest)
		return
	}
	if err := s.checkProjectAccess(r, projectID); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseUint(v, 10, 64)
	}

	ch, replay, cancel := s.appBus.Subscribe(app, projectID, since)
	defer cancel()

	for _, ev := range replay {
		writeAppEventSSE(w, ev)
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeAppEventSSE(w, ev)
			flusher.Flush()
		}
	}
}

// checkProjectAccess returns nil if the requesting principal is
// allowed to subscribe to the given project's app-event stream.
//
// Two principal kinds:
//
//   - Browser session (X-User-ID set, no install header). Any
//     logged-in user is permitted — the dashboard enforces project
//     switching on the client side. Membership refinement is a
//     follow-up once project membership is formally modelled.
//
//   - Sidecar (X-Apteva-App-Install-ID set, X-User-ID also set).
//     Permitted when the install belongs to the same project being
//     subscribed to OR is global-scope (project_id=''). This lets
//     apps like media subscribe to storage's file.deleted events
//     for instant cascade cleanup without polling.
func (s *Server) checkProjectAccess(r *http.Request, projectID string) error {
	if projectID == "" {
		return errors.New("project_id required")
	}

	// Sidecar path: install token. Verify the install's project
	// matches the requested project_id (or is global). This keeps
	// project A's media app from reading project B's storage events.
	if installStr := r.Header.Get("X-Apteva-App-Install-ID"); installStr != "" {
		installID, err := strconv.ParseInt(installStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid install id")
		}
		var installProject string
		if err := s.store.db.QueryRow(
			`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, installID,
		).Scan(&installProject); err != nil {
			return fmt.Errorf("install not found")
		}
		// Global install (no project) can subscribe to any project's
		// events (it serves them all). Project-scoped install can
		// only subscribe to its own project.
		if installProject != "" && installProject != projectID {
			return fmt.Errorf("sidecar's install project (%s) doesn't match requested project (%s)",
				installProject, projectID)
		}
		return nil
	}

	if r.Header.Get("X-User-ID") == "" {
		return errors.New("login required")
	}
	// Verify the project actually exists. Membership refinement is a
	// follow-up — for now any authenticated user can attach.
	var exists int
	err := s.store.db.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("project not found")
	}
	return nil
}

// writeAppEventSSE serializes an AppEvent as one unnamed SSE frame:
//
//   id:   <seq>\n
//   data: <json>\n
//   \n
//
// We deliberately do NOT set `event: <topic>` — that would force
// every dashboard consumer to pre-declare addEventListener calls
// per topic. The topic travels inside the JSON body instead, so a
// single onmessage handler is enough and the consumer filters
// client-side. The id= field still populates EventSource's
// lastEventId for transparent reconnect.
func writeAppEventSSE(w io.Writer, ev AppEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, body)
}
