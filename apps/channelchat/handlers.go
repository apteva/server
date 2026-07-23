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

// Every internal conversation uses an isolated runtime context by default,
// including the primary/default chat and platform-helper conversations.
// CHANNELCHAT_PER_THREAD=0 is the temporary rollback switch.
func perThreadEnabled() bool {
	return strings.TrimSpace(os.Getenv("CHANNELCHAT_PER_THREAD")) != "0"
}

// chatThreadDirectiveSuffix is appended to main's directive for an ordinary
// user-facing conversation. Core supplies the durable conversation runtime;
// the server owns this role and communication policy.
const chatThreadDirectiveSuffix = `

---
[USER CHAT ROLE]
You are this agent's user-facing conversation endpoint. Talk naturally with the
user, perform interactive work with your attached tools, and remain responsible
for the user-visible result. Do not behave like main's autonomous monitor and do
not start unrelated work merely because it appears in the inherited directive.

[USER COMMUNICATION]
- Deliver user-visible text only through channels_send(channel="current", ...).
- An immediate answer needs one complete final message and no preliminary acknowledgement.
- Before noticeable tool work, you may send one short acknowledgement naming the concrete user-facing action. Wait for its successful receipt before starting action tools.
- For longer work, keep the user informed at major phases or achievements: when a meaningful phase completes, a child job starts or finishes, the plan materially changes, work becomes blocked, user input is needed, or you begin waiting on a slow external result. A useful progress message says what was achieved and what meaningful step comes next.
- Do not narrate every tool call, search result, retry, temporary plan, or unchanged wait. Never send repetitive "still working" updates.
- Progress never replaces the final outcome. After tool work, send exactly one complete final result, clarification, blocker, or next question. A successful final delivery ends the user turn; never repeat or paraphrase it.
- Thoughts and plain assistant output are not visible to the user and do not count as a reply.
- Any active user turn with an observed non-channel tool result is still unfinished. On your next decision, either run the next required action tool or send the visible outcome or question with channels_send. Pace, done, and idle are prohibited until the final channels_send receipt succeeds.

[INTERACTIVE WORK AND CHILD JOBS]
- Handle short interactive work yourself.
- For substantial, parallel, isolated, or slow one-off work, you may create temporary child jobs. Those jobs are your defined team for this conversation.
- Give each child a distinct concrete assignment and only the tools or MCPs it needs. Every child assignment must name the exact result it owes you and explicitly require the child to report that result to its parent before sleeping. Children report to you; you synthesize their work and communicate with the user.
- A successful spawn receipt means only that the child started; it is never the child's result. After any useful start/progress message, wait for the child to report. Do not send the final user outcome until you have consumed every child result the outcome depends on.
- Do not spawn replacements because a child is quiet. Update or cancel your own children when the user's request changes or is cancelled.
- Child jobs are for one-off work, not recurring or autonomous responsibilities.

[SELECTIVE REPORTING TO MAIN]
- Keep ordinary answers, minor progress, raw tool output, routine retries, temporary plans, and ordinary one-off completions inside this conversation.
- Send main a concise REPORT ONLY — no action or reply required: ... message only when wider coordination genuinely benefits: a significant goal or child job begins or completes, a plan-changing milestone occurs, an important artifact or workspace change is produced, or a blocker, conflict, permission, or resource issue affects persistent work. A report-only message does not make you wait and does not replace the user-facing final result.
- Use one ACTION REQUIRED — reply to this conversation: ... message when work must continue after this chat, changes persistent behavior, creates a recurring responsibility, requires unavailable authority, or needs coordination across persistent threads. End it by asking main to reply to this originating conversation with the result, then wait for that reply before confirming completion to the user.
- Never send the same milestone both as a report and an action request. Do not forward every child event. If main replies to a report-only message without being asked, use it only when it materially changes the user's outcome.
- After relaying an action-required result to the user, do not send main a confirmation or completion acknowledgement.

[DIRECTIVE OWNERSHIP]
Conversation-local preferences remain in this conversation. Never call evolve
for this chat thread. Send persistent behavior or memory changes to main as an
ACTION REQUIRED request. If the inherited directive is only a placeholder,
ignore it.

[PRIVACY]
Never expose internal terms such as main, parent thread, child thread,
directive, handoff, concierge, idle, or tool names to the user.`

// Platform Helper owns an authoritative control-plane tool surface. Durable
// mutations performed through those tools are already complete and persisted;
// routing them through Helper main adds latency, hides useful tool activity
// from the conversation, and creates another opportunity for duplicate replies.
// Keep this separate from the ordinary-agent handoff policy above.
const platformHelperChatThreadDirectiveSuffix = chatThreadDirectiveSuffix + "\n\n" +
	"[PLATFORM HELPER CONVERSATION POLICY]\n" +
	"Use the Apteva control-plane tools attached to this conversation directly. If an available " +
	"tool can complete the operator's request now, follow the shared acknowledgement and selective-progress guidance, " +
	"call it here, and report its authoritative receipt in exactly one final message. This includes " +
	"agents_get, agents_update (including directive and schedule edits), agent " +
	"lifecycle actions, apps, integrations, connections, and MCP-server configuration. An atomic " +
	"tool mutation is already durable and MUST NOT be handed to main merely because it changes " +
	"persistent behavior. For an agent directive edit, inspect the target when needed, call " +
	"agents_update directly, then send exactly one complete final channels_send outcome. An optional " +
	"acknowledgement or progress message does not replace that final outcome. Temporary children may " +
	"assist with substantial one-off research or analysis, but they do not perform control-plane mutations " +
	"that this conversation can perform directly. Do not call core send(id=\"main\") for work that an attached " +
	"Apteva tool can finish in this conversation. Only " +
	"hand off genuinely ongoing work that cannot finish in this turn, a change to Apteva Helper's own " +
	"durable directive, or work requiring a capability unavailable here. A successful final " +
	"channels_send receipt ends the user turn: pace and never send a second paraphrase."

