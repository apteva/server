package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// computeInstanceSkillView is the pure status logic — tested without
// touching the DB or filesystem.

func TestComputeInstanceSkillView_AllStatuses(t *testing.T) {
	syncedBody := "synced body"
	staleBodyOld := "old body"
	staleBodyNew := "new body"

	cat := []Skill{
		{ID: 1, Slug: "user:synced", Name: "Synced", Source: "user", Body: syncedBody},
		{ID: 2, Slug: "user:stale", Name: "Stale", Source: "user", Body: staleBodyNew},
		{ID: 3, Slug: "user:missing", Name: "Missing", Source: "user", Body: "x"},
	}
	active := map[string]journalRecord{
		"user:synced": {
			ID:   "skill_1_0",
			TS:   time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
			Tags: []string{SkillTag, SkillSlugTagPrefix + "user:synced", SkillHashTagPrefix + skillBodyHash(syncedBody)},
		},
		"user:stale": {
			ID:   "skill_2_0",
			TS:   time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
			Tags: []string{SkillTag, SkillSlugTagPrefix + "user:stale", SkillHashTagPrefix + skillBodyHash(staleBodyOld)},
		},
		"user:orphan": {
			ID: "skill_99_0",
			TS: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
			Tags: []string{
				SkillTag,
				SkillSlugTagPrefix + "user:orphan",
				SkillSourceTagPrefix + "user",
			},
		},
	}

	got := computeInstanceSkillView(cat, active)
	byslug := map[string]instanceSkillView{}
	for _, v := range got {
		byslug[v.Slug] = v
	}
	if byslug["user:synced"].Status != "synced" {
		t.Errorf("user:synced status = %q, want synced", byslug["user:synced"].Status)
	}
	if byslug["user:stale"].Status != "stale" {
		t.Errorf("user:stale status = %q, want stale", byslug["user:stale"].Status)
	}
	if byslug["user:missing"].Status != "missing" {
		t.Errorf("user:missing status = %q, want missing", byslug["user:missing"].Status)
	}
	if byslug["user:orphan"].Status != "orphaned" {
		t.Errorf("user:orphan status = %q, want orphaned", byslug["user:orphan"].Status)
	}
	if byslug["user:missing"].MemoryID != "" {
		t.Errorf("missing rows should not carry a memory_id, got %q", byslug["user:missing"].MemoryID)
	}
	if byslug["user:synced"].MemoryID != "skill_1_0" {
		t.Errorf("synced row should carry the journal record id, got %q", byslug["user:synced"].MemoryID)
	}
	if byslug["user:orphan"].SkillID != 0 {
		t.Errorf("orphan rows have no catalog ID, got %d", byslug["user:orphan"].SkillID)
	}
	if byslug["user:orphan"].Source != "user" {
		t.Errorf("orphan source should come from journal tag, got %q", byslug["user:orphan"].Source)
	}
}

func TestComputeInstanceSkillView_EmptyCatalog(t *testing.T) {
	active := map[string]journalRecord{
		"user:lone": {
			ID: "skill_5_0", TS: time.Now(),
			Tags: []string{SkillTag, SkillSlugTagPrefix + "user:lone"},
		},
	}
	got := computeInstanceSkillView(nil, active)
	if len(got) != 1 || got[0].Status != "orphaned" {
		t.Errorf("expected single orphaned row, got %+v", got)
	}
}

