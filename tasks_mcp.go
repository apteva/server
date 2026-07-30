package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	taskMainMCPName         = "tasks"
	taskConversationMCPName = "tasks-conversation"
	taskWorkerMCPName       = "tasks-worker"
)

type taskMCPProfile string

const (
	taskMCPProfileMain         taskMCPProfile = "main"
	taskMCPProfileConversation taskMCPProfile = "conversation"
	taskMCPProfileWorker       taskMCPProfile = "worker"
)

type taskCompletionDelivery func(context.Context, *AgentTask) error
type taskHandoffDelivery func(context.Context, *AgentTask) error
type taskStepExecutor func(context.Context, string, string, map[string]any) (json.RawMessage, error)
type taskWorkerSpawner func(context.Context, *AgentTask, string, string) error

type taskMCPServer struct {
	store       *Store
	agent       *Agent
	profile     taskMCPProfile
	deliver     taskCompletionDelivery
	handoff     taskHandoffDelivery
	executeStep taskStepExecutor
	spawnWorker taskWorkerSpawner
}

func taskTool(name, description, wake string, schema map[string]any) map[string]any {
	tool := map[string]any{
		"name": name, "description": description, "inputSchema": schema,
	}
	if wake != "" {
		tool["_meta"] = map[string]any{"io.apteva/wakeOnResult": wake}
	}
	return tool
}

func taskObjectSchema(required []string, properties map[string]any) map[string]any {
	return map[string]any{
		"type": "object", "required": required, "properties": properties,
		"additionalProperties": false,
	}
}

func taskCreateProperties(profile taskMCPProfile) map[string]any {
	properties := map[string]any{
		"title": map[string]any{
			"type":        "string",
			"description": "Short outcome-oriented task title. Create a task only for durable, multi-step, delegated, or leave-and-return work; never for a brief answer or a single quick tool call.",
		},
		"description": map[string]any{
			"type":        "string",
			"description": "Concrete scope, success condition, and relevant constraints. Do not copy the entire conversation.",
		},
		"idempotency_key": map[string]any{
			"type":        "string",
			"description": "Stable key for this logical work item. Reusing it returns the existing task instead of duplicating work.",
		},
	}
	if profile == taskMCPProfileConversation {
		properties["assign_to"] = map[string]any{
			"type": "string", "enum": []string{"self", "main"}, "default": "self",
			"description": "Use self when this conversation will do the work. Use main only when work must continue autonomously after the user leaves or belongs to the agent's durable directive.",
		}
		if taskSchedulingEnabled() {
			properties["schedule"] = agentTaskScheduleSchema()
		}
	}
	if profile == taskMCPProfileMain {
		if taskSchedulingEnabled() {
			properties["schedule"] = agentTaskScheduleSchema()
		}
	}
	return properties
}

func agentTaskScheduleSchema() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "Optional durable execution schedule, only when the user explicitly requested a future time, delay, deadline, interval, or recurrence. Never invent a schedule to estimate how long immediate, slow, multi-step, or leave-and-return work may take. Exact cadence belongs here, never duplicated in the directive or pace. Use once with exactly one of at or after, interval+every, or cron+cron with an explicit timezone.",
		"required":    []string{"kind"},
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string", "enum": []string{taskScheduleOnce, taskScheduleInterval, taskScheduleCron},
			},
			"at": map[string]any{
				"type": "string", "description": "RFC3339 future timestamp for kind=once.",
			},
			"after": map[string]any{
				"type": "string", "description": "Server-relative duration such as 10m for kind=once. Prefer this over calculating an absolute timestamp for requests like 'in ten minutes'.",
			},
			"every": map[string]any{
				"type": "string", "description": "Go duration such as 1h or 24h for kind=interval.",
			},
			"cron": map[string]any{
				"type": "string", "description": "Standard five-field cron expression for kind=cron.",
			},
			"timezone": map[string]any{
				"type": "string", "description": "IANA timezone such as UTC or Europe/Madrid. Defaults to UTC.",
			},
			"overlap_policy": map[string]any{
				"type": "string", "enum": []string{"skip"}, "default": "skip",
			},
			"catchup_policy": map[string]any{
				"type": "string", "enum": []string{"skip"}, "default": "skip",
			},
		},
		"additionalProperties": false,
	}
}

func taskUpdateProperties(profile taskMCPProfile) map[string]any {
	properties := map[string]any{
		"task_id": map[string]any{"type": "string"},
		"state": map[string]any{
			"type":        "string",
			"enum":        []string{taskStateQueued, taskStateRunning, taskStateWaiting, taskStateBlocked, taskStateFailed},
			"description": "Current durable state. Use task_complete, not task_update, for successful completion.",
		},
		"progress": map[string]any{
			"type": "integer", "minimum": 0, "maximum": 100,
			"description": "Optional coarse progress. Update only at meaningful milestones; do not emit token-by-token or tool-by-tool churn.",
		},
		"clear_progress": map[string]any{
			"type": "boolean", "description": "Remove a previously recorded progress percentage.",
		},
		"current_step": map[string]any{
			"type": "string", "description": "Concise current milestone, wait condition, blocker, or failure context.",
		},
		"error": map[string]any{
			"type": "string", "description": "Failure detail when state is failed.",
		},
	}
	if profile == taskMCPProfileMain && taskSchedulingEnabled() {
		properties["schedule"] = agentTaskScheduleSchema()
	}
	return properties
}

