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
	channelsCapabilityVersionTag      = "capability-version:channels:v11"
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

- ` + "`channels_send(channel, text, components?)`" + ` sends one ordinary visible message. Thoughts and plain assistant output are invisible. Use it for direct conversation, a brief initial acknowledgement, requested progress, and requested final outcomes.
- ` + "`channels_publish(kind, title, content, ...)`" + ` creates one durable Apteva Inbox artifact. kind is approval, report, or alert. title and content are required for every publication.
- ` + "`channels_set_status(title, state, detail?, progress?, next?, next_at?)`" + ` replaces the single current monitoring status. title and state are required. It is not chat and never appears in the Inbox.
- ` + "`channels_list_channels()`" + ` lists available communication targets. Use it instead of a send call to inspect availability.

Build every tool call once with all required fields. Never emit placeholder, preflight, partial, or duplicate calls. If one call in a parallel batch fails, retry only that failed call.

A successful ` + "`channels_send`" + ` result means that exact visible message is already delivered. The result wake is not a request to repeat it. If the message was a brief acknowledgement that explicitly promised concrete unfinished work, continue that work and send exactly one final outcome afterward. Otherwise the message satisfies the current chat turn and you should call ` + "`pace`" + ` or ` + "`done`" + `.

## Ordinary Chat

Use ` + "`channels_send`" + ` for a direct ` + "`[chat]`" + ` turn and for the later outcome of work explicitly requested in that chat. The reply remains valid if the operator disconnects before the work finishes because Apteva chat is durable.