func TestComputeInstanceSkillView_EmptyJournal(t *testing.T) {
	cat := []Skill{
		{ID: 1, Slug: "user:a", Body: "x"},
		{ID: 2, Slug: "user:b", Body: "y"},
	}
	got := computeInstanceSkillView(cat, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	for _, v := range got {
		if v.Status != "missing" {
			t.Errorf("%s status = %q, want missing", v.Slug, v.Status)
		}
	}
}

// ---- end-to-end stopped-instance push: skill row → journal -----------

func TestPushSkillToInstance_StoppedWritesToJournal(t *testing.T) {
	s := newTestServer(t)
	inst, err := s.store.CreateAgent(1, "test", "directive", "autonomous", "{}", "proj-a")
	if err != nil {
		t.Fatal(err)
	}
	sk := Skill{
		ID:      42,
		Slug:    "user:morning-routine",
		Name:    "Morning routine",
		Source:  "user",
		Body:    "## checklist\n- step 1\n- step 2\n",
		Enabled: true,
	}
	if err := s.PushSkillToInstance(inst.ID, sk); err != nil {
		t.Fatalf("PushSkillToInstance: %v", err)
	}

	// Verify the journal contains the expected record.
	dir := s.agents.instanceDir(inst.ID)
	active, err := journalActiveSkillRecords(dir + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := active["user:morning-routine"]
	if !ok {
		t.Fatalf("expected slug user:morning-routine in journal, got %+v", active)
	}
	if rec.ID != skillMemoryID(42) {
		t.Errorf("journal id = %q, want %q", rec.ID, skillMemoryID(42))
	}
	if extractTag(rec.Tags, SkillHashTagPrefix) != skillBodyHash(sk.Body) {
		t.Errorf("journal hash tag missing or mismatched: %v", rec.Tags)
	}
}

func TestPushSkillToInstance_StoppedUpsertsOnRePush(t *testing.T) {
	s := newTestServer(t)
	inst, err := s.store.CreateAgent(1, "test", "directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	skV1 := Skill{ID: 7, Slug: "user:foo", Name: "Foo", Source: "user", Body: "v1", Enabled: true}
	skV2 := skV1
	skV2.Body = "v2 — updated body"

	if err := s.PushSkillToInstance(inst.ID, skV1); err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(inst.ID, skV2); err != nil {
		t.Fatal(err)
	}

	dir := s.agents.instanceDir(inst.ID)
	active, err := journalActiveSkillRecords(dir + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 active skill after upsert, got %d (%+v)", len(active), active)
	}
	rec := active["user:foo"]
	if extractTag(rec.Tags, SkillHashTagPrefix) != skillBodyHash("v2 — updated body") {
		t.Errorf("active record hash should match v2, tags=%v", rec.Tags)
	}
}

func TestUpdateSkillRefreshesAssignedInstanceMemories(t *testing.T) {
	s := newTestServer(t)
	res, err := s.store.db.Exec(`
		INSERT INTO skills (slug, name, description, body, source, project_id, metadata_json)
		VALUES ('user:briefing', 'briefing', 'old description', 'old body', 'user', 'proj-x', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	skillID, _ := res.LastInsertId()
	assigned, _ := s.store.CreateAgent(1, "assigned", "d", "autonomous", "{}", "proj-x")
	unassigned, _ := s.store.CreateAgent(1, "unassigned", "d", "autonomous", "{}", "proj-x")
	otherProject, _ := s.store.CreateAgent(1, "other", "d", "autonomous", "{}", "proj-y")

	oldSkill, err := s.loadSkillByID(skillID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(assigned.ID, oldSkill); err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(otherProject.ID, oldSkill); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{
		"description": "new description",
		"body":        "new body",
	})
	req := httptest.NewRequest(http.MethodPut, "/skills/"+strconv.FormatInt(skillID, 10), bytes.NewReader(body))
	req.Header.Set("X-User-ID", "1")
	w := httptest.NewRecorder()
	s.handleUpdateSkill(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["refreshed"] != 1 {
		t.Fatalf("refreshed=%d, want 1", resp["refreshed"])
	}

	assignedActive, err := journalActiveSkillRecords(s.agents.instanceDir(assigned.ID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rec := assignedActive["user:briefing"]
	if extractTag(rec.Tags, SkillHashTagPrefix) != skillBodyHash("new body") {
		t.Fatalf("assigned skill hash = %q, want %q; tags=%v", extractTag(rec.Tags, SkillHashTagPrefix), skillBodyHash("new body"), rec.Tags)
	}
	if !strings.Contains(rec.Content, "new description") || !strings.Contains(rec.Content, "new body") {
		t.Fatalf("assigned skill content was not refreshed: %q", rec.Content)
	}
	unassignedActive, err := journalActiveSkillRecords(s.agents.instanceDir(unassigned.ID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unassignedActive["user:briefing"]; ok {
		t.Fatalf("unassigned agent should not receive skill refresh")
	}
	otherActive, err := journalActiveSkillRecords(s.agents.instanceDir(otherProject.ID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if extractTag(otherActive["user:briefing"].Tags, SkillHashTagPrefix) != skillBodyHash("old body") {
		t.Fatalf("other project should remain on old body")
	}
}

func TestRegisterAppSkillsRefreshesAssignedMemoriesBySlug(t *testing.T) {
	s := newTestServer(t)
	appRes, err := s.store.db.Exec(`INSERT INTO apps (name, source, manifest_json) VALUES ('media', 'git', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := appRes.LastInsertId()
	installRes, err := s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'proj-x', 'running', 1)`, appID)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := installRes.LastInsertId()
	skillRes, err := s.store.db.Exec(`
		INSERT INTO skills (slug, name, description, body, source, install_id, project_id, metadata_json)
		VALUES ('media:clip', 'clip', 'old description', 'old body', 'app', ?, 'proj-x', '{}')`, installID)
	if err != nil {
		t.Fatal(err)
	}
	oldSkillID, _ := skillRes.LastInsertId()
	assigned, _ := s.store.CreateAgent(1, "assigned", "d", "autonomous", "{}", "proj-x")
	unassigned, _ := s.store.CreateAgent(1, "unassigned", "d", "autonomous", "{}", "proj-x")

	oldSkill, err := s.loadSkillByID(oldSkillID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(assigned.ID, oldSkill); err != nil {
		t.Fatal(err)
	}

	err = s.registerAppSkills(installID, "media", "proj-x", []sdk.Skill{
		{Name: "clip", Description: "new description", Body: "new body"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	assignedActive, err := journalActiveSkillRecords(s.agents.instanceDir(assigned.ID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rec := assignedActive["media:clip"]
	if extractTag(rec.Tags, SkillHashTagPrefix) != skillBodyHash("new body") {
		t.Fatalf("assigned app skill hash = %q, want %q; tags=%v", extractTag(rec.Tags, SkillHashTagPrefix), skillBodyHash("new body"), rec.Tags)
	}
	if extractTag(rec.Tags, SkillIDTagPrefix) == strconv.FormatInt(oldSkillID, 10) {
		t.Fatalf("expected refreshed memory to carry new skill row id, tags=%v", rec.Tags)
	}
	unassignedActive, err := journalActiveSkillRecords(s.agents.instanceDir(unassigned.ID) + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unassignedActive["media:clip"]; ok {
		t.Fatalf("unassigned agent should not receive app skill refresh")
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM skills WHERE install_id = ? AND slug = 'media:clip'`, installID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("skill row count = %d, want 1", count)
	}
}

func TestSweepSkillFromProject_DropsAcrossAllInstances(t *testing.T) {
	s := newTestServer(t)
	// Two instances in the same project + one in a different project.
	a, _ := s.store.CreateAgent(1, "a", "d", "autonomous", "{}", "proj-x")
	b, _ := s.store.CreateAgent(1, "b", "d", "autonomous", "{}", "proj-x")
	c, _ := s.store.CreateAgent(1, "c", "d", "autonomous", "{}", "proj-y")

	sk := Skill{ID: 50, Slug: "user:shared", Name: "Shared", Source: "user", Body: "x", Enabled: true}
	if err := s.PushSkillToInstance(a.ID, sk); err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(b.ID, sk); err != nil {
		t.Fatal(err)
	}
	if err := s.PushSkillToInstance(c.ID, sk); err != nil {
		t.Fatal(err)
	}

	s.sweepSkillFromProject(1, "proj-x", sk.ID, sk.Slug, "test sweep")

	for _, inst := range []*Agent{a, b} {
		path := s.agents.instanceDir(inst.ID) + "/memory.jsonl"
		got, err := journalActiveSkillRecords(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["user:shared"]; present {
			t.Errorf("instance %d should have been swept", inst.ID)
		}
	}
	// proj-y is untouched.
	cPath := s.agents.instanceDir(c.ID) + "/memory.jsonl"
	got, err := journalActiveSkillRecords(cPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["user:shared"]; !present {
		t.Errorf("instance in proj-y should NOT have been swept, got %+v", got)
	}
}

func TestRemoveSkillFromInstance_StoppedTombstones(t *testing.T) {
	s := newTestServer(t)
	inst, err := s.store.CreateAgent(1, "test", "directive", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	sk := Skill{ID: 11, Slug: "user:gone", Name: "Gone", Source: "user", Body: "x", Enabled: true}
	if err := s.PushSkillToInstance(inst.ID, sk); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSkillFromInstance(inst.ID, sk.ID, "test"); err != nil {
		t.Fatalf("RemoveSkillFromInstance: %v", err)
	}
	dir := s.agents.instanceDir(inst.ID)
	active, err := journalActiveSkillRecords(dir + "/memory.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := active["user:gone"]; present {
		// Sort keys for stable test output.
		keys := make([]string, 0, len(active))
		for k := range active {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("skill should be gone from active set, got %v", keys)
	}
}
