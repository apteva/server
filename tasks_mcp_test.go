package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func callTaskMCP(t *testing.T, server *taskMCPServer, name string, arguments map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := server.handleToolCall(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("RPC error: %+v", rpcErr)
	}
	decoded, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	return decoded
}

func taskMCPTextJSON(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	content, ok := result["content"].([]map[string]string)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected MCP content: %#v", result)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(content[0]["text"]), &decoded); err != nil {
		t.Fatalf("decode MCP text %q: %v", content[0]["text"], err)
	}
	return decoded
}

func taskIDFromMCPResult(t *testing.T, result map[string]any) string {
	t.Helper()
	decoded := taskMCPTextJSON(t, result)
	task, ok := decoded["task"].(map[string]any)
	if !ok {
		t.Fatalf("task missing from result: %#v", decoded)
	}
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatalf("task id missing: %#v", task)
	}
	return id
}

func TestTaskMCPProfilesExposeSeparateAuthority(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	tests := []struct {
		profile taskMCPProfile
		want    []string
		deny    []string
	}{
		{
			profile: taskMCPProfileMain,
			want:    []string{"task_create", "task_list", "task_get", "task_assign", "task_spawn_worker", "task_run_step", "task_update", "task_complete", "task_cancel", "task_pause", "task_resume", "task_run_now"},
		},
		{
			profile: taskMCPProfileConversation,
			want:    []string{"task_create", "task_get", "task_update", "task_run_step", "task_complete", "task_cancel"},
			deny:    []string{"task_list", "task_assign"},
		},
		{
			profile: taskMCPProfileWorker,
			want:    []string{"task_get", "task_update", "task_run_step", "task_complete"},
			deny:    []string{"task_create", "task_list", "task_assign", "task_cancel"},
		},
	}
	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			server := &taskMCPServer{store: store, agent: agent, profile: test.profile}
			names := map[string]bool{}
			for _, tool := range server.tools() {
				name, _ := tool["name"].(string)
				names[name] = true
			}
			for _, name := range test.want {
				if !names[name] {
					t.Errorf("missing %s", name)
				}
			}
			for _, name := range test.deny {
				if names[name] {
					t.Errorf("unexpected %s", name)
				}
			}
		})
	}
}

func TestTaskSchedulingDisableSwitchRemovesModelVisibleScheduleControls(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "0")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	server := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileMain}
	for _, tool := range server.tools() {
		name, _ := tool["name"].(string)
		switch name {
		case "task_pause", "task_resume", "task_run_now":
			t.Fatalf("disabled scheduling exposed %s", name)
		case "task_create", "task_update":
			schema, _ := tool["inputSchema"].(map[string]any)
			properties, _ := schema["properties"].(map[string]any)
			if _, ok := properties["schedule"]; ok {
				t.Fatalf("disabled scheduling exposed schedule on %s", name)
			}
		}
	}
	disabledCall := callTaskMCP(t, server, "task_run_now", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                "task-disabled",
	})
	if isError, _ := disabledCall["isError"].(bool); !isError {
		t.Fatalf("hidden scheduling operation remained callable: %#v", disabledCall)
	}
}

func TestTaskMCPDescriptionsReinforceResumeAndConversationCancellation(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
	}
	descriptions := map[string]string{}
	for _, tool := range server.tools() {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		descriptions[name] = description
	}
	for name, required := range map[string]string{
		"task_get":      "already successful work is not repeated",
		"task_complete": "successful receipt ends this assignment",
		"task_cancel":   "call this tool directly here even if main or a worker is assigned",
	} {
		if !strings.Contains(descriptions[name], required) {
			t.Fatalf("%s description missing %q: %q", name, required, descriptions[name])
		}
	}
}

func TestTaskGetReturnsRecentHistoryAndContinuationGuidance(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Recoverable staged work",
		AssignedThreadID: "worker-a", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	running, progress, step := taskStateRunning, 40, "Stage one completed"
	if _, _, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-a", UpdateAgentTaskInput{
		State: &running, Progress: &progress, CurrentStep: &step,
	}); err != nil {
		t.Fatal(err)
	}
	server := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileWorker}
	result := callTaskMCP(t, server, "task_get", map[string]any{
		"_apteva_caller_context": "worker-a", "task_id": task.ID,
	})
	decoded := taskMCPTextJSON(t, result)
	events, ok := decoded["recent_events"].([]any)
	if !ok || len(events) < 2 {
		t.Fatalf("task_get recent_events=%#v, want creation and progress history", decoded["recent_events"])
	}
	guidance, _ := decoded["guidance"].(string)
	if !strings.Contains(guidance, "do not repeat") {
		t.Fatalf("task_get guidance=%q, want no-repeat recovery instruction", guidance)
	}
}