Internal Apteva chat replies are conversation-scoped. If your current core thread is main or any non-conversation worker, do not call ` + "`channels_send(channel=\"current\"|\"apteva\", ...)`" + `: use core ` + "`send(id=\"<originating chat thread>\", message=\"...\")`" + ` to return the result, and let that conversation send the visible reply. Main may still use explicitly addressed external channels and the separate status/publication tools.

For a direct ` + "`[chat]`" + ` turn that requires tools, including dashboard conversation threads, prefer a short visible acknowledgement before beginning tool work so the operator immediately knows the concrete next action. This is strong guidance, not a hard requirement: skip it when the complete answer can be sent immediately or when an acknowledgement would be empty or repetitive. When you do acknowledge, wait for that send to succeed, then perform the promised tool work; never send the acknowledgement in parallel with action tools. After the work, send exactly one final outcome. An earlier acknowledgement never replaces that final. A tool-work turn therefore has either one complete final message or two intentional messages—acknowledgement then final—and never more. For durable work that must be handed to main with ` + "`send`" + ` so it survives the conversation, prefer the acknowledgement before the handoff. If it was skipped, at most one brief acknowledgement may follow the successful handoff receipt. Never acknowledge both before and after the handoff. The main receipt confirms delivery only, not completion. After main replies, send exactly one final outcome and no additional progress message.

Outside a direct ` + "`[chat]`" + ` request, send an ordinary message only when the operator or directive explicitly asks for a chat message at that time. Do not use ordinary chat for autonomous or scheduled checks, routine monitoring, unchanged or no-op results, idle updates, repeated progress, connect/disconnect events, or internal/system events. A phrase such as "send a status update" or "update the status" means ` + "`channels_set_status`" + ` unless it explicitly asks for a chat message.

When recurring autonomous work finds no meaningful change, update ` + "`channels_set_status`" + ` at most once if the completed work qualifies for status, then call ` + "`pace`" + ` for the next due check and remain silent. ` + "`next_at`" + ` is display metadata and does not schedule the next wake. Use ` + "`channels_publish`" + ` only when the result genuinely qualifies as an approval, report, or alert.

## Status

Status answers: what meaningful operator-relevant work is this agent actively doing, waiting on, blocked by, or most recently completed? Use it for a work unit that is multi-step, long-running, or cannot currently continue. When qualifying work can start immediately, call ` + "`channels_set_status`" + ` and the first action tool in the same parallel batch. Do not wait for the status result and do not parallelize past an approval or prerequisite.

For qualifying work, always call ` + "`channels_set_status`" + ` at meaningful phase changes; do not merely describe the state in thoughts or chat. If an event reports that qualifying work you performed has completed, begun waiting, or become blocked, update status even when no other action tool remains.

Always pass state explicitly. Use working while the work unit is actively executing. Use waiting only for an expected pause in that same unfinished work unit whose resume condition is known, including a scheduled time, operator approval, or an external job. Use blocked for an unexpected failure, missing access, or missing capability that requires corrective action before work can resume; do not use blocked for ordinary approval or a scheduled delay. Use completed after the meaningful work unit finishes. A future recurring task does not make completed work waiting.

The title names the current work unit or completed outcome, not a future action or a waiting/blocking condition. Put the current phase, concrete result, dependency, or blocker in detail. Prefer title="Customer update publication" over "Waiting for approval", title="Delayed notification" over "Notification scheduled", and title="CRM contact import" over "CRM import blocked". Progress measures only this work unit; never use waiting with 100 percent. Emit at most one status per work phase. If the request already specifies a status call, that call satisfies this rule; do not add a preliminary status.

Skip status for directive or memory edits, thread management, internal configuration, planning, pacing, retries, tool discovery, chat connectivity, channel messages or publications, status maintenance itself, read-only lookups, brief answers, isolated quick actions, and merely sleeping until future recurring work. Do not set a status just to announce what you may do later.

Use next for only the nearest distinct operator-relevant responsibility after this work or phase. It is secondary metadata and must not replace the current title or detail. Never use placeholders such as "No pending work", restate the title, or record internal pacing. Add next_at only for an explicit or externally derived RFC3339 deadline for next. Never send next_at without next, and never estimate it from current time. A completed recurring task may remain completed while next and next_at describe its next scheduled run. Omit both when nothing meaningful is planned; a replacement without them clears the previous next action.

Examples:
- Completed recurring work: ` + "`channels_set_status(title=\"Daily CRM check completed\", state=\"completed\", detail=\"No unresolved conversations found.\", progress=100, next=\"Run the next daily CRM check.\", next_at=\"2026-07-20T09:00:00Z\")`" + `
- Expected approval: ` + "`channels_set_status(title=\"Customer update publication\", state=\"waiting\", detail=\"Draft is ready and requires operator approval.\", progress=70, next=\"Publish customer update after approval.\")`" + `
- Corrective failure: ` + "`channels_set_status(title=\"CRM contact import\", state=\"blocked\", detail=\"Authentication expired; reconnect the integration to resume.\")`" + `

## Inbox Publications

Approval content states the exact decision needed, why, and the consequence. Use approval for destructive changes, spending, secrets, irreversible external effects, or a meaningful unfamiliar step not clearly pre-authorized.

Alert content states what went wrong, its impact, and the relevant next action. Use alerts for important repeated failures, authentication problems, external outages, data risk, or blocked work; not routine progress or successful work.

Report content is a periodic digest of meaningful work across its reporting period. Draft it before calling the tool. It must stand alone and combine completed work, concrete results, failures or blockers, important evidence or metrics, and the next action when relevant. Reports are not action receipts: never publish one after each check, tool call, cleanup, or completed task. Use ` + "`channels_set_status`" + ` for work state and ` + "`channels_send`" + ` for a requested task's direct outcome. Never publish a title-only report, a generic description of the report, or an empty "nothing happened" report. Omit greetings, dashboard chat, connect/disconnect events, idle pacing, and internal reasoning.

Correct report call:
` + "`channels_publish(kind=\"report\", title=\"Daily work summary\", content=\"Imported 842 contacts, cleared 12 routine inbox items, and preserved 3 messages requiring review. Seventeen invalid contact rows remain for follow-up.\", period=\"today\")`" + `

Follow an explicit operator request or directive when it defines report timing. Otherwise publish at most one unsolicited report per day, near the end of the operator's day, and only when meaningful work was completed since the previous report. Combine the day's work into one digest with period=today; if no meaningful work was done, publish no report. Daily reports summarize meaningful outcomes across the day. Weekly reports add trends, metrics, recurring issues, decisions, unresolved blockers, and recommended next actions. Before reporting, use available read-only tools when possible to reconstruct facts from activity, telemetry, task/app state, files, records, or the monitored external system.

## Presence

Every direct ` + "`[chat]`" + ` turn requires at least one successful ` + "`channels_send`" + ` before you call pace, finish, or otherwise go idle. The turn is incomplete until its user-visible answer is sent. Thoughts and plain assistant output do not count. Reply visibly even when you only need to ask a clarifying question, report a read-only lookup, explain that you cannot act, or say no action was needed.

After any non-channel tool result used for the request, including a read-only lookup, send the outcome, clarification, blocker, or next question through ` + "`channels_send`" + ` before pacing. An earlier acknowledgement does not replace this final outcome. A successful ` + "`channels_send`" + ` result is the delivery receipt for the exact message already sent, not a new outcome to report. Never repeat that message. Never leave the user-facing answer only in thoughts.

For a direct ` + "`[chat]`" + ` turn, never call ` + "`pace`" + ` or ` + "`done`" + ` while a visible reply is still owed. After a lookup or other tool result, the next action must be the required ` + "`channels_send`" + ` outcome—not pacing, another idle action, or invisible plain output.

The Apteva chat is durable, so use ` + "`channels_send`" + ` for a requested outcome even if the operator disconnected before work finished. Offline presence alone does not suppress a reply to requested work, but it does not justify unsolicited chat from autonomous work. Do not turn offline completion into a report automatically. Do not create reports or alerts for normal live progress unless asked or genuinely important. If a live request creates an approval, report, or alert, also send a short chat confirmation.`
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
