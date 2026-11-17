package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func starterRPC(t *testing.T, s *Server, agent *Agent, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req := httptest.NewRequest("POST", addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, agent)), strings.NewReader(string(raw)))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(agent.ID))
	rec := httptest.NewRecorder()
	s.handlePlatformMCP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("MCP %s status=%d: %s", name, rec.Code, rec.Body.String())
	}
	var reply map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestStarterPresetsApplyAndCreateScopedSpecialist(t *testing.T) {
	catalog, err := loadProjectPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, preset := range catalog.Presets {
		if preset.Category != "personal" && preset.Category != "work" {
			continue
		}
		t.Run(preset.ID, func(t *testing.T) {
			if len(preset.Agents) != 1 {
				t.Fatal("expected one starter")
			}
			spec := preset.Agents[0]
			for _, app := range []string{"tasks", "notes", "a2a", "conversations"} {
				if !containsString(spec.Apps, app) {
					t.Fatalf("missing %s", app)
				}
			}
			s := newTestServer(t)
			seedPresetProject(t, s, "fresh")
			seedPresetProject(t, s, "untouched")
			original, _ := s.store.CreateAgent(1, "Existing", "Preserve me", "cautious", `{"model":"custom"}`, "untouched")
			fixture, _ := presetEvalsFixture(t)
			for _, name := range projectPresetApps(preset) {
				if name == "evals" {
					seedPresetEvalsApp(t, s, "fresh", fixture.URL)
					continue
				}
				id := seedAppWithTools(t, s, name, "fresh", []string{name + "_fixture"})
				if err := s.registerAppMCP(id); err != nil {
					t.Fatal(err)
				}
			}
			// Preview is read-only; cancelling it creates no agent or grant.
			preview, err := s.compileProjectPresetPreview(context.Background(), 1, "fresh", ProjectPresetPreviewRequest{PresetID: preset.ID, Description: "Local starter test"})
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Agents) != 1 || len(preview.Agents[0].Capabilities) != 1 {
				t.Fatal("preview lost the management grant")
			}
			rows, _ := s.store.ListAgentsInProject("fresh")
			if len(rows) != 0 {
				t.Fatal("preview provisioned agents")
			}
			apply := func() {
				req := authedRequest(t, "POST", "/projects/fresh/setup/apply", "", map[string]any{"preset_id": preset.ID, "description": "Local starter test"})
				req.Header.Set("X-Apteva-Operator-ID", "1")
				rec := httptest.NewRecorder()
				s.handleProject(rec, req)
				if rec.Code != 200 {
					t.Fatalf("apply: %s", rec.Body.String())
				}
			}
			apply()
			rows, _ = s.store.ListAgentsInProject("fresh")
			if len(rows) != 1 {
				t.Fatalf("agents=%d", len(rows))
			}
			starter, _ := s.store.GetAgentByID(rows[0].ID)
			if !platformMCPAttached(starter) || !platformToolAllowed(starter, "agents_create") || platformToolAllowed(starter, "agents_delete") {
				t.Fatal("wrong management grant")
			}
			var cfg map[string]any
			json.Unmarshal([]byte(starter.Config), &cfg)
			if !hasMCPName(mcpMaps(cfg["mcp_servers"]), "a2a") || !hasMCPName(mcpMaps(cfg["mcp_servers"]), "conversations") {
				t.Fatal("missing normal app attachments")
			}
			apply()
			rows, _ = s.store.ListAgentsInProject("fresh")
			if len(rows) != 1 {
				t.Fatal("reapply duplicated starter")
			}
			again, _ := s.store.GetAgentByID(starter.ID)
			if again.Config != starter.Config || again.Directive != starter.Directive {
				t.Fatal("reapply changed existing starter")
			}
			// Execute the actual gateway handler through the regular agent HTTP API.
			api := newGatewayAgentAPITestServer(s)
			defer api.Close()
			client := gatewayAPIClient{baseURL: api.URL + "/api", userID: 1, instanceSecret: s.instanceSecret}
			s.platformGatewayExec = func(_ context.Context, _ int64, project string, body []byte) ([]byte, error) {
				var rpc struct {
					ID     any `json:"id"`
					Params struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				json.Unmarshal(body, &rpc)
				result, err := handleGatewayAgentTool(rpc.Params.Name, rpc.Params.Arguments, project, client, s.store, "/tmp/server")
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
			}
			attachments := []int64{}
			for _, name := range []string{"a2a", "conversations"} {
				var id int64
				s.store.db.QueryRow(`SELECT i.id FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE a.name=? AND i.project_id='fresh'`, name).Scan(&id)
				attachments = append(attachments, id)
			}
			args := map[string]any{"name": "Requested specialist", "directive": "Answer the user and coordinate only when assigned.", "idempotency_key": "one-logical-request", "start": false, "bound_app_install_ids": attachments, "project_id": "forged"}
			for i := 0; i < 2; i++ {
				reply := starterRPC(t, s, starter, "agents_create", args)
				if strings.Contains(string(mustJSON(reply)), `"isError":true`) {
					t.Fatalf("create: %v", reply)
				}
			}
			rows, _ = s.store.ListAgentsInProject("fresh")
			if len(rows) != 2 {
				t.Fatalf("retry created %d agents", len(rows))
			}
			for _, listed := range rows {
				freshChild, err := s.store.GetAgentByID(listed.ID)
				if err != nil {
					t.Fatal(err)
				}
				child := *freshChild
				if child.ID == starter.ID {
					continue
				}
				if platformMCPAttached(&child) {
					t.Fatal("specialist inherited management")
				}
				var c map[string]any
				json.Unmarshal([]byte(child.Config), &c)
				if !hasMCPName(mcpMaps(c["mcp_servers"]), "conversations") || !hasMCPName(mcpMaps(c["mcp_servers"]), "a2a") {
					t.Fatal("child cannot converse/coordinate")
				}
			}
			preserved, _ := s.store.GetAgentByID(original.ID)
			{
				if preserved.Name != original.Name || preserved.Config != original.Config || preserved.Directive != original.Directive || preserved.ProjectID != original.ProjectID {
					t.Fatal("other project changed")
				}
			}
		})
	}
}

