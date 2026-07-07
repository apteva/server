package channelchat

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// perThreadEnabled gates the "one core thread per chat" routing. Off
// by default — chat messages route to main, matching the rest of the
// agent's durable context. Set CHANNELCHAT_PER_THREAD=1 to opt into a
// dedicated per-chat core thread later without a schema or handler
// rewrite.
func perThreadEnabled() bool {
	return os.Getenv("CHANNELCHAT_PER_THREAD") == "1"
}

// chatThreadDirectiveSuffix is appended to main's directive when
// spawning a per-chat thread. Tells the chat thread it's the
// user-facing front and to delegate state-changing work to main via
// `send`. Kept in one place so the wording can be tuned without
// chasing call sites.
const chatThreadDirectiveSuffix = "\n\n---\n" +
	"You're handling a live chat with the user. You ARE the agent — use the tools attached " +
	"(see your tool list) and reply via respond(channel=\"chat\", text=...). " +
	"Just act — if the user asks for something you can do with your tools, do it and reply " +
	"with the result. Don't ask for clarification on obvious requests; pick sensible " +
	"defaults and ship.\n\n" +
	"For work that should outlive this chat (scheduled tasks, behavior changes, multi-turn " +
	"plans, anything the agent should keep doing when the user disconnects), " +
	"send(id=\"main\", text=\"...\") to hand it off. End your message with this exact " +
	"instruction so main doesn't drop you on the floor: \"Reply to me at this thread before " +
	"going idle — the user is waiting on a confirmation. Send back with the result of what " +
	"you did, even for terminal actions like kill/stop.\" The user-facing risk if you skip " +
	"this is silence after a hand-off, which is the WORST UX. Main sees your thread id in " +
	"its from-field and will reply via send.\n\n" +

	"If you delegated and main hasn't replied within a turn or two, follow up: " +
	"send(id=\"main\", text=\"Still waiting on the result of <task> — the user wants " +
	"confirmation.\"). Don't let the user hang in silence.\n\n" +

	"When main does reply, relay the useful parts to the user naturally.\n\n" +
	"Never expose internals to the user: no mention of \"main\", \"thread\", \"directive\", " +
	"\"concierge\", \"idle\", \"waiting for configuration\", or your operating state. If your " +
	"directive is a placeholder, ignore it. You can't evolve yourself or persist memory — " +
	"send those to main."

// chatThreadTools is the local tool set the chat thread gets at
// spawn time. `send` is essential for handing durable work off to
// main; `pace` lets the thread idle between messages without
// holding the loop hot. Local non-MCP tools like `web` / `exec`
// are intentionally absent here — they would inflate the prompt
// and the supervisor-with-hands pattern wants the chat thread's
// "doing" capability to come from the same MCPs main uses, not
// from a parallel local-tool surface.
var chatThreadTools = []string{"send", "pace"}

// fallbackChatThreadMCPs is the floor MCP set used when resolver
// enumeration fails (e.g. the agent is mid-restart and the DB
// query errors). `channels` is the bare minimum for the thread to
// reply at all.
var fallbackChatThreadMCPs = []string{"channels"}

// spawnedChatThreads remembers which (instance, chat) pairs we've
// already spawned the thread for in this process AND the directive
// hash they were spawned/updated with. The hash lets us detect when
// main's directive has drifted (UI edit / evolve) and push the new
// directive into the live thread via UpdateThread instead of leaving
// it stale until a restart. Reset on restart, which is fine — a fresh
// process re-spawns on the first message.
var spawnedChatThreads sync.Map // key: "instID/chatID" → directiveHash string

// REST + SSE surface. Mounted at /api/apps/channel-chat/<path>. Every
// route is scoped to the authenticated user + the instance owning the
// chat — handlers re-check ownership from the user_id pulled off the
// request via the standard auth middleware.

