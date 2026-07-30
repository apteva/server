package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannelsCapabilityPayloadDocumentsMainOwnedOutput(t *testing.T) {
	payload := channelsCapabilityPayload()
	if payload.ID != channelsCapabilityMemoryID {
		t.Fatalf("id=%q, want %q", payload.ID, channelsCapabilityMemoryID)
	}
	for _, want := range []string{
		channelsCapabilitySystemTag,
		channelsCapabilityTag,
		channelsCapabilityVersionTag,
	} {
		if !tagInList(payload.Tags, want) {
			t.Fatalf("missing tag %q in %v", want, payload.Tags)
		}
	}
	for _, want := range []string{
		"Main owns the agent's global operator state",
		"conversation-scoped reply capability",
		"create explicit scheduled work directly in the Tasks ledger",
		"set_status replaces the agent's compact global operational summary",
		"publish creates a central approval, report, or alert",
		"notify sends an explicitly required message to an external channel",
		"Main has no internal Apteva chat-reply capability",
		`core send(id="<originating conversation thread>"`,
		"conversation remains responsible for the visible final reply",
		"dashboard user disconnected",
		"conversation replies are already durable",
		`"STATUS QUERY — reply to this conversation:"`,
		"checking main's authoritative history",
		`id begins "chat-conv-"`,
		"It is never spare worker capacity",
		"lifecycle information only and never authorizes delegation",
		"Never proactively use core send or update",
		"Never redirect main's due work into a user conversation",
		"distinct non-conversation worker",
		`matching request that arrived from that same "[from-conversation:chat-conv-...]" source`,
		`"REPORT ONLY — no action or reply required:" message never authorizes a reply`,
		"do not evolve the directive",
		"creates one structured scheduled task assigned to main",
		"must not create a setup task or linked schedule",
		"without waking main early",
		"Exact timing belongs only in the task schedule",
		"never duplicate it in the directive, status next_at, or pace",
		"broader continuing role",
		"without cron, interval, timestamp, or task identity",
		"Do not create a persistent child thread to hold a schedule",
		"Children and other worker threads report their results and state changes to main with core send",
		"never grant agent-output tools",
		"Main consumes the child's report",
		"Main is its only writer",
		"durable task, including a server-created scheduled occurrence",
		"task is the authoritative record",
		"Every task owner, including main, uses task_run_step",
		"another wake or retry receives the stored receipt",
		"Use task_update and task_complete",
		"never mirror task state, percentage, or exact cadence",
		"Use working while a meaningful multi-step or long-running work unit is actively executing",
		"Use waiting only for an expected pause",
		"Use blocked for an unexpected failure",
		"Use completed after the meaningful work unit finishes",
		"future recurring task does not make completed work waiting",
		"title names the current work unit or completed outcome",
		"never a future action or waiting/blocking condition",
		"nearest distinct operator-relevant responsibility",
		"recurring or scheduled work, always add next_at",
		"derive the expected next occurrence from the recurrence rule and current UTC time",
		"current [CURRENT TIME] block's UTC: line",
		"Never use, infer, or convert from local wall-clock time",
		"2026-07-29T05:37:00Z",
		"2026-07-29T06:37:00Z",
		"never 08:37:00Z",
		"next_at and pace.sleep must describe the same relative interval",
		"non-recurring work, add next_at only for a known deadline",
		"next_at is display metadata and does not schedule a wake",
		"Except for a completed recurring-monitor cycle",
		"Adopting or editing a recurring schedule does not execute its work",
		"must not produce a completed status",
		"Never infer a missed or overdue run",
		"explicitly says to run now or catch up",
		"next future occurrence",
		"Every due cycle of a directive-defined recurring monitor must call set_status exactly once",
		"state=completed and a concrete result",
		"Include both next and the exact or derived next_at",
		"same original model turn, call pace exactly once",
		"successful set_status result intentionally does not wake main",
		"never schedule the same cycle twice",
		"Publish an approval only when a real decision is required",
		"alert only for an important problem",
		"report only as a substantive periodic digest",
		"Use notify only when the directive or an originating external event explicitly requires communication",
		"Internal Apteva conversation outcomes always return through core send",
	} {
		if !strings.Contains(payload.Content, want) {
			t.Fatalf("payload content missing %q:\n%s", want, payload.Content)
		}
	}
	for _, unwanted := range []string{
		"`channels_send",
		"Use channel=\"current\"",
		"Every direct `[chat]` turn",
		"acknowledgement then final",
		"Main may still use explicitly addressed external channels",
		"update main's directive with evolve",
	} {
		if strings.Contains(payload.Content, unwanted) {
			t.Fatalf("main capability retained conversation guidance %q:\n%s", unwanted, payload.Content)
		}
	}
}

