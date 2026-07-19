// Package channelchat implements the first Apteva App — a DB-backed
// chat channel. Agent-facing, it plugs into the existing Channel
// interface so channels_send(channel="current", ...) persists replies.
// Dashboard-facing, it exposes a REST+SSE surface keyed on chat_id so
// the UI can fetch history and subscribe to live messages without
// reconstructing state from telemetry events.
package channelchat

import (
	"context"
	_ "embed"
	"net/http"
	"time"

	"github.com/apteva/server/apps/framework"
)

//go:embed migrations/001_init.sql
var migration001 string

//go:embed migrations/002_seen.sql
var migration002 string

//go:embed migrations/003_clamp_seen.sql
var migration003 string

//go:embed migrations/004_components.sql
var migration004 string

//go:embed migrations/005_chat_thread.sql
var migration005 string

//go:embed migrations/006_attachments.sql
var migration006 string

//go:embed migrations/007_conversations.sql
var migration007 string

// New constructs the app, ready to be loaded into a framework.Registry.
// The InstanceResolver lets the HTTP handlers authorize per-chat and
// forward user messages into the instance's core /event endpoint —
// decouples the app from apteva-server internal types.
func New(resolver InstanceResolver) framework.App {
	return &App{resolver: resolver}
}

type App struct {
	resolver  InstanceResolver
	store     *store
	hub       *hub
	handlers  *handlers
	factories []framework.ChannelFactory
	bus       *framework.AppBus
	streamer  *Streamer
}

// Streamer returns the channelchat Streamer that converts LLM
// tool-args telemetry events into ephemeral chat SSE frames. The
// server wires this into its live-telemetry tap so we don't need a
// new IPC path — the existing /telemetry/live POST is the only
// thing the agent emits to. Nil-safe: callers can pass the result
// to any Ingest call site even before OnMount finishes.
func (a *App) Streamer() *Streamer { return a.streamer }

func (a *App) Manifest() framework.Manifest {
	return framework.Manifest{
		Slug:        "channel-chat",
		Name:        "Chat",
		Version:     "1.0.0",
		Description: "DB-backed chat channel with per-instance history. Agent replies land as chat messages; dashboard subscribes via SSE.",
		Internal:    true,
		UISlots: []framework.UISlot{
			{Slot: "instance.chat", Title: "Chat"},
		},
		Publishes:  []string{"chat.message"},
		Subscribes: nil,
	}
}

func (a *App) Migrations() []framework.Migration {
	return []framework.Migration{
		{Version: 1, Name: "create channel_chat tables", SQL: migration001},
		{Version: 2, Name: "add last_seen_id watermark", SQL: migration002},
		{Version: 3, Name: "clamp inflated last_seen_id", SQL: migration003},
		{Version: 4, Name: "add components_json column", SQL: migration004},
		{Version: 5, Name: "add per-chat thread_id column", SQL: migration005},
		{Version: 6, Name: "add user attachments", SQL: migration006},
		{Version: 7, Name: "add project conversations and agent participants", Apply: applyMigration007},
	}
}

func (a *App) OnMount(ctx *framework.AppCtx) error {
	a.store = newStore(ctx.DB)
	// Phase 1 catch-up rename — channel_chat_chats.instance_id →
	// agent_id on DBs that pre-date the rename. Idempotent.
	a.store.renameInstanceIDToAgentID()
	if err := a.store.CleanupOrphanedAgentData(); err != nil {
		return err
	}
	if err := a.store.CleanupLegacyMainConversationData(); err != nil {
		return err
	}
	a.hub = newHub()
	a.bus = ctx.Bus
	a.streamer = newStreamer(a.hub)
	a.handlers = &handlers{
		store:     a.store,
		hub:       a.hub,
		bus:       ctx.Bus,
		instances: a.resolver,
	}
	if removed, err := a.handlers.cleanupOrphanConversationThreads(); err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Warn("conversation thread reconciliation incomplete", "removed", removed, "err", err)
		}
	} else if removed > 0 && ctx.Logger != nil {
		ctx.Logger.Info("removed orphaned conversation threads", "count", removed)
	}
	if removed, err := a.handlers.cleanupUnusedConversationThreads(); err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Warn("unused conversation thread cleanup incomplete", "removed", removed, "err", err)
		}
	} else if removed > 0 && ctx.Logger != nil {
		ctx.Logger.Info("removed unused conversation threads", "count", removed)
	}
	a.factories = []framework.ChannelFactory{
		&chatChannelFactory{store: a.store, hub: a.hub, bus: ctx.Bus},
	}
	return nil
}

