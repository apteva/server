// Package status implements the Status app — a one-row-per-instance
// status line the agent writes via MCP. Short-horizon counterpart to
// the long-lived directive: "what I'm on right now" vs "what I am for".
package status

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/apteva/server/apps/framework"
)

//go:embed migrations/001_init.sql
var migration001 string

func New(resolver InstanceResolver) framework.App {
	return &App{resolver: resolver}
}

type App struct {
	resolver InstanceResolver
	store    *store
	hub      *hub
	handlers *handlers
}

func (a *App) Manifest() framework.Manifest {
	return framework.Manifest{
		Slug:        "status",
		Name:        "Status",
		Version:     "1.0.0",
		Description: "Per-instance status line. Agent writes it via MCP; dashboard reads live.",
		UISlots: []framework.UISlot{
			{Slot: "instance.status", Title: "Status"},
		},
		Publishes: []string{"status.updated"},
	}
}

func (a *App) Migrations() []framework.Migration {
	return []framework.Migration{
		{Version: 1, Name: "create status table", SQL: migration001},
	}
}

func (a *App) OnMount(ctx *framework.AppCtx) error {
	a.store = newStore(ctx.DB)
	a.hub = newHub()
	a.handlers = &handlers{store: a.store, hub: a.hub, instances: a.resolver}
	return nil
}

func (a *App) OnUnmount(_ *framework.AppCtx) error { return nil }

func (a *App) HTTPRoutes() []framework.Route {
	return []framework.Route{
		{Method: "GET", Path: "/status", Handler: a.handlers.getStatus},
		{Method: "GET", Path: "/stream", Handler: a.handlers.stream},
	}
}

func (a *App) Channels() []framework.ChannelFactory      { return nil }
func (a *App) Workers() []framework.Worker               { return nil }
func (a *App) EventHandlers() []framework.EventHandler   { return nil }

// MCPTools exposes a single tool, `status_set`. The agent calls it at
// human cadence (every few minutes, or at notable transitions) — NOT
// on every iteration. The UI's Status header subscribes to /stream
// and renders whatever the agent wrote.
func (a *App) MCPTools() []framework.MCPTool {
	return []framework.MCPTool{
		{
			Name:        "status_set",
			Description: "Set your visible status line — one sentence describing what you're working on right now. Shown in the dashboard header. Call at notable transitions (starting a new focus, hitting a blocker, finishing something), NOT on every iteration.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"message"},
				"properties": map[string]any{
					"message": map[string]any{"type": "string", "description": "One short sentence. Prefer concrete nouns over meta narration."},
					"emoji":   map[string]any{"type": "string", "description": "Optional leading emoji."},
					"tone":    map[string]any{"type": "string", "enum": []string{"info", "working", "warn", "error", "success", "idle"}, "description": "How the status should be rendered. Default 'info'."},
					"thread":  map[string]any{"type": "string", "description": "Calling thread id (for provenance)."},
				},
			},
			Handler: func(args map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				msg, _ := args["message"].(string)
				if msg == "" {
					return "", fmt.Errorf("message required")
				}
				emoji, _ := args["emoji"].(string)
				tone, _ := args["tone"].(string)
				thread, _ := args["thread"].(string)
				s, err := a.store.Upsert(UpsertParams{
					InstanceID:  inst.ID,
					Message:     msg,
					Emoji:       emoji,
					Tone:        tone,
					SetByThread: thread,
				})
				if err != nil {
					return "", err
				}
				a.hub.publish(*s)
				b, _ := json.Marshal(s)
				return string(b), nil
			},
		},
		{
			Name:        "status_clear",
			Description: "Clear your status line. The dashboard falls back to showing the latest internal thought.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler: func(_ map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				if err := a.store.Clear(inst.ID); err != nil {
					return "", err
				}
				a.hub.publish(Status{InstanceID: inst.ID, Message: "", Tone: "idle"})
				return `{"ok":true}`, nil
			},
		},
	}
}

func (a *App) OnInstanceAttach(_ *framework.AppCtx, _ framework.InstanceInfo) error { return nil }
func (a *App) OnInstanceDetach(_ *framework.AppCtx, _ framework.InstanceInfo) error { return nil }
