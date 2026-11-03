package main

import (
	"strings"
	"testing"
)

// failedRuntimeCandidate publishes one deployment and leaves a pending
// candidate whose prepare operation failed, either before Prepare reached the
// backend (target validation) or after (the prepare call itself). The backend
// then answers 404 for IDs it never recorded, as the Instances backend does.
func failedRuntimeCandidate(t *testing.T, prepareSent bool) (*Server, *appRuntimeManager, *runtimeBackendFixture, int64, runtimeDeployment, runtimeDeployment) {
	t.Helper()
	s, m, f, id, p := runtimeTestSetup(t)
	prepareRuntimeFixture(t, m, id, p, "prepare-active")
	active := activateRuntimeFixture(t, m, id, "activate-active")
	f.mu.Lock()
	f.unknownNotFound, f.failPrepare, f.failValidateTarget = true, prepareSent, !prepareSent
	f.mu.Unlock()
	op, err := m.begin(id, "prepare", "prepare-candidate", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitRuntimeOperation(t, m, op); result.Status != "failed" {
		t.Fatalf("candidate prepare %s", result.Status)
	}
	f.mu.Lock()
	f.failPrepare, f.failValidateTarget = false, false
	f.mu.Unlock()
	var pending string
	if err = s.store.db.QueryRow(`SELECT runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	candidate, err := m.loadDeployment(pending)
	if err != nil {
		t.Fatal("failed candidate is not pending: ", err)
	}
	if candidate.Unprepared == prepareSent {
		t.Fatalf("unprepared=%v after prepare sent=%v", candidate.Unprepared, prepareSent)
	}
	return s, m, f, id, active, candidate
}

func runtimeInstallPointers(t *testing.T, s *Server, id int64) (string, string) {
	t.Helper()
	var active, pending string
	if err := s.store.db.QueryRow(`SELECT runtime_active_deployment,runtime_pending_deployment FROM app_installs WHERE id=?`, id).Scan(&active, &pending); err != nil {
		t.Fatal(err)
	}
	return active, pending
}

func runtimeBackendEvent(f *runtimeBackendFixture, event string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e == event {
			return true
		}
	}
	return false
}

func TestManagedRuntimeDiscardsCandidateNeverSentToBackend(t *testing.T) {
	s, m, f, id, active, candidate := failedRuntimeCandidate(t, false)
	op, err := m.begin(id, "discard", "discard-unprepared", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitRuntimeOperation(t, m, op); result.Status != "succeeded" {
		t.Fatalf("discard %s: %s", result.Status, result.Error)
	}
	if runtimeBackendEvent(f, "delete:"+candidate.ID) {
		t.Fatal("backend was asked to delete a deployment it never prepared")
	}
	if record, _ := m.loadDeployment(candidate.ID); record.Status != "deleted" {
		t.Fatalf("candidate status %q", record.Status)
	}
	if published, pending := runtimeInstallPointers(t, s, id); published != active.ID || pending != "" {
		t.Fatalf("pointers active=%q pending=%q", published, pending)
	}
	next := prepareRuntimeFixture(t, m, id, active.Placement, "prepare-after-discard")
	if next.Unprepared || next.Generation <= candidate.Generation {
		t.Fatalf("next candidate unprepared=%v generation=%d", next.Unprepared, next.Generation)
	}
}

func TestManagedRuntimeKeepsDeleteForCandidateSentToBackend(t *testing.T) {
	s, m, f, id, active, candidate := failedRuntimeCandidate(t, true)
	op, err := m.begin(id, "discard", "discard-sent", runtimePlacement{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := waitRuntimeOperation(t, m, op)
	// A lost prepare response may still create the deployment, so "not found"
	// is not proof that it is gone.
	if result.Status != "failed" || !strings.Contains(result.Error, "delete returned HTTP 404") {
		t.Fatalf("discard %s: %s", result.Status, result.Error)
	}
	if !runtimeBackendEvent(f, "delete:"+candidate.ID) {
		t.Fatal("backend delete was skipped after prepare was sent")
	}
	if published, pending := runtimeInstallPointers(t, s, id); published != active.ID || pending != candidate.ID {
		t.Fatalf("pointers active=%q pending=%q", published, pending)
	}
}

func TestManagedRuntimeDeleteSettlesCandidateBeforePublished(t *testing.T) {
	t.Run("never prepared", func(t *testing.T) {
		s, m, f, id, active, candidate := failedRuntimeCandidate(t, false)
		if err := m.stopSync(id, true); err != nil {
			t.Fatal(err)
		}
		if runtimeBackendEvent(f, "delete:"+candidate.ID) || !runtimeBackendEvent(f, "delete:"+active.ID) {
			t.Fatal("delete did not skip only the unprepared candidate")
		}
		for _, ref := range []string{active.ID, candidate.ID} {
			if record, _ := m.loadDeployment(ref); record.Status != "deleted" {
				t.Fatalf("%s status %q", ref, record.Status)
			}
		}
		if published, pending := runtimeInstallPointers(t, s, id); published != "" || pending != "" {
			t.Fatalf("pointers active=%q pending=%q", published, pending)
		}
	})
	t.Run("backend refuses candidate", func(t *testing.T) {
		s, m, _, id, active, candidate := failedRuntimeCandidate(t, true)
		if err := m.stopSync(id, true); err == nil || !strings.Contains(err.Error(), "delete returned HTTP 404") {
			t.Fatalf("delete error %v", err)
		}
		if record, _ := m.loadDeployment(active.ID); record.Status == "deleted" {
			t.Fatal("published deployment deleted before the candidate")
		}
		if published, pending := runtimeInstallPointers(t, s, id); published != active.ID || pending != candidate.ID {
			t.Fatalf("pointers active=%q pending=%q", published, pending)
		}
	})
}

func TestRuntimeBackendErrorKeepsTextOutOfOperations(t *testing.T) {
	for raw, want := range map[string]string{
		"deployment not found; ownership cannot be established\n": "deployment not found; ownership cannot be established",
		`{"code":"invalid_target","error":"no such\tmachine"}`:    "invalid_target no such machine",
		"\x00\x1b[31m":                  "[31m",
		"":                              "(no reason given)",
		strings.Repeat("é", 400) + "\n": strings.Repeat("é", 150) + "…",
	} {
		if got := runtimeBackendReason([]byte(raw)); got != want {
			t.Errorf("reason(%q)=%q want %q", raw, got, want)
		}
	}
	if got := (&runtimeBackendHTTPError{Action: "prepare", Status: 409, Code: "capacity_insufficient"}).Error(); got != "backend prepare returned HTTP 409 (capacity_insufficient)" {
		t.Fatal(got)
	}
	if got := (&runtimeBackendHTTPError{Action: "prepare", Status: 409, Code: "Not a code!"}).Error(); got != "backend prepare returned HTTP 409" {
		t.Fatal(got)
	}
}