func TestWorkerTaskRunStepExecutesDownstreamExactlyOnce(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Three-stage audit",
		AssignedThreadID: "worker-a", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileWorker,
		executeStep: func(_ context.Context, mcpServer, tool string, arguments map[string]any) (json.RawMessage, error) {
			executions++
			if mcpServer != "audit" || tool != "run_stage" || arguments["stage"] != float64(1) {
				t.Fatalf("unexpected downstream call server=%q tool=%q arguments=%#v", mcpServer, tool, arguments)
			}
			return json.RawMessage(`{"content":[{"type":"text","text":"stage one complete"}]}`), nil
		},
	}
	arguments := map[string]any{
		"_apteva_caller_context": "worker-a",
		"task_id":                task.ID, "step_key": "inventory",
		"step_index": 1, "step_count": 3,
		"mcp_server": "audit", "tool": "run_stage",
		"arguments": map[string]any{"stage": 1},
	}
	first := callTaskMCP(t, server, "task_run_step", arguments)
	if isError, _ := first["isError"].(bool); isError {
		t.Fatalf("first task step failed: %#v", first)
	}
	arguments["_apteva_caller_context"] = "worker-a"
	arguments["step_key"] = "inventory-retry-with-new-label"
	second := callTaskMCP(t, server, "task_run_step", arguments)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("cached task step failed: %#v", second)
	}
	if executions != 1 {
		t.Fatalf("downstream executions=%d, want exactly one", executions)
	}
	decoded := taskMCPTextJSON(t, second)
	if cached, _ := decoded["cached"].(bool); !cached {
		t.Fatalf("second task step was not returned as cached: %#v", decoded)
	}
	updated, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Progress == nil || *updated.Progress != 33 ||
		!strings.Contains(updated.CurrentStep, "inventory") {
		t.Fatalf("automatic step progress=%+v, want 33%% inventory milestone", updated)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var started, completed int
	for _, event := range events {
		switch event.EventType {
		case "step_started":
			started++
		case "step_completed":
			completed++
		}
	}
	if started != 1 || completed != 1 {
		t.Fatalf("step events started=%d completed=%d, want 1/1: %+v", started, completed, events)
	}
}

func TestMainTaskRunStepDeduplicatesACompetingWake(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Scheduled CRM check",
		AssignedThreadID: "main", CreatedByThreadID: "scheduler",
	})
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileMain,
		executeStep: func(_ context.Context, _, _ string, _ map[string]any) (json.RawMessage, error) {
			executions++
			return json.RawMessage(`{"checked":true}`), nil
		},
	}
	arguments := map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                task.ID,
		"step_key":               "check-current-state",
		"step_index":             1,
		"step_count":             1,
		"mcp_server":             "crm",
		"tool":                   "check",
		"arguments":              map[string]any{"scope": "today"},
	}
	first := callTaskMCP(t, server, "task_run_step", arguments)
	if isError, _ := first["isError"].(bool); isError {
		t.Fatalf("first main task step failed: %#v", first)
	}
	arguments["_apteva_caller_context"] = "main"
	second := callTaskMCP(t, server, "task_run_step", arguments)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("replayed main task step failed: %#v", second)
	}
	if executions != 1 {
		t.Fatalf("competing main wake executed the same logical step %d times, want 1", executions)
	}
	if cached, _ := taskMCPTextJSON(t, second)["cached"].(bool); !cached {
		t.Fatalf("second main step was not served from the durable receipt: %#v", second)
	}
}

