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

func TestChannelsCapabilityPayloadDocumentsUnifiedSend(t *testing.T) {
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
		"`channels_send`",
		"kind=\"message\" for normal operator conversation",
		"kind=\"status\" and channel=\"apteva\"",
		"state=\"working\" before meaningful multi-step work",
		"before any substantive external action",
		"even when it needs only one tool call",
		"creating, updating, deleting, sending, publishing, triggering",
		"same parallel tool-call batch",
		"do not wait for the status result",
		"Do not parallelize past a required approval or prerequisite",
		"state=\"waiting\"",
		"state=\"blocked\"",
		"state=\"completed\"",
		"never in chat, Inbox, or notifications",
		"Do not emit status for read-only lookups",
		"every individual tool call within one stated phase",
		"kind=\"approval\" and channel=\"apteva\"",
		"kind=\"report\" and channel=\"apteva\"",
		"kind=\"alert\" and channel=\"apteva\"",
		"`channels_list_channels`",
		"Thoughts are not visible",
		"destructive changes",
		"daily/weekly report",
		"Daily reports are useful for ongoing agents",
		"meaningful progress",
		"Do not send empty \"nothing happened\" reports",
		"Weekly reports should be more structured",
		"metrics/trends when available",
		"Before writing a report, use available read-only tools",
		"recent agent activity",
		"telemetry/actions",
		"Prefer facts from tools over vague memory",
		"check/review/summarize something later",
		"send the result as a report, not a normal message",
		"requested delayed/background checks",
		"repeated failures",
		"If an operator is actively chatting",
		"If no operator is actively chatting",
		"omit routine chat/connect/disconnect/idle events",
		"Skip dashboard chat greetings",
	} {
		if !strings.Contains(payload.Content, want) {
			t.Fatalf("payload content missing %q:\n%s", want, payload.Content)
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
