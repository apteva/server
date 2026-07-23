package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func newRuntimeAPITestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.port = "5280"
	s.environments = NewEnvironmentManager(environmentDataRoot(t.TempDir()))
	s.environments.server = s
	s.installedApps = NewInstalledAppsRegistry()
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO users (id, email, password_hash) VALUES (1, 'runtime@test.local', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO projects (id, user_id, name) VALUES ('proj-1', 1, 'Runtime test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT OR IGNORE INTO project_members (project_id, user_id, role, added_by) VALUES ('proj-1', 1, 'owner', 1)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.environments.StopAll)
	return s
}

func seedRuntimeAPIInstall(t *testing.T, s *Server, name string, permissions ...sdk.Permission) int64 {
	t.Helper()
	manifest := sdk.Manifest{Name: name, DisplayName: strings.ToUpper(name), Requires: sdk.Requires{Permissions: permissions}}
	return seedInstallWithBindings(t, s, name, manifest, nil)
}

func runtimeAPIRequest(t *testing.T, s *Server, installID int64, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func TestRuntimeAPI_CreateIsOpaqueOwnedAndDestroyable(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	other := seedRuntimeAPIInstall(t, s, "other-runtime-owner", sdk.PermRuntimesManage, sdk.PermRuntimesRead)

	rec := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-owned", TTLSeconds: 60, Subscriptions: []sdk.RuntimeSubscription{{App: "crm", Topic: "contact.created"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, leaked := range []string{"proxy_url", "mcp_url", "data_dir", "api_key", "\"port\""} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("runtime response leaked %q: %s", leaked, rec.Body.String())
		}
	}
	var created sdk.RuntimeSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "rt-owned" || created.NetworkMode != sdk.RuntimeNetworkBlock || created.ProjectID != "proj-1" {
		t.Fatalf("unexpected runtime: %+v", created)
	}
	if created.ExpiresAt.IsZero() {
		t.Fatal("runtime expiry missing")
	}
	live, _ := s.environments.Get("rt-owned")
	if subscriptions := live.SubscriptionSpecs(); len(subscriptions) != 1 || subscriptions[0].TargetAgentAlias != "main" || !subscriptions[0].Enabled {
		t.Fatalf("runtime subscriptions not normalized: %+v", subscriptions)
	}

	foreign := runtimeAPIRequest(t, s, other, http.MethodGet, "/apps/callback/runtimes/rt-owned", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("other install read status=%d body=%s", foreign.Code, foreign.Body.String())
	}

	deleted := runtimeAPIRequest(t, s, owner, http.MethodDelete, "/apps/callback/runtimes/rt-owned", nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if _, ok := s.environments.Get("rt-owned"); ok {
		t.Fatal("runtime still live after delete")
	}
}

func TestRuntimeAPI_SnapshotArtifactsAreOwnerScoped(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "snapshot-owner", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	other := seedRuntimeAPIInstall(t, s, "snapshot-other", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	created := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-snapshot"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	snapshot := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-snapshot/snapshots", sdk.RuntimeSnapshotRequest{ID: "snap-owned"})
	if snapshot.Code != http.StatusCreated {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	if strings.Contains(snapshot.Body.String(), "owner_install_id") {
		t.Fatalf("snapshot exposed ownership internals: %s", snapshot.Body.String())
	}
	foreignList := runtimeAPIRequest(t, s, other, http.MethodGet, "/apps/callback/runtimes/artifacts/snapshots", nil)
	if foreignList.Code != http.StatusOK || strings.Contains(foreignList.Body.String(), "snap-owned") {
		t.Fatalf("snapshot leaked in foreign list: status=%d body=%s", foreignList.Code, foreignList.Body.String())
	}
	foreignDelete := runtimeAPIRequest(t, s, other, http.MethodDelete, "/apps/callback/runtimes/artifacts/snapshots/snap-owned", nil)
	if foreignDelete.Code != http.StatusNotFound {
		t.Fatalf("foreign delete status=%d body=%s", foreignDelete.Code, foreignDelete.Body.String())
	}
	ownerDelete := runtimeAPIRequest(t, s, owner, http.MethodDelete, "/apps/callback/runtimes/artifacts/snapshots/snap-owned", nil)
	if ownerDelete.Code != http.StatusOK {
		t.Fatalf("owner delete status=%d body=%s", ownerDelete.Code, ownerDelete.Body.String())
	}
}

func TestRuntimeAPI_RequiresDeclaredPermission(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "unprivileged")
	rec := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-denied"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.environments.Get("rt-denied"); ok {
		t.Fatal("permission-denied runtime was created")
	}
}

func TestRuntimeAPI_RejectsOversizedResourceSet(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "bounded-runtime", sdk.PermRuntimesManage)
	appIDs := make([]int64, maxRuntimeApps+1)
	rec := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-too-large", AppInstallIDs: appIDs})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.environments.Get("rt-too-large"); ok {
		t.Fatal("oversized runtime was created")
	}
}

