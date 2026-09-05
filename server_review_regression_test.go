package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditLegacyProxyCredential(t *testing.T) {
	s := newTestServer(t)
	id := seedSecurityAppInstall(t, s)
	r := httptest.NewRequest("POST", "/apps/callback/whoami", nil)
	r.RemoteAddr = "127.0.0.1:4567"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("Authorization", "Bearer dev-"+itoa(id))
	w := httptest.NewRecorder()
	s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(w, r)
	if w.Code != 401 {
		t.Fatalf("external request forwarded over loopback authenticated with guessed credential: status=%d", w.Code)
	}
}

func TestAuditCrossOriginMutation(t *testing.T) {
	s := newTestServer(t)
	token := adminSession(t, s)
	r := httptest.NewRequest("POST", "/auth/keys", strings.NewReader(`{"name":"cross-origin-created"}`))
	r.Header.Set("Origin", "https://untrusted.example")
	r.Header.Set("Content-Type", "text/plain")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w := httptest.NewRecorder()
	newCORSConfig("https://trusted.example").middleware(s.authMiddleware(s.handleCreateKey)).ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatalf("cross-origin simple POST executed with session cookie: status=%d", w.Code)
	}
}

func auditCore(t *testing.T, s *Server, h http.HandlerFunc) (*Agent, *httptest.Server) {
	t.Helper()
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "audit", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	_, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	s.agents.processes[a.ID] = &runningAgent{port: port, reattached: true}
	return a, ts
}

func TestAuditProxyQuery(t *testing.T) {
	s := newTestServer(t)
	a, _ := auditCore(t, s, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.URL.RawQuery) })
	r := httptest.NewRequest("GET", "/instances/"+itoa(a.ID)+"/events?after=25&limit=1", nil)
	w := httptest.NewRecorder()
	s.handleProxy(w, r)
	if w.Body.String() != "after=25&limit=1" {
		t.Fatalf("query lost: upstream received %q", w.Body.String())
	}
}

func TestAuditProxyDuplicatesMutation(t *testing.T) {
	s := newTestServer(t)
	var calls atomic.Int32
	a, ts := auditCore(t, s, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(200)
	})
	resp, err := s.coreDoWithBootWait(a.ID, "POST", ts.URL+"/event", []byte(`{"message":"once"}`), "")
	if resp != nil {
		resp.Body.Close()
	}
	if calls.Load() != 1 {
		t.Fatalf("mutation executed %d times after lost response (err=%v)", calls.Load(), err)
	}
}

func TestAuditHostFallback(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.installedApps.Add(&InstalledApp{InstallID: 2, AppName: "storage", ProjectID: "project-b", SidecarURL: "http://127.0.0.1:9999"})
	target, ok := NewHostRouter(s, http.NotFoundHandler()).resolveTarget(RouteHit{OriginApp: "storage", OriginProject: "project-a"})
	if ok {
		t.Fatalf("missing project A backend falls through to project B: %v", target)
	}
}

func TestAuditIngressOperatorOwnership(t *testing.T) {
	s := newTestServer(t)
	id := seedSecurityAppInstall(t, s)
	_, err := s.ExposeIngressRoute(IngressExposeRequest{Hostname: "operator.example", Target: "http://127.0.0.1:8000", OwnerKind: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ExposeIngressRoute(IngressExposeRequest{Hostname: "operator.example", Target: "http://127.0.0.1:9000", OwnerInstallID: id})
	if err == nil {
		t.Fatal("app install took ownership of operator route")
	}
}

func TestAuditHostPrincipalHeaders(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("X-Apteva-Bound-Caller-Install-ID"))
	}))
	defer ts.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 7, AppName: "audit-app", SidecarURL: ts.URL, Token: "test-app-token"})
	target, _ := url.Parse("app://audit-app?ingress_auth=app_token")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://app.example/mcp", nil)
	r.Header.Set("X-Apteva-Bound-Caller-Install-ID", "123")
	NewHostRouter(s, http.NotFoundHandler()).serveRoute(w, r, RouteHit{Hostname: "app.example", OriginApp: "audit-app", OriginAppTokenAuth: true, OwnerInstallID: 7, Target: target})
	if w.Body.String() != "" {
		t.Fatalf("network client forged server-owned app caller identity: %q", w.Body.String())
	}
}

func TestAuditRestoreOuterLimit(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/platform/restore", strings.NewReader("x"))
	r.ContentLength = 17 << 20
	w := httptest.NewRecorder()
	limitAPIRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("17 MiB restore rejected by outer middleware before 64 GiB restore handler: status=%d", w.Code)
	}
}

