package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	taskScheduleOnce     = "once"
	taskScheduleInterval = "interval"
	taskScheduleCron     = "cron"
)

// AgentTaskScheduleInput is embedded in task_create/task_update and the Tasks
// API. Exact timing lives here, not in the agent directive or status metadata.
type AgentTaskScheduleInput struct {
	Kind          string `json:"kind"`
	At            string `json:"at,omitempty"`
	After         string `json:"after,omitempty"`
	Every         string `json:"every,omitempty"`
	Cron          string `json:"cron,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	OverlapPolicy string `json:"overlap_policy,omitempty"`
	CatchupPolicy string `json:"catchup_policy,omitempty"`
}

type normalizedAgentTaskSchedule struct {
	Kind          string
	Expression    string
	Timezone      string
	OverlapPolicy string
	CatchupPolicy string
	NextRunAt     time.Time
}

var taskCronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

func taskSchedulingEnabled() bool {
	if !taskTrackingEnabled() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_TASK_SCHEDULING"))) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

func normalizeAgentTaskSchedule(input AgentTaskScheduleInput, now time.Time) (normalizedAgentTaskSchedule, error) {
	now = now.UTC()
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	timezone := strings.TrimSpace(input.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return normalizedAgentTaskSchedule{}, fmt.Errorf("invalid schedule timezone %q", timezone)
	}
	overlap := strings.ToLower(strings.TrimSpace(input.OverlapPolicy))
	if overlap == "" {
		overlap = "skip"
	}
	if overlap != "skip" {
		return normalizedAgentTaskSchedule{}, fmt.Errorf("overlap_policy must be skip")
	}
	catchup := strings.ToLower(strings.TrimSpace(input.CatchupPolicy))
	if catchup == "" {
		catchup = "skip"
	}
	if catchup != "skip" {
		return normalizedAgentTaskSchedule{}, fmt.Errorf("catchup_policy must be skip")
	}

	normalized := normalizedAgentTaskSchedule{
		Kind: kind, Timezone: timezone, OverlapPolicy: overlap, CatchupPolicy: catchup,
	}
	switch kind {
	case taskScheduleOnce:
		atText := strings.TrimSpace(input.At)
		afterText := strings.TrimSpace(input.After)
		if atText != "" && afterText != "" {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("once schedule must use exactly one of at or after")
		}
		var at time.Time
		if afterText != "" {
			delay, parseErr := time.ParseDuration(afterText)
			if parseErr != nil {
				return normalizedAgentTaskSchedule{}, fmt.Errorf("invalid once schedule after duration: %w", parseErr)
			}
			if delay < time.Minute {
				return normalizedAgentTaskSchedule{}, fmt.Errorf("once schedule after must be at least 1m")
			}
			at = now.Add(delay)
		} else {
			var parseErr error
			at, parseErr = time.Parse(time.RFC3339, atText)
			if parseErr != nil {
				return normalizedAgentTaskSchedule{}, fmt.Errorf("once schedule at must be RFC3339, or after must be a duration: %w", parseErr)
			}
		}
		at = at.UTC()
		if !at.After(now) {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("once schedule must be in the future")
		}
		normalized.Expression = at.Format(time.RFC3339)
		normalized.NextRunAt = at
	case taskScheduleInterval:
		every := strings.TrimSpace(input.Every)
		duration, parseErr := time.ParseDuration(every)
		if parseErr != nil {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("invalid interval schedule: %w", parseErr)
		}
		if duration < time.Minute {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("interval schedule must be at least 1m")
		}
		normalized.Expression = duration.String()
		normalized.NextRunAt = now.Add(duration)
	case taskScheduleCron:
		expression := strings.Join(strings.Fields(input.Cron), " ")
		if expression == "" {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("cron expression is required")
		}
		parsed, parseErr := taskCronParser.Parse(expression)
		if parseErr != nil {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("invalid five-field cron expression: %w", parseErr)
		}
		next := parsed.Next(now.In(location))
		if next.IsZero() {
			return normalizedAgentTaskSchedule{}, fmt.Errorf("cron expression has no future occurrence")
		}
		normalized.Expression = expression
		normalized.NextRunAt = next.UTC()
	default:
		return normalizedAgentTaskSchedule{}, fmt.Errorf("schedule kind must be once, interval, or cron")
	}
	return normalized, nil
}

func nextAgentTaskScheduleOccurrence(task *AgentTask, scheduledFor, now time.Time) (*time.Time, bool, error) {
	if task == nil {
		return nil, false, errTaskNotFound
	}
	now, scheduledFor = now.UTC(), scheduledFor.UTC()
	switch task.ScheduleKind {
	case taskScheduleOnce:
		return nil, false, nil
	case taskScheduleInterval:
		duration, err := time.ParseDuration(task.ScheduleExpression)
		if err != nil || duration < time.Minute {
			return nil, false, fmt.Errorf("invalid persisted interval %q", task.ScheduleExpression)
		}
		next := scheduledFor.Add(duration)
		if !next.After(now) {
			missed := now.Sub(next)/duration + 1
			next = next.Add(missed * duration)
		}
		return &next, true, nil
	case taskScheduleCron:
		location, err := time.LoadLocation(task.ScheduleTimezone)
		if err != nil {
			return nil, false, fmt.Errorf("invalid persisted timezone %q", task.ScheduleTimezone)
		}
		parsed, err := taskCronParser.Parse(task.ScheduleExpression)
		if err != nil {
			return nil, false, fmt.Errorf("invalid persisted cron %q", task.ScheduleExpression)
		}
		anchor := scheduledFor
		if now.After(anchor) {
			anchor = now
		}
		next := parsed.Next(anchor.In(location)).UTC()
		if next.IsZero() {
			return nil, false, fmt.Errorf("cron expression has no future occurrence")
		}
		return &next, true, nil
	default:
		return nil, false, fmt.Errorf("task %s is not a schedule", task.ID)
	}
}

// scheduleMissedAnotherOccurrence distinguishes normal scheduler polling delay
// from stale recurring work. A run that is only a few seconds late is still
// materialized; when at least one additional cadence boundary has already
// passed, catchup=skip advances the parent without replaying old work.
func scheduleMissedAnotherOccurrence(task *AgentTask, scheduledFor, now time.Time) (bool, error) {
	if task == nil || !now.After(scheduledFor) {
		return false, nil
	}
	switch task.ScheduleKind {
	case taskScheduleOnce:
		// A one-time task is a durable reminder, not a recurring backlog. Run it
		// on recovery even when the server was unavailable at its exact instant.
		return false, nil
	case taskScheduleInterval:
		duration, err := time.ParseDuration(task.ScheduleExpression)
		if err != nil || duration < time.Minute {
			return false, fmt.Errorf("invalid persisted interval %q", task.ScheduleExpression)
		}
		return !scheduledFor.Add(duration).After(now), nil
	case taskScheduleCron:
		location, err := time.LoadLocation(task.ScheduleTimezone)
		if err != nil {
			return false, fmt.Errorf("invalid persisted timezone %q", task.ScheduleTimezone)
		}
		parsed, err := taskCronParser.Parse(task.ScheduleExpression)
		if err != nil {
			return false, fmt.Errorf("invalid persisted cron %q", task.ScheduleExpression)
		}
		following := parsed.Next(scheduledFor.In(location)).UTC()
		return !following.After(now), nil
	default:
		return false, fmt.Errorf("task %s is not a schedule", task.ID)
	}
}

func (s *Store) UpdateAgentTaskSchedule(
	agentID int64,
	taskID, actorThreadID string,
	input AgentTaskScheduleInput,
	now time.Time,
) (*AgentTask, bool, error) {
	normalized, err := normalizeAgentTaskSchedule(input, now)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	current, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		taskID, agentID,
	))
	if err != nil {
		return nil, false, err
	}
	if current.ScheduleKind == "" || taskStateTerminal(current.State) {
		return nil, false, fmt.Errorf("task %s is not an active scheduled task", taskID)
	}
	changed := current.ScheduleKind != normalized.Kind ||
		current.ScheduleExpression != normalized.Expression ||
		current.ScheduleTimezone != normalized.Timezone ||
		current.ScheduleOverlapPolicy != normalized.OverlapPolicy ||
		current.ScheduleCatchupPolicy != normalized.CatchupPolicy ||
		current.NextRunAt == nil || !current.NextRunAt.Equal(normalized.NextRunAt) ||
		!current.ScheduleEnabled
	if !changed {
		return current, false, nil
	}
	now = now.UTC()
	if _, err := tx.Exec(`UPDATE agent_tasks
		SET schedule_kind = ?, schedule_expression = ?, schedule_timezone = ?,
		    schedule_enabled = 1, schedule_overlap_policy = ?,
		    schedule_catchup_policy = ?, next_run_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		normalized.Kind, normalized.Expression, normalized.Timezone,
		normalized.OverlapPolicy, normalized.CatchupPolicy,
		normalized.NextRunAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		taskID, agentID,
	); err != nil {
		return nil, false, err
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: "schedule_updated",
		ThreadID: strings.TrimSpace(actorThreadID), FromState: current.State,
		ToState: current.State, CreatedAt: now,
		Data: map[string]any{
			"schedule_kind": normalized.Kind, "schedule_expression": normalized.Expression,
			"schedule_timezone": normalized.Timezone, "next_run_at": normalized.NextRunAt,
		},
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task, err := s.GetAgentTask(agentID, taskID)
	if err == nil {
		s.emitAgentTaskEvent(event)
	}
	return task, true, err
}

