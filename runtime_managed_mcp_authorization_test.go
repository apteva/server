package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeMCPTestConfig(raw string) map[string]any {
	return map[string]any{"mcp_servers": []any{map[string]any{"name": "runtime-tools", "transport": "http", "url": raw}}}
}

func runtimeAuthorizationFixture(t *testing.T) (*Server, *Environment, *Agent, *EnvironmentAgent) {
	t.Helper()
	s := newRuntimeAPITestServer(t)
	runtime := &Environment{
		ID: "rt-auth", ProjectID: "proj-1",
		managedMCPs: map[string]*RuntimeManagedMCP{
			"runtime-tools": {Name: "runtime-tools", Token: "capability", Status: "running", Process: &MCPProcess{}},
		},
	}
	s.environments.environments[runtime.ID] = runtime
	agent := &Agent{ID: 41, UserID: 1, ProjectID: "proj-1", Kind: "environment_agent"}
	pending := &EnvironmentAgent{AgentID: agent.ID, Alias: "main"}
	if err := runtime.reserveAgent(pending); err != nil {
		t.Fatal(err)
	}
	return s, runtime, agent, pending
}

func TestRuntimeManagedMCPAuthorization(t *testing.T) {
	tests := []struct {
		name string
		url  string
		edit func(*Server, *Environment, *Agent)
		ok   bool
	}{
		{name: "pending agent", ok: true},
		{name: "localhost", url: "http://localhost:5280/mcp/runtime/rt-auth/capability", ok: true},
		{name: "wrong token", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/wrong"},
		{name: "missing runtime", url: "http://127.0.0.1:5280/mcp/runtime/missing/capability"},
		{name: "stopped runtime", edit: func(_ *Server, r *Environment, _ *Agent) { r.Stop() }},
		{name: "expired runtime", edit: func(_ *Server, r *Environment, _ *Agent) { r.expiresAt = time.Now().Add(-time.Second) }},
		{name: "stopped runner", edit: func(_ *Server, r *Environment, _ *Agent) { r.managedMCPs["runtime-tools"].Status = "stopped" }},
		{name: "missing process", edit: func(_ *Server, r *Environment, _ *Agent) { r.managedMCPs["runtime-tools"].Process = nil }},
		{name: "same project unrelated agent", edit: func(_ *Server, _ *Environment, a *Agent) { a.ID++ }},
		{name: "client kind alone", edit: func(_ *Server, r *Environment, a *Agent) { r.releaseAgentReservation(a.ID) }},
		{name: "ordinary agent", edit: func(_ *Server, _ *Environment, a *Agent) { a.Kind = "" }},
		{name: "another runtime agent", edit: func(s *Server, r *Environment, a *Agent) {
			r.releaseAgentReservation(a.ID)
			other := &Environment{ID: "rt-other", ProjectID: r.ProjectID}
			s.environments.environments[other.ID] = other
			if err := other.reserveAgent(&EnvironmentAgent{AgentID: a.ID, Alias: "main"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "foreign host", url: "http://example.invalid:5280/mcp/runtime/rt-auth/capability"},
		{name: "wrong port", url: "http://127.0.0.1:5281/mcp/runtime/rt-auth/capability"},
		{name: "wrong scheme", url: "https://127.0.0.1:5280/mcp/runtime/rt-auth/capability"},
		{name: "userinfo", url: "http://user@127.0.0.1:5280/mcp/runtime/rt-auth/capability"},
		{name: "fragment", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/capability#extra"},
		{name: "unsupported query", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/capability?environment_id=other"},
		{name: "missing token", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/"},
		{name: "extra path", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/capability/extra"},
		{name: "trailing slash", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth/capability/"},
		{name: "empty runtime", url: "http://127.0.0.1:5280/mcp/runtime//capability"},
		{name: "encoded slash", url: "http://127.0.0.1:5280/mcp/runtime/rt-auth%2Fcapability"},
		{name: "encoded alias", url: "http://127.0.0.1:5280/mcp/runtime/%72t-auth/capability"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, runtime, agent, _ := runtimeAuthorizationFixture(t)
			if tt.edit != nil {
				tt.edit(s, runtime, agent)
			}
			raw := tt.url
			if raw == "" {
				raw = s.runtimeManagedMCPURL(runtime.ID, "capability")
			}
			config := runtimeMCPTestConfig(raw)
			err := s.authorizeAgentMCPConfig(agent, config)
			if (err == nil) != tt.ok {
				t.Fatalf("authorized=%v want=%v error=%v", err == nil, tt.ok, err)
			}
			if tt.ok {
				// Startup, config edits and retries must tolerate the Server's
				// regenerated file-reference metadata without granting membership.
				if err := s.authorizeAgentMCPConfig(agent, config); err != nil {
					t.Fatalf("second authorization: %v", err)
				}
			}
		})
	}
}

func TestRuntimeManagedMCPReservationLifecycle(t *testing.T) {
	s, runtime, agent, pending := runtimeAuthorizationFixture(t)
	config := runtimeMCPTestConfig(s.runtimeManagedMCPURL(runtime.ID, "capability"))
	if runtime.GetAgent(agent.ID) != nil || len(runtime.Agents()) != 0 {
		t.Fatal("pending agent exposed as running")
	}
	if err := runtime.reserveAgent(&EnvironmentAgent{AgentID: 42, Alias: "main"}); err == nil {
		t.Fatal("concurrent alias reservation allowed")
	}
	if err := runtime.activateReservedAgent(pending); err != nil {
		t.Fatal(err)
	}
	if runtime.pendingAgents[agent.ID] != nil || runtime.GetAgent(agent.ID) != pending {
		t.Fatal("reservation was not promoted")
	}
	if err := s.authorizeAgentMCPConfig(agent, config); err != nil {
		t.Fatal(err)
	}
	runtime.StopAgent(agent.ID)
	if err := s.authorizeAgentMCPConfig(agent, config); err == nil {
		t.Fatal("removed runtime agent retained authorization")
	}
	var cleaned atomic.Int32
	pending = &EnvironmentAgent{AgentID: agent.ID, Alias: "main", cleanup: func() { cleaned.Add(1) }}
	if err := runtime.reserveAgent(pending); err != nil {
		t.Fatal(err)
	}
	runtime.Stop()
	if cleaned.Load() != 1 || len(runtime.pendingAgents) != 0 {
		t.Fatal("teardown did not clean pending agent")
	}
	if err := runtime.activateReservedAgent(pending); err == nil {
		t.Fatal("startup completed after teardown")
	}
	if err := runtime.reserveAgent(pending); err == nil {
		t.Fatal("stopped runtime accepted startup")
	}
}

func TestRuntimeManagedMCPBridgeRejectsInvalidReferences(t *testing.T) {
	s, runtime, _, _ := runtimeAuthorizationFixture(t)
	for _, path := range []string{"/mcp/runtime/rt-auth/wrong", "/mcp/runtime/missing/capability", "/mcp/runtime/rt-auth/capability/", "/mcp/runtime/rt-auth/capability/extra"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		s.handleRuntimeManagedMCPBridge(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d", path, rec.Code)
		}
	}
	runtime.Stop()
	req := httptest.NewRequest(http.MethodPost, s.runtimeManagedMCPURL(runtime.ID, "capability"), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.handleRuntimeManagedMCPBridge(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatal("stopped runtime bridge accepted request")
	}
}

func TestRuntimeManagedMCPOrdinaryAuthorizationUnchanged(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	agent := &Agent{ID: 41, UserID: 1, ProjectID: "proj-1"}
	for _, item := range []struct {
		source, prefix string
		connection     int64
	}{
		{managedMCPSource, "custom/", 0}, {"local", "connection/", 91},
	} {
		record, err := s.store.CreateMCPServerExt(MCPServerInput{UserID: 1, Name: item.prefix, Source: item.source, Transport: "stdio", Command: "unused", ProjectID: "proj-1", ConnectionID: item.connection})
		if err != nil {
			t.Fatal(err)
		}
		id := record.ID
		if item.connection > 0 {
			id = item.connection
		}
		raw := fmt.Sprintf("http://127.0.0.1:5280/mcp/%s%d", item.prefix, id)
		config := runtimeMCPTestConfig(raw)
		if err := s.authorizeAgentMCPConfig(agent, config); err != nil {
			t.Fatalf("%s: %v", item.prefix, err)
		}
		u, _ := url.Parse(config["mcp_servers"].([]any)[0].(map[string]any)["url"].(string))
		if u.Query().Get("mcp_token") != internalMCPCapability(s.instanceSecret, u.Path) {
			t.Fatal("ordinary route capability missing")
		}
		foreign := *agent
		foreign.ProjectID = "other-project"
		if err := s.authorizeAgentMCPConfig(&foreign, runtimeMCPTestConfig(raw)); err == nil {
			t.Fatal("cross-project ordinary MCP allowed")
		}
	}
	// The adjacent private app gateway retains its existing authorization path.
	if err := s.authorizeAgentMCPConfig(agent, runtimeMCPTestConfig(s.runtimeMCPAttachmentURL("rt", "token"))); err != nil {
		t.Fatalf("adjacent attachment: %v", err)
	}
}

func TestRuntimeMCPAttachmentGatewayUnchanged(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "evals")
	runtime, err := s.environments.Create(EnvironmentSpec{ID: "rt-attachment", ProjectID: "proj-1", RuntimeOwnerInstallID: owner, CreatorUserID: 1, NetworkMode: EdgeBlock})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.appInstallToken(owner)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := make(chan bool, 1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r.URL.Path == "/runtime-sessions/session-1/mcp" && r.Header.Get("Authorization") == "Bearer "+token
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(app.Close)
	s.installedApps.Add(&InstalledApp{InstallID: owner, AppName: "evals", SidecarURL: app.URL})
	if err := runtime.AddMCPAttachment(RuntimeMCPAttachment{ID: "attachment-1", Name: "eval-mocks", Token: "private-capability", InstallID: owner, Path: "/runtime-sessions/session-1/mcp"}); err != nil {
		t.Fatal(err)
	}
	raw := s.runtimeMCPAttachmentURL(runtime.ID, "private-capability")
	if err := s.authorizeAgentMCPConfig(&Agent{ID: 41, UserID: 1, ProjectID: "proj-1"}, runtimeMCPTestConfig(raw)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, raw, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
	rec := httptest.NewRecorder()
	s.handleRuntimeMCPGateway(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("adjacent gateway: %d %s", rec.Code, rec.Body.String())
	}
	if !<-forwarded {
		t.Fatal("adjacent attachment used incorrect path or installation credentials")
	}
}