func (s *taskMCPServer) tools() []map[string]any {
	get := taskTool("task_get",
		"Get the authoritative durable task record. Read its progress and current step before resuming so already successful work is not repeated. Do not infer task state from chat history.",
		"", taskObjectSchema([]string{"task_id"}, map[string]any{
			"task_id": map[string]any{"type": "string"},
		}))
	update := taskTool("task_update",
		"Record a meaningful task milestone, wait, blocker, or failure. Main may also atomically replace the structured cadence of a schedule parent. When schedule is present, it is a schedule-only update; other optional fields are ignored because some providers populate them with defaults. This is the canonical task record; do not mirror percentages or exact schedule timing into global status or the directive.",
		"always", taskObjectSchema([]string{"task_id"}, taskUpdateProperties(s.profile)))
	complete := taskTool("task_complete",
		"Complete the task with its concrete result. A successful receipt ends this assignment: do not call more domain tools, repeat successful stages, or send a second terminal report afterward. If the task originated in a user conversation, the server automatically routes a structured completion event back to that conversation; do not separately send a duplicate completion to it.",
		"on_error", taskObjectSchema([]string{"task_id", "result"}, map[string]any{
			"task_id": map[string]any{"type": "string"},
			"result": map[string]any{
				"type": "string", "description": "Concrete outcome and any evidence or next relevant fact.",
			},
		}))
	runStep := taskTool("task_run_step",
		"Execute one task-backed domain operation through the server's idempotency boundary. Main, a conversation, or a worker that owns a task MUST use this instead of calling raw domain tools directly. A repeated task_id + step_key returns the stored receipt without running the downstream tool again. Use one stable outcome-oriented step_key per logical operation and ordered step_index/step_count so the server records increasing progress automatically.",
		"always", taskObjectSchema(
			[]string{"task_id", "step_key", "step_index", "step_count", "mcp_server", "tool", "arguments"},
			map[string]any{
				"task_id": map[string]any{"type": "string"},
				"step_key": map[string]any{
					"type":        "string",
					"description": "Stable logical operation key such as inventory, verify, or publish-final. Reuse the same key when resuming; never generate a new key to retry the same side effect.",
				},
				"step_index": map[string]any{
					"type": "integer", "minimum": 1,
					"description": "One-based position of this operation in the bounded task plan.",
				},
				"step_count": map[string]any{
					"type": "integer", "minimum": 1,
					"description": "Total operations in the bounded task plan.",
				},
				"mcp_server": map[string]any{
					"type": "string", "description": "Exact name of an MCP server attached to the owning agent.",
				},
				"tool": map[string]any{
					"type": "string", "description": "Exact downstream tool name.",
				},
				"arguments": map[string]any{
					"type": "object", "description": "Arguments for the downstream tool.",
				},
				"allow_repeated_input": map[string]any{
					"type":        "boolean",
					"description": "Defaults false. Set true only when this task intentionally needs the exact same tool and arguments executed more than once as distinct side effects.",
				},
			},
		))
	cancelDescription := "Cancel durable work that should no longer continue. The server notifies the assigned thread to stop and returns one authoritative cancellation outcome to the originating conversation."
	if s.profile == taskMCPProfileConversation {
		cancelDescription += " When this conversation's user asks to cancel its originating task, call this tool directly here even if main or a worker is assigned; do not hand the cancellation to main."
	}
	cancel := taskTool("task_cancel",
		cancelDescription,
		"on_error", taskObjectSchema([]string{"task_id"}, map[string]any{
			"task_id": map[string]any{"type": "string"},
			"reason": map[string]any{
				"type": "string", "description": "Concise user or operator reason for cancellation.",
			},
		}))

	switch s.profile {
	case taskMCPProfileConversation:
		return []map[string]any{
			taskTool("task_create",
				"Create a durable task when the user's request is multi-step, delegated, explicitly scheduled, or must continue after this chat. User wording such as durable, track this work, multiple steps, or I may leave/close is an explicit requirement to call task_create before substantive work. Brief answers and single quick tool calls stay in the conversation without a task. For immediate self-contained multi-step work, use assign_to=self with no schedule, then update and complete that same task in this conversation. The server binds origin and creator automatically. Set schedule only when the user explicitly supplied a future time, delay, deadline, interval, or recurrence; never invent a schedule for immediate, slow, multi-step, or leave-and-return work. For explicitly scheduled work, create this one task with assign_to=main and schedule; the server stores the timer without waking main until an occurrence is due. For immediate durable work assigned to main, this same call wakes main with the task id. Never create a separate setup task or call send(main).",
				"always", taskObjectSchema([]string{"title"}, taskCreateProperties(s.profile))),
			get, update, runStep, complete, cancel,
		}
	case taskMCPProfileWorker:
		return []map[string]any{get, update, runStep, complete}
	default:
		tools := []map[string]any{
			taskTool("task_create",
				"Create durable tracked work. Set schedule only when the operator explicitly supplied a future time, delay, deadline, interval, or recurrence; never invent a schedule for immediate, slow, multi-step, or leave-and-return work. With schedule, this creates a durable schedule parent: the server creates and wakes one normal child task at each due occurrence, independently of pace. Exact cadence belongs only in the task schedule. Evolve the directive separately only when the request changes the agent's broader continuing responsibility, and never copy exact timing into it.",
				"always", taskObjectSchema([]string{"title"}, taskCreateProperties(s.profile))),
			taskTool("task_list",
				"List this agent's durable tasks. Filter by state or assigned thread when checking active work.",
				"", taskObjectSchema(nil, map[string]any{
					"state": map[string]any{
						"type": "string", "enum": sortedTaskStates(),
					},
					"assigned_thread_id": map[string]any{"type": "string"},
					"limit":              map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
				})),
			get,
			taskTool("task_assign",
				"Return an existing task to main. To delegate to a worker, use task_spawn_worker so the server atomically assigns and creates a restricted worker without raw domain MCP access.",
				"always", taskObjectSchema([]string{"task_id", "assigned_thread_id"}, map[string]any{
					"task_id": map[string]any{"type": "string"},
					"assigned_thread_id": map[string]any{
						"type": "string", "enum": []string{"main"},
					},
				})),
			taskTool("task_spawn_worker",
				"Atomically assign a task to a dedicated server-scoped worker and create that worker with only Channels. Use this for every worker delegation; never combine task_assign with Core spawn. The worker performs domain operations through task_run_step, so repeated model actions cannot bypass server idempotency.",
				"always", taskObjectSchema([]string{"task_id", "worker_id", "instructions"}, map[string]any{
					"task_id": map[string]any{"type": "string"},
					"worker_id": map[string]any{
						"type":        "string",
						"description": "Stable non-main, non-conversation worker id for this task.",
					},
					"instructions": map[string]any{
						"type":        "string",
						"description": "Concrete success condition plus ordered steps. For every domain step include the exact mcp_server, tool, arguments, step_index, and step_count that task_run_step must receive.",
					},
				})),
			runStep, update, complete, cancel,
		}
		if taskSchedulingEnabled() {
			tools = append(tools,
				taskTool("task_pause",
					"Pause a scheduled task. Its current occurrence, if any, is not cancelled; no future occurrence is created until resume.",
					"always", taskObjectSchema([]string{"task_id"}, map[string]any{
						"task_id": map[string]any{"type": "string"},
					})),
				taskTool("task_resume",
					"Resume a scheduled task from its next future occurrence. Missed occurrences are not replayed.",
					"always", taskObjectSchema([]string{"task_id"}, map[string]any{
						"task_id": map[string]any{"type": "string"},
					})),
				taskTool("task_run_now",
					"Create one immediate execution of a scheduled task without changing its next normal occurrence. The server durably assigns and wakes main for the new run. Reuse the same idempotency_key if this logical request is retried so it cannot create a second run.",
					"always", taskObjectSchema([]string{"task_id", "idempotency_key"}, map[string]any{
						"task_id": map[string]any{"type": "string"},
						"idempotency_key": map[string]any{
							"type": "string", "description": "Stable key for this one manual run request.",
						},
					})),
			)
		}
		return tools
	}
}

