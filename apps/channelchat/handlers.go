package channelchat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apteva/server/apps/framework"
)

// perThreadEnabled gates the "one core thread per chat" routing. On
// by default — chat messages route to a dedicated per-chat core
// thread so a busy main can't block user-facing replies. Set
// CHANNELCHAT_PER_THREAD=0 to revert to the legacy "everything goes
// to main" behavior (single env-var flip, no DB rollback needed).
func perThreadEnabled() bool {
	return os.Getenv("CHANNELCHAT_PER_THREAD") != "0"
}

// chatThreadDirectiveSuffix is appended to main's directive when
// spawning a per-chat thread. Tells the chat thread it's the
// user-facing front and to delegate state-changing work to main via
// `send`. Kept in one place so the wording can be tuned without
// chasing call sites.
const chatThreadDirectiveSuffix = "\n\n---\nYou are this agent's chat-handling thread. The user is talking to you directly. Reply to them by calling respond(channel=\"chat\", text=...). For any action that modifies state (sending messages on other channels, writing files, calling external APIs, updating memory), use send(id=\"main\", text=...) to delegate to the main thread, which has the full tool set. Acknowledge the request to the user immediately and relay main's reply when it arrives."

// chatThreadTools is the minimal local tool set the chat thread gets
// at spawn time. Read-only by design — anything mutating goes through
// `send` to main. `pace` is harmless and lets the thread idle between
// messages without holding the loop hot.
var chatThreadTools = []string{"send", "pace"}

// chatThreadMCPs is the MCP servers the chat thread connects to. Just
// `channels` so the thread can call respond(channel="chat", ...) to
// reply to the user. Anything else (storage, etc.) main handles.
var chatThreadMCPs = []string{"channels"}

// spawnedChatThreads remembers which (instance, chat) pairs we've
// already spawned the thread for in this process. The core endpoint
// is idempotent, but the round-trip is wasted work on every message;
// this cache turns it into one POST per chat per process. Reset on
// restart, which is fine — the second call returns status=exists.
var spawnedChatThreads sync.Map // key: "instID/chatID" → struct{}{}

// REST + SSE surface. Mounted at /api/apps/channel-chat/<path>. Every
// route is scoped to the authenticated user + the instance owning the
// chat — handlers re-check ownership from the user_id pulled off the
// request via the standard auth middleware.

type handlers struct {
	store      *store
	hub        *hub
	bus        *framework.AppBus
	instances  InstanceResolver
}

// InstanceResolver is the small callback the app needs from
// apteva-server to answer "does this chat belong to an instance the
// caller owns, and what port/core_key should I use to forward user
// messages into the agent's /event endpoint?". Keeps the app decoupled
// from server-internal types.
type InstanceResolver interface {
	// OwnedInstance returns the instance info IF the user owns it,
	// else error. Used for ownership checks on chat operations.
	OwnedInstance(userID, agentID int64) (framework.InstanceInfo, error)

	// LookupUserID pulls the user id off the request (via the
	// server's auth middleware header).
	LookupUserID(r *http.Request) int64

	// ForwardEvent posts a text event into the instance's core
	// /event endpoint. The server already has the makeSendEvent
	// helper — this wraps it so the app doesn't need to know the
	// port/core-key layout.
	ForwardEvent(inst framework.InstanceInfo, text, threadID string) error

	// SpawnThread idempotently spawns a core thread by id with the
	// given directive + tool set + MCP servers. Used by channelchat
	// (when CHANNELCHAT_PER_THREAD is on) to bootstrap a dedicated
	// chat-handling thread so a busy main can't block user replies.
	// Returns nil for both newly-created and pre-existing threads.
	SpawnThread(inst framework.InstanceInfo, threadID, directive string, tools, mcp []string) error

	// InstanceIDsForUser returns every instance id the user owns,
	// across all projects. Used by the unread-summary endpoint and
	// the global SSE stream to scope to "this user's chats".
	InstanceIDsForUser(userID int64) ([]int64, error)
}

// --- Chats collection -------------------------------------------------

