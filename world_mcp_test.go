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

// mcpCall posts one JSON-RPC call to the world-mcp handler and returns the
// raw "result".
func worldMCPRPC(t *testing.T, s *Server, method string, params any) json.RawMessage {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/world-mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleWorldMCP(w, r)
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

// TestWorldMCP_ToolsList: the control surface advertises the world tools
// (fast, no app build).
func TestWorldMCP_ToolsList(t *testing.T) {
	s := &Server{}
	res := worldMCPRPC(t, s, "tools/list", map[string]any{})
	for _, want := range []string{"world_create_for_agent", "world_call_app", "world_list", "world_destroy"} {
		if !strings.Contains(string(res), want) {
			t.Fatalf("tools/list missing %q: %s", want, res)
		}
	}
}

// TestWorldMCP_CallApp_RealStorage (gated): the meta-agent's seeding path —
// world_call_app drives the real storage app's files_upload, landing a real
// file in the world's isolated DB. This is "worlds seeded via MCP".
func TestWorldMCP_CallApp_RealStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("real-app world test builds the storage sidecar")
	}
	src := findAppSource(t, "storage")
	s := newWorldTestServer(t)

	world, err := s.worlds.Create(WorldSpec{
		ID: "mcp-w", ProjectID: "mcp-w", GatewayURL: s.localGatewayURL(),
		AppSrcDirs: map[string]string{"storage": src}, Mode: EdgeBlock, HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create world: %v", err)
	}
	defer world.Stop()

	// Drive seeding purely through the MCP tool surface — exactly what the
	// meta-agent would emit.
	_ = worldMCPRPC(t, s, "tools/call", map[string]any{
		"name": "world_call_app",
		"arguments": map[string]any{
			"world_id": "mcp-w", "app": "storage", "tool": "files_upload",
			"input": map[string]any{
				"name":           "via-mcp.txt",
				"content_base64": base64.StdEncoding.EncodeToString([]byte("seeded through the MCP tool")),
			},
		},
	})

	dbPath, _ := world.AppDBPath("storage")
	if n := countRows(t, dbPath, `SELECT COUNT(*) FROM files WHERE name='via-mcp.txt' AND deleted_at IS NULL`); n != 1 {
		t.Fatalf("expected 1 file seeded via world_call_app, got %d", n)
	}
	t.Logf("✓ world seeded via MCP: world_call_app → real storage.files_upload → 1 real file row")
}
