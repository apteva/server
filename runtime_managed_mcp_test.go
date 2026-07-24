package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/internal/managedmcp"
)

func seedRuntimeManagedMCP(t *testing.T, s *Server, projectID string) *MCPServerRecord {
	t.Helper()
	s.dataDir = t.TempDir()
	s.secret = bytes.Repeat([]byte{0x42}, 32)
	cfg := normalizeManagedMCPConfig(managedMCPConfig{Env: map[string]string{"RUNTIME_VALUE": "isolated"}})
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	def := managedmcp.Definition{
		Version: managedmcp.DefinitionVersion,
		Tools: []managedmcp.Tool{{
			Name: "echo", Description: "Echo a value.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}},
			Handler:     "tools/echo.js",
			Code:        `return {message: input.message, configured: apteva.env("RUNTIME_VALUE") || ""};`,
		}},
	}
	revision, err := managedMCPRevision(def, cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.store.CreateMCPServerExt(MCPServerInput{
		UserID: 1, Name: "runtime-tools", Description: "Runtime tools",
		Source: managedMCPSource, Transport: "stdio", Args: "[]",
		EncryptedEnv: encrypted, ProjectID: projectID, UpstreamID: revision, ToolCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagedMCPSource(s.managedMCPSourceDir(record.ID), def); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRuntimeManagedMCPIsIsolatedCallableAndSnapshotted(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	s.mcpManager = NewMCPManager()
	record := seedRuntimeManagedMCP(t, s, "proj-1")
	t.Setenv("APTEVA_MCP_RUNNER_BIN", buildManagedMCPRunner(t))

	mux := http.NewServeMux()
	mux.Handle("/api/runtime-managed-mcp/", http.StripPrefix("/api", http.HandlerFunc(s.handleRuntimeManagedMCPGateway)))
	local := httptest.NewServer(mux)
	t.Cleanup(local.Close)
	parsed, _ := url.Parse(local.URL)
	s.port = parsed.Port()

	owner := seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesRead, sdk.PermRuntimesCall, sdk.PermRuntimeCatalogRead)
	created := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-managed", TTLSeconds: 60, MCPServerIDs: []int64{record.ID},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var summary sdk.RuntimeSummary
	if err := json.Unmarshal(created.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.ManagedMCPs) != 1 || summary.ManagedMCPs[0].Name != "runtime-tools" || summary.ManagedMCPs[0].Revision != record.UpstreamID {
		t.Fatalf("managed MCP summary=%#v", summary.ManagedMCPs)
	}

	called := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-managed/managed-mcps/runtime-tools/call", map[string]any{
		"tool": "echo", "input": map[string]any{"message": "hello"},
	})
	if called.Code != http.StatusOK || !strings.Contains(called.Body.String(), `"message":"hello"`) || !strings.Contains(called.Body.String(), `"configured":"isolated"`) {
		t.Fatalf("call status=%d body=%s", called.Code, called.Body.String())
	}

	snapshot := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-managed/snapshots", sdk.RuntimeSnapshotRequest{ID: "snap-managed"})
	if snapshot.Code != http.StatusCreated {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	var snap sdk.RuntimeSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.ManagedMCPs) != 1 || snap.ManagedMCPs[0].SourceID != record.ID {
		t.Fatalf("snapshot managed MCPs=%#v", snap.ManagedMCPs)
	}

	destroyed := runtimeAPIRequest(t, s, owner, http.MethodDelete, "/apps/callback/runtimes/rt-managed", nil)
	if destroyed.Code != http.StatusOK {
		t.Fatalf("destroy status=%d body=%s", destroyed.Code, destroyed.Body.String())
	}
	restored := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-restored", TTLSeconds: 60, SnapshotID: snap.ID,
	})
	if restored.Code != http.StatusCreated {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
	runtimeAPIRequest(t, s, owner, http.MethodDelete, "/apps/callback/runtimes/rt-restored", nil)
	if err := s.store.UpdateMCPServerUpstreamID(record.ID, "changed-revision"); err != nil {
		t.Fatal(err)
	}
	conflict := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-stale", TTLSeconds: 60, SnapshotID: snap.ID,
	})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("stale restore status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	catalog := runtimeAPIRequest(t, s, owner, http.MethodGet, "/apps/callback/runtimes/catalog/managed-mcps?project_id=proj-1", nil)
	if catalog.Code != http.StatusOK || !strings.Contains(catalog.Body.String(), `"name":"runtime-tools"`) {
		t.Fatalf("catalog status=%d body=%s", catalog.Code, catalog.Body.String())
	}
}

func TestEnvironmentConnectionMCPURLRejectsGlobalCustomBridge(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:5280/mcp/42":               true,
		"http://127.0.0.1:5280/mcp/42?project_id=p":  true,
		"http://127.0.0.1:5280/mcp/custom/42":        false,
		"http://127.0.0.1:5280/mcp/runtime/rt/token": false,
		"http://127.0.0.1:5280/api/apps/example/mcp": false,
		"not a URL": false,
	}
	for raw, want := range cases {
		if got := isEnvironmentConnectionMCPURL(raw); got != want {
			t.Fatalf("%q = %v, want %v", raw, got, want)
		}
	}
}

func TestRuntimeManagedMCPSourceWorkspaceIsServerOwned(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	record := seedRuntimeManagedMCP(t, s, "proj-1")
	want := filepath.Join(s.dataDir, "mcp-servers", strconv.FormatInt(record.ID, 10), "source")
	if got := s.managedMCPSourceDir(record.ID); got != want {
		t.Fatalf("workspace=%q want=%q", got, want)
	}
}
