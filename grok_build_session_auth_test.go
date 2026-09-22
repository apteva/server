package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withTestGrokBuildEndpoints(t *testing.T, server *httptest.Server) {
	t.Helper()
	oldDeviceCodeURL := integrationGrokBuildDeviceCodeURL
	oldTokenURL := integrationGrokBuildTokenURL
	oldSessionClient := grokBuildSessionHTTPClient
	integrationGrokBuildDeviceCodeURL = server.URL + "/oauth2/device/code"
	integrationGrokBuildTokenURL = server.URL + "/oauth2/token"
	grokBuildSessionHTTPClient = server.Client()
	t.Cleanup(func() {
		integrationGrokBuildDeviceCodeURL = oldDeviceCodeURL
		integrationGrokBuildTokenURL = oldTokenURL
		grokBuildSessionHTTPClient = oldSessionClient
	})
}

func TestGrokBuildDeviceStartUsesRFC8628FormAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/device/code" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != integrationGrokBuildClientID ||
			r.Form.Get("referrer") != "grok-build" ||
			!strings.Contains(r.Form.Get("scope"), "grok-cli:access") ||
			!strings.Contains(r.Form.Get("scope"), "offline_access") {
			t.Fatalf("unexpected device form: %v", r.Form)
		}
		if r.Header.Get("x-grok-client-version") != grokBuildClientVersion || r.Header.Get("x-grok-client-surface") != "ui" {
			t.Fatalf("missing Grok client headers: %v", r.Header)
		}
		writeJSON(w, map[string]any{
			"device_code": "device-secret", "user_code": "ABCD-EFGH",
			"verification_uri":          serverURL(r) + "/activate",
			"verification_uri_complete": serverURL(r) + "/activate?user_code=ABCD-EFGH",
			"expires_in":                600, "interval": 3,
		})
	}))
	defer server.Close()
	withTestGrokBuildEndpoints(t, server)

	auth, err := (grokBuildSessionAuthDriver{}).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth.DeviceCode != "device-secret" || auth.UserCode != "ABCD-EFGH" || auth.Interval != 3 || auth.VerificationURIComplete == "" {
		t.Fatalf("authorization = %+v", auth)
	}
}

func TestGrokBuildDevicePollStates(t *testing.T) {
	tests := []struct {
		name       string
		upstream   map[string]any
		wantStatus string
		wantNext   int
	}{
		{name: "pending", upstream: map[string]any{"error": "authorization_pending"}, wantStatus: "pending", wantNext: 5},
		{name: "slow down", upstream: map[string]any{"error": "slow_down"}, wantStatus: "pending", wantNext: 10},
		{name: "denied", upstream: map[string]any{"error": "access_denied", "error_description": "request rejected"}, wantStatus: "failed"},
		{name: "expired", upstream: map[string]any{"error": "expired_token"}, wantStatus: "expired"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("grant_type") != grokBuildDeviceGrantType || r.Form.Get("device_code") != "device-secret" {
					t.Fatalf("poll form = %v", r.Form)
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(test.upstream)
			}))
			defer server.Close()
			withTestGrokBuildEndpoints(t, server)
			result, err := (grokBuildSessionAuthDriver{}).Poll(context.Background(), &connectionDeviceAuthSession{
				DeviceAuthID: "device-secret", Interval: 5,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.wantStatus || result.NextPollSeconds != test.wantNext {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestGrokBuildDevicePollBuildsSessionCredentials(t *testing.T) {
	accessToken := grokBuildTestJWT(t, map[string]any{
		"sub": "user-42", "principal_type": "User", "principal_id": "principal-42",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	idToken := grokBuildTestJWT(t, map[string]any{"sub": "user-42", "email": "user@example.test"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": accessToken, "refresh_token": "refresh-1", "id_token": idToken, "expires_in": 3600,
		})
	}))
	defer server.Close()
	withTestGrokBuildEndpoints(t, server)

	result, err := (grokBuildSessionAuthDriver{}).Poll(context.Background(), &connectionDeviceAuthSession{DeviceAuthID: "device-1"})
	if err != nil {
		t.Fatal(err)
	}
	creds := result.Credentials
	if result.Status != "connected" || creds["access_token"] != accessToken || creds["refresh_token"] != "refresh-1" ||
		creds["account_id"] != "principal-42" || creds["user_id"] != "user-42" || creds["account_email"] != "user@example.test" ||
		creds["auth_provider"] != integrationGrokBuildSlug || creds["runtime_base_url"] != integrationGrokBuildRuntimeURL {
		t.Fatalf("credentials = %v", filterKeys(creds))
	}
	if expiresAt, err := time.Parse(time.RFC3339, creds["token_expires_at"]); err != nil || time.Until(expiresAt) < 50*time.Minute {
		t.Fatalf("token_expires_at = %q, err=%v", creds["token_expires_at"], err)
	}
}

func TestGrokBuildRefreshPreservesRotatedIdentityAndRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "keep-refresh" ||
			r.Form.Get("principal_type") != "Team" || r.Form.Get("principal_id") != "team-1" {
			t.Fatalf("refresh form = %v", r.Form)
		}
		writeJSON(w, map[string]any{"access_token": "new-access", "expires_in": 7200})
	}))
	defer server.Close()
	withTestGrokBuildEndpoints(t, server)

	credentials := map[string]string{
		"access_token": "old-access", "refresh_token": "keep-refresh", "principal_type": "Team",
		"principal_id": "team-1", "account_id": "team-1", "user_id": "team-1", "account_email": "old@example.test",
	}
	if err := (grokBuildSessionAuthDriver{}).Refresh(context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	if credentials["access_token"] != "new-access" || credentials["refresh_token"] != "keep-refresh" ||
		credentials["principal_id"] != "team-1" || credentials["account_email"] != "old@example.test" {
		t.Fatalf("credentials = %v", filterKeys(credentials))
	}
}