func TestAuditSecretCorruption(t *testing.T) {
	t.Setenv("SERVER_SECRET", "")
	dir := t.TempDir()
	p := filepath.Join(dir, ".secret")
	old := []byte("invalid-existing-secret")
	if err := os.WriteFile(p, old, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSecret(dir)
	after, _ := os.ReadFile(p)
	if err == nil || !bytes.Equal(old, after) {
		t.Fatal("existing malformed encryption key silently replaced")
	}
}

func TestAuditEdgeMetadataBound(t *testing.T) {
	c := NewEdgeCache()
	r := httptest.NewRequest("GET", "http://assets.example/a", nil)
	k := edgeKey("assets.example", r.URL.RequestURI())
	for i := 0; i < 1000; i++ {
		c.store(k, &cacheEntry{body: []byte("x"), size: 1, expires: time.Now().Add(-time.Second)})
		c.serve(httptest.NewRecorder(), r, "assets.example")
	}
	if len(c.order) > 1 {
		t.Fatalf("expired objects removed but eviction index retains %d entries; cached bytes=%d", len(c.order), c.curBytes)
	}
}

func TestAuditScopeDestinationAuthorization(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	s.localApps = NewLocalSupervisor(t.TempDir())
	seedProject(t, s, "source-project")
	u, err := s.store.CreateUser("editor@test.example", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec(`INSERT INTO project_members(project_id,user_id,role,added_by) VALUES(?,?,'editor',1)`, "source-project", u.ID); err != nil {
		t.Fatal(err)
	}
	id := seedInstall(t, s, "scope-audit", "source-project")
	if _, err = s.store.db.Exec(`UPDATE apps SET manifest_json='{"name":"scope-audit","scopes":["project","global"]}' WHERE name='scope-audit'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec(`UPDATE app_installs SET status='pending' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PATCH", "/apps/installs/"+itoa(id)+"/scope", strings.NewReader(`{"project_id":""}`))
	r.Header.Set("X-User-ID", itoa(u.ID))
	w := httptest.NewRecorder()
	if _, ok := s.requireAppInstallAccess(w, r, id, ProjectEditor); ok {
		s.handleSetInstallScope(w, r)
	}
	if w.Code < 400 {
		t.Fatalf("nonadmin source-project editor promoted app to global: status=%d", w.Code)
	}
}

func TestAuditCoreReaperReplacement(t *testing.T) {
	s := newTestServer(t)
	script := filepath.Join(t.TempDir(), "fake-core")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.agents.coreCmd = script
	a := &Agent{ID: 901, UserID: 1, Name: "audit", Mode: "autonomous", Config: `{"include_channels":false,"include_apteva_server":false}`}
	if err := s.agents.Start(a, nil, "5280", nil, "test"); err != nil {
		t.Fatal(err)
	}
	s.agents.mu.Lock()
	old := s.agents.processes[a.ID]
	if old.channels != nil {
		defer old.channels.Stop()
	}
	if err := old.process().Kill(); err != nil {
		s.agents.mu.Unlock()
		t.Fatal(err)
	}
	<-old.done
	// Equivalent to Start publishing its replacement while old exit cleanup waits for the lock.
	replacement := &runningAgent{port: 9988, reattached: true}
	s.agents.processes[a.ID] = replacement
	s.agents.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.agents.mu.RLock()
		got := s.agents.processes[a.ID]
		s.agents.mu.RUnlock()
		if got != replacement {
			t.Fatal("old core's exit cleanup deleted newly published replacement")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAuditIntegrationTransportReuse(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) }))
	ts.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			connections.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()
	app := &AppTemplate{Slug: "audit-http", BaseURL: ts.URL}
	tool := &AppToolDef{Name: "read", Method: "GET", Path: "/"}
	for i := 0; i < 5; i++ {
		if _, err := executeIntegrationTool(app, tool, map[string]string{}, map[string]any{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := connections.Load(); n > 1 {
		t.Fatalf("5 sequential integration calls opened %d TCP connections", n)
	}
}

func TestAuditLoopbackMCPThroughProxy(t *testing.T) {
	s := newTestServer(t)
	s.catalog = routeTestCatalog()
	seedMCPRouteTestData(t, s.store, s.secret)
	r := httptest.NewRequest("POST", "/mcp/71", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.RemoteAddr = "127.0.0.1:4567"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	w := httptest.NewRecorder()
	s.handleMCPEndpoint(w, r)
	if w.Code == 200 {
		t.Fatal("unauthenticated external MCP request via loopback proxy accessed integration tools")
	}
}

func TestAuditRestoreFailureKeepsDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "app.db")
	if err := os.WriteFile(dst, []byte("old-database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicReplaceWithBackup(dst, filepath.Join(dir, "missing-staged-file")); err == nil {
		t.Fatal("expected source-read error")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("failed replacement removed active database path: %v", err)
	}
}

func TestAuditAgentUpdateLostState(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "before", "directive-before", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.store.GetAgentByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec(`UPDATE agents SET directive='new-directive',status='running',port=30000,pid=200,core_api_key='new-key' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	stale.Name = "renamed"
	if err = s.store.UpdateAgent(stale); err != nil {
		t.Fatal(err)
	}
	after, err := s.store.GetAgentByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Directive != "new-directive" || after.Status != "running" {
		t.Fatalf("rename with stale snapshot reverted concurrent changes: directive=%q status=%q port=%d", after.Directive, after.Status, after.Port)
	}
}

func TestAuditDeleteAgentAtomicity(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	a, err := s.store.CreateAgent(1, "before", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.InsertTelemetry([]TelemetryEvent{{AgentID: a.ID, Type: "audit", Time: time.Now(), Data: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec(`CREATE TRIGGER audit_reject_delete BEFORE DELETE ON agents BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.store.DeleteAgent(1, a.ID); err == nil {
		t.Fatal("expected fault-injected deletion failure")
	}
	var n int
	if err = s.store.db.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE agent_id=?`, a.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("failed agent deletion committed removal of existing telemetry")
	}
}

func TestAuditMissingWebhookSignature(t *testing.T) {
	s := newTestServer(t)
	var delivered atomic.Int32
	a, _ := auditCore(t, s, func(w http.ResponseWriter, r *http.Request) { delivered.Add(1); w.WriteHeader(200) })
	secret, err := Encrypt(s.secret, "configured-hmac-secret")
	if err != nil {
		t.Fatal(err)
	}
	sub := &Subscription{ID: "audit", UserID: 1, AgentID: a.ID, Slug: "audit", Enabled: true}
	r := httptest.NewRequest("POST", "/webhooks/test-opaque-token", strings.NewReader(`{"event":"unsigned"}`))
	w := httptest.NewRecorder()
	s.handleSubscriptionWebhook(w, r, sub, secret)
	if delivered.Load() != 0 {
		t.Fatalf("HMAC-protected webhook delivered unsigned payload: status=%d", w.Code)
	}
}

func TestAuditUnsignedEmailWebhook(t *testing.T) {
	s := newTestServer(t)
	var delivered atomic.Int32
	gw := NewEmailGateway("unused")
	gw.MapInbox(1, "known-inbox", "inbox@test.example", NewChannelRegistry(), func(string, string) { delivered.Add(1) })
	emailMu.Lock()
	saved := emailGateways
	emailGateways = map[string]*EmailGateway{"test": gw}
	emailMu.Unlock()
	defer func() { emailMu.Lock(); emailGateways = saved; emailMu.Unlock() }()
	r := httptest.NewRequest("POST", "/webhooks/email", strings.NewReader(`{"event_type":"message.received","message":{"inbox_id":"known-inbox","from":"spoofed@example.com","text":"unsigned message"}}`))
	w := httptest.NewRecorder()
	s.handleEmailWebhook(w, r)
	if delivered.Load() != 0 {
		t.Fatalf("unsigned public email webhook invoked agent callback: status=%d", w.Code)
	}
}

func TestAuditEventFailureSkippedByCursor(t *testing.T) {
	s := newTestServer(t)
	var calls atomic.Int32
	a, _ := auditCore(t, s, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	})
	sub, err := s.store.CreateAppEventSubscription(1, a.ID, "audit", "audit:*", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := NewAppEventDispatcher(s)
	lane := &appEventLane{rows: []*Subscription{sub}}
	first := AppEvent{Seq: 1, App: "audit", Topic: "changed", Data: []byte(`{}`)}
	d.dispatch(lane, first)
	d.dispatch(lane, AppEvent{Seq: 2, App: "audit", Topic: "changed", Data: []byte(`{}`)})
	// A later replay of the failed event should still attempt delivery.
	d.dispatch(lane, first)
	if calls.Load() != 3 {
		t.Fatalf("failed event became permanently skipped: calls=%d cursor=%d", calls.Load(), sub.LastSeqDelivered)
	}
}
