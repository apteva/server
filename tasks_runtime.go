package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type taskStepMCPConfig struct {
	URL string
	Env map[string]string
}

func taskStepExecutorFromConfig(config map[string]any) taskStepExecutor {
	configs := map[string]taskStepMCPConfig{}
	add := func(raw map[string]any) {
		name, _ := raw["name"].(string)
		name = strings.TrimSpace(name)
		url, _ := raw["url"].(string)
		url = strings.TrimSpace(url)
		transport, _ := raw["transport"].(string)
		transport = strings.ToLower(strings.TrimSpace(transport))
		if name == "" || url == "" || (transport != "" && transport != "http") ||
			name == "channels" || name == agentOutputMCPName ||
			isServerOwnedTaskMCP(name) {
			return
		}
		env := map[string]string{}
		switch rawEnv := raw["env"].(type) {
		case map[string]any:
			for key, value := range rawEnv {
				if text, ok := value.(string); ok {
					env[key] = text
				}
			}
		case map[string]string:
			for key, value := range rawEnv {
				env[key] = value
			}
		}
		configs[name] = taskStepMCPConfig{URL: url, Env: env}
	}
	switch servers := config["mcp_servers"].(type) {
	case []any:
		for _, item := range servers {
			if raw, ok := item.(map[string]any); ok {
				add(raw)
			}
		}
	case []map[string]any:
		for _, raw := range servers {
			add(raw)
		}
	default:
		// Some tests and migration paths round-trip the config through an
		// untyped JSON value. Normalize it without changing the live config.
		encoded, _ := json.Marshal(servers)
		var normalized []map[string]any
		if json.Unmarshal(encoded, &normalized) == nil {
			for _, raw := range normalized {
				add(raw)
			}
		}
	}
	return func(_ context.Context, mcpServer, toolName string, arguments map[string]any) (json.RawMessage, error) {
		selected, ok := configs[strings.TrimSpace(mcpServer)]
		if !ok {
			return nil, fmt.Errorf("MCP server %q is not an attached HTTP server available for task steps", mcpServer)
		}
		toolName = strings.TrimSpace(toolName)
		if toolName == "" {
			return nil, fmt.Errorf("downstream tool name is required")
		}
		// Core namespaces colliding MCP tools as <server>_<tool>. The task
		// executor addresses the selected server directly, so forward only
		// the underlying tool name.
		if prefix := strings.TrimSpace(mcpServer) + "_"; strings.HasPrefix(toolName, prefix) {
			toolName = strings.TrimPrefix(toolName, prefix)
		}
		return callRemoteMCPTool(selected.URL, toolName, arguments, selected.Env)
	}
}

