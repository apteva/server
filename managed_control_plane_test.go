package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func managedControllerRequest(t *testing.T, s *Server, installID int64, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/apps/callback/managed-tenants/"+path, bytes.NewReader(raw))
	req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprintf("%d", installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func managedPublicRequest(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/managed/"+path, bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleManagedPublic(rec, req)
	return rec
}

func TestManagedControlPlaneEnrollmentGrantAndReconcile(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = createTestCatalog(t)
	s.port = "5280"

	installID := managedConnectionTestInstall(t, s, "saas-controller",
		sdk.PermConnectionsManageOwnedCredentials,
		sdk.PermManagedTenantsManage,
	)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET project_id='' WHERE id=?`, installID); err != nil {
		t.Fatal(err)
	}
	managed := managedConnectionRequest(t, s, installID, http.MethodPost, "managed/ensure", sdk.ManagedConnectionRequest{
		Key: "provider:main", AppSlug: "pushover", Name: "Managed provider",
		Fields: map[string]string{"app_token": "top-secret", "user_key": "safe-public"},
	})
	if managed.Code != http.StatusOK {
		t.Fatalf("managed connection status=%d body=%s", managed.Code, managed.Body.String())
	}
	conn := decodePlatformConnection(t, managed)

	enrollmentRec := managedControllerRequest(t, s, installID, http.MethodPost, "enrollments", sdk.ManagedTenantEnrollmentRequest{
		TenantID: "account-123", AccountID: "acct_123", ExpiresIn: 300,
	})
	if enrollmentRec.Code != http.StatusOK {
		t.Fatalf("enrollment status=%d body=%s", enrollmentRec.Code, enrollmentRec.Body.String())
	}
	var enrollment sdk.ManagedTenantEnrollment
	if err := json.Unmarshal(enrollmentRec.Body.Bytes(), &enrollment); err != nil || enrollment.Ticket == "" {
		t.Fatalf("decode enrollment err=%v body=%s", err, enrollmentRec.Body.String())
	}
	var storedTicket string
	if err := s.store.db.QueryRow(`SELECT ticket_hash FROM managed_tenant_enrollments WHERE tenant_id=?`, enrollment.TenantID).Scan(&storedTicket); err != nil {
		t.Fatal(err)
	}
	if storedTicket == enrollment.Ticket || strings.Contains(storedTicket, "apte_") {
		t.Fatal("raw enrollment ticket was stored")
	}

	grantRec := managedControllerRequest(t, s, installID, http.MethodPost, "grants", sdk.ManagedConnectionGrantRequest{
		TenantID: "account-123", GrantID: "phone-provider", ConnectionID: conn.ID, AppSlug: "pushover",
		AllowedTools: []string{"send_notification"}, PublicFields: map[string]string{"phone_number": "+15551234567"},
		Constraints: sdk.ManagedGrantConstraints{FixedInput: map[string]any{"account": "account-123"}},
	})
	if grantRec.Code != http.StatusOK {
		t.Fatalf("grant status=%d body=%s", grantRec.Code, grantRec.Body.String())
	}
	var tokenHash, tokenEncrypted string
	if err := s.store.db.QueryRow(`SELECT token_hash,token_encrypted FROM managed_connection_grants WHERE tenant_id='account-123' AND grant_id='phone-provider'`).Scan(&tokenHash, &tokenEncrypted); err != nil {
		t.Fatal(err)
	}
	if tokenHash == "" || tokenEncrypted == "" || strings.Contains(tokenEncrypted, "aptg_") {
		t.Fatal("grant token was not stored as hash plus ciphertext")
	}

	bundleRec := managedControllerRequest(t, s, installID, http.MethodPost, "bundles", sdk.ManagedTenantBundleRequest{
		TenantID: "account-123", BundleID: "phone", Apps: []sdk.ManagedBundleApp{{
			Key: "telephony", ManifestYAML: "schema: apteva-app/v1\nname: telephony\nversion: 1.0.0\nscopes: [project]\n",
			Bindings: map[string]string{"provider": "phone-provider"},
		}},
	})
	if bundleRec.Code != http.StatusOK {
		t.Fatalf("bundle status=%d body=%s", bundleRec.Code, bundleRec.Body.String())
	}
	bundleStatus := managedControllerRequest(t, s, installID, http.MethodGet, "bundles/account-123/phone", nil)
	var gotBundle sdk.ManagedTenantBundle
	_ = json.Unmarshal(bundleStatus.Body.Bytes(), &gotBundle)
	if bundleStatus.Code != http.StatusOK || gotBundle.Status != "pending" || gotBundle.Revision != 1 {
		t.Fatalf("bundle get status=%d body=%s", bundleStatus.Code, bundleStatus.Body.String())
	}

	enroll := managedPublicRequest(t, s, http.MethodPost, "enroll", "", map[string]string{"ticket": enrollment.Ticket})
	if enroll.Code != http.StatusOK {
		t.Fatalf("public enroll status=%d body=%s", enroll.Code, enroll.Body.String())
	}
	var identity managedTenantIdentity
	if err := json.Unmarshal(enroll.Body.Bytes(), &identity); err != nil || identity.Token == "" || identity.TenantID != "account-123" {
		t.Fatalf("identity err=%v value=%+v", err, identity)
	}
	var storedIdentity string
	if err := s.store.db.QueryRow(`SELECT identity_token_hash FROM managed_tenants WHERE tenant_id=?`, identity.TenantID).Scan(&storedIdentity); err != nil {
		t.Fatal(err)
	}
	if storedIdentity == identity.Token || strings.Contains(storedIdentity, "apti_") {
		t.Fatal("raw tenant identity was stored")
	}
	// Tickets are one-time even when replayed immediately.
	replay := managedPublicRequest(t, s, http.MethodPost, "enroll", "", map[string]string{"ticket": enrollment.Ticket})
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("ticket replay status=%d body=%s", replay.Code, replay.Body.String())
	}

	reconcile := managedPublicRequest(t, s, http.MethodGet, "reconcile", identity.Token, nil)
	if reconcile.Code != http.StatusOK {
		t.Fatalf("reconcile status=%d body=%s", reconcile.Code, reconcile.Body.String())
	}
	var desired managedReconcileResponse
	if err := json.Unmarshal(reconcile.Body.Bytes(), &desired); err != nil {
		t.Fatal(err)
	}
	if len(desired.Grants) != 1 || len(desired.Bundles) != 1 || desired.Grants[0].ControllerToken == "" || desired.Grants[0].PublicFields["phone_number"] == "" {
		t.Fatalf("unexpected desired state: %+v", desired)
	}

	// Both ends enforce generic constraints. This is rejected before any
	// upstream integration call can happen.
	denied := managedPublicRequest(t, s, http.MethodPost, "grants/phone-provider/execute", desired.Grants[0].ControllerToken, map[string]any{
		"tool": "send_notification", "input": map[string]any{"account": "someone-else"},
	})
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "controller-defined") {
		t.Fatalf("constraint status=%d body=%s", denied.Code, denied.Body.String())
	}

	revoke := managedControllerRequest(t, s, installID, http.MethodDelete, "grants/account-123/phone-provider", nil)
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	reconcile = managedPublicRequest(t, s, http.MethodGet, "reconcile", identity.Token, nil)
	if err := json.Unmarshal(reconcile.Body.Bytes(), &desired); err != nil {
		t.Fatal(err)
	}
	if len(desired.Grants) != 0 || len(desired.RevokedGrants) != 1 || desired.RevokedGrants[0] != "phone-provider" {
		t.Fatalf("revoked grant not reconciled: %+v", desired)
	}
	oldGrantToken := tokenHash
	grantRec = managedControllerRequest(t, s, installID, http.MethodPost, "grants", sdk.ManagedConnectionGrantRequest{
		TenantID: "account-123", GrantID: "phone-provider", ConnectionID: conn.ID, AppSlug: "pushover",
		AllowedTools: []string{"send_notification"},
	})
	if grantRec.Code != http.StatusOK {
		t.Fatalf("reactivate status=%d body=%s", grantRec.Code, grantRec.Body.String())
	}
	if err := s.store.db.QueryRow(`SELECT token_hash FROM managed_connection_grants WHERE tenant_id='account-123' AND grant_id='phone-provider'`).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if tokenHash == oldGrantToken {
		t.Fatal("reactivating a revoked grant did not rotate its bearer token")
	}
}

func TestManagedControlPlaneRequiresGlobalAdminOwnedPermission(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	installID := managedConnectionTestInstall(t, s, "controller-without-permission")
	rec := managedControllerRequest(t, s, installID, http.MethodPost, "enrollments", sdk.ManagedTenantEnrollmentRequest{TenantID: "tenant-one"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), string(sdk.PermManagedTenantsManage)) {
		t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
	}

	installID = managedConnectionTestInstall(t, s, "project-controller", sdk.PermManagedTenantsManage)
	rec = managedControllerRequest(t, s, installID, http.MethodPost, "enrollments", sdk.ManagedTenantEnrollmentRequest{TenantID: "tenant-two"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "global") {
		t.Fatalf("project-scoped controller status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDelegatedProviderGenericConstraints(t *testing.T) {
	plain := `{"_apteva_delegated_provider":"1","grant_id":"g","controller_execute_url":"https://controller.test/x","controller_token":"secret","parent_connection_id":"4","allowed_tools":"send","constraints":"{\"fixed_input\":{\"tenant\":\"t1\"},\"allowed_values\":{\"region\":[\"eu\"]},\"denied_fields\":[\"admin\"]}"}`
	grant, ok, err := parseDelegatedProviderCredentials(plain)
	if err != nil || !ok {
		t.Fatalf("parse ok=%v err=%v", ok, err)
	}
	if err := grant.validate("generic", "send", map[string]any{"tenant": "t1", "region": "eu"}); err != nil {
		t.Fatalf("valid input denied: %v", err)
	}
	injected := map[string]any{"region": "eu"}
	if err := grant.validate("generic", "send", injected); err != nil || injected["tenant"] != "t1" {
		t.Fatalf("fixed input was not injected: input=%+v err=%v", injected, err)
	}
	for _, input := range []map[string]any{
		{"tenant": "t2", "region": "eu"},
		{"tenant": "t1", "region": "us"},
		{"tenant": "t1", "region": "eu", "admin": true},
	} {
		if err := grant.validate("generic", "send", input); err == nil {
			t.Fatalf("invalid input accepted: %+v", input)
		}
	}
}
