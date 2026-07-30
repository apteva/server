package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) requireTaskAgentAccess(w http.ResponseWriter, r *http.Request, agentID int64, need ProjectRole) (*Agent, bool) {
	agent, err := s.store.GetAgentByID(agentID)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return nil, false
	}
	if _, _, ok := s.requireProjectAccess(w, r, agent.ProjectID, need); !ok {
		return nil, false
	}
	return agent, true
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !taskTrackingEnabled() {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		agentID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("agent_id")), 10, 64)
		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		allProjects := r.URL.Query().Get("all") == "1"
		if allProjects && (agentID > 0 || projectID != "") {
			http.Error(w, "all cannot be combined with agent_id or project_id", http.StatusBadRequest)
			return
		}
		filter := AgentTaskListFilter{
			State:                strings.TrimSpace(r.URL.Query().Get("state")),
			States:               splitAgentTaskStates(r.URL.Query().Get("states")),
			AssignedThread:       strings.TrimSpace(r.URL.Query().Get("assigned_thread_id")),
			OriginConversationID: strings.TrimSpace(r.URL.Query().Get("origin_conversation_id")),
			Limit:                parseAgentTaskLimit(r.URL.Query().Get("limit")),
		}
		var (
			tasks  []AgentTask
			counts *AgentTaskCounts
			err    error
		)
		if agentID > 0 {
			agent, ok := s.requireTaskAgentAccess(w, r, agentID, ProjectViewer)
			if !ok {
				return
			}
			filter.ProjectID = projectID
			tasks, err = s.store.ListAgentTasks(agentID, filter)
			if projectID == "" {
				projectID = agent.ProjectID
			}
		} else if allProjects {
			userID := getUserID(r)
			if userID <= 0 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			tasks, err = s.store.ListVisibleAgentTasks(userID, filter)
			if err == nil {
				value, countErr := s.store.CountVisibleAgentTasks(userID)
				if countErr != nil {
					err = countErr
				} else {
					counts = &value
				}
			}
		} else {
			if projectID == "" {
				http.Error(w, "agent_id, project_id, or all=1 is required", http.StatusBadRequest)
				return
			}
			if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
				return
			}
			tasks, err = s.store.ListProjectAgentTasks(projectID, filter)
			if err == nil {
				value, countErr := s.store.CountProjectAgentTasks(projectID)
				if countErr != nil {
					err = countErr
				} else {
					counts = &value
				}
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := map[string]any{
			"tasks": tasks, "enabled": true,
			"scheduling_enabled": taskSchedulingEnabled(),
		}
		if counts != nil {
			response["counts"] = counts
		}
		writeJSON(w, response)

	case http.MethodPost:
		var body struct {
			AgentID              int64                   `json:"agent_id"`
			Title                string                  `json:"title"`
			Description          string                  `json:"description"`
			State                string                  `json:"state"`
			Progress             *int                    `json:"progress"`
			CurrentStep          string                  `json:"current_step"`
			AssignedThreadID     string                  `json:"assigned_thread_id"`
			OriginConversationID string                  `json:"origin_conversation_id"`
			OriginMessageID      string                  `json:"origin_message_id"`
			ParentTaskID         string                  `json:"parent_task_id"`
			IdempotencyKey       string                  `json:"idempotency_key"`
			Schedule             *AgentTaskScheduleInput `json:"schedule"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		agent, ok := s.requireTaskAgentAccess(w, r, body.AgentID, ProjectEditor)
		if !ok {
			return
		}
		if body.Schedule != nil && !taskSchedulingEnabled() {
			http.Error(w, "task scheduling is disabled", http.StatusConflict)
			return
		}
		if strings.TrimSpace(body.ParentTaskID) != "" ||
			strings.TrimSpace(body.OriginConversationID) != "" ||
			strings.TrimSpace(body.OriginMessageID) != "" {
			http.Error(w, "task parent and origin fields are server-managed", http.StatusBadRequest)
			return
		}
		if assigned := strings.TrimSpace(body.AssignedThreadID); assigned != "" && assigned != "main" {
			http.Error(w, "new operator tasks must begin on main", http.StatusBadRequest)
			return
		}
		if state := strings.TrimSpace(body.State); state != "" && state != taskStateQueued {
			http.Error(w, "new operator tasks must begin queued", http.StatusBadRequest)
			return
		}
		if body.Progress != nil || strings.TrimSpace(body.CurrentStep) != "" {
			http.Error(w, "new operator task progress is recorded by the assigned thread", http.StatusBadRequest)
			return
		}
		task, created, err := s.store.CreateAgentTask(CreateAgentTaskInput{
			AgentID: body.AgentID, ProjectID: agent.ProjectID,
			Title:                body.Title,
			Description:          body.Description,
			State:                taskStateQueued,
			AssignedThreadID:     "main",
			IdempotencyKey:       body.IdempotencyKey,
			Schedule:             body.Schedule,
			CreatedByThreadID:    fmt.Sprintf("api:user:%d", getUserID(r)),
			QueueHandoffDelivery: true,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if task.AssignedThreadID == "main" && !taskStateTerminal(task.State) &&
			task.HandoffDeliveryStatus != "" {
			if err := deliverAndRecordTaskHandoff(r.Context(), s.store, task, s.deliverTaskHandoff); err != nil {
				task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
				if task.OriginConversationID == "" {
					if created {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusCreated)
					}
					writeJSON(w, map[string]any{
						"task": task, "created": created,
						"delivery_warning": "task saved and will be delivered when the agent is available: " + err.Error(),
					})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				writeJSON(w, map[string]any{
					"task": task, "created": created,
					"error": "task created but automatic main handoff failed: " + err.Error(),
				})
				return
			}
			task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
		}
		if taskStateTerminal(task.State) && task.OriginConversationID != "" {
			if err := deliverAndRecordTaskCompletion(r.Context(), s.store, task, s.deliverTaskCompletion); err != nil {
				task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				writeJSON(w, map[string]any{
					"task": task, "created": created,
					"error": "task completed but origin delivery failed: " + err.Error(),
				})
				return
			}
			task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
		}
		if created {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, map[string]any{"task": task, "created": created})

	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	if !taskTrackingEnabled() {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimSpace(r.URL.Path)
	path = strings.TrimPrefix(path, "/api")
	path = strings.Trim(strings.TrimPrefix(path, "/tasks/"), "/")
	parts := strings.Split(path, "/")
	taskID := parseAgentTaskID(parts[0])
	if taskID == "" {
		http.NotFound(w, r)
		return
	}
	task, err := s.store.GetAgentTaskByID(taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	need := ProjectViewer
	if r.Method != http.MethodGet {
		need = ProjectEditor
	}
	if _, ok := s.requireTaskAgentAccess(w, r, task.AgentID, need); !ok {
		return
	}

	if len(parts) == 2 {
		switch parts[1] {
		case "events":
			if r.Method != http.MethodGet {
				http.Error(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			events, err := s.store.ListAgentTaskEvents(task.AgentID, task.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"events": events})
			return
		case "steps":
			if r.Method != http.MethodGet {
				http.Error(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			steps, err := s.store.ListAgentTaskSteps(task.AgentID, task.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			type safeTaskStep struct {
				TaskID      string `json:"task_id"`
				StepKey     string `json:"step_key"`
				ThreadID    string `json:"thread_id"`
				MCPServer   string `json:"mcp_server"`
				ToolName    string `json:"tool_name"`
				State       string `json:"state"`
				Error       string `json:"error,omitempty"`
				CreatedAt   any    `json:"created_at"`
				UpdatedAt   any    `json:"updated_at"`
				CompletedAt any    `json:"completed_at,omitempty"`
			}
			safe := make([]safeTaskStep, 0, len(steps))
			for _, step := range steps {
				safe = append(safe, safeTaskStep{
					TaskID: step.TaskID, StepKey: step.StepKey,
					ThreadID: step.ThreadID, MCPServer: step.MCPServer,
					ToolName: step.ToolName, State: step.State, Error: step.Error,
					CreatedAt: step.CreatedAt, UpdatedAt: step.UpdatedAt,
					CompletedAt: step.CompletedAt,
				})
			}
			writeJSON(w, map[string]any{"steps": safe})
			return
		case "runs":
			if r.Method != http.MethodGet {
				http.Error(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			runs, err := s.store.ListAgentTaskRuns(task.AgentID, task.ID, 100)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"runs": runs})
			return
		case "schedule":
			if r.Method != http.MethodPatch {
				http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
				return
			}
			if !taskSchedulingEnabled() {
				http.Error(w, "task scheduling is disabled", http.StatusConflict)
				return
			}
			var body AgentTaskScheduleInput
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			updated, changed, err := s.store.UpdateAgentTaskSchedule(
				task.AgentID, task.ID, fmt.Sprintf("api:user:%d", getUserID(r)),
				body, time.Now().UTC(),
			)
			if err != nil {
				writeTaskHTTPError(w, err)
				return
			}
			writeJSON(w, map[string]any{"task": updated, "changed": changed})
			return
		case "pause", "resume":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			if !taskSchedulingEnabled() {
				http.Error(w, "task scheduling is disabled", http.StatusConflict)
				return
			}
			updated, changed, err := s.store.SetAgentTaskScheduleEnabled(
				task.AgentID, task.ID, fmt.Sprintf("api:user:%d", getUserID(r)),
				parts[1] == "resume", time.Now().UTC(),
			)
			if err != nil {
				writeTaskHTTPError(w, err)
				return
			}
			writeJSON(w, map[string]any{"task": updated, "changed": changed})
			return
		case "run-now":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			if !taskSchedulingEnabled() {
				http.Error(w, "task scheduling is disabled", http.StatusConflict)
				return
			}
			var body struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			run, created, err := s.store.RunAgentTaskScheduleNow(
				task.AgentID, task.ID, fmt.Sprintf("api:user:%d", getUserID(r)),
				body.IdempotencyKey,
				time.Now().UTC(),
			)
			if err != nil {
				writeTaskHTTPError(w, err)
				return
			}
			if err := deliverAndRecordTaskHandoff(r.Context(), s.store, run, s.deliverTaskHandoff); err != nil {
				run, _ = s.store.GetAgentTask(run.AgentID, run.ID)
				writeJSON(w, map[string]any{
					"task": run, "created": created,
					"delivery_warning": "run saved and will be delivered when the agent is available: " + err.Error(),
				})
				return
			}
			run, _ = s.store.GetAgentTask(run.AgentID, run.ID)
			writeJSON(w, map[string]any{"task": run, "created": created})
			return
		case "cancel":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Reason string `json:"reason"`
			}
			if r.Body != nil {
				_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
			}
			task, changed, err := s.store.CancelAgentTask(
				task.AgentID, task.ID, fmt.Sprintf("api:user:%d", getUserID(r)), body.Reason,
			)
			if err != nil {
				writeTaskHTTPError(w, err)
				return
			}
			if task.OriginConversationID != "" {
				if err := deliverAndRecordTaskCompletion(r.Context(), s.store, task, s.deliverTaskCompletion); err != nil {
					task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					writeJSON(w, map[string]any{
						"task": task, "changed": changed,
						"error": "task cancelled but delivery failed: " + err.Error(),
					})
					return
				}
				task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
			}
			writeJSON(w, map[string]any{"task": task, "changed": changed})
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"task": task})
	case http.MethodPatch:
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		var input UpdateAgentTaskInput
		if value, ok := raw["state"]; ok {
			var decoded string
			if json.Unmarshal(value, &decoded) != nil {
				http.Error(w, "state must be a string", http.StatusBadRequest)
				return
			}
			input.State = &decoded
		}
		if value, ok := raw["progress"]; ok {
			if string(value) == "null" {
				input.ClearProgress = true
			} else {
				var decoded int
				if json.Unmarshal(value, &decoded) != nil {
					http.Error(w, "progress must be an integer or null", http.StatusBadRequest)
					return
				}
				input.Progress = &decoded
			}
		}
		decodeTaskPatchString := func(key string, target **string) bool {
			value, ok := raw[key]
			if !ok {
				return true
			}
			var decoded string
			if json.Unmarshal(value, &decoded) != nil {
				http.Error(w, key+" must be a string", http.StatusBadRequest)
				return false
			}
			*target = &decoded
			return true
		}
		if !decodeTaskPatchString("current_step", &input.CurrentStep) ||
			!decodeTaskPatchString("assigned_thread_id", &input.AssignedThreadID) ||
			!decodeTaskPatchString("result", &input.Result) ||
			!decodeTaskPatchString("error", &input.Error) {
			return
		}
		task, changed, err := s.store.UpdateAgentTask(
			task.AgentID, task.ID, fmt.Sprintf("api:user:%d", getUserID(r)), input,
		)
		if err != nil {
			writeTaskHTTPError(w, err)
			return
		}
		if taskStateTerminal(task.State) && task.OriginConversationID != "" {
			if err := deliverAndRecordTaskCompletion(r.Context(), s.store, task, s.deliverTaskCompletion); err != nil {
				task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				writeJSON(w, map[string]any{
					"task": task, "changed": changed,
					"error": "task completed but origin delivery failed: " + err.Error(),
				})
				return
			}
			task, _ = s.store.GetAgentTask(task.AgentID, task.ID)
		}
		writeJSON(w, map[string]any{"task": task, "changed": changed})
	default:
		http.Error(w, "GET or PATCH", http.StatusMethodNotAllowed)
	}
}

func splitAgentTaskStates(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	states := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			states = append(states, value)
		}
	}
	return states
}

func writeTaskHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTaskNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errTaskTerminal):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
