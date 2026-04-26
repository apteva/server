package channelchat

import (
	"sync"
	"sync/atomic"
)

// hub is the per-app live-push dispatcher. When a message is written
// (via the Channel.Send path, the HTTP POST path, or any other insert)
// we publish it here and every connected SSE subscriber receives it.
// Reconnects fill the gap from the DB via the `since=<last_id>` query
// param — the hub only carries forward motion, never replay.
//
// Two subscription scopes coexist:
//
//   subs    — keyed by chatID. Drives the per-chat-panel SSE stream
//             AND the IsActive() gate that tells the agent whether
//             chat is being watched.
//
//   userSub — keyed by userID. Drives the dashboard's global
//             notifications tray: one connection per tab, fans out
//             every message for any chat the user owns. Does NOT
//             count toward IsActive — the agent's channel-selection
//             logic stays scoped to "is someone watching this chat".
type hub struct {
	mu       sync.RWMutex
	subs     map[string]map[uint64]chan Message // chatID → subID → channel
	userSubs map[int64]map[uint64]chan Message  // userID → subID → channel
	next     atomic.Uint64
}

func newHub() *hub {
	return &hub{
		subs:     make(map[string]map[uint64]chan Message),
		userSubs: make(map[int64]map[uint64]chan Message),
	}
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

// subscriberCounts returns (per-chat sub count, per-user sub count)
// for debugging visibility of who's listening when a message fires.
func (h *hub) subscriberCounts(chatID string, userID int64) (int, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[chatID]), len(h.userSubs[userID])
}

// subscribeUser attaches a wildcard listener for one user. Every
// message published via publishToUser(userID, m) is fanned out to it.
// Used by the dashboard's global notifications tray.
func (h *hub) subscribeUser(userID int64) (chan Message, uint64, func()) {
	ch := make(chan Message, 128)
	id := h.next.Add(1)
	h.mu.Lock()
	if h.userSubs[userID] == nil {
		h.userSubs[userID] = make(map[uint64]chan Message)
	}
	h.userSubs[userID][id] = ch
	h.mu.Unlock()
	cancel := func() { h.unsubscribeUser(userID, id) }
	return ch, id, cancel
}

func (h *hub) unsubscribeUser(userID int64, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.userSubs[userID]; ok {
		if ch, ok := m[id]; ok {
			close(ch)
			delete(m, id)
		}
		if len(m) == 0 {
			delete(h.userSubs, userID)
		}
	}
}

// publishToUser fans out a message to every wildcard listener for one
// user. Caller is responsible for knowing the owner — the hub doesn't
// resolve chat→instance→user. Same drop-on-overflow semantics as
// publish; the SSE consumer recovers via the unread-summary endpoint
// on reconnect.
func (h *hub) publishToUser(userID int64, m Message) {
	if userID == 0 {
		return
	}
	h.mu.RLock()
	subs := h.userSubs[userID]
	fanout := make([]chan Message, 0, len(subs))
	for _, ch := range subs {
		fanout = append(fanout, ch)
	}
	h.mu.RUnlock()
	for _, ch := range fanout {
		select {
		case ch <- m:
		default:
		}
	}
}