func TestStarterManagementGrantRejectsEscalation(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "p")
	cfg, err := s.configWithAgentCapabilities(&Agent{UserID: 1, ProjectID: "p", Config: "{}"}, []AgentCapability{{Name: platformMCPName, AllowedTools: []string{"agents_create", "agents_update", "agents_get"}}})
	if err != nil {
		t.Fatal(err)
	}
	starter, _ := s.store.CreateAgent(1, "Starter", "test", "cautious", cfg, "p")
	other, _ := s.store.CreateAgent(1, "Outside", "test", "cautious", "{}", "other")
	called := false
	s.platformGatewayExec = func(context.Context, int64, string, []byte) ([]byte, error) {
		called = true
		return []byte(`{"result":{}}`), nil
	}
	cases := []struct {
		name string
		args map[string]any
	}{
		{"agents_delete", map[string]any{"id": starter.ID}},
		{"agents_update", map[string]any{"id": starter.ID, "name": "Escalation"}},
		{"agents_get", map[string]any{"id": other.ID}},
		{"agents_create", map[string]any{"name": "Bad", "config": map[string]any{"include_apteva_server": true}}},
		{"agents_create", map[string]any{"name": "Bad", "config": map[string]any{"platform_mcp_tools": []string{"agents_delete"}}}},
		{"agents_create", map[string]any{"name": "Bad", "config": map[string]any{"mcp_servers": []any{}}}},
	}
	for _, c := range cases {
		called = false
		reply := starterRPC(t, s, starter, c.name, c.args)
		if called || !strings.Contains(string(mustJSON(reply)), `"isError":true`) {
			t.Fatalf("allowed %s %v", c.name, c.args)
		}
	}
	legacy := &Agent{Config: `{"include_apteva_server":true}`}
	if !platformToolAllowed(legacy, "agents_delete") {
		t.Fatal("legacy behavior changed")
	}
	empty := &Agent{Config: `{"platform_mcp_tools":[]}`}
	if platformToolAllowed(empty, "agents_create") {
		t.Fatal("empty grant did not deny")
	}
	for _, caps := range [][]AgentCapability{{{Name: "unknown", AllowedTools: []string{}}}, {{Name: platformMCPName}}, {{Name: platformMCPName, AllowedTools: []string{"unknown"}}}} {
		if validateAgentCapabilities(caps) == nil {
			t.Fatal("invalid grant accepted")
		}
	}
}

func TestStarterAssistantPreferenceDoesNotOverwriteOperatorChoice(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "p")
	if err := s.initializeStarterAssistant(1, "p", 42); err != nil {
		t.Fatal(err)
	}
	first, rev := s.store.GetUserUILayoutWithRevision(1)
	if !strings.Contains(string(first), `"defaultTarget":{"id":42,"kind":"agent"}`) {
		t.Fatalf("preference missing: %s", first)
	}
	if err := s.initializeStarterAssistant(1, "p", 99); err != nil {
		t.Fatal(err)
	}
	second, newRev := s.store.GetUserUILayoutWithRevision(1)
	if string(first) != string(second) || rev != newRev {
		t.Fatal("saved preference changed")
	}
}

func TestStarterToolListUsesSameGrantAsCalls(t *testing.T) {
	s := newTestServer(t)
	seedPresetProject(t, s, "p")
	agent, _ := s.store.CreateAgent(1, "Starter", "test", "cautious", `{"include_apteva_server":true,"platform_mcp_tools":["agents_create"]}`, "p")
	s.platformGatewayExec = func(context.Context, int64, string, []byte) ([]byte, error) {
		return []byte(`{"result":{"tools":[{"name":"agents_create"},{"name":"agents_delete"}]}}`), nil
	}
	req := httptest.NewRequest(http.MethodPost, addQueryParam(platformMCPPath, "mcp_token", platformMCPToken(s.instanceSecret, agent)), strings.NewReader(`{"method":"tools/list","id":1}`))
	req.RemoteAddr = "127.0.0.1:12"
	req.Header.Set("X-Apteva-Caller-Agent", itoa64(agent.ID))
	rec := httptest.NewRecorder()
	s.handlePlatformMCP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "agents_delete") || !strings.Contains(rec.Body.String(), "agents_create") {
		t.Fatal(rec.Body.String())
	}
}

func TestStarterGrantSurvivesPortablePresetEnvelope(t *testing.T) {
	catalog, _ := loadProjectPresetCatalog()
	preset := catalog.ByID["personal-assistant"]
	envelope := systemPresetEnvelope(preset)
	raw, _ := json.Marshal(envelope)
	var decoded Preset
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	restored := projectPresetFromEnvelope(decoded)
	if !restored.Recommended || len(restored.Agents[0].Capabilities) != 1 || !containsString(restored.Agents[0].Capabilities[0].AllowedTools, "agents_create") {
		t.Fatal("portable preset lost capability/recommendation")
	}
}
