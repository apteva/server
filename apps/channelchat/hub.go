package channelchat

import (
	"sync"
	"sync/atomic"
)

// hub is the per-app live-push dispatcher. When a message is written
// (via the Channel.Send path, the HTTP POST path, or any other insert)
// we publish it here and every connected SSE subscriber for that
// chat receives it. Reconnects fill the gap from the DB via the
// `since=<last_id>` query param — the hub only carries forward
// motion, never replay.
type hub struct {
	mu   sync.RWMutex
	subs map[string]map[uint64]chan Message // chatID → subID → channel
	next atomic.Uint64
}

func newHub() *hub {
	return &hub{subs: make(map[string]map[uint64]chan Message)}
}

// hasSubscribers returns true if at least one SSE client is currently
// connected to the chat. Used by the channel's IsActive gate so the
// channels MCP advertises "chat" only when someone is actually
// listening — otherwise the agent sees "chat" in its available-
// channels list and tries to respond there even when no user is
// connected (which then looks wrong when e.g. an inject/admin event
// gets a chat reply nobody asked for).
func (h *hub) hasSubscribers(chatID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[chatID]) > 0
}

// subscribe adds a listener for one chat. Returns the channel, the
// subscription id, and a cancel function. Buffer size is generous
// because a burst of agent-reply UPDATEs (future streaming mode)
// shouldn't drop on a slow SSE writer; if it does overflow, the
// subscriber catches up via `since` on reconnect.
func (h *hub) subscribe(chatID string) (chan Message, uint64, func()) {
	ch := make(chan Message, 64)
	id := h.next.Add(1)
	h.mu.Lock()
	if h.subs[chatID] == nil {
		h.subs[chatID] = make(map[uint64]chan Message)
	}
	h.subs[chatID][id] = ch
	h.mu.Unlock()
	cancel := func() { h.unsubscribe(chatID, id) }
	return ch, id, cancel
}

func (h *hub) unsubscribe(chatID string, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.subs[chatID]; ok {
		if ch, ok := m[id]; ok {
			close(ch)
			delete(m, id)
		}
		if len(m) == 0 {
			delete(h.subs, chatID)
		}
	}
}

// publish broadcasts a message to every subscriber of its chat. Never
// blocks on a full subscriber buffer — the DB is the source of truth,
// SSE clients drop events are a latency issue, not a correctness one
// (reconnect with since= recovers).
func (h *hub) publish(m Message) {
	h.mu.RLock()
	subs := h.subs[m.ChatID]
	fanout := make([]chan Message, 0, len(subs))
	for _, ch := range subs {
		fanout = append(fanout, ch)
	}
	h.mu.RUnlock()
	for _, ch := range fanout {
		select {
		case ch <- m:
		default:
			// drop — subscriber will catch up via since= on reconnect
		}
	}
}
