package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
)

const (
	channelsCapabilityMemoryID        = "system_channels_v1"
	channelsCapabilityTag             = "capability:channels"
	channelsCapabilityVersionTag      = "capability-version:channels:v17"
	channelsCapabilityHashTagPrefix   = "capability-hash:"
	channelsCapabilitySystemTag       = "system"
	channelsCapabilityMemoryReason    = "channels capability sync"
	channelsCapabilityMemoryTombstone = "channels disabled"
)

func channelsCapabilityMemoryContent() string {
	return `# Main operator output

Main owns the agent's global operator state. User-facing Apteva conversations have a separate, conversation-scoped reply capability and report durable or recurring ownership changes to main with the core send tool.

## Main tools

- set_status replaces the agent's one global current-work status.
- publish creates a central approval, report, or alert Inbox artifact.
- notify sends an explicitly required message to an external channel.
- list_channels lists external notification targets.

Main has no internal Apteva chat-reply capability. When a user conversation asks main to perform durable work and requests a result, use core send(id="<originating conversation thread>", message="...") after the work. The conversation remains responsible for the visible final reply. Never publish or externally notify merely because a dashboard user disconnected; conversation replies are already durable.

## Durable ownership

When a user conversation asks this agent to adopt a simple recurring schedule or persistent behavior, main owns it: update main's directive with evolve, wait for the successful receipt, and then return the concrete result to the originating conversation with core send. Do not create a persistent child merely to hold that schedule or behavior. Use children only for bounded work that genuinely benefits from delegation unless the agent's directive explicitly defines a long-lived subdivision.

Children and other worker threads report their results and state changes to main with core send. They never own the agent's global operator output. When spawning or updating a child, never grant agent-output tools, including set_status, publish, notify, or list_channels. Main consumes the child's report and performs any required global status, Inbox publication, or external notification itself.

## Status

Status answers what meaningful operator-relevant work the agent is actively doing, waiting on, blocked by, or most recently completed. Main is its only writer.

Use working while a meaningful multi-step or long-running work unit is actively executing. Use waiting only for an expected pause with a known resume condition, including a scheduled time, operator approval, or an external job. Use blocked for an unexpected failure, missing access, or missing capability requiring corrective action. Use completed after the meaningful work unit finishes. A future recurring task does not make completed work waiting.

The title names the current work unit or completed outcome, never a future action or waiting/blocking condition. Put the current phase, concrete result, dependency, or blocker in detail. Progress measures only this work unit; never use waiting with 100 percent. Emit at most one status per meaningful phase.

Use next only for the nearest distinct operator-relevant responsibility. Add next_at only for an exact RFC3339 deadline supplied by the directive, scheduler event, operator, or an external system. Never estimate next_at from current time, expected duration, or pace. A completed recurring task may remain completed while next and next_at describe its next run.

Except for a completed recurring-monitor cycle, skip status for directive edits, thread management, configuration, planning, pacing, retries, tool discovery, brief answers, read-only lookups, isolated quick actions, publications, external notifications, and merely sleeping until future work.

Every due cycle of a directive-defined recurring monitor must call set_status exactly once after it runs, including quick or read-only checks whose result is unchanged or empty. Record state=completed and a concrete result. In the same turn call pace exactly once for the next cycle, then remain silent. A successful pace result is the scheduling receipt and must not trigger another pace call. Include next for the following cycle and include next_at only when its exact time was supplied.

## Inbox

Publish an approval only when a real decision is required, an alert only for an important problem requiring attention, and a report only as a substantive periodic digest or when explicitly requested. Reports summarize meaningful outcomes across their period; they are not receipts for every action, check, or completed task. Never publish placeholders, routine progress, unchanged checks, internal reasoning, or duplicates.

## External notification

Use notify only when the directive or an originating external event explicitly requires communication through a connected external channel. Do not notify for autonomous no-change checks, internal events, status updates, or Inbox publications. Internal Apteva conversation outcomes always return through core send to the originating conversation.`
}

func channelsCapabilityPayload() pushPayload {
	content := channelsCapabilityMemoryContent()
	return pushPayload{
		ID:      channelsCapabilityMemoryID,
		Content: content,
		Tags: []string{
			channelsCapabilitySystemTag,
			channelsCapabilityTag,
			channelsCapabilityVersionTag,
			channelsCapabilityHashTagPrefix + skillBodyHash(content),
		},
		Weight: 0.9,
		Reason: channelsCapabilityMemoryReason,
	}
}

func (s *Server) installCapabilityMemoryHooks() {
	if s == nil || s.agents == nil {
		return
	}
	s.agents.CapabilityMemorySync = func(inst *Agent, includeChannels bool, live bool) error {
		if inst == nil {
			return nil
		}
		if live {
			return s.syncChannelsCapabilityMemory(inst.ID, includeChannels)
		}
		return s.syncChannelsCapabilityMemoryDisk(inst.ID, includeChannels)
	}
}

type activeMemoryRecord struct {
	ID      string
	Content string
	Tags    []string
}

// syncChannelsCapabilityMemory keeps the server-owned Channels guidance
// memory aligned with include_channels. It is not a catalog skill: it uses
// a separate capability tag namespace so the Skills UI and skill assignment
// APIs stay focused on user/app skills.
func (s *Server) syncChannelsCapabilityMemory(instanceID int64, enabled bool) error {
	if enabled {
		return s.ensureChannelsCapabilityMemory(instanceID)
	}
	return s.removeChannelsCapabilityMemory(instanceID)
}

