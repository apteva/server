package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestInstallMCPCompletionResumesOnlyOriginatingThread(t *testing.T) {
	s, id := pageToolsFixture(t)
	s.appBus = NewAppEventBus()
	agent, err := s.store.CreateAgent(1, "origin caller", "", "autonomous", "{}", "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.BindAgentThreadScope(agent.ID, "origin-thread", "p", 91); err != nil {
		t.Fatal(err)
	}
	var deliveries atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("untrusted delivery credential")
		}
		if r.URL.Path == "/threads" {
			writeJSON(w, []map[string]string{{"id": "origin-thread"}})
			return
		}
		var event map[string]string
		if r.URL.Path != "/event" || json.NewDecoder(r.Body).Decode(&event) != nil || event["thread_id"] != "origin-thread" || event["event_id"] == "" {
			t.Error("completion was redirected or lacked deduplication ID")
		}
		deliveries.Add(1)
		w.WriteHeader(200)
	}))
	defer core.Close()
	parsed, _ := url.Parse(core.URL)
	_, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "fixture-key", reattached: true}
	s.agents.mu.Unlock()
	op := seedInstallOperation(t, s, id, "running")
	req := s.inspectPlatformInstallWait(agent, "origin-thread", "p", []byte(`{"method":"tools/call","params":{"name":"apps_install"}}`))
	if _, err = s.augmentPlatformInstallWait(req, installRPCReceipt(appReadiness{InstallID: id, Name: "metrics", ProjectID: "p", OperationID: op.ID, State: "prepare"})); err != nil {
		t.Fatal(err)
	}
	op.Status = "succeeded"
	if err = s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewAppEventDispatcher(s)
	dispatcher.drainOutbox(context.Background())
	dispatcher.drainOutbox(context.Background())
	if deliveries.Load() != 1 {
		t.Fatalf("completion deliveries: %d", deliveries.Load())
	}
	var subscriptions int
	s.store.db.QueryRow(`SELECT count(*) FROM subscriptions WHERE kind='ephemeral'`).Scan(&subscriptions)
	if subscriptions != 0 {
		t.Fatal("terminal wait was not closed")
	}
}

func seedInstallOperation(t *testing.T, s *Server, id int64, status string) runtimeOperation {
	t.Helper()
	m := s.appRuntimeManager()
	op := runtimeOperation{ID: generateID(), InstallID: id, Kind: "deploy", Status: status, Phase: "prepare", Owner: m.owner, IdempotencyKey: generateID(), CreatedAt: runtimeTimestamp(), UpdatedAt: runtimeTimestamp(), DeploymentID: "candidate"}
	if _, err := s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,idempotency_key,fingerprint,record_json) VALUES(?,?,?,?,?,?,?,?)`, op.ID, id, op.Kind, status, m.owner, op.IdempotencyKey, "test", runtimeJSON(op)); err != nil {
		t.Fatal(err)
	}
	return op
}

func installRPCReceipt(state appReadiness) []byte {
	raw, _ := json.Marshal(state)
	rpc, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}}})
	return rpc
}

func TestInstallMCPReadinessAndScope(t *testing.T) {
	s, id := pageToolsFixture(t)
	got, err := s.installReadiness(id)
	if err != nil || !got.Ready || !got.UIAvailable || len(got.Widgets) != 1 || got.Widgets[0] != "metrics:metric" {
		t.Fatalf("readiness: %+v %v", got, err)
	}
	op := seedInstallOperation(t, s, id, "running")
	got, err = s.installReadiness(id)
	if err != nil || got.Ready || got.OperationID != op.ID || got.ToolsAvailable {
		t.Fatalf("old serving app made candidate ready: %+v %v", got, err)
	}
	op.Status = "succeeded"
	op.Phase = "complete"
	if err = s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatal(err)
	}
	got, err = s.installReadiness(id)
	if err != nil || got.Ready {
		t.Fatal("unpublished candidate marked ready")
	}
	s.store.db.Exec(`UPDATE app_installs SET runtime_active_deployment=? WHERE id=?`, op.DeploymentID, id)
	got, err = s.installReadiness(id)
	if err != nil || !got.Ready {
		t.Fatalf("published candidate not ready: %+v %v", got, err)
	}
	user, err := s.store.CreateUser("install-status@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/apps/readiness?install_id="+itoa(id), nil)
	r.Header.Set("X-User-ID", itoa(user.ID))
	w := httptest.NewRecorder()
	s.handleAppReadiness(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-project readiness HTTP %d", w.Code)
	}
	manifest := s.installedApps.Get(id).Manifest
	manifest.ConfigSchema = []sdk.ConfigField{{Name: "secret", Required: true, Type: "password"}}
	s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(manifest), id)
	got, err = s.installReadiness(id)
	if err != nil || got.Ready || got.State != "needs_configuration" || len(got.MissingSetup) != 1 || got.MissingSetup[0] != "config:secret" {
		t.Fatalf("missing setup: %+v %v", got, err)
	}
	op.Status = "failed"
	op.Error = "build failed"
	if err = s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatal(err)
	}
	got, err = s.installReadiness(id)
	if err != nil || got.Error != "build failed" || got.Ready {
		t.Fatalf("failure hidden: %+v %v", got, err)
	}
}

func TestInstallMCPAsyncReplayDedupAndRestart(t *testing.T) {
	s, id := pageToolsFixture(t)
	helper, err := s.store.CreateAgent(1, "waiting Helper", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	helper.Kind = "platform_helper"
	if _, err = s.store.BindAgentThreadScope(helper.ID, "install-thread", "p", 91); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"method":"tools/call","params":{"name":"apps_install","arguments":{}}}`)
	req := s.inspectPlatformInstallWait(helper, "install-thread", "p", body)
	op := seedInstallOperation(t, s, id, "running")
	receipt := appReadiness{InstallID: id, Name: "metrics", ProjectID: "p", OperationID: op.ID, State: "prepare"}
	// Completion precedes the install response and subscription registration.
	op.Status = "succeeded"
	if err = s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatal(err)
	}
	raw, err := s.augmentPlatformInstallWait(req, installRPCReceipt(receipt))
	if err != nil || !bytes.Contains(raw, []byte(`will_notify\":true`)) {
		t.Fatalf("no async receipt: %s %v", raw, err)
	}
	again := s.inspectPlatformInstallWait(helper, "install-thread", "p", body)
	if _, err = s.augmentPlatformInstallWait(again, installRPCReceipt(receipt)); err != nil {
		t.Fatal(err)
	}
	var count int
	s.store.db.QueryRow(`SELECT count(*) FROM subscriptions WHERE kind='ephemeral'`).Scan(&count)
	if count != 1 {
		t.Fatalf("retry made %d subscriptions", count)
	}
	s.store.db.QueryRow(`SELECT count(*) FROM app_subscription_outbox WHERE status='pending'`).Scan(&count)
	if count != 1 {
		t.Fatalf("fast completion missing or duplicated: %d", count)
	}
	// Durable notifications survive reopening the database, not just a bus replay.
	path := s.store.path
	s.store.Close()
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.store = reopened
	t.Cleanup(func() { reopened.Close() })
	s.store.db.QueryRow(`SELECT count(*) FROM app_subscription_outbox WHERE status='pending'`).Scan(&count)
	if count != 1 {
		t.Fatal("pending completion lost on restart")
	}
	rows, err := s.store.ListAllAppEventSubscriptions()
	if err != nil || len(rows) != 1 || rows[0].ThreadID != "install-thread" || rows[0].AgentID != helper.ID || rows[0].SourceInstallID != id || rows[0].ExpiresAt == "" {
		t.Fatalf("wrong caller target: %+v %v", rows, err)
	}
}

