package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- pure helpers -----------------------------------------------------

func TestSkillMemoryID_Deterministic(t *testing.T) {
	if a, b := skillMemoryID(42), skillMemoryID(42); a != b {
		t.Errorf("non-deterministic id: %s vs %s", a, b)
	}
	if skillMemoryID(1) == skillMemoryID(2) {
		t.Error("ids for different skills must differ")
	}
	if !strings.HasPrefix(skillMemoryID(7), "skill_") {
		t.Errorf("expected skill_ prefix, got %s", skillMemoryID(7))
	}
}

func TestSkillBodyHash_Stable(t *testing.T) {
	a := skillBodyHash("hello")
	b := skillBodyHash("hello")
	c := skillBodyHash("world")
	if a != b {
		t.Errorf("hash should be stable: %s vs %s", a, b)
	}
	if a == c {
		t.Error("different bodies should have different hashes")
	}
	if len(a) != 12 {
		t.Errorf("hash length = %d, want 12", len(a))
	}
}

func TestSkillTags_Composition(t *testing.T) {
	sk := Skill{
		ID:      17,
		Slug:    "storage:upload-files",
		Source:  "app",
		AppName: "storage",
		Body:    "step 1\nstep 2",
		Metadata: map[string]any{
			"tags": []any{"workflow", "io"},
		},
	}
	tags := skillTags(sk)
	want := map[string]bool{
		SkillTag:                                 false,
		SkillSlugTagPrefix + "storage:upload-files": false,
		SkillIDTagPrefix + "17":                   false,
		SkillSourceTagPrefix + "app":              false,
		SkillAppTagPrefix + "storage":             false,
		"workflow":                                false,
		"io":                                      false,
	}
	for _, tg := range tags {
		if _, ok := want[tg]; ok {
			want[tg] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing tag: %q (got %v)", k, tags)
		}
	}
	// hash tag is dynamic but its prefix should always be present.
	hasHash := false
	for _, tg := range tags {
		if strings.HasPrefix(tg, SkillHashTagPrefix) {
			hasHash = true
		}
	}
	if !hasHash {
		t.Errorf("missing skill-hash tag (got %v)", tags)
	}
}

func TestSkillTags_NoAppTagWhenAppNameEmpty(t *testing.T) {
	sk := Skill{ID: 1, Slug: "user:foo", Source: "user", Body: "x"}
	for _, tg := range skillTags(sk) {
		if strings.HasPrefix(tg, SkillAppTagPrefix) {
			t.Errorf("user skill should not carry skill-app tag, got %q", tg)
		}
	}
}

func TestSkillWeight_DefaultAndOverride(t *testing.T) {
	if w := skillWeight(Skill{}); w != 0.85 {
		t.Errorf("default weight = %v, want 0.85", w)
	}
	sk := Skill{Metadata: map[string]any{"weight": 0.5}}
	if w := skillWeight(sk); w != 0.5 {
		t.Errorf("metadata weight = %v, want 0.5", w)
	}
	// Out-of-range overrides are ignored.
	bad := Skill{Metadata: map[string]any{"weight": 1.5}}
	if w := skillWeight(bad); w != 0.85 {
		t.Errorf("out-of-range weight should fall back to default, got %v", w)
	}
}

func TestSkillMemoryContent_HasNameAndBody(t *testing.T) {
	sk := Skill{Name: "Upload files", Description: "How to upload", Body: "step 1"}
	c := skillMemoryContent(sk)
	for _, want := range []string{"# Upload files", "How to upload", "step 1"} {
		if !strings.Contains(c, want) {
			t.Errorf("content missing %q: %q", want, c)
		}
	}
}

// ---- on-disk journal helpers ------------------------------------------

func TestJournalAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")
	rec := journalRecord{ID: "id1", Content: "x", Tags: []string{"skill", "skill:foo"}, Weight: 0.7}
	if err := journalAppendRaw(path, rec); err != nil {
		t.Fatal(err)
	}
	got, err := journalReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "id1" {
		t.Errorf("read mismatch: %+v", got)
	}
}