func (a *App) OnUnmount(_ *framework.AppCtx) error { return nil }

func (a *App) HTTPRoutes() []framework.Route {
	return []framework.Route{
		{Method: "GET", Path: "/chats", Handler: a.wrap(a.handlers.listChats)},
		{Method: "POST", Path: "/chats", Handler: a.wrap(a.handlers.createChat)},
		{Method: "", Path: "/conversations", Handler: a.wrap(a.handlers.conversations)},
		{Method: "", Path: "/conversation", Handler: a.wrap(a.handlers.conversation)},
		{Method: "", Path: "/participants", Handler: a.wrap(a.handlers.participants)},
		// /messages handles GET, POST, DELETE internally — framework's
		// per-route Method filter would force three separate entries,
		// so we leave Method empty for this one.
		{Method: "", Path: "/messages", Handler: a.wrap(a.handlers.messages)},
		{Method: "GET", Path: "/stream", Handler: a.wrap(a.handlers.stream)},
		{Method: "GET", Path: "/unread-summary", Handler: a.wrap(a.handlers.unreadSummary)},
		{Method: "GET", Path: "/approval-messages", Handler: a.wrap(a.handlers.approvalMessages)},
		{Method: "GET", Path: "/report-messages", Handler: a.wrap(a.handlers.reportMessages)},
		{Method: "GET", Path: "/alert-messages", Handler: a.wrap(a.handlers.alertMessages)},
		{Method: "GET", Path: "/current-statuses", Handler: a.wrap(a.handlers.currentStatuses)},
		{Method: "POST", Path: "/message-action", Handler: a.wrap(a.handlers.messageAction)},
		{Method: "POST", Path: "/message-dismiss", Handler: a.wrap(a.handlers.messageDismiss)},
		{Method: "POST", Path: "/seen", Handler: a.wrap(a.handlers.markSeen)},
		// Presence events ("[chat] user connected/disconnected") used
		// to go from the dashboard straight to core /event with
		// thread_id="main". Routing them through channelchat instead
		// keeps the per-chat thread resolution in one place — both
		// presence and messages land on the same chat thread.
		{Method: "POST", Path: "/presence", Handler: a.wrap(a.handlers.presence)},
	}
}

// wrap adapts an http.HandlerFunc-shaped method to the framework's
// Route.Handler signature without every route needing to know the
// AppCtx it already has via the closed-over handler struct.
func (a *App) wrap(fn func(http.ResponseWriter, *http.Request, *framework.AppCtx)) func(http.ResponseWriter, *http.Request, *framework.AppCtx) {
	return fn
}

func (a *App) Channels() []framework.ChannelFactory { return a.factories }

// MCPTools: v1 doesn't need any. The agent reaches chat through the
// existing channels_send(channel="current", ...) tool mounted by the
// channelMCPServer. The server retains respond only as a cached-schema legacy
// alias. Future tools (chat_read for
// explicit backfill, chat_list for multi-chat) can slot in here.
func (a *App) MCPTools() []framework.MCPTool { return nil }

func (a *App) Workers() []framework.Worker {
	return []framework.Worker{{
		Name: "conversation-delivery-retry",
		Run: func(ctx context.Context, appCtx *framework.AppCtx) error {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					if a.handlers != nil {
						if err := a.handlers.retryPendingDeliveries(); err != nil && appCtx.Logger != nil {
							appCtx.Logger.Warn("conversation delivery retry failed", "err", err)
						}
					}
				}
			}
		},
	}}
}
func (a *App) EventHandlers() []framework.EventHandler { return nil }

// Per-instance attach: ensure the internal operator-inbox row exists for
// reports, alerts, approvals, and status. It is never listed as a dashboard
// conversation. ChannelFactory.Build also ensures it idempotently.
func (a *App) OnInstanceAttach(_ *framework.AppCtx, inst framework.InstanceInfo) error {
	_, err := a.store.EnsureDefaultChat(inst.ID)
	return err
}

func (a *App) OnInstanceDetach(_ *framework.AppCtx, inst framework.InstanceInfo) error {
	// The server currently emits detach only for permanent deletion. Normal
	// stops and restarts do not pass through this hook, so their history stays.
	return a.store.DeleteAgentData(inst.ID)
}
