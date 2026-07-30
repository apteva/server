package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func seedTaskAgent(t *testing.T, store *Store) *Agent {
	t.Helper()
	user, err := store.CreateUser("tasks@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(user.ID, "Tasks", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(user.ID, "Task agent", "Do work", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestTaskTrackingFeatureFlagDefaultsOff(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "")
	if taskTrackingEnabled() {
		t.Fatal("task tracking must default off")
	}
	t.Setenv("APTEVA_TASK_TRACKING", "true")
	if !taskTrackingEnabled() {
		t.Fatal("true should enable task tracking")
	}
	t.Setenv("APTEVA_TASK_TRACKING", "0")
	if taskTrackingEnabled() {
		t.Fatal("0 should disable task tracking")
	}
}

func TestDeletedConversationReconcilesOwnedAndDurableTasks(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	const conversationID = "conv-deleted-task-origin"
	create := func(input CreateAgentTaskInput) *AgentTask {
		t.Helper()
		input.AgentID = agent.ID
		input.ProjectID = agent.ProjectID
		input.OriginConversationID = conversationID
		task, _, err := store.CreateAgentTask(input)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	conversationOwned := create(CreateAgentTaskInput{
		Title: "Conversation-only work", AssignedThreadID: "chat-" + conversationID,
		CreatedByThreadID: "chat-" + conversationID,
	})
	mainOwned := create(CreateAgentTaskInput{
		Title: "Durable main work", AssignedThreadID: "main",
		CreatedByThreadID: "chat-" + conversationID,
	})
	schedule := create(CreateAgentTaskInput{
		Title: "Hourly durable work", AssignedThreadID: "main",
		Schedule: &AgentTaskScheduleInput{
			Kind: taskScheduleInterval, Every: "1h", Timezone: "UTC",
		},
		CreatedByThreadID: "chat-" + conversationID,
	})
	failed := create(CreateAgentTaskInput{
		Title: "Already failed", State: taskStateFailed, AssignedThreadID: "main",
		CreatedByThreadID: "main",
	})

	reconciled, err := store.ReconcileAgentTasksForDeletedConversation(conversationID)
	if err != nil || reconciled != 4 {
		t.Fatalf("reconciled=%d err=%v", reconciled, err)
	}
	assertTask := func(id string) *AgentTask {
		t.Helper()
		task, err := store.GetAgentTask(agent.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.OriginConversationID != "" || task.OriginMessageID != "" {
			t.Fatalf("task retained deleted origin: %+v", task)
		}
		return task
	}
	cancelled := assertTask(conversationOwned.ID)
	if cancelled.State != taskStateCancelled ||
		cancelled.Error != deletedConversationTaskReason ||
		cancelled.CompletionDeliveryStatus != "discarded" {
		t.Fatalf("conversation task not cancelled safely: %+v", cancelled)
	}
	continued := assertTask(mainOwned.ID)
	if continued.State != taskStateQueued || continued.HandoffDeliveryStatus != "pending" {
		t.Fatalf("main work did not continue with its pending wake: %+v", continued)
	}
	scheduled := assertTask(schedule.ID)
	if scheduled.State != taskStateWaiting || !scheduled.ScheduleEnabled || scheduled.NextRunAt == nil {
		t.Fatalf("schedule did not continue detached: %+v", scheduled)
	}
	terminal := assertTask(failed.ID)
	if terminal.State != taskStateFailed || terminal.CompletionDeliveryStatus != "discarded" {
		t.Fatalf("terminal delivery was not discarded: %+v", terminal)
	}
	undelivered, err := store.ListUndeliveredTerminalAgentTasks(agent.ID, 20)
	if err != nil || len(undelivered) != 0 {
		t.Fatalf("deleted origin retained completion retries: %+v err=%v", undelivered, err)
	}
	again, err := store.ReconcileAgentTasksForDeletedConversation(conversationID)
	if err != nil || again != 0 {
		t.Fatalf("reconciliation is not idempotent: count=%d err=%v", again, err)
	}
	for _, task := range []*AgentTask{cancelled, continued, scheduled, terminal} {
		events, err := store.ListAgentTaskEvents(agent.ID, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		last := events[len(events)-1]
		if last.Data["previous_origin_conversation_id"] != conversationID {
			t.Fatalf("task %s lost origin audit evidence: %+v", task.ID, last)
		}
	}
}

func TestAgentTaskLifecycleIdempotencyAndEvents(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, created, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID,
		Title:                "Research durable task tracking",
		Description:          "Produce a recommendation",
		AssignedThreadID:     "worker-research",
		OriginConversationID: "conv-origin",
		IdempotencyKey:       "research-v1",
		CreatedByThreadID:    "chat-conv-origin",
	})
	if err != nil || !created {
		t.Fatalf("create task: created=%v err=%v", created, err)
	}
	if task.State != taskStateQueued || task.AssignedThreadID != "worker-research" {
		t.Fatalf("unexpected created task: %+v", task)
	}

	duplicate, duplicateCreated, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID,
		Title:          "A duplicate title should be ignored",
		IdempotencyKey: "research-v1",
	})
	if err != nil || duplicateCreated || duplicate.ID != task.ID {
		t.Fatalf("idempotent create: task=%+v created=%v err=%v", duplicate, duplicateCreated, err)
	}

	running := taskStateRunning
	progress := 40
	step := "Source review complete"
	updated, changed, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-research", UpdateAgentTaskInput{
		State: &running, Progress: &progress, CurrentStep: &step,
	})
	if err != nil || !changed {
		t.Fatalf("update task: changed=%v err=%v", changed, err)
	}
	if updated.StartedAt == nil || updated.Progress == nil || *updated.Progress != 40 {
		t.Fatalf("running metadata not recorded: %+v", updated)
	}

	completed, changed, err := store.CompleteAgentTask(agent.ID, task.ID, "worker-research", "Recommendation published")
	if err != nil || !changed {
		t.Fatalf("complete task: changed=%v err=%v", changed, err)
	}
	if completed.State != taskStateCompleted || completed.CompletedAt == nil ||
		completed.CompletionDeliveryStatus != "pending" ||
		completed.CurrentStep != "" {
		t.Fatalf("completion metadata not recorded: %+v", completed)
	}

	changedAfterCompletion := "This must not replace the terminal step"
	_, _, err = store.UpdateAgentTask(agent.ID, task.ID, "worker-research", UpdateAgentTaskInput{
		CurrentStep: &changedAfterCompletion,
	})
	if !errors.Is(err, errTaskTerminal) {
		t.Fatalf("terminal mutation error=%v, want errTaskTerminal", err)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("event count=%d, want 3: %+v", len(events), events)
	}
	if events[0].EventType != "created" ||
		events[1].EventType != "state_changed" ||
		events[2].ToState != taskStateCompleted {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestAgentTaskUpdateWaitsForParallelWriterInsteadOfReturningBusy(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Concurrent progress",
		AssignedThreadID: "main", CreatedByThreadID: "api:user:1",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	writer, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	writerOpen := true
	defer func() {
		if writerOpen {
			_, _ = writer.ExecContext(ctx, "ROLLBACK")
		}
	}()

	type updateResult struct {
		task *AgentTask
		err  error
	}
	result := make(chan updateResult, 1)
	go func() {
		running, progress, step := taskStateRunning, 20, "Reviewing records"
		updated, _, updateErr := store.UpdateAgentTask(
			agent.ID,
			task.ID,
			"main",
			UpdateAgentTaskInput{
				State: &running, Progress: &progress, CurrentStep: &step,
			},
		)
		result <- updateResult{task: updated, err: updateErr}
	}()

	// A deferred read-then-write transaction returns SQLITE_BUSY here instead
	// of honoring busy_timeout. An immediate transaction waits for this writer.
	select {
	case early := <-result:
		t.Fatalf("task update returned before the writer committed: task=%+v err=%v", early.task, early.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := writer.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	writerOpen = false

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("task update failed after writer released: %v", got.err)
		}
		if got.task == nil || got.task.State != taskStateRunning ||
			got.task.Progress == nil || *got.task.Progress != 20 ||
			got.task.CurrentStep != "Reviewing records" {
			t.Fatalf("progress milestone was not preserved: %+v", got.task)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task update did not resume after writer released")
	}
}

func TestAgentTasksPersistAcrossStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Survive restart",
		AssignedThreadID: "main", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Title != "Survive restart" || persisted.AssignedThreadID != "main" {
		t.Fatalf("unexpected persisted task: %+v", persisted)
	}
	events, err := reopened.ListAgentTaskEvents(agent.ID, task.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("persisted events=%d err=%v", len(events), err)
	}
}

func TestAgentTaskCountsSeparateSchedulesAndAccumulateTerminalGroups(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	normal, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Normal cancellation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CancelAgentTask(agent.ID, normal.ID, "main", "No longer needed"); err != nil {
		t.Fatal(err)
	}
	cancelledSchedule, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Cancelled schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CancelAgentTask(agent.ID, cancelledSchedule.ID, "main", "Stop future runs"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Active schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleCron, Cron: "0 9 * * *"},
	}); err != nil {
		t.Fatal(err)
	}
	paused, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Paused schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SetAgentTaskScheduleEnabled(
		agent.ID, paused.ID, "main", false, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}

	counts, err := store.CountProjectAgentTasks(agent.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Cancelled != 2 || counts.Scheduled != 1 || counts.Paused != 1 ||
		counts.Active != 0 || counts.Waiting != 0 {
		t.Fatalf("unexpected mixed task counts: %+v", counts)
	}
}

