package main

import (
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeResourcePolicyCASAndDeploymentSnapshot(t *testing.T) {
	s, m, _, id, p := runtimeTestSetup(t)
	manifest, e := m.resourceManifest(id)
	if e != nil {
		t.Fatal(e)
	}
	manifest.Runtime.ResourcePolicy = &sdk.ResourceDeclaration{Workloads: []string{"batch"}, TrafficClasses: []string{"media"}, Defaults: sdk.ResourcePolicy{Workloads: map[string]sdk.WorkloadPolicy{"batch": {Concurrency: 1, Queue: 2, Weight: 1}}, Network: map[string]sdk.SocketPolicy{"media": {}}}}
	if _, e = s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(manifest), id); e != nil {
		t.Fatal(e)
	}
	desired := sdk.ResourcePolicy{Workloads: map[string]sdk.WorkloadPolicy{"batch": {Concurrency: 2, Queue: 4, Weight: 2}}}
	if _, e = s.store.db.Exec(`INSERT INTO app_runtime_resource_policies VALUES(?,1,?)`, id, runtimeJSON(desired)); e != nil {
		t.Fatal(e)
	}
	d, e := m.makeDeployment(id, p, &manifest, 0)
	if e != nil {
		t.Fatal(e)
	}
	if d.ResourceRevision != 1 || d.Resources.Workloads["batch"].Concurrency != 2 {
		t.Fatal(d)
	}
	env, e := m.deploymentEnvironment(d, "token")
	if e != nil {
		t.Fatal(e)
	}
	var envelope sdk.ResourceEnvelope
	if e = json.Unmarshal([]byte(env["APTEVA_RESOURCE_POLICY"]), &envelope); e != nil {
		t.Fatal(e)
	}
	if envelope.DeploymentID != d.ID || envelope.Generation == "" || envelope.Policy.Workloads["batch"].Concurrency != 2 {
		t.Fatal(envelope)
	}
	if _, e = s.store.db.Exec(`UPDATE app_runtime_resource_policies SET revision=2,policy_json='{}' WHERE install_id=?`, id); e != nil {
		t.Fatal(e)
	}
	if d.Resources.Workloads["batch"].Concurrency != 2 {
		t.Fatal("deployment changed when desired policy changed")
	}
	d2, e := m.makeDeployment(id, p, &manifest, 0)
	if e != nil {
		t.Fatal(e)
	}
	if d2.Resources.Workloads["batch"].Concurrency != 1 || d2.ResourceRevision != 2 {
		t.Fatal(d2)
	}
}
func TestRuntimeResourceOldBackendRejectsProcessPolicy(t *testing.T) {
	_, m, _, id, p := runtimeTestSetup(t)
	manifest, e := m.resourceManifest(id)
	if e != nil {
		t.Fatal(e)
	}
	p.Resources = &sdk.ResourcePolicy{Process: sdk.ProcessResources{CPUWeight: 200}}
	if _, e = m.makeDeployment(id, p, &manifest, 0); e == nil || !strings.Contains(e.Error(), "does not support") {
		t.Fatal(e)
	}
}
func TestRuntimeResourceRoutesRequireAdmin(t *testing.T) {
	s, _, _, id, _ := runtimeTestSetup(t)
	// No authenticated principal may mutate the installation even with valid JSON.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/apps/installs/1/deployment/resources", strings.NewReader(`{"revision":0,"policy":{}}`))
	s.handleRuntimeResources(w, r, id)
	if w.Code == 200 {
		t.Fatal("anonymous resource mutation")
	}
}

func TestRuntimeResourceSaveCASAndAppTokenRejection(t *testing.T) {
	s, m, _, id, _ := runtimeTestSetup(t)
	key := "resource-admin-test-key"
	if _, e := s.store.CreateAPIKey(1, "test", HashAPIKey(key), "resources"); e != nil {
		t.Fatal(e)
	}
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { s.handleRuntimeResources(w, r, id) })
	request := func(token, body string) int {
		r := httptest.NewRequest("PUT", "/apps/installs/1/deployment/resources", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := request(key, `{"revision":0,"policy":{"process":{"cpu_weight":200}}}`); code != 200 {
		t.Fatal(code)
	}
	if code := request(key, `{"revision":0,"policy":{}}`); code != 409 {
		t.Fatal("lost update accepted", code)
	}
	saved, e := m.resourcePolicy(id)
	if e != nil || saved.Revision != 1 || saved.Policy.Process.CPUWeight != 200 {
		t.Fatal(saved, e)
	}
	token, _ := s.appInstallToken(id)
	if code := request(token, `{"revision":1,"policy":{}}`); code == 200 {
		t.Fatal("app token changed host priority")
	}
	t.Setenv("APTEVA_RESOURCE_CEILINGS", `{"cpu_weight":250}`)
	if code := request(key, `{"revision":1,"policy":{"process":{"cpu_weight":300}}}`); code != 400 {
		t.Fatal("host ceiling bypassed", code)
	}
	if code := request(key, `{"revision":1,"policy":{}}`); code != 200 {
		t.Fatal("reset failed", code)
	}
	saved, e = m.resourcePolicy(id)
	if e != nil || saved.Revision != 2 || !saved.Policy.Process.Empty() {
		t.Fatal(saved, e)
	}
}
