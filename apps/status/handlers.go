package status

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

type handlers struct {
	store     *store
	hub       *hub
	instances InstanceResolver
}

// InstanceResolver is the same shape as channelchat/tasks use.
type InstanceResolver interface {
	OwnedInstance(userID, instanceID int64) (framework.InstanceInfo, error)
	LookupUserID(r *http.Request) int64
}

// GET /api/apps/status/status?instance_id=N
func (h *handlers) getStatus(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	_, instanceID, ok := h.authorize(w, r)
	if !ok {
		return
	}
	s, err := h.store.Get(instanceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Empty body (with a 200) is nicer than 404 for a
			// "we just haven't set anything yet" case — the UI's
			// rendering code treats empty message as no status.
			writeJSON(w, map[string]any{
				"instance_id": instanceID,
				"message":     "",
				"tone":        "",
			})
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s)
}

// GET /api/apps/status/stream?instance_id=N
// SSE of full Status payloads on each update. No `since` — there's
// only one row per instance, so reconnect semantics are trivial.
func (h *handlers) stream(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	_, instanceID, ok := h.authorize(w, r)
	if !ok {
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

	// Send current row once as initial frame so the client has state
	// without an extra GET.
	if s, err := h.store.Get(instanceID); err == nil {
		writeSSE(w, *s)
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
		case s, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, s)
			flusher.Flush()
		}
	}
}

func (h *handlers) authorize(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	instanceID, err := strconv.ParseInt(r.URL.Query().Get("instance_id"), 10, 64)
	if err != nil || instanceID == 0 {
		http.Error(w, "instance_id required", http.StatusBadRequest)
		return 0, 0, false
	}
	userID := h.instances.LookupUserID(r)
	if _, err := h.instances.OwnedInstance(userID, instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return 0, 0, false
	}
	return userID, instanceID, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, s Status) {
	body, _ := json.Marshal(s)
	var buf strings.Builder
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	_, _ = io.WriteString(w, buf.String())
}
