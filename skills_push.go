package main

// Skills are pushed to an instance's memory journal as ordinary memory
// records carrying reserved-namespace tags. The core has no notion of
// "skill" — it sees memories with tags. This file is the only place
// the server writes that mapping.
//
// Wire shape on a pushed record:
//   id      = skillMemoryID(sk.ID)        // deterministic, lets re-push upsert
//   content = "# Name\n\nDescription\n\nBody"
//   tags    = skillTags(sk)               // see SkillTag* below
//   weight  = 0.85 (default, override via metadata.weight)
//
// Two transports:
//   - Running instance → HTTP POST /memory on the core
//   - Stopped instance → append directly to memory.jsonl (the core
//     picks it up at next boot via its own load() path)
//
// Errors are returned but never bubble to user-facing operations: a
// push that misses one instance shouldn't block the dashboard mutation
// that triggered it. Callers log and continue.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reserved tag namespace. The dashboard and the cleanup hooks both
// scan for these prefixes; everything else (manifest metadata.tags,
// agent-authored tags) is opaque passthrough.
const (
	SkillTag             = "skill"         // bucket — "this came from a skill row"
	SkillSlugTagPrefix   = "skill:"        // skill:<slug>
	SkillIDTagPrefix     = "skill-id:"     // skill-id:<row id>
	SkillSourceTagPrefix = "skill-source:" // skill-source:app|user|builtin
	SkillAppTagPrefix    = "skill-app:"    // skill-app:<app name>, only for source=app
	SkillHashTagPrefix   = "skill-hash:"   // skill-hash:<short hex>, drift detection
)

// skillMemoryID is the deterministic memory id for a skill row.
// Re-pushing the same skill upserts via Supersede. The "_0" suffix is
// a chunk index reserved for the future where a long body is split
// into multiple records (skill_<id>_0, skill_<id>_1, …).
func skillMemoryID(skillID int64) string {
	return fmt.Sprintf("skill_%d_0", skillID)
}

// skillBodyHash returns a short hex hash of a skill body. Stored as
// the skill-hash:<h> tag on every pushed record so the dashboard can
// compute drift status (synced/stale) by comparing the journal record's
// hash tag against a fresh hash of skills.body without re-fetching
// every record's full content.
func skillBodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:6]) // 12 hex chars — plenty of collision room for body equality
}

// skillTags assembles the tag set on a pushed record. Manifest-supplied
// metadata.tags pass through verbatim if present; everything else is
// platform-controlled namespace.
func skillTags(sk Skill) []string {
	tags := []string{
		SkillTag,
		SkillSlugTagPrefix + sk.Slug,
		SkillIDTagPrefix + fmt.Sprintf("%d", sk.ID),
		SkillSourceTagPrefix + sk.Source,
		SkillHashTagPrefix + skillBodyHash(sk.Body),
	}
	if sk.AppName != "" {
		tags = append(tags, SkillAppTagPrefix+sk.AppName)
	}
	if sk.Metadata != nil {
		if raw, ok := sk.Metadata["tags"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					tags = append(tags, s)
				}
			}
		}
	}
	return tags
}

// skillMemoryContent prefixes the body with the skill's name and
// description so recall can match on either, and so a record returned
// by GET /memory is human-readable in the dashboard's existing memory
// panel.
func skillMemoryContent(sk Skill) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(sk.Name)
	b.WriteString("\n\n")
	if sk.Description != "" {
		b.WriteString(sk.Description)
		b.WriteString("\n\n")
	}
	b.WriteString(sk.Body)
	return b.String()
}

// skillWeight resolves the weight for a pushed record. Default 0.85 —
// higher than the casual-memory default (0.7) so skills outrank typical
// memories on overlapping queries during recall. Manifest metadata can
// override via metadata.weight.
func skillWeight(sk Skill) float64 {
	if sk.Metadata != nil {
		if w, ok := sk.Metadata["weight"].(float64); ok && w > 0 && w <= 1 {
			return w
		}
	}
	return 0.85
}

