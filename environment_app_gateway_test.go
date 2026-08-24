package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
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

func TestEnvironmentAppPublicGatewayAndRuntimeIdentity(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	installID := seedRuntimeAPIInstall(t, s, "telephony", sdk.PermRuntimesCall)
	token, err := s.appInstallToken(installID)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "missing brokered token", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"path": r.URL.Path})
	}))
	defer sidecar.Close()

	runtime := &Environment{
		ID: "rt-public", ownerInstallID: installID,
		installs: map[string]*localInstall{"telephony": {
			InstallID: installID, AppName: "telephony", SidecarURL: sidecar.URL,
		}},
		agents: map[int64]*EnvironmentAgent{}, agentAliases: map[string]int64{},
		createdAt: time.Now(),
	}
	s.environments.mu.Lock()
	s.environments.environments[runtime.ID] = runtime
	s.environments.mu.Unlock()

	endpoint, err := s.runtimeAppEndpoint(runtime, "telephony")
	if err != nil {
		t.Fatal(err)
	}
	wantBase := "https://runtime.apteva.invalid/rt-public"
	wantGateway := "http://127.0.0.1:5280/api/environment-app-public/rt-public"
	if endpoint.PlatformURL != wantBase || endpoint.GatewayURL != wantGateway || !strings.HasSuffix(endpoint.AppURL, "/api/apps/telephony/_install/"+itoa(installID)) {
		t.Fatalf("endpoint=%+v", endpoint)
	}

	req := httptest.NewRequest(http.MethodPost, "/environment-app-public/rt-public/api/apps/telephony/_install/"+itoa(installID)+"/inbound/twilio/route", strings.NewReader("CallSid=CA1"))
	req.RemoteAddr = "127.0.0.1:4567"
	rec := httptest.NewRecorder()
	s.handleEnvironmentAppPublicGateway(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"path":"/inbound/twilio/route"`) {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}

	whoami := httptest.NewRequest(http.MethodGet, "/apps/callback/whoami", nil)
	whoami.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	whoRec := httptest.NewRecorder()
	s.handleCallbackWhoami(whoRec, whoami)
	if whoRec.Code != http.StatusOK || !strings.Contains(whoRec.Body.String(), `"public_url":"`+wantBase+`"`) {
		t.Fatalf("whoami status=%d body=%s", whoRec.Code, whoRec.Body.String())
	}
}

func TestCallAppMCPToolAsAgentForwardsRuntimeCaller(t *testing.T) {
	var caller string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = r.Header.Get("X-Apteva-Caller-Agent")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`)
	}))
	defer server.Close()
	if _, err := callAppMCPToolAsAgent(server.URL, "", "42", "test", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if caller != "42" {
		t.Fatalf("caller header=%q", caller)
	}
}

// The gateway's optional /agent-<id>/ segment attributes the calling
// core: the sidecar must receive a server-set X-Apteva-Caller-Agent
// (spoofed inbound values scrubbed), and the plain 2-segment form must
// stay caller-less. This is what makes caller-aware apps (a2a) work
// inside environments.
func TestEnvironmentAppGateway_AgentSegmentAttributesCaller(t *testing.T) {
	s := newTestServer(t)
	s.environments = NewEnvironmentManager(t.TempDir())
	s.environments.server = s

	type seen struct {
		auth, caller, thread, role, toolCall, project, deprecatedProfile, body string
	}
	var got seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{
			auth:              r.Header.Get("Authorization"),
			caller:            r.Header.Get("X-Apteva-Caller-Agent"),
			thread:            r.Header.Get("X-Apteva-Caller-Thread"),
			role:              r.Header.Get("X-Apteva-Caller-Thread-Role"),
			toolCall:          r.Header.Get("X-Apteva-Tool-Call-ID"),
			project:           r.Header.Get("X-Apteva-Project-ID"),
			deprecatedProfile: r.Header.Get("X-Apteva-MCP-Profile"),
		}
		payload, _ := io.ReadAll(r.Body)
		got.body = string(payload)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	res, err := s.store.db.Exec(`INSERT INTO apps (name, source, repo, ref, manifest_json) VALUES ('a2a','git','','','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	appID, _ := res.LastInsertId()
	res, err = s.store.db.Exec(`INSERT INTO app_installs (app_id, project_id, status, installed_by) VALUES (?, 'env-attr', 'running', 1)`, appID)
	if err != nil {
		t.Fatal(err)
	}
	installID, _ := res.LastInsertId()

	environment := &Environment{
		ID: "env-attr",
		installs: map[string]*localInstall{
			"a2a": {InstallID: installID, SidecarURL: backend.URL},
		},
	}
	s.environments.environments["env-attr"] = environment

	do := func(path, spoofCaller, body string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:54321" // the gateway is loopback-only
		r.Header.Set("X-Apteva-MCP-Profile", "conversation")
		if spoofCaller != "" {
			r.Header.Set("X-Apteva-Caller-Agent", spoofCaller)
			r.Header.Set("X-Apteva-Caller-Thread", "spoof-thread")
		}
		w := httptest.NewRecorder()
		s.handleEnvironmentAppGateway(w, r)
		return w.Code
	}

	// Attributed form: header set from URL, spoof scrubbed.
	if code := do("/environment-app-gateway/env-attr/agent-42/a2a/mcp", "999", `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"reply","arguments":{"_apteva_caller_thread":"chat-room-7","_apteva_tool_call_id":"call-7"}}
	}`); code != 200 {
		t.Fatalf("attributed call status %d", code)
	}
	if got.caller != "42" || got.project != "env-attr" || got.thread != "chat-room-7" || got.role != "conversation" || got.toolCall != "call-7" || got.deprecatedProfile != "" {
		t.Fatalf("attributed headers = %+v", got)
	}
	if strings.Contains(got.body, "_apteva_caller") || strings.Contains(got.body, "_apteva_tool_call_id") {
		t.Fatalf("hidden metadata leaked to runtime sidecar: %s", got.body)
	}
	if got.auth == "" {
		t.Fatal("install token not injected")
	}

	// Plain form: caller-less, spoof still scrubbed.
	if code := do("/environment-app-gateway/env-attr/a2a/mcp", "999", `{}`); code != 200 {
		t.Fatalf("plain call status %d", code)
	}
	if got.caller != "" || got.thread != "" {
		t.Fatalf("plain form leaked caller headers: %+v", got)
	}

	// Malformed agent segment is rejected.
	if code := do("/environment-app-gateway/env-attr/agent-x/a2a/mcp", "", `{}`); code != http.StatusBadRequest {
		t.Fatalf("malformed agent segment status %d, want 400", code)
	}
}