func TestWorkerTaskRunStepAllowsExplicitRepeatedInput(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, Title: "Intentional repeated checks",
		AssignedThreadID: "worker-a", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	executions := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileWorker,
		executeStep: func(_ context.Context, _, _ string, _ map[string]any) (json.RawMessage, error) {
			executions++
			return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
		},
	}
	for index, key := range []string{"check-first", "check-second"} {
		result := callTaskMCP(t, server, "task_run_step", map[string]any{
			"_apteva_caller_context": "worker-a",
			"task_id":                task.ID, "step_key": key,
			"step_index": index + 1, "step_count": 2,
			"mcp_server": "audit", "tool": "check",
			"arguments":            map[string]any{"target": "same"},
			"allow_repeated_input": true,
		})
		if isError, _ := result["isError"].(bool); isError {
			t.Fatalf("intentional repeated step %d failed: %#v", index+1, result)
		}
	}
	if executions != 2 {
		t.Fatalf("explicit repeated-input executions=%d, want two", executions)
	}
}

func TestMainCannotSupersedeActiveConversationTask(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	conversation := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		handoff: func(context.Context, *AgentTask) error { return nil },
	}
	created := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-origin",
		"title":                  "Atlas readiness audit",
		"assign_to":              "main",
	})
	originTaskID := taskIDFromMCPResult(t, created)
	main := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileMain,
		deliver: func(context.Context, *AgentTask) error { return nil },
		handoff: func(context.Context, *AgentTask) error { return nil },
	}
	duplicate := callTaskMCP(t, main, "task_create", map[string]any{
		"_apteva_caller_context": "main",
		"title":                  "Replacement Atlas readiness audit",
	})
	if isError, _ := duplicate["isError"].(bool); !isError {
		t.Fatalf("main duplicate root task should be rejected: %#v", duplicate)
	}
	content := duplicate["content"].([]map[string]string)
	if !strings.Contains(content[0]["text"], originTaskID) ||
		!strings.Contains(content[0]["text"], "recreate, or supersede") {
		t.Fatalf("duplicate rejection did not direct main to existing task: %#v", duplicate)
	}
	tasks, err := store.ListAgentTasks(agent.ID, AgentTaskListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("duplicate guard left %d tasks, want one: %+v", len(tasks), tasks)
	}
	parallel := callTaskMCP(t, main, "task_create", map[string]any{
		"_apteva_caller_context": "main",
		"title":                  "Independent incident response",
	})
	if isError, _ := parallel["isError"].(bool); !isError {
		t.Fatalf("model-visible parallel root bypass should not exist: %#v", parallel)
	}
}

