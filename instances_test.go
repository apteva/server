package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

// helper: register + login (creates user, session cookie set as side effect)
func registerAndLogin(t *testing.T, s *Server) {
	t.Helper()
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	postJSON(t, s.handleLogin, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
}

func authedRequest(t *testing.T, method, path, token string, body any) *http.Request {
	t.Helper()
	var req *http.Request
	if body != nil {
		data, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(data))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Set user ID (normally done by middleware)
	req.Header.Set("X-User-ID", "1")
	return req
}

func TestListInstances_Empty(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)

	req := authedRequest(t, "GET", "/instances", "", nil)
	w := httptest.NewRecorder()
	s.handleListInstances(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var instances []any
	json.Unmarshal(w.Body.Bytes(), &instances)
	if len(instances) != 0 {
		t.Errorf("expected 0, got %d", len(instances))
	}
}

func TestCreateInstance_NoStart(t *testing.T) {
	// Test that instance is created in DB even if core binary doesn't exist
	s := newTestServer(t)
	registerAndLogin(t, s)

	// Create instance — core won't start (binary is "echo") but DB entry should exist
	req := authedRequest(t, "POST", "/instances", "", map[string]string{
		"name": "test-agent", "directive": "do stuff",
	})
	w := httptest.NewRecorder()
	s.handleCreateInstance(w, req)

	// May fail to start core, but instance should be in DB
	instances, _ := s.store.ListInstances(1, "")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance in DB, got %d", len(instances))
	}
	if instances[0].Name != "test-agent" {
		t.Errorf("expected test-agent, got %s", instances[0].Name)
	}
}

func TestGetInstance(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateInstance(1, "agent", "directive", "{}", "")

	req := authedRequest(t, "GET", "/instances/1", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeJSON(t, w)
	if body["name"] != "agent" {
		t.Errorf("expected agent, got %v", body["name"])
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)

	req := authedRequest(t, "GET", "/instances/999", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteInstance(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateInstance(1, "agent", "dir", "{}", "")

	req := authedRequest(t, "DELETE", "/instances/1", "", nil)
	w := httptest.NewRecorder()
	s.handleInstance(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	instances, _ := s.store.ListInstances(1, "")
	if len(instances) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(instances))
	}
}

func TestUpdateConfig(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	s.store.CreateInstance(1, "agent", "old directive", "{}", "")

	req := authedRequest(t, "PUT", "/instances/1/config", "", map[string]string{
		"directive": "new directive",
	})
	w := httptest.NewRecorder()
	s.handleUpdateConfig(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	inst, _ := s.store.GetInstance(1, 1)
	if inst.Directive != "new directive" {
		t.Errorf("expected new directive, got %s", inst.Directive)
	}
}

func TestInstanceManager_PortTracking(t *testing.T) {
	im := NewInstanceManager(t.TempDir(), "sleep", 5000)

	// Not running initially
	if im.IsRunning(1) {
		t.Error("should not be running")
	}
	if im.GetPort(1) != 0 {
		t.Error("port should be 0 when not running")
	}

	// Simulate a running instance by directly inserting into the map
	cmd := exec.Command("sleep", "60")
	cmd.Start()
	defer cmd.Process.Kill()

	im.mu.Lock()
	im.processes[1] = &runningInstance{cmd: cmd, port: 5001}
	im.mu.Unlock()

	if !im.IsRunning(1) {
		t.Error("should be running")
	}
	if im.GetPort(1) != 5001 {
		t.Errorf("expected port 5001, got %d", im.GetPort(1))
	}

	// Stop should clear the process
	im.Stop(1)
	if im.IsRunning(1) {
		t.Error("should not be running after stop")
	}
	if im.GetPort(1) != 0 {
		t.Error("port should be 0 after stop")
	}
}

func TestInstanceManager_StopNotRunning(t *testing.T) {
	im := NewInstanceManager(t.TempDir(), "sleep", 5000)
	// Should not panic
	im.Stop(999)
}

func TestInstanceIsolation(t *testing.T) {
	s := newTestServer(t)

	// Create two users
	postJSON(t, s.handleRegister, map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	postJSON(t, s.handleRegister, map[string]string{
		"email": "bob@test.com", "password": "password123",
	})

	// Alice creates an instance
	s.store.CreateInstance(1, "alice-agent", "alice stuff", "{}", "")

	// Bob creates an instance
	s.store.CreateInstance(2, "bob-agent", "bob stuff", "{}", "")

	// Alice should see only her instance
	aliceInstances, _ := s.store.ListInstances(1, "")
	if len(aliceInstances) != 1 || aliceInstances[0].Name != "alice-agent" {
		t.Errorf("alice should see only alice-agent, got %v", aliceInstances)
	}

	// Bob should see only his
	bobInstances, _ := s.store.ListInstances(2, "")
	if len(bobInstances) != 1 || bobInstances[0].Name != "bob-agent" {
		t.Errorf("bob should see only bob-agent, got %v", bobInstances)
	}

	// Alice can't access Bob's instance
	_, err := s.store.GetInstance(1, 2)
	if err == nil {
		t.Error("alice should not see bob's instance")
	}
}