func taskTextResult(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return taskTextError(err.Error())
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(encoded)}},
	}
}

func taskTextError(message string) any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": message}},
		"isError": true,
	}
}

func taskCallerConversationID(caller string) string {
	caller = strings.TrimSpace(caller)
	if strings.HasPrefix(caller, "chat-conv-") {
		return strings.TrimPrefix(caller, "chat-")
	}
	return ""
}

func (s *taskMCPServer) validateCaller(caller string) error {
	caller = strings.TrimSpace(caller)
	if caller == "" {
		return fmt.Errorf("trusted caller context is required")
	}
	switch s.profile {
	case taskMCPProfileMain:
		if caller != "main" {
			return fmt.Errorf("main task administration is available only from the agent's main thread")
		}
	case taskMCPProfileConversation:
		if taskCallerConversationID(caller) == "" {
			return fmt.Errorf("conversation task tools are available only from an originating user conversation")
		}
	case taskMCPProfileWorker:
		if caller == "main" || strings.HasPrefix(caller, "chat-") {
			return fmt.Errorf("worker task tools are available only from the assigned worker thread")
		}
	default:
		return fmt.Errorf("unknown task MCP profile")
	}
	return nil
}

func taskString(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return strings.TrimSpace(value)
}

func taskInt(arguments map[string]any, key string) (*int, bool) {
	value, ok := arguments[key]
	if !ok {
		return nil, false
	}
	switch typed := value.(type) {
	case float64:
		integer := int(typed)
		if typed != float64(integer) {
			return nil, false
		}
		return &integer, true
	case int:
		return &typed, true
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return nil, false
		}
		value := int(integer)
		return &value, true
	default:
		return nil, false
	}
}