// GET /api/apps/channel-chat/chats?agent_id=<id>
// Lists chats for one agent (usually just the default). The legacy
// ?instance_id= name still works during the rename deprecation
// window; the dashboard switched to ?agent_id= in Phase 3.
func (h *handlers) listChats(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	raw := r.URL.Query().Get("agent_id")
	if raw == "" {
		raw = r.URL.Query().Get("instance_id")
	}
	agentID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	if _, err := h.instances.OwnedInstance(userID, agentID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	chats, err := h.store.ListChatsForAgent(agentID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, chats)
}

// POST /api/apps/channel-chat/chats {agent_id, title?}
// Creates (or returns existing default) chat for an agent. v1 always
// returns the default chat; multi-chat creation is a later UI. Body
// also accepts the legacy `instance_id` field during the rename
// deprecation window.
func (h *handlers) createChat(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	var body struct {
		AgentID  int64  `json:"agent_id"`
		LegacyID int64  `json:"instance_id"` // legacy alias for dual-naming window
		Title    string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 {
		body.AgentID = body.LegacyID
	}
	if _, err := h.instances.OwnedInstance(userID, body.AgentID); err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	chat, err := h.store.EnsureDefaultChat(body.AgentID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, chat)
}

// --- Messages ---------------------------------------------------------

// GET  /api/apps/channel-chat/messages?chat_id=<id>&since=<id>&limit=<n>
// POST /api/apps/channel-chat/messages { chat_id, content }
// DELETE /api/apps/channel-chat/messages?chat_id=<id>
func (h *handlers) messages(w http.ResponseWriter, r *http.Request, ctx *framework.AppCtx) {
	switch r.Method {
	case http.MethodGet:
		h.listMessages(w, r)
	case http.MethodPost:
		h.postMessage(w, r, ctx)
	case http.MethodDelete:
		h.deleteMessages(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handlers) listMessages(w http.ResponseWriter, r *http.Request) {
	chatID, inst, ok := h.authorizeChat(w, r)
	if !ok {
		return
	}
	_ = inst
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := h.store.ListMessages(chatID, since, limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, msgs)
}

// postMessage inserts a user row AND forwards the text to the
// instance's core /event endpoint so the agent sees it as input on
// its next think iteration. Same pattern as Slack: DB insert for the
// UI, /event forward for the agent. Both happen before the response
// so the caller can't race the agent's first reaction.
func (h *handlers) postMessage(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	chatID, inst, ok := h.authorizeChat(w, r)
	if !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	// Accept chat_id in body too for POST ergonomics; query param wins
	// (we already parsed it in authorizeChat).
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	// If JSON lacked content but had chat_id, re-parse leniently so
	// dashboards that send {chat_id, content} in the body still work.
	if body.Content == "" {
		var alt struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(bytes.TrimSpace(raw), &alt)
		body.Content = alt.Content
	}
	if strings.TrimSpace(body.Content) == "" {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}

	userID := h.instances.LookupUserID(r)
	uid := userID
	m, err := h.store.Append(chatID, "user", body.Content, &uid, "", "final", nil)
	if err != nil {
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	h.hub.publish(*m)
	if h.bus != nil {
		h.bus.Publish("chat.message", "channel-chat", *m)
	}

	// Forward to the agent's /event endpoint using the same shape
	// the Slack / email paths use. Prefix identifies the channel so
	// the agent knows which channel to respond via
	// (channels_respond(channel="chat", ...)). We use a stable
	// "[chat]" prefix so existing channel-routing logic in core works
	// without per-chat-id knowledge for the single-default case.
	//
	// Failure used to be silent — the DB row persisted but the agent
	// never saw the message, and the user had no way to know. Now we
	// log loudly AND drop a system row into the chat so the user sees
	// "agent unreachable, will see the message when it's running again"
	// instead of an indefinite quiet.
	go func(inst framework.InstanceInfo, text string, chatID string) {
		evText := fmt.Sprintf("[chat] %s", text)
		threadID := h.resolveChatThread(inst, chatID)
		if err := h.instances.ForwardEvent(inst, evText, threadID); err != nil {
			log.Printf("[CHAT] ForwardEvent FAILED chat=%s instance=%d thread=%s: %v",
				chatID, inst.ID, threadID, err)
			// Surface the failure to the user inline. The system row
			// goes through the same hub/SSE path as a regular message
			// so the chat panel renders it next to the user's input.
			notice := fmt.Sprintf("(could not reach agent — your message is saved and will be delivered when the agent is running. err: %v)", err)
			if sm, sErr := h.store.Append(chatID, "system", notice, nil, "", "final", nil); sErr == nil {
				h.hub.publish(*sm)
			}
		}
	}(inst, body.Content, chatID)

	writeJSON(w, m)
}

func (h *handlers) deleteMessages(w http.ResponseWriter, r *http.Request) {
	chatID, _, ok := h.authorizeChat(w, r)
	if !ok {
		return
	}
	n, err := h.store.DeleteMessages(chatID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"deleted": n})
}

// GET /api/apps/channel-chat/stream?chat_id=<id>&since=<id>
// GET /api/apps/channel-chat/stream?scope=user
//
// Two modes:
//   chat_id=… — per-chat panel stream (back-compat). Backfills since=
//               from the DB then live-tails via the per-chat hub.
//   scope=user — global notifications stream. Live-tails every message
//                inserted into any chat the user owns; no backfill (the
//                tray seeds itself via /unread-summary on connect).
func (h *handlers) stream(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	scope := r.URL.Query().Get("scope")
	if scope == "user" {
		h.streamUser(w, r)
		return
	}
	chatID, _, ok := h.authorizeChat(w, r)
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

	// Backfill from DB — everything since client's checkpoint.
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		since, _ = strconv.ParseInt(sinceStr, 10, 64)
	}
	backfill, err := h.store.ListMessages(chatID, since, 1000)
	if err == nil {
		for _, m := range backfill {
			writeSSE(w, m)
			if m.ID > since {
				since = m.ID
			}
		}
		flusher.Flush()
	}

	// Subscribe AFTER backfill so we don't miss anything written
	// between backfill and subscribe (the DB query + subscribe sandwich
	// is the canonical "no missed events" pattern).
	ch, _, cancel := h.hub.subscribe(chatID)
	defer cancel()

	// Parallel stream-frame subscription. Ephemeral LLM-args chunks
	// for in-progress `channels_respond` calls land here. Separate
	// from the Message channel so reverting streaming = delete this
	// block (no Message-path side effects).
	streamCh, _, streamCancel := h.hub.subscribeStream(chatID)
	defer streamCancel()

	// Keepalive ping every 15s prevents intermediary proxies from
	// killing an idle connection.
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
		case m, ok := <-ch:
			if !ok {
				return
			}
			// Dedup: if the hub delivered an event we already saw in
			// backfill, skip it. Since the hub only fires forward,
			// this is just the "same tick" edge case.
			if m.ID <= since {
				continue
			}
			writeSSE(w, m)
			since = m.ID
			flusher.Flush()
		case f, ok := <-streamCh:
			if !ok {
				continue
			}
			writeSSEStream(w, f)
			flusher.Flush()
		}
	}
}

// streamUser is the wildcard-by-user SSE path. No backfill — the tray
// seeds via /unread-summary when it connects, then this stream keeps
// it live. System messages are filtered out so the tray only shows
// user-addressable agent replies and inbound user messages.
func (h *handlers) streamUser(w http.ResponseWriter, r *http.Request) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
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

	ch, _, cancel := h.hub.subscribeUser(userID)
	defer cancel()
	log.Printf("[CHAT-DEBUG] streamUser SUBSCRIBED user=%d", userID)
	defer log.Printf("[CHAT-DEBUG] streamUser CLOSED user=%d", userID)

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	// Initial event so the client can confirm the stream is up before
	// any messages arrive.
	_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case m, ok := <-ch:
			if !ok {
				return
			}
			if m.Role == "system" {
				continue
			}
			log.Printf("[CHAT-DEBUG] streamUser DELIVERING user=%d msgID=%d role=%s chat=%s",
				userID, m.ID, m.Role, m.ChatID)
			writeSSE(w, m)
			flusher.Flush()
		}
	}
}

