package main

// Tests for the per-(install, instance) grants surface — both the
// dashboard endpoints (/api/apps/installs/:id/...) and the sidecar
// callback (/api/apps/callback/grants).
//
// Back-compat is the load-bearing property: a freshly-installed app
// with no grants must return default_effect='allow' + empty rules,
// and the SDK's caller treats that as "all calls allowed".

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// seedStorageInstall stamps an install with a manifest that declares
// a folder resource + files.read/write/delete permissions, the same
// way the storage app will.
func seedStorageInstall(t *testing.T, s *Server) int64 {
	t.Helper()
	manifest := sdk.Manifest{
		Schema:  sdk.SchemaCurrent,
		Name:    "storage",
		Version: "0.6.0",
		Provides: sdk.Provides{
			Resources: []sdk.ResourceDecl{
				{Name: "folder", Label: "Folder", Matcher: "glob", Picker: "tree", ListingVisibility: "navigable"},
			},
			ProvidedPermissions: []sdk.ProvidedPermission{
				{Name: "files.read", Resource: "folder"},
				{Name: "files.write", Resource: "folder"},
				{Name: "files.delete", Resource: "folder"},
			},
			MCPTools: []sdk.MCPToolSpec{
				{Name: "files_get", Requires: "files.read", ResourceFrom: "folder/{arg.folder}"},
				{Name: "files_list"}, // handler-side filter, no platform gate
				{Name: "files_delete", Requires: "files.delete", ResourceFrom: "folder/{arg.folder}"},
			},
		},
	}
	id := seedInstallWithBindings(t, s, "storage", manifest, nil)
	// Need an instances row for FK.
	s.store.db.Exec(`INSERT OR IGNORE INTO instances (id, project_id, name, status) VALUES (7, 'proj-1', 'agent-A', 'running')`)
	s.store.db.Exec(`INSERT OR IGNORE INTO instances (id, project_id, name, status) VALUES (8, 'proj-1', 'agent-B', 'running')`)
	return id
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode: %v — body: %s", err, rec.Body.String())
	}
}

