package main

import "path/filepath"

const (
	taskCapabilityMemoryID        = "system_tasks_v1"
	taskCapabilityTag             = "capability:agent-tasks"
	taskCapabilityVersionTag      = "capability-version:agent-tasks:v4"
	taskCapabilityMemoryReason    = "agent task capability sync"
	taskCapabilityMemoryTombstone = "agent task tracking disabled"
)

func taskCapabilityMemoryContent() string {
	return `# Durable agent tasks

The server provides a durable task ledger for meaningful work. It is distinct from global status and from visible conversation messages.

Create exactly one task before substantive work starts when the request is a meaningful multi-step plan, delegates any child work, is scheduled or long-running, or must survive the user's departure. Do not create a task for a brief answer, a quick lookup, every tool call, ordinary thinking, or routine pacing. Creating a task does not imply a main handoff: self-contained interactive work remains assigned to the user conversation; only persistent or autonomous work is assigned to main.

Every execution task is a bounded work record. A scheduled task is a durable parent series whose exact cadence, timezone, next run, and lifecycle are server-owned structured fields. At each due time the server atomically creates exactly one normal child task, queues it for main, and requests a main wake. The schedule does not depend on pace, status next_at, chat history, or a copied directive instruction. Main must process the child task id once even if another wake arrives simultaneously; it must never create a duplicate occurrence or call pace to reproduce the cadence.

For an explicit one-time or recurring request from a user conversation, the conversation creates exactly one scheduled task assigned to main. That task is already the authoritative schedule; main must not create a setup task or a linked schedule. The server stores exact cadence without waking main, then creates and wakes one bounded child occurrence when work is due. For relative one-time requests, the conversation uses schedule.after so the server calculates the deadline. Exact timing belongs only in the scheduled task. Evolve the directive separately only when the user is changing the agent's broader continuing role or responsibility; describe that responsibility without copying cron, interval, timestamp, or task identity into the directive. A request that merely repeats work does not require directive evolution.

Main owns task assignment and the agent's global status. A worker may only read, update, and complete work assigned to its own thread. A user conversation may track work it performs itself or create a task assigned to main when the work must continue autonomously. Runtime caller context enforces these boundaries.

For task-backed work, the task record is the canonical detailed progress record. Read it before resuming work and use its progress and current step to avoid repeating already successful stages. Update it only at meaningful milestones, waits, blockers, or failure. Do not mirror task state or percentages into global status and do not call set_status merely for task milestones. Status is separate and remains only for a distinct non-task agent-level operating condition.

Every task owner, including main, uses task_run_step for each task-backed domain operation instead of calling the raw attached tool. This is the server-owned idempotency boundary: it records increasing progress and returns the stored receipt if another wake or retry repeats the same logical step, without invoking the downstream tool twice. Use one stable outcome-oriented step_key per logical operation plus ordered step_index and step_count. Exact repeated inputs are allowed only with the explicit allow_repeated_input opt-in.

When a task request or description explicitly requires a separate or dedicated worker, delegation is mandatory: main must use task_spawn_worker before any domain action, must not perform that domain work itself, and must not complete the worker's task from main. Never combine task_assign with Core spawn for task work. task_spawn_worker atomically assigns the task and creates a restricted worker with Channels only, preventing raw domain MCP bypass. Give it a stable worker id and instructions containing the task success condition, ordered logical steps, and exact MCP server/tool/arguments for every step. The worker calls task_get first, task_run_step for every domain operation, and task_complete once. If task_get rejects the worker's scope, stop and report the assignment mismatch to main without doing domain work. The worker must never create a duplicate task. For parallel work, create distinct tasks and worker ids.

A recoverable worker problem should use state=blocked with the concrete error and report to main. Main may resolve the issue, assign the same blocked task to a replacement worker, return it to queued or running, and continue its event history. Use state=failed only when the task has reached an unrecoverable terminal outcome. If a domain tool explicitly says an error is permanent, unrecoverable, or cannot be retried, do not call that tool again: record the task failed immediately. For a transient error, block and report it instead of blindly retrying in a loop.

Completing a task with task_complete stores the concrete result. A successful task_complete receipt ends that worker assignment: do not call another domain tool, repeat a successful stage, or send a second terminal report afterward; pace. A terminal failure uses task_update state=failed with an error. task_cancel stops unwanted work. When a terminal outcome belongs to a user conversation, the server automatically routes a structured completed, failed, or cancelled event to that same conversation; cancellation also notifies the assigned thread to stop. Do not send a second terminal outcome through main or channels. The conversation turns the delivered event into one visible final reply. A conversation that originated a task has direct authority to call task_cancel even when main or a worker is assigned; it must do that rather than hand the cancellation to main.

When a conversation delegates immediate durable work to main, it acknowledges the user first and creates the task assigned to main. That unscheduled task_create call durably and automatically wakes main with a server-authoritative assignment event; the conversation must not also call core send for the handoff. A scheduled task_create stores the timer and returns its authoritative next_run_at to the conversation without waking main early. Main is woken only for the server-created due occurrence. Main should call task_get and update and complete an assigned execution task rather than create a duplicate. While any unscheduled conversation-origin task remains active, the server rejects unrelated task_create calls from main. Independent root work may be created after active conversation handoffs complete or through the server API, but there is no model-visible bypass for replacing a handoff.`
}