func (s *Store) SetAgentTaskScheduleEnabled(
	agentID int64,
	taskID, actorThreadID string,
	enabled bool,
	now time.Time,
) (*AgentTask, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	current, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		taskID, agentID,
	))
	if err != nil {
		return nil, false, err
	}
	if current.ScheduleKind == "" || taskStateTerminal(current.State) {
		return nil, false, fmt.Errorf("task %s is not an active scheduled task", taskID)
	}
	now = now.UTC()
	var nextRunAt any
	if enabled {
		normalized, normalizeErr := normalizeAgentTaskSchedule(scheduleInputFromTask(current), now)
		if normalizeErr != nil {
			return nil, false, normalizeErr
		}
		nextRunAt = normalized.NextRunAt.Format(time.RFC3339Nano)
	}
	if current.ScheduleEnabled == enabled && (!enabled || current.NextRunAt != nil) {
		return current, false, nil
	}
	if _, err := tx.Exec(`UPDATE agent_tasks
		SET schedule_enabled = ?, next_run_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		boolToInt(enabled), nextRunAt, now.Format(time.RFC3339Nano), taskID, agentID,
	); err != nil {
		return nil, false, err
	}
	eventType := "schedule_paused"
	if enabled {
		eventType = "schedule_resumed"
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: eventType,
		ThreadID: strings.TrimSpace(actorThreadID), FromState: current.State,
		ToState: current.State, CreatedAt: now,
		Data: map[string]any{"enabled": enabled, "next_run_at": nextRunAt},
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task, err := s.GetAgentTask(agentID, taskID)
	if err == nil {
		s.emitAgentTaskEvent(event)
	}
	return task, true, err
}

func scheduleInputFromTask(task *AgentTask) AgentTaskScheduleInput {
	input := AgentTaskScheduleInput{
		Kind: task.ScheduleKind, Timezone: task.ScheduleTimezone,
		OverlapPolicy: task.ScheduleOverlapPolicy,
		CatchupPolicy: task.ScheduleCatchupPolicy,
	}
	switch task.ScheduleKind {
	case taskScheduleOnce:
		input.At = task.ScheduleExpression
	case taskScheduleInterval:
		input.Every = task.ScheduleExpression
	case taskScheduleCron:
		input.Cron = task.ScheduleExpression
	}
	return input
}

func (s *Store) ListAgentTaskRuns(agentID int64, parentTaskID string, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE agent_id = ? AND parent_task_id = ? AND schedule_occurrence_key <> ''
		ORDER BY scheduled_for DESC, created_at DESC LIMIT ?`,
		agentID, strings.TrimSpace(parentTaskID), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]AgentTask, 0)
	for rows.Next() {
		task, scanErr := scanAgentTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		tasks = append(tasks, *task)
	}
	return tasks, rows.Err()
}

