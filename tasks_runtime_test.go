package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTaskStepExecutorResolvesCoreNamespacedTool(t *testing.T) {
	var calledTool string
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "audit", "version": "1"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			calledTool = request.Params.Name
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{
					"content": []map[string]string{{"type": "text", "text": "ok"}},
				},
			})
		default:
			t.Fatalf("unexpected MCP method %q", request.Method)
		}
	}))
	defer mcp.Close()
	execute := taskStepExecutorFromConfig(map[string]any{
		"mcp_servers": []any{map[string]any{
			"name": "worker-fixture", "transport": "http", "url": mcp.URL,
		}},
	})
	if _, err := execute(
		t.Context(), "worker-fixture", "worker-fixture_run_atlas_stage",
		map[string]any{"stage": 1},
	); err != nil {
		t.Fatal(err)
	}
	if calledTool != "run_atlas_stage" {
		t.Fatalf("downstream tool=%q, want namespace removed", calledTool)
	}
}

func TestSpawnTaskWorkerOnCoreUsesRestrictedProfile(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer core.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	task := &AgentTask{ID: "task-safe", Title: "Safe audit"}
	if err := spawnTaskWorkerOnCore(
		t.Context(), port, "core-key", task, "audit-worker",
		"Run audit/run with task_run_step.",
	); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/threads/audit-worker" || gotAuth != "Bearer core-key" {
		t.Fatalf("spawn path=%q auth=%q", gotPath, gotAuth)
	}
	if conversation, _ := payload["conversation"].(bool); conversation {
		t.Fatalf("task worker was incorrectly marked as a conversation: %#v", payload)
	}
	if got := stringSliceFromAny(payload["mcp"]); strings.Join(got, ",") != "channels" {
		t.Fatalf("task worker MCPs=%v, want channels only", got)
	}
	if got := stringSliceFromAny(payload["tools"]); strings.Join(got, ",") != "send,pace" {
		t.Fatalf("task worker tools=%v, want send,pace", got)
	}
	directive, _ := payload["directive_suffix"].(string)
	if !strings.Contains(directive, "Never call a raw") ||
		!strings.Contains(directive, "task_run_step") {
		t.Fatalf("restricted worker directive missing execution boundary: %q", directive)
	}
}

func stringSliceFromAny(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func TestScheduledTaskHandoffDeclaresOneServerOwnedOccurrence(t *testing.T) {
	scheduledFor := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	var gotThread, gotMessage string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotThread, gotMessage = body.ThreadID, body.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[42] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	server := &Server{agents: manager}
	task := &AgentTask{
		ID: "task-occurrence", AgentID: 42, State: taskStateQueued,
		Title: "Check Patreon posting", Description: "Verify the current post.",
		AssignedThreadID: "main", ParentTaskID: "task-schedule",
		ScheduledFor: &scheduledFor,
	}
	if err := server.deliverTaskHandoff(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(gotMessage), " ")
	for _, required := range []string{
		"[SCHEDULED TASK OCCURRENCE — SERVER-AUTHORITATIVE]",
		"task_id: task-occurrence",
		"schedule_task_id: task-schedule",
		"scheduled_for: 2026-07-31T09:00:00Z",
		"persisted exactly one occurrence",
		"Use task_run_step with stable logical step keys for every domain operation",
		"do not call pace to reproduce this cadence",
		"simultaneous ordinary wake does not change this task identity",
	} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("scheduled handoff missing %q: %q", required, gotMessage)
		}
	}
	if gotThread != "main" {
		t.Fatalf("scheduled occurrence delivered to %q, want main", gotThread)
	}
}

