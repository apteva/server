package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentProactivityDefaultsAndCreation(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	for _, level := range []int{25, 0, 50, 100} {
		payload := map[string]any{"name": fmt.Sprintf("initiative-%d", level), "mode": "cautious", "start": false, "bound_app_install_ids": []int64{}}
		if level != 25 {
			payload["proactivity"] = level
		}
		w := httptest.NewRecorder()
		s.handleCreateInstance(w, authedRequest(t, "POST", "/instances", "", payload))
		if w.Code != 200 && w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		a, err := s.store.GetAgentByID(result.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.Proactivity != level || a.Mode != "cautious" || !strings.Contains(a.Directive, fmt.Sprintf("PROACTIVITY: %d/100", level)) {
			t.Fatalf("unexpected policy: %+v", a)
		}
	}
	a, created, err := s.store.CreateAgentIdempotent(1, "replay", "role", "autonomous", "{}", "", "same", 0)
	if err != nil || !created {
		t.Fatal(err)
	}
	replay, created, err := s.store.CreateAgentIdempotent(1, "replay", "changed", "cautious", "{}", "", "same", 100)
	if err != nil || created || replay.ID != a.ID || replay.Proactivity != 0 {
		t.Fatal(replay, created, err)
	}
	rows, err := s.store.ListAgents(1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		a, err := s.store.GetAgentByID(row.ID)
		if err != nil || row.Proactivity != a.Proactivity {
			t.Fatal(row, err)
		}
	}
}

func TestAgentProactivityUpdatesAndValidation(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "learn")
	for _, value := range []any{-1, 101, 2.5, "25", nil, true} {
		w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": value, "directive": "must not persist"})
		if w.Code != 400 {
			t.Fatal(value, w.Code, w.Body.String())
		}
		saved, _ := s.store.GetAgentByID(a.ID)
		if saved.Proactivity != 25 || strings.Contains(saved.Directive, "must not persist") {
			t.Fatal("invalid request mutated policy")
		}
	}
	for _, level := range []int{0, 100, 25} {
		w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": level})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		a, _ = s.store.GetAgentByID(a.ID)
		if a.Proactivity != level || a.Mode != "learn" {
			t.Fatal(a)
		}
		cfg := readBehaviorDisk(t, s, a.ID)
		if cfg["directive"] != a.Directive || cfg["proactivity"] != nil {
			t.Fatal(cfg)
		}
	}
	// A mode-only update must retain an explicit zero.
	if w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": 0}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := behaviorRequest(t, s, a.ID, map[string]any{"mode": "cautious"}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a, _ = s.store.GetAgentByID(a.ID)
	if a.Proactivity != 0 {
		t.Fatal("mode update reset zero")
	}
	reopened, err := NewStore(s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.GetAgentByID(a.ID)
	if err != nil || restored.Proactivity != 0 {
		t.Fatal(restored, err)
	}
}

func TestAgentProactivityLiveWorkersAndPendingRetry(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	c := attachBehaviorTestCore(t, s, a)
	c.rejectVoice = true
	w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": 100})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if response["proactivity"] != float64(100) || response["mode"] != "cautious" {
		t.Fatal(response)
	}
	c.mu.Lock()
	if c.cfg["proactivity"] != nil || !strings.Contains(c.workers[1]["directive"].(string), "PROACTIVITY: 100/100") || c.workers[1]["history"] != "keep" || c.workers[3]["directive"] != "internal memory rules" {
		t.Fatal("worker policy/scope mismatch")
	}
	c.cfg["directive"] = c.cfg["directive"].(string) + "\nKeep evolved strategy"
	c.rejectVoice = false
	c.mu.Unlock()
	a, _ = s.store.GetAgentByID(a.ID)
	if err := s.reconcileAgentBehavior(context.Background(), a, s.agents.GetPort(a.ID)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Directive, "Keep evolved strategy") || a.Proactivity != 100 {
		t.Fatal(a)
	}
	state, _ := s.store.agentBehaviorState(a.ID)
	if state.Pending {
		t.Fatal(state)
	}
	c.mu.Lock()
	c.rejectMain = true
	c.mu.Unlock()
	if w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": 0}); w.Code != 503 {
		t.Fatal(w.Code)
	}
	a, _ = s.store.GetAgentByID(a.ID)
	s.agents.mu.Lock()
	delete(s.agents.processes, a.ID)
	s.agents.mu.Unlock()
	if err := s.reconcileAgentBehavior(context.Background(), a, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readBehaviorDisk(t, s, a.ID)["directive"].(string), "PROACTIVITY: 0/100") {
		t.Fatal("lost pending zero")
	}
}

func TestAgentProactivityLegacyMigration(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "learn")
	// Recreate the pre-proactivity schema while retaining telemetry migrations.
	for _, q := range []string{
		`DROP TRIGGER agent_behavior_update`,
		`ALTER TABLE agents DROP COLUMN proactivity`,
		`DELETE FROM server_schema_migrations WHERE version=4`,
		`UPDATE agents SET directive='legacy role'`,
		`UPDATE agent_behavior_state SET revision=0,main_revision=0,applied_revision=0,version=1`,
	} {
		if _, err := s.store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error { cfg["directive"] = "newer disk role"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.store.migrateReviewFixes(); err != nil {
		t.Fatal(err)
	}
	a, _ = s.store.GetAgentByID(a.ID)
	if a.Proactivity != 25 {
		t.Fatal(a.Proactivity)
	}
	if err := s.reconcileAgentBehavior(context.Background(), a, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Directive, "newer disk role") || !strings.Contains(a.Directive, "25/100 (Conservative)") || a.Mode != "learn" {
		t.Fatal(a)
	}
	if err := s.store.migrateReviewFixes(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentProactivitySharperPolicyUpgrade(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	if w := behaviorRequest(t, s, a.ID, map[string]any{"proactivity": 75}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// A deployed v2 policy was fully applied, and core has evolved its mission.
	legacy := "# Role\nKeep the new core-authored method.\n\n<!-- apteva:behavior:v1:start -->\nLegacy generic initiative instructions.\n<!-- apteva:behavior:end -->"
	if err := s.writeStoppedConfigAtomic(a.ID, func(cfg map[string]any) error { cfg["directive"] = legacy; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE agent_behavior_state SET version=2,main_revision=revision,applied_revision=revision WHERE agent_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	state, err := s.store.agentBehaviorState(a.ID)
	if err != nil || !state.Pending {
		t.Fatal("old policy must require reconciliation", state, err)
	}
	a, err = s.store.GetAgentByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileAgentBehavior(context.Background(), a, 0); err != nil {
		t.Fatal(err)
	}
	state, err = s.store.agentBehaviorState(a.ID)
	if err != nil || state.Pending || state.Version != behaviorVersion {
		t.Fatal(state, err)
	}
	if a.Proactivity != 75 || a.Mode != "cautious" || !strings.Contains(a.Directive, "Keep the new core-authored method.") || strings.Contains(a.Directive, "Legacy generic") {
		t.Fatal("policy upgrade changed agent intent or settings")
	}
	if strings.Count(a.Directive, behaviorStart) != 1 {
		t.Fatal("policy upgrade duplicated its managed section")
	}
}
