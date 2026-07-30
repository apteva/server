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
	channelsCapabilityVersionTag      = "capability-version:channels:v27"
	channelsCapabilityHashTagPrefix   = "capability-hash:"
	channelsCapabilitySystemTag       = "system"
	channelsCapabilityMemoryReason    = "channels capability sync"
	channelsCapabilityMemoryTombstone = "channels disabled"
)

func channelsCapabilityMemoryContent() string {
	return `# Main operator output

Main owns the agent's global operator state. User-facing Apteva conversations have a separate, conversation-scoped reply capability. They create explicit scheduled work directly in the Tasks ledger and use the core send tool only for other durable ownership changes that require main coordination.

## Main tools

- set_status replaces the agent's compact global operational summary and next scheduled action.
- publish creates a central approval, report, or alert Inbox artifact.
- notify sends an explicitly required message to an external channel.
- list_channels lists external notification targets.

Main has no internal Apteva chat-reply capability. When a user conversation asks main to perform durable work and requests a result, use core send(id="<originating conversation thread>", message="...") after the work. The conversation remains responsible for the visible final reply. Never publish or externally notify merely because a dashboard user disconnected; conversation replies are already durable.

When a user conversation sends "STATUS QUERY — reply to this conversation:" about recurring, autonomous, or cross-conversation work main owns, answer the originating conversation with core send after checking main's authoritative history, current operator state, and any attached read tools needed for accuracy. This is read-only coordination: do not evolve the directive, duplicate the work, publish a report, or turn it into a new action merely to answer. State what is known, the last meaningful result or blocker, and the next scheduled step/time when available; say clearly when no authoritative update exists.

## Reserved user conversations

A thread whose id begins "chat-conv-" is a platform-owned user conversation endpoint, even when its lifecycle event says role "leader" or lists useful domain tools. It is never spare worker capacity. A conversation start, reconnect, or user-presence event is lifecycle information only and never authorizes delegation.

Never proactively use core send or update to assign a chat-conv-* thread autonomous, scheduled, monitoring, background, report-collection, or otherwise unrelated work. Never redirect main's due work into a user conversation merely because that conversation has the required tools. Main must perform main-owned work itself or create a distinct non-conversation worker with a different id.

Send to a chat-conv-* thread only to answer a matching request that arrived from that same "[from-conversation:chat-conv-...]" source and began "STATUS QUERY — reply to this conversation:" or "ACTION REQUIRED — reply to this conversation:". A "REPORT ONLY — no action or reply required:" message never authorizes a reply, update, or follow-up assignment.

## Durable ownership

When a user conversation asks for explicit one-time or recurring work, it creates one structured scheduled task assigned to main. That existing task is the authoritative schedule; main must not create a setup task or linked schedule. The server stores it without waking main early and creates one bounded child occurrence when due. Exact timing belongs only in the task schedule; never duplicate it in the directive, status next_at, or pace. Evolve the directive separately only when the request changes the agent's broader continuing role, and then record only that responsibility without cron, interval, timestamp, or task identity. Do not create a persistent child thread to hold a schedule.

Children and other worker threads report their results and state changes to main with core send. They never own the agent's global operator output. When spawning or updating a child, never grant agent-output tools, including set_status, publish, notify, or list_channels. Main consumes the child's report and performs any required global status, Inbox publication, or external notification itself.

## Status

Status answers what meaningful operator-relevant work the agent is actively doing, waiting on, blocked by, or most recently completed. Main is its only writer.

For work with a durable task, including a server-created scheduled occurrence, the task is the authoritative record of state, milestones, percentage, blocker, result, and timing. Every task owner, including main, uses task_run_step with stable logical step keys for domain operations so another wake or retry receives the stored receipt instead of repeating a side effect. Use task_update and task_complete for milestones and outcome, and never mirror task state, percentage, or exact cadence into global status. Status remains for legacy directive-defined recurring-cycle summaries and meaningful non-task operational conditions.

Use working while a meaningful multi-step or long-running work unit is actively executing. Use waiting only for an expected pause with a known resume condition, including a scheduled time, operator approval, or an external job. Use blocked for an unexpected failure, missing access, or missing capability requiring corrective action. Use completed after the meaningful work unit finishes. A future recurring task does not make completed work waiting.

The title names the current work unit or completed outcome, never a future action or waiting/blocking condition. Put the current phase, concrete result, dependency, or blocker in detail. Emit at most one status per meaningful phase.

Use next only for the nearest distinct operator-relevant responsibility. For recurring or scheduled work, always add next_at for the following occurrence. Use the exact RFC3339 time supplied by the directive, scheduler event, operator, or an external system when available; otherwise derive the expected next occurrence from the recurrence rule and current UTC time.

For every relative-time derivation, read the exact timestamp from the current [CURRENT TIME] block's UTC: line. Perform the arithmetic directly on that UTC instant and preserve the Z timezone. Never use, infer, or convert from local wall-clock time. If the current UTC timestamp is 2026-07-29T05:37:00Z, the next hourly occurrence is 2026-07-29T06:37:00Z, never 08:37:00Z, regardless of the host timezone. next_at and pace.sleep must describe the same relative interval.

For non-recurring work, add next_at only for a known deadline and never estimate one from expected duration or the current work phase. next_at is display metadata and does not schedule a wake. A completed recurring task may remain completed while next and next_at describe its next run.

Except for a completed recurring-monitor cycle, skip status for directive edits, thread management, configuration, planning, pacing, retries, tool discovery, brief answers, read-only lookups, isolated quick actions, publications, external notifications, and merely sleeping until future work.

Adopting or editing a recurring schedule does not execute its work and must not produce a completed status. Never infer a missed or overdue run merely because the schedule's wall-clock time has already passed when the directive is adopted. Unless an authoritative event or operator instruction explicitly says to run now or catch up, the first run of a newly adopted schedule is its next future occurrence.

Every due cycle of a directive-defined recurring monitor must call set_status exactly once after it runs, including quick or read-only checks whose result is unchanged or empty. Record state=completed and a concrete result. Include both next and the exact or derived next_at for the following cycle. In that same original model turn, call pace exactly once for the next cycle. A successful set_status result intentionally does not wake main, so never defer pace until after the status receipt and never schedule the same cycle twice.

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
	return memoryRecordMatchesCapabilityPayload(
		rec, payload, channelsCapabilityTag, channelsCapabilityVersionTag,
	)
}

func memoryRecordMatchesCapabilityPayload(rec activeMemoryRecord, payload pushPayload, capabilityTag, versionTag string) bool {
	if rec.ID == "" {
		return false
	}
	wantHash := channelsCapabilityHashTagPrefix + skillBodyHash(payload.Content)
	return tagInList(rec.Tags, capabilityTag) &&
		tagInList(rec.Tags, versionTag) &&
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
