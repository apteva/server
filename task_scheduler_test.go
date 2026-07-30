package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTaskSchedulingFeatureFlagDefaultsWithTaskTrackingAndCanBeDisabled(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	t.Setenv("APTEVA_TASK_SCHEDULING", "")
	if !taskSchedulingEnabled() {
		t.Fatal("task scheduling should default on with task tracking")
	}
	t.Setenv("APTEVA_TASK_SCHEDULING", "0")
	if taskSchedulingEnabled() {
		t.Fatal("task scheduling disable switch was ignored")
	}
}

func TestNormalizeTaskScheduleUsesTimezoneAndDST(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	schedule, err := normalizeAgentTaskSchedule(AgentTaskScheduleInput{
		Kind: taskScheduleCron, Cron: "0 9 * * *", Timezone: "America/New_York",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := time.Date(2026, 3, 7, 14, 0, 0, 0, time.UTC)
	if !schedule.NextRunAt.Equal(wantFirst) {
		t.Fatalf("first run=%s, want %s", schedule.NextRunAt, wantFirst)
	}
	task := &AgentTask{
		ID: "task-schedule", ScheduleKind: schedule.Kind,
		ScheduleExpression: schedule.Expression, ScheduleTimezone: schedule.Timezone,
	}
	next, enabled, err := nextAgentTaskScheduleOccurrence(task, wantFirst, wantFirst)
	if err != nil || !enabled {
		t.Fatalf("next enabled=%v err=%v", enabled, err)
	}
	wantDST := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	if next == nil || !next.Equal(wantDST) {
		t.Fatalf("DST next=%v, want %s", next, wantDST)
	}
}

func TestNormalizeOneTimeScheduleAfterUsesServerTime(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	schedule, err := normalizeAgentTaskSchedule(AgentTaskScheduleInput{
		Kind: taskScheduleOnce, After: "10m",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(10 * time.Minute)
	if schedule.Expression != want.Format(time.RFC3339) || !schedule.NextRunAt.Equal(want) {
		t.Fatalf("relative schedule=%+v, want %s", schedule, want)
	}
	if _, err := normalizeAgentTaskSchedule(AgentTaskScheduleInput{
		Kind: taskScheduleOnce, At: want.Format(time.RFC3339), After: "10m",
	}, now); err == nil {
		t.Fatal("once schedule accepted both at and after")
	}
}

func TestScheduledTaskMaterializesExactlyOneChildAndAdvancesFromScheduledTime(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	parent, created, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID,
		Title: "Check Patreon posting", Description: "Verify the latest scheduled post.",
		Schedule: &AgentTaskScheduleInput{
			Kind: taskScheduleInterval, Every: "1h", Timezone: "UTC",
		},
		CreatedByThreadID: "main",
	})
	if err != nil || !created {
		t.Fatalf("create schedule: created=%v err=%v", created, err)
	}
	if parent.State != taskStateWaiting || !parent.ScheduleEnabled ||
		parent.NextRunAt == nil || parent.HandoffDeliveryStatus != "" {
		t.Fatalf("unexpected schedule parent: %+v", parent)
	}
	due := parent.NextRunAt.Add(time.Second)
	children, err := store.MaterializeDueAgentTaskSchedules(due, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("children=%d, want 1: %+v", len(children), children)
	}
	child := children[0]
	if child.ParentTaskID != parent.ID || child.ScheduledFor == nil ||
		!child.ScheduledFor.Equal(*parent.NextRunAt) ||
		child.HandoffDeliveryStatus != "pending" ||
		child.AssignedThreadID != "main" {
		t.Fatalf("unexpected occurrence: %+v", child)
	}
	again, err := store.MaterializeDueAgentTaskSchedules(due, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("same due instant created duplicate occurrences: %+v", again)
	}
	persisted, err := store.GetAgentTask(agent.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := parent.NextRunAt.Add(time.Hour)
	if persisted.NextRunAt == nil || !persisted.NextRunAt.Equal(wantNext) {
		t.Fatalf("next=%v, want anchored %s", persisted.NextRunAt, wantNext)
	}
	runs, err := store.ListAgentTaskRuns(agent.ID, parent.ID, 20)
	if err != nil || len(runs) != 1 || runs[0].ID != child.ID {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestConcurrentScheduleClaimsCannotCreateDuplicateOccurrence(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Concurrent schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	due := parent.NextRunAt.Add(time.Second)
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			children, runErr := store.MaterializeDueAgentTaskSchedules(due, 50)
			counts <- len(children)
			errs <- runErr
		}()
	}
	wg.Wait()
	close(counts)
	close(errs)
	total := 0
	for count := range counts {
		total += count
	}
	for runErr := range errs {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if total != 1 {
		t.Fatalf("concurrent materializations created %d occurrences, want 1", total)
	}
}

func TestScheduledTaskSkipsOverlappingOccurrence(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "No overlap",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDue := parent.NextRunAt.Add(time.Second)
	first, err := store.MaterializeDueAgentTaskSchedules(firstDue, 50)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	refreshed, _ := store.GetAgentTask(agent.ID, parent.ID)
	secondDue := refreshed.NextRunAt.Add(time.Second)
	second, err := store.MaterializeDueAgentTaskSchedules(secondDue, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("overlap created a second active run: %+v", second)
	}
	refreshed, _ = store.GetAgentTask(agent.ID, parent.ID)
	if refreshed.NextRunAt == nil || !refreshed.NextRunAt.After(secondDue) {
		t.Fatalf("missed catchup was not skipped: next=%v due=%s", refreshed.NextRunAt, secondDue)
	}
	events, _ := store.ListAgentTaskEvents(agent.ID, parent.ID)
	foundSkip := false
	for _, event := range events {
		foundSkip = foundSkip ||
			(event.EventType == "schedule_occurrence_skipped" && event.Data["reason"] == "overlap")
	}
	if !foundSkip {
		t.Fatalf("overlap skip event missing: %+v", events)
	}
}

func TestScheduledTaskSkipsStaleRecurringCatchupWithoutAnActiveRun(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Skip stale catchup",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredAt := parent.NextRunAt.Add(3*time.Hour + time.Second)
	children, err := store.MaterializeDueAgentTaskSchedules(recoveredAt, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("stale recurring work replayed during recovery: %+v", children)
	}
	refreshed, err := store.GetAgentTask(agent.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.NextRunAt == nil || !refreshed.NextRunAt.After(recoveredAt) {
		t.Fatalf("catchup did not advance to a future occurrence: %+v", refreshed)
	}
	events, _ := store.ListAgentTaskEvents(agent.ID, parent.ID)
	foundCatchupSkip := false
	for _, event := range events {
		foundCatchupSkip = foundCatchupSkip ||
			(event.EventType == "schedule_occurrence_skipped" && event.Data["reason"] == "catchup")
	}
	if !foundCatchupSkip {
		t.Fatalf("catchup skip event missing: %+v", events)
	}
}

func TestOverdueOneTimeTaskStillRunsAfterServerRecovery(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	at := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "One-time reminder",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleOnce, At: at},
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredAt := parent.NextRunAt.Add(24 * time.Hour)
	children, err := store.MaterializeDueAgentTaskSchedules(recoveredAt, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ScheduledFor == nil ||
		!children[0].ScheduledFor.Equal(*parent.NextRunAt) {
		t.Fatalf("overdue one-time task did not recover once: %+v", children)
	}
	refreshed, err := store.GetAgentTask(agent.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ScheduleEnabled || refreshed.NextRunAt != nil {
		t.Fatalf("one-time schedule remained enabled after recovery: %+v", refreshed)
	}
	completed, _, err := store.CompleteAgentTask(
		agent.ID, children[0].ID, "main", "Reminder sent.",
	)
	if err != nil || completed.State != taskStateCompleted {
		t.Fatalf("complete one-time occurrence: task=%+v err=%v", completed, err)
	}
	refreshed, err = store.GetAgentTask(agent.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != taskStateCompleted || refreshed.Result != "Reminder sent." ||
		refreshed.ScheduleEnabled || refreshed.NextRunAt != nil {
		t.Fatalf("one-time parent did not finish with its occurrence: %+v", refreshed)
	}
}

func TestSchedulePauseResumeAndPersistenceAcrossRestart(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	path := filepath.Join(t.TempDir(), "scheduled-tasks.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	agent := seedTaskAgent(t, store)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Persistent schedule",
		Schedule: &AgentTaskScheduleInput{
			Kind: taskScheduleCron, Cron: "0 9 * * *", Timezone: "UTC",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, changed, err := store.SetAgentTaskScheduleEnabled(
		agent.ID, parent.ID, "api:user:1", false, time.Now().UTC(),
	)
	if err != nil || !changed || paused.ScheduleEnabled || paused.NextRunAt != nil {
		t.Fatalf("pause task=%+v changed=%v err=%v", paused, changed, err)
	}
	store.Close()

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetAgentTask(agent.ID, parent.ID)
	if err != nil || persisted.ScheduleEnabled || persisted.ScheduleExpression != "0 9 * * *" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	resumed, changed, err := reopened.SetAgentTaskScheduleEnabled(
		agent.ID, parent.ID, "api:user:1", true, time.Now().UTC(),
	)
	if err != nil || !changed || !resumed.ScheduleEnabled || resumed.NextRunAt == nil {
		t.Fatalf("resume task=%+v changed=%v err=%v", resumed, changed, err)
	}
}

func TestDueScheduleDispatchUsesExistingDurableMainHandoffOnce(t *testing.T) {
	t.Setenv("APTEVA_TASK_TRACKING", "1")
	store := newTestStore(t)
	agent := seedTaskAgent(t, store)
	parent, _, err := store.CreateAgentTask(CreateAgentTaskInput{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Title: "Dispatched schedule",
		Schedule: &AgentTaskScheduleInput{Kind: taskScheduleInterval, Every: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	var delivered AgentTask
	deliver := func(_ context.Context, task *AgentTask) error {
		deliveries++
		delivered = *task
		return nil
	}
	children, err := dispatchDueAgentTaskSchedules(
		t.Context(), store, parent.NextRunAt.Add(time.Second), deliver,
	)
	if err != nil || len(children) != 1 {
		t.Fatalf("children=%+v err=%v", children, err)
	}
	if deliveries != 1 || delivered.ParentTaskID != parent.ID ||
		children[0].HandoffDeliveryStatus != "delivered" {
		t.Fatalf("deliveries=%d delivered=%+v children=%+v", deliveries, delivered, children)
	}
	if _, err := dispatchDueAgentTaskSchedules(
		t.Context(), store, parent.NextRunAt.Add(time.Second), deliver,
	); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("duplicate wake delivery count=%d", deliveries)
	}
}