func TestRuntimeAPI_IntegrationCatalogIncludesMockResponses(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{Slug: "facebook", Name: "Facebook", Description: "Meta pages API", Tools: []AppToolDef{{Name: "pages_list", Description: "List pages", InputSchema: map[string]any{"type": "object"}, MockResponse: json.RawMessage(`{"data":[]}`)}}})
	installID := seedRuntimeAPIInstall(t, s, "environments", sdk.PermRuntimeCatalogRead)

	list := runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/runtimes/catalog/integrations", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"slug":"facebook"`) {
		t.Fatalf("catalog status=%d body=%s", list.Code, list.Body.String())
	}
	tools := runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/runtimes/catalog/integrations/facebook/tools", nil)
	if tools.Code != http.StatusOK || !strings.Contains(tools.Body.String(), `"mock_response":{"data":[]}`) {
		t.Fatalf("tools status=%d body=%s", tools.Code, tools.Body.String())
	}
}

func TestRuntimeAPI_PrivateMCPAttachmentDoesNotExposeCapability(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "evals", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	created := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-mcp"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	attached := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes/rt-mcp/mcp-attachments", sdk.RuntimeMCPAttachmentRequest{Name: "eval-mocks", Path: "/runtime-sessions/session-1/mcp"})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status=%d body=%s", attached.Code, attached.Body.String())
	}
	if strings.Contains(attached.Body.String(), "token") || strings.Contains(attached.Body.String(), "runtime-mcp-gateway") {
		t.Fatalf("attachment leaked capability: %s", attached.Body.String())
	}

	bad := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes/rt-mcp/mcp-attachments", sdk.RuntimeMCPAttachmentRequest{Name: "evil", Path: "https://example.com/mcp"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("external attachment status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestRuntimeAPI_EmptyTelemetryIsJSONArray(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "runtime-telemetry", sdk.PermRuntimesManage, sdk.PermRuntimesCall)
	created := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-telemetry"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	runtime, _ := s.environments.Get("rt-telemetry")
	if err := runtime.AttachAgent(&EnvironmentAgent{AgentID: 991, Alias: "main", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec := runtimeAPIRequest(t, s, installID, http.MethodGet, "/apps/callback/runtimes/rt-telemetry/agents/main/telemetry", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty telemetry must be an array, got %s", rec.Body.String())
	}
}

func TestRuntimeAPI_WaitReturnsNormalizedTraceAndMetrics(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "runtime-wait", sdk.PermRuntimesManage, sdk.PermRuntimesCall)
	created := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-wait"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/threads/main/history" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("after") == "0" {
			writeJSON(w, map[string]any{"entries": []any{
				map[string]any{"role": "user", "content": "Create the contact"},
				map[string]any{"role": "assistant", "content": "I will create it.", "tool_calls": []any{map[string]any{"id": "call-1", "name": "crm.contacts_create", "args": map[string]any{"email": "test@example.com"}}}},
				map[string]any{"role": "user", "tool_results": []any{map[string]any{"call_id": "call-1", "content": `{"id":"contact-1"}`}}},
			}, "next_cursor": 3})
			return
		}
		writeJSON(w, map[string]any{"entries": []any{}, "next_cursor": 3})
	}))
	defer core.Close()
	port, err := strconv.Atoi(strings.TrimPrefix(core.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := s.environments.Get("rt-wait")
	started := time.Now().Add(-time.Second)
	if err := runtime.AttachAgent(&EnvironmentAgent{AgentID: 992, Alias: "main", Port: port, CreatedAt: started, Provider: "anthropic", Model: "claude-test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertTelemetry([]TelemetryEvent{
		{ID: "wait-llm", AgentID: 992, ThreadID: "main", Type: "llm.done", Time: time.Now(), Data: json.RawMessage(`{"tokens_in":100,"tokens_out":20,"tokens_cached":10,"cost_usd":0.01,"duration_ms":500,"provider":"anthropic","model":"claude-test"}`)},
		{ID: "wait-tool", AgentID: 992, ThreadID: "main", Type: "tool.call", Time: time.Now(), Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rec := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes/rt-wait/agents/main/wait", sdk.RuntimeAgentWaitRequest{IdleSeconds: 1, PostToolIdleSeconds: 1, TimeoutSeconds: 5, MaxTurns: 5})
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status=%d body=%s", rec.Code, rec.Body.String())
	}
	var execution sdk.RuntimeAgentExecution
	if err := json.Unmarshal(rec.Body.Bytes(), &execution); err != nil {
		t.Fatal(err)
	}
	if execution.Status != "completed" || execution.Reason != "idle" || execution.Turns != 1 || execution.Metrics.TokensIn != 100 || execution.Metrics.ToolCalls != 1 {
		t.Fatalf("execution=%#v", execution)
	}
	foundTool := false
	for _, event := range execution.Trace {
		if event.ToolCall != nil && event.ToolCall.Name == "crm.contacts_create" && strings.Contains(event.ToolCall.Output, "contact-1") {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("normalized tool trace missing: %#v", execution.Trace)
	}
}

func TestRuntimeAPI_RealtimeLifecycleIsRuntimeScoped(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "voice-runtime", sdk.PermRuntimesManage, sdk.PermRuntimesCall)
	created := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-voice"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/threads/voice" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "id": "voice", "audio_token": "one-use"})
		case r.URL.Path == "/threads/voice/audio-token" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "renewed", "id": "voice", "audio_token": "renewed"})
		case r.URL.Path == "/threads/voice" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	port, err := strconv.Atoi(strings.TrimPrefix(core.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := s.environments.Get("rt-voice")
	if err := runtime.AttachAgent(&EnvironmentAgent{AgentID: 993, Alias: "main", Port: port, APIKey: "core-key", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	spawned := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes/rt-voice/agents/main/realtime", sdk.RuntimeRealtimeSpawnRequest{ThreadID: "voice", Directive: "Answer the caller"})
	if spawned.Code != http.StatusOK || !strings.Contains(spawned.Body.String(), `"audio_bridge_url":"ws://`) {
		t.Fatalf("spawn status=%d body=%s", spawned.Code, spawned.Body.String())
	}
	renewed := runtimeAPIRequest(t, s, installID, http.MethodPost, "/apps/callback/runtimes/rt-voice/agents/main/realtime/voice/audio-token", nil)
	if renewed.Code != http.StatusOK || !strings.Contains(renewed.Body.String(), `"status":"renewed"`) {
		t.Fatalf("renew status=%d body=%s", renewed.Code, renewed.Body.String())
	}
	stopped := runtimeAPIRequest(t, s, installID, http.MethodDelete, "/apps/callback/runtimes/rt-voice/agents/main/realtime/voice", nil)
	if stopped.Code != http.StatusNoContent {
		t.Fatalf("stop status=%d body=%s", stopped.Code, stopped.Body.String())
	}
}

func TestRuntimeProviderPoolPinsProviderAndModel(t *testing.T) {
	pool := []ProviderInfo{
		{Type: "anthropic", ModelLarge: "old"},
		{Type: "openai", ModelLarge: "gpt"},
		{Type: "openai-realtime", ModelLarge: "gpt-realtime"},
	}
	selected, provider, model, err := runtimeProviderPool(pool, "anthropic", "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || provider != "anthropic" || model != "claude-test" || selected[0].ModelSmall != "claude-test" || selected[1].Type != "openai-realtime" {
		t.Fatalf("selected=%#v provider=%q model=%q", selected, provider, model)
	}
	if _, _, _, err := runtimeProviderPool(pool, "missing", ""); err == nil {
		t.Fatal("missing provider accepted")
	}
}

func TestRuntimeManager_ExpiresLease(t *testing.T) {
	manager := NewEnvironmentManager(filepath.Join(t.TempDir(), "runtime"))
	runtime, err := manager.Create(EnvironmentSpec{ID: "rt-expiring", RuntimeOwnerInstallID: 42, RuntimeExpiresAt: time.Now().Add(40 * time.Millisecond), NetworkMode: EdgeBlock})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.OwnerInstallID() != 42 {
		t.Fatalf("owner=%d", runtime.OwnerInstallID())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := manager.Get("rt-expiring"); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime did not expire")
}

func TestRuntimeAPI_DirectiveUpdateUsesETagAndAudits(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "eval-directive", sdk.PermAgentsDirectiveWrite)
	agent, err := s.store.CreateAgent(1, "Target", "old directive", "autonomous", `{}`, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	path := "/apps/callback/runtimes/catalog/agents/" + itoa(agent.ID) + "/directive"
	req := sdk.AgentDirectiveUpdateRequest{Directive: "new directive", ExpectedETag: directiveETag("old directive"), Reason: "accepted suggestion"}
	updated := runtimeAPIRequest(t, s, installID, http.MethodPut, path, req)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	got, _ := s.store.GetAgentByID(agent.ID)
	if got.Directive != "new directive" || !strings.Contains(got.Config, "new directive") {
		t.Fatalf("directive/config not updated: %+v", got)
	}
	var auditCount int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM agent_change_history WHERE agent_id=? AND source_app_install_id=?`, agent.ID, installID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
	stale := runtimeAPIRequest(t, s, installID, http.MethodPut, path, req)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale update status=%d body=%s", stale.Code, stale.Body.String())
	}
}