func spawnTaskWorkerOnCore(
	ctx context.Context,
	port int,
	coreAPIKey string,
	task *AgentTask,
	workerID, instructions string,
) error {
	if port <= 0 || task == nil {
		return fmt.Errorf("running core and task are required")
	}
	directive := fmt.Sprintf(`

---
[SERVER-OWNED DURABLE TASK WORKER]
You are the dedicated worker for task %s: %s.

%s

This is a hard execution boundary. Call task_get(%q) first. Execute every
domain operation with task_run_step using the supplied stable logical step,
MCP server, tool, arguments, step index, and step count. Never call a raw
domain MCP tool directly. task_run_step records progress and deduplicates
repeated inputs. After the success condition is met, call task_complete once.
For a recoverable failure, record the task blocked and report the concrete
blocker to main with send. Do not create tasks, change assignment, or accept
unrelated work.`,
		task.ID, task.Title, strings.TrimSpace(instructions), task.ID,
	)
	body, _ := json.Marshal(map[string]any{
		"directive_suffix": directive,
		"tools":            []string{"send", "pace"},
		"mcp":              []string{"channels"},
		"conversation":     false,
	})
	endpoint := fmt.Sprintf(
		"http://127.0.0.1:%d/threads/%s", port, url.PathEscape(workerID),
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if coreAPIKey != "" {
		request.Header.Set("Authorization", "Bearer "+coreAPIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("core worker spawn returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *Server) installTaskTrackingHooks() {
	if s == nil {
		return
	}
	if s.store != nil {
		s.store.SetAgentTaskEventHook(s.broadcastAgentTaskEvent)
	}
	if s.agents == nil {
		return
	}
	s.agents.TaskStore = s.store
	s.agents.TaskTracking = taskTrackingEnabled()
	s.agents.TaskCompletionDelivery = s.deliverTaskCompletion
	s.agents.TaskHandoffDelivery = s.deliverTaskHandoff
	s.agents.TaskCapabilitySync = s.syncTaskCapabilityMemory
	s.agents.TaskDeliveryRecovery = s.recoverTaskDeliveries
}

func (s *Server) broadcastAgentTaskEvent(event AgentTaskEvent) {
	if s == nil || s.store == nil || s.broadcaster == nil || event.AgentID <= 0 {
		return
	}
	task, err := s.store.GetAgentTask(event.AgentID, event.TaskID)
	if err != nil {
		return
	}
	if event.ID == "" {
		event.ID = "task-live-" + newServerULID()
	}
	eventType := "task.updated"
	switch event.EventType {
	case "created":
		eventType = "task.created"
	case "schedule_updated", "schedule_paused", "schedule_resumed",
		"schedule_occurrence_created", "schedule_occurrence_skipped",
		"schedule_run_now":
		eventType = "task.schedule.updated"
	case "assigned":
		eventType = "task.assigned"
	case "step_started":
		eventType = "task.step.started"
	case "step_completed":
		eventType = "task.step.completed"
	case "step_failed":
		eventType = "task.step.failed"
	case "handoff_nudge_claimed":
		eventType = "task.handoff.nudged"
	case "handoff_delivery_delivered", "handoff_delivery_failed",
		"completion_delivery_delivered", "completion_delivery_failed":
		eventType = "task.delivery.updated"
	case "state_changed":
		switch event.ToState {
		case taskStateCompleted:
			eventType = "task.completed"
		case taskStateFailed:
			eventType = "task.failed"
		case taskStateCancelled:
			eventType = "task.cancelled"
		}
	}
	payload, err := json.Marshal(map[string]any{
		"task":       task,
		"task_event": event,
	})
	if err != nil {
		return
	}
	s.broadcaster.Broadcast([]TelemetryEvent{{
		ID:       event.ID,
		AgentID:  event.AgentID,
		ThreadID: event.ThreadID,
		Type:     eventType,
		Time:     event.CreatedAt,
		Data:     payload,
	}})
}

func (s *Server) deliverTaskHandoff(ctx context.Context, task *AgentTask) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	conversationID := strings.TrimSpace(task.OriginConversationID)
	if task.AssignedThreadID != "main" || taskStateTerminal(task.State) {
		return nil
	}
	if conversationID != "" && !strings.HasPrefix(conversationID, "conv-") {
		return fmt.Errorf("invalid origin conversation id %q", conversationID)
	}
	port := s.agents.GetPort(task.AgentID)
	if port <= 0 {
		return fmt.Errorf("agent %d is not running", task.AgentID)
	}
	var message string
	if task.ScheduledFor != nil && task.ParentTaskID != "" {
		message = fmt.Sprintf(`[SCHEDULED TASK OCCURRENCE — SERVER-AUTHORITATIVE]
task_id: %s
schedule_task_id: %s
scheduled_for: %s
title: %s
requirements:
%s

The Tasks scheduler persisted exactly one occurrence and assigned it to main.
Adopt this exact task with task_get; do not create another task and do not
execute from a copied directive schedule. Use task_run_step with stable logical
step keys for every domain operation so a retry or simultaneous wake receives
the stored receipt instead of repeating the side effect. Update it at meaningful
milestones, delegate only through task_spawn_worker when useful, and complete it
once with the concrete result. The parent schedule already owns the next
occurrence, so do not call pace to reproduce this cadence. A simultaneous
ordinary wake does not change this task identity; process the queued occurrence
once.`,
			task.ID, task.ParentTaskID, task.ScheduledFor.UTC().Format(time.RFC3339),
			task.Title, strings.TrimSpace(task.Description),
		)
	} else if conversationID != "" {
		message = fmt.Sprintf(`[TASK ASSIGNED FROM USER CONVERSATION — SERVER-AUTHORITATIVE]
task_id: %s
title: %s
origin_thread: chat-%s
requirements:
%s

The server already persisted and assigned this task to main. Adopt this exact task with task_get; do not create or supersede it. Use task_run_step with stable logical keys for every domain operation. Update it at meaningful milestones. If the requirements mandate a dedicated worker, use task_spawn_worker. Otherwise perform the durable work from main. Complete the task once with its concrete result. The server will return the terminal outcome to the originating conversation automatically, so do not send a duplicate completion there.`,
			task.ID, task.Title, conversationID, strings.TrimSpace(task.Description),
		)
	} else {
		message = fmt.Sprintf(`[TASK CREATED BY OPERATOR — SERVER-AUTHORITATIVE]
task_id: %s
title: %s
requirements:
%s

The operator created this durable task from the Tasks page. Adopt this exact task with task_get; do not create or supersede it. Use task_run_step with stable logical keys for every domain operation and update it only at meaningful milestones. If the requirements call for a dedicated worker, use task_spawn_worker. Otherwise perform the work from main. Complete the task once with its concrete result; the result and progress are visible live in the Tasks page. Do not send an unrelated channel message unless the task itself requires one.`,
			task.ID, task.Title, strings.TrimSpace(task.Description),
		)
	}
	if err := postCoreEvent(ctx, port, s.agents.GetCoreAPIKey(task.AgentID), "main", message); err != nil {
		return err
	}
	s.scheduleQueuedTaskHandoffNudge(task.AgentID, task.ID)
	return nil
}

func deliverAndRecordTaskHandoff(ctx context.Context, store *Store, task *AgentTask, deliver taskHandoffDelivery) error {
	if task == nil || task.AssignedThreadID != "main" || taskStateTerminal(task.State) ||
		task.HandoffDeliveryStatus == "" || task.HandoffDeliveryStatus == "delivered" {
		return nil
	}
	if deliver == nil {
		return fmt.Errorf("main handoff delivery is unavailable")
	}
	if err := deliver(ctx, task); err != nil {
		_ = store.MarkAgentTaskHandoffDelivery(task.AgentID, task.ID, "failed", err.Error())
		return err
	}
	return store.MarkAgentTaskHandoffDelivery(task.AgentID, task.ID, "delivered", "")
}

func (s *Server) deliverTaskCompletion(ctx context.Context, task *AgentTask) error {
	if task == nil {
		return fmt.Errorf("task is nil")
	}
	conversationID := strings.TrimSpace(task.OriginConversationID)
	if conversationID == "" {
		return nil
	}
	if !strings.HasPrefix(conversationID, "conv-") {
		return fmt.Errorf("invalid origin conversation id %q", conversationID)
	}
	port := s.agents.GetPort(task.AgentID)
	if port <= 0 {
		return fmt.Errorf("agent %d is not running", task.AgentID)
	}
	if s.taskConversationEnsure != nil {
		if err := s.taskConversationEnsure(task.AgentID, conversationID); err != nil {
			return fmt.Errorf("ensure originating conversation thread: %w", err)
		}
	}
	coreKey := s.agents.GetCoreAPIKey(task.AgentID)
	threadID := "chat-" + conversationID
	var heading, outcomeLabel, outcome string
	switch task.State {
	case taskStateCompleted:
		heading, outcomeLabel, outcome = "TASK COMPLETED", "result", task.Result
	case taskStateFailed:
		heading, outcomeLabel, outcome = "TASK FAILED", "error", task.Error
	case taskStateCancelled:
		heading, outcomeLabel, outcome = "TASK CANCELLED", "reason", task.Error
	default:
		return fmt.Errorf("task %s is not terminal", task.ID)
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = "No additional detail was recorded."
	}

	// Cancellation must also reach the responsible thread so work actually
	// stops rather than merely disappearing from the ledger.
	if task.State == taskStateCancelled {
		assigned := strings.TrimSpace(task.AssignedThreadID)
		if assigned != "" && assigned != threadID {
			stopMessage := fmt.Sprintf(`[TASK CANCELLED — SERVER-AUTHORITATIVE]
task_id: %s
title: %s
reason:
%s

Stop work on this task now. Do not call task_complete or continue its tools. Acknowledge internally with pace after any in-flight tool returns; the originating user conversation receives its own cancellation outcome.`,
				task.ID, task.Title, outcome,
			)
			if err := postCoreEvent(ctx, port, coreKey, assigned, stopMessage); err != nil {
				return fmt.Errorf("notify assigned thread %s: %w", assigned, err)
			}
		}
	}

	message := fmt.Sprintf(`[%s — SERVER-AUTHORITATIVE]
task_id: %s
title: %s
%s:
%s

This durable task reached its terminal outcome. Send one concise final visible reply to the user in this conversation with channels_send(channel="current", phase="final"). State the outcome accurately. Do not create, hand off, update, or complete another task for this receipt, and do not duplicate the outcome through main.`,
		heading, task.ID, task.Title, outcomeLabel, outcome,
	)
	return postCoreEvent(ctx, port, coreKey, threadID, message)
}

func deliverAndRecordTaskCompletion(ctx context.Context, store *Store, task *AgentTask, deliver taskCompletionDelivery) error {
	if task == nil || task.OriginConversationID == "" || task.CompletionDeliveryStatus == "delivered" {
		return nil
	}
	if deliver == nil {
		return fmt.Errorf("completion delivery is unavailable")
	}
	if err := deliver(ctx, task); err != nil {
		_ = store.MarkAgentTaskCompletionDelivery(task.AgentID, task.ID, "failed", err.Error())
		return err
	}
	return store.MarkAgentTaskCompletionDelivery(task.AgentID, task.ID, "delivered", "")
}

func (s *Server) recoverTaskDeliveries(agentID int64) {
	if s == nil || !taskTrackingEnabled() || agentID <= 0 {
		return
	}
	handoffs, err := s.store.ListUndeliveredAgentTaskHandoffs(agentID, 100)
	if err != nil {
		log.Printf("[TASKS] list undelivered handoffs agent=%d: %v", agentID, err)
	} else {
		for i := range handoffs {
			task := &handoffs[i]
			if err := deliverAndRecordTaskHandoff(context.Background(), s.store, task, s.deliverTaskHandoff); err != nil {
				log.Printf("[TASKS] recover handoff agent=%d task=%s: %v", agentID, task.ID, err)
			}
		}
	}
	tasks, err := s.store.ListUndeliveredTerminalAgentTasks(agentID, 100)
	if err != nil {
		log.Printf("[TASKS] list undelivered outcomes agent=%d: %v", agentID, err)
		return
	}
	for i := range tasks {
		task := &tasks[i]
		if err := deliverAndRecordTaskCompletion(context.Background(), s.store, task, s.deliverTaskCompletion); err != nil {
			log.Printf("[TASKS] recover outcome agent=%d task=%s state=%s: %v", agentID, task.ID, task.State, err)
		}
	}
	queued, err := s.store.ListQueuedDeliveredAgentTaskHandoffs(agentID, 100)
	if err != nil {
		log.Printf("[TASKS] list queued delivered handoffs agent=%d: %v", agentID, err)
		return
	}
	for i := range queued {
		s.scheduleQueuedTaskHandoffNudge(agentID, queued[i].ID)
	}
}

func (s *Server) scheduleQueuedTaskHandoffNudge(agentID int64, taskID string) {
	if s == nil || !taskTrackingEnabled() || agentID <= 0 || strings.TrimSpace(taskID) == "" {
		return
	}
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		<-timer.C
		if err := s.wakeQueuedTaskHandoff(agentID, taskID); err != nil {
			log.Printf("[TASKS] queued handoff nudge agent=%d task=%s: %v", agentID, taskID, err)
		}
	}()
}

func (s *Server) wakeQueuedTaskHandoff(agentID int64, taskID string) error {
	if s == nil || s.store == nil || s.agents == nil {
		return nil
	}
	task, claimed, err := s.store.ClaimAgentTaskHandoffNudge(agentID, taskID)
	if err != nil || !claimed {
		return err
	}
	port := s.agents.GetPort(agentID)
	if port <= 0 {
		_ = s.store.ReleaseAgentTaskHandoffNudge(agentID, taskID)
		return fmt.Errorf("agent is not running")
	}
	message := fmt.Sprintf(`[QUEUED TASK FOLLOW-UP — SERVER-AUTHORITATIVE]
task_id: %s
title: %s

This server-owned task was delivered to main but remains queued. Call
task_get now and begin the existing task. Honor its execution constraints; use
task_run_step for every domain operation and task_spawn_worker when it requires
a separate worker. Do not create a duplicate, repeat the user acknowledgement,
or send an early completion. This is the server's only automatic queued-task
follow-up.`,
		task.ID, task.Title,
	)
	if err := postCoreEvent(
		context.Background(), port, s.agents.GetCoreAPIKey(agentID), "main", message,
	); err != nil {
		_ = s.store.ReleaseAgentTaskHandoffNudge(agentID, taskID)
		return err
	}
	return nil
}

// scheduleMainTaskResumeAfterDirectiveEvolution closes a Core wake gap without
// making telemetry authoritative for task state. Core has already emitted the
// durable directive.evolved receipt and the server has persisted the new
// directive before this runs. If Core naturally continues and completes the
// task, the delayed state check is a no-op. Otherwise one ordinary event wakes
// main to finish the existing task rather than repeat the directive mutation.
func (s *Server) scheduleMainTaskResumeAfterDirectiveEvolution(agentID int64) {
	if s == nil || !taskTrackingEnabled() || agentID <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		<-timer.C
		if err := s.wakeMainTasksAfterDirectiveEvolution(agentID); err != nil {
			log.Printf("[TASKS] post-evolve resume agent=%d: %v", agentID, err)
		}
	}()
}

func (s *Server) wakeMainTasksAfterDirectiveEvolution(agentID int64) error {
	if s == nil || s.store == nil || s.agents == nil {
		return nil
	}
	tasks, err := s.store.ListActiveConversationTasks(agentID, 20)
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		if task.AssignedThreadID != "main" || taskStateTerminal(task.State) {
			continue
		}
		refs = append(refs, fmt.Sprintf("- %s: %s (state=%s)", task.ID, task.Title, task.State))
	}
	if len(refs) == 0 {
		return nil
	}
	port := s.agents.GetPort(agentID)
	if port <= 0 {
		return fmt.Errorf("agent is not running")
	}
	message := `[DIRECTIVE UPDATE PERSISTED — SERVER TASK RESUME]
The server received Core's authoritative directive.evolved receipt. Do not
repeat the directive change. These conversation-origin tasks are still assigned
to main:
` + strings.Join(refs, "\n") + `

Call task_get for the relevant existing task. Verify any remaining setup
requirements. When the request is recurring, verify that one structured
scheduled parent exists and contains its authoritative next_run_at; do not
reproduce that timing with pace. If the setup success condition is satisfied,
call task_complete exactly once with the concrete configured outcome. Do not
create another setup task or send a duplicate result to the conversation; the
server routes terminal delivery.`
	return postCoreEvent(
		context.Background(), port, s.agents.GetCoreAPIKey(agentID), "main", message,
	)
}
