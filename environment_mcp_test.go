package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mcpCall posts one JSON-RPC call to the environment-mcp handler and returns the
// raw "result".
func environmentMCPRPC(t *testing.T, s *Server, method string, params any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/environment-mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleEnvironmentMCP(w, r)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v (%s)", method, err, w.Body.String())
	}
	if env.Error != nil {
		t.Fatalf("%s error: %s", method, env.Error.Message)
	}
	return env.Result
}

// TestEnvironmentMCP_ToolsList: the control surface advertises the environment tools
// (fast, no app build).
func TestEnvironmentMCP_ToolsList(t *testing.T) {
	s := &Server{}
	res := environmentMCPRPC(t, s, "tools/list", map[string]any{})
	for _, want := range []string{"environment_create_for_agent", "environment_call_app", "environment_list", "environment_destroy"} {
		if !strings.Contains(string(res), want) {
			t.Fatalf("tools/list missing %q: %s", want, res)
		}
	}
}

// TestEnvironmentMCP_CallApp_RealStorage (gated): the meta-agent's seeding path —
// environment_call_app drives the real storage app's files_upload, landing a
// real file in the environment's isolated DB.
func TestEnvironmentMCP_CallApp_RealStorage(t *testing.T) {
	requireRealAppEnvironmentTests(t)
	src := findAppSource(t, "storage")
	s := newEnvironmentTestServer(t)

	environment, err := s.environments.Create(EnvironmentSpec{
		ID: "mcp-w", ProjectID: "mcp-w", GatewayURL: s.localGatewayURL(),
		AppSrcDirs: map[string]string{"storage": src}, Mode: EdgeBlock, HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	defer environment.Stop()

	// Drive seeding purely through the MCP tool surface — exactly what the
	// meta-agent would emit.
	_ = environmentMCPRPC(t, s, "tools/call", map[string]any{
		"name": "environment_call_app",
		"arguments": map[string]any{
			"environment_id": "mcp-w", "app": "storage", "tool": "files_upload",
			"input": map[string]any{
				"name":           "via-mcp.txt",
				"content_base64": base64.StdEncoding.EncodeToString([]byte("seeded through the MCP tool")),
			},
		},
	})

	dbPath, _ := environment.AppDBPath("storage")
	if n := countRows(t, dbPath, `SELECT COUNT(*) FROM files WHERE name='via-mcp.txt' AND deleted_at IS NULL`); n != 1 {
		t.Fatalf("expected 1 file seeded via environment_call_app, got %d", n)
	}
	t.Logf("✓ environment seeded via MCP: environment_call_app -> real storage.files_upload -> 1 real file row")
}