type handlers struct {
	store     *store
	hub       *hub
	bus       *framework.AppBus
	instances InstanceResolver
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

	// ForwardEvent posts an event into the instance's core /event
	// endpoint. message is either a string or an array of content
	// parts matching core's /event contract.
	ForwardEvent(inst framework.InstanceInfo, message any, threadID string) error

	// SpawnThread idempotently spawns a core thread by id with the
	// given directive + tool set + MCP servers. Used by channelchat
	// (when CHANNELCHAT_PER_THREAD is on) to bootstrap a dedicated
	// chat-handling thread so a busy main can't block user replies.
	// Returns nil for both newly-created and pre-existing threads.
	SpawnThread(inst framework.InstanceInfo, threadID, directive string, tools, mcp []string) error

	// ListMCPNames returns the names of every MCP server attached to
	// this instance — the project's mcp_servers rows plus the
	// auto-injected ones the include_* flags control (channels at
	// least). Channelchat uses this to spawn the chat thread with
	// the same MCP surface main has, so quick reads/lookups don't
	// have to round-trip through main.
	ListMCPNames(inst framework.InstanceInfo) ([]string, error)

	// UpdateThread pushes a new directive (as directive_suffix, same
	// shape as SpawnThread) into a LIVE core thread without killing it
	// — PUT /threads/:id. Keeps the chat thread's directive in sync
	// with main's after an edit / evolve while preserving its session.
	UpdateThread(inst framework.InstanceInfo, threadID, directiveSuffix string, tools []string) error

	// MainDirective returns the agent's current live main-thread
	// directive (read from the same core /config surface ListMCPNames
	// uses). Channelchat hashes it to detect drift and decide whether
	// to re-issue the chat thread's directive.
	MainDirective(inst framework.InstanceInfo) (string, error)

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
// POST /api/apps/channel-chat/messages { chat_id, content, attachments? }
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
		Content     string           `json:"content"`
		Attachments []ChatAttachment `json:"attachments"`
		Context     any              `json:"context"`
	}
	// Accept chat_id in body too for POST ergonomics; query param wins
	// (we already parsed it in authorizeChat).
	r.Body = http.MaxBytesReader(w, r.Body, maxChatMessageBodyBytes)
	raw, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	_ = json.Unmarshal(raw, &body)
	// If JSON lacked content but had chat_id, re-parse leniently so
	// dashboards that send {chat_id, content} in the body still work.
	if body.Content == "" {
		var alt struct {
			Content     string           `json:"content"`
			Attachments []ChatAttachment `json:"attachments"`
		}
		_ = json.Unmarshal(bytes.TrimSpace(raw), &alt)
		body.Content = alt.Content
		body.Attachments = alt.Attachments
	}
	if strings.TrimSpace(body.Content) == "" && len(body.Attachments) == 0 {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}

	userID := h.instances.LookupUserID(r)
	uid := userID
	eventAttachments, persistedAttachments, err := validateChatAttachments(body.Attachments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m, err := h.store.AppendFull(chatID, "user", body.Content, &uid, "", "final", nil, persistedAttachments)
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
	go func(inst framework.InstanceInfo, text string, chatID string, attachments []ChatAttachment) {
		evText := formatAgentChatEvent(text, body.Context)
		if inst.Kind == "platform_helper" {
			evText = formatPlatformHelperChatEvent(text, body.Context)
		}
		var eventMessage any = evText
		if len(attachments) > 0 {
			eventMessage = buildCoreContentParts(evText, attachments)
		}
		threadID := h.resolveChatThread(inst, chatID)
		if err := h.instances.ForwardEvent(inst, eventMessage, threadID); err != nil {
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
	}(inst, body.Content, chatID, eventAttachments)

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
//
//	chat_id=… — per-chat panel stream (back-compat). Backfills since=
//	            from the DB then live-tails via the per-chat hub.
//	scope=user — global notifications stream. Live-tails every message
//	             inserted into any chat the user owns; no backfill (the
//	             tray seeds itself via /unread-summary on connect).
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

// GET /api/apps/channel-chat/approval-messages?project_id=<id>&status=pending&limit=20
//
// Returns approval-card chat messages for the authenticated user's
// agents. No separate approvals table: the component JSON remains the
// source of truth, and this endpoint is just an indexed-enough view
// over recent channel_chat_messages rows.
func (h *handlers) approvalMessages(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	ids, err := h.instances.InstanceIDsForUser(userID)
	if err != nil {
		log.Printf("[CHAT] approval-messages: list instances for user=%d: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.store.ListApprovalMessages(ids, r.URL.Query().Get("project_id"), r.URL.Query().Get("status"), limit)
	if err != nil {
		log.Printf("[CHAT] approval-messages: user=%d ids=%v: %v", userID, ids, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// POST /api/apps/channel-chat/message-action
// Body: {message_id, action_id, note?}
//
// Updates a built-in approval-card component in-place, publishes the
// updated chat row over SSE, and forwards an approval.result event to
// the agent thread that owns the chat.
func (h *handlers) messageAction(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	var body struct {
		MessageID int64  `json:"message_id"`
		ActionID  string `json:"action_id"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	body.ActionID = strings.TrimSpace(body.ActionID)
	if body.MessageID == 0 || body.ActionID == "" {
		http.Error(w, "message_id and action_id required", http.StatusBadRequest)
		return
	}
	msg, err := h.store.GetMessage(body.MessageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	chat, err := h.store.GetChat(msg.ChatID)
	if err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return
	}
	userID := h.instances.LookupUserID(r)
	inst, err := h.instances.OwnedInstance(userID, chat.AgentID)
	if err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return
	}
	updatedComponents, approval, err := applyApprovalAction(msg.Components, body.ActionID, body.Note, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := h.store.UpdateMessageComponents(msg.ID, updatedComponents)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	h.hub.publish(*updated)
	h.hub.publishToUser(inst.UserID, *updated)
	if h.bus != nil {
		h.bus.Publish("chat.message", "channel-chat", *updated)
	}

	threadID := msg.ThreadID
	if threadID == "" {
		threadID = h.resolveChatThread(inst, msg.ChatID)
	}
	evText := formatApprovalResultEvent(updated.ID, body.ActionID, approval, body.Note)
	forwardErr := h.instances.ForwardEvent(inst, evText, threadID)
	if forwardErr != nil {
		log.Printf("[CHAT] approval result forward message=%d agent=%d thread=%s action=%s: %v",
			updated.ID, inst.ID, threadID, body.ActionID, forwardErr)
	}
	writeJSON(w, map[string]any{
		"message":        updated,
		"status":         approval["status"],
		"forwarded":      forwardErr == nil,
		"delivery_error": errString(forwardErr),
	})
}

func applyApprovalAction(components []framework.ChatComponent, actionID, note string, userID int64) ([]framework.ChatComponent, map[string]any, error) {
	out := append([]framework.ChatComponent(nil), components...)
	for i, c := range out {
		if c.App != "channel-chat" || c.Name != "approval-card" {
			continue
		}
		props := copyProps(c.Props)
		status, _ := props["status"].(string)
		if status == "" {
			status = "pending"
		}
		if status != "pending" {
			return nil, nil, fmt.Errorf("approval already %s", status)
		}
		if !approvalActionAllowed(props["actions"], actionID) {
			return nil, nil, fmt.Errorf("unknown approval action %q", actionID)
		}
		nextStatus := approvalStatusForAction(actionID)
		decision := map[string]any{
			"action_id":  actionID,
			"status":     nextStatus,
			"user_id":    userID,
			"decided_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
		if strings.TrimSpace(note) != "" {
			decision["note"] = strings.TrimSpace(note)
		}
		props["status"] = nextStatus
		props["decision"] = decision
		out[i].Props = props
		return out, props, nil
	}
	return nil, nil, fmt.Errorf("approval card not found")
}

func copyProps(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func approvalActionAllowed(raw any, actionID string) bool {
	if actionID == "" {
		return false
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return actionID == "approve" || actionID == "deny"
	}
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			if id, _ := m["id"].(string); id == actionID {
				return true
			}
		}
	}
	return false
}

func approvalStatusForAction(actionID string) string {
	switch strings.ToLower(actionID) {
	case "approve", "approved", "yes", "allow":
		return "approved"
	case "deny", "denied", "reject", "rejected", "no":
		return "denied"
	default:
		return "acted"
	}
}

func formatApprovalResultEvent(messageID int64, actionID string, approval map[string]any, note string) string {
	title, _ := approval["title"].(string)
	status, _ := approval["status"].(string)
	var b strings.Builder
	b.WriteString("[approval.result]\n")
	b.WriteString(fmt.Sprintf("Approval message %d was %s with action %q.", messageID, status, actionID))
	if title != "" {
		b.WriteString("\nTitle: ")
		b.WriteString(title)
	}
	if strings.TrimSpace(note) != "" {
		b.WriteString("\nNote: ")
		b.WriteString(strings.TrimSpace(note))
	}
	b.WriteString("\nContinue from this decision. If this was requested from dashboard chat, send a visible channels_respond with the result when you have acted on it.")
	return b.String()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

func formatDashboardContext(v any) string {
	ctx, ok := v.(map[string]any)
	if !ok || len(ctx) == 0 {
		return ""
	}
	if source, _ := ctx["source"].(string); source != "dashboard-floating" {
		return ""
	}
	pick := func(key string) string {
		if s, ok := ctx[key].(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var lines []string
	lines = append(lines, "Dashboard context:")
	for _, item := range []struct {
		label string
		key   string
	}{
		{"page", "title"},
		{"route", "route"},
		{"project", "project_name"},
		{"project_id", "project_id"},
		{"kind", "page_kind"},
		{"detail", "detail"},
	} {
		if val := pick(item.key); val != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", item.label, val))
		}
	}
	if raw, ok := ctx["chips"].([]any); ok && len(raw) > 0 {
		var chips []string
		for _, item := range raw {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				chips = append(chips, strings.TrimSpace(s))
			}
		}
		if len(chips) > 0 {
			lines = append(lines, "- tags: "+strings.Join(chips, ", "))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func formatAgentChatEvent(text string, context any) string {
	var b strings.Builder
	b.WriteString("[chat]\n")
	b.WriteString("A user is talking to you in dashboard chat. Thoughts are not visible to the user. ")
	b.WriteString("Use channels_respond with channel=\"chat\" for visible chat messages. ")
	b.WriteString("A successful channels_respond wakes you again. If you promised work, continue after the tool result: call the needed tools, schedule yourself with pace, or explain why blocked. ")
	b.WriteString("After action tool results arrive, send another channels_respond with the outcome before pacing or going idle.\n\n")
	if ctx := formatDashboardContext(context); ctx != "" {
		b.WriteString(ctx)
		b.WriteString("\n\n")
	}
	b.WriteString("User message:\n")
	b.WriteString(strings.TrimSpace(text))
	return b.String()
}

func formatPlatformHelperChatEvent(text string, context any) string {
	var b strings.Builder
	b.WriteString("[chat]\n")
	b.WriteString("TASK TYPE: platform_assistant (NOT eval grading)\n\n")
	b.WriteString("You are Apteva Helper in the dashboard. Help the operator understand the current page, design agents, and turn rough goals into concrete next steps. ")
	b.WriteString("When the operator wants to create or manage agents, ask briefly for missing details, then use the apteva-server MCP tools such as agents_create, agents_list, agents_start, agents_stop, and agents_update when appropriate. ")
	b.WriteString("Do not grade anything. Do not return judge JSON. Use channels_respond with channel=\"chat\" for visible chat messages; if you promised tool work, continue after the respond result and then send another channels_respond with the outcome.\n\n")
	if ctx := formatDashboardContext(context); ctx != "" {
		b.WriteString(ctx)
		b.WriteString("\n\n")
	}
	b.WriteString("User message:\n")
	b.WriteString(strings.TrimSpace(text))
	return b.String()
}

type coreContentPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *coreImageURLPart `json:"image_url,omitempty"`
}

type coreImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

const (
	maxChatMessageBodyBytes = 20 << 20
	maxChatAttachments      = 4
	maxChatAttachmentBytes  = 5 << 20
	maxChatAttachmentTotal  = 12 << 20
)

func validateChatAttachments(in []ChatAttachment) ([]ChatAttachment, []ChatAttachment, error) {
	if len(in) == 0 {
		return nil, nil, nil
	}
	if len(in) > maxChatAttachments {
		return nil, nil, fmt.Errorf("too many attachments")
	}
	eventAttachments := make([]ChatAttachment, 0, len(in))
	persistedAttachments := make([]ChatAttachment, 0, len(in))
	var total int64
	for i, att := range in {
		if att.Type == "" {
			att.Type = "image"
		}
		if att.Type != "image" {
			return nil, nil, fmt.Errorf("unsupported attachment type")
		}
		mimeType, decodedSize, err := inspectImageDataURL(att.DataURL)
		if err != nil {
			return nil, nil, err
		}
		if decodedSize > maxChatAttachmentBytes {
			return nil, nil, fmt.Errorf("attachment too large")
		}
		total += decodedSize
		if total > maxChatAttachmentTotal {
			return nil, nil, fmt.Errorf("attachments too large")
		}
		if att.ID == "" {
			att.ID = fmt.Sprintf("ephemeral-%d", i+1)
		}
		att.MimeType = mimeType
		att.Size = decodedSize
		eventAttachments = append(eventAttachments, att)
		persisted := att
		persistedAttachments = append(persistedAttachments, persisted)
	}
	return eventAttachments, persistedAttachments, nil
}

func inspectImageDataURL(dataURL string) (string, int64, error) {
	prefix, encoded, ok := strings.Cut(strings.TrimSpace(dataURL), ",")
	if !ok || encoded == "" {
		return "", 0, fmt.Errorf("invalid image data URL")
	}
	if !strings.HasPrefix(strings.ToLower(prefix), "data:") || !strings.Contains(strings.ToLower(prefix), ";base64") {
		return "", 0, fmt.Errorf("invalid image data URL")
	}
	mimeType := strings.TrimPrefix(strings.Split(prefix, ";")[0], "data:")
	if !isAllowedImageMime(mimeType) {
		return "", 0, fmt.Errorf("unsupported image type")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", 0, fmt.Errorf("invalid image data")
	}
	if len(decoded) == 0 {
		return "", 0, fmt.Errorf("empty attachment")
	}
	return mimeType, int64(len(decoded)), nil
}

func buildCoreContentParts(text string, attachments []ChatAttachment) []coreContentPart {
	parts := []coreContentPart{{Type: "text", Text: text}}
	for _, att := range attachments {
		if att.Type != "image" || att.DataURL == "" {
			continue
		}
		parts = append(parts, coreContentPart{
			Type:     "image_url",
			ImageURL: &coreImageURLPart{URL: att.DataURL, Detail: "auto"},
		})
	}
	return parts
}

func isAllowedImageMime(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// resolveChatThread decides which core thread the chat's events
// should target. Flag off → "main" (legacy behavior). Platform-helper
// chat also stays on "main" so the dashboard widget and agent detail
// page use the same request path, and so the meta-agent cannot leave
// behind autonomous per-chat worker threads. Flag on for normal agents
// → look up (or assign) the chat's persisted thread id and spawn it
// idempotently on first use this process. Falls back to "main" on any
// error so a transient DB/network glitch can't drop messages.
func (h *handlers) resolveChatThread(inst framework.InstanceInfo, chatID string) string {
	if inst.Kind == "platform_helper" || !perThreadEnabled() {
		return "main"
	}
	threadID, err := h.store.EnsureChatThread(chatID)
	if err != nil || threadID == "" {
		log.Printf("[CHAT] EnsureChatThread chat=%s: %v — falling back to main", chatID, err)
		return "main"
	}
	cacheKey := fmt.Sprintf("%d/%s", inst.ID, chatID)

	// Compute the directive hash to compare against what the chat
	// thread was last spawned/updated with. The hash covers main's
	// LIVE directive PLUS the suffix constant, so it catches three
	// kinds of drift: a UI directive edit, the agent's own evolve,
	// and a change to chatThreadDirectiveSuffix shipped in a new
	// binary. MainDirective is one /config fetch; on the common
	// "already current" path it's the only round-trip we make.
	wantHash := ""
	if dir, derr := h.instances.MainDirective(inst); derr == nil {
		wantHash = directiveHash(dir + chatThreadDirectiveSuffix)
	} else {
		log.Printf("[CHAT] MainDirective inst=%d: %v — skipping drift check", inst.ID, derr)
	}

	prev, alreadySpawned := spawnedChatThreads.Load(cacheKey)
	// Already spawned AND current (or we couldn't compute a hash to
	// compare against) → reuse as-is.
	if alreadySpawned && (wantHash == "" || prev.(string) == wantHash) {
		return threadID
	}

	// We're going to either spawn (first time) or update (drifted).
	// Both want main's MCP surface. "channels" is always required —
	// the chat thread needs channels_respond to reply at all; the
	// rest mirrors the instance's effective MCP list so quick
	// reads/lookups can be served without round-tripping through main.
	mcps, err := h.instances.ListMCPNames(inst)
	if err != nil || len(mcps) == 0 {
		log.Printf("[CHAT] ListMCPNames inst=%d: %v — using minimal fallback", inst.ID, err)
		mcps = fallbackChatThreadMCPs
	} else {
		mcps = ensureChannels(mcps)
	}

	if alreadySpawned {
		// Directive drifted — update the LIVE thread in place so its
		// session (conversation history) survives. The chat thread
		// answers the user's next message under the new directive.
		if err := h.instances.UpdateThread(inst, threadID, chatThreadDirectiveSuffix, chatThreadTools); err != nil {
			log.Printf("[CHAT] UpdateThread chat=%s thread=%s: %v — keeping stale directive", chatID, threadID, err)
			return threadID // stale but functional; better than dropping the message
		}
		spawnedChatThreads.Store(cacheKey, wantHash)
		return threadID
	}

	// First spawn this process. The "directive" arg flows into core as
	// directive_suffix: the thread inherits main's directive verbatim
	// and appends this chat-handling hint.
	if err := h.instances.SpawnThread(inst, threadID, chatThreadDirectiveSuffix, chatThreadTools, mcps); err != nil {
		log.Printf("[CHAT] SpawnThread chat=%s thread=%s: %v — falling back to main", chatID, threadID, err)
		return "main"
	}
	spawnedChatThreads.Store(cacheKey, wantHash)
	return threadID
}

// directiveHash is a cheap stable fingerprint of a chat thread's
// composed directive (main directive + suffix) used to detect drift.
func directiveHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ensureChannels guarantees the `channels` MCP is in the slice —
// without it the chat thread literally cannot respond to the user.
// Idempotent; preserves the input order.
func ensureChannels(mcps []string) []string {
	for _, m := range mcps {
		if m == "channels" {
			return mcps
		}
	}
	return append([]string{"channels"}, mcps...)
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
