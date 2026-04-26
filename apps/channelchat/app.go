// Package channelchat implements the first Apteva App — a DB-backed
// chat channel. Agent-facing, it plugs into the existing Channel
// interface so channels_respond(channel="chat", ...) just works.
// Dashboard-facing, it exposes a REST+SSE surface keyed on chat_id so
// the UI can fetch history and subscribe to live messages without
// reconstructing state from telemetry events.
package channelchat

import (
	_ "embed"
	"net/http"

	"github.com/apteva/server/apps/framework"
)

//go:embed migrations/001_init.sql
var migration001 string

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
}

func (a *App) Manifest() framework.Manifest {
	return framework.Manifest{
		Slug:        "channel-chat",
		Name:        "Chat",
		Version:     "1.0.0",
		Description: "DB-backed chat channel with per-instance history. Agent replies land as chat messages; dashboard subscribes via SSE.",
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
	}
}

func (a *App) OnMount(ctx *framework.AppCtx) error {
	a.store = newStore(ctx.DB)
	a.hub = newHub()
	a.bus = ctx.Bus
	a.handlers = &handlers{
		store:     a.store,
		hub:       a.hub,
		bus:       ctx.Bus,
		instances: a.resolver,
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
		// /messages handles GET, POST, DELETE internally — framework's
		// per-route Method filter would force three separate entries,
		// so we leave Method empty for this one.
		{Method: "", Path: "/messages", Handler: a.wrap(a.handlers.messages)},
		{Method: "GET", Path: "/stream", Handler: a.wrap(a.handlers.stream)},
		{Method: "GET", Path: "/unread-summary", Handler: a.wrap(a.handlers.unreadSummary)},
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
// existing channels_respond(channel="chat", ...) tool that's already
// mounted by the channelMCPServer. Future tools (chat_read for
// explicit backfill, chat_list for multi-chat) can slot in here.
func (a *App) MCPTools() []framework.MCPTool { return nil }

func (a *App) Workers() []framework.Worker              { return nil }
func (a *App) EventHandlers() []framework.EventHandler  { return nil }

// Per-instance attach: ensure the default chat row exists so the SSE
// stream has something to backfill against on the dashboard's first
// fetch. The framework separately calls each ChannelFactory's Build,
// which also calls EnsureDefaultChat — idempotent INSERT OR IGNORE,
// so safe to do twice.
func (a *App) OnInstanceAttach(_ *framework.AppCtx, inst framework.InstanceInfo) error {
	_, err := a.store.EnsureDefaultChat(inst.ID)
	return err
}

func (a *App) OnInstanceDetach(_ *framework.AppCtx, _ framework.InstanceInfo) error {
	// Chat rows persist across instance restarts — no detach work.
	return nil
}
