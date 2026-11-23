package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// helperCoreExecutionMode reads the execution mode from the Helper's Core
// config file, which is what Core loads at start.
func helperCoreExecutionMode(t *testing.T, s *Server, helperID int64) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.agents.instanceDir(helperID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Execution struct {
			Mode string `json:"mode"`
		} `json:"execution_control"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Execution.Mode
}

// storedHelperExecutionMode reads the execution mode saved on the Helper row.
func storedHelperExecutionMode(t *testing.T, s *Server, userID int64) string {
	t.Helper()
	helper, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	execControl, _ := helperConfigMap(helper)["execution_control"].(map[string]any)
	mode, _ := execControl["mode"].(string)
	return mode
}

// pauseHelperCoreConfig writes the execution control Core saves after a pause.
func pauseHelperCoreConfig(t *testing.T, s *Server, helperID int64) {
	t.Helper()
	if err := s.writeStoppedConfigAtomic(helperID, func(cfg map[string]any) error {
		cfg["execution_control"] = map[string]any{"mode": "paused", "scope": "instance"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func helperProcess(s *Server, helperID int64) *runningAgent {
	s.agents.mu.RLock()
	defer s.agents.mu.RUnlock()
	return s.agents.processes[helperID]
}

func TestHelperStartsCoreInAutoModeEvenAfterASavedPause(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	s.agents.coreCmd, _ = fakeHelperCore(t)
	stored, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	pauseHelperCoreConfig(t, s, stored.ID)

	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		t.Fatalf("start Helper: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(helper.ID) })

	if mode := helperCoreExecutionMode(t, s, helper.ID); mode != "auto" {
		t.Fatalf("Helper Core started with execution mode %q, want auto: nothing releases a paused Helper", mode)
	}
	if mode := storedHelperExecutionMode(t, s, userID); mode != "auto" {
		t.Fatalf("Helper row saved execution mode %q, want auto", mode)
	}
}

func TestRunningHelperSavedPausedRestartsOnceIntoAutoMode(t *testing.T) {
	s, userID, _ := activatedStoppedHelper(t)
	s.agents.coreCmd, _ = fakeHelperCore(t)
	helper, err := s.ensureMetaAgentRunning(userID)
	if err != nil {
		t.Fatalf("start Helper: %v", err)
	}
	t.Cleanup(func() { s.agents.Stop(helper.ID) })

	// A Helper started by an older server: its row asks for an iteration.start
	// pause and its Core saved a paused execution control.
	stored, err := s.store.GetPlatformHelper(userID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := helperConfigMap(stored)
	cfg["execution_control"] = map[string]any{"mode": "paused", "breakpoints": []string{"iteration.start"}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stored.Config = string(raw)
	if err := s.store.UpdateAgent(stored); err != nil {
		t.Fatal(err)
	}
	pauseHelperCoreConfig(t, s, helper.ID)
	before := helperProcess(s, helper.ID)

	if _, err := s.ensureMetaAgentRunning(userID); err != nil {
		t.Fatalf("open Helper saved paused: %v", err)
	}
	restarted := helperProcess(s, helper.ID)
	if before == nil || restarted == nil || restarted == before {
		t.Fatal("a running Helper saved paused was not restarted")
	}
	if mode := helperCoreExecutionMode(t, s, helper.ID); mode != "auto" {
		t.Fatalf("restarted Helper Core has execution mode %q, want auto", mode)
	}
	if mode := storedHelperExecutionMode(t, s, userID); mode != "auto" {
		t.Fatalf("Helper row still saves execution mode %q after the restart, want auto", mode)
	}

	if _, err := s.ensureMetaAgentRunning(userID); err != nil {
		t.Fatalf("open Helper again: %v", err)
	}
	if again := helperProcess(s, helper.ID); again != restarted {
		t.Fatal("a Helper already in auto mode was restarted a second time")
	}
}