func TestInstallMCPDurableOperationRollbackAndGlobalWait(t *testing.T) {
	s, id := pageToolsFixture(t)
	s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, id)
	agent, err := s.store.CreateAgent(1, "project caller", "", "autonomous", "{}", "p")
	if err != nil {
		t.Fatal(err)
	}
	op := seedInstallOperation(t, s, id, "running")
	req := s.inspectPlatformInstallWait(agent, "main", "p", []byte(`{"method":"tools/call","params":{"name":"apps_install"}}`))
	receipt := appReadiness{InstallID: id, Name: "metrics", OperationID: op.ID, State: "prepare"}
	if raw, err := s.augmentPlatformInstallWait(req, installRPCReceipt(receipt)); err != nil || !bytes.Contains(raw, []byte(`will_notify\":true`)) {
		t.Fatalf("global wait: %s %v", raw, err)
	}
	s.store.db.Exec(`CREATE TRIGGER fail_install_event BEFORE INSERT ON app_async_event_journal BEGIN SELECT RAISE(ABORT,'forced sink failure'); END`)
	op.Status = "failed"
	op.Error = "compile failed"
	if err = s.appRuntimeManager().saveOperation(op); err == nil {
		t.Fatal("notification failure did not roll back operation state")
	}
	saved, _ := s.appRuntimeManager().operation(op.ID)
	if saved.Status != "running" {
		t.Fatal("operation committed without durable completion")
	}
	s.store.db.Exec(`DROP TRIGGER fail_install_event`)
	if err = s.appRuntimeManager().saveOperation(op); err != nil {
		t.Fatal(err)
	}
	var n int
	s.store.db.QueryRow(`SELECT count(*) FROM app_subscription_outbox WHERE status='pending'`).Scan(&n)
	if n != 1 {
		t.Fatalf("project-scoped global completion: %d", n)
	}
}