func taskCapabilityPayload() pushPayload {
	content := taskCapabilityMemoryContent()
	return pushPayload{
		ID:      taskCapabilityMemoryID,
		Content: content,
		Tags: []string{
			channelsCapabilitySystemTag,
			taskCapabilityTag,
			taskCapabilityVersionTag,
			channelsCapabilityHashTagPrefix + skillBodyHash(content),
		},
		Weight: 0.9,
		Reason: taskCapabilityMemoryReason,
	}
}

func (s *Server) syncTaskCapabilityMemory(inst *Agent, enabled, live bool) error {
	if inst == nil {
		return nil
	}
	if live {
		if enabled {
			return s.ensureTaskCapabilityMemoryHTTP(inst.ID)
		}
		return s.removeTaskCapabilityMemoryHTTP(inst.ID)
	}
	if enabled {
		return s.ensureTaskCapabilityMemoryDisk(inst.ID)
	}
	return s.removeTaskCapabilityMemoryDisk(inst.ID)
}

func (s *Server) ensureTaskCapabilityMemoryHTTP(agentID int64) error {
	payload := taskCapabilityPayload()
	record, err := s.findActiveMemoryRecordByTagHTTP(agentID, taskCapabilityTag)
	if err != nil {
		return err
	}
	if memoryRecordMatchesCapabilityPayload(record, payload, taskCapabilityTag, taskCapabilityVersionTag) {
		return nil
	}
	if record.ID != "" {
		if err := s.deleteMemoryHTTP(agentID, record.ID, taskCapabilityMemoryReason); err != nil {
			return err
		}
		if record.ID == payload.ID {
			payload.ID = newServerULID()
		}
	}
	return s.pushPayloadHTTP(agentID, payload)
}

func (s *Server) removeTaskCapabilityMemoryHTTP(agentID int64) error {
	record, err := s.findActiveMemoryRecordByTagHTTP(agentID, taskCapabilityTag)
	if err != nil {
		return err
	}
	if record.ID == "" {
		return nil
	}
	return s.deleteMemoryHTTP(agentID, record.ID, taskCapabilityMemoryTombstone)
}

func (s *Server) ensureTaskCapabilityMemoryDisk(agentID int64) error {
	payload := taskCapabilityPayload()
	path := filepath.Join(s.agents.instanceDir(agentID), "memory.jsonl")
	record, err := findActiveMemoryRecordByTagDisk(path, taskCapabilityTag)
	if err != nil {
		return err
	}
	if memoryRecordMatchesCapabilityPayload(record, payload, taskCapabilityTag, taskCapabilityVersionTag) {
		return nil
	}
	if record.ID != "" {
		if err := s.tombstoneOnDisk(agentID, record.ID, taskCapabilityMemoryReason); err != nil {
			return err
		}
	}
	if seen, err := journalHasID(path, payload.ID); err != nil {
		return err
	} else if seen {
		payload.ID = newServerULID()
	}
	return pushPayloadDiskAt(path, payload)
}

func (s *Server) removeTaskCapabilityMemoryDisk(agentID int64) error {
	path := filepath.Join(s.agents.instanceDir(agentID), "memory.jsonl")
	record, err := findActiveMemoryRecordByTagDisk(path, taskCapabilityTag)
	if err != nil {
		return err
	}
	if record.ID == "" {
		return nil
	}
	return s.tombstoneOnDisk(agentID, record.ID, taskCapabilityMemoryTombstone)
}