func TestJournalReadAll_MissingFile(t *testing.T) {
	got, err := journalReadAll(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should return empty, got %v", got)
	}
}

func TestJournalReadAll_TornLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")
	if err := journalAppendRaw(path, journalRecord{ID: "ok"}); err != nil {
		t.Fatal(err)
	}
	// Append a half-written line (simulating a writer crash mid-record).
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString(`{"id":"torn","content":"unter`)
	f.Close()

	got, err := journalReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Errorf("expected only the complete record, got %+v", got)
	}
}

func TestJournalActiveSkillRecords_ExcludesTombstoned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")

	// One skill record, then a tombstone for it.
	mustAppend(t, path, journalRecord{ID: "skill_1_0", Content: "v1", Tags: []string{SkillTag, SkillSlugTagPrefix + "user:foo"}})
	mustAppend(t, path, journalRecord{ID: "tomb1", Tombstone: true, IDTarget: "skill_1_0", Reason: "test"})
	// Another skill, still active.
	mustAppend(t, path, journalRecord{ID: "skill_2_0", Content: "v2", Tags: []string{SkillTag, SkillSlugTagPrefix + "user:bar"}})

	got, err := journalActiveSkillRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["user:foo"]; ok {
		t.Error("tombstoned record should not appear in active set")
	}
	if r, ok := got["user:bar"]; !ok || r.ID != "skill_2_0" {
		t.Errorf("expected user:bar active, got %+v", got)
	}
}

func TestJournalActiveSkillRecords_ExcludesNonSkillTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")
	mustAppend(t, path, journalRecord{ID: "casual", Content: "x", Tags: []string{"preference"}})
	mustAppend(t, path, journalRecord{ID: "skill_5_0", Content: "y", Tags: []string{SkillTag, SkillSlugTagPrefix + "user:keep"}})
	got, err := journalActiveSkillRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 skill record, got %d (%+v)", len(got), got)
	}
	if _, ok := got["user:keep"]; !ok {
		t.Errorf("expected user:keep, got %+v", got)
	}
}

func TestJournalActiveSkillRecords_ExcludesSuperseded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")
	mustAppend(t, path, journalRecord{ID: "skill_1_0", Content: "v1", Tags: []string{SkillTag, SkillSlugTagPrefix + "user:foo"}})
	// New record supersedes the old, on a *different* id (e.g. core
	// would write a fresh ULID for the supersede target). The active
	// computation should skip the original.
	mustAppend(t, path, journalRecord{
		ID: "newer", Content: "v2", Supersedes: "skill_1_0",
		Tags: []string{SkillTag, SkillSlugTagPrefix + "user:foo"},
	})
	got, err := journalActiveSkillRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got["user:foo"]; !ok || r.ID != "newer" || r.Content != "v2" {
		t.Errorf("expected newer record to be active, got %+v", got)
	}
}

func TestExtractTag(t *testing.T) {
	tags := []string{SkillTag, SkillSlugTagPrefix + "user:foo", SkillHashTagPrefix + "abcd"}
	if got := extractTag(tags, SkillSlugTagPrefix); got != "user:foo" {
		t.Errorf("slug = %q, want user:foo", got)
	}
	if got := extractTag(tags, SkillHashTagPrefix); got != "abcd" {
		t.Errorf("hash = %q, want abcd", got)
	}
	if got := extractTag(tags, "skill-app:"); got != "" {
		t.Errorf("missing prefix should return empty, got %q", got)
	}
}

func TestNewServerULID_Unique(t *testing.T) {
	a, b := newServerULID(), newServerULID()
	if a == b {
		t.Error("two consecutive ulids should differ")
	}
	if len(a) != 32 {
		t.Errorf("len(ulid) = %d, want 32", len(a))
	}
}

func mustAppend(t *testing.T, path string, rec journalRecord) {
	t.Helper()
	if err := journalAppendRaw(path, rec); err != nil {
		t.Fatal(err)
	}
}
