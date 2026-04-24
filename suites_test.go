package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fixture: a minimal group with an OmniKit-shaped app declaring the
// credential_group + scopes.account.project_binding.
func newSuiteTestCatalog() *AppCatalog {
	c := NewAppCatalog()
	desc := "desc"
	logo := "https://example.com/logo.png"
	app := &AppTemplate{
		Slug: "testsuite-storage", Name: "Test Storage",
		BaseURL: "https://api.test.example",
		Auth: AppAuthConfig{
			Types:   []string{"api_key"},
			Headers: map[string]string{"X-API-Key": "{{api_key}}"},
		},
		Tools: []AppToolDef{
			{Name: "list", Method: "GET", Path: "/items"},
		},
		CredentialGroup: &CredentialGroup{
			ID: "testsuite", Name: "TestSuite", Logo: &logo, Description: desc,
			Discovery: &GroupDiscoveryConfig{
				ListProjects: DiscoveryCall{
					Method: "GET", Path: "/projects", ResponsePath: "data",
					IDField: "id", LabelField: "name",
				},
			},
		},
		Scopes: &AppScopes{
			Account: &AppScope{
				CredentialFields: []CredentialField{{Name: "api_key", Label: "API Key"}},
				AuthHeaders:      map[string]string{"X-API-Key": "{{api_key}}"},
				ProjectBinding:   &ProjectBinding{Type: "header", Name: "X-Project-Id", Value: "{{project_id}}"},
			},
			Project: &AppScope{
				CredentialFields: []CredentialField{{Name: "api_key", Label: "API Key"}},
				AuthHeaders:      map[string]string{"X-API-Key": "{{api_key}}"},
			},
		},
	}
	c.Register(app)
	return c
}

func TestGroupAggregation(t *testing.T) {
	c := newSuiteTestCatalog()
	g := c.GetGroup("testsuite")
	if g == nil {
		t.Fatal("group not found after Register")
	}
	if len(g.Members) != 1 || g.Members[0] != "testsuite-storage" {
		t.Fatalf("unexpected members: %+v", g.Members)
	}
	sums := c.ListGroups()
	if len(sums) != 1 || sums[0].ID != "testsuite" {
		t.Fatalf("unexpected summaries: %+v", sums)
	}
	if !sums[0].HasAccountScope || !sums[0].HasProjectScope {
		t.Fatal("expected both scope flags true")
	}
}

func TestMasterSlug(t *testing.T) {
	if !IsMasterSlug(MasterSlug("omnikit")) {
		t.Fatal("MasterSlug output should be a master slug")
	}
	if IsMasterSlug("omnikit-storage") {
		t.Fatal("regular slug marked as master")
	}
	if GroupIDFromMasterSlug(MasterSlug("omnikit")) != "omnikit" {
		t.Fatal("roundtrip failed")
	}
}

func TestStripReservedCreds(t *testing.T) {
	in := map[string]string{
		"_type": "child", "_master_id": "7", "_project_id": "p1",
		"api_key": "k", "username": "u",
	}
	out := stripReservedCreds(in)
	if _, ok := out["_type"]; ok {
		t.Fatal("expected _type stripped")
	}
	if out["api_key"] != "k" || out["username"] != "u" {
		t.Fatalf("real creds not preserved: %+v", out)
	}
}