type scheduledTaskMaterialization struct {
	Task  AgentTask
	Event AgentTaskEvent
}

func (s *Store) MaterializeDueAgentTaskSchedules(now time.Time, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	now = now.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE schedule_kind <> '' AND schedule_enabled = 1
		  AND next_run_at IS NOT NULL AND next_run_at <= ?
		  AND state NOT IN ('completed','failed','cancelled')
		ORDER BY next_run_at ASC, id ASC LIMIT ?`,
		now.Format(time.RFC3339Nano), limit,
	)
	if err != nil {
		return nil, err
	}
	parents := make([]AgentTask, 0)
	for rows.Next() {
		task, scanErr := scanAgentTask(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		parents = append(parents, *task)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	children := make([]AgentTask, 0, len(parents))
	events := make([]AgentTaskEvent, 0, len(parents)*2)
	for i := range parents {
		parent := &parents[i]
		if parent.NextRunAt == nil {
			continue
		}
		scheduledFor := parent.NextRunAt.UTC()
		nextRun, staysEnabled, nextErr := nextAgentTaskScheduleOccurrence(parent, scheduledFor, now)
		if nextErr != nil {
			return nil, nextErr
		}
		var nextRunValue any
		if nextRun != nil {
			nextRunValue = nextRun.Format(time.RFC3339Nano)
		}
		missedCatchup, missedErr := scheduleMissedAnotherOccurrence(parent, scheduledFor, now)
		if missedErr != nil {
			return nil, missedErr
		}
		if parent.ScheduleCatchupPolicy == "skip" && missedCatchup {
			if _, err := tx.Exec(`UPDATE agent_tasks
				SET schedule_enabled = ?, next_run_at = ?, updated_at = ?
				WHERE id = ? AND next_run_at = ? AND schedule_enabled = 1`,
				boolToInt(staysEnabled), nextRunValue, now.Format(time.RFC3339Nano),
				parent.ID, scheduledFor.Format(time.RFC3339Nano),
			); err != nil {
				return nil, err
			}
			event := AgentTaskEvent{
				TaskID: parent.ID, AgentID: parent.AgentID,
				EventType: "schedule_occurrence_skipped", ThreadID: "scheduler",
				FromState: parent.State, ToState: parent.State, CreatedAt: now,
				Data: map[string]any{
					"scheduled_for": scheduledFor, "reason": "catchup",
					"next_run_at": nextRun,
				},
			}
			if err := insertAgentTaskEvent(tx, event); err != nil {
				return nil, err
			}
			events = append(events, event)
			continue
		}
		activeChildren := 0
		if parent.ScheduleOverlapPolicy == "skip" {
			if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_tasks
				WHERE parent_task_id = ?
				  AND schedule_occurrence_key <> ''
				  AND state IN ('queued','running','waiting','blocked')`,
				parent.ID,
			).Scan(&activeChildren); err != nil {
				return nil, err
			}
		}
		if activeChildren > 0 {
			if _, err := tx.Exec(`UPDATE agent_tasks
				SET schedule_enabled = ?, next_run_at = ?, updated_at = ?
				WHERE id = ? AND next_run_at = ? AND schedule_enabled = 1`,
				boolToInt(staysEnabled), nextRunValue, now.Format(time.RFC3339Nano),
				parent.ID, scheduledFor.Format(time.RFC3339Nano),
			); err != nil {
				return nil, err
			}
			event := AgentTaskEvent{
				TaskID: parent.ID, AgentID: parent.AgentID,
				EventType: "schedule_occurrence_skipped", ThreadID: "scheduler",
				FromState: parent.State, ToState: parent.State, CreatedAt: now,
				Data: map[string]any{
					"scheduled_for": scheduledFor, "reason": "overlap",
					"next_run_at": nextRun,
				},
			}
			if err := insertAgentTaskEvent(tx, event); err != nil {
				return nil, err
			}
			events = append(events, event)
			continue
		}

		child, childEvent, insertErr := insertScheduledAgentTaskOccurrence(
			tx, parent, scheduledFor,
			"scheduled:"+scheduledFor.Format(time.RFC3339Nano), now,
		)
		if insertErr != nil {
			return nil, insertErr
		}
		if _, err := tx.Exec(`UPDATE agent_tasks
			SET schedule_enabled = ?, next_run_at = ?, last_run_at = ?, updated_at = ?
			WHERE id = ? AND next_run_at = ? AND schedule_enabled = 1`,
			boolToInt(staysEnabled), nextRunValue, scheduledFor.Format(time.RFC3339Nano),
			now.Format(time.RFC3339Nano), parent.ID, scheduledFor.Format(time.RFC3339Nano),
		); err != nil {
			return nil, err
		}
		parentEvent := AgentTaskEvent{
			TaskID: parent.ID, AgentID: parent.AgentID,
			EventType: "schedule_occurrence_created", ThreadID: "scheduler",
			FromState: parent.State, ToState: parent.State, CreatedAt: now,
			Data: map[string]any{
				"scheduled_for": scheduledFor, "occurrence_task_id": child.ID,
				"next_run_at": nextRun,
			},
		}
		if err := insertAgentTaskEvent(tx, parentEvent); err != nil {
			return nil, err
		}
		children = append(children, *child)
		events = append(events, childEvent, parentEvent)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, event := range events {
		s.emitAgentTaskEvent(event)
	}
	for i := range children {
		refreshed, getErr := s.GetAgentTask(children[i].AgentID, children[i].ID)
		if getErr == nil {
			children[i] = *refreshed
		}
	}
	return children, nil
}

