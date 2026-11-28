package main

import (
	"errors"
	"testing"
)

func agentStatus(t *testing.T, s *Server, id int64) string {
	t.Helper()
	inst, err := s.store.GetAgentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return inst.Status
}

// Disabling an account stopped its agents' processes but left them marked
// running, so the next server start brought them back.
func TestSecurityDisablingAccountKeepsItsAgentsStopped(t *testing.T) {
	h := newSecurityHarness(t)
	mine, err := h.s.store.CreateAgent(h.userID, "user-agent", "directive", "autonomous", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := h.s.store.CreateAgent(h.adminID, "admin-agent", "directive", "autonomous", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.store.db.Exec(`UPDATE agents SET status='running' WHERE id IN (?, ?)`, mine.ID, theirs.ID); err != nil {
		t.Fatal(err)
	}

	admin := h.login(t, "admin@example.test", "198.51.100.2:1")
	if w := h.do(t, harnessReq{method: "POST", path: "/security/users/" + itoa(h.userID) + "/disable", cookie: admin, body: map[string]string{"reason": "left the company"}}); w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if got := agentStatus(t, h.s, mine.ID); got != "stopped" {
		t.Fatalf("disabled user's agent status = %q, want stopped", got)
	}
	if got := agentStatus(t, h.s, theirs.ID); got != "running" {
		t.Fatalf("another user's agent status = %q, want running", got)
	}
	running, err := h.s.store.ListAgentsByStatus("running")
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range running {
		if inst.ID == mine.ID {
			t.Fatal("disabled user's agent would be resumed at the next server start")
		}
	}

	// Nobody can start it while the owner is disabled.
	inst, _ := h.s.store.GetAgentByID(mine.ID)
	if _, err := h.s.startManagedAgent(inst, nil, nil); !errors.Is(err, errAgentOwnerDisabled) {
		t.Fatalf("start for a disabled owner: err=%v, want errAgentOwnerDisabled", err)
	}
	if _, err := h.s.reattachManagedAgent(inst); !errors.Is(err, errAgentOwnerDisabled) {
		t.Fatalf("reattach for a disabled owner: err=%v, want errAgentOwnerDisabled", err)
	}

	// Re-enabling lifts the refusal; the agent stays stopped until started.
	if w := h.do(t, harnessReq{method: "POST", path: "/security/users/" + itoa(h.userID) + "/enable", cookie: admin}); w.Code != 200 {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if h.s.store.UserDisabled(h.userID) {
		t.Fatal("account still disabled after enable")
	}
	if got := agentStatus(t, h.s, mine.ID); got != "stopped" {
		t.Fatalf("agent status after enable = %q, want stopped", got)
	}
}
