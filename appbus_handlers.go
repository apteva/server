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
//
// Resolution rules for the event's project_id:
//
//   - Project-scoped install (install row's project_id is set): server
//     pins the event to the install's project, ignoring body.project_id.
//     Prevents a project-scoped sidecar from spoofing events into
//     another project.
//
//   - Global install (install row's project_id is empty): server reads
//     body.project_id from the sidecar's emit payload. Validated to
//     belong to the install's owner (installed_by user). An empty
//     body.project_id is allowed and produces a wildcard-only event
//     (rare; most domain events should anchor to a project).
//
// Either way the app's bus key is derived from app_installs.app_id,
// so the sidecar can't spoof a sibling app's lane.
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
		Topic     string          `json:"topic"`
		ProjectID string          `json:"project_id,omitempty"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Topic) == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}
	var (
		appName        string
		installProject string
		ownerID        int64
	)
	err = s.store.db.QueryRow(
		`SELECT a.name, COALESCE(i.project_id, ''), COALESCE(i.installed_by, 0)
		 FROM app_installs i JOIN apps a ON a.id = i.app_id
		 WHERE i.id = ?`, installID,
	).Scan(&appName, &installProject, &ownerID)
	if err != nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	resolvedProject := installProject
	if resolvedProject == "" {
		// Global install — caller specifies per-event project.
		resolvedProject = strings.TrimSpace(body.ProjectID)
		if resolvedProject != "" && ownerID > 0 {
			var ok int
			s.store.db.QueryRow(
				`SELECT 1 FROM projects WHERE id=? AND user_id=?`,
				resolvedProject, ownerID,
			).Scan(&ok)
			if ok != 1 {
				http.Error(w, "project_id not owned by install", http.StatusForbidden)
				return
			}
		}
	}
	data := body.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	ev := s.appBus.Publish(appName, resolvedProject, installID, body.Topic, data)
	go s.dispatchAppEventToSubscribers(ev)
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
	// The all-apps firehose (app=_all) is inherently cross-project +
	// cross-app: global-scoped sidecars only, project_id ignored.
	if app == allAppsLaneKey {
		projectID = ""
		if err := s.checkFirehoseAccess(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	} else if projectID == "" {
		// Per-app firehose — events for the given app across every
		// project. Restricted to sidecars whose install is global-scope,
		// so a project-scoped sidecar (or a browser session that hasn't
		// picked a project) cannot read sibling projects' events.
		// Browser sessions still need a project_id; the dashboard's
		// per-project pages always carry one.
		if err := s.checkFirehoseAccess(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	} else if err := s.checkProjectAccess(r, projectID); err != nil {
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

// checkFirehoseAccess gates the cross-project subscription
// (project_id=""). Only sidecars whose install is global-scope are
// allowed — that's the install class the SDK's per-project event
// dispatcher runs from. Browsers and project-scoped sidecars are
// refused so they can't accidentally read events outside their
// project boundary.
func (s *Server) checkFirehoseAccess(r *http.Request) error {
	installStr := r.Header.Get("X-Apteva-App-Install-ID")
	if installStr == "" {
		return errors.New("firehose subscription requires a sidecar install token")
	}
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
	if installProject != "" {
		return fmt.Errorf("firehose subscription is global-only; install %d is scoped to project %s", installID, installProject)
	}
	return nil
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
//     subscribed to OR is global-scope (project_id=”). This lets
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
//	id:   <seq>\n
//	data: <json>\n
//	\n
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
