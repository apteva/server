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
