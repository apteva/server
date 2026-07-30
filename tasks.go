package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	taskStateQueued    = "queued"
	taskStateRunning   = "running"
	taskStateWaiting   = "waiting"
	taskStateBlocked   = "blocked"
	taskStateCompleted = "completed"
	taskStateFailed    = "failed"
	taskStateCancelled = "cancelled"
)

var (
	errTaskNotFound        = errors.New("task not found")
	errTaskInvalidState    = errors.New("invalid task state")
	errTaskTerminal        = errors.New("task is already terminal")
	errTaskScope           = errors.New("task is outside this thread's scope")
	errTaskInvalidProgress = errors.New("progress must be between 0 and 100")
)

func taskTrackingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_TASK_TRACKING"))) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

type AgentTask struct {
	ID                       string     `json:"id"`
	AgentID                  int64      `json:"agent_id"`
	ProjectID                string     `json:"project_id"`
	Title                    string     `json:"title"`
	Description              string     `json:"description,omitempty"`
	State                    string     `json:"state"`
	Progress                 *int       `json:"progress,omitempty"`
	CurrentStep              string     `json:"current_step,omitempty"`
	AssignedThreadID         string     `json:"assigned_thread_id"`
	OriginConversationID     string     `json:"origin_conversation_id,omitempty"`
	OriginMessageID          string     `json:"origin_message_id,omitempty"`
	ParentTaskID             string     `json:"parent_task_id,omitempty"`
	IdempotencyKey           string     `json:"idempotency_key,omitempty"`
	ScheduleKind             string     `json:"schedule_kind,omitempty"`
	ScheduleExpression       string     `json:"schedule_expression,omitempty"`
	ScheduleTimezone         string     `json:"schedule_timezone,omitempty"`
	ScheduleEnabled          bool       `json:"schedule_enabled,omitempty"`
	ScheduleOverlapPolicy    string     `json:"schedule_overlap_policy,omitempty"`
	ScheduleCatchupPolicy    string     `json:"schedule_catchup_policy,omitempty"`
	NextRunAt                *time.Time `json:"next_run_at,omitempty"`
	LastRunAt                *time.Time `json:"last_run_at,omitempty"`
	ScheduledFor             *time.Time `json:"scheduled_for,omitempty"`
	ScheduleOccurrenceKey    string     `json:"schedule_occurrence_key,omitempty"`
	Result                   string     `json:"result,omitempty"`
	Error                    string     `json:"error,omitempty"`
	HandoffDeliveryStatus    string     `json:"handoff_delivery_status,omitempty"`
	HandoffDeliveryError     string     `json:"handoff_delivery_error,omitempty"`
	HandoffDeliveredAt       *time.Time `json:"handoff_delivered_at,omitempty"`
	CompletionDeliveryStatus string     `json:"completion_delivery_status,omitempty"`
	CompletionDeliveryError  string     `json:"completion_delivery_error,omitempty"`
	CompletionDeliveredAt    *time.Time `json:"completion_delivered_at,omitempty"`
	LastActivityAt           *time.Time `json:"last_activity_at,omitempty"`
	CreatedByThreadID        string     `json:"created_by_thread_id,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	StartedAt                *time.Time `json:"started_at,omitempty"`
	CompletedAt              *time.Time `json:"completed_at,omitempty"`
}

type AgentTaskEvent struct {
	ID        string         `json:"id"`
	TaskID    string         `json:"task_id"`
	AgentID   int64          `json:"agent_id"`
	EventType string         `json:"event_type"`
	ThreadID  string         `json:"thread_id,omitempty"`
	FromState string         `json:"from_state,omitempty"`
	ToState   string         `json:"to_state,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type AgentTaskStep struct {
	TaskID      string     `json:"task_id"`
	StepKey     string     `json:"step_key"`
	AgentID     int64      `json:"agent_id"`
	ThreadID    string     `json:"thread_id"`
	MCPServer   string     `json:"mcp_server"`
	ToolName    string     `json:"tool_name"`
	InputHash   string     `json:"input_hash"`
	State       string     `json:"state"`
	ResultJSON  string     `json:"result_json,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type CreateAgentTaskInput struct {
	AgentID               int64
	ProjectID             string
	Title                 string
	Description           string
	State                 string
	Progress              *int
	CurrentStep           string
	AssignedThreadID      string
	OriginConversationID  string
	OriginMessageID       string
	ParentTaskID          string
	IdempotencyKey        string
	Schedule              *AgentTaskScheduleInput
	ScheduledFor          *time.Time
	ScheduleOccurrenceKey string
	CreatedByThreadID     string
	QueueHandoffDelivery  bool
}

type UpdateAgentTaskInput struct {
	State            *string
	Progress         *int
	ClearProgress    bool
	CurrentStep      *string
	AssignedThreadID *string
	Result           *string
	Error            *string
}

type AgentTaskListFilter struct {
	ProjectID            string
	State                string
	States               []string
	AssignedThread       string
	OriginConversationID string
	Limit                int
}

type AgentTaskCounts struct {
	Active    int `json:"active"`
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Waiting   int `json:"waiting"`
	Blocked   int `json:"blocked"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	Scheduled int `json:"scheduled"`
	Paused    int `json:"paused"`
}

func (s *Store) SetAgentTaskEventHook(hook func(AgentTaskEvent)) {
	s.taskEventMu.Lock()
	s.taskEventHook = hook
	s.taskEventMu.Unlock()
}

func (s *Store) emitAgentTaskEvent(event AgentTaskEvent) {
	s.taskEventMu.RLock()
	hook := s.taskEventHook
	s.taskEventMu.RUnlock()
	if hook != nil {
		hook(event)
	}
}

type taskRowScanner interface {
	Scan(dest ...any) error
}

const agentTaskSelectColumns = `id, agent_id, project_id, title, description, state,
	progress, current_step, assigned_thread_id, origin_conversation_id,
	origin_message_id, parent_task_id, idempotency_key,
	schedule_kind, schedule_expression, schedule_timezone, schedule_enabled,
	schedule_overlap_policy, schedule_catchup_policy, next_run_at, last_run_at,
	scheduled_for, schedule_occurrence_key, result, error,
	handoff_delivery_status, handoff_delivery_error, handoff_delivered_at,
	completion_delivery_status, completion_delivery_error, completion_delivered_at,
	last_activity_at, created_by_thread_id, created_at, updated_at, started_at,
	completed_at`

