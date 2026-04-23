// Package tasks implements the Tasks app — a lightweight mission board
// each instance can use to track work. Agents create, update, and
// complete tasks via MCP tools; the dashboard reads the same list via
// REST + SSE. Events emitted here are independent of the core
// telemetry stream — the UI subscribes to both.
package tasks

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/apteva/server/apps/framework"
)

//go:embed migrations/001_init.sql
var migration001 string

// New constructs the app. The InstanceResolver is identical in shape
// to the one channelchat takes, so apteva-server's single
// serverResolver can satisfy both.
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
		Slug:        "tasks",
		Name:        "Tasks",
		Version:     "1.0.0",
		Description: "Mission board for an instance. Agents create and complete tasks via MCP tools; dashboard tracks progress live.",
		UISlots: []framework.UISlot{
			{Slot: "instance.tasks", Title: "Tasks"},
		},
		Publishes:  []string{"task.created", "task.updated", "task.deleted"},
		Subscribes: nil,
	}
}

func (a *App) Migrations() []framework.Migration {
	return []framework.Migration{
		{Version: 1, Name: "create tasks table", SQL: migration001},
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
		// /tasks   — list (GET) + create (POST)
		{Method: "", Path: "/tasks", Handler: a.handlers.tasksCollection},
		// /task    — single-item get / update / delete (?id=N)
		{Method: "", Path: "/task", Handler: a.handlers.taskItem},
		// /stream  — SSE
		{Method: "GET", Path: "/stream", Handler: a.handlers.stream},
	}
}

func (a *App) Channels() []framework.ChannelFactory { return nil }
func (a *App) Workers() []framework.Worker          { return nil }
func (a *App) EventHandlers() []framework.EventHandler {
	return nil
}