// GET /api/apps/channel-chat/unread-summary
//
// One row per chat the user owns. Dashboard subtracts a localStorage
// watermark client-side to compute unread counts; the server only
// reports latest_id + a preview of the latest message.
func (h *handlers) unreadSummary(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	ids, err := h.instances.InstanceIDsForUser(userID)
	if err != nil {
		log.Printf("[CHAT] unread-summary: list instances for user=%d: %v", userID, err)
		http.Error(w, "internal error: list instances: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := h.store.LatestForOwner(ids)
	if err != nil {
		log.Printf("[CHAT] unread-summary: LatestForOwner user=%d ids=%v: %v", userID, ids, err)
		http.Error(w, "internal error: latest-for-owner: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// POST /api/apps/channel-chat/seen { chat_id, last_seen_id }
//
// Advances the per-chat read watermark. Idempotent + monotonic: lower
// last_seen_id values are dropped, so a slow tab can't un-read a more
// recent ack from another device. Returns { last_seen_id } as accepted.
func (h *handlers) markSeen(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	var body struct {
		ChatID     string `json:"chat_id"`
		LastSeenID int64  `json:"last_seen_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.ChatID == "" {
		http.Error(w, "chat_id required", http.StatusBadRequest)
		return
	}
	chat, err := h.store.GetChat(body.ChatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	userID := h.instances.LookupUserID(r)
	if _, err := h.instances.OwnedInstance(userID, chat.AgentID); err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return
	}
	current, err := h.store.MarkSeen(body.ChatID, body.LastSeenID)
	if err != nil {
		log.Printf("[CHAT] mark-seen chat=%s id=%d: %v", body.ChatID, body.LastSeenID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"last_seen_id": current})
}

// presence forwards a "[chat] user connected/disconnected" event to
// the agent. Routes via resolveChatThread so presence lands on the
// same thread as the chat's messages — chat thread when the feature
// is on, main when off (single-flag revert).
//
// Used to live in the dashboard as a direct call to /agents/:id/event
// with thread_id="main" hardcoded, which bypassed the per-chat
// thread resolution. Moving it here keeps "which thread does this
// chat go to" decided in exactly one place.
func (h *handlers) presence(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	var body struct {
		ChatID string `json:"chat_id"`
		Action string `json:"action"` // "connected" | "disconnected"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.ChatID == "" {
		http.Error(w, "chat_id required", http.StatusBadRequest)
		return
	}
	var evText string
	switch body.Action {
	case "connected":
		evText = "[chat] user connected to chat"
	case "disconnected":
		evText = "[chat] user disconnected from chat"
	default:
		http.Error(w, "action must be \"connected\" or \"disconnected\"", http.StatusBadRequest)
		return
	}
	chat, err := h.store.GetChat(body.ChatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "chat not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	userID := h.instances.LookupUserID(r)
	inst, err := h.instances.OwnedInstance(userID, chat.AgentID)
	if err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return
	}
	threadID := h.resolveChatThread(inst, body.ChatID)
	if err := h.instances.ForwardEvent(inst, evText, threadID); err != nil {
		log.Printf("[CHAT] presence forward chat=%s thread=%s action=%s: %v",
			body.ChatID, threadID, body.Action, err)
		// Don't fail the request — presence is fire-and-forget for the
		// client, and a transient core unavailability shouldn't surface
		// as a noisy error in the chat UI.
	}
	writeJSON(w, map[string]string{"status": "ok", "thread_id": threadID})
}

// --- Helpers ----------------------------------------------------------

// authorizeChat pulls chat_id from the query, verifies the chat
// belongs to an instance the caller owns, and returns the pair.
// Writes an HTTP error + returns ok=false on failure.
func (h *handlers) authorizeChat(w http.ResponseWriter, r *http.Request) (string, framework.InstanceInfo, bool) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		http.Error(w, "chat_id required", http.StatusBadRequest)
		return "", framework.InstanceInfo{}, false
	}
	chat, err := h.store.GetChat(chatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "chat not found", http.StatusNotFound)
			return "", framework.InstanceInfo{}, false
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", framework.InstanceInfo{}, false
	}
	userID := h.instances.LookupUserID(r)
	inst, err := h.instances.OwnedInstance(userID, chat.AgentID)
	if err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return "", framework.InstanceInfo{}, false
	}
	return chatID, inst, true
}

// resolveChatThread decides which core thread the chat's events
// should target. Flag off → "main" (legacy behavior). Flag on →
// look up (or assign) the chat's persisted thread id and spawn it
// idempotently on first use this process. Falls back to "main" on
// any error so a transient DB/network glitch can't drop messages.
func (h *handlers) resolveChatThread(inst framework.InstanceInfo, chatID string) string {
	if !perThreadEnabled() {
		return "main"
	}
	threadID, err := h.store.EnsureChatThread(chatID)
	if err != nil || threadID == "" {
		log.Printf("[CHAT] EnsureChatThread chat=%s: %v — falling back to main", chatID, err)
		return "main"
	}
	cacheKey := fmt.Sprintf("%d/%s", inst.ID, chatID)
	if _, alreadySpawned := spawnedChatThreads.Load(cacheKey); alreadySpawned {
		return threadID
	}
	// The "directive" arg flows into core as directive_suffix: the
	// thread inherits main's directive verbatim and appends this
	// chat-handling hint. Sending only the suffix avoids round-tripping
	// to fetch main's directive on the channelchat side.
	if err := h.instances.SpawnThread(inst, threadID, chatThreadDirectiveSuffix, chatThreadTools, chatThreadMCPs); err != nil {
		log.Printf("[CHAT] SpawnThread chat=%s thread=%s: %v — falling back to main", chatID, threadID, err)
		return "main"
	}
	spawnedChatThreads.Store(cacheKey, struct{}{})
	return threadID
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, m Message) {
	body, _ := json.Marshal(m)
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(body)
	_, _ = io.WriteString(w, "\n\n")
}

// writeSSEStream emits a stream frame with a distinct event name so
// the client can branch on `event.type === "stream"` vs falling
// through to the default Message handler. The default SSE event is
// "message" — using a named event here keeps the existing handler's
// `onmessage` callback receiving only real Message rows.
func writeSSEStream(w http.ResponseWriter, f StreamFrame) {
	body, _ := json.Marshal(f)
	_, _ = io.WriteString(w, "event: stream\ndata: ")
	_, _ = w.Write(body)
	_, _ = io.WriteString(w, "\n\n")
}