func scanAgentTask(row taskRowScanner) (*AgentTask, error) {
	var task AgentTask
	var progress sql.NullInt64
	var scheduleEnabled int
	var nextRunAt, lastRunAt, scheduledFor sql.NullString
	var handoffDeliveredAt, deliveredAt, lastActivityAt, startedAt, completedAt sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(
		&task.ID, &task.AgentID, &task.ProjectID, &task.Title, &task.Description,
		&task.State, &progress, &task.CurrentStep, &task.AssignedThreadID,
		&task.OriginConversationID, &task.OriginMessageID, &task.ParentTaskID,
		&task.IdempotencyKey, &task.ScheduleKind, &task.ScheduleExpression,
		&task.ScheduleTimezone, &scheduleEnabled, &task.ScheduleOverlapPolicy,
		&task.ScheduleCatchupPolicy, &nextRunAt, &lastRunAt, &scheduledFor,
		&task.ScheduleOccurrenceKey, &task.Result, &task.Error,
		&task.HandoffDeliveryStatus, &task.HandoffDeliveryError, &handoffDeliveredAt,
		&task.CompletionDeliveryStatus, &task.CompletionDeliveryError, &deliveredAt,
		&lastActivityAt, &task.CreatedByThreadID, &createdAt, &updatedAt, &startedAt,
		&completedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errTaskNotFound
		}
		return nil, err
	}
	if progress.Valid {
		value := int(progress.Int64)
		task.Progress = &value
	}
	task.ScheduleEnabled = scheduleEnabled != 0
	task.NextRunAt = parseOptionalTaskTime(nextRunAt)
	task.LastRunAt = parseOptionalTaskTime(lastRunAt)
	task.ScheduledFor = parseOptionalTaskTime(scheduledFor)
	task.CreatedAt, _ = parseTime(createdAt)
	task.UpdatedAt, _ = parseTime(updatedAt)
	task.HandoffDeliveredAt = parseOptionalTaskTime(handoffDeliveredAt)
	task.CompletionDeliveredAt = parseOptionalTaskTime(deliveredAt)
	task.LastActivityAt = parseOptionalTaskTime(lastActivityAt)
	task.StartedAt = parseOptionalTaskTime(startedAt)
	task.CompletedAt = parseOptionalTaskTime(completedAt)
	return &task, nil
}

func parseOptionalTaskTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil
	}
	return &parsed
}

func validTaskState(state string) bool {
	switch state {
	case taskStateQueued, taskStateRunning, taskStateWaiting, taskStateBlocked,
		taskStateCompleted, taskStateFailed, taskStateCancelled:
		return true
	default:
		return false
	}
}

func taskStateTerminal(state string) bool {
	switch state {
	case taskStateCompleted, taskStateFailed, taskStateCancelled:
		return true
	default:
		return false
	}
}

func validTaskTransition(from, to string) bool {
	if from == to {
		return true
	}
	if taskStateTerminal(from) {
		return false
	}
	switch to {
	case taskStateQueued:
		return from == taskStateWaiting || from == taskStateBlocked
	case taskStateRunning, taskStateWaiting, taskStateBlocked,
		taskStateCompleted, taskStateFailed, taskStateCancelled:
		return true
	default:
		return false
	}
}

func normalizeTaskProgress(progress *int) error {
	if progress != nil && (*progress < 0 || *progress > 100) {
		return errTaskInvalidProgress
	}
	return nil
}