func TestSyncChannelsCapabilityMemoryDiskIdempotentAndRemovable(t *testing.T) {
	s := &Server{agents: NewAgentManager(t.TempDir(), "")}
	id := int64(7)
	path := filepath.Join(s.agents.instanceDir(id), "memory.jsonl")

	if err := s.syncChannelsCapabilityMemoryDisk(id, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	active, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find active: %v", err)
	}
	if active.ID != channelsCapabilityMemoryID {
		t.Fatalf("active id=%q, want %q", active.ID, channelsCapabilityMemoryID)
	}
	recs, _ := journalReadAll(path)
	if len(recs) != 1 {
		t.Fatalf("records after first enable=%d, want 1", len(recs))
	}

	if err := s.syncChannelsCapabilityMemoryDisk(id, true); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	recs, _ = journalReadAll(path)
	if len(recs) != 1 {
		t.Fatalf("idempotent enable wrote duplicate records=%d", len(recs))
	}

	if err := s.syncChannelsCapabilityMemoryDisk(id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	active, err = findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find after disable: %v", err)
	}
	if active.ID != "" {
		t.Fatalf("active after disable=%q, want empty", active.ID)
	}

	if err := s.syncChannelsCapabilityMemoryDisk(id, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	active, err = findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find after re-enable: %v", err)
	}
	if active.ID == "" {
		t.Fatal("missing active record after re-enable")
	}
	if active.ID == channelsCapabilityMemoryID {
		t.Fatalf("re-enable reused tombstoned deterministic id %q", active.ID)
	}
}

func TestCreateInstanceSeedsChannelsCapabilityMemoryByDefault(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("channels-capability-create@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	start := false
	body, _ := json.Marshal(map[string]any{
		"name":      "chat-ready",
		"directive": "help users",
		"start":     start,
	})
	req := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleCreateInstance(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	path := filepath.Join(s.agents.instanceDir(resp.ID), "memory.jsonl")
	active, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find active: %v", err)
	}
	if active.ID == "" {
		t.Fatalf("channels capability memory missing after create; journal=%s", readFileForTest(t, path))
	}
}

func TestCreateInstanceWithChannelsDisabledDoesNotSeedCapabilityMemory(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("channels-capability-disabled@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"name":             "quiet",
		"directive":        "no channels",
		"start":            false,
		"include_channels": false,
	})
	req := httptest.NewRequest(http.MethodPost, "/instances", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleCreateInstance(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	path := filepath.Join(s.agents.instanceDir(resp.ID), "memory.jsonl")
	active, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find active: %v", err)
	}
	if active.ID != "" {
		t.Fatalf("unexpected channels capability memory with channels disabled: %q", active.ID)
	}
}

func TestSystemMCPToggleAddsAndRemovesChannelsCapabilityMemory(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("channels-capability-toggle@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.store.CreateAgent(user.ID, "toggle", "directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.syncChannelsCapabilityMemoryDisk(agent.ID, true); err != nil {
		t.Fatalf("seed: %v", err)
	}

	callToggle := func(enable bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"name": "channels", "enable": enable})
		req := httptest.NewRequest(http.MethodPost, "/instances/"+itoa64(agent.ID)+"/system-mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-ID", itoa(user.ID))
		w := httptest.NewRecorder()
		s.handleSystemMCPToggle(w, req)
		return w
	}

	w := callToggle(false)
	if w.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", w.Code, w.Body.String())
	}
	path := filepath.Join(s.agents.instanceDir(agent.ID), "memory.jsonl")
	active, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find after disable: %v", err)
	}
	if active.ID != "" {
		t.Fatalf("active after disable=%q", active.ID)
	}

	w = callToggle(true)
	if w.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", w.Code, w.Body.String())
	}
	active, err = findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		t.Fatalf("find after enable: %v", err)
	}
	if active.ID == "" {
		t.Fatalf("active missing after enable; journal=%s", readFileForTest(t, path))
	}
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(data)
}
