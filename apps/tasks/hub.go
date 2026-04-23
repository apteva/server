package tasks

import (
	"sync"
	"sync/atomic"
)

// Event kinds emitted on the hub. Thin enum so SSE consumers can
// switch on `kind` without inspecting the payload.
const (
	EventCreated = "task.created"
	EventUpdated = "task.updated"
	EventDeleted = "task.deleted"
)

// HubEvent is one frame pushed to an SSE subscriber. `Task` is the
// full row (even for deletes — so the UI can remove it by id without
// a second fetch).
type HubEvent struct {
	Kind string `json:"kind"`
	Task Task   `json:"task"`
}

// hub fans mutations to per-instance SSE subscribers. Deliberately
// identical shape to channelchat's hub — the DB is the source of
// truth, SSE is a latency shortcut, drops on a full buffer recover
// on reconnect via `since=<last_id>`.
type hub struct {
	mu   sync.RWMutex
	subs map[int64]map[uint64]chan HubEvent // instanceID → subID → channel
	next atomic.Uint64
}

func newHub() *hub {
	return &hub{subs: make(map[int64]map[uint64]chan HubEvent)}
}

func (h *hub) subscribe(instanceID int64) (chan HubEvent, uint64, func()) {
	ch := make(chan HubEvent, 64)
	id := h.next.Add(1)
	h.mu.Lock()
	if h.subs[instanceID] == nil {
		h.subs[instanceID] = make(map[uint64]chan HubEvent)
	}
	h.subs[instanceID][id] = ch
	h.mu.Unlock()
	cancel := func() { h.unsubscribe(instanceID, id) }
	return ch, id, cancel
}

func (h *hub) unsubscribe(instanceID int64, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.subs[instanceID]; ok {
		if ch, ok := m[id]; ok {
			close(ch)
			delete(m, id)
		}
		if len(m) == 0 {
			delete(h.subs, instanceID)
		}
	}
}

func (h *hub) publish(ev HubEvent) {
	h.mu.RLock()
	subs := h.subs[ev.Task.InstanceID]
	fanout := make([]chan HubEvent, 0, len(subs))
	for _, ch := range subs {
		fanout = append(fanout, ch)
	}
	h.mu.RUnlock()
	for _, ch := range fanout {
		select {
		case ch <- ev:
		default:
			// drop — subscriber catches up via since=
		}
	}
}