func TestGrokBuildTeamCredentialsUsePrincipalAsUserID(t *testing.T) {
	accessToken := grokBuildTestJWT(t, map[string]any{
		"principal_type": "Team", "principal_id": "team-42", "exp": time.Now().Add(time.Hour).Unix(),
	})
	credentials := buildConnectionGrokBuildCredentials(map[string]any{
		"access_token": accessToken, "refresh_token": "refresh-team", "expires_in": 3600,
	})
	if credentials["user_id"] != "team-42" || credentials["account_id"] != "team-42" {
		t.Fatalf("team credentials = %v", filterKeys(credentials))
	}
}

func TestGrokBuildModelCatalogSendsSessionHeadersAndParsesModels(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		for header, want := range map[string]string{
			"Authorization": "Bearer token-1", "X-XAI-Token-Auth": "xai-grok-cli",
			"x-grok-client-version": grokBuildClientVersion, "x-grok-client-identifier": "apteva-server",
			"x-grok-client-mode": "headless", "x-authenticateresponse": "authenticate-response",
			"x-userid": "user-1", "x-email": "user@example.test",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		w.Header().Set("ETag", `"grok-models-1"`)
		writeJSON(w, map[string]any{"data": []map[string]any{
			{"model": "grok-4.6", "name": "Grok 4.6", "context_window": 500000, "apiBackend": "responses", "inputModalities": []string{"text", "image"}},
			{"id": "grok-4.5", "contextWindow": 500000, "reasoningEfforts": []string{"low", "high"}},
			{"model": "internal", "hidden": true, "context_window": 10},
		}})
	}))
	defer server.Close()
	oldBaseURL := grokBuildModelCatalogBaseURL
	oldClient := grokBuildModelCatalogClient
	grokBuildModelCatalogBaseURL = server.URL
	grokBuildModelCatalogClient = server.Client()
	globalGrokBuildCatalogCache = &grokBuildCatalogCacheStore{entries: map[string]grokBuildCatalogCacheEntry{}}
	t.Cleanup(func() {
		grokBuildModelCatalogBaseURL = oldBaseURL
		grokBuildModelCatalogClient = oldClient
		globalGrokBuildCatalogCache = &grokBuildCatalogCacheStore{entries: map[string]grokBuildCatalogCacheEntry{}}
	})
	credentials := map[string]string{
		"access_token": "token-1", "user_id": "user-1", "account_email": "user@example.test",
	}
	models, err := fetchGrokBuildModelCatalog(context.Background(), credentials, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "grok-4.6" || models[0].ContextSize != 500000 ||
		models[1].ID != "grok-4.5" || len(models[1].Capabilities.SupportedReasoningLevels) != 2 {
		t.Fatalf("models = %#v", models)
	}
	state := map[string]any{"model_small": "grok-4.5"}
	applyGrokBuildCatalogToState(state, models)
	if state["model_large"] != "grok-4.6" || state["model_medium"] != "grok-4.6" || state["model_small"] != "grok-4.5" {
		t.Fatalf("hydrated model tiers = %#v", state)
	}
	capabilities, ok := state["model_capabilities"].(map[string]ProviderModelCapabilities)
	if !ok || capabilities["grok-4.6"].ContextWindow != 500000 || len(capabilities["grok-4.5"].SupportedReasoningLevels) != 2 {
		t.Fatalf("hydrated capabilities = %#v", state["model_capabilities"])
	}
	if _, err := fetchGrokBuildModelCatalog(context.Background(), credentials, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("catalog calls = %d, want one cached request", calls)
	}
}

