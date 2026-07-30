package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTaskAPIIsHiddenWhenFeatureDisabled(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "")
	server := newTestServer(t)
	request := authedRequest(t, http.MethodGet, "/api/tasks?agent_id=1", "", nil)
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.Code)
	}
}

func TestTaskAPICreateListPatchEventsAndCancel(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)

	createRequest := authedRequest(t, http.MethodPost, "/api/tasks", "", map[string]any{
		"agent_id": agent.ID, "title": "API durable task",
		"assigned_thread_id": "main", "idempotency_key": "api-task",
	})
	createResponse := httptest.NewRecorder()
	server.handleTasks(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	task := created["task"].(map[string]any)
	taskID := task["id"].(string)

	listRequest := authedRequest(t, http.MethodGet, "/api/tasks?agent_id=1&state=queued", "", nil)
	listResponse := httptest.NewRecorder()
	server.handleTasks(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), taskID) {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	patchRequest := authedRequest(t, http.MethodPatch, "/api/tasks/"+taskID, "", map[string]any{
		"state": "running", "progress": 50, "current_step": "Halfway",
	})
	patchResponse := httptest.NewRecorder()
	server.handleTaskByID(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusOK || !strings.Contains(patchResponse.Body.String(), `"progress":50`) {
		t.Fatalf("patch status=%d body=%s", patchResponse.Code, patchResponse.Body.String())
	}

	eventsRequest := authedRequest(t, http.MethodGet, "/api/tasks/"+taskID+"/events", "", nil)
	eventsResponse := httptest.NewRecorder()
	server.handleTaskByID(eventsResponse, eventsRequest)
	if eventsResponse.Code != http.StatusOK ||
		!strings.Contains(eventsResponse.Body.String(), `"event_type":"created"`) ||
		!strings.Contains(eventsResponse.Body.String(), `"event_type":"state_changed"`) {
		t.Fatalf("events status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}

	cancelRequest := authedRequest(t, http.MethodPost, "/api/tasks/"+taskID+"/cancel", "", map[string]any{
		"reason": "Operator cancelled",
	})
	cancelResponse := httptest.NewRecorder()
	server.handleTaskByID(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusOK ||
		!strings.Contains(cancelResponse.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("cancel status=%d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}
}

func TestTaskAPICreateDispatchesOperatorTaskToMain(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)

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
	server.agents.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-api", reattached: true, channels: &AgentChannels{},
	}

	request := authedRequest(t, http.MethodPost, "/api/tasks", "", map[string]any{
		"agent_id":    agent.ID,
		"title":       "Prepare the operator briefing",
		"description": "Collect the current results and produce a verified summary.",
	})
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if gotThread != "main" ||
		!strings.Contains(gotMessage, "[TASK CREATED BY OPERATOR") ||
		!strings.Contains(gotMessage, payload.Task.ID) ||
		!strings.Contains(gotMessage, "Prepare the operator briefing") ||
		payload.Task.AssignedThreadID != "main" ||
		payload.Task.HandoffDeliveryStatus != "delivered" {
		t.Fatalf("thread=%q message=%q task=%+v", gotThread, gotMessage, payload.Task)
	}

	running := taskStateRunning
	if _, _, err := server.store.UpdateAgentTask(
		agent.ID, payload.Task.ID, "main", UpdateAgentTaskInput{State: &running},
	); err != nil {
		t.Fatal(err)
	}
}

func TestTaskAPICreateRejectsServerManagedRelationshipsAndExecutionState(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	tests := []map[string]any{
		{"parent_task_id": "task-forged"},
		{"origin_conversation_id": "conv-forged"},
		{"origin_message_id": "message-forged"},
		{"assigned_thread_id": "worker-forged"},
		{"state": "running"},
		{"progress": 20},
		{"current_step": "Pretend this already started"},
	}
	for _, patch := range tests {
		body := map[string]any{"agent_id": agent.ID, "title": "Forged task"}
		for key, value := range patch {
			body[key] = value
		}
		request := authedRequest(t, http.MethodPost, "/api/tasks", "", body)
		response := httptest.NewRecorder()
		server.handleTasks(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("patch=%v status=%d body=%s", patch, response.Code, response.Body.String())
		}
	}
	tasks, err := server.store.ListAgentTasks(agent.ID, AgentTaskListFilter{Limit: 20})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("invalid API creates persisted tasks=%+v err=%v", tasks, err)
	}
}

func TestTaskAPICreatePersistsWhenAgentIsOffline(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)

	request := authedRequest(t, http.MethodPost, "/api/tasks", "", map[string]any{
		"agent_id": agent.ID, "title": "Queue for later",
	})
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusCreated ||
		!strings.Contains(response.Body.String(), `"delivery_warning"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Task.State != taskStateQueued ||
		payload.Task.HandoffDeliveryStatus != "failed" ||
		!strings.Contains(payload.Task.HandoffDeliveryError, "not running") {
		t.Fatalf("unexpected offline task: %+v", payload.Task)
	}
}

func TestTaskAPIScheduleLifecycleUsesTaskRoutesWithoutEarlyAgentWake(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)

	createRequest := authedRequest(t, http.MethodPost, "/api/tasks", "", map[string]any{
		"agent_id": agent.ID,
		"title":    "Daily Patreon check",
		"schedule": map[string]any{
			"kind": "cron", "cron": "0 9 * * *", "timezone": "UTC",
		},
	})
	createResponse := httptest.NewRecorder()
	server.handleTasks(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Task.ScheduleKind != taskScheduleCron || created.Task.NextRunAt == nil ||
		created.Task.HandoffDeliveryStatus != "" {
		t.Fatalf("schedule was not stored without an early wake: %+v", created.Task)
	}

	pauseRequest := authedRequest(t, http.MethodPost, "/api/tasks/"+created.Task.ID+"/pause", "", nil)
	pauseResponse := httptest.NewRecorder()
	server.handleTaskByID(pauseResponse, pauseRequest)
	var paused struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(pauseResponse.Body.Bytes(), &paused); err != nil {
		t.Fatal(err)
	}
	if pauseResponse.Code != http.StatusOK || paused.Task.ScheduleEnabled {
		t.Fatalf("pause status=%d body=%s", pauseResponse.Code, pauseResponse.Body.String())
	}

	updateRequest := authedRequest(t, http.MethodPatch, "/api/tasks/"+created.Task.ID+"/schedule", "", map[string]any{
		"kind": "interval", "every": "2h", "timezone": "UTC",
	})
	updateResponse := httptest.NewRecorder()
	server.handleTaskByID(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusOK ||
		!strings.Contains(updateResponse.Body.String(), `"schedule_expression":"2h0m0s"`) {
		t.Fatalf("update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}

	runRequest := authedRequest(t, http.MethodPost, "/api/tasks/"+created.Task.ID+"/run-now", "", map[string]any{
		"idempotency_key": "api-run-now-1",
	})
	runResponse := httptest.NewRecorder()
	server.handleTaskByID(runResponse, runRequest)
	if runResponse.Code != http.StatusOK ||
		!strings.Contains(runResponse.Body.String(), `"delivery_warning"`) {
		t.Fatalf("run-now status=%d body=%s", runResponse.Code, runResponse.Body.String())
	}
	var runPayload struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(runResponse.Body.Bytes(), &runPayload); err != nil {
		t.Fatal(err)
	}
	if runPayload.Task.ParentTaskID != created.Task.ID || runPayload.Task.ScheduledFor == nil {
		t.Fatalf("unexpected immediate run: %+v", runPayload.Task)
	}
	retryRequest := authedRequest(t, http.MethodPost, "/api/tasks/"+created.Task.ID+"/run-now", "", map[string]any{
		"idempotency_key": "api-run-now-1",
	})
	retryResponse := httptest.NewRecorder()
	server.handleTaskByID(retryResponse, retryRequest)
	var retryPayload struct {
		Task    AgentTask `json:"task"`
		Created bool      `json:"created"`
	}
	if err := json.Unmarshal(retryResponse.Body.Bytes(), &retryPayload); err != nil {
		t.Fatal(err)
	}
	if retryResponse.Code != http.StatusOK || retryPayload.Created ||
		retryPayload.Task.ID != runPayload.Task.ID {
		t.Fatalf("run-now retry duplicated the occurrence: status=%d payload=%+v body=%s",
			retryResponse.Code, retryPayload, retryResponse.Body.String())
	}

	runsRequest := authedRequest(t, http.MethodGet, "/api/tasks/"+created.Task.ID+"/runs", "", nil)
	runsResponse := httptest.NewRecorder()
	server.handleTaskByID(runsResponse, runsRequest)
	if runsResponse.Code != http.StatusOK ||
		!strings.Contains(runsResponse.Body.String(), runPayload.Task.ID) {
		t.Fatalf("runs status=%d body=%s", runsResponse.Code, runsResponse.Body.String())
	}
}

func TestTaskAPISchedulingDisableSwitchBlocksScheduleMutations(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Disabled schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("APTEVA_TASK_SCHEDULING", "0")
	for _, request := range []*http.Request{
		authedRequest(t, http.MethodPost, "/api/tasks/"+task.ID+"/pause", "", nil),
		authedRequest(t, http.MethodPost, "/api/tasks/"+task.ID+"/resume", "", nil),
		authedRequest(t, http.MethodPost, "/api/tasks/"+task.ID+"/run-now", "", map[string]any{
			"idempotency_key": "disabled-run",
		}),
		authedRequest(t, http.MethodPatch, "/api/tasks/"+task.ID+"/schedule", "", map[string]any{
			"kind": "interval", "every": "2h",
		}),
	} {
		response := httptest.NewRecorder()
		server.handleTaskByID(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s %s status=%d body=%s", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
	}
}

func TestTaskByIDAcceptsPathAfterAPIMountPrefixIsStripped(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Mounted task route",
		AssignedThreadID: "main", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authedRequest(t, http.MethodGet, "/tasks/"+task.ID, "", nil)
	response := httptest.NewRecorder()
	server.handleTaskByID(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), task.ID) {
		t.Fatalf("mounted GET status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskAPICompletionDeliversToOrigin(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	var deliveredThread string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ThreadID string `json:"thread_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		deliveredThread = body.ThreadID
		w.WriteHeader(http.StatusAccepted)
	}))
	defer core.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(core.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	server.agents.processes[agent.ID] = &runningAgent{
		port: port, coreAPIKey: "core-api", reattached: true, channels: &AgentChannels{},
	}
	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "API completion",
		AssignedThreadID: "main", OriginConversationID: "conv-api-origin",
		CreatedByThreadID: "api:user:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authedRequest(t, http.MethodPatch, "/api/tasks/"+task.ID, "", map[string]any{
		"state": "completed", "result": "Completed through API",
	})
	response := httptest.NewRecorder()
	server.handleTaskByID(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	persisted, err := server.store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveredThread != "chat-conv-api-origin" ||
		persisted.CompletionDeliveryStatus != "delivered" {
		t.Fatalf("thread=%q task=%+v", deliveredThread, persisted)
	}
}