func TestInstallMCPManagedStaticReuseAndConflicts(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	seedPresetProject(t, s, "p")
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("fixture"), 0600)
	manifest := "schema: apteva-app/v1\nname: readiness-static\nversion: 1.0.0\nscopes: [project]\nruntime:\n  kind: static\n  static_dir: " + dir + "\n"
	call := func(cfg map[string]any) appReadiness {
		t.Helper()
		body := map[string]any{"manifest_yaml": manifest, "project_id": "p", "reuse_existing": true}
		for k, v := range cfg {
			body[k] = v
		}
		w := httptest.NewRecorder()
		s.handleInstallApp(w, authedRequest(t, "POST", "/apps/install", "", body))
		if w.Code != 200 {
			t.Fatalf("install HTTP %d: %s", w.Code, w.Body.String())
		}
		var out appReadiness
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := call(nil)
	if first.OperationID == "" {
		t.Fatal("MCP install has no operation")
	}
	again := call(nil)
	if again.InstallID != first.InstallID || again.OperationID != first.OperationID || !again.Reused {
		t.Fatal("retry duplicated install or operation")
	}
	op, _ := s.appRuntimeManager().operation(first.OperationID)
	done := waitRuntimeOperation(t, s.appRuntimeManager(), op)
	if done.Status != "succeeded" {
		t.Fatalf("static deploy failed: %+v", done)
	}
	ready := call(nil)
	if !ready.Ready {
		t.Fatalf("static install not ready: %+v", ready)
	}
	other := httptest.NewRecorder()
	s.handleInstallApp(other, authedRequest(t, "POST", "/apps/install", "", map[string]any{"manifest_yaml": manifest, "project_id": "p", "reuse_existing": true, "config": map[string]string{"different": "value"}}))
	if other.Code != 409 {
		t.Fatalf("conflicting config accepted: %d %s", other.Code, other.Body.String())
	}
	var count int
	s.store.db.QueryRow(`SELECT count(*) FROM app_installs`).Scan(&count)
	if count != 1 {
		t.Fatal("conflicting retry created another installation")
	}
}

func TestInstallMCPReceiptBeforeOperationResumesAfterRestart(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("recovered"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := sdk.Manifest{Name: "recover-mcp-install", Version: "1.0.0", Runtime: sdk.Runtime{Kind: "static", StaticDir: dir}}
	id := seedRunningInstall(t, s, manifest.Name, "", manifest, nil)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET status='pending',runtime_managed=1,status_message=?,runtime_placement_json='{"type":"local"}' WHERE id=?`, mcpInstallQueued, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO app_runtime_install_receipts(user_id,request_key,fingerprint,install_id) VALUES(1,'recover-key','fingerprint',?)`, id); err != nil {
		t.Fatal(err)
	}
	// Model a crash after the row and receipt commit, before begin() runs.
	path := s.store.path
	s.store.Close()
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.store = reopened
	t.Cleanup(func() { reopened.Close() })
	m := s.appRuntimeManager()
	m.resumeMCPInstallIntents()
	var opID string
	if err = s.store.db.QueryRow(`SELECT id FROM app_runtime_operations WHERE install_id=?`, id).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, err := m.operation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatalf("receipt recovery failed: %+v", done)
	}
	m.resumeMCPInstallIntents()
	var count int
	s.store.db.QueryRow(`SELECT count(*) FROM app_runtime_operations WHERE install_id=?`, id).Scan(&count)
	if count != 1 {
		t.Fatal("recovery queued duplicate work")
	}
}

func TestInstallMCPMarketplaceFallbackIsBoundedAndMarked(t *testing.T) {
	s := newTestServer(t)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"apps":[{"name":"todo","display_name":"Todo","description":"Personal lists","category":"productivity"},{"name":"tasks","display_name":"Tasks","description":"Agent scheduling","category":"business"}]}`))
	}))
	defer registry.Close()
	query := func(q, category string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleMarketplace(w, authedRequest(t, "GET", "/apps/marketplace?registry_url="+url.QueryEscape(registry.URL)+"&q="+url.QueryEscape(q)+"&category="+category, "", nil))
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("marketplace: %s", w.Body.String())
		}
		return out
	}
	result := query("todo tasks", "")
	if result["match_type"] != "suggestions" || len(result["apps"].([]any)) != 0 || len(result["suggestions"].([]any)) != 2 {
		t.Fatalf("fallback: %+v", result)
	}
	filtered := query("todo tasks", "productivity")
	if len(filtered["suggestions"].([]any)) != 1 {
		t.Fatal("suggestions escaped category")
	}
	exact := query("todo", "")
	if len(exact["apps"].([]any)) != 1 || exact["suggestions"] != nil {
		t.Fatal("exact result replaced with suggestions")
	}
	missing := query("completely-unrelated", "")
	if len(missing["apps"].([]any)) != 0 || missing["suggestions"] != nil {
		t.Fatal("irrelevant suggestions")
	}
	widgets := marketplaceWidgets(&sdk.Manifest{Name: "generic", Provides: sdk.Provides{UIComponents: []sdk.UIComponent{{Name: "board", Label: "Board", Entry: "/board.mjs", Slots: []string{"dashboard.home"}}, {Name: "card", Entry: "/card.mjs", Slots: []string{"chat.message_attachment"}}}}})
	compact := compactGatewayMarketplaceApp(map[string]any{"name": "generic", "installed": true, "surfaces": map[string]any{"widgets": widgets}})
	if len(widgets) != 1 || widgets[0].Component != "generic:board" || compact["widgets"] == nil {
		t.Fatal("compact marketplace lost embeddable widget metadata")
	}
}