func TestGrokBuildRuntimeTokenAndProviderPoolAreEnabled(t *testing.T) {
	s := runtimeTestServer(t)
	registerRuntimeApp(s, integrationGrokBuildSlug, integrationGrokBuildSlug, map[string]string{
		"GROK_BUILD_ACCESS_TOKEN":   "{{credentials.access_token}}",
		"GROK_BUILD_PROVIDER_ID":    "{{connection.provider_ref}}",
		"GROK_BUILD_USER_ID":        "{{credentials.user_id}}",
		"GROK_BUILD_ACCOUNT_EMAIL":  "{{credentials.account_email}}",
		"GROK_BUILD_PRINCIPAL_TYPE": "{{credentials.principal_type}}",
		"GROK_BUILD_PRINCIPAL_ID":   "{{credentials.principal_id}}",
		"GROK_BUILD_BASE_URL":       "{{credentials.runtime_base_url}}",
	})
	conn := addConnection(t, s, integrationGrokBuildSlug, "Grok Build", "", map[string]string{
		"access_token": "grok-access", "account_id": "user-1", "user_id": "user-1", "account_email": "user@example.test",
		"principal_type": "User", "principal_id": "principal-1", "runtime_base_url": integrationGrokBuildRuntimeURL,
		"token_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID, `{"model_large":"grok-4.6","model_medium":"grok-4.6","model_small":"grok-4.6"}`); err != nil {
		t.Fatal(err)
	}
	rec := getRuntimeToken(t, s, conn.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["provider"] != integrationGrokBuildSlug || payload["access_token"] != "grok-access" || payload["base_url"] != integrationGrokBuildRuntimeURL {
		t.Fatalf("payload = %#v", payload)
	}
	if !isLLMKey(integrationGrokBuildSlug) {
		t.Fatal("Grok Build must be advertised to the Core provider pool")
	}
	found := false
	for _, provider := range s.GetProviderPool(1) {
		if provider.Type == integrationGrokBuildSlug {
			found = true
			if provider.ModelLarge != "grok-4.6" || provider.ModelMedium != "grok-4.6" || provider.ModelSmall != "grok-4.6" {
				t.Fatalf("Grok Build models = %+v", provider)
			}
		}
	}
	if !found {
		t.Fatal("Grok Build did not reach the Core provider pool")
	}
	env, err := s.runtimeEnvFromConnections(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["GROK_BUILD_ACCESS_TOKEN"] != "grok-access" || env["GROK_BUILD_PROVIDER_ID"] == "" ||
		env["GROK_BUILD_USER_ID"] != "user-1" || env["GROK_BUILD_ACCOUNT_EMAIL"] != "user@example.test" ||
		env["GROK_BUILD_PRINCIPAL_TYPE"] != "User" || env["GROK_BUILD_PRINCIPAL_ID"] != "principal-1" ||
		env["GROK_BUILD_BASE_URL"] != integrationGrokBuildRuntimeURL {
		t.Fatalf("Grok Build runtime env = %#v", env)
	}
}

func TestGrokBuildDeviceSessionCannotBePolledByAnotherUser(t *testing.T) {
	globalConnectionDeviceAuthSessions = &connectionDeviceAuthSessionStore{sessions: map[string]*connectionDeviceAuthSession{}}
	t.Cleanup(func() {
		globalConnectionDeviceAuthSessions = &connectionDeviceAuthSessionStore{sessions: map[string]*connectionDeviceAuthSession{}}
	})
	globalConnectionDeviceAuthSessions.put(&connectionDeviceAuthSession{
		ID: "cauth_private", UserID: 10, AppSlug: integrationGrokBuildSlug, ExpiresAt: time.Now().Add(time.Minute),
	})
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/connections/auth/cauth_private", nil)
	req.Header.Set("X-User-ID", "11")
	rec := httptest.NewRecorder()
	s.handlePollConnectionDeviceAuth(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestEmbeddedGrokBuildCatalogIsRuntimeEnabled(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/grok-build.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	if app.Runtime == nil || app.Runtime.ProviderKey != integrationGrokBuildSlug || !supportsConnectionDeviceAuth(&app) {
		t.Fatalf("catalog app = %+v", app)
	}
	if len(app.Runtime.Env) != 7 || !isLLMKey(app.Runtime.ProviderKey) {
		t.Fatal("Grok Build catalog must inject its session environment and enter Core's provider pool")
	}
	foundUsage := false
	for _, capability := range app.Runtime.Capabilities {
		if capability == providerUsageCapability {
			foundUsage = true
			break
		}
	}
	if !foundUsage {
		t.Fatalf("Grok Build capabilities=%v missing %q", app.Runtime.Capabilities, providerUsageCapability)
	}
}

func grokBuildTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