func TestResolveConnectionContext_HeaderBinding(t *testing.T) {
	// Arrange: a master row (id 1) with api_key=foo, and a child row's
	// credentials pointing at it.
	store := newTestStore(t)
	defer store.Close()
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	// Insert master
	masterBlob := map[string]string{
		"_type": "master", "_group": "testsuite", "_scope": "account",
		"api_key": "master_key_abc",
	}
	mb, _ := json.Marshal(masterBlob)
	enc, err := Encrypt(secret, string(mb))
	if err != nil {
		t.Fatal(err)
	}
	masterConn, err := store.CreateConnectionExt(ConnectionInput{
		UserID: 1, AppSlug: MasterSlug("testsuite"), AppName: "TestSuite",
		Name: "master", AuthType: "api_key", EncryptedCreds: enc, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Build child creds blob pointing at masterConn
	childCreds := map[string]string{
		"_type": "child", "_master_id": ntoa(masterConn.ID), "_project_id": "proj_ext_1",
	}

	c := newSuiteTestCatalog()
	app := c.Get("testsuite-storage")

	ctx, err := resolveConnectionContextRaw(store, secret, 1, app, childCreds, map[string]any{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ctx.Credentials["api_key"] != "master_key_abc" {
		t.Fatalf("expected master's api_key, got %q", ctx.Credentials["api_key"])
	}
	if ctx.App == app {
		t.Fatal("expected cloned app (binding clones)")
	}
	if ctx.App.Auth.Headers["X-Project-Id"] != "proj_ext_1" {
		t.Fatalf("expected X-Project-Id=proj_ext_1, got %q", ctx.App.Auth.Headers["X-Project-Id"])
	}
	// Sanity: master's own api_key header still in place
	if ctx.App.Auth.Headers["X-API-Key"] != "{{api_key}}" {
		t.Fatalf("expected X-API-Key template preserved, got %q", ctx.App.Auth.Headers["X-API-Key"])
	}
	// And the original catalog app must not have been mutated.
	if _, leaked := app.Auth.Headers["X-Project-Id"]; leaked {
		t.Fatal("binding leaked into shared catalog app")
	}
}

func TestResolveConnectionContext_Legacy(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	secret := make([]byte, 32)
	c := newSuiteTestCatalog()
	app := c.Get("testsuite-storage")
	// No _type key → passthrough
	creds := map[string]string{"api_key": "plain"}
	ctx, err := resolveConnectionContextRaw(store, secret, 1, app, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.App != app {
		t.Fatal("legacy path should not clone app")
	}
	if ctx.Credentials["api_key"] != "plain" {
		t.Fatal("creds changed in legacy path")
	}
}

func TestDiscoverProjects_Happy(t *testing.T) {
	// Fake upstream that returns {data: [{id,name}, ...]}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "okt_acc_123" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"p1","name":"Marketing"},{"id":"p2","name":"Staging"}]}`))
	}))
	defer srv.Close()

	logo := "l"
	app := &AppTemplate{
		Slug:    "testsuite-storage",
		BaseURL: srv.URL,
		Auth:    AppAuthConfig{Headers: map[string]string{"X-API-Key": "{{api_key}}"}},
		Scopes: &AppScopes{Account: &AppScope{AuthHeaders: map[string]string{"X-API-Key": "{{api_key}}"}}},
	}
	group := &CredentialGroup{
		ID: "testsuite", Name: "TestSuite", Logo: &logo,
		Discovery: &GroupDiscoveryConfig{
			ListProjects: DiscoveryCall{
				Method: "GET", Path: "/projects", ResponsePath: "data",
				IDField: "id", LabelField: "name",
			},
		},
	}
	projs, err := discoverProjects(app, group, map[string]string{"api_key": "okt_acc_123"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(projs) != 2 || projs[0].ID != "p1" || projs[0].Label != "Marketing" {
		t.Fatalf("unexpected projects: %+v", projs)
	}
}

func TestDiscoverProjects_BadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid key"}`, 401)
	}))
	defer srv.Close()
	logo := "l"
	app := &AppTemplate{
		Slug: "x", BaseURL: srv.URL,
		Auth: AppAuthConfig{Headers: map[string]string{"X-API-Key": "{{api_key}}"}},
	}
	group := &CredentialGroup{
		ID: "g", Logo: &logo,
		Discovery: &GroupDiscoveryConfig{
			ListProjects: DiscoveryCall{Method: "GET", Path: "/p", IDField: "id", LabelField: "name"},
		},
	}
	_, err := discoverProjects(app, group, map[string]string{"api_key": "wrong"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

// Small helpers ---------------------------------------------------------------

func ntoa(n int64) string {
	return strings.TrimSpace(intToStr(n))
}

func intToStr(n int64) string {
	// avoid strconv import cycle in this file — use the same
	// conversion as FormatInt.
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