func TestAgentTaskTelemetryOnlyTouchesActivity(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Tracked worker",
		AssignedThreadID: "worker-1", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAgentTasksByThread(agent.ID, "worker-1", task.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	touched, err := store.GetAgentTask(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if touched.LastActivityAt == nil || touched.State != taskStateQueued {
		t.Fatalf("activity must not mutate state: %+v", touched)
	}
}

func TestAgentTaskProgressBlockAndReassignmentPreserveOneHistory(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	task, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Multi-step import",
		AssignedThreadID: "worker-a", OriginConversationID: "conv-import",
		CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	running := taskStateRunning
	progress10, step10 := 10, "Validated source files"
	if _, _, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-a", UpdateAgentTaskInput{
		State: &running, Progress: &progress10, CurrentStep: &step10,
	}); err != nil {
		t.Fatal(err)
	}
	progress60, step60 := 60, "Imported customer records"
	if _, _, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-a", UpdateAgentTaskInput{
		Progress: &progress60, CurrentStep: &step60,
	}); err != nil {
		t.Fatal(err)
	}
	blocked, blocker, blockedStep := taskStateBlocked, "rate limit exhausted", "Waiting for a fresh quota window"
	if _, _, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-a", UpdateAgentTaskInput{
		State: &blocked, Error: &blocker, CurrentStep: &blockedStep,
	}); err != nil {
		t.Fatal(err)
	}
	queued, workerB, retryStep := taskStateQueued, "worker-b", "Retry with refreshed quota"
	reassigned, _, err := store.UpdateAgentTask(agent.ID, task.ID, "main", UpdateAgentTaskInput{
		State: &queued, AssignedThreadID: &workerB, CurrentStep: &retryStep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reassigned.AssignedThreadID != "worker-b" || reassigned.State != taskStateQueued {
		t.Fatalf("unexpected reassignment: %+v", reassigned)
	}
	progress80, resumedStep := 80, "Verified imported records"
	if _, _, err := store.UpdateAgentTask(agent.ID, task.ID, "worker-b", UpdateAgentTaskInput{
		State: &running, Progress: &progress80, CurrentStep: &resumedStep,
	}); err != nil {
		t.Fatal(err)
	}
	completed, _, err := store.CompleteAgentTask(agent.ID, task.ID, "worker-b", "Import completed and verified")
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != taskStateCompleted || completed.Progress == nil || *completed.Progress != 100 {
		t.Fatalf("unexpected completion: %+v", completed)
	}
	events, err := store.ListAgentTaskEvents(agent.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("events=%d, want create + 6 lifecycle events: %+v", len(events), events)
	}
	var sawBlocked, sawReassignment, sawCompletion bool
	for _, event := range events {
		sawBlocked = sawBlocked || event.ToState == taskStateBlocked
		sawReassignment = sawReassignment || event.Data["assigned_thread_id"] == "worker-b"
		sawCompletion = sawCompletion || event.ToState == taskStateCompleted
	}
	if !sawBlocked || !sawReassignment || !sawCompletion {
		t.Fatalf("missing lifecycle evidence blocked=%v reassigned=%v completed=%v: %+v",
			sawBlocked, sawReassignment, sawCompletion, events)
	}
}

func TestAgentTaskConcurrentIdempotentCreateProducesOneTask(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	const callers = 16
	var wg sync.WaitGroup
	wg.Add(callers)
	ids := make(chan string, callers)
	created := make(chan bool, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			task, wasCreated, err := store.CreateAgentTask(CreateAgentTaskInput{
				AgentID: agent.ID, ProjectID: agent.ProjectID,
				Title:            "One logical concurrent task",
				AssignedThreadID: "main", IdempotencyKey: "concurrent-task",
				CreatedByThreadID: "main",
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- task.ID
			created <- wasCreated
		}()
	}
	wg.Wait()
	close(ids)
	close(created)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent create: %v", err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	if len(unique) != 1 || createdCount != 1 {
		t.Fatalf("unique ids=%v created_count=%d", unique, createdCount)
	}
	tasks, err := store.ListAgentTasks(agent.ID, AgentTaskListFilter{Limit: 100})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

func TestAgentTaskMultipleWorkersRemainIsolated(t *testing.T) {
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	taskA, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Task A",
		AssignedThreadID: "worker-a", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskB, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Task B",
		AssignedThreadID: "worker-b", CreatedByThreadID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	workerServer := &taskMCPServer{store: store, agent: agent, profile: taskMCPProfileWorker}
	denied := callTaskMCP(t, workerServer, "task_update", map[string]any{
		"_apteva_caller_context": "worker-a",
		"task_id":                taskB.ID, "state": "running",
	})
	if isError, _ := denied["isError"].(bool); !isError {
		t.Fatalf("worker-a mutated worker-b task: %#v", denied)
	}
	allowed := callTaskMCP(t, workerServer, "task_update", map[string]any{
		"_apteva_caller_context": "worker-a",
		"task_id":                taskA.ID, "state": "running", "progress": 20,
	})
	if isError, _ := allowed["isError"].(bool); isError {
		t.Fatalf("worker-a could not mutate its task: %#v", allowed)
	}
	listedA, err := store.ListAgentTasks(agent.ID, AgentTaskListFilter{AssignedThread: "worker-a"})
	if err != nil || len(listedA) != 1 || listedA[0].ID != taskA.ID {
		t.Fatalf("worker-a tasks=%+v err=%v", listedA, err)
	}
}
