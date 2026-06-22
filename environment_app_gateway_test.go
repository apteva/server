package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEnvironmentAppGateway_BrokersToken (gated) proves an agent core can reach a
// token-protected in-environment app: we call the gateway with NO Authorization;
// it must inject the install's dev token, so storage accepts the call and the
// file lands. Without brokering, storage's /mcp would 401 and nothing writes.
func TestEnvironmentAppGateway_BrokersToken(t *testing.T) {
	requireRealAppEnvironmentTests(t)
	src := findAppSource(t, "storage")
	s := newEnvironmentTestServer(t)

	environment, err := s.environments.Create(EnvironmentSpec{
		ID: "gw-w", ProjectID: "gw-w", GatewayURL: s.localGatewayURL(),
		AppSrcDirs: map[string]string{"storage": src}, Mode: EdgeBlock, HealthBudget: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	defer environment.Stop()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "files_upload",
			"arguments": map[string]any{
				"name":           "gw.txt",
				"content_base64": base64.StdEncoding.EncodeToString([]byte("through the gateway")),
			},
		},
	})
	// No Authorization header — the gateway must add it.
	r := httptest.NewRequest("POST", "/environment-app-gateway/gw-w/storage/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleEnvironmentAppGateway(w, r)

	if w.Code != 200 {
		t.Fatalf("gateway returned %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error != nil {
		t.Fatalf("MCP error through gateway: %s", env.Error.Message)
	}

	dbPath, _ := environment.AppDBPath("storage")
	if n := countRows(t, dbPath, `SELECT COUNT(*) FROM files WHERE name='gw.txt' AND deleted_at IS NULL`); n != 1 {
		t.Fatalf("expected 1 file written via brokered gateway, got %d", n)
	}
	t.Logf("✓ agent→in-environment-app works: gateway injected the install token; storage wrote the file")
}