type chatThreadProfile struct {
	DirectiveSuffix string
	Tools           []string
}

// chatThreadTools is the local Core tool profile for user-facing chats. spawn
// makes the chat a leader of temporary one-off children; send handles selective
// reports and durable requests; pace idles it between user turns. The actual
// work surface still comes from the agent's attached MCPs.
var chatThreadTools = []string{"send", "spawn", "pace"}

func chatThreadProfileFor(inst framework.InstanceInfo) chatThreadProfile {
	directive := chatThreadDirectiveSuffix
	if inst.Kind == "platform_helper" {
		directive = platformHelperChatThreadDirectiveSuffix
	}
	return chatThreadProfile{
		DirectiveSuffix: directive,
		Tools:           append([]string(nil), chatThreadTools...),
	}
}

func chatThreadDirectiveFor(inst framework.InstanceInfo) string {
	return chatThreadProfileFor(inst).DirectiveSuffix
}

// fallbackChatThreadMCPs is the floor MCP set used when resolver
// enumeration fails (e.g. the agent is mid-restart and the DB
// query errors). `channels` is the bare minimum for the thread to
// reply at all.
var fallbackChatThreadMCPs = []string{"channels"}

// spawnedChatThreads remembers the last directive hash applied to each
// (instance, chat) pair. It is only a drift-detection optimization: every
// delivery still issues an idempotent SpawnThread ensure, so a core child
// restart cannot leave this server cache pointing at a missing thread.
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

	presenceMu     sync.Mutex
	presenceStates map[string]*chatPresenceState
	presenceGrace  time.Duration
	shutdownGrace  time.Duration
}

type chatPresenceState struct {
	inst        framework.InstanceInfo
	subscribers int
	connected   bool
	generation  uint64
	timer       *time.Timer
}

const defaultChatPresenceGrace = 3 * time.Second
const defaultConversationShutdownGrace = 15 * time.Second

const conversationClosingEvent = "[chat.session_closing] This conversation was permanently deleted. " +
	"Do not call channels_send, channels_respond, or publish anything to the deleted conversation. " +
	"Call done exactly once now. Use done(message=...) to give main a concise final handoff with " +
	"meaningful decisions, completed actions, and pending durable work from this conversation. " +
	"If there is no durable handoff, say that plainly. Do not continue working after done."

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

	// ThreadTools returns the live effective tool allowlist. Channelchat uses
	// it to add newly introduced profile tools without removing MCP tools that
	// Core resolved when the conversation was originally spawned.
	ThreadTools(inst framework.InstanceInfo, threadID string) ([]string, error)

	// UpdateThread pushes a new directive and, when tools is non-empty, an
	// already-merged effective tool allowlist into a LIVE core thread without
	// killing it or losing conversation history.
	UpdateThread(inst framework.InstanceInfo, threadID, directiveSuffix string, tools []string) error

	// KillThread permanently removes a non-main core thread. It must be
	// idempotent and also remove the persisted thread definition when the
	// agent is stopped. Conversation deletion uses it as the bounded fallback
	// when a chat thread does not honor its graceful done instruction.
	KillThread(inst framework.InstanceInfo, threadID string) error

	// ListThreadIDs returns live or persisted core thread definitions for an
	// agent. Channel-chat uses it at mount to remove chat-conv-* definitions
	// whose authoritative conversation row no longer exists.
	ListThreadIDs(inst framework.InstanceInfo) ([]string, error)

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
// Legacy explicit single-agent conversation creation. Unlike the internal
// default-* inbox record, the returned conv-* conversation is user-visible
// and can be archived or deleted. Body also accepts the legacy instance_id.
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
	inst, err := h.instances.OwnedInstance(userID, body.AgentID)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = inst.Name
	}
	chat, err := h.store.CreateConversation(userID, inst.ProjectID, title, []int64{body.AgentID}, body.AgentID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, chat)
}

