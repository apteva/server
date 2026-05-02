// Package framework defines the Apteva Apps contract — a small pluggable
// runtime that lets a feature (chat, helpdesk, lead-scorer, …) declare
// its facets (channels, MCP tools, HTTP routes, DB migrations, workers,
// event handlers, UI slots) in one place and get mounted into the
// apteva-server with uniform lifecycle management.
//
// An App is Go code today (stage 1 compiled-in). The interface is
// shaped to also fit stage-2 sidecar / stage-3 container mounts without
// needing a breaking rename — every facet returned here has a natural
// JSON-RPC equivalent. Apps don't import apteva-server internals
// directly; they interact with the platform through the AppCtx handed
// to them at OnMount time.
package framework

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// App is the single interface every Apteva App implements. Every facet
// is optional — return nil / an empty slice / a no-op to opt out.
// Framework calls methods in this order at server boot:
//
//	Manifest  →  Migrations (run)  →  OnMount  →  HTTPRoutes, Channels,
//	MCPTools, Workers, EventHandlers  (collected for mounting)
//
// Per-instance lifecycle hooks fire as instances come and go at
// runtime (not at boot).
type App interface {
	Manifest() Manifest
	Migrations() []Migration

	// OnMount runs once after migrations complete. Apps use it to
	// build internal state, warm caches, or spin up private
	// goroutines. Returning an error aborts the server boot.
	OnMount(ctx *AppCtx) error

	// OnUnmount runs on graceful server shutdown. Apps should stop
	// workers and flush state. The supervisor also cancels the
	// AppCtx after this returns.
	OnUnmount(ctx *AppCtx) error

	HTTPRoutes() []Route
	Channels() []ChannelFactory
	MCPTools() []MCPTool
	Workers() []Worker
	EventHandlers() []EventHandler

	// OnInstanceAttach runs when an instance comes online. Channels
	// from Channels() are registered automatically; this hook is
	// for any additional per-instance state (default rows, caches).
	OnInstanceAttach(ctx *AppCtx, instance InstanceInfo) error

	// OnInstanceDetach runs BEFORE channels are unregistered, so
	// the app can flush pending writes that depend on the channel.
	OnInstanceDetach(ctx *AppCtx, instance InstanceInfo) error
}

// Manifest is what /api/apps/manifest reports — the shape the
// dashboard consumes to decide what UI slots to render.
type Manifest struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	UISlots     []UISlot `json:"ui_slots,omitempty"`
	Publishes   []string `json:"publishes,omitempty"`
	Subscribes  []string `json:"subscribes,omitempty"`
}

// UISlot declares where in the dashboard this app's UI should render.
// Stage 1 apps bundle their UI directly in the dashboard codebase and
// register against a slot name. Stage 2+ will resolve Entry to a
// lazy-loaded JS module URL.
type UISlot struct {
	Slot  string `json:"slot"`            // e.g. "instance.chat"
	Title string `json:"title,omitempty"` // human label
	Entry string `json:"entry,omitempty"` // stage 2+ module URL
}

// Migration is one SQL script. Apps supply them in order; the runner
// tracks the highest applied version per slug.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Route is one HTTP handler mounted at /api/apps/<slug><Path>.
// Path must start with '/'. Leave empty to mount at the prefix root.
type Route struct {
	Method  string // "GET" | "POST" | ...
	Path    string
	Handler func(w http.ResponseWriter, r *http.Request, ctx *AppCtx)
	NoAuth  bool // default false — require the server's auth middleware
}

// ChannelFactory builds a Channel for a given instance. The framework
// calls each factory on instance attach and registers the built
// channel in the instance's ChannelRegistry under ChannelID().
type ChannelFactory interface {
	ChannelID(instance InstanceInfo) string
	Build(ctx *AppCtx, instance InstanceInfo) (Channel, error)
}

// Channel is the subset of the apteva-server Channel interface. Kept
// identical so apps work against the same contract as slack/email/cli.
type Channel interface {
	ID() string
	Send(text string) error
	Status(text, level string) error
	Close()
}

// ChatComponent is a render hint the agent attaches to a chat message
// via the `respond` tool's optional `components` arg. The chat panel
// resolves (App, Name) to a UIComponent declared by the named app's
// manifest and mounts it under the message bubble with the supplied
// props. Decoupled from tool responses by design — apps declare what
// they can render; the agent decides when to render.
type ChatComponent struct {
	App   string         `json:"app"`             // app slug, e.g. "storage"
	Name  string         `json:"name"`            // component name, e.g. "file-card"
	Props map[string]any `json:"props,omitempty"` // forwarded to the component
}

// RichSender is the optional capability a Channel can implement to
// receive ChatComponents alongside text. Channels that implement it
// (channelchat does) get rich attachments; channels that don't (cli,
// slack, email, telegram) ignore the components and just deliver
// text — graceful degradation keeps the agent's `respond` call
// working everywhere.
type RichSender interface {
	SendWithComponents(text string, components []ChatComponent) error
}

// MCPTool is one tool exposed through the instance's channel MCP.
// Handlers have full access to the app's DB + the calling instance.
type MCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(args map[string]any, instance InstanceInfo, ctx *AppCtx) (string, error)
}

// Worker is a background goroutine the supervisor manages. Supervisor
// starts on mount, restarts on panic (with RestartBackoff), and stops
// on unmount.
type Worker struct {
	Name           string
	Cron           string // empty = run-forever loop
	Run            func(ctx context.Context, app *AppCtx) error
	RestartBackoff time.Duration
}

// EventHandler subscribes to an AppBus topic. Handlers run in their
// own goroutine per delivery — the bus doesn't serialize.
type EventHandler struct {
	Topic   string // e.g. "chat.message"; "*" to receive all
	Handler func(event Event, ctx *AppCtx) error
}

// Event is one payload pushed through the AppBus.
type Event struct {
	Topic     string
	Source    string // slug of the publishing app
	Timestamp time.Time
	Payload   any
}

// InstanceInfo is the read-only view of an apteva instance passed to
// apps. Adding fields is safe; removing is breaking.
type InstanceInfo struct {
	ID         int64
	Name       string
	UserID     int64
	ProjectID  string
	Port       int
	CoreAPIKey string
}

// DB re-exports *sql.DB so apps don't need to import database/sql
// just to type their store.
type DB = sql.DB
