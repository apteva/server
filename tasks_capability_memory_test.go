package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskCapabilityDocumentsLedgerStatusAndDeliveryBoundaries(t *testing.T) {
	payload := taskCapabilityPayload()
	for _, required := range []string{
		taskCapabilityTag,
		taskCapabilityVersionTag,
	} {
		if !tagInList(payload.Tags, required) {
			t.Fatalf("missing tag %q in %v", required, payload.Tags)
		}
	}
	for _, required := range []string{
		"distinct from global status",
		"Do not create a task for a brief answer",
		"Main owns task assignment",
		"task record is the canonical detailed progress record",
		"Do not mirror task state or percentages into global status",
		"Every task owner, including main, uses task_run_step",
		"another wake or retry repeats the same logical step",
		"server automatically routes a structured completed, failed, or cancelled event",
		"task_spawn_worker atomically assigns",
		"delegation is mandatory",
		"This is the server-owned idempotency boundary",
		"server rejects unrelated task_create calls from main",
		"recoverable worker problem should use state=blocked",
		"creates the task assigned to main",
		"durably and automatically wakes main",
		"scheduled task is a durable parent series",
		"atomically creates exactly one normal child task",
		"does not depend on pace",
		"Exact timing belongs only in the scheduled task",
		"conversation creates exactly one scheduled task assigned to main",
		"must not create a setup task or a linked schedule",
		"uses schedule.after",
		"without waking main",
		"broader continuing role or responsibility",
		"without copying cron, interval, timestamp, or task identity",
	} {
		if !strings.Contains(payload.Content, required) {
			t.Fatalf("task capability missing %q:\n%s", required, payload.Content)
		}
	}
}

func TestTaskCapabilityMemoryDiskIsIdempotentAndRemovable(t *testing.T) {
	server := &Server{agents: NewAgentManager(t.TempDir(), "")}
	agent := &Agent{ID: 42}
	path := filepath.Join(server.agents.instanceDir(agent.ID), "memory.jsonl")
	if err := server.syncTaskCapabilityMemory(agent, true, false); err != nil {
		t.Fatal(err)
	}
	if err := server.syncTaskCapabilityMemory(agent, true, false); err != nil {
		t.Fatal(err)
	}
	records, err := journalReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("idempotent sync wrote %d records, want 1", len(records))
	}
	active, err := findActiveMemoryRecordByTagDisk(path, taskCapabilityTag)
	if err != nil || active.ID == "" {
		t.Fatalf("active task memory=%+v err=%v", active, err)
	}
	if err := server.syncTaskCapabilityMemory(agent, false, false); err != nil {
		t.Fatal(err)
	}
	active, err = findActiveMemoryRecordByTagDisk(path, taskCapabilityTag)
	if err != nil || active.ID != "" {
		t.Fatalf("disabled task memory=%+v err=%v", active, err)
	}
}
