package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const helperSecretMCPConfig = `{
	"model_override": "gpt-test",
	"service_tier_overrides": {"openai": "flex"},
	"helper_activated": true,
	"mcp_servers": [
		{"name": "crm", "transport": "http", "url": "http://127.0.0.1:5280/mcp/7?mcp_token=SECRET-CAP"},
		{"name": "remote", "transport": "http", "url": "https://example.com/mcp?key=SECRET-REMOTE", "headers": {"Authorization": "Bearer SECRET-HEADER"}}
	]
}`

func assertNoHelperSecrets(t *testing.T, where, body string) {
	t.Helper()
	for _, secret := range []string{"SECRET-CAP", "SECRET-REMOTE", "SECRET-HEADER", "mcp_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("%s leaked %q: %s", where, secret, body)
		}
	}
}

func newHelperWithSecretMCPs(t *testing.T) (*Server, *Agent) {
	t.Helper()
	s := newTestServer(t)
	user, err := s.store.CreateUser("helper-redact@test.local", "hash")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.store.GetOrCreatePlatformHelper(user.ID, "directive")
	if err != nil {
		t.Fatal(err)
	}
	helper.Config = helperSecretMCPConfig
	if err := s.store.UpdateAgent(helper); err != nil {
		t.Fatal(err)
	}
	return s, helper
}

func TestPlatformHelperResponsesOmitMCPSecrets(t *testing.T) {
	s, helper := newHelperWithSecretMCPs(t)

	// /platform/helper and /platform/helper/status both serialize this.
	sanitized, _ := json.Marshal(sanitizedPlatformHelper(helper, false))
	assertNoHelperSecrets(t, "sanitizedPlatformHelper", string(sanitized))
	var cfg map[string]any
	if err := json.Unmarshal([]byte(sanitizedPlatformHelper(helper, false).Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model_override"] != "gpt-test" || cfg["service_tier_overrides"] == nil {
		t.Fatalf("settings fields the dashboard reads were dropped: %v", cfg)
	}
	if helper.Config != helperSecretMCPConfig {
		t.Fatal("sanitizing mutated the stored helper")
	}

	// Generic GET /instances/<id>.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/instances/%d", helper.ID), nil)
	req.Header.Set("X-User-ID", fmt.Sprint(helper.UserID))
	s.handleInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET instance status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertNoHelperSecrets(t, "GET /instances/<helper>", rec.Body.String())
	if !strings.Contains(rec.Body.String(), "gpt-test") {
		t.Fatalf("GET instance dropped model_override: %s", rec.Body.String())
	}

	// Stopped-agent /config, served from the agent's config.json on disk.
	dir := s.agents.instanceDir(helper.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(helperSecretMCPConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := httptest.NewRecorder()
	s.serveStoppedInstanceData(stopped, helper, "/config")
	assertNoHelperSecrets(t, "stopped /config", stopped.Body.String())
	if !strings.Contains(stopped.Body.String(), `"name":"crm"`) {
		t.Fatalf("stopped /config dropped MCP names: %s", stopped.Body.String())
	}
}

func TestRedactPlatformHelperConfigBodyKeepsAttachmentState(t *testing.T) {
	core := `{"directive":"d","mcp_servers":[{"name":"apteva-server","connected":true,"transport":"http","url":"http://127.0.0.1:5280/mcp/platform?mcp_token=SECRET-CAP"},{"name":"local","command":"/bin/x","env":{"K":"SECRET-REMOTE"}}]}`
	out, rewritten := redactPlatformHelperConfigBody([]byte(core))
	if !rewritten {
		t.Fatal("expected rewrite")
	}
	assertNoHelperSecrets(t, "core /config", string(out))
	var payload struct {
		MCPServers []map[string]any `json:"mcp_servers"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.MCPServers) != 2 || payload.MCPServers[0]["name"] != "apteva-server" || payload.MCPServers[0]["connected"] != true {
		t.Fatalf("attachment state lost: %+v", payload.MCPServers)
	}
	if _, rewritten := redactPlatformHelperConfigBody([]byte(`{"directive":"d"}`)); rewritten {
		t.Fatal("body without mcp_servers should pass through")
	}
}
