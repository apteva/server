package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSyncAgentRuntimeReplacesStaleSameVersionStart(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("runtime@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "runtime", "wait", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}

	const coreKey = "core_runtime_test"
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+coreKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"core_version":    "0.25.8",
			"core_build_time": "2026-07-14T09:26:35Z",
			"uptime_seconds":  5,
		})
	}))
	defer core.Close()
	port := core.Listener.Addr().(*net.TCPAddr).Port

	staleStart := time.Now().Add(-2 * time.Hour).UTC()
	if err := s.store.UpdateAgentCoreRuntime(agent.ID, "0.25.8", "2026-07-14T09:26:35Z", staleStart); err != nil {
		t.Fatal(err)
	}
	agent.Pid = 4242
	agent.Port = port
	agent.CoreAPIKey = coreKey
	agent.Status = "running"
	agent.CoreVersion = "0.25.8"
	agent.CoreBuildTime = "2026-07-14T09:26:35Z"
	agent.CoreStartedAt = staleStart.Format(time.RFC3339Nano)

	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{
		pid: agent.Pid, port: port, coreAPIKey: coreKey, reattached: true,
	}
	s.agents.mu.Unlock()

	before := time.Now().UTC()
	if _, err := s.syncAgentRuntime(agent, time.Second, true); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()

	stored, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "running" || stored.Pid != 4242 || stored.Port != port || stored.CoreAPIKey != coreKey {
		t.Fatalf("incomplete process snapshot: %+v", stored)
	}
	if stored.CoreVersion != "0.25.8" || stored.CoreBuildTime != "2026-07-14T09:26:35Z" {
		t.Fatalf("incomplete core snapshot: %+v", stored)
	}
	startedAt, err := parseTime(stored.CoreStartedAt)
	if err != nil {
		t.Fatalf("parse core_started_at: %v", err)
	}
	if startedAt.Before(before.Add(-6*time.Second)) || startedAt.After(after) {
		t.Fatalf("core_started_at=%s, want current start derived from uptime", startedAt)
	}
}