// MCPTools exposes task CRUD to the agent. All tools implicitly scope
// to the calling instance via the InstanceInfo passed to the handler —
// the agent never needs to know or pass its own instance id.
//
// A generic `thread` arg on create/update lets the caller identify
// which sub-thread spawned or is working on the task. The MCP harness
// doesn't surface the calling thread id automatically, so the agent
// passes it explicitly when it matters.
func (a *App) MCPTools() []framework.MCPTool {
	return []framework.MCPTool{
		{
			Name:        "tasks_list",
			Description: "List tasks for this instance. Filter by status ('open', 'in_progress', 'blocked', 'done', 'cancelled' — comma-separated ok) or by assigned thread.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "description": "Optional comma-separated status filter."},
					"thread": map[string]any{"type": "string", "description": "Optional thread id to filter to."},
					"limit":  map[string]any{"type": "integer", "description": "Max rows (default 100)."},
				},
			},
			Handler: func(args map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				p := ListParams{InstanceID: inst.ID}
				if v, ok := args["status"].(string); ok {
					p.Status = v
				}
				if v, ok := args["thread"].(string); ok {
					p.AssignedThread = v
				}
				if v, ok := args["limit"].(float64); ok {
					p.Limit = int(v)
				}
				out, err := a.store.List(p)
				if err != nil {
					return "", err
				}
				b, _ := json.Marshal(out)
				return string(b), nil
			},
		},
		{
			Name:        "tasks_create",
			Description: "Create a task on this instance's mission board. Use for work you want to track or assign to a sub-thread.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"title"},
				"properties": map[string]any{
					"title":           map[string]any{"type": "string"},
					"description":     map[string]any{"type": "string"},
					"assigned_thread": map[string]any{"type": "string", "description": "Thread id to assign (optional)."},
					"parent_task_id":  map[string]any{"type": "integer", "description": "Parent task id if this is a sub-task."},
					"thread":          map[string]any{"type": "string", "description": "Calling thread id (for provenance)."},
					"reward_xp":       map[string]any{"type": "integer"},
				},
			},
			Handler: func(args map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				title, _ := args["title"].(string)
				if title == "" {
					return "", fmt.Errorf("title required")
				}
				p := CreateParams{
					InstanceID: inst.ID,
					Title:      title,
				}
				if v, ok := args["description"].(string); ok {
					p.Description = v
				}
				if v, ok := args["assigned_thread"].(string); ok {
					p.AssignedThread = v
				}
				if v, ok := args["parent_task_id"].(float64); ok {
					vv := int64(v)
					p.ParentTaskID = &vv
				}
				if v, ok := args["thread"].(string); ok {
					p.CreatedByThread = v
				}
				if v, ok := args["reward_xp"].(float64); ok {
					p.RewardXP = int(v)
				}
				t, err := a.store.Create(p)
				if err != nil {
					return "", err
				}
				a.hub.publish(HubEvent{Kind: EventCreated, Task: *t})
				b, _ := json.Marshal(t)
				return string(b), nil
			},
		},
		{
			Name:        "tasks_update",
			Description: "Update a task. Use to report progress (progress 0..100), change status, add a status note, or reassign.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id":              map[string]any{"type": "integer"},
					"status":          map[string]any{"type": "string", "enum": []string{"open", "in_progress", "blocked", "done", "cancelled"}},
					"progress":        map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
					"note":            map[string]any{"type": "string"},
					"assigned_thread": map[string]any{"type": "string"},
				},
			},
			Handler: func(args map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				idF, ok := args["id"].(float64)
				if !ok {
					return "", fmt.Errorf("id required")
				}
				id := int64(idF)
				// Ownership check: the task must belong to the calling
				// instance so one instance can't tamper with another's.
				existing, err := a.store.Get(id)
				if err != nil {
					return "", err
				}
				if existing.InstanceID != inst.ID {
					return "", fmt.Errorf("task does not belong to this instance")
				}
				var p UpdateParams
				if v, ok := args["status"].(string); ok {
					p.Status = &v
				}
				if v, ok := args["progress"].(float64); ok {
					i := int(v)
					p.Progress = &i
				}
				if v, ok := args["note"].(string); ok {
					p.Note = &v
				}
				if v, ok := args["assigned_thread"].(string); ok {
					p.AssignedThread = &v
				}
				updated, err := a.store.Update(id, p)
				if err != nil {
					return "", err
				}
				a.hub.publish(HubEvent{Kind: EventUpdated, Task: *updated})
				b, _ := json.Marshal(updated)
				return string(b), nil
			},
		},
		{
			Name:        "tasks_complete",
			Description: "Mark a task done. Shortcut for tasks_update(id, status='done', progress=100, note=summary).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id":      map[string]any{"type": "integer"},
					"summary": map[string]any{"type": "string", "description": "Optional completion note."},
				},
			},
			Handler: func(args map[string]any, inst framework.InstanceInfo, _ *framework.AppCtx) (string, error) {
				idF, ok := args["id"].(float64)
				if !ok {
					return "", fmt.Errorf("id required")
				}
				id := int64(idF)
				existing, err := a.store.Get(id)
				if err != nil {
					return "", err
				}
				if existing.InstanceID != inst.ID {
					return "", fmt.Errorf("task does not belong to this instance")
				}
				status := "done"
				progress := 100
				note, _ := args["summary"].(string)
				p := UpdateParams{Status: &status, Progress: &progress}
				if note != "" {
					p.Note = &note
				}
				updated, err := a.store.Update(id, p)
				if err != nil {
					return "", err
				}
				a.hub.publish(HubEvent{Kind: EventUpdated, Task: *updated})
				b, _ := json.Marshal(updated)
				return string(b), nil
			},
		},
	}
}

func (a *App) OnInstanceAttach(_ *framework.AppCtx, _ framework.InstanceInfo) error { return nil }
func (a *App) OnInstanceDetach(_ *framework.AppCtx, _ framework.InstanceInfo) error { return nil }

// Silence an unused-import warning from net/http in future refactors;
// keeping the dependency explicit since we hand handler functions to
// the framework that implement the http signature indirectly.
var _ http.Handler = http.HandlerFunc(nil)
