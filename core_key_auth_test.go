package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// coreKeyAccess is what the two routes that accept a core_ key made of one.
type coreKeyAccess struct {
	// llm is the /api/llm status. No managed connection is configured in
	// these tests, so 503 means the key was accepted and 401 that it was not.
	llm int
	// user is the X-User-ID the runtime-token route resolved, or "" when the
	// key was refused.
	user string
}

func checkCoreKey(t *testing.T, s *Server, key string) coreKeyAccess {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/llm/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.handleManagedLLMChat(rec, req)

	var user string
	tokenReq := httptest.NewRequest(http.MethodPost, "/providers/7/auth/runtime-token", nil)
	tokenReq.RemoteAddr = "127.0.0.1:43123"
	tokenReq.Header.Set("Authorization", "Bearer "+key)
	tokenRec := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		user = r.Header.Get("X-User-ID")
		w.WriteHeader(http.StatusNoContent)
	})(tokenRec, tokenReq)
	if user == "" && tokenRec.Code != http.StatusUnauthorized {
		t.Fatalf("runtime-token route refused the key with status %d, want 401", tokenRec.Code)
	}
	return coreKeyAccess{llm: rec.Code, user: user}
}

func coreKeyTestAgent(t *testing.T, s *Server, email string) *Agent {
	t.Helper()
	user, err := s.store.CreateUser(email, "hash")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(user.ID, "agent", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func recordCoreRunning(t *testing.T, s *Server, agentID int64, key string) {
	t.Helper()
	if _, err := s.store.db.Exec(`UPDATE agents SET status='running', core_api_key=?, pid=42, port=4100 WHERE id=?`, key, agentID); err != nil {
		t.Fatal(err)
	}
}

func wantCoreKeyAccepted(t *testing.T, got coreKeyAccess, agent *Agent) {
	t.Helper()
	if got.llm != http.StatusServiceUnavailable || got.user != itoa(agent.UserID) {
		t.Fatalf("key refused: /api/llm status=%d runtime-token user=%q, want 503 and %d", got.llm, got.user, agent.UserID)
	}
}

func wantCoreKeyRefused(t *testing.T, got coreKeyAccess) {
	t.Helper()
	if got.llm != http.StatusUnauthorized || got.user != "" {
		t.Fatalf("key accepted: /api/llm status=%d runtime-token user=%q, want 401 and none", got.llm, got.user)
	}
}

// A core calls back with its key as soon as it boots, before syncAgentRuntime
// records the key and status on the agent row.
func TestCoreKeyOfStartingCoreIsAccepted(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "starting-core@test.local")
	const key = "core_starting"
	s.agents.processes[agent.ID] = &runningAgent{cmd: &exec.Cmd{}, port: 4100, coreAPIKey: key}

	stored, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == "running" || stored.CoreAPIKey != "" {
		t.Fatalf("fixture already records the runtime: status=%q key=%q", stored.Status, stored.CoreAPIKey)
	}
	wantCoreKeyAccepted(t, checkCoreKey(t, s, key), agent)
}

func TestCoreKeyUnknownIsRefused(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "unknown-key@test.local")
	s.agents.processes[agent.ID] = &runningAgent{cmd: &exec.Cmd{}, port: 4100, coreAPIKey: "core_live"}
	recordCoreRunning(t, s, agent.ID, "core_live")

	wantCoreKeyRefused(t, checkCoreKey(t, s, "core_unknown"))
}

func TestCoreKeyOfStoppedAgentIsRefused(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "stopped-core@test.local")
	const key = "core_stopped"
	s.agents.processes[agent.ID] = &runningAgent{cmd: &exec.Cmd{}, port: 4100, coreAPIKey: key}
	recordCoreRunning(t, s, agent.ID, key)
	wantCoreKeyAccepted(t, checkCoreKey(t, s, key), agent)

	s.agents.Stop(agent.ID)
	if err := s.store.SetAgentRuntimeStopped(agent); err != nil {
		t.Fatal(err)
	}
	wantCoreKeyRefused(t, checkCoreKey(t, s, key))
}

// A core that has exited but is not yet removed from the process table no
// longer holds a usable key.
func TestCoreKeyOfExitedCoreIsRefused(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "exited-core@test.local")
	const key = "core_exited"
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ri := &runningAgent{cmd: cmd, port: 4100, coreAPIKey: key, done: make(chan struct{})}
	_ = ri.wait()
	s.agents.processes[agent.ID] = ri

	wantCoreKeyRefused(t, checkCoreKey(t, s, key))
}

func TestCoreKeyOfReattachedCoreIsAccepted(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "reattached-core@test.local")
	const key = "core_reattached"
	s.agents.processes[agent.ID] = &runningAgent{port: 4100, coreAPIKey: key, reattached: true}
	recordCoreRunning(t, s, agent.ID, key)

	wantCoreKeyAccepted(t, checkCoreKey(t, s, key), agent)
}

// A core that outlived the previous server calls the new one before
// ResumeRunningInstances re-attaches it; the stored row lets it in.
func TestCoreKeyRecordedButNotYetReattachedIsAccepted(t *testing.T) {
	s := newTestServer(t)
	agent := coreKeyTestAgent(t, s, "surviving-core@test.local")
	const key = "core_surviving"
	recordCoreRunning(t, s, agent.ID, key)

	wantCoreKeyAccepted(t, checkCoreKey(t, s, key), agent)
}
