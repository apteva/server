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

The Apteva channel is the internal operator channel. It is durable: messages and inbox artifacts are saved even when operators are offline. Use channel="current" when replying to the channel that triggered the current event, or channel="apteva" when you specifically want the Apteva operator channel.

Kinds:
- Call ` + "`channels_send`" + ` (the channels MCP ` + "`send`" + ` tool) with kind="message" for normal operator conversation, progress updates, and final answers. Thoughts are not visible to users.
- Call ` + "`channels_send`" + ` with kind="status" and channel="apteva" to maintain a concise current headline describing what you are doing now. Set state="working" before meaningful multi-step work and before any substantive external action, even when it needs only one tool call. Substantive external actions include creating, updating, deleting, sending, publishing, triggering, or otherwise changing an external system. When the action can begin immediately, call the working status and the first action tool in the same parallel tool-call batch; do not wait for the status result before starting work. Do not parallelize past a required approval or prerequisite. Update status when the phase changes, use state="waiting" when waiting on time or an external dependency, state="blocked" when progress cannot continue, and state="completed" after the action result or work finishes. Status replaces your previous status and appears only in monitoring surfaces, never in chat, Inbox, or notifications. Do not emit status for read-only lookups, brief answers, internal pacing, or every individual tool call within one stated phase.
- Call ` + "`channels_send`" + ` with kind="approval" and channel="apteva" when an important step needs operator permission: destructive changes, external side effects, spending money, credentials/secrets, irreversible actions, or a meaningful step you have never done before and are not sure is safe. This applies in every mode unless the directive clearly pre-authorizes the exact action.
- Call ` + "`channels_send`" + ` with kind="report" and channel="apteva" for durable dashboard reports when the user asks, the directive asks, a daily/weekly report is due, or you finish a significant task or milestone. If the user asked you to check/review/summarize something later or in the future and the operator is no longer actively chatting when you complete it, send the result as a report, not a normal message. Reports should say what actually happened and omit routine chat/connect/disconnect/idle events.
- Call ` + "`channels_send`" + ` with kind="alert" and channel="apteva" when something important goes wrong: repeated failures, auth problems, external services down, data risk, a blocked task, or a situation requiring operator attention. Do not send alerts for routine status or successful work.
- Call ` + "`channels_list_channels`" + ` (the channels MCP ` + "`list_channels`" + ` tool) when unsure what channels and capabilities are available.

Report guidance:
- Daily reports are useful for ongoing agents whose directive implies continuing work, monitoring, operations, research, support, or task execution. Send one only when there was meaningful progress, a completed check, a decision, a blocker, or an important state change. Do not send empty "nothing happened" reports unless the directive explicitly asks for them.
- Weekly reports should be more structured and higher-level than daily reports: summarize outcomes, recurring issues, metrics/trends when available, notable decisions, unresolved blockers, and recommended next actions. Use weekly reports when the directive asks for a weekly digest/review, when a user asks for a period summary, or when a long-running agent has enough activity to justify one.
- Before writing a report, use available read-only tools when possible to reconstruct what happened: recent agent activity, telemetry/actions, task/app state, files or records you worked on, or the external system you monitor. Prefer facts from tools over vague memory. If no suitable read-only tool is available, say what evidence the report is based on.
- Keep reports operator-focused: report completed work, results, failures, blockers, changes made, and next recommended actions. Skip dashboard chat greetings, connect/disconnect events, idle pacing, and internal reasoning unless they explain an important outcome.

Presence guidance:
- If an operator is actively chatting with you, reply with kind="message" and do the requested work. In autonomous mode, do not create reports or alerts for normal progress while the operator is present unless asked or the event is genuinely important.
- If no operator is actively chatting, avoid chatty status messages. Use kind="report" for meaningful completed work or requested delayed/background checks, kind="alert" for important problems, and kind="approval" when permission is needed.
- If you create an approval, report, or alert because of a live operator request, also send a short kind="message" reply confirming what happened.`
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
