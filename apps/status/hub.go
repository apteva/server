package status

import (
	"sync"
	"sync/atomic"
)

// hub fans status updates to per-instance SSE subscribers. Dropped
// frames aren't catastrophic — the UI can re-fetch the current row on
// reconnect (there's only ever one row per instance).
type hub struct {
	mu   sync.RWMutex
	subs map[int64]map[uint64]chan Status
	next atomic.Uint64
}

func newHub() *hub {
	return &hub{subs: make(map[int64]map[uint64]chan Status)}
}

func (h *hub) subscribe(instanceID int64) (chan Status, uint64, func()) {
	ch := make(chan Status, 8)
	id := h.next.Add(1)
	h.mu.Lock()
	if h.subs[instanceID] == nil {
		h.subs[instanceID] = make(map[uint64]chan Status)
	}
	h.subs[instanceID][id] = ch
	h.mu.Unlock()
	return ch, id, func() { h.unsubscribe(instanceID, id) }
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

func (h *hub) publish(s Status) {
	h.mu.RLock()
	subs := h.subs[s.InstanceID]
	fanout := make([]chan Status, 0, len(subs))
	for _, ch := range subs {
		fanout = append(fanout, ch)
	}
	h.mu.RUnlock()
	for _, ch := range fanout {
		select {
		case ch <- s:
		default:
			// drop — subscriber will re-fetch on reconnect
		}
	}
}