func TestConversationCreatesSingleStructuredScheduleWithoutSetupOrEarlyWake(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	handoffCalls := 0
	conversation := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		handoff: func(context.Context, *AgentTask) error {
			handoffCalls++
			return nil
		},
	}
	scheduled := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-recurring",
		"title":                  "Check Patreon posting daily",
		"description":            "Every day at 09:00 UTC, verify the latest scheduled Patreon post.",
		"assign_to":              "main",
		"idempotency_key":        "patreon-daily-schedule",
		"schedule": map[string]any{
			"kind": "cron", "cron": "0 9 * * *", "timezone": "UTC",
		},
	})
	if isError, _ := scheduled["isError"].(bool); isError {
		t.Fatalf("structured schedule failed: %#v", scheduled)
	}
	scheduleID := taskIDFromMCPResult(t, scheduled)
	parent, err := store.GetAgentTask(agent.ID, scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.ParentTaskID != "" || parent.OriginConversationID != "conv-recurring" ||
		parent.ScheduleKind != taskScheduleCron ||
		parent.ScheduleExpression != "0 9 * * *" || parent.ScheduleTimezone != "UTC" ||
		parent.NextRunAt == nil || !parent.ScheduleEnabled {
		t.Fatalf("unexpected single scheduled task: %+v", parent)
	}
	if parent.HandoffDeliveryStatus != "" || handoffCalls != 0 {
		t.Fatalf("schedule configuration incorrectly woke an execution: %+v", parent)
	}
	tasks, err := store.ListAgentTasks(agent.ID, AgentTaskListFilter{Limit: 20})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("one logical schedule created %d top-level records: tasks=%+v err=%v", len(tasks), tasks, err)
	}
	duplicate := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-recurring",
		"title":                  "Check Patreon posting daily",
		"assign_to":              "main",
		"idempotency_key":        "patreon-daily-schedule",
		"schedule": map[string]any{
			"kind": "cron", "cron": "0 10 * * *", "timezone": "UTC",
		},
	})
	duplicatePayload := taskMCPTextJSON(t, duplicate)
	duplicateTask, _ := duplicatePayload["task"].(map[string]any)
	if duplicateTask["id"] != scheduleID || duplicatePayload["created"] != false {
		t.Fatalf("schedule retry created a second task: %#v", duplicatePayload)
	}
	parent, err = store.GetAgentTask(agent.ID, scheduleID)
	if err != nil || parent.ScheduleExpression != "0 9 * * *" {
		t.Fatalf("idempotent retry mutated the authoritative schedule: task=%+v err=%v", parent, err)
	}
	main := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileMain,
		handoff: func(context.Context, *AgentTask) error { return nil },
	}
	updated := callTaskMCP(t, main, "task_update", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                scheduleID,
		"schedule": map[string]any{
			"kind": "cron", "cron": "0 10 * * *", "timezone": "UTC",
		},
		// Providers may materialize optional schema fields even though this is
		// semantically a schedule-only update. They must not make it fail or
		// mutate the execution state.
		"state":          "waiting",
		"progress":       "0",
		"clear_progress": false,
		"current_step":   "",
		"error":          "",
	})
	if isError, _ := updated["isError"].(bool); isError {
		t.Fatalf("provider-normalized schedule update failed: %#v", updated)
	}
	parent, err = store.GetAgentTask(agent.ID, scheduleID)
	if err != nil || parent.ScheduleExpression != "0 10 * * *" ||
		parent.State != taskStateWaiting || parent.Progress != nil {
		t.Fatalf("schedule-only update did not remain atomic: task=%+v err=%v", parent, err)
	}
	runArgs := map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                scheduleID,
		"idempotency_key":        "manual-check-1",
	}
	firstRun := taskMCPTextJSON(t, callTaskMCP(t, main, "task_run_now", runArgs))
	runArgs["_apteva_caller_context"] = "main"
	secondRun := taskMCPTextJSON(t, callTaskMCP(t, main, "task_run_now", runArgs))
	firstTask, _ := firstRun["task"].(map[string]any)
	secondTask, _ := secondRun["task"].(map[string]any)
	if firstRun["created"] != true || secondRun["created"] != false ||
		firstTask["id"] == "" || firstTask["id"] != secondTask["id"] {
		t.Fatalf("manual schedule retry was not idempotent: first=%#v second=%#v", firstRun, secondRun)
	}
	runs, err := store.ListAgentTaskRuns(agent.ID, scheduleID, 20)
	if err != nil || len(runs) != 1 {
		t.Fatalf("manual schedule retry created duplicate runs: runs=%+v err=%v", runs, err)
	}
}

