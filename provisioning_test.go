package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestProvisioningApplyUsesAdminAPIKeyAndIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	adminID := ensureTestAdmin(t, s)
	s.catalog = createTestCatalog(t)
	rawAPIKey := "sk-provisioning-test"
	if _, err := s.store.CreateAPIKey(adminID, "provisioner", HashAPIKey(rawAPIKey), "sk-provisio"); err != nil {
		t.Fatal(err)
	}

	manifest := sdk.Manifest{
		Schema:  sdk.SchemaCurrent,
		Name:    "telephony-push-test",
		Version: "1.0.0",
		Scopes:  []sdk.Scope{sdk.ScopeProject},
		Requires: sdk.Requires{Integrations: []sdk.IntegrationDep{{
			Role: "provider", Kind: "integration", CompatibleSlugs: []string{"pushover"}, Required: true,
		}}},
	}
	installID := seedInstallWithBindings(t, s, manifest.Name, manifest, nil)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE app_installs SET version=?,manifest_json=? WHERE id=?`, manifest.Version, string(manifestJSON), installID); err != nil {
		t.Fatal(err)
	}
	body := sdk.ManagedProvisioningApplyRequest{
		RequestID: "account-123:phone:1",
		TenantID:  "account-123",
		Grants: []sdk.ManagedConnectionGrantDelivery{{
			TenantID: "account-123", GrantID: "phone-provider", ConnectionID: 42,
			AppSlug: "pushover", ProjectID: "proj-1", ControllerToken: "aptg_delivery_secret",
			ControllerExecute: "https://controller.example/api/managed/grants/phone-provider/execute",
			AllowedTools:      []string{"send_notification"},
		}},
		Bundle: &sdk.ManagedTenantBundle{
			TenantID: "account-123", BundleID: "phone", Revision: 1,
			Apps: []sdk.ManagedBundleApp{{
				Key: "telephony", ManifestYAML: string(manifestJSON), ProjectID: "proj-1",
				Bindings: map[string]string{"provider": "phone-provider"},
			}},
		},
	}
	apply := func(value sdk.ManagedProvisioningApplyRequest) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(value)
		req := httptest.NewRequest(http.MethodPut, "/provisioning/apply", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+rawAPIKey)
		rec := httptest.NewRecorder()
		s.authMiddleware(s.handleProvisioningApply)(rec, req)
		return rec
	}

	first := apply(body)
	if first.Code != http.StatusOK {
		t.Fatalf("first apply status=%d body=%s", first.Code, first.Body.String())
	}
	var result sdk.ManagedProvisioningApplyResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || result.Connections["phone-provider"] <= 0 || result.Revision != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var bindingsRaw string
	if err := s.store.db.QueryRow(`SELECT integration_bindings FROM app_installs WHERE id=?`, installID).Scan(&bindingsRaw); err != nil {
		t.Fatal(err)
	}
	var bindings map[string]any
	_ = json.Unmarshal([]byte(bindingsRaw), &bindings)
	if numeric, ok := numericBindingID(bindings["provider"]); !ok || numeric != result.Connections["phone-provider"] {
		t.Fatalf("managed connection was not bound: bindings=%s result=%+v", bindingsRaw, result)
	}

	retry := apply(body)
	if retry.Code != http.StatusOK || retry.Body.String() != first.Body.String() {
		t.Fatalf("retry status=%d body=%s first=%s", retry.Code, retry.Body.String(), first.Body.String())
	}
	var connectionCount int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE external_id='managed:account-123:phone-provider'`).Scan(&connectionCount); err != nil {
		t.Fatal(err)
	}
	if connectionCount != 1 {
		t.Fatalf("managed connection count=%d, want 1", connectionCount)
	}

	conflictBody := body
	conflictBody.Grants = append([]sdk.ManagedConnectionGrantDelivery(nil), body.Grants...)
	conflictBody.Grants[0].ControllerToken = "aptg_different"
	conflict := apply(conflictBody)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestProvisioningApplyRejectsNonAdminAPIKey(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("member@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	rawAPIKey := "sk-member-test"
	if _, err := s.store.CreateAPIKey(user.ID, "member", HashAPIKey(rawAPIKey), "sk-member"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(sdk.ManagedProvisioningApplyRequest{RequestID: "r1", TenantID: "t1", RevokedGrantIDs: []string{"g1"}})
	req := httptest.NewRequest(http.MethodPut, "/provisioning/apply", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawAPIKey)
	rec := httptest.NewRecorder()
	s.authMiddleware(s.handleProvisioningApply)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