func insertScheduledAgentTaskOccurrence(
	tx *sql.Tx,
	parent *AgentTask,
	scheduledFor time.Time,
	occurrenceKey string,
	now time.Time,
) (*AgentTask, AgentTaskEvent, error) {
	taskID := "task-" + newServerULID()
	nowText := now.UTC().Format(time.RFC3339Nano)
	scheduledText := scheduledFor.UTC().Format(time.RFC3339Nano)
	idempotencyKey := "schedule-occurrence:" + parent.ID + ":" + occurrenceKey
	_, err := tx.Exec(`INSERT INTO agent_tasks (
		id, agent_id, project_id, title, description, state, current_step,
		assigned_thread_id, parent_task_id, idempotency_key, scheduled_for,
		schedule_occurrence_key, handoff_delivery_status, created_by_thread_id,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, 'queued', '', 'main', ?, ?, ?, ?, 'pending',
		'scheduler', ?, ?)`,
		taskID, parent.AgentID, parent.ProjectID, parent.Title, parent.Description,
		parent.ID, idempotencyKey, scheduledText, occurrenceKey, nowText, nowText,
	)
	if err != nil {
		return nil, AgentTaskEvent{}, err
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: parent.AgentID, EventType: "created",
		ThreadID: "scheduler", ToState: taskStateQueued, CreatedAt: now.UTC(),
		Data: map[string]any{
			"title": parent.Title, "assigned_thread_id": "main",
			"parent_task_id": parent.ID, "scheduled_for": scheduledFor.UTC(),
		},
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return nil, AgentTaskEvent{}, err
	}
	task := &AgentTask{
		ID: taskID, AgentID: parent.AgentID, ProjectID: parent.ProjectID,
		Title: parent.Title, Description: parent.Description, State: taskStateQueued,
		AssignedThreadID: "main", ParentTaskID: parent.ID,
		IdempotencyKey: idempotencyKey, ScheduledFor: &scheduledFor,
		ScheduleOccurrenceKey: occurrenceKey, HandoffDeliveryStatus: "pending",
		CreatedByThreadID: "scheduler", CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	return task, event, nil
}

func (s *Store) RunAgentTaskScheduleNow(
	agentID int64,
	taskID, actorThreadID, idempotencyKey string,
	now time.Time,
) (*AgentTask, bool, error) {
	now = now.UTC()
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, false, fmt.Errorf("idempotency_key is required")
	}
	if len(idempotencyKey) > 240 {
		return nil, false, fmt.Errorf("idempotency_key must be at most 240 characters")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	parent, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		taskID, agentID,
	))
	if err != nil {
		return nil, false, err
	}
	if parent.ScheduleKind == "" || taskStateTerminal(parent.State) {
		return nil, false, fmt.Errorf("task %s is not an active scheduled task", taskID)
	}
	occurrenceKey := "manual:" + idempotencyKey
	existing, existingErr := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks
		 WHERE agent_id = ? AND parent_task_id = ? AND schedule_occurrence_key = ?`,
		agentID, parent.ID, occurrenceKey,
	))
	if existingErr == nil {
		return existing, false, nil
	}
	if !errors.Is(existingErr, errTaskNotFound) {
		return nil, false, existingErr
	}
	child, childEvent, err := insertScheduledAgentTaskOccurrence(
		tx, parent, now, occurrenceKey, now,
	)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE agent_tasks SET last_run_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), taskID, agentID,
	); err != nil {
		return nil, false, err
	}
	parentEvent := AgentTaskEvent{
		TaskID: parent.ID, AgentID: parent.AgentID,
		EventType: "schedule_run_now", ThreadID: strings.TrimSpace(actorThreadID),
		FromState: parent.State, ToState: parent.State, CreatedAt: now,
		Data: map[string]any{
			"scheduled_for": now, "occurrence_task_id": child.ID,
		},
	}
	if err := insertAgentTaskEvent(tx, parentEvent); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	s.emitAgentTaskEvent(childEvent)
	s.emitAgentTaskEvent(parentEvent)
	task, err := s.GetAgentTask(agentID, child.ID)
	return task, true, err
}

func dispatchDueAgentTaskSchedules(
	ctx context.Context,
	store *Store,
	now time.Time,
	deliver taskHandoffDelivery,
) ([]AgentTask, error) {
	children, err := store.MaterializeDueAgentTaskSchedules(now, 50)
	if err != nil {
		return nil, err
	}
	var deliveryErrors []error
	for i := range children {
		task := &children[i]
		if err := deliverAndRecordTaskHandoff(ctx, store, task, deliver); err != nil {
			deliveryErrors = append(deliveryErrors, fmt.Errorf("%s: %w", task.ID, err))
		}
		if refreshed, getErr := store.GetAgentTask(task.AgentID, task.ID); getErr == nil {
			children[i] = *refreshed
		}
	}
	return children, errors.Join(deliveryErrors...)
}

func taskSchedulerInterval() time.Duration {
	if value := strings.TrimSpace(os.Getenv("APTEVA_TASK_SCHEDULER_INTERVAL")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed >= time.Second {
			return parsed
		}
	}
	return 10 * time.Second
}

func (s *Server) startTaskScheduler() {
	if s == nil || s.store == nil || !taskSchedulingEnabled() || s.taskSchedulerCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.taskSchedulerCancel = cancel
	go func() {
		run := func() {
			if _, err := dispatchDueAgentTaskSchedules(
				ctx, s.store, time.Now().UTC(), s.deliverTaskHandoff,
			); err != nil && ctx.Err() == nil {
				log.Printf("[TASKS] scheduled dispatch: %v", err)
			}
		}
		run()
		ticker := time.NewTicker(taskSchedulerInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}

func (s *Server) stopTaskScheduler() {
	if s != nil && s.taskSchedulerCancel != nil {
		s.taskSchedulerCancel()
		s.taskSchedulerCancel = nil
	}
}