func taskBool(arguments map[string]any, key string) bool {
	value, _ := arguments[key].(bool)
	return value
}

func taskScheduleArgument(arguments map[string]any) (*AgentTaskScheduleInput, error) {
	raw, ok := arguments["schedule"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}
	var schedule AgentTaskScheduleInput
	if err := json.Unmarshal(encoded, &schedule); err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}
	// Some providers materialize every optional object and enum default even
	// when the model did not select it. For schedule this commonly arrives as
	// {kind:"once", at:"", after:"", ...}. It carries no executable timing
	// intent and must behave exactly like an omitted optional field; otherwise
	// immediate conversation work is falsely rejected as a malformed timer.
	// Keep incomplete interval/cron objects intact so validation still reports
	// their missing expression.
	if (strings.TrimSpace(schedule.Kind) == "" || strings.TrimSpace(schedule.Kind) == taskScheduleOnce) &&
		strings.TrimSpace(schedule.At) == "" &&
		strings.TrimSpace(schedule.After) == "" &&
		strings.TrimSpace(schedule.Every) == "" &&
		strings.TrimSpace(schedule.Cron) == "" {
		return nil, nil
	}
	return &schedule, nil
}

func validTaskStepKey(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 96 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '.', char == '-', char == '_', char == ':':
		default:
			return false
		}
	}
	return true
}

func validTaskWorkerID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "main" || strings.HasPrefix(value, "chat-") {
		return false
	}
	return validTaskStepKey(value)
}

func cloneTaskArguments(arguments map[string]any) map[string]any {
	encoded, _ := json.Marshal(arguments)
	var cloned map[string]any
	_ = json.Unmarshal(encoded, &cloned)
	if cloned == nil {
		cloned = map[string]any{}
	}
	return cloned
}

func taskMCPResultIsError(result json.RawMessage) bool {
	var payload struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(result, &payload) == nil && payload.IsError
}

func taskStepResponse(step *AgentTaskStep, cached bool) map[string]any {
	response := map[string]any{
		"step": step, "cached": cached,
		"guidance": "The downstream operation has a durable receipt. Continue only with the next uncompleted logical step; if this was the final step, call task_complete now.",
	}
	if step != nil && strings.TrimSpace(step.ResultJSON) != "" {
		response["downstream_result"] = json.RawMessage(step.ResultJSON)
	}
	return response
}

func taskMCPGuidance(task *AgentTask) string {
	if task == nil {
		return ""
	}
	if task.ScheduleKind != "" && !taskStateTerminal(task.State) {
		if task.ScheduleEnabled {
			return "This is a durable schedule parent. The server creates one normal child task at each due occurrence. Confirm the schedule to the user by restating the concrete requested action or exact content and the authoritative next_run_at; do not merely say it is scheduled. Do not claim the action already ran, execute it early, complete it, copy its exact cadence into the directive, or use pace to reproduce it."
		}
		return "This durable schedule is paused. Do not execute or compensate for missed occurrences; resume it explicitly when requested."
	}
	executionConstraint := " Honor explicit execution constraints in the task: if it requires a separate or dedicated worker and is still assigned to main, main must use task_spawn_worker before any domain action."
	switch task.State {
	case taskStateCompleted, taskStateFailed, taskStateCancelled:
		return "This task is terminal. Stop this assignment; do not call more domain tools or send a second terminal report."
	case taskStateWaiting:
		return "The task is waiting. Do not repeat or resubmit already successful external work; pace until the recorded wait condition changes." + executionConstraint
	case taskStateBlocked:
		return "The task is blocked. Inspect recent_events, avoid repeating successful work, and report or resolve the concrete blocker rather than blindly retrying." + executionConstraint
	}
	if task.Progress != nil && *task.Progress >= 100 {
		return "Progress is 100. If the success condition is satisfied, call task_complete now; do not restart completed stages."
	}
	return "Continue only work not already recorded in recent_events. Treat successful domain-tool receipts as completed work and do not repeat them merely to reconfirm them." + executionConstraint
}

func recentTaskEvents(events []AgentTaskEvent, limit int) []AgentTaskEvent {
	if limit <= 0 || len(events) <= limit {
		return events
	}
	return events[len(events)-limit:]
}

