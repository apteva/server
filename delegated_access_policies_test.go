package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestConfiguredDelegatedAccessPolicyControlsMint(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "delegated-policy@example.com")
	project, err := s.store.CreateProject(user, "Delegated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	authInstallID := seedAppInstall(t, s, user, project.ID, "auth")
	policyScopes := json.RawMessage(`[
		{"type":"app_user","app":"channel-chat","actions":["chat.list","message.send"],"agent_ids":[566]},
		{"type":"app_user","app":"catalog","actions":["catalog.items.list"]}
	]`)
	policies, err := s.replaceDelegatedAccessPolicies(authInstallID, project.ID, []delegatedAccessPolicy{{
		OAuthClientID: "oauth-client-1", Scopes: policyScopes, TokenTTLSeconds: 900, RateLimitPerMinute: 45,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || policies[0].ProjectID != project.ID {
		t.Fatalf("stored policies=%+v", policies)
	}

	out, err := s.mintDelegatedUserKeyForInstall(authInstallID, delegatedUserKeyRequest{
		ProjectID: project.ID, OAuthClientID: "oauth-client-1",
		SubjectType: "user", SubjectID: "auth-user-7", SubjectEmail: "user@example.com",
		OrganizationID: "org-8", OrganizationSlug: "acme",
		AllowedOrigins: []string{"https://app.example.com"},
		// A generic issuer cannot escalate by supplying its own scopes or TTL.
		Scopes:    json.RawMessage(`[{"type":"app_user","app":"*","actions":["*"]}]`),
		ExpiresIn: 86400,
		RateLimit: 9999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["expires_in"] != 900 || out["oauth_client_id"] != "oauth-client-1" {
		t.Fatalf("mint response=%+v", out)
	}
	rawToken, _ := out["access_token"].(string)
	key, err := s.store.GetDelegatedUserAPIKey(HashAPIKey(rawToken))
	if err != nil {
		t.Fatal(err)
	}
	if key.Scopes != string(policyScopes) || key.RateLimitPerMinute != 45 {
		t.Fatalf("minted key scopes/rate=%s/%d", key.Scopes, key.RateLimitPerMinute)
	}
	if !delegatedUserScopeAllows(key.Scopes, "catalog", "catalog.items.list") || delegatedUserScopeAllows(key.Scopes, "catalog", "catalog.items.delete") {
		t.Fatal("generic catalog action policy was not enforced")
	}
	chatScope, ok := delegatedChannelChatScope(key.Scopes)
	if !ok || !reflect.DeepEqual(chatScope.AgentIDs, []int64{566}) {
		t.Fatalf("chat resource constraints were not preserved: %+v", chatScope)
	}

	allowed := httptest.NewRecorder()
	allowedReq := httptest.NewRequest(http.MethodGet, "/apps/catalog/items", nil)
	allowedReq.Header.Set("Origin", "https://app.example.com")
	if !s.authorizeDelegatedAppRequest(allowed, allowedReq, key, "catalog") {
		t.Fatalf("configured generic app denied: %d %s", allowed.Code, allowed.Body.String())
	}
	blocked := httptest.NewRecorder()
	blockedReq := httptest.NewRequest(http.MethodGet, "/apps/billing/items", nil)
	blockedReq.Header.Set("Origin", "https://app.example.com")
	if s.authorizeDelegatedAppRequest(blocked, blockedReq, key, "billing") || blocked.Code != http.StatusForbidden {
		t.Fatalf("unconfigured app status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	if _, err := s.replaceDelegatedAccessPolicies(authInstallID, project.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetDelegatedUserAPIKey(HashAPIKey(rawToken)); err == nil {
		t.Fatal("policy replacement did not revoke an existing delegated credential")
	}
}

func TestDelegatedAccessPolicyIsRequiredForOAuthClientMint(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "delegated-no-policy@example.com")
	project, err := s.store.CreateProject(user, "Delegated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	authInstallID := seedAppInstall(t, s, user, project.ID, "auth")
	_, err = s.mintDelegatedUserKeyForInstall(authInstallID, delegatedUserKeyRequest{
		ProjectID: project.ID, OAuthClientID: "unconfigured-client", SubjectID: "user-1",
		AllowedOrigins: []string{"https://app.example.com"},
	})
	if !errors.Is(err, errDelegatedPolicyNotFound) {
		t.Fatalf("mint error=%v, want policy not found", err)
	}
}

func TestDelegatedAccessPolicyRejectsWildcardsAndInvalidResources(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`[{"type":"app_user","app":"*","actions":["read"]}]`),
		json.RawMessage(`[{"type":"app_user","app":"catalog","actions":["*"]}]`),
		json.RawMessage(`[{"type":"app_user","app":"catalog","actions":["read"],"agent_ids":[0]}]`),
	}
	for _, scopes := range cases {
		if err := validateDelegatedPolicyScopes(scopes); err == nil {
			t.Errorf("accepted invalid scopes: %s", scopes)
		}
	}
}

func TestDelegatedAccessPolicyHandlerReplacesAndLists(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "delegated-policy-handler@example.com")
	project, err := s.store.CreateProject(user, "Delegated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	installID := seedAppInstall(t, s, user, project.ID, "auth")
	path := "/apps/installs/" + itoa64(installID) + "/delegated-access-policies"
	put := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{
		"policies":[{
			"oauth_client_id":"web-client",
			"scopes":[{"type":"app_user","app":"catalog","actions":["items.list"]}]
		}]
	}`))
	putRec := httptest.NewRecorder()
	s.handleDelegatedAccessPolicies(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, path, nil)
	getRec := httptest.NewRecorder()
	s.handleDelegatedAccessPolicies(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var body struct {
		Policies []delegatedAccessPolicy `json:"policies"`
	}
	if err := json.NewDecoder(getRec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Policies) != 1 || body.Policies[0].OAuthClientID != "web-client" || body.Policies[0].ProjectID != project.ID {
		t.Fatalf("listed policies=%+v", body.Policies)
	}
}

func TestDelegatedDynamicCORSWorksForAnyConfiguredApp(t *testing.T) {
	s := newTestServer(t)
	user := mkUser(t, s, "delegated-cors@example.com")
	project, err := s.store.CreateProject(user, "Delegated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	raw := "uk_generic_cors_test"
	if _, err := s.store.CreateAPIKey(user, "delegated", HashAPIKey(raw), "uk_generic", APIKeyCreateOptions{
		Kind: "delegated_user", ProjectID: project.ID,
		Scopes:         `[{"type":"app_user","app":"catalog","actions":["items.list"]}]`,
		AllowedOrigins: `["https://store.example"]`, ExpiresAt: "2999-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodOptions, "/api/apps/catalog/items", nil)
	if !s.delegatedAppCORSOriginAllowed(req, "https://store.example") {
		t.Fatal("configured catalog origin was not allowed")
	}
	if s.delegatedAppCORSOriginAllowed(req, "https://evil.example") {
		t.Fatal("unconfigured origin was allowed")
	}
	other := httptest.NewRequest(http.MethodOptions, "/api/apps/billing/items", nil)
	if s.delegatedAppCORSOriginAllowed(other, "https://store.example") {
		t.Fatal("origin was allowed for an app outside the policy")
	}
}