func TestDeliverTaskCompletionTargetsOriginatingConversation(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	var gotThread, gotMessage, gotAuth string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotThread, gotMessage = body.ThreadID, body.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	ensured := false
	server := &Server{
		store: store, agents: manager,
		taskConversationEnsure: func(agentID int64, conversationID string) error {
			if agentID != agent.ID || conversationID != "conv-origin" {
				t.Fatalf("ensure agent=%d conversation=%q", agentID, conversationID)
			}
			ensured = true
			return nil
		},
	}
	task := &AgentTask{
		ID: "task-test", AgentID: agent.ID, Title: "Research",
		State: taskStateCompleted, Result: "Completed safely",
		OriginConversationID: "conv-origin",
	}
	if err := server.deliverTaskCompletion(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if !ensured || gotThread != "chat-conv-origin" {
		t.Fatalf("thread=%q, want chat-conv-origin", gotThread)
	}
	if gotAuth != "Bearer core-test" ||
		!strings.Contains(gotMessage, "[TASK COMPLETED — SERVER-AUTHORITATIVE]") ||
		!strings.Contains(gotMessage, "task_id: task-test") ||
		!strings.Contains(gotMessage, "Completed safely") ||
		!strings.Contains(gotMessage, `phase="final"`) {
		t.Fatalf("unexpected completion event auth=%q message=%q", gotAuth, gotMessage)
	}
}

func TestRecoverTaskHandoffWakesMainExactlyOnce(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Atlas audit",
		Description:      "Run all three checks with a dedicated worker.",
		AssignedThreadID: "main", OriginConversationID: "conv-handoff-recover",
		CreatedByThreadID: "chat-conv-handoff-recover",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.HandoffDeliveryStatus != "pending" {
		t.Fatalf("handoff status=%q, want pending", task.HandoffDeliveryStatus)
	}
	var deliveries int
	var gotThread, gotMessage string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries++
		var body struct {
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotThread, gotMessage = body.ThreadID, body.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	server := &Server{store: store, agents: manager}
	server.recoverTaskDeliveries(agent.ID)
	server.recoverTaskDeliveries(agent.ID)
	persisted, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || gotThread != "main" ||
		persisted.HandoffDeliveryStatus != "delivered" ||
		persisted.HandoffDeliveredAt == nil ||
		!strings.Contains(gotMessage, "[TASK ASSIGNED FROM USER CONVERSATION") ||
		!strings.Contains(gotMessage, task.ID) ||
		!strings.Contains(gotMessage, "task_spawn_worker") ||
		!strings.Contains(gotMessage, "automatically") {
		t.Fatalf("deliveries=%d thread=%q message=%q task=%+v",
			deliveries, gotThread, gotMessage, persisted)
	}
	if err := server.wakeQueuedTaskHandoff(agent.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	normalizedNudge := strings.Join(strings.Fields(gotMessage), " ")
	if deliveries != 2 || gotThread != "main" ||
		!strings.Contains(gotMessage, "[QUEUED TASK FOLLOW-UP") ||
		!strings.Contains(normalizedNudge, "only automatic queued-task follow-up") {
		t.Fatalf("queued nudge deliveries=%d thread=%q message=%q", deliveries, gotThread, gotMessage)
	}
	if err := server.wakeQueuedTaskHandoff(agent.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 {
		t.Fatalf("queued handoff received more than one nudge: deliveries=%d", deliveries)
	}
}

func TestPostEvolveResumeWakesOnlyActiveMainConversationTask(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID,
		Title:            "Configure daily check-in",
		Description:      "Persist the schedule and verify its next wake.",
		AssignedThreadID: "main", OriginConversationID: "conv-schedule",
		CreatedByThreadID: "chat-conv-schedule",
	})
	if err != nil {
		t.Fatal(err)
	}
	running := taskStateRunning
	task, _, err = store.UpdateAgentTask(agent.ID, task.ID, "main", UpdateAgentTaskInput{
		State: &running,
	})
	if err != nil {
		t.Fatal(err)
	}
	var deliveries int
	var gotThread, gotMessage string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries++
		var body struct {
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotThread, gotMessage = body.ThreadID, body.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	server := &Server{store: store, agents: manager}
	if err := server.wakeMainTasksAfterDirectiveEvolution(agent.ID); err != nil {
		t.Fatal(err)
	}
	normalizedMessage := strings.Join(strings.Fields(gotMessage), " ")
	if deliveries != 1 || gotThread != "main" ||
		!strings.Contains(gotMessage, "[DIRECTIVE UPDATE PERSISTED") ||
		!strings.Contains(gotMessage, task.ID) ||
		!strings.Contains(normalizedMessage, "do not reproduce that timing with pace") {
		t.Fatalf("deliveries=%d thread=%q message=%q", deliveries, gotThread, gotMessage)
	}
	if _, _, err := store.CompleteAgentTask(agent.ID, task.ID, "main", "Configured daily at 09:00 UTC."); err != nil {
		t.Fatal(err)
	}
	if err := server.wakeMainTasksAfterDirectiveEvolution(agent.ID); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("terminal task received an unnecessary resume wake: deliveries=%d", deliveries)
	}
}

func TestTaskCompletionDeliveryFailureCanRetry(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	attempts := 0
	server := &taskMCPServer{
		store: store, agent: agent, profile: taskMCPProfileConversation,
		deliver: func(_ context.Context, _ *AgentTask) error {
			attempts++
			if attempts == 1 {
				return errors.New("core temporarily unavailable")
			}
			return nil
		},
	}
	create := callTaskMCP(t, server, "task_create", map[string]any{
		"_apteva_caller_context": "chat-conv-retry",
		"title":                  "Retry delivery", "assign_to": "self",
	})
	taskID := taskIDFromMCPResult(t, create)
	arguments := map[string]any{
		"_apteva_caller_context": "chat-conv-retry",
		"task_id":                taskID, "result": "Completed once",
	}
	first := callTaskMCP(t, server, "task_complete", arguments)
	if isError, _ := first["isError"].(bool); !isError {
		t.Fatalf("first failed delivery should be visible as a tool error: %#v", first)
	}
	task, err := store.GetAgentTask(agent.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != taskStateCompleted || task.CompletionDeliveryStatus != "failed" {
		t.Fatalf("work completion must survive delivery failure: %+v", task)
	}
	second := callTaskMCP(t, server, "task_complete", arguments)
	if isError, _ := second["isError"].(bool); isError {
		t.Fatalf("retry failed: %#v", second)
	}
	task, _ = store.GetAgentTask(agent.ID, taskID)
	if attempts != 2 || task.CompletionDeliveryStatus != "delivered" {
		t.Fatalf("attempts=%d task=%+v", attempts, task)
	}
}

func TestCancellationNotifiesAssignedThreadAndOrigin(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	type receivedEvent struct {
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
	}
	var received []receivedEvent
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event receivedEvent
		_ = json.NewDecoder(r.Body).Decode(&event)
		received = append(received, event)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	server := &Server{store: store, agents: manager}
	task := &AgentTask{
		ID: "task-cancel", AgentID: agent.ID, Title: "Slow export",
		State: taskStateCancelled, Error: "User cancelled",
		AssignedThreadID: "worker-export", OriginConversationID: "conv-cancel",
	}
	if err := server.deliverTaskCompletion(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 {
		t.Fatalf("events=%d, want worker stop + origin outcome: %+v", len(received), received)
	}
	if received[0].ThreadID != "worker-export" ||
		!strings.Contains(received[0].Message, "Stop work on this task now") {
		t.Fatalf("unexpected worker cancellation: %+v", received[0])
	}
	if received[1].ThreadID != "chat-conv-cancel" ||
		!strings.Contains(received[1].Message, "[TASK CANCELLED") {
		t.Fatalf("unexpected origin cancellation: %+v", received[1])
	}
}

func TestRecoverTaskDeliveriesAfterAgentReturns(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Recover outcome",
		AssignedThreadID: "main", OriginConversationID: "conv-recover",
		CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err = store.CompleteAgentTask(agent.ID, task.ID, "main", "Recovered result")
	if err != nil {
		t.Fatal(err)
	}
	if task.CompletionDeliveryStatus != "pending" {
		t.Fatalf("delivery status=%q, want pending", task.CompletionDeliveryStatus)
	}
	deliveries := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	manager := NewAgentManager(t.TempDir(), "echo")
	manager.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-test", channels: &AgentChannels{}, reattached: true,
	}
	server := &Server{store: store, agents: manager}
	server.recoverTaskDeliveries(agent.ID)
	server.recoverTaskDeliveries(agent.ID)
	persisted, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || persisted.CompletionDeliveryStatus != "delivered" {
		t.Fatalf("deliveries=%d task=%+v", deliveries, persisted)
	}
}