func TestConversationTaskCreateDurablyWakesMainAndRetriesWithoutDuplicate(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	handoffCalls := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		handoff: func(_ context.Context, task *AgentTask) error {
			handoffCalls++
			if task.AssignedThreadID != "main" || task.OriginConversationID != "conv-handoff" {
				t.Fatalf("unexpected automatic handoff task: %+v", task)
			}
			if handoffCalls == 1 {
				return errors.New("temporary core wake failure")
			}
			return nil
		},
	}
	args := map[string]any{
		"_apteva_caller_context": "chat-conv-handoff",
		"title":                  "Continue autonomously",
		"description":            "Run the durable operation and return its verified result.",
		"assign_to":              "main",
		"idempotency_key":        "conversation-handoff-once",
	}
	first := callTaskMCP(t, server, "task_create", args)
	if isError, _ := first["isError"].(bool); !isError {
		t.Fatalf("failed main wake should be visible to the caller: %#v", first)
	}
	second := callTaskMCP(t, server, "task_create", args)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("idempotent handoff retry failed: %#v", second)
	}
	taskID := taskIDFromMCPResult(t, second)
	third := callTaskMCP(t, server, "task_create", args)
	if isError, _ := third["isError"].(bool); isError {
		t.Fatalf("delivered handoff retry should be cached: %#v", third)
	}
	if handoffCalls != 2 {
		t.Fatalf("handoff callback calls=%d, want failure + one success only", handoffCalls)
	}
	tasks, err := store.ListAgentTasks(agent.ID, AgentTaskListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != taskID ||
		tasks[0].HandoffDeliveryStatus != "delivered" ||
		tasks[0].HandoffDeliveredAt == nil {
		t.Fatalf("unexpected persisted handoff task: %+v", tasks)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var failed, delivered int
	for _, event := range events {
		switch event.EventType {
		case "handoff_delivery_failed":
			failed++
		case "handoff_delivery_delivered":
			delivered++
		}
	}
	if failed != 1 || delivered != 1 {
		t.Fatalf("handoff events failed=%d delivered=%d, want 1/1: %+v", failed, delivered, events)
	}
}

func TestMainTaskSpawnWorkerAtomicallyAssignsRestrictedWorker(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, Title: "Delegated audit",
		AssignedThreadID: "main", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	var spawnedWorker, spawnedInstructions string
	spawnCalls := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileMain,
		spawnWorker: func(_ context.Context, gotTask *AgentTask, workerID, instructions string) error {
			spawnCalls++
			if gotTask.ID != task.ID || gotTask.AssignedThreadID != workerID {
				t.Fatalf("spawn saw task before atomic assignment: %+v worker=%q", gotTask, workerID)
			}
			spawnedWorker, spawnedInstructions = workerID, instructions
			return nil
		},
	}
	directAssign := callTaskMCP(t, server, "task_assign", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                task.ID, "assigned_thread_id": "unsafe-worker",
	})
	if isError, _ := directAssign["isError"].(bool); !isError {
		t.Fatalf("direct worker assignment should require task_spawn_worker: %#v", directAssign)
	}
	spawned := callTaskMCP(t, server, "task_spawn_worker", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                task.ID, "worker_id": "audit-worker",
		"instructions": "Run inventory through audit/run with task_run_step.",
	})
	if isError, _ := spawned["isError"].(bool); isError {
		t.Fatalf("server worker spawn failed: %#v", spawned)
	}
	updated, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AssignedThreadID != "audit-worker" ||
		spawnedWorker != "audit-worker" ||
		spawnCalls != 1 ||
		!strings.Contains(spawnedInstructions, "task_run_step") {
		t.Fatalf("worker spawn result task=%+v worker=%q instructions=%q",
			updated, spawnedWorker, spawnedInstructions)
	}
	retry := callTaskMCP(t, server, "task_spawn_worker", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                task.ID, "worker_id": "audit-worker",
		"instructions": "Run inventory through audit/run with task_run_step.",
	})
	if isError, _ := retry["isError"].(bool); isError || spawnCalls != 1 {
		t.Fatalf("same-worker retry should return the existing assignment without another spawn: calls=%d result=%#v",
			spawnCalls, retry)
	}
	replacement := callTaskMCP(t, server, "task_spawn_worker", map[string]any{
		"_apteva_caller_context": "main",
		"task_id":                task.ID, "worker_id": "replacement-worker",
		"instructions": "Repeat the same audit.",
	})
	if isError, _ := replacement["isError"].(bool); !isError || spawnCalls != 1 {
		t.Fatalf("replacement worker should be rejected without another spawn: calls=%d result=%#v",
			spawnCalls, replacement)
	}
}

func TestConversationTaskMCPBindsOriginAndEnforcesConversationScope(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		handoff: func(context.Context, *AgentTask) error { return nil },
	}
	create := callTaskMCP(t, server, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-alpha",
		"title":                  "Prepare a durable result",
		"assign_to":              "self",
		"idempotency_key":        "alpha-work",
	})
	taskID := taskIDFromMCPResult(t, create)
	task, err := store.GetAgentTask(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.OriginConversationID != "conv-alpha" ||
		task.AssignedThreadID != "chat-conv-alpha" ||
		task.CreatedByThreadID != "chat-conv-alpha" {
		t.Fatalf("trusted conversation binding failed: %+v", task)
	}

	crossConversation := callTaskMCP(t, server, "task_get", map[string]any{
		"_apteva_caller_context": "chat-conv-beta", "task_id": taskID,
	})
	if isError, _ := crossConversation["isError"].(bool); !isError {
		t.Fatalf("cross-conversation read should fail: %#v", crossConversation)
	}
	content := crossConversation["content"].([]map[string]string)
	if !strings.Contains(content[0]["text"], "scope") {
		t.Fatalf("unexpected scope error: %#v", crossConversation)
	}

	mainAssigned := callTaskMCP(t, server, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-alpha",
		"title":                  "Continue after user leaves", "assign_to": "main",
	})
	mainTaskID := taskIDFromMCPResult(t, mainAssigned)
	mutation := callTaskMCP(t, server, "task_update", map[string]any{
		"_apteva_caller_context": "chat-conv-alpha",
		"task_id":                mainTaskID, "state": "running",
	})
	if isError, _ := mutation["isError"].(bool); !isError {
		t.Fatalf("conversation must not mutate main-assigned task: %#v", mutation)
	}
}