// conversations is the project-level collection used by the dashboard. The
// legacy /chats collection now exposes the same explicit conv-* records for a
// single agent; internal default-* inbox records are never returned.
func (h *handlers) conversations(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	switch r.Method {
	case http.MethodGet:
		userID := h.instances.LookupUserID(r)
		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		if userID == 0 || projectID == "" {
			http.Error(w, "project_id required", http.StatusBadRequest)
			return
		}
		rows, err := h.store.ListConversations(userID, projectID, r.URL.Query().Get("archived") == "1", "")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, rows)
	case http.MethodPost:
		userID := h.instances.LookupUserID(r)
		var body struct {
			ProjectID   string  `json:"project_id"`
			Title       string  `json:"title"`
			AgentIDs    []int64 `json:"agent_ids"`
			LeadAgentID int64   `json:"lead_agent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.AgentIDs) == 0 {
			http.Error(w, "agent_ids required", http.StatusBadRequest)
			return
		}
		if len(body.AgentIDs) > 8 || len([]rune(strings.TrimSpace(body.Title))) > 120 {
			http.Error(w, "conversation supports up to 8 agents and a 120 character title", http.StatusBadRequest)
			return
		}
		projectID, agents, ok := h.validateConversationAgents(w, userID, body.ProjectID, body.AgentIDs)
		if !ok {
			return
		}
		if body.LeadAgentID == 0 {
			body.LeadAgentID = body.AgentIDs[0]
		}
		leadFound := false
		for _, id := range body.AgentIDs {
			if id == body.LeadAgentID {
				leadFound = true
				break
			}
		}
		if !leadFound {
			http.Error(w, "lead_agent_id must be a participant", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Title) == "" {
			if len(agents) == 1 {
				body.Title = agents[0].Name
			} else {
				body.Title = "New conversation"
			}
		}
		chat, err := h.store.CreateConversation(userID, projectID, body.Title, body.AgentIDs, body.LeadAgentID)
		if err != nil {
			log.Printf("[CHAT] create conversation: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, chat)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handlers) conversation(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	chat, _, ok := h.authorizeConversation(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, chat)
	case http.MethodPatch:
		var body struct {
			Title    string `json:"title"`
			Archived *bool  `json:"archived"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if len([]rune(strings.TrimSpace(body.Title))) > 120 {
			http.Error(w, "title is too long", http.StatusBadRequest)
			return
		}
		if body.Archived != nil && *body.Archived && chat.ID == defaultChatID(chat.AgentID) {
			http.Error(w, "primary conversation cannot be archived", http.StatusConflict)
			return
		}
		updated, err := h.store.UpdateConversation(chat.ID, body.Title, body.Archived)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if chat.ID == defaultChatID(chat.AgentID) {
			http.Error(w, "primary conversation cannot be deleted", http.StatusConflict)
			return
		}
		if err := h.store.DeleteConversation(chat.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.stopConversationPresence(chat.ID)
		h.shutdownConversationThreads(chat)
		writeJSON(w, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handlers) participants(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	chat, _, ok := h.authorizeConversation(w, r)
	if !ok {
		return
	}
	var body struct {
		AgentID int64 `json:"agent_id"`
	}
	if r.Method == http.MethodDelete {
		body.AgentID, _ = strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	} else if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.AgentID == 0 {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	inst, err := h.instances.OwnedInstance(chat.OwnerUserID, body.AgentID)
	if err != nil || inst.ProjectID != chat.ProjectID {
		http.Error(w, "agent not found in conversation project", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPost:
		if chat.ID == defaultChatID(chat.AgentID) {
			http.Error(w, "primary conversation participants cannot be changed", http.StatusConflict)
			return
		}
		if len(chat.AgentIDs) >= 8 {
			for _, existingID := range chat.AgentIDs {
				if existingID == body.AgentID {
					updated, getErr := h.store.GetChat(chat.ID)
					if getErr != nil {
						http.Error(w, "internal error", http.StatusInternalServerError)
						return
					}
					writeJSON(w, updated)
					return
				}
			}
			http.Error(w, "conversation supports up to 8 agents", http.StatusConflict)
			return
		}
		err = h.store.AddParticipant(chat.ID, body.AgentID)
	case http.MethodDelete:
		if chat.ID == defaultChatID(chat.AgentID) {
			http.Error(w, "primary conversation participants cannot be changed", http.StatusConflict)
			return
		}
		err = h.store.RemoveParticipant(chat.ID, body.AgentID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	updated, err := h.store.GetChat(chat.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, updated)
}

func (h *handlers) validateConversationAgents(w http.ResponseWriter, userID int64, requestedProject string, agentIDs []int64) (string, []framework.InstanceInfo, bool) {
	seen := map[int64]bool{}
	agents := make([]framework.InstanceInfo, 0, len(agentIDs))
	projectID := strings.TrimSpace(requestedProject)
	for _, id := range agentIDs {
		if id == 0 || seen[id] {
			http.Error(w, "invalid or duplicate agent_ids", http.StatusBadRequest)
			return "", nil, false
		}
		seen[id] = true
		inst, err := h.instances.OwnedInstance(userID, id)
		if err != nil {
			http.Error(w, "agent not found", http.StatusNotFound)
			return "", nil, false
		}
		if projectID == "" {
			projectID = inst.ProjectID
		}
		// The platform helper is one user-owned, global agent. Allow it to
		// participate in an explicitly scoped project conversation without
		// moving the singleton helper row into that project.
		globalPlatformHelper := inst.Kind == "platform_helper" && inst.ProjectID == "" && projectID != ""
		if inst.ProjectID != projectID && !globalPlatformHelper {
			http.Error(w, "all agents must belong to the same project", http.StatusBadRequest)
			return "", nil, false
		}
		agents = append(agents, inst)
	}
	return projectID, agents, true
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
	chatID, _, ok := h.authorizeChat(w, r)
	if !ok {
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var msgs []Message
	var err error
	if since > 0 {
		msgs, err = h.store.ListMessages(chatID, since, limit)
	} else {
		msgs, err = h.store.ListRecentMessages(chatID, limit)
	}
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
	chat, _, ok := h.authorizeConversation(w, r)
	if !ok {
		return
	}
	chatID := chat.ID
	var body struct {
		Content        string           `json:"content"`
		Attachments    []ChatAttachment `json:"attachments"`
		Context        any              `json:"context"`
		TargetAgentIDs []int64          `json:"target_agent_ids"`
		ClientID       string           `json:"client_message_id"`
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
	eventAttachments, persistedAttachments, err := validateChatAttachments(body.Attachments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	targets, err := h.resolveConversationTargets(chat, body.Content, body.TargetAgentIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	targetIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		targetIDs = append(targetIDs, target.ID)
	}
	metadata := map[string]any{"target_agent_ids": targetIDs}
	if body.Context != nil {
		metadata["context"] = body.Context
	}
	m, inserted, err := h.store.AppendUserMessageWithDeliveries(chatID, body.Content, userID, persistedAttachments, metadata, body.ClientID, targetIDs)
	if err != nil {
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	if !inserted {
		writeJSON(w, m)
		return
	}
	h.hub.publish(*m)
	if h.bus != nil {
		h.bus.Publish("chat.message", "channel-chat", *m)
	}

	// Forward to the agent's /event endpoint using the same shape
	// the Slack / email paths use. We keep the stable "[chat]" tag as
	// the event source label, while the channels MCP resolves the
	// reply target to the canonical internal channel "apteva".
	//
	// Failure used to be silent — the DB row persisted but the agent
	// never saw the message, and the user had no way to know. Now we
	// log loudly AND drop a system row into the chat so the user sees
	// "agent unreachable, will see the message when it's running again"
	// instead of an indefinite quiet.
	go func(targets []framework.InstanceInfo, text string, chat Chat, attachments []ChatAttachment, messageID int64, context any) {
		participantNames := make([]string, 0, len(targets))
		for _, target := range targets {
			participantNames = append(participantNames, target.Name)
		}
		for _, inst := range targets {
			deliveryErr := h.deliverConversationMessage(inst, chat, text, attachments, context, participantNames)
			_ = h.store.MarkDelivery(messageID, inst.ID, deliveryErr == nil, deliveryErr)
			if deliveryErr == nil {
				continue
			}
			log.Printf("[CHAT] ForwardEvent FAILED chat=%s instance=%d: %v", chat.ID, inst.ID, deliveryErr)
			notice := fmt.Sprintf("(could not reach %s — your message is saved. err: %v)", inst.Name, deliveryErr)
			if sm, sErr := h.store.Append(chat.ID, "system", notice, nil, "", "final", nil); sErr == nil {
				h.hub.publish(*sm)
			}
		}
	}(targets, body.Content, *chat, eventAttachments, m.ID, body.Context)

	writeJSON(w, m)
}

func (h *handlers) deliverConversationMessage(inst framework.InstanceInfo, chat Chat, text string, attachments []ChatAttachment, context any, addressedNames []string) error {
	evText := formatAgentChatEvent(text, context)
	if inst.Kind == "platform_helper" {
		evText = formatPlatformHelperChatEvent(text, context)
	}
	if chat.Kind == "room" {
		evText += fmt.Sprintf("\nConversation: %s. Addressed agent: %s. Other addressed agents: %s. Reply to this same conversation using channels_send(channel=\"current\", ...).",
			chat.Title, inst.Name, strings.Join(addressedNames, ", "))
	}
	var eventMessage any = evText
	if len(attachments) > 0 {
		eventMessage = buildCoreContentParts(evText, attachments)
	}
	_, err := h.forwardConversationEvent(inst, chat.ID, eventMessage)
	return err
}

func (h *handlers) retryPendingDeliveries() error {
	pending, err := h.store.ListPendingDeliveries(25)
	if err != nil {
		return err
	}
	for _, delivery := range pending {
		inst, err := h.instances.OwnedInstance(delivery.Chat.OwnerUserID, delivery.AgentID)
		if err == nil {
			context := delivery.Message.Metadata["context"]
			names := make([]string, 0, len(delivery.Chat.AgentIDs))
			for _, agentID := range delivery.Chat.AgentIDs {
				participant, lookupErr := h.instances.OwnedInstance(delivery.Chat.OwnerUserID, agentID)
				if lookupErr == nil {
					names = append(names, participant.Name)
				}
			}
			err = h.deliverConversationMessage(inst, delivery.Chat, delivery.Message.Content, delivery.Message.Attachments, context, names)
		}
		_ = h.store.MarkDelivery(delivery.Message.ID, delivery.AgentID, err == nil, err)
	}
	return nil
}

func (h *handlers) resolveConversationTargets(chat *Chat, text string, explicit []int64) ([]framework.InstanceInfo, error) {
	participants := map[int64]framework.InstanceInfo{}
	for _, agentID := range chat.AgentIDs {
		inst, err := h.instances.OwnedInstance(chat.OwnerUserID, agentID)
		if err != nil {
			return nil, fmt.Errorf("conversation participant %d is unavailable", agentID)
		}
		participants[agentID] = inst
	}
	selected := []int64{}
	if len(explicit) > 0 {
		selected = append(selected, explicit...)
	} else if chat.Kind == "room" {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "@all") {
			selected = append(selected, chat.AgentIDs...)
		} else {
			for _, id := range chat.AgentIDs {
				inst := participants[id]
				if strings.Contains(lower, "@"+strings.ToLower(inst.Name)) {
					selected = append(selected, id)
				}
			}
		}
	}
	if len(selected) == 0 {
		selected = []int64{chat.AgentID}
	}
	seen := map[int64]bool{}
	out := make([]framework.InstanceInfo, 0, len(selected))
	for _, id := range selected {
		inst, exists := participants[id]
		if !exists {
			return nil, fmt.Errorf("agent %d is not a conversation participant", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, inst)
	}
	return out, nil
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
	chatID, inst, ok := h.authorizeChat(w, r)
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

	// Subscribe before querying the DB. Messages written during catch-up are
	// buffered by the hub and deduplicated by `since` below, closing the race
	// where an insert between the final query and subscribe was lost forever.
	ch, _, cancel := h.hub.subscribe(chatID)
	defer cancel()
	streamCh, _, streamCancel := h.hub.subscribeStream(chatID)
	defer streamCancel()
	h.chatStreamOpened(chatID, inst)
	defer h.chatStreamClosed(chatID)

	// Backfill from DB — every page since the client's checkpoint. The old
	// one-shot LIMIT 1000 query silently skipped the rest of a large gap.
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		since, _ = strconv.ParseInt(sinceStr, 10, 64)
	}
	for {
		backfill, err := h.store.ListMessages(chatID, since, 1000)
		if err != nil {
			break
		}
		for _, m := range backfill {
			writeSSE(w, m)
			if m.ID > since {
				since = m.ID
			}
		}
		flusher.Flush()
		if len(backfill) < 1000 {
			break
		}
	}

	// Parallel stream-frame subscription. Ephemeral LLM-args chunks
	// for in-progress `channels_send` calls (and legacy respond calls) land here. Separate
	// from the Message channel so reverting streaming = delete this
	// block (no Message-path side effects).
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

// GET /api/apps/channel-chat/report-messages?project_id=<id>&limit=20
//
// Returns report-card messages for the authenticated user's agents.
// Reports reuse channel_chat_messages as the persistence layer, but
// normal chat history/stream queries filter them out so they remain
// inbox artifacts instead of chat turns.
func (h *handlers) reportMessages(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	ids, err := h.instances.InstanceIDsForUser(userID)
	if err != nil {
		log.Printf("[CHAT] report-messages: list instances for user=%d: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.store.ListReportMessages(ids, r.URL.Query().Get("project_id"), limit)
	if err != nil {
		log.Printf("[CHAT] report-messages: user=%d ids=%v: %v", userID, ids, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// GET /api/apps/channel-chat/alert-messages?project_id=<id>&limit=20
//
// Returns alert-card messages for the authenticated user's agents.
func (h *handlers) alertMessages(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	ids, err := h.instances.InstanceIDsForUser(userID)
	if err != nil {
		log.Printf("[CHAT] alert-messages: list instances for user=%d: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.store.ListAlertMessages(ids, r.URL.Query().Get("project_id"), limit)
	if err != nil {
		log.Printf("[CHAT] alert-messages: user=%d ids=%v: %v", userID, ids, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// GET /api/apps/channel-chat/current-statuses?project_id=<id>
//
// Returns one mutable status-card per agent. Statuses are monitoring state,
// not inbox artifacts, so this endpoint is separate from alert/report views.
func (h *handlers) currentStatuses(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	userID := h.instances.LookupUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	ids, err := h.instances.InstanceIDsForUser(userID)
	if err != nil {
		log.Printf("[CHAT] current-statuses: list agents for user=%d: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows, err := h.store.ListCurrentStatuses(ids, r.URL.Query().Get("project_id"))
	if err != nil {
		log.Printf("[CHAT] current-statuses: user=%d ids=%v: %v", userID, ids, err)
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
		threadID, err = h.ensureChatThread(inst, msg.ChatID)
		if err != nil {
			log.Printf("[CHAT] approval thread resolution message=%d agent=%d: %v", updated.ID, inst.ID, err)
			writeJSON(w, map[string]any{
				"message": updated, "status": approval["status"], "forwarded": false, "delivery_error": err.Error(),
			})
			return
		}
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

// POST /api/apps/channel-chat/message-dismiss
// Body: {message_id}
//
// Hides an inbox artifact (approval/report/alert) from inbox queries
// by updating the component props in-place. This is intentionally not
// an approval decision and does not notify the agent.
func (h *handlers) messageDismiss(w http.ResponseWriter, r *http.Request, _ *framework.AppCtx) {
	var body struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.MessageID == 0 {
		http.Error(w, "message_id required", http.StatusBadRequest)
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
	updatedComponents, err := applyInboxDismiss(msg.Components, userID)
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
	writeJSON(w, map[string]any{
		"message":   updated,
		"dismissed": true,
	})
}

func applyInboxDismiss(components []framework.ChatComponent, userID int64) ([]framework.ChatComponent, error) {
	out := append([]framework.ChatComponent(nil), components...)
	for i, c := range out {
		if c.App != "channel-chat" || !isInboxComponentName(c.Name) {
			continue
		}
		props := copyProps(c.Props)
		if componentDismissed(props) {
			return out, nil
		}
		props["dismissed"] = true
		props["dismissed_by"] = userID
		props["dismissed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		out[i].Props = props
		return out, nil
	}
	return nil, fmt.Errorf("inbox component not found")
}

func isInboxComponentName(name string) bool {
	switch name {
	case "approval-card", "report-card", "alert-card":
		return true
	default:
		return false
	}
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
	b.WriteString("\nContinue from this decision. If this was requested from dashboard chat, send a visible channels_send with channel=\"current\" and complete text when you have acted on it.")
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

// presence handles explicit Connect/Disconnect requests from compatible
// clients. Normal connected state is derived from the actual SSE subscription
// in stream: a short grace period makes refresh/remount invisible while a real
// tab close still becomes a disconnected event.
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
	switch body.Action {
	case "connected":
	case "disconnected":
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
	shouldForward := h.applyExplicitPresence(body.ChatID, inst, body.Action)
	threadID, exists := h.existingConversationThread(chat)
	if shouldForward {
		if err := h.forwardPresenceToExistingThread(inst, threadID, exists, body.Action); err != nil {
			log.Printf("[CHAT] presence forward chat=%s thread=%s action=%s: %v",
				body.ChatID, threadID, body.Action, err)
		}
	}
	writeJSON(w, map[string]string{"status": "ok", "thread_id": threadID})
}

func (h *handlers) chatStreamOpened(chatID string, inst framework.InstanceInfo) {
	h.presenceMu.Lock()
	if h.presenceStates == nil {
		h.presenceStates = make(map[string]*chatPresenceState)
	}
	state := h.presenceStates[chatID]
	if state == nil {
		state = &chatPresenceState{}
		h.presenceStates[chatID] = state
	}
	state.inst = inst
	state.subscribers++
	state.generation++
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	shouldForward := !state.connected
	state.connected = true
	h.presenceMu.Unlock()
	if shouldForward {
		go func() {
			if err := h.forwardPresence(inst, chatID, "connected"); err != nil {
				log.Printf("[CHAT] stream presence connect chat=%s agent=%d: %v", chatID, inst.ID, err)
			}
		}()
	}
}

func (h *handlers) chatStreamClosed(chatID string) {
	h.presenceMu.Lock()
	state := h.presenceStates[chatID]
	if state == nil {
		h.presenceMu.Unlock()
		return
	}
	if state.subscribers > 0 {
		state.subscribers--
	}
	if state.subscribers > 0 || !state.connected {
		h.presenceMu.Unlock()
		return
	}
	state.generation++
	generation := state.generation
	grace := h.presenceGrace
	if grace <= 0 {
		grace = defaultChatPresenceGrace
	}
	state.timer = time.AfterFunc(grace, func() {
		h.presenceMu.Lock()
		current := h.presenceStates[chatID]
		if current == nil || current.generation != generation || current.subscribers > 0 || !current.connected {
			h.presenceMu.Unlock()
			return
		}
		current.connected = false
		current.timer = nil
		inst := current.inst
		h.presenceMu.Unlock()
		if err := h.forwardPresence(inst, chatID, "disconnected"); err != nil {
			log.Printf("[CHAT] stream presence disconnect chat=%s agent=%d: %v", chatID, inst.ID, err)
		}
	})
	h.presenceMu.Unlock()
}

// stopConversationPresence prevents a stream-close timer from resolving a
// deleted chat and falling back to main. Existing SSE handlers may still run
// their deferred chatStreamClosed call, which becomes a harmless no-op after
// the state is removed here.
func (h *handlers) stopConversationPresence(chatID string) {
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	state := h.presenceStates[chatID]
	if state != nil && state.timer != nil {
		state.timer.Stop()
	}
	delete(h.presenceStates, chatID)
}

// shutdownConversationThreads gives every participant's dedicated chat
// thread one last event in which to hand useful state to main and call done.
// The database row has already been deleted, so a late channels_send cannot
// write to this conversation or be rerouted to a different one. Deletion is
// intentionally not held open while an LLM finishes: the force-kill timer is
// the bounded reliability backstop and KillThread is idempotent if done wins.
func (h *handlers) shutdownConversationThreads(chat *Chat) {
	if chat == nil {
		return
	}
	threadID := strings.TrimSpace(chat.ThreadID)
	if threadID == "" || threadID == "main" {
		return
	}
	grace := h.shutdownGrace
	if grace <= 0 {
		grace = defaultConversationShutdownGrace
	}
	for _, agentID := range chat.AgentIDs {
		inst, err := h.instances.OwnedInstance(chat.OwnerUserID, agentID)
		if err != nil {
			log.Printf("[CHAT] delete conversation=%s resolve agent=%d: %v", chat.ID, agentID, err)
			continue
		}
		cacheKey := fmt.Sprintf("%d/%s", inst.ID, chat.ID)
		spawnedChatThreads.Delete(cacheKey)

		// Schedule the hard cleanup before forwarding so a stuck core request
		// cannot leave the runtime thread alive forever.
		time.AfterFunc(grace, func() {
			if err := h.instances.KillThread(inst, threadID); err != nil {
				log.Printf("[CHAT] force-kill deleted conversation=%s agent=%d thread=%s: %v", chat.ID, inst.ID, threadID, err)
			}
		})
		go func() {
			if err := h.instances.ForwardEvent(inst, conversationClosingEvent, threadID); err != nil {
				log.Printf("[CHAT] notify deleted conversation=%s agent=%d thread=%s: %v", chat.ID, inst.ID, threadID, err)
			}
		}()
	}
}

// cleanupOrphanConversationThreads reconciles persisted Core conversation
// threads with channel-chat's authoritative database. A server process can
// stop during the graceful deletion window, before its force-kill timer fires;
// without this mount-time sweep that deleted conversation would return on the
// next Core restart and remain forever. Only the reserved chat-conv-* namespace
// is touched. Default chats and unrelated worker/realtime threads are ignored.
func (h *handlers) cleanupOrphanConversationThreads() (int, error) {
	if h == nil || h.store == nil || h.instances == nil {
		return 0, nil
	}
	owners, err := h.store.AgentConversationThreads()
	if err != nil {
		return 0, err
	}
	removed := 0
	var cleanupErrs []error
	for _, owner := range owners {
		inst, err := h.instances.OwnedInstance(owner.UserID, owner.AgentID)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("resolve agent %d: %w", owner.AgentID, err))
			continue
		}
		threadIDs, err := h.instances.ListThreadIDs(inst)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("list agent %d threads: %w", owner.AgentID, err))
			continue
		}
		for _, threadID := range threadIDs {
			threadID = strings.TrimSpace(threadID)
			if !strings.HasPrefix(threadID, "chat-conv-") {
				continue
			}
			if _, exists := owner.ThreadIDs[threadID]; exists {
				continue
			}
			if err := h.instances.KillThread(inst, threadID); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("kill orphan agent %d thread %s: %w", owner.AgentID, threadID, err))
				continue
			}
			spawnedChatThreads.Delete(fmt.Sprintf("%d/%s", owner.AgentID, strings.TrimPrefix(threadID, "chat-")))
			removed++
		}
	}
	return removed, errors.Join(cleanupErrs...)
}

// cleanupUnusedConversationThreads removes runtime threads produced by older
// passive-presence behavior. It deliberately keeps the conversation database
// row so agent-detail routing and explicitly created empty conversations still
// exist; the first real user message will assign and spawn a fresh thread.
func (h *handlers) cleanupUnusedConversationThreads() (int, error) {
	if h == nil || h.store == nil || h.instances == nil {
		return 0, nil
	}
	chats, err := h.store.UnusedConversationThreads()
	if err != nil {
		return 0, err
	}
	removed := 0
	var cleanupErrs []error
	for i := range chats {
		chat := &chats[i]
		threadID := strings.TrimSpace(chat.ThreadID)
		if threadID == "" || threadID == "main" {
			continue
		}
		allRemoved := true
		for _, agentID := range chat.AgentIDs {
			inst, err := h.instances.OwnedInstance(chat.OwnerUserID, agentID)
			if err != nil {
				allRemoved = false
				cleanupErrs = append(cleanupErrs, fmt.Errorf("resolve unused chat %s agent %d: %w", chat.ID, agentID, err))
				continue
			}
			if err := h.instances.KillThread(inst, threadID); err != nil {
				allRemoved = false
				cleanupErrs = append(cleanupErrs, fmt.Errorf("kill unused chat %s agent %d thread %s: %w", chat.ID, agentID, threadID, err))
				continue
			}
			spawnedChatThreads.Delete(fmt.Sprintf("%d/%s", agentID, chat.ID))
		}
		if !allRemoved {
			continue
		}
		if err := h.store.ClearUnusedConversationThread(chat.ID, threadID); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("clear unused chat %s thread: %w", chat.ID, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(cleanupErrs...)
}

func (h *handlers) applyExplicitPresence(chatID string, inst framework.InstanceInfo, action string) bool {
	h.presenceMu.Lock()
	defer h.presenceMu.Unlock()
	if h.presenceStates == nil {
		h.presenceStates = make(map[string]*chatPresenceState)
	}
	state := h.presenceStates[chatID]
	if state == nil {
		state = &chatPresenceState{inst: inst}
		h.presenceStates[chatID] = state
	}
	state.inst = inst
	state.generation++
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	if action == "connected" {
		changed := !state.connected
		state.connected = true
		return changed
	}
	changed := state.connected
	state.connected = false
	return changed
}

func (h *handlers) forwardPresence(inst framework.InstanceInfo, chatID, action string) error {
	if !perThreadEnabled() {
		return h.forwardPresenceToExistingThread(inst, "main", true, action)
	}
	if h.store == nil {
		return nil
	}
	chat, err := h.store.GetChat(chatID)
	if err != nil {
		return err
	}
	threadID, exists := h.existingConversationThread(chat)
	return h.forwardPresenceToExistingThread(inst, threadID, exists, action)
}

// Passive dashboard activity must never create a Core conversation thread.
// Presence is useful only after the user has already started the conversation;
// the first user message remains the sole path that calls ensureChatThread.
func (h *handlers) existingConversationThread(chat *Chat) (string, bool) {
	if !perThreadEnabled() {
		return "main", true
	}
	if chat == nil {
		return "", false
	}
	threadID := strings.TrimSpace(chat.ThreadID)
	return threadID, threadID != ""
}

func (h *handlers) forwardPresenceToExistingThread(inst framework.InstanceInfo, threadID string, exists bool, action string) error {
	if !exists {
		return nil
	}
	evText := "[chat] user connected to chat"
	if action == "disconnected" {
		evText = "[chat] user disconnected from chat"
	}
	return h.instances.ForwardEvent(inst, evText, threadID)
}

// --- Helpers ----------------------------------------------------------

// authorizeChat pulls chat_id from the query, verifies the chat
// belongs to an instance the caller owns, and returns the pair.
// Writes an HTTP error + returns ok=false on failure.
func (h *handlers) authorizeChat(w http.ResponseWriter, r *http.Request) (string, framework.InstanceInfo, bool) {
	chat, inst, ok := h.authorizeConversation(w, r)
	if !ok {
		return "", framework.InstanceInfo{}, false
	}
	return chat.ID, inst, true
}

func (h *handlers) authorizeConversation(w http.ResponseWriter, r *http.Request) (*Chat, framework.InstanceInfo, bool) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		chatID = r.URL.Query().Get("id")
	}
	// default-* identifies the private main-thread inbox/status sink. It is
	// intentionally not addressable as a dashboard conversation.
	if strings.HasPrefix(chatID, "default-") {
		http.Error(w, "chat not found", http.StatusNotFound)
		return nil, framework.InstanceInfo{}, false
	}
	if chatID == "" {
		http.Error(w, "chat_id required", http.StatusBadRequest)
		return nil, framework.InstanceInfo{}, false
	}
	chat, err := h.store.GetChat(chatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "chat not found", http.StatusNotFound)
			return nil, framework.InstanceInfo{}, false
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, framework.InstanceInfo{}, false
	}
	userID := h.instances.LookupUserID(r)
	if chat.OwnerUserID != 0 && chat.OwnerUserID != userID {
		http.Error(w, "chat not found", http.StatusNotFound)
		return nil, framework.InstanceInfo{}, false
	}
	inst, err := h.instances.OwnedInstance(userID, chat.AgentID)
	if err != nil {
		http.Error(w, "chat not found", http.StatusNotFound)
		return nil, framework.InstanceInfo{}, false
	}
	return chat, inst, true
}

func formatDashboardContext(v any) string {
	ctx, ok := v.(map[string]any)
	if !ok || len(ctx) == 0 {
		return ""
	}
	source, _ := ctx["source"].(string)
	if source != "dashboard-floating" && source != "dashboard-build" {
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
	if projectID := pick("project_id"); projectID != "" {
		lines = append(lines, "Project scope rule: This conversation is authoritatively scoped to project_id "+projectID+". Pass this exact project_id to every project-aware read or mutation tool. Do not use another project or global scope unless the operator explicitly asks to change scope.")
	}
	return strings.Join(lines, "\n")
}

func formatAgentChatEvent(text string, context any) string {
	var b strings.Builder
	b.WriteString("[chat]\n")
	b.WriteString("This is a new dashboard-chat user turn. Follow the user-chat role in your directive: handle interactive work here, use temporary children for substantial one-off work when useful, give each child an explicit result-to-parent completion contract, and keep the conversation responsive. ")
	b.WriteString("Visible communication may contain one optional acknowledgement, selective progress at major phase completions or achievements, and exactly one complete final outcome. Say what was achieved and the meaningful next step; do not narrate tools, searches, routine retries, temporary plans, or unchanged waiting. ")
	b.WriteString("Use REPORT ONLY selectively for wider-system milestones that need no action; continue without waiting. Use ACTION REQUIRED only for durable or cross-thread work that main must own, request a reply to this originating conversation, and wait for that result before confirming completion. ")
	b.WriteString("Thoughts and plain assistant output are not visible to the user. Never repeat a message after a successful channels_send receipt.\n\n")
	if ctx := formatDashboardContext(context); ctx != "" {
		b.WriteString(ctx)
		b.WriteString("\n\n")
	}
	b.WriteString("User message:\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\nDASHBOARD CHAT COMPLETION REQUIREMENT: Direct answers use one complete channels_send. For tool work, follow this order: optional channels_send acknowledgement; action or read tool; observe its result in a later turn; then one complete final channels_send. A message sent in the same batch as an action or read tool occurs before that tool's result and is only an acknowledgement, never the final outcome. Tool-work turns may add a small number of meaningful progress messages, but exactly one final outcome is still required after the last tool, child result, or action-required reply. REPORT ONLY messages do not satisfy the visible reply requirement. Never call pace or go idle on a direct chat turn until the final delivery receipt succeeds, and never repeat it.")
	return b.String()
}

func formatPlatformHelperChatEvent(text string, context any) string {
	return formatAgentChatEvent(text, context) + "\n\nPLATFORM HELPER TURN REQUIREMENT: " +
		"When an attached Apteva tool can finish this request now, use it directly in this conversation. " +
		"A persistent agents_update or other atomic control-plane mutation does not require a main handoff. " +
		"The shared acknowledgement and selective-progress guidance applies here too. After the tool's " +
		"successful receipt, send exactly one final channels_send response and then pace; never " +
		"send(id=\"main\") for the same work and never repeat the final response after its delivery receipt."
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

// ensureChatThread resolves and idempotently ensures the full core thread
// specification for an internal conversation. The spawn cache is only a
// directive-drift optimization; it is never proof that the core child still
// has the thread after a restart. Consequently SpawnThread is called on every
// delivery. Internal conversations never fall back to main.
func (h *handlers) ensureChatThread(inst framework.InstanceInfo, chatID string) (string, error) {
	if !perThreadEnabled() {
		return "main", nil
	}
	if h.store == nil {
		return "", fmt.Errorf("conversation store unavailable")
	}
	threadID, err := h.store.EnsureChatThread(chatID)
	if err != nil {
		return "", fmt.Errorf("ensure conversation thread id for %s: %w", chatID, err)
	}
	if threadID == "" {
		return "", fmt.Errorf("ensure conversation thread id for %s returned empty id", chatID)
	}
	cacheKey := fmt.Sprintf("%d/%s", inst.ID, chatID)
	profile := chatThreadProfileFor(inst)
	threadDirective := profile.DirectiveSuffix

	// Compute the directive hash to compare against what the chat
	// thread was last spawned/updated with. The hash covers main's live
	// directive, the server role suffix, and the requested Core tool profile.
	wantHash := ""
	if dir, derr := h.instances.MainDirective(inst); derr == nil {
		wantHash = directiveHash(dir + threadDirective + "\x00" + strings.Join(profile.Tools, ","))
	} else {
		log.Printf("[CHAT] MainDirective inst=%d: %v — skipping drift check", inst.ID, derr)
	}

	prev, alreadySpawned := spawnedChatThreads.Load(cacheKey)

	// We're going to either spawn (first time) or update (drifted).
	// Both want main's MCP surface. "channels" is always required —
	// the chat thread needs channels_send to reply at all; the
	// rest mirrors the instance's effective MCP list so quick
	// reads/lookups can be served without round-tripping through main.
	mcps, err := h.instances.ListMCPNames(inst)
	if err != nil || len(mcps) == 0 {
		log.Printf("[CHAT] ListMCPNames inst=%d: %v — using minimal fallback", inst.ID, err)
		mcps = fallbackChatThreadMCPs
	} else {
		mcps = ensureChannels(mcps)
	}

	// POST is intentionally unconditional and idempotent. Besides creating a
	// missing thread, the core persistence contract backfills an older live
	// thread whose Config.Threads record is absent.
	if err := h.instances.SpawnThread(inst, threadID, threadDirective, profile.Tools, mcps); err != nil {
		return "", fmt.Errorf("ensure core conversation thread %s: %w", threadID, err)
	}

	// A server restart loses the local hash while core may restore a persisted
	// thread created under an older directive or tool profile. Read its live
	// effective tools and add missing profile tools while preserving every
	// scoped MCP tool Core already resolved.
	drifted := !alreadySpawned
	if alreadySpawned && wantHash != "" {
		drifted = prev.(string) != wantHash
	}
	var mergedTools []string
	profileVerified := true
	if drifted {
		if currentTools, toolsErr := h.instances.ThreadTools(inst, threadID); toolsErr != nil {
			log.Printf("[CHAT] ThreadTools inst=%d thread=%s: %v — preserving current tools", inst.ID, threadID, toolsErr)
			profileVerified = false
		} else if missingAny(currentTools, profile.Tools) {
			mergedTools = mergeUnique(currentTools, profile.Tools)
		}
	}
	if drifted {
		if err := h.instances.UpdateThread(inst, threadID, threadDirective, mergedTools); err != nil {
			return "", fmt.Errorf("update core conversation thread %s: %w", threadID, err)
		}
	}
	if wantHash != "" && profileVerified {
		spawnedChatThreads.Store(cacheKey, wantHash)
	} else {
		spawnedChatThreads.Delete(cacheKey)
	}
	return threadID, nil
}

// forwardConversationEvent ensures the conversation thread and forwards one
// event. A typed missing-thread response is recovered once; all other errors
// are returned to the durable delivery queue without redirecting to main.
func (h *handlers) forwardConversationEvent(inst framework.InstanceInfo, chatID string, message any) (string, error) {
	threadID, err := h.ensureChatThread(inst, chatID)
	if err != nil {
		return "", err
	}
	if err := h.instances.ForwardEvent(inst, message, threadID); err != nil {
		if !isMissingCoreThread(err) || threadID == "main" {
			return threadID, err
		}
		spawnedChatThreads.Delete(fmt.Sprintf("%d/%s", inst.ID, chatID))
		threadID, ensureErr := h.ensureChatThread(inst, chatID)
		if ensureErr != nil {
			return "", ensureErr
		}
		if retryErr := h.instances.ForwardEvent(inst, message, threadID); retryErr != nil {
			return threadID, retryErr
		}
	}
	return threadID, nil
}

type missingCoreThreadError interface {
	ThreadMissing() bool
}

func isMissingCoreThread(err error) bool {
	var target missingCoreThreadError
	return errors.As(err, &target) && target.ThreadMissing()
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

func missingAny(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, item := range have {
		set[strings.TrimSpace(item)] = true
	}
	for _, item := range want {
		if !set[strings.TrimSpace(item)] {
			return true
		}
	}
	return false
}

func mergeUnique(existing, additions []string) []string {
	out := make([]string, 0, len(existing)+len(additions))
	seen := make(map[string]bool, cap(out))
	for _, list := range [][]string{existing, additions} {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
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