func TestTaskAPIProjectRolesProtectReadsAndMutations(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	viewer, err := server.store.CreateUser("task-viewer@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.AddProjectMember(agent.ProjectID, viewer.ID, ProjectViewer, agent.UserID); err != nil {
		t.Fatal(err)
	}
	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Protected task",
		AssignedThreadID: "main", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	getRequest := authedRequest(t, http.MethodGet, "/api/tasks/"+task.ID, "", nil)
	getRequest.Header.Set("X-User-ID", strconv.FormatInt(viewer.ID, 10))
	getResponse := httptest.NewRecorder()
	server.handleTaskByID(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("viewer GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	patchRequest := authedRequest(t, http.MethodPatch, "/api/tasks/"+task.ID, "", map[string]any{
		"state": "running",
	})
	patchRequest.Header.Set("X-User-ID", strconv.FormatInt(viewer.ID, 10))
	patchResponse := httptest.NewRecorder()
	server.handleTaskByID(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusForbidden {
		t.Fatalf("viewer PATCH status=%d body=%s", patchResponse.Code, patchResponse.Body.String())
	}

	outsider, err := server.store.CreateUser("task-outsider@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	outsiderRequest := authedRequest(t, http.MethodGet, "/api/tasks/"+task.ID, "", nil)
	outsiderRequest.Header.Set("X-User-ID", strconv.FormatInt(outsider.ID, 10))
	outsiderResponse := httptest.NewRecorder()
	server.handleTaskByID(outsiderResponse, outsiderRequest)
	if outsiderResponse.Code != http.StatusForbidden {
		t.Fatalf("outsider GET status=%d body=%s", outsiderResponse.Code, outsiderResponse.Body.String())
	}
}

func TestTaskAPIProjectListCountsStatesAndIsolation(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	running := taskStateRunning
	for _, input := range []CreateAgentTaskInput{
		{AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Queued project task", AssignedThreadID: "main"},
		{AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Running project task", State: running, AssignedThreadID: "main"},
	} {
		if _, _, err := server.store.CreateAgentTask(input); err != nil {
			t.Fatal(err)
		}
	}
	otherProject, err := server.store.CreateProject(agent.UserID, "Other task project", "", "")
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := server.store.CreateAgent(agent.UserID, "Other task agent", "Work", "autonomous", "{}", otherProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: otherAgent.ID, ProjectID: otherProject.ID, Title: "Must stay isolated",
	}); err != nil {
		t.Fatal(err)
	}

	request := authedRequest(t, http.MethodGet,
		"/api/tasks?project_id="+agent.ProjectID+"&states=queued,running", "", nil)
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"enabled":true`, `"active":2`, "Queued project task", "Running project task"} {
		if !strings.Contains(body, want) {
			t.Fatalf("project response missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Must stay isolated") {
		t.Fatalf("project task list leaked another project: %s", body)
	}
}

func TestTaskAPIAllProjectsListsOnlyProjectsVisibleToCaller(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	ownerAgent := seedTaskAgent(t, server.store)

	member, err := server.store.CreateUser("task-all-member@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.AddProjectMember(ownerAgent.ProjectID, member.ID, ProjectViewer, ownerAgent.UserID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: ownerAgent.ID, ProjectID: ownerAgent.ProjectID,
		Title: "Visible shared project task", AssignedThreadID: "main",
	}); err != nil {
		t.Fatal(err)
	}

	privateProject, err := server.store.CreateProject(ownerAgent.UserID, "Private task project", "", "")
	if err != nil {
		t.Fatal(err)
	}
	privateAgent, err := server.store.CreateAgent(
		ownerAgent.UserID, "Private task agent", "Work", "autonomous", "{}", privateProject.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: privateAgent.ID, ProjectID: privateProject.ID,
		Title: "Private project task", AssignedThreadID: "main",
	}); err != nil {
		t.Fatal(err)
	}

	request := authedRequest(t, http.MethodGet, "/api/tasks?all=1&limit=500", "", nil)
	request.Header.Set("X-User-ID", strconv.FormatInt(member.ID, 10))
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "Visible shared project task") ||
		!strings.Contains(body, `"active":1`) {
		t.Fatalf("visible task or count missing: %s", body)
	}
	if strings.Contains(body, "Private project task") {
		t.Fatalf("all-project task list leaked a project: %s", body)
	}
}

func TestTaskAPIAllProjectsRequiresAuthenticatedCaller(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks?all=1", nil)
	response := httptest.NewRecorder()
	server.handleTasks(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskAPIStepsHideRawInputsAndResults(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	agent := seedTaskAgent(t, server.store)
	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Safe step details",
		AssignedThreadID: "worker-safe",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.store.ClaimAgentTaskStep(
		agent.ID, task.ID, "lookup", "worker-safe", "crm", "contacts_get",
		"private-input-hash",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.FinishAgentTaskStep(
		agent.ID, task.ID, "lookup", "worker-safe",
		`{"secret_customer_record":"do-not-render"}`, "",
	); err != nil {
		t.Fatal(err)
	}
	request := authedRequest(t, http.MethodGet, "/api/tasks/"+task.ID+"/steps", "", nil)
	response := httptest.NewRecorder()
	server.handleTaskByID(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"step_key":"lookup"`, `"mcp_server":"crm"`, `"tool_name":"contacts_get"`, `"state":"completed"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("steps response missing %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"private-input-hash", "secret_customer_record", "result_json", "input_hash"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("steps response exposed %q: %s", forbidden, body)
		}
	}
}

func TestTaskMutationsBroadcastLiveTelemetrySnapshots(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	server := newTestServer(t)
	server.installTaskTrackingHooks()
	agent := seedTaskAgent(t, server.store)
	ch := server.broadcaster.Subscribe(agent.ID)
	defer server.broadcaster.Unsubscribe(agent.ID, ch)

	task, _, err := server.store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Live task",
		AssignedThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	progress := 35
	step := "Checking inventory"
	state := taskStateRunning
	if _, _, err := server.store.UpdateAgentTask(agent.ID, task.ID, "main", UpdateAgentTaskInput{
		State: &state, Progress: &progress, CurrentStep: &step,
	}); err != nil {
		t.Fatal(err)
	}

	var events []TelemetryEvent
	deadline := time.After(2 * time.Second)
	for len(events) < 2 {
		select {
		case event := <-ch:
			events = append(events, event)
		case <-deadline:
			t.Fatalf("live task events=%+v, want created and updated", events)
		}
	}
	if events[0].Type != "task.created" || events[1].Type != "task.updated" {
		t.Fatalf("live event types=%q,%q", events[0].Type, events[1].Type)
	}
	var payload struct {
		Task AgentTask `json:"task"`
	}
	if err := json.Unmarshal(events[1].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Task.ID != task.ID || payload.Task.State != taskStateRunning ||
		payload.Task.Progress == nil || *payload.Task.Progress != 35 ||
		payload.Task.CurrentStep != step {
		t.Fatalf("unexpected live task snapshot: %+v", payload.Task)
	}
}