func TestTaskMCPReservesRelationshipsAndConversationOwnership(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	main := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileMain}
	for _, args := range []map[string]any{
		{"title": "Forged child", "parent_task_id": "task-forged"},
		{"title": "Forged origin", "origin_conversation_id": "conv-forged"},
		{"title": "Forged worker", "assigned_thread_id": "worker-forged"},
	} {
		args["_apteva_caller_context"] = "main"
		result := callTaskMCP(t, main, "task_create", args)
		if isError, _ := result["isError"].(bool); !isError {
			t.Fatalf("reserved relationship was accepted: args=%v result=%#v", args, result)
		}
	}

	conversation := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
	}
	created := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-owned",
		"title":                  "Conversation-owned work",
		"assign_to":              "self",
	})
	taskID := taskIDFromMCPResult(t, created)
	parallel := callTaskMCP(t, main, "task_create", map[string]any{
		"_apteva_caller_context": "main",
		"title":                  "Independent main work",
	})
	if isError, _ := parallel["isError"].(bool); isError {
		t.Fatalf("conversation-self work incorrectly blocked main creation: %#v", parallel)
	}
	for _, operation := range []struct {
		name string
		args map[string]any
	}{
		{"task_update", map[string]any{"task_id": taskID, "state": "running"}},
		{"task_assign", map[string]any{"task_id": taskID, "assigned_thread_id": "main"}},
		{"task_complete", map[string]any{"task_id": taskID, "result": "Forged completion"}},
	} {
		operation.args["_apteva_caller_context"] = "main"
		result := callTaskMCP(t, main, operation.name, operation.args)
		if isError, _ := result["isError"].(bool); !isError {
			t.Fatalf("main %s changed conversation-owned work: %#v", operation.name, result)
		}
	}
	task, err := store.GetAgentTask(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != taskStateQueued || task.AssignedThreadID != "chat-conv-owned" {
		t.Fatalf("conversation-owned task was changed: %+v", task)
	}
}

func TestTaskMCPTreatsProviderDefaultScheduleAsOmitted(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	conversation := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
	}
	result := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-provider-defaults",
		"title":                  "Immediate tracked work",
		"assign_to":              "self",
		"schedule": map[string]any{
			"kind": "once", "at": "", "after": "", "every": "", "cron": "",
			"timezone": "UTC", "overlap_policy": "skip", "catchup_policy": "skip",
		},
	})
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("provider-default schedule should be omitted: %#v", result)
	}
	task, err := store.GetAgentTask(agent.ID, taskIDFromMCPResult(t, result))
	if err != nil {
		t.Fatal(err)
	}
	if task.ScheduleKind != "" || task.AssignedThreadID != "chat-conv-provider-defaults" ||
		task.State != taskStateQueued {
		t.Fatalf("default schedule changed immediate task semantics: %+v", task)
	}

	invalid := callTaskMCP(t, conversation, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-provider-defaults",
		"title":                  "Incomplete recurrence",
		"assign_to":              "main",
		"schedule":               map[string]any{"kind": "interval", "every": ""},
	})
	if isError, _ := invalid["isError"].(bool); !isError {
		t.Fatalf("genuinely incomplete interval schedule was accepted: %#v", invalid)
	}
}

