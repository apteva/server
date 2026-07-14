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
	channelsCapabilityVersionTag      = "capability-version:channels:v1"
	channelsCapabilityHashTagPrefix   = "capability-hash:"
	channelsCapabilitySystemTag       = "system"
	channelsCapabilityMemoryReason    = "channels capability sync"
	channelsCapabilityMemoryTombstone = "channels disabled"
)

func channelsCapabilityMemoryContent() string {
	return `# Channels

Use the channels MCP server to communicate with the outside world.

The Apteva channel is the durable internal operator chat. Messages and Inbox artifacts are saved even when operators are offline. Use channel="current" to reply where the current event originated, or channel="apteva" to target Apteva directly.

## Tools

- ` + "`channels_send(channel, text, components?)`" + ` sends one ordinary visible message. Thoughts and plain assistant output are invisible. Use it for conversation, immediate progress, and final outcomes.
- ` + "`channels_publish(kind, title, content, ...)`" + ` creates one durable Apteva Inbox artifact. kind is approval, report, or alert. title and content are required for every publication.
- ` + "`channels_set_status(title, state, detail?, progress?)`" + ` replaces the single current monitoring status. title and state are required. It is not chat and never appears in the Inbox.
- ` + "`channels_list_channels()`" + ` lists available communication targets. Use it instead of a send call to inspect availability.

Build every tool call once with all required fields. Never emit placeholder, preflight, partial, or duplicate calls. If one call in a parallel batch fails, retry only that failed call.

## Status

Set status to working before meaningful multi-step work or a substantive external action such as create, update, delete, send, publish, or trigger. When work can start immediately, call ` + "`channels_set_status`" + ` and the first action tool in the same parallel batch. Do not wait for the status result and do not parallelize past an approval or prerequisite.

Always pass state explicitly: working while acting, waiting for time or an external dependency, blocked when progress cannot continue, and completed after the action result or work finishes. Emit at most one status per work phase. If the request already specifies a status call, that call satisfies the rule; do not add a preliminary status. Skip status for read-only lookups, brief answers, internal pacing, and individual tools within one phase.

## Inbox Publications

Approval content states the exact decision needed, why, and the consequence. Use approval for destructive changes, spending, secrets, irreversible external effects, or a meaningful unfamiliar step not clearly pre-authorized.

Alert content states what went wrong, its impact, and the relevant next action. Use alerts for important repeated failures, authentication problems, external outages, data risk, or blocked work; not routine progress or successful work.

Report content is the report summary. Draft it before calling the tool. It must stand alone and say what actually happened: completed work, concrete results, failures or blockers, important evidence or metrics, and the next action when relevant. Never publish a title-only report, a generic description of the report, or an empty "nothing happened" report. Omit greetings, dashboard chat, connect/disconnect events, idle pacing, and internal reasoning.

Correct report call:
` + "`channels_publish(kind=\"report\", title=\"Import completed\", content=\"Imported 842 contacts; 17 invalid rows were skipped and saved for review.\", period=\"today\")`" + `

Use reports when requested, when a directive requires a daily/weekly report, after a delayed/background check, or after significant completed work the operator should review later. Daily reports cover meaningful recent outcomes. Weekly reports add trends, metrics, recurring issues, decisions, unresolved blockers, and recommended next actions. Before reporting, use available read-only tools when possible to reconstruct facts from activity, telemetry, task/app state, files, records, or the monitored external system.

## Presence

When an operator is actively chatting, use ` + "`channels_send`" + ` for replies and final outcomes. Do not create reports or alerts for normal live progress unless asked or genuinely important. If work finishes after the operator disconnects, publish a report when the result was requested for later review. If a live request creates an approval, report, or alert, also send a short chat confirmation.`
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
