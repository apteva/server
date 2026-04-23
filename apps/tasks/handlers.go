package tasks

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apteva/server/apps/framework"
)

// REST + SSE surface. Mounted at /api/apps/tasks/<path>.
//
// The framework's ServeMux matches exact paths, so single-resource
// routes (update / delete by id) take the id as a query parameter
// rather than in the URL. Same pattern as channelchat/messages uses.

type handlers struct {
	store     *store
	hub       *hub
	instances InstanceResolver
}

// InstanceResolver decouples the app from server-internal types.
// Mirrors the channelchat pattern.
type InstanceResolver interface {
	OwnedInstance(userID, instanceID int64) (framework.InstanceInfo, error)
	LookupUserID(r *http.Request) int64
}

// GET  /api/apps/tasks/tasks?instance_id=N&status=&thread=&parent=&since=
// POST /api/apps/tasks/tasks   {instance_id, title, description?, ...}
func (h *handlers) tasksCollection(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	switch r.Method {
	case http.MethodGet:
		h.listTasks(w, r)
	case http.MethodPost:
		h.createTask(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// GET    /api/apps/tasks/task?id=N
// PATCH  /api/apps/tasks/task?id=N   {status?, progress?, note?, ...}
// DELETE /api/apps/tasks/task?id=N
func (h *handlers) taskItem(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	switch r.Method {
	case http.MethodGet:
		h.getTask(w, r)
	case http.MethodPatch, http.MethodPut:
		h.updateTask(w, r)
	case http.MethodDelete:
		h.deleteTask(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- list / create ---------------------------------------------------

func (h *handlers) listTasks(w http.ResponseWriter, r *http.Request) {
	userID, instanceID, ok := h.authorizeInstance(w, r, "instance_id")
	if !ok {
		return
	}
	_ = userID
	q := r.URL.Query()
	p := ListParams{
		InstanceID:     instanceID,
		Status:         q.Get("status"),
		AssignedThread: q.Get("thread"),
	}
	if v := q.Get("parent"); v != "" {
		if v == "null" || v == "none" {
			neg := int64(-1)
			p.ParentTaskID = &neg
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.ParentTaskID = &n
		}
	}
	if v := q.Get("since"); v != "" {
		p.SinceID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("limit"); v != "" {
		p.Limit, _ = strconv.Atoi(v)
	}
	out, err := h.store.List(p)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

func (h *handlers) createTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstanceID     int64  `json:"instance_id"`
		Title          string `json:"title"`
		Description    string `json:"description"`
		AssignedThread string `json:"assigned_thread"`
		ParentTaskID   *int64 `json:"parent_task_id"`
		RewardXP       int    `json:"reward_xp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	userID := h.instances.LookupUserID(r)
	if _, err := h.instances.OwnedInstance(userID, body.InstanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	uid := userID
	t, err := h.store.Create(CreateParams{
		InstanceID:     body.InstanceID,
		Title:          body.Title,
		Description:    body.Description,
		AssignedThread: body.AssignedThread,
		ParentTaskID:   body.ParentTaskID,
		CreatedByUser:  &uid,
		RewardXP:       body.RewardXP,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.hub.publish(HubEvent{Kind: EventCreated, Task: *t})
	writeJSON(w, t)
}

// --- single-item ------------------------------------------------------

func (h *handlers) getTask(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadOwnedTask(w, r)
	if !ok {
		return
	}
	writeJSON(w, t)
}

func (h *handlers) updateTask(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadOwnedTask(w, r)
	if !ok {
		return
	}
	var body struct {
		Title          *string `json:"title"`
		Description    *string `json:"description"`
		Status         *string `json:"status"`
		Progress       *int    `json:"progress"`
		Note           *string `json:"note"`
		AssignedThread *string `json:"assigned_thread"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	updated, err := h.store.Update(t.ID, UpdateParams{
		Title:          body.Title,
		Description:    body.Description,
		Status:         body.Status,
		Progress:       body.Progress,
		Note:           body.Note,
		AssignedThread: body.AssignedThread,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.hub.publish(HubEvent{Kind: EventUpdated, Task: *updated})
	writeJSON(w, updated)
}

func (h *handlers) deleteTask(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadOwnedTask(w, r)
	if !ok {
		return
	}
	if err := h.store.Delete(t.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.hub.publish(HubEvent{Kind: EventDeleted, Task: *t})
	writeJSON(w, map[string]bool{"ok": true})
}

// --- SSE --------------------------------------------------------------

// GET /api/apps/tasks/stream?instance_id=N&since=<last_id>
// Streams {kind, task} frames for every mutation in that instance.
// Reconnect with since=<last_id> — we backfill any tasks created or
// updated with id > since, then subscribe.
func (h *handlers) stream(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID, instanceID, ok := h.authorizeInstance(w, r, "instance_id")
	if !ok {
		return
	}
	_ = userID

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Backfill — anything created/updated after the client's last-seen
	// id, ordered oldest-first so the UI can apply them in order.
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		since, _ = strconv.ParseInt(sinceStr, 10, 64)
	}
	backfill, err := h.store.List(ListParams{InstanceID: instanceID, SinceID: since, Limit: 500})
	if err == nil {
		// List returns id DESC; reverse for chronological delivery.
		for i, j := 0, len(backfill)-1; i < j; i, j = i+1, j-1 {
			backfill[i], backfill[j] = backfill[j], backfill[i]
		}
		for _, t := range backfill {
			writeSSE(w, HubEvent{Kind: EventCreated, Task: t})
			if t.ID > since {
				since = t.ID
			}
		}
		flusher.Flush()
	}

	ch, _, cancel := h.hub.subscribe(instanceID)
	defer cancel()

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
			writeSSE(w, ev)
			flusher.Flush()
		}
	}
}

// --- helpers ----------------------------------------------------------

func (h *handlers) authorizeInstance(w http.ResponseWriter, r *http.Request, key string) (int64, int64, bool) {
	instanceID, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil || instanceID == 0 {
		http.Error(w, key+" required", http.StatusBadRequest)
		return 0, 0, false
	}
	userID := h.instances.LookupUserID(r)
	if _, err := h.instances.OwnedInstance(userID, instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return 0, 0, false
	}
	return userID, instanceID, true
}

// loadOwnedTask resolves ?id=N, fetches the task, and verifies the
// authenticated user owns the parent instance.
func (h *handlers) loadOwnedTask(w http.ResponseWriter, r *http.Request) (*Task, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id == 0 {
		http.Error(w, "id required", http.StatusBadRequest)
		return nil, false
	}
	t, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return nil, false
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	userID := h.instances.LookupUserID(r)
	if _, err := h.instances.OwnedInstance(userID, t.InstanceID); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return t, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, ev HubEvent) {
	body, _ := json.Marshal(ev)
	var buf strings.Builder
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	_, _ = io.WriteString(w, buf.String())
}