func TestWorkerTaskMCPOnlyMutatesAssignedTask(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Worker scope",
		AssignedThreadID: "worker-a", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileWorker}
	denied := callTaskMCP(t, server, "task_update", map[string]any{
		"_apteva_caller_context": "worker-b",
		"task_id":                task.ID, "state": "running",
	})
	if isError, _ := denied["isError"].(bool); !isError {
		t.Fatalf("unassigned worker mutation should fail: %#v", denied)
	}
	allowed := callTaskMCP(t, server, "task_update", map[string]any{
		"_apteva_caller_context": "worker-a",
		"task_id":                task.ID, "state": "running", "progress": 25,
	})
	if isError, _ := allowed["isError"].(bool); isError {
		t.Fatalf("assigned worker mutation failed: %#v", allowed)
	}
}

func TestTaskCompletionDeliversExactlyOnceToOrigin(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	deliveries := 0
	var delivered *AgentTask
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		deliver: func(_ context.Context, task *AgentTask) error {
			deliveries++
			delivered = task
			return nil
		},
	}
	create := callTaskMCP(t, server, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-delivery",
		"title":                  "Finish and return", "assign_to": "self",
	})
	taskID := taskIDFromMCPResult(t, create)
	completeArgs := map[string]any{
		"_apteva_caller_context": "chat-conv-delivery",
		"task_id":                taskID, "result": "The durable result",
	}
	first := callTaskMCP(t, server, "task_complete", completeArgs)
	if isError, _ := first["isError"].(bool); isError {
		t.Fatalf("completion failed: %#v", first)
	}
	second := callTaskMCP(t, server, "task_complete", completeArgs)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("idempotent completion failed: %#v", second)
	}
	if deliveries != 1 || delivered == nil || delivered.OriginConversationID != "conv-delivery" {
		t.Fatalf("deliveries=%d delivered=%+v", deliveries, delivered)
	}
	task, err := store.GetAgentTask(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.CompletionDeliveryStatus != "delivered" || task.CompletionDeliveredAt == nil {
		t.Fatalf("delivery receipt missing: %+v", task)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	deliveryEvents := 0
	for _, event := range events {
		if event.EventType == "completion_delivery_delivered" {
			deliveryEvents++
		}
	}
	if deliveryEvents != 1 {
		t.Fatalf("delivery events=%d, want 1: %+v", deliveryEvents, events)
	}
}

func TestWorkerFailureDeliversTerminalOutcomeToOrigin(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Fail safely",
		AssignedThreadID: "worker-failure", OriginConversationID: "conv-failure",
		CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileWorker,
		deliver: func(_ context.Context, delivered *AgentTask) error {
			deliveries++
			if delivered.State != taskStateFailed || delivered.Error != "upstream rejected the request" {
				t.Fatalf("unexpected failure delivery: %+v", delivered)
			}
			return nil
		},
	}
	result := callTaskMCP(t, server, "task_update", map[string]any{
		"_apteva_caller_context": "worker-failure",
		"task_id":                task.ID, "state": "failed",
		"error":        "upstream rejected the request",
		"current_step": "Stopped after upstream rejection",
	})
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("worker failure update failed: %#v", result)
	}
	persisted, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || persisted.State != taskStateFailed ||
		persisted.CompletionDeliveryStatus != "delivered" ||
		persisted.CompletedAt == nil {
		t.Fatalf("deliveries=%d task=%+v", deliveries, persisted)
	}
}

func TestWorkerFailureDeliveryRetriesWithoutMutatingTerminalTask(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Retry failure receipt",
		AssignedThreadID: "worker-retry", OriginConversationID: "conv-retry-failure",
		CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileWorker,
		deliver: func(_ context.Context, delivered *AgentTask) error {
			deliveries++
			if deliveries == 1 {
				return context.DeadlineExceeded
			}
			if delivered.State != taskStateFailed {
				t.Fatalf("retry delivered non-failure: %+v", delivered)
			}
			return nil
		},
	}
	args := map[string]any{
		"_apteva_caller_context": "worker-retry",
		"task_id":                task.ID,
		"state":                  "failed",
		"error":                  "permanent upstream rejection",
		"current_step":           "Stopped permanently",
	}
	first := callTaskMCP(t, server, "task_update", args)
	if isError, _ := first["isError"].(bool); !isError {
		t.Fatalf("first delivery should report its failure: %#v", first)
	}
	second := callTaskMCP(t, server, "task_update", args)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("same terminal update must retry only delivery: %#v", second)
	}
	persisted, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || persisted.State != taskStateFailed ||
		persisted.CompletionDeliveryStatus != "delivered" {
		t.Fatalf("deliveries=%d task=%+v", deliveries, persisted)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var failedTransitions, failedDeliveries, deliveredReceipts int
	for _, event := range events {
		if event.ToState == taskStateFailed && event.EventType == "state_changed" {
			failedTransitions++
		}
		switch event.EventType {
		case "completion_delivery_failed":
			failedDeliveries++
		case "completion_delivery_delivered":
			deliveredReceipts++
		}
	}
	if failedTransitions != 1 || failedDeliveries != 1 || deliveredReceipts != 1 {
		t.Fatalf("events transitions=%d delivery_failed=%d delivered=%d: %+v",
			failedTransitions, failedDeliveries, deliveredReceipts, events)
	}
}