func (s *taskMCPServer) taskInCallerScope(task *AgentTask, caller string, mutate bool) error {
	if task == nil || task.AgentID != s.agent.ID {
		return errTaskNotFound
	}
	switch s.profile {
	case taskMCPProfileMain:
		if mutate && strings.HasPrefix(task.AssignedThreadID, "chat-") {
			return fmt.Errorf("%w: conversation-owned task may only be changed by its conversation", errTaskScope)
		}
		return nil
	case taskMCPProfileConversation:
		conversationID := taskCallerConversationID(caller)
		if task.OriginConversationID != conversationID && task.AssignedThreadID != caller {
			return errTaskScope
		}
		if mutate && task.AssignedThreadID != caller {
			return fmt.Errorf("%w: task is assigned to %s", errTaskScope, task.AssignedThreadID)
		}
	case taskMCPProfileWorker:
		if task.AssignedThreadID != caller {
			return errTaskScope
		}
	}
	return nil
}

func (s *taskMCPServer) handleToolCall(ctx context.Context, params json.RawMessage) (any, *mcpRPCError) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "invalid params"}
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	caller := taskString(call.Arguments, "_apteva_caller_context")
	delete(call.Arguments, "_apteva_caller_context")
	if err := s.validateCaller(caller); err != nil {
		return taskTextError(err.Error()), nil
	}
	allowed := map[taskMCPProfile]map[string]bool{
		taskMCPProfileMain: {
			"task_create": true, "task_list": true, "task_get": true,
			"task_assign": true, "task_spawn_worker": true, "task_run_step": true,
			"task_update": true, "task_complete": true, "task_cancel": true,
			"task_pause": true, "task_resume": true, "task_run_now": true,
		},
		taskMCPProfileConversation: {
			"task_create": true, "task_get": true, "task_update": true,
			"task_run_step": true, "task_complete": true, "task_cancel": true,
		},
		taskMCPProfileWorker: {
			"task_get": true, "task_update": true, "task_run_step": true,
			"task_complete": true,
		},
	}
	if !allowed[s.profile][call.Name] {
		return taskTextError(fmt.Sprintf("%s is not available in the %s task scope", call.Name, s.profile)), nil
	}
	if !taskSchedulingEnabled() &&
		(call.Name == "task_pause" || call.Name == "task_resume" || call.Name == "task_run_now") {
		return taskTextError("task scheduling is disabled"), nil
	}

	switch call.Name {
	case "task_create":
		if taskString(call.Arguments, "parent_task_id") != "" {
			return taskTextError("parent_task_id is server-managed and cannot be supplied"), nil
		}
		if taskString(call.Arguments, "origin_conversation_id") != "" ||
			taskString(call.Arguments, "origin_message_id") != "" {
			return taskTextError("task origin is server-managed and cannot be supplied"), nil
		}
		if s.profile == taskMCPProfileMain &&
			taskString(call.Arguments, "assigned_thread_id") != "" {
			return taskTextError("main-created tasks begin on main; use task_spawn_worker for delegated execution"), nil
		}
		schedule, err := taskScheduleArgument(call.Arguments)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if schedule != nil && !taskSchedulingEnabled() {
			return taskTextError("task scheduling is disabled"), nil
		}
		if schedule != nil && s.profile == taskMCPProfileConversation &&
			taskString(call.Arguments, "assign_to") != "main" {
			return taskTextError("conversation-created schedules must use assign_to=main"), nil
		}
		if schedule != nil && s.profile == taskMCPProfileWorker {
			return taskTextError("workers cannot create scheduled tasks"), nil
		}
		if s.profile == taskMCPProfileMain {
			active, err := s.store.ListActiveConversationTasks(s.agent.ID, 20)
			if err != nil {
				return taskTextError(err.Error()), nil
			}
			if len(active) > 0 {
				refs := make([]string, 0, len(active))
				for _, existing := range active {
					refs = append(refs, fmt.Sprintf("%s (%s)", existing.ID, existing.Title))
				}
				return taskTextError(
					"task_create was not executed because conversation-origin task(s) are already active: " +
						strings.Join(refs, ", ") +
						". Adopt the relevant existing task. A conversation-created schedule is already the authoritative schedule and does not need a setup or linked parent task. Do not create unrelated parallel work, recreate, or supersede an existing handoff.",
				), nil
			}
		}
		assignedThreadID := "main"
		originConversationID := ""
		if s.profile == taskMCPProfileConversation {
			originConversationID = taskCallerConversationID(caller)
			if taskString(call.Arguments, "assign_to") == "main" {
				assignedThreadID = "main"
			} else {
				assignedThreadID = caller
			}
		}
		task, created, err := s.store.CreateAgentTask(CreateAgentTaskInput{
			AgentID: s.agent.ID, ProjectID: s.agent.ProjectID,
			Title:                taskString(call.Arguments, "title"),
			Description:          taskString(call.Arguments, "description"),
			AssignedThreadID:     assignedThreadID,
			OriginConversationID: originConversationID,
			IdempotencyKey:       taskString(call.Arguments, "idempotency_key"),
			Schedule:             schedule,
			CreatedByThreadID:    caller,
		})
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		handoffAttempted := false
		if task.ScheduleKind == "" && task.OriginConversationID != "" && task.AssignedThreadID == "main" &&
			!taskStateTerminal(task.State) {
			handoffAttempted = true
			if err := deliverAndRecordTaskHandoff(ctx, s.store, task, s.handoff); err != nil {
				task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
				return taskTextError(fmt.Sprintf(
					"task %s was durably created for main, but its automatic main wake failed: %v; retry task_create with the same idempotency_key",
					task.ID, err,
				)), nil
			}
			task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		}
		return taskTextResult(map[string]any{
			"task": task, "created": created,
			"main_handoff_attempted": handoffAttempted,
			"guidance":               taskMCPGuidance(task),
		}), nil

	case "task_list":
		limit := 100
		if parsed, ok := taskInt(call.Arguments, "limit"); ok {
			limit = *parsed
		}
		tasks, err := s.store.ListAgentTasks(s.agent.ID, AgentTaskListFilter{
			State:          taskString(call.Arguments, "state"),
			AssignedThread: taskString(call.Arguments, "assigned_thread_id"),
			Limit:          limit,
		})
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		return taskTextResult(map[string]any{"tasks": tasks}), nil

	case "task_get":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err == nil {
			err = s.taskInCallerScope(task, caller, false)
		}
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		events, err := s.store.ListAgentTaskEvents(s.agent.ID, task.ID)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		response := map[string]any{
			"task":          task,
			"recent_events": recentTaskEvents(events, 20),
			"guidance":      taskMCPGuidance(task),
		}
		if task.ScheduleKind != "" {
			runs, runsErr := s.store.ListAgentTaskRuns(s.agent.ID, task.ID, 20)
			if runsErr != nil {
				return taskTextError(runsErr.Error()), nil
			}
			response["recent_runs"] = runs
		}
		return taskTextResult(response), nil

	case "task_assign":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if strings.HasPrefix(task.AssignedThreadID, "chat-") {
			return taskTextError("conversation-owned task may only be changed by its conversation"), nil
		}
		assigned := taskString(call.Arguments, "assigned_thread_id")
		if assigned != "main" {
			return taskTextError("worker assignment requires task_spawn_worker; task_assign accepts only main"), nil
		}
		task, changed, err := s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, UpdateAgentTaskInput{
			AssignedThreadID: &assigned,
		})
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		return taskTextResult(map[string]any{"task": task, "changed": changed}), nil

	case "task_spawn_worker":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if taskStateTerminal(task.State) {
			return taskTextError("task is already terminal"), nil
		}
		workerID := taskString(call.Arguments, "worker_id")
		if !validTaskWorkerID(workerID) {
			return taskTextError("worker_id must be 1-96 safe characters and cannot be main or a chat-conversation id"), nil
		}
		if task.AssignedThreadID != "main" {
			if task.AssignedThreadID == workerID {
				return taskTextResult(map[string]any{
					"task": task, "worker_id": workerID, "cached": true,
					"guidance": "This restricted worker is already assigned and running. Continue waiting for its task updates; do not spawn another worker.",
				}), nil
			}
			return taskTextError(fmt.Sprintf(
				"task is already assigned to %s; do not spawn a replacement unless main first resolves and reclaims the existing assignment",
				task.AssignedThreadID,
			)), nil
		}
		instructions := taskString(call.Arguments, "instructions")
		if instructions == "" {
			return taskTextError("instructions are required"), nil
		}
		if s.spawnWorker == nil {
			return taskTextError("server task worker spawning is unavailable"), nil
		}
		previousThread := task.AssignedThreadID
		task, _, err = s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, UpdateAgentTaskInput{
			AssignedThreadID: &workerID,
		})
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if err := s.spawnWorker(ctx, task, workerID, instructions); err != nil {
			_, _, _ = s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, UpdateAgentTaskInput{
				AssignedThreadID: &previousThread,
			})
			return taskTextError(fmt.Sprintf(
				"worker spawn failed and assignment was restored to %s: %v",
				previousThread, err,
			)), nil
		}
		task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		return taskTextResult(map[string]any{
			"task": task, "worker_id": workerID,
			"guidance": "The restricted worker is running with Channels only. Do not execute this task's domain tools from main or spawn another worker.",
		}), nil

	case "task_update":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err == nil {
			err = s.taskInCallerScope(task, caller, true)
		}
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		schedule, scheduleErr := taskScheduleArgument(call.Arguments)
		if scheduleErr != nil {
			return taskTextError(scheduleErr.Error()), nil
		}
		if schedule != nil {
			if s.profile != taskMCPProfileMain || !taskSchedulingEnabled() {
				return taskTextError("only main may update a scheduled task"), nil
			}
			task, changed, err := s.store.UpdateAgentTaskSchedule(
				s.agent.ID, task.ID, caller, *schedule, time.Now().UTC(),
			)
			if err != nil {
				return taskTextError(err.Error()), nil
			}
			return taskTextResult(map[string]any{
				"task": task, "changed": changed,
				"guidance": "The schedule was replaced atomically; unrelated optional task-update fields were ignored. The server owns this exact cadence. Do not copy it into the directive or call pace for its occurrences.",
			}), nil
		}
		input := UpdateAgentTaskInput{ClearProgress: taskBool(call.Arguments, "clear_progress")}
		if value := taskString(call.Arguments, "state"); value != "" {
			input.State = &value
		}
		if value, ok := taskInt(call.Arguments, "progress"); ok {
			input.Progress = value
		} else if _, supplied := call.Arguments["progress"]; supplied {
			return taskTextError("progress must be an integer between 0 and 100"), nil
		}
		if _, ok := call.Arguments["current_step"]; ok {
			value := taskString(call.Arguments, "current_step")
			input.CurrentStep = &value
		}
		if _, ok := call.Arguments["error"]; ok {
			value := taskString(call.Arguments, "error")
			input.Error = &value
		}
		task, changed, err := s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, input)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		deliveryAttempted := false
		if taskStateTerminal(task.State) && task.OriginConversationID != "" &&
			task.CompletionDeliveryStatus != "delivered" {
			deliveryAttempted = true
			if err := deliverAndRecordTaskCompletion(ctx, s.store, task, s.deliver); err != nil {
				return taskTextError(fmt.Sprintf(
					"task state was recorded, but delivery to the originating conversation failed: %v; retry the same task_update",
					err,
				)), nil
			}
			task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		}
		return taskTextResult(map[string]any{
			"task": task, "changed": changed,
			"terminal_delivery_attempted": deliveryAttempted,
			"guidance":                    taskMCPGuidance(task),
		}), nil

	case "task_run_step":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err == nil {
			err = s.taskInCallerScope(task, caller, true)
		}
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if taskStateTerminal(task.State) {
			return taskTextError("task is already terminal; no domain step may run"), nil
		}
		stepKey := taskString(call.Arguments, "step_key")
		if !validTaskStepKey(stepKey) {
			return taskTextError("step_key must be 1-96 characters using letters, numbers, dot, dash, underscore, or colon"), nil
		}
		stepIndex, indexOK := taskInt(call.Arguments, "step_index")
		stepCount, countOK := taskInt(call.Arguments, "step_count")
		if !indexOK || !countOK || *stepIndex < 1 || *stepCount < 1 || *stepIndex > *stepCount {
			return taskTextError("step_index and step_count must be positive integers with step_index <= step_count"), nil
		}
		mcpServer := taskString(call.Arguments, "mcp_server")
		toolName := taskString(call.Arguments, "tool")
		rawArguments, ok := call.Arguments["arguments"]
		if mcpServer == "" || toolName == "" || !ok {
			return taskTextError("mcp_server, tool, and arguments are required"), nil
		}
		downstreamArguments, ok := rawArguments.(map[string]any)
		if !ok {
			return taskTextError("arguments must be an object"), nil
		}
		fingerprintPayload, _ := json.Marshal(map[string]any{
			"mcp_server": mcpServer, "tool": toolName, "arguments": downstreamArguments,
		})
		if taskBool(call.Arguments, "allow_repeated_input") {
			fingerprintPayload, _ = json.Marshal(map[string]any{
				"mcp_server": mcpServer, "tool": toolName,
				"arguments": downstreamArguments, "logical_occurrence": stepKey,
			})
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintPayload))
		step, claimed, err := s.store.ClaimAgentTaskStep(
			s.agent.ID, task.ID, stepKey, caller, mcpServer, toolName, fingerprint,
		)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if !claimed {
			switch step.State {
			case "completed":
				return taskTextResult(taskStepResponse(step, true)), nil
			case "failed":
				return taskTextError(fmt.Sprintf(
					"task step %q previously failed and was not re-executed: %s",
					step.StepKey, step.Error,
				)), nil
			default:
				return taskTextResult(map[string]any{
					"step": step, "cached": true,
					"guidance": "This logical step is already running. Do not call its downstream tool directly or create another step key.",
				}), nil
			}
		}
		if s.executeStep == nil {
			step, _ = s.store.FinishAgentTaskStep(
				s.agent.ID, task.ID, stepKey, caller, "", "task step executor is unavailable",
			)
			return taskTextError("task step executor is unavailable"), nil
		}
		downstreamResult, executeErr := s.executeStep(
			ctx, mcpServer, toolName, cloneTaskArguments(downstreamArguments),
		)
		stepError := ""
		if executeErr != nil {
			stepError = executeErr.Error()
		} else if taskMCPResultIsError(downstreamResult) {
			stepError = strings.TrimSpace(string(downstreamResult))
		}
		step, finishErr := s.store.FinishAgentTaskStep(
			s.agent.ID, task.ID, stepKey, caller, string(downstreamResult), stepError,
		)
		if finishErr != nil {
			return taskTextError(fmt.Sprintf(
				"downstream step returned but its durable receipt could not be stored: %v",
				finishErr,
			)), nil
		}
		if stepError != "" {
			blocked := taskStateBlocked
			currentStep := fmt.Sprintf("Step %d/%d %s failed: %s", *stepIndex, *stepCount, stepKey, stepError)
			_, _, _ = s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, UpdateAgentTaskInput{
				State: &blocked, CurrentStep: &currentStep, Error: &stepError,
			})
			return taskTextError(fmt.Sprintf(
				"task step %q failed and was durably blocked without automatic retry: %s",
				stepKey, stepError,
			)), nil
		}
		progress := (*stepIndex * 100) / *stepCount
		running := taskStateRunning
		currentStep := fmt.Sprintf("Completed step %d/%d: %s", *stepIndex, *stepCount, stepKey)
		if _, _, err := s.store.UpdateAgentTask(s.agent.ID, task.ID, caller, UpdateAgentTaskInput{
			State: &running, Progress: &progress, CurrentStep: &currentStep,
		}); err != nil {
			return taskTextError(fmt.Sprintf(
				"step completed and its receipt is durable, but task progress update failed: %v",
				err,
			)), nil
		}
		return taskTextResult(taskStepResponse(step, false)), nil

	case "task_complete":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err == nil {
			err = s.taskInCallerScope(task, caller, true)
		}
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		result := taskString(call.Arguments, "result")
		if result == "" {
			return taskTextError("result is required"), nil
		}
		task, changed, err := s.store.CompleteAgentTask(s.agent.ID, task.ID, caller, result)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		deliveryAttempted := false
		if task.OriginConversationID != "" && task.CompletionDeliveryStatus != "delivered" {
			deliveryAttempted = true
			if err := deliverAndRecordTaskCompletion(ctx, s.store, task, s.deliver); err != nil {
				return taskTextError(fmt.Sprintf(
					"task completed, but delivery to the originating conversation failed: %v; retry task_complete with the same result",
					err,
				)), nil
			}
			task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		}
		return taskTextResult(map[string]any{
			"task": task, "changed": changed,
			"completion_delivery_attempted": deliveryAttempted,
			"guidance":                      taskMCPGuidance(task),
		}), nil

	case "task_cancel":
		task, err := s.store.GetAgentTask(s.agent.ID, parseAgentTaskID(taskString(call.Arguments, "task_id")))
		if err == nil {
			err = s.taskInCallerScope(task, caller, false)
		}
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		task, changed, err := s.store.CancelAgentTask(
			s.agent.ID, task.ID, caller, taskString(call.Arguments, "reason"),
		)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		deliveryAttempted := false
		if task.OriginConversationID != "" && task.CompletionDeliveryStatus != "delivered" {
			deliveryAttempted = true
			if err := deliverAndRecordTaskCompletion(ctx, s.store, task, s.deliver); err != nil {
				return taskTextError(fmt.Sprintf(
					"task was cancelled, but cancellation delivery failed: %v; retry task_cancel",
					err,
				)), nil
			}
			task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		}
		return taskTextResult(map[string]any{
			"task": task, "changed": changed,
			"cancellation_delivery_attempted": deliveryAttempted,
			"guidance":                        taskMCPGuidance(task),
		}), nil

	case "task_pause", "task_resume":
		task, changed, err := s.store.SetAgentTaskScheduleEnabled(
			s.agent.ID,
			parseAgentTaskID(taskString(call.Arguments, "task_id")),
			caller,
			call.Name == "task_resume",
			time.Now().UTC(),
		)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		return taskTextResult(map[string]any{
			"task": task, "changed": changed,
			"guidance": "The schedule state is durable. Do not reproduce or compensate for it with pace.",
		}), nil

	case "task_run_now":
		task, created, err := s.store.RunAgentTaskScheduleNow(
			s.agent.ID,
			parseAgentTaskID(taskString(call.Arguments, "task_id")),
			caller,
			taskString(call.Arguments, "idempotency_key"),
			time.Now().UTC(),
		)
		if err != nil {
			return taskTextError(err.Error()), nil
		}
		if err := deliverAndRecordTaskHandoff(ctx, s.store, task, s.handoff); err != nil {
			return taskTextError(fmt.Sprintf(
				"scheduled run %s was durably created, but its automatic main wake failed: %v",
				task.ID, err,
			)), nil
		}
		task, _ = s.store.GetAgentTask(s.agent.ID, task.ID)
		return taskTextResult(map[string]any{
			"task": task, "created": created,
			"guidance": "This is one immediate occurrence. The parent schedule's next normal occurrence is unchanged.",
		}), nil
	}
	return taskTextError("unknown task tool"), nil
}

func isServerOwnedTaskMCP(name string) bool {
	switch strings.TrimSpace(name) {
	case taskMainMCPName, taskConversationMCPName, taskWorkerMCPName:
		return true
	default:
		return false
	}
}