// pushPayload is the body shape for both the HTTP POST /memory route
// (running instance) and the on-disk record (stopped instance).
type pushPayload struct {
	ID      string   `json:"id"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	Weight  float64  `json:"weight"`
	Reason  string   `json:"reason,omitempty"`
}

func skillPushPayload(sk Skill) pushPayload {
	return pushPayload{
		ID:      skillMemoryID(sk.ID),
		Content: skillMemoryContent(sk),
		Tags:    skillTags(sk),
		Weight:  skillWeight(sk),
		Reason:  "skill push: " + sk.Slug,
	}
}

// PushSkillToInstance materializes one skills row as a memory record
// on the target instance. Running → HTTP. Stopped → direct file append.
// Idempotent.
//
// Two paths converge here: re-pushes mint a fresh ULID for the new
// record and link it via Supersedes to the *currently-active* record
// for the slug (which after the first re-push is no longer the
// deterministic skill_<id>_0). We lean on the slug, not the id, as
// the stable handle — the deterministic id is only used for the very
// first push, after which supersede chains take over.
//
// Why not always use the deterministic id? Because core's tombstone
// model is per-id: once skill_<id>_0 is tombstoned, you can't revive
// it. And re-issuing Supersede(skill_<id>_0, …) repeatedly produces
// multiple active records (each new record has Supersedes=skill_<id>_0
// but a fresh id, so none of them supersede each other).
func (s *Server) PushSkillToInstance(instanceID int64, sk Skill) error {
	payload := skillPushPayload(sk)
	activeID, err := s.findActiveSkillRecordID(instanceID, sk.Slug)
	if err != nil {
		return err
	}
	if activeID != "" {
		// Re-push: target the supersede at the slug's current active
		// record, whatever its id happens to be.
		payload.ID = activeID
	}
	if s.instances.IsRunning(instanceID) {
		return s.pushPayloadHTTP(instanceID, payload)
	}
	return s.pushPayloadDisk(instanceID, payload, activeID != "")
}

// RemoveSkillFromInstance drops the currently-active memory record for
// this skill from the target instance's journal. Running → DELETE
// /memory/by-id. Stopped → append a tombstone directly. Idempotent.
//
// The skillID is used only to look up the catalog row's slug — the
// actual delete targets the journal's active record by id (which may
// be the deterministic id or a fresh ULID from a prior re-push).
func (s *Server) RemoveSkillFromInstance(instanceID int64, skillID int64, reason string) error {
	// We need the slug to find the active record. Grab the catalog
	// row; if the row was already deleted (orphan sweep path), fall
	// back to the deterministic id and rely on the journal having
	// that id from an earlier push.
	var slug string
	row := s.store.db.QueryRow(`SELECT slug FROM skills WHERE id = ?`, skillID)
	_ = row.Scan(&slug)

	targetID := skillMemoryID(skillID)
	if slug != "" {
		if id, err := s.findActiveSkillRecordID(instanceID, slug); err == nil && id != "" {
			targetID = id
		}
	}
	if s.instances.IsRunning(instanceID) {
		return s.deleteMemoryHTTP(instanceID, targetID, reason)
	}
	return s.tombstoneOnDisk(instanceID, targetID, reason)
}

// findActiveSkillRecordID returns the id of the currently-active
// memory record carrying skill:<slug>, or "" if there isn't one.
// Running instance → GET /memory via HTTP. Stopped instance → scan
// memory.jsonl on disk.
func (s *Server) findActiveSkillRecordID(instanceID int64, slug string) (string, error) {
	if s.instances.IsRunning(instanceID) {
		return s.findActiveSkillRecordIDHTTP(instanceID, slug)
	}
	return s.findActiveSkillRecordIDDisk(instanceID, slug)
}

func (s *Server) findActiveSkillRecordIDHTTP(instanceID int64, slug string) (string, error) {
	port := s.instances.GetPort(instanceID)
	coreKey := s.instances.GetCoreAPIKey(instanceID)
	if port == 0 {
		return "", fmt.Errorf("instance %d not running", instanceID)
	}
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/memory", port), nil)
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get /memory instance=%d: %w", instanceID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get /memory instance=%d: status=%d body=%s", instanceID, resp.StatusCode, string(b))
	}
	var items []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return "", err
	}
	target := SkillSlugTagPrefix + slug
	for _, it := range items {
		for _, t := range it.Tags {
			if t == target {
				return it.ID, nil
			}
		}
	}
	return "", nil
}

func (s *Server) findActiveSkillRecordIDDisk(instanceID int64, slug string) (string, error) {
	dir := s.instances.instanceDir(instanceID)
	active, err := journalActiveSkillRecords(filepath.Join(dir, "memory.jsonl"))
	if err != nil {
		return "", err
	}
	if r, ok := active[slug]; ok {
		return r.ID, nil
	}
	return "", nil
}

// ---- transport: running instance (HTTP to core) -----------------------

func (s *Server) pushPayloadHTTP(instanceID int64, payload pushPayload) error {
	port := s.instances.GetPort(instanceID)
	coreKey := s.instances.GetCoreAPIKey(instanceID)
	if port == 0 {
		return fmt.Errorf("instance %d not running", instanceID)
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/memory", port),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("push instance=%d: %w", instanceID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push instance=%d: status=%d body=%s", instanceID, resp.StatusCode, string(b))
	}
	return nil
}

func (s *Server) deleteMemoryHTTP(instanceID int64, id, reason string) error {
	port := s.instances.GetPort(instanceID)
	coreKey := s.instances.GetCoreAPIKey(instanceID)
	if port == 0 {
		return fmt.Errorf("instance %d not running", instanceID)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/memory/by-id/%s", port, id)
	if reason != "" {
		url += "?reason=" + reason
	}
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if coreKey != "" {
		req.Header.Set("Authorization", "Bearer "+coreKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete instance=%d id=%s: %w", instanceID, id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete instance=%d: status=%d body=%s", instanceID, resp.StatusCode, string(b))
	}
	return nil
}

// ---- transport: stopped instance (direct journal append) --------------

// pushPayloadDisk writes a memory record directly to the instance's
// memory.jsonl. When supersede==true, payload.ID is the id of the
// currently-active record (looked up by slug); we append a tombstone
// for it and a new record with a fresh ULID and Supersedes=payload.ID.
// When supersede==false, this is the first push: write a new record
// with payload.ID as-is (the deterministic skill_<id>_0).
func (s *Server) pushPayloadDisk(instanceID int64, payload pushPayload, supersede bool) error {
	dir := s.instances.instanceDir(instanceID)
	path := filepath.Join(dir, "memory.jsonl")

	if supersede {
		tomb := journalRecord{
			ID:        newServerULID(),
			TS:        time.Now().UTC(),
			Tombstone: true,
			IDTarget:  payload.ID,
			Reason:    "superseded by skill push (offline)",
		}
		if err := journalAppendRaw(path, tomb); err != nil {
			return err
		}
		rec := journalRecord{
			ID:         newServerULID(),
			TS:         time.Now().UTC(),
			Content:    payload.Content,
			Tags:       payload.Tags,
			Weight:     payload.Weight,
			Supersedes: payload.ID,
		}
		return journalAppendRaw(path, rec)
	}
	// First push for this slug: deterministic id, no supersede link.
	rec := journalRecord{
		ID:      payload.ID,
		TS:      time.Now().UTC(),
		Content: payload.Content,
		Tags:    payload.Tags,
		Weight:  payload.Weight,
	}
	return journalAppendRaw(path, rec)
}

// tombstoneOnDisk appends a tombstone for the given memory id to the
// stopped instance's journal. Idempotent — if the id was never on
// disk we still write the tombstone (cheap, and the next boot's
// active-set computation simply ignores tombstones with no IDTarget
// match).
func (s *Server) tombstoneOnDisk(instanceID int64, id, reason string) error {
	dir := s.instances.instanceDir(instanceID)
	path := filepath.Join(dir, "memory.jsonl")
	rec := journalRecord{
		ID:        newServerULID(),
		TS:        time.Now().UTC(),
		Tombstone: true,
		IDTarget:  id,
		Reason:    reason,
	}
	return journalAppendRaw(path, rec)
}

// ---- on-disk journal helpers ------------------------------------------
//
// We deliberately don't import core/memory here — duplicating a 5-field
// struct is cheaper than the import cycle the alternative would create
// (server already depends on core via spawn, but core depends on
// nothing from server, and we're keeping it that way).

type journalRecord struct {
	ID         string    `json:"id"`
	TS         time.Time `json:"ts"`
	Content    string    `json:"content,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Weight     float64   `json:"weight,omitempty"`
	Supersedes string    `json:"supersedes,omitempty"`
	Tombstone  bool      `json:"tombstone,omitempty"`
	IDTarget   string    `json:"id_target,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

func journalAppendRaw(path string, rec journalRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// journalReadAll reads every record in insertion order, including
// tombstones. Tolerates a torn last line (concurrent writer) by
// stopping at the first decode error — matches core/memory.go:277.
func journalReadAll(path string) ([]journalRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []journalRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var rec journalRecord
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec.ID == "" {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// journalHasID reports whether a record with this id was ever written
// to the journal (active, tombstoned, or superseded). Used by the
// stopped-instance push path to decide whether to write a tombstone
// before the new record.
func journalHasID(path, id string) (bool, error) {
	recs, err := journalReadAll(path)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// journalActiveSkillRecords returns the active (non-tombstoned,
// non-superseded) memory records that carry the SkillTag, grouped
// by the skill:<slug> tag. Used by the list/status endpoint — the
// dashboard then joins each slug back to the skills table.
//
// Reading from disk (rather than calling core's GET /memory) lets a
// single code path serve both running and stopped instances. The
// runtime is single-writer per file, so an append-while-reading race
// at worst returns N-1 records with a torn last line; the next refresh
// picks it up.
func journalActiveSkillRecords(path string) (map[string]journalRecord, error) {
	recs, err := journalReadAll(path)
	if err != nil {
		return nil, err
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
	out := map[string]journalRecord{}
	for _, r := range recs {
		if r.Tombstone || tombstoned[r.ID] || superseded[r.ID] {
			continue
		}
		isSkill := false
		var slug string
		for _, t := range r.Tags {
			if t == SkillTag {
				isSkill = true
			}
			if strings.HasPrefix(t, SkillSlugTagPrefix) {
				slug = strings.TrimPrefix(t, SkillSlugTagPrefix)
			}
		}
		if !isSkill || slug == "" {
			continue
		}
		out[slug] = r
	}
	return out, nil
}

// sweepSkillFromProject removes the given skill from every instance
// in the named project owned by the user. Best-effort: a failure on
// one instance is logged and the sweep continues. Used by uninstall
// and delete hooks where we need to clean up a skill that's about to
// disappear from the catalog.
//
// `slug` is needed because the skill row may already be gone (orphan
// sweep path) or about to be deleted in the same transaction. With
// the slug we can target by tag rather than catalog row.
func (s *Server) sweepSkillFromProject(userID int64, projectID string, skillID int64, slug, reason string) {
	insts, err := s.store.ListInstances(userID, projectID)
	if err != nil {
		// log and bail — sweep is best-effort
		fmt.Printf("[SKILLS-SWEEP] list instances for user=%d project=%q: %v\n", userID, projectID, err)
		return
	}
	for _, inst := range insts {
		// Use the slug-targeted path: find active record id by slug,
		// drop it. RemoveSkillFromInstance does this internally when
		// the catalog row has the slug. If the row is gone, fall back
		// to the deterministic id lookup.
		id, err := s.findActiveSkillRecordID(inst.ID, slug)
		if err != nil {
			fmt.Printf("[SKILLS-SWEEP] find active instance=%d slug=%s: %v\n", inst.ID, slug, err)
			continue
		}
		if id == "" {
			continue // not assigned, nothing to do
		}
		if s.instances.IsRunning(inst.ID) {
			if err := s.deleteMemoryHTTP(inst.ID, id, reason); err != nil {
				fmt.Printf("[SKILLS-SWEEP] delete instance=%d skill=%d: %v\n", inst.ID, skillID, err)
			}
		} else {
			if err := s.tombstoneOnDisk(inst.ID, id, reason); err != nil {
				fmt.Printf("[SKILLS-SWEEP] tombstone instance=%d skill=%d: %v\n", inst.ID, skillID, err)
			}
		}
	}
}

// extractTag returns the value of the first tag with the given prefix
// stripped, or "" if none. Convenience for status computation.
func extractTag(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimPrefix(t, prefix)
		}
	}
	return ""
}

// newServerULID is a small ULID-shaped id generator used for tombstones
// the server writes directly to the journal. Same shape as core's
// newULID (32 hex chars: 12 hex of unix-nano timestamp + 20 hex
// random) so loaded records sort sensibly alongside core-written ones.
func newServerULID() string {
	now := time.Now().UTC().UnixNano()
	tsHex := fmt.Sprintf("%012x", now&0xffffffffffff)
	var rnd [10]byte
	_, _ = rand.Read(rnd[:])
	return tsHex + hex.EncodeToString(rnd[:])
}