func TestOriginConversationCanCancelMainAssignedTask(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Long main task",
		AssignedThreadID: "main", OriginConversationID: "conv-cancel",
		CreatedByThreadID: "chat-conv-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		deliver: func(_ context.Context, delivered *AgentTask) error {
			deliveries++
			if delivered.State != taskStateCancelled {
				t.Fatalf("unexpected cancellation delivery: %+v", delivered)
			}
			return nil
		},
	}
	result := callTaskMCP(t, server, "task_cancel", map[string]any{
		"_apteva_caller_context": "chat-conv-cancel",
		"task_id":                task.ID, "reason": "The user changed their mind",
	})
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("origin cancellation failed: %#v", result)
	}
	persisted, _ := store.GetAgentTask(agent.ID, task.ID)
	if deliveries != 1 || persisted.State != taskStateCancelled ||
		persisted.Error != "The user changed their mind" ||
		persisted.CompletionDeliveryStatus != "delivered" {
		t.Fatalf("deliveries=%d task=%+v", deliveries, persisted)
	}
}

func TestTaskMCPRequiresTrustedCallerContext(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	server := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileMain}
	result := callTaskMCP(t, server, "task_list", map[string]any{})
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("missing trusted caller should fail: %#v", result)
	}
}

func TestTaskCapabilityUsesTrustedChannelsTransport(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "")
	disabled := channelsMCPConfig("http://channels")
	if disabled["no_spawn"] != true {
		t.Fatal("disabled channels transport should remain unavailable to workers")
	}
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	enabled := channelsMCPConfig("http://channels")
	if _, found := enabled["no_spawn"]; found {
		t.Fatal("enabled trusted channels transport must be grantable to assigned workers")
	}
	if !isServerOwnedTaskMCP("tasks") || !isServerOwnedTaskMCP("tasks-worker") ||
		isServerOwnedTaskMCP("app-tasks") {
		t.Fatal("legacy task MCP cleanup classification is incorrect")
	}
}

func TestChannelProfilesProjectTaskToolsByTrustedCaller(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	conversation := &channelMCPServer{
		profile: channelMCPProfileConversation, taskEnabled: true,
		taskStore: store, taskAgent: agent, registry: NewChannelRegistry(),
	}
	listed := conversation.toolsList()["tools"].([]map[string]any)
	names := map[string]bool{}
	for _, tool := range listed {
		names[tool["name"].(string)] = true
	}
	if !names["send"] || !names["task_create"] || !names["task_complete"] {
		t.Fatalf("conversation transport tools=%v", names)
	}
	params, _ := json.Marshal(map[string]any{
		"name": "task_create",
		"arguments": map[string]any{
			"_apteva_caller_context": "chat-conv-trusted",
			"title":                  "Trusted transport task",
			"assign_to":              "self",
		},
	})
	result, rpcErr := conversation.handleToolCall(params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	created := result.(map[string]any)
	if isError, _ := created["isError"].(bool); isError {
		t.Fatalf("trusted conversation task failed: %#v", created)
	}

	main := &channelMCPServer{
		profile: channelMCPProfileAgentOutput, taskEnabled: true,
		taskStore: store, taskAgent: agent, registry: NewChannelRegistry(),
	}
	params, _ = json.Marshal(map[string]any{
		"name": "task_list", "arguments": map[string]any{},
	})
	result, rpcErr = main.handleToolCall(params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if isError, _ := result.(map[string]any)["isError"].(bool); isError {
		t.Fatalf("main transport did not establish main authority: %#v", result)
	}
}