// GET /api/apps/installs/:id/permissions returns the catalog the
// dashboard renders the picker from.
func TestGrants_PermissionsCatalog(t *testing.T) {
	s := newTestServer(t)
	installID := seedStorageInstall(t, s)
	req := httptest.NewRequest(http.MethodGet, "/apps/installs/"+itoa(installID)+"/permissions", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleInstallGrants(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Resources   []sdk.ResourceDecl       `json:"resources"`
		Permissions []sdk.ProvidedPermission `json:"permissions"`
		Tools       []map[string]any         `json:"tools"`
	}
	decodeBody(t, rec, &got)
	if len(got.Resources) != 1 || got.Resources[0].Name != "folder" {
		t.Fatalf("expected folder resource, got %+v", got.Resources)
	}
	if len(got.Permissions) != 3 {
		t.Fatalf("expected 3 permissions, got %d", len(got.Permissions))
	}
	// Verify per-tool annotations made it through.
	gateExpected := map[string]string{"files_get": "files.read", "files_delete": "files.delete"}
	for _, tool := range got.Tools {
		want, ok := gateExpected[tool["name"].(string)]
		if !ok {
			continue
		}
		if tool["requires"] != want {
			t.Errorf("tool %s: requires=%v, want %s", tool["name"], tool["requires"], want)
		}
	}
}

// PUT replace + GET round-trip the policy. Then the sidecar callback
// (which the SDK's MCP handler hits) should return the same grants.
func TestGrants_ReplaceAndCallback(t *testing.T) {
	s := newTestServer(t)
	installID := seedStorageInstall(t, s)

	// Operator writes the policy: agent-A gets read on invoices/**.
	body := `{"default_effect":"deny","rules":[
		{"effect":"allow","permission":"files.read","resource":"folder/invoices/**"},
		{"effect":"allow","permission":"files.write","resource":"folder/invoices/**"}
	]}`
	req := httptest.NewRequest(http.MethodPut,
		"/apps/installs/"+itoa(installID)+"/grants/by-instance/7", strings.NewReader(body))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleInstallGrants(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	var stored grantsResponse
	decodeBody(t, rec, &stored)
	if stored.DefaultEffect != "deny" || len(stored.Grants) != 2 {
		t.Fatalf("after PUT: %+v", stored)
	}

	// Sidecar fetches policy via the callback that the SDK uses.
	req = httptest.NewRequest(http.MethodGet, "/apps/callback/grants?instance_id=7", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", rec.Code, rec.Body.String())
	}
	var fromCallback sdk.GrantsResponse
	decodeBody(t, rec, &fromCallback)
	if fromCallback.DefaultEffect != "deny" || len(fromCallback.Grants) != 2 {
		t.Fatalf("callback: %+v", fromCallback)
	}
	if fromCallback.Grants[0].Permission != "files.read" {
		t.Fatalf("unexpected first grant: %+v", fromCallback.Grants[0])
	}

	// Another agent (8) was not given a policy — gets default-allow,
	// no rules. (Important: each agent gets its own row count.)
	req = httptest.NewRequest(http.MethodGet, "/apps/callback/grants?instance_id=8", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	var unscoped sdk.GrantsResponse
	decodeBody(t, rec, &unscoped)
	// default_effect on the install was set to deny by the PUT above
	// (via DefaultEffect on body), so agent-B inherits it. This is
	// the desired behavior — install-wide default applies to anyone
	// without explicit grants.
	if unscoped.DefaultEffect != "deny" {
		t.Fatalf("default_effect for unscoped agent: %q want deny", unscoped.DefaultEffect)
	}
	if len(unscoped.Grants) != 0 {
		t.Fatalf("agent-B should have no grants, got %+v", unscoped.Grants)
	}
}

// Evaluate dry-runs without persistence — uses the same matcher as the
// SDK so dashboard probes match runtime behavior.
func TestGrants_Evaluate(t *testing.T) {
	s := newTestServer(t)
	installID := seedStorageInstall(t, s)
	// Seed a deny-default install with an allow on invoices/**.
	s.store.db.Exec(`UPDATE app_installs SET default_effect='deny' WHERE id=?`, installID)
	s.store.db.Exec(`INSERT INTO app_grants (install_id, instance_id, effect, permission, resource)
		VALUES (?, 7, 'allow', 'files.read', 'folder/invoices/**')`, installID)

	cases := []struct {
		permission, resource string
		want                 bool
	}{
		{"files.read", "folder/invoices/q3/x.pdf", true},
		{"files.read", "folder/salaries/jan", false},
		{"files.delete", "folder/invoices/x.pdf", false}, // wrong perm
	}
	for _, tc := range cases {
		body := `{"instance_id":7,"permission":"` + tc.permission + `","resource":"` + tc.resource + `"}`
		req := httptest.NewRequest(http.MethodPost,
			"/apps/installs/"+itoa(installID)+"/grants/evaluate", strings.NewReader(body))
		req.Header.Set("X-User-ID", "1")
		rec := httptest.NewRecorder()
		s.handleInstallGrants(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Allowed bool `json:"allowed"`
		}
		decodeBody(t, rec, &out)
		if out.Allowed != tc.want {
			t.Errorf("evaluate(%s, %s) = %v, want %v", tc.permission, tc.resource, out.Allowed, tc.want)
		}
	}
}

// Back-compat: an install that hasn't migrated to the permissions
// model (no grants written, default_effect untouched) returns "allow"
// + empty rules. The SDK gate then degrades to pass-through.
func TestGrants_BackwardsCompat_DefaultAllow(t *testing.T) {
	s := newTestServer(t)
	installID := seedStorageInstall(t, s)

	req := httptest.NewRequest(http.MethodGet, "/apps/callback/grants?instance_id=7", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got sdk.GrantsResponse
	decodeBody(t, rec, &got)
	if got.DefaultEffect != "allow" {
		t.Fatalf("default_effect=%q, want allow", got.DefaultEffect)
	}
	if len(got.Grants) != 0 {
		t.Fatalf("expected no grants, got %+v", got.Grants)
	}
	// Round-trip through the SDK's Caller — the install's full
	// access should let any (perm, resource) through.
	caller := &sdk.Caller{DefaultEffect: got.DefaultEffect, Grants: got.Grants}
	if !caller.Allows("files.delete", "folder/secret") {
		t.Fatal("default-allow caller should permit anything")
	}
}

// Rejecting unknown permissions prevents typos from accidentally
// granting nothing.
func TestGrants_RejectsUnknownPermission(t *testing.T) {
	s := newTestServer(t)
	installID := seedStorageInstall(t, s)
	body := `{"instance_id":7,"effect":"allow","permission":"files.bogus","resource":"*"}`
	req := httptest.NewRequest(http.MethodPost,
		"/apps/installs/"+itoa(installID)+"/grants", strings.NewReader(body))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleInstallGrants(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown permission, got %d: %s", rec.Code, rec.Body.String())
	}
}