// syncChannelsCapabilityMemoryDisk is the startup-safe variant. It does not
// call AgentManager accessors, so AgentManager.Start can invoke it while
// holding its own lock before the core process exists.
func (s *Server) syncChannelsCapabilityMemoryDisk(instanceID int64, enabled bool) error {
	if enabled {
		return s.ensureChannelsCapabilityMemoryDisk(instanceID)
	}
	return s.removeChannelsCapabilityMemoryDisk(instanceID)
}

func (s *Server) ensureChannelsCapabilityMemory(instanceID int64) error {
	if s.agents != nil && s.agents.IsRunning(instanceID) {
		return s.ensureChannelsCapabilityMemoryHTTP(instanceID)
	}
	return s.ensureChannelsCapabilityMemoryDisk(instanceID)
}

func (s *Server) removeChannelsCapabilityMemory(instanceID int64) error {
	if s.agents != nil && s.agents.IsRunning(instanceID) {
		rec, err := s.findActiveMemoryRecordByTagHTTP(instanceID, channelsCapabilityTag)
		if err != nil {
			return err
		}
		if rec.ID == "" {
			return nil
		}
		return s.deleteMemoryHTTP(instanceID, rec.ID, channelsCapabilityMemoryTombstone)
	}
	return s.removeChannelsCapabilityMemoryDisk(instanceID)
}

func (s *Server) ensureChannelsCapabilityMemoryHTTP(instanceID int64) error {
	payload := channelsCapabilityPayload()
	rec, err := s.findActiveMemoryRecordByTagHTTP(instanceID, channelsCapabilityTag)
	if err != nil {
		return err
	}
	if memoryRecordMatchesPayload(rec, payload) {
		return nil
	}
	if rec.ID != "" {
		if err := s.deleteMemoryHTTP(instanceID, rec.ID, channelsCapabilityMemoryReason); err != nil {
			return err
		}
		if rec.ID == payload.ID {
			payload.ID = newServerULID()
		}
	}
	return s.pushPayloadHTTP(instanceID, payload)
}

func (s *Server) ensureChannelsCapabilityMemoryDisk(instanceID int64) error {
	payload := channelsCapabilityPayload()
	path := filepath.Join(s.agents.instanceDir(instanceID), "memory.jsonl")
	rec, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		return err
	}
	if memoryRecordMatchesPayload(rec, payload) {
		return nil
	}
	if rec.ID != "" {
		if err := s.tombstoneOnDisk(instanceID, rec.ID, channelsCapabilityMemoryReason); err != nil {
			return err
		}
	}
	if seen, err := journalHasID(path, payload.ID); err != nil {
		return err
	} else if seen {
		payload.ID = newServerULID()
	}
	return pushPayloadDiskAt(path, payload)
}

func (s *Server) removeChannelsCapabilityMemoryDisk(instanceID int64) error {
	path := filepath.Join(s.agents.instanceDir(instanceID), "memory.jsonl")
	rec, err := findActiveMemoryRecordByTagDisk(path, channelsCapabilityTag)
	if err != nil {
		return err
	}
	if rec.ID == "" {
		return nil
	}
	return s.tombstoneOnDisk(instanceID, rec.ID, channelsCapabilityMemoryTombstone)
}

func (s *Server) findActiveMemoryRecordByTagHTTP(instanceID int64, tag string) (activeMemoryRecord, error) {
	port := s.agents.GetPort(instanceID)
	coreKey := s.agents.GetCoreAPIKey(instanceID)
	if port == 0 {
		return activeMemoryRecord{}, fmt.Errorf("instance %d not running", instanceID)
	}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/memory", port), nil)
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return activeMemoryRecord{}, fmt.Errorf("get /memory agent=%d: %w", instanceID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return activeMemoryRecord{}, fmt.Errorf("get /memory agent=%d: status=%d body=%s", instanceID, resp.StatusCode, string(b))
	}
	var items []struct {
		ID      string   `json:"id"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return activeMemoryRecord{}, err
	}
	for _, it := range items {
		if tagInList(it.Tags, tag) {
			return activeMemoryRecord{ID: it.ID, Content: it.Content, Tags: it.Tags}, nil
		}
	}
	return activeMemoryRecord{}, nil
}

func findActiveMemoryRecordByTagDisk(path, tag string) (activeMemoryRecord, error) {
	recs, err := journalReadAll(path)
	if err != nil {
		return activeMemoryRecord{}, err
	}
	tombstoned := map[string]bool{}
	superseded := map[string]bool{}
	for _, r := range recs {
		if r.Tombstone && r.IDTarget != "" {
			tombstoned[r.IDTarget] = true
		}
		if r.Supersedes != "" {
			superseded[r.Supersedes] = true
		}
	}
	var out activeMemoryRecord
	for _, r := range recs {
		if r.Tombstone || tombstoned[r.ID] || superseded[r.ID] || !tagInList(r.Tags, tag) {
			continue
		}
		out = activeMemoryRecord{ID: r.ID, Content: r.Content, Tags: r.Tags}
	}
	return out, nil
}

func pushPayloadDiskAt(path string, payload pushPayload) error {
	rec := journalRecord{
		ID:      payload.ID,
		TS:      time.Now().UTC(),
		Content: payload.Content,
		Tags:    payload.Tags,
		Weight:  payload.Weight,
		Reason:  payload.Reason,
	}
	return journalAppendRaw(path, rec)
}

func memoryRecordMatchesPayload(rec activeMemoryRecord, payload pushPayload) bool {
	if rec.ID == "" {
		return false
	}
	wantHash := channelsCapabilityHashTagPrefix + skillBodyHash(payload.Content)
	return tagInList(rec.Tags, channelsCapabilityTag) &&
		tagInList(rec.Tags, channelsCapabilityVersionTag) &&
		tagInList(rec.Tags, wantHash)
}

func tagInList(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