func (s *Store) CreateAgentTask(input CreateAgentTaskInput) (*AgentTask, bool, error) {
	input.Title = strings.TrimSpace(input.Title)
	if input.AgentID <= 0 || input.Title == "" {
		return nil, false, fmt.Errorf("agent_id and title are required")
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.Description = strings.TrimSpace(input.Description)
	input.State = strings.ToLower(strings.TrimSpace(input.State))
	if input.State == "" {
		input.State = taskStateQueued
	}
	if !validTaskState(input.State) {
		return nil, false, fmt.Errorf("%w: %q", errTaskInvalidState, input.State)
	}
	if err := normalizeTaskProgress(input.Progress); err != nil {
		return nil, false, err
	}
	if input.State == taskStateCompleted {
		progress := 100
		input.Progress = &progress
	}
	input.AssignedThreadID = strings.TrimSpace(input.AssignedThreadID)
	if input.AssignedThreadID == "" {
		input.AssignedThreadID = "main"
	}
	input.CreatedByThreadID = strings.TrimSpace(input.CreatedByThreadID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	var schedule *normalizedAgentTaskSchedule
	if input.Schedule != nil {
		normalized, normalizeErr := normalizeAgentTaskSchedule(*input.Schedule, time.Now().UTC())
		if normalizeErr != nil {
			return nil, false, normalizeErr
		}
		schedule = &normalized
		input.State = taskStateWaiting
		input.AssignedThreadID = "main"
		input.Progress = nil
		input.CurrentStep = ""
		input.QueueHandoffDelivery = false
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if input.IdempotencyKey != "" {
		existing, getErr := scanAgentTask(tx.QueryRow(
			`SELECT `+agentTaskSelectColumns+` FROM agent_tasks
			 WHERE agent_id = ? AND idempotency_key = ?`,
			input.AgentID, input.IdempotencyKey,
		))
		if getErr == nil {
			return existing, false, nil
		}
		if !errors.Is(getErr, errTaskNotFound) {
			return nil, false, getErr
		}
	}
	if schedule != nil && strings.TrimSpace(input.ParentTaskID) != "" {
		existing, getErr := scanAgentTask(tx.QueryRow(
			`SELECT `+agentTaskSelectColumns+` FROM agent_tasks
			 WHERE agent_id = ? AND parent_task_id = ? AND schedule_kind <> ''
			   AND state NOT IN ('completed','failed','cancelled')
			 ORDER BY created_at ASC, id ASC LIMIT 1`,
			input.AgentID, strings.TrimSpace(input.ParentTaskID),
		))
		if getErr == nil {
			return existing, false, nil
		}
		if !errors.Is(getErr, errTaskNotFound) {
			return nil, false, getErr
		}
	}

	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	taskID := "task-" + newServerULID()
	var progress any
	if input.Progress != nil {
		progress = *input.Progress
	}
	var startedAt any
	if input.State == taskStateRunning {
		startedAt = nowText
	}
	var completedAt any
	if taskStateTerminal(input.State) {
		completedAt = nowText
	}
	handoffDeliveryStatus := ""
	if schedule == nil && !taskStateTerminal(input.State) && input.AssignedThreadID == "main" &&
		(input.OriginConversationID != "" || input.QueueHandoffDelivery) {
		handoffDeliveryStatus = "pending"
	}
	completionDeliveryStatus := ""
	if taskStateTerminal(input.State) && input.OriginConversationID != "" {
		completionDeliveryStatus = "pending"
	}
	scheduleKind, scheduleExpression, scheduleTimezone := "", "", ""
	scheduleEnabled, overlapPolicy, catchupPolicy := 0, "", ""
	var nextRunAt, scheduledFor any
	if schedule != nil {
		scheduleKind = schedule.Kind
		scheduleExpression = schedule.Expression
		scheduleTimezone = schedule.Timezone
		scheduleEnabled = 1
		overlapPolicy = schedule.OverlapPolicy
		catchupPolicy = schedule.CatchupPolicy
		nextRunAt = schedule.NextRunAt.Format(time.RFC3339Nano)
	}
	if input.ScheduledFor != nil {
		scheduledFor = input.ScheduledFor.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.Exec(`INSERT INTO agent_tasks (
		id, agent_id, project_id, title, description, state, progress, current_step,
		assigned_thread_id, origin_conversation_id, origin_message_id, parent_task_id,
		idempotency_key, schedule_kind, schedule_expression, schedule_timezone,
		schedule_enabled, schedule_overlap_policy, schedule_catchup_policy,
		next_run_at, scheduled_for, schedule_occurrence_key,
		handoff_delivery_status, completion_delivery_status, created_by_thread_id,
		created_at, updated_at, started_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, input.AgentID, input.ProjectID, input.Title, input.Description,
		input.State, progress, strings.TrimSpace(input.CurrentStep),
		input.AssignedThreadID, strings.TrimSpace(input.OriginConversationID),
		strings.TrimSpace(input.OriginMessageID), strings.TrimSpace(input.ParentTaskID),
		input.IdempotencyKey, scheduleKind, scheduleExpression, scheduleTimezone,
		scheduleEnabled, overlapPolicy, catchupPolicy, nextRunAt, scheduledFor,
		strings.TrimSpace(input.ScheduleOccurrenceKey),
		handoffDeliveryStatus, completionDeliveryStatus,
		input.CreatedByThreadID, nowText, nowText, startedAt, completedAt,
	)
	if err != nil {
		if input.IdempotencyKey != "" && strings.Contains(strings.ToLower(err.Error()), "unique") {
			existing, getErr := scanAgentTask(tx.QueryRow(
				`SELECT `+agentTaskSelectColumns+` FROM agent_tasks
				 WHERE agent_id = ? AND idempotency_key = ?`,
				input.AgentID, input.IdempotencyKey,
			))
			if getErr == nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: input.AgentID, EventType: "created",
		ThreadID: input.CreatedByThreadID, ToState: input.State,
		Data: map[string]any{
			"title":                  input.Title,
			"assigned_thread_id":     input.AssignedThreadID,
			"origin_conversation_id": input.OriginConversationID,
			"schedule_kind":          scheduleKind,
			"next_run_at":            nextRunAt,
		},
		CreatedAt: now,
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task, err := s.GetAgentTask(input.AgentID, taskID)
	if err == nil {
		s.emitAgentTaskEvent(event)
	}
	return task, true, err
}

func (s *Store) GetAgentTask(agentID int64, taskID string) (*AgentTask, error) {
	return scanAgentTask(s.db.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		strings.TrimSpace(taskID), agentID,
	))
}

func (s *Store) GetAgentTaskByID(taskID string) (*AgentTask, error) {
	return scanAgentTask(s.db.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ?`,
		strings.TrimSpace(taskID),
	))
}

func (s *Store) ListAgentTasks(agentID int64, filter AgentTaskListFilter) ([]AgentTask, error) {
	if agentID <= 0 {
		return nil, fmt.Errorf("agent_id is required")
	}
	query := `SELECT ` + agentTaskSelectColumns + ` FROM agent_tasks WHERE agent_id = ?`
	args := []any{agentID}
	query, args, err := appendAgentTaskListFilters(query, args, filter)
	if err != nil {
		return nil, err
	}
	return s.queryAgentTasks(query, args, filter.Limit)
}

func (s *Store) ListProjectAgentTasks(projectID string, filter AgentTaskListFilter) ([]AgentTask, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	filter.ProjectID = ""
	query := `SELECT ` + agentTaskSelectColumns + ` FROM agent_tasks WHERE project_id = ?`
	args := []any{projectID}
	query, args, err := appendAgentTaskListFilters(query, args, filter)
	if err != nil {
		return nil, err
	}
	return s.queryAgentTasks(query, args, filter.Limit)
}

// ListVisibleAgentTasks returns tasks from every project visible to a user.
// Project membership remains the authorization source of truth; platform
// administrators inherit the same all-project visibility as ListProjectsForUser.
func (s *Store) ListVisibleAgentTasks(userID int64, filter AgentTaskListFilter) ([]AgentTask, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user_id is required")
	}
	filter.ProjectID = ""
	query := `SELECT ` + agentTaskSelectColumns + ` FROM agent_tasks WHERE project_id IN (
		SELECT p.id FROM projects p
		 WHERE p.id IN (SELECT project_id FROM project_members WHERE user_id = ?)
		    OR (SELECT role FROM users WHERE id = ?) = 'admin'
	)`
	args := []any{userID, userID}
	query, args, err := appendAgentTaskListFilters(query, args, filter)
	if err != nil {
		return nil, err
	}
	return s.queryAgentTasks(query, args, filter.Limit)
}

func appendAgentTaskListFilters(query string, args []any, filter AgentTaskListFilter) (string, []any, error) {
	if value := strings.TrimSpace(filter.ProjectID); value != "" {
		query += ` AND project_id = ?`
		args = append(args, value)
	}
	states := append([]string(nil), filter.States...)
	if value := strings.ToLower(strings.TrimSpace(filter.State)); value != "" {
		states = append(states, value)
	}
	if len(states) > 0 {
		seen := map[string]bool{}
		validated := make([]string, 0, len(states))
		for _, raw := range states {
			value := strings.ToLower(strings.TrimSpace(raw))
			if value == "" || seen[value] {
				continue
			}
			if !validTaskState(value) {
				return "", nil, fmt.Errorf("%w: %q", errTaskInvalidState, value)
			}
			seen[value] = true
			validated = append(validated, value)
		}
		if len(validated) > 0 {
			query += ` AND state IN (` + strings.TrimRight(strings.Repeat("?,", len(validated)), ",") + `)`
			for _, value := range validated {
				args = append(args, value)
			}
		}
	}
	if value := strings.TrimSpace(filter.AssignedThread); value != "" {
		query += ` AND assigned_thread_id = ?`
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.OriginConversationID); value != "" {
		query += ` AND origin_conversation_id = ?`
		args = append(args, value)
	}
	return query, args, nil
}

func (s *Store) queryAgentTasks(query string, args []any, requestedLimit int) ([]AgentTask, error) {
	limit := requestedLimit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
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

func (s *Store) CountProjectAgentTasks(projectID string) (AgentTaskCounts, error) {
	rows, err := s.db.Query(`SELECT state, schedule_kind, schedule_enabled, COUNT(*) FROM agent_tasks
		WHERE project_id = ? GROUP BY state, schedule_kind, schedule_enabled`, strings.TrimSpace(projectID))
	if err != nil {
		return AgentTaskCounts{}, err
	}
	defer rows.Close()
	return scanAgentTaskCounts(rows)
}

func (s *Store) CountVisibleAgentTasks(userID int64) (AgentTaskCounts, error) {
	if userID <= 0 {
		return AgentTaskCounts{}, fmt.Errorf("user_id is required")
	}
	rows, err := s.db.Query(`SELECT state, schedule_kind, schedule_enabled, COUNT(*) FROM agent_tasks
		WHERE project_id IN (
			SELECT p.id FROM projects p
			 WHERE p.id IN (SELECT project_id FROM project_members WHERE user_id = ?)
			    OR (SELECT role FROM users WHERE id = ?) = 'admin'
		)
		GROUP BY state, schedule_kind, schedule_enabled`, userID, userID)
	if err != nil {
		return AgentTaskCounts{}, err
	}
	defer rows.Close()
	return scanAgentTaskCounts(rows)
}

type agentTaskCountRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanAgentTaskCounts(rows agentTaskCountRows) (AgentTaskCounts, error) {
	var counts AgentTaskCounts
	for rows.Next() {
		var state string
		var scheduleKind string
		var scheduleEnabled int
		var count int
		if err := rows.Scan(&state, &scheduleKind, &scheduleEnabled, &count); err != nil {
			return counts, err
		}
		if scheduleKind != "" && !taskStateTerminal(state) {
			if scheduleEnabled != 0 {
				counts.Scheduled += count
			} else {
				counts.Paused += count
			}
			continue
		}
		switch state {
		case taskStateQueued:
			counts.Queued += count
		case taskStateRunning:
			counts.Running += count
		case taskStateWaiting:
			counts.Waiting += count
		case taskStateBlocked:
			counts.Blocked += count
		case taskStateCompleted:
			counts.Completed += count
		case taskStateFailed:
			counts.Failed += count
		case taskStateCancelled:
			counts.Cancelled += count
		}
	}
	counts.Active = counts.Queued + counts.Running + counts.Waiting + counts.Blocked
	return counts, rows.Err()
}

func (s *Store) ListActiveConversationTasks(agentID int64, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE agent_id = ? AND origin_conversation_id <> ''
		  AND assigned_thread_id = 'main'
		  AND schedule_kind = ''
		  AND state NOT IN ('completed','failed','cancelled')
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, agentID, limit)
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

const deletedConversationTaskReason = "Originating conversation was permanently deleted"

// ReconcileAgentTasksForDeletedConversation removes a conversation as a live
// delivery target without discarding durable work that belongs to main, a
// worker, or the scheduler. Work owned by the conversation thread itself
// cannot continue after that thread is killed, so it is cancelled atomically.
// The operation is idempotent because every affected row is detached.
func (s *Store) ReconcileAgentTasksForDeletedConversation(conversationID string) (int, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE origin_conversation_id = ?
		ORDER BY created_at ASC, id ASC`, conversationID)
	if err != nil {
		return 0, err
	}
	tasks := make([]AgentTask, 0)
	for rows.Next() {
		task, scanErr := scanAgentTask(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	events := make([]AgentTaskEvent, 0, len(tasks))
	conversationThreadID := "chat-" + conversationID
	for i := range tasks {
		task := &tasks[i]
		cancel := !taskStateTerminal(task.State) &&
			task.ScheduleKind == "" &&
			task.AssignedThreadID == conversationThreadID
		if cancel {
			if _, err := tx.Exec(`UPDATE agent_tasks
				SET state = ?, error = ?, origin_conversation_id = '',
				    origin_message_id = '', completion_delivery_status = 'discarded',
				    completion_delivery_error = ?, completion_delivered_at = NULL,
				    completed_at = ?, updated_at = ?
				WHERE id = ? AND origin_conversation_id = ?`,
				taskStateCancelled, deletedConversationTaskReason,
				deletedConversationTaskReason, nowText, nowText,
				task.ID, conversationID,
			); err != nil {
				return 0, err
			}
			event := AgentTaskEvent{
				TaskID: task.ID, AgentID: task.AgentID, EventType: "state_changed",
				ThreadID: "server:conversation-delete", FromState: task.State,
				ToState: taskStateCancelled, CreatedAt: now,
				Data: map[string]any{
					"state":                           taskStateCancelled,
					"error":                           deletedConversationTaskReason,
					"origin_detached":                 true,
					"previous_origin_conversation_id": conversationID,
				},
			}
			if err := insertAgentTaskEvent(tx, event); err != nil {
				return 0, err
			}
			events = append(events, event)
			continue
		}

		deliveryStatus := task.CompletionDeliveryStatus
		deliveryError := task.CompletionDeliveryError
		if taskStateTerminal(task.State) {
			deliveryStatus = "discarded"
			deliveryError = deletedConversationTaskReason
		} else {
			deliveryStatus = ""
			deliveryError = ""
		}
		if _, err := tx.Exec(`UPDATE agent_tasks
			SET origin_conversation_id = '', origin_message_id = '',
			    completion_delivery_status = ?, completion_delivery_error = ?,
			    completion_delivered_at = NULL, updated_at = ?
			WHERE id = ? AND origin_conversation_id = ?`,
			deliveryStatus, deliveryError, nowText, task.ID, conversationID,
		); err != nil {
			return 0, err
		}
		event := AgentTaskEvent{
			TaskID: task.ID, AgentID: task.AgentID, EventType: "origin_detached",
			ThreadID: "server:conversation-delete", FromState: task.State,
			ToState: task.State, CreatedAt: now,
			Data: map[string]any{
				"reason":                          deletedConversationTaskReason,
				"previous_origin_conversation_id": conversationID,
			},
		}
		if err := insertAgentTaskEvent(tx, event); err != nil {
			return 0, err
		}
		events = append(events, event)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, event := range events {
		s.emitAgentTaskEvent(event)
	}
	return len(tasks), nil
}

// ReconcileOrphanedConversationTasks repairs rows left by older server
// versions that deleted channel-chat records without detaching task origins.
// channel_chat_chats must exist; startApps calls this after channel-chat mount.
func (s *Store) ReconcileOrphanedConversationTasks() (int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT t.origin_conversation_id
		FROM agent_tasks t
		WHERE t.origin_conversation_id <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM channel_chat_chats c
			WHERE c.id = t.origin_conversation_id
		  )
		ORDER BY t.origin_conversation_id`)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	total := 0
	for _, id := range ids {
		count, reconcileErr := s.ReconcileAgentTasksForDeletedConversation(id)
		if reconcileErr != nil {
			return total, reconcileErr
		}
		total += count
	}
	return total, nil
}

func (s *Store) UpdateAgentTask(agentID int64, taskID, actorThreadID string, input UpdateAgentTaskInput) (*AgentTask, bool, error) {
	if err := normalizeTaskProgress(input.Progress); err != nil {
		return nil, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	current, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		strings.TrimSpace(taskID), agentID,
	))
	if err != nil {
		return nil, false, err
	}

	nextState := current.State
	if input.State != nil {
		nextState = strings.ToLower(strings.TrimSpace(*input.State))
		if !validTaskState(nextState) {
			return nil, false, fmt.Errorf("%w: %q", errTaskInvalidState, nextState)
		}
		if !validTaskTransition(current.State, nextState) {
			return nil, false, fmt.Errorf("%w: %s -> %s", errTaskTerminal, current.State, nextState)
		}
	}
	if current.ScheduleKind != "" && nextState != taskStateCancelled {
		changesScheduleSeries := input.State != nil || input.Progress != nil ||
			input.ClearProgress || input.CurrentStep != nil ||
			input.AssignedThreadID != nil || input.Result != nil || input.Error != nil
		if changesScheduleSeries {
			return nil, false, fmt.Errorf("scheduled task series must be edited, paused, resumed, run, or cancelled through schedule operations")
		}
	}
	if nextState == taskStateCompleted && current.State != taskStateCompleted {
		if input.Progress != nil && *input.Progress != 100 {
			return nil, false, fmt.Errorf("completed task progress must be 100")
		}
		progress := 100
		input.Progress = &progress
		input.ClearProgress = false
		// The active milestone and any transient blocker are no longer current
		// once the task has a successful terminal result. Keep their historical
		// event payloads, but do not leave the task card looking active or
		// blocked after completion.
		if input.CurrentStep == nil && current.CurrentStep != "" {
			cleared := ""
			input.CurrentStep = &cleared
		}
		if input.Error == nil && current.Error != "" {
			cleared := ""
			input.Error = &cleared
		}
	}
	if taskStateTerminal(current.State) {
		changesTerminalTask := nextState != current.State ||
			(input.ClearProgress && current.Progress != nil) ||
			(input.Progress != nil && (current.Progress == nil || *input.Progress != *current.Progress)) ||
			(input.CurrentStep != nil && strings.TrimSpace(*input.CurrentStep) != current.CurrentStep) ||
			(input.AssignedThreadID != nil && strings.TrimSpace(*input.AssignedThreadID) != current.AssignedThreadID) ||
			(input.Result != nil && strings.TrimSpace(*input.Result) != current.Result) ||
			(input.Error != nil && strings.TrimSpace(*input.Error) != current.Error)
		if changesTerminalTask {
			return nil, false, fmt.Errorf("%w: %s", errTaskTerminal, current.State)
		}
	}

	updates := []string{}
	args := []any{}
	changed := map[string]any{}
	if nextState != current.State {
		updates = append(updates, "state = ?")
		args = append(args, nextState)
		changed["state"] = nextState
		if nextState == taskStateRunning && current.StartedAt == nil {
			updates = append(updates, "started_at = ?")
			args = append(args, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if taskStateTerminal(nextState) {
			updates = append(updates, "completed_at = ?")
			args = append(args, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if (nextState == taskStateQueued || nextState == taskStateRunning) &&
			current.Error != "" && input.Error == nil {
			updates = append(updates, "error = ''")
			changed["error"] = ""
		}
		if taskStateTerminal(nextState) && current.OriginConversationID != "" {
			updates = append(updates,
				"completion_delivery_status = 'pending'",
				"completion_delivery_error = ''",
				"completion_delivered_at = NULL",
			)
		}
	}
	if input.ClearProgress {
		if current.Progress != nil {
			updates = append(updates, "progress = NULL")
			changed["progress"] = nil
		}
	} else if input.Progress != nil && (current.Progress == nil || *current.Progress != *input.Progress) {
		updates = append(updates, "progress = ?")
		args = append(args, *input.Progress)
		changed["progress"] = *input.Progress
	}
	if input.CurrentStep != nil && strings.TrimSpace(*input.CurrentStep) != current.CurrentStep {
		value := strings.TrimSpace(*input.CurrentStep)
		updates = append(updates, "current_step = ?")
		args = append(args, value)
		changed["current_step"] = value
	}
	if input.AssignedThreadID != nil && strings.TrimSpace(*input.AssignedThreadID) != current.AssignedThreadID {
		value := strings.TrimSpace(*input.AssignedThreadID)
		if value == "" {
			return nil, false, fmt.Errorf("assigned_thread_id cannot be empty")
		}
		updates = append(updates, "assigned_thread_id = ?")
		args = append(args, value)
		changed["assigned_thread_id"] = value
	}
	if input.Result != nil && strings.TrimSpace(*input.Result) != current.Result {
		value := strings.TrimSpace(*input.Result)
		updates = append(updates, "result = ?")
		args = append(args, value)
		changed["result"] = value
	}
	if input.Error != nil && strings.TrimSpace(*input.Error) != current.Error {
		value := strings.TrimSpace(*input.Error)
		updates = append(updates, "error = ?")
		args = append(args, value)
		changed["error"] = value
	}

	if len(updates) == 0 {
		return current, false, nil
	}
	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now.Format(time.RFC3339Nano), taskID, agentID)
	if _, err := tx.Exec(
		`UPDATE agent_tasks SET `+strings.Join(updates, ", ")+` WHERE id = ? AND agent_id = ?`,
		args...,
	); err != nil {
		return nil, false, err
	}
	eventType := "updated"
	if nextState != current.State {
		eventType = "state_changed"
	}
	if assigned, ok := changed["assigned_thread_id"]; ok && len(changed) == 1 {
		eventType = "assigned"
		changed["previous_thread_id"] = current.AssignedThreadID
		changed["assigned_thread_id"] = assigned
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: eventType,
		ThreadID: strings.TrimSpace(actorThreadID), FromState: current.State,
		ToState: nextState, Data: changed, CreatedAt: now,
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
		if taskStateTerminal(task.State) {
			if finalizeErr := s.finalizeOneTimeScheduleFromOccurrence(task, actorThreadID); finalizeErr != nil {
				return task, true, finalizeErr
			}
		}
	}
	return task, true, err
}

func (s *Store) finalizeOneTimeScheduleFromOccurrence(occurrence *AgentTask, actorThreadID string) error {
	if occurrence == nil || occurrence.ParentTaskID == "" ||
		occurrence.ScheduleOccurrenceKey == "" || !taskStateTerminal(occurrence.State) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parent, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks
		 WHERE id = ? AND agent_id = ?`,
		occurrence.ParentTaskID, occurrence.AgentID,
	))
	if err != nil {
		if errors.Is(err, errTaskNotFound) {
			return nil
		}
		return err
	}
	if parent.ScheduleKind != taskScheduleOnce || taskStateTerminal(parent.State) {
		return nil
	}
	now := time.Now().UTC()
	var progress any
	if occurrence.State == taskStateCompleted {
		progress = 100
	}
	if _, err := tx.Exec(`UPDATE agent_tasks
		SET state = ?, progress = ?, current_step = '', result = ?, error = ?,
		    schedule_enabled = 0, next_run_at = NULL, completed_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		occurrence.State, progress, occurrence.Result, occurrence.Error,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		parent.ID, parent.AgentID,
	); err != nil {
		return err
	}
	event := AgentTaskEvent{
		TaskID: parent.ID, AgentID: parent.AgentID, EventType: "state_changed",
		ThreadID: strings.TrimSpace(actorThreadID), FromState: parent.State,
		ToState: occurrence.State, CreatedAt: now,
		Data: map[string]any{
			"state": occurrence.State, "progress": progress,
			"result": occurrence.Result, "error": occurrence.Error,
			"occurrence_task_id": occurrence.ID,
		},
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.emitAgentTaskEvent(event)
	return nil
}

func (s *Store) CompleteAgentTask(agentID int64, taskID, actorThreadID, result string) (*AgentTask, bool, error) {
	state := taskStateCompleted
	result = strings.TrimSpace(result)
	current, err := s.GetAgentTask(agentID, taskID)
	if err != nil {
		return nil, false, err
	}
	if current.ScheduleKind != "" {
		return nil, false, fmt.Errorf("scheduled task series cannot be completed; pause or cancel it instead")
	}
	if current.State == taskStateCompleted && current.Result == result {
		return current, false, nil
	}
	return s.UpdateAgentTask(agentID, taskID, actorThreadID, UpdateAgentTaskInput{
		State: &state, Result: &result,
	})
}

func (s *Store) CancelAgentTask(agentID int64, taskID, actorThreadID, reason string) (*AgentTask, bool, error) {
	state := taskStateCancelled
	reason = strings.TrimSpace(reason)
	current, err := s.GetAgentTask(agentID, taskID)
	if err != nil {
		return nil, false, err
	}
	if current.State == taskStateCancelled && (reason == "" || current.Error == reason) {
		return current, false, nil
	}
	task, changed, err := s.UpdateAgentTask(agentID, taskID, actorThreadID, UpdateAgentTaskInput{
		State: &state, Error: &reason,
	})
	if err != nil || !changed || current.ScheduleKind == "" {
		return task, changed, err
	}
	if _, err := s.db.Exec(`UPDATE agent_tasks
		SET schedule_enabled = 0, next_run_at = NULL
		WHERE id = ? AND agent_id = ?`, taskID, agentID); err != nil {
		return task, changed, err
	}
	task, err = s.GetAgentTask(agentID, taskID)
	return task, changed, err
}

func (s *Store) MarkAgentTaskHandoffDelivery(agentID int64, taskID, status, detail string) error {
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "delivered" && status != "failed" {
		return fmt.Errorf("invalid handoff delivery status %q", status)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		taskID, agentID,
	))
	if err != nil {
		return err
	}
	var deliveredAt any
	if status == "delivered" {
		deliveredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE agent_tasks
		SET handoff_delivery_status = ?, handoff_delivery_error = ?,
		    handoff_delivered_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		status, strings.TrimSpace(detail), deliveredAt, now.Format(time.RFC3339Nano),
		taskID, agentID,
	); err != nil {
		return err
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: "handoff_delivery_" + status,
		ThreadID: "main", FromState: task.State, ToState: task.State,
		Data: map[string]any{
			"detail":                 strings.TrimSpace(detail),
			"origin_conversation_id": task.OriginConversationID,
		},
		CreatedAt: now,
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.emitAgentTaskEvent(event)
	return nil
}

func (s *Store) ClaimAgentTaskHandoffNudge(agentID int64, taskID string) (*AgentTask, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.Exec(`UPDATE agent_tasks
		SET handoff_nudge_count = handoff_nudge_count + 1, updated_at = ?
		WHERE id = ? AND agent_id = ?
		  AND state = 'queued'
		  AND assigned_thread_id = 'main'
		  AND handoff_delivery_status = 'delivered'
		  AND handoff_nudge_count = 0`,
		now.Format(time.RFC3339Nano), taskID, agentID,
	)
	if err != nil {
		return nil, false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		task, getErr := scanAgentTask(tx.QueryRow(
			`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
			taskID, agentID,
		))
		if getErr != nil {
			return nil, false, getErr
		}
		return task, false, nil
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: "handoff_nudge_claimed",
		ThreadID: "main", Data: map[string]any{"nudge": 1}, CreatedAt: now,
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

func (s *Store) ReleaseAgentTaskHandoffNudge(agentID int64, taskID string) error {
	_, err := s.db.Exec(`UPDATE agent_tasks
		SET handoff_nudge_count = CASE
			WHEN handoff_nudge_count > 0 THEN handoff_nudge_count - 1
			ELSE 0 END
		WHERE id = ? AND agent_id = ? AND state = 'queued'`,
		taskID, agentID,
	)
	return err
}

func (s *Store) MarkAgentTaskCompletionDelivery(agentID int64, taskID, status, detail string) error {
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "delivered" && status != "failed" {
		return fmt.Errorf("invalid completion delivery status %q", status)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanAgentTask(tx.QueryRow(
		`SELECT `+agentTaskSelectColumns+` FROM agent_tasks WHERE id = ? AND agent_id = ?`,
		taskID, agentID,
	))
	if err != nil {
		return err
	}
	var deliveredAt any
	if status == "delivered" {
		deliveredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE agent_tasks
		SET completion_delivery_status = ?, completion_delivery_error = ?,
		    completion_delivered_at = ?, updated_at = ?
		WHERE id = ? AND agent_id = ?`,
		status, strings.TrimSpace(detail), deliveredAt, now.Format(time.RFC3339Nano),
		taskID, agentID,
	); err != nil {
		return err
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: "completion_delivery_" + status,
		ThreadID: task.AssignedThreadID, FromState: task.State, ToState: task.State,
		Data: map[string]any{"detail": strings.TrimSpace(detail)}, CreatedAt: now,
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.emitAgentTaskEvent(event)
	return nil
}

func (s *Store) TouchAgentTasksByThread(agentID int64, threadID string, at time.Time) error {
	threadID = strings.TrimSpace(threadID)
	if agentID <= 0 || threadID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE agent_tasks SET last_activity_at = ?
		WHERE agent_id = ? AND assigned_thread_id = ?
		  AND state IN ('queued','running','waiting','blocked')`,
		at.UTC().Format(time.RFC3339Nano), agentID, threadID,
	)
	return err
}

func (s *Store) ListAgentTaskEvents(agentID int64, taskID string) ([]AgentTaskEvent, error) {
	if _, err := s.GetAgentTask(agentID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, task_id, agent_id, event_type, thread_id,
		from_state, to_state, data, created_at
		FROM agent_task_events WHERE task_id = ? AND agent_id = ?
		ORDER BY created_at ASC, id ASC`, taskID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]AgentTaskEvent, 0)
	for rows.Next() {
		var event AgentTaskEvent
		var rawData, createdAt string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.AgentID,
			&event.EventType, &event.ThreadID, &event.FromState, &event.ToState,
			&rawData, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt, _ = parseTime(createdAt)
		_ = json.Unmarshal([]byte(rawData), &event.Data)
		events = append(events, event)
	}
	return events, rows.Err()
}

const agentTaskStepSelectColumns = `task_id, step_key, agent_id, thread_id,
	mcp_server, tool_name, input_hash, state, result_json, error, created_at,
	updated_at, completed_at`

func scanAgentTaskStep(row taskRowScanner) (*AgentTaskStep, error) {
	var step AgentTaskStep
	var createdAt, updatedAt string
	var completedAt sql.NullString
	if err := row.Scan(
		&step.TaskID, &step.StepKey, &step.AgentID, &step.ThreadID,
		&step.MCPServer, &step.ToolName, &step.InputHash, &step.State,
		&step.ResultJSON, &step.Error, &createdAt, &updatedAt, &completedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errTaskNotFound
		}
		return nil, err
	}
	step.CreatedAt, _ = parseTime(createdAt)
	step.UpdatedAt, _ = parseTime(updatedAt)
	step.CompletedAt = parseOptionalTaskTime(completedAt)
	return &step, nil
}

func (s *Store) ListAgentTaskSteps(agentID int64, taskID string) ([]AgentTaskStep, error) {
	if _, err := s.GetAgentTask(agentID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT `+agentTaskStepSelectColumns+`
		FROM agent_task_steps WHERE task_id = ? AND agent_id = ?
		ORDER BY created_at ASC, step_key ASC`, strings.TrimSpace(taskID), agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := make([]AgentTaskStep, 0)
	for rows.Next() {
		step, scanErr := scanAgentTaskStep(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		steps = append(steps, *step)
	}
	return steps, rows.Err()
}

func (s *Store) ClaimAgentTaskStep(
	agentID int64,
	taskID, stepKey, threadID, mcpServer, toolName, inputHash string,
) (*AgentTaskStep, bool, error) {
	taskID = strings.TrimSpace(taskID)
	stepKey = strings.TrimSpace(stepKey)
	threadID = strings.TrimSpace(threadID)
	mcpServer = strings.TrimSpace(mcpServer)
	toolName = strings.TrimSpace(toolName)
	inputHash = strings.TrimSpace(inputHash)
	if agentID <= 0 || taskID == "" || stepKey == "" || threadID == "" ||
		mcpServer == "" || toolName == "" || inputHash == "" {
		return nil, false, fmt.Errorf("complete task step identity is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	result, err := tx.Exec(`INSERT OR IGNORE INTO agent_task_steps (
		task_id, step_key, agent_id, thread_id, mcp_server, tool_name, input_hash,
		state, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?)`,
		taskID, stepKey, agentID, threadID, mcpServer, toolName, inputHash,
		nowText, nowText,
	)
	if err != nil {
		return nil, false, err
	}
	inserted, _ := result.RowsAffected()
	step, err := scanAgentTaskStep(tx.QueryRow(
		`SELECT `+agentTaskStepSelectColumns+` FROM agent_task_steps
		 WHERE task_id = ? AND agent_id = ?
		   AND (step_key = ? OR input_hash = ?)
		 ORDER BY CASE WHEN step_key = ? THEN 0 ELSE 1 END
		 LIMIT 1`,
		taskID, agentID, stepKey, inputHash, stepKey,
	))
	if err != nil {
		return nil, false, err
	}
	if step.InputHash != inputHash || step.MCPServer != mcpServer ||
		step.ToolName != toolName {
		return nil, false, fmt.Errorf(
			"task step %q already exists with different tool or arguments", stepKey,
		)
	}
	var event AgentTaskEvent
	if inserted > 0 {
		event = AgentTaskEvent{
			TaskID: taskID, AgentID: agentID, EventType: "step_started",
			ThreadID: threadID,
			Data: map[string]any{
				"step_key": stepKey, "mcp_server": mcpServer,
				"tool_name": toolName, "input_hash": inputHash,
			},
			CreatedAt: now,
		}
		if err := insertAgentTaskEvent(tx, event); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	if inserted > 0 {
		s.emitAgentTaskEvent(event)
	}
	return step, inserted > 0, nil
}

func (s *Store) FinishAgentTaskStep(
	agentID int64,
	taskID, stepKey, threadID, resultJSON, stepError string,
) (*AgentTaskStep, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	step, err := scanAgentTaskStep(tx.QueryRow(
		`SELECT `+agentTaskStepSelectColumns+` FROM agent_task_steps
		 WHERE task_id = ? AND step_key = ? AND agent_id = ?`,
		taskID, stepKey, agentID,
	))
	if err != nil {
		return nil, err
	}
	if step.State != "running" {
		return step, nil
	}
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	state := "completed"
	eventType := "step_completed"
	if strings.TrimSpace(stepError) != "" {
		state = "failed"
		eventType = "step_failed"
	}
	if _, err := tx.Exec(`UPDATE agent_task_steps
		SET state = ?, result_json = ?, error = ?, updated_at = ?, completed_at = ?
		WHERE task_id = ? AND step_key = ? AND agent_id = ? AND state = 'running'`,
		state, resultJSON, strings.TrimSpace(stepError), nowText, nowText,
		taskID, stepKey, agentID,
	); err != nil {
		return nil, err
	}
	data := map[string]any{"step_key": stepKey}
	if strings.TrimSpace(stepError) != "" {
		data["error"] = strings.TrimSpace(stepError)
	}
	event := AgentTaskEvent{
		TaskID: taskID, AgentID: agentID, EventType: eventType,
		ThreadID: threadID, Data: data, CreatedAt: now,
	}
	if err := insertAgentTaskEvent(tx, event); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	finished, err := scanAgentTaskStep(s.db.QueryRow(
		`SELECT `+agentTaskStepSelectColumns+` FROM agent_task_steps
		 WHERE task_id = ? AND step_key = ? AND agent_id = ?`,
		taskID, stepKey, agentID,
	))
	if err == nil {
		s.emitAgentTaskEvent(event)
	}
	return finished, err
}

func (s *Store) ListUndeliveredTerminalAgentTasks(agentID int64, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE agent_id = ?
		  AND origin_conversation_id <> ''
		  AND state IN ('completed','failed','cancelled')
		  AND completion_delivery_status <> 'delivered'
		ORDER BY updated_at ASC, id ASC
		LIMIT ?`, agentID, limit)
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

func (s *Store) ListUndeliveredAgentTaskHandoffs(agentID int64, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE agent_id = ?
		  AND assigned_thread_id = 'main'
		  AND state IN ('queued','running','waiting','blocked')
		  AND handoff_delivery_status IN ('pending','failed')
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, agentID, limit)
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

func (s *Store) ListQueuedDeliveredAgentTaskHandoffs(agentID int64, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+agentTaskSelectColumns+`
		FROM agent_tasks
		WHERE agent_id = ?
		  AND assigned_thread_id = 'main'
		  AND state = 'queued'
		  AND handoff_delivery_status = 'delivered'
		  AND handoff_nudge_count = 0
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, agentID, limit)
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

func insertAgentTaskEvent(tx *sql.Tx, event AgentTaskEvent) error {
	if event.ID == "" {
		event.ID = "task-event-" + newServerULID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.Data == nil {
		event.Data = map[string]any{}
	}
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO agent_task_events (
		id, task_id, agent_id, event_type, thread_id, from_state, to_state, data,
		created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.TaskID, event.AgentID, event.EventType,
		strings.TrimSpace(event.ThreadID), event.FromState, event.ToState,
		string(data), event.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func parseAgentTaskID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "task-") {
		return value
	}
	return ""
}

func parseAgentTaskLimit(value string) int {
	parsed, _ := strconv.Atoi(strings.TrimSpace(value))
	if parsed <= 0 {
		return 100
	}
	if parsed > 500 {
		return 500
	}
	return parsed
}

func sortedTaskStates() []string {
	states := []string{
		taskStateQueued, taskStateRunning, taskStateWaiting, taskStateBlocked,
		taskStateCompleted, taskStateFailed, taskStateCancelled,
	}
	sort.Strings(states)
	return states
}
