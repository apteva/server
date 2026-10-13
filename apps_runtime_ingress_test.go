package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedRuntimeIngressReservationAndOwnedDNSCleanup(t *testing.T) {
	s, m, _, id, _ := runtimeTestSetup(t)
	called := false
	dns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Tool == "domain_records_list" {
			writeJSON(w, map[string]any{"records": []any{map[string]any{"id": "owned-id", "name": "voice.example.com", "type": "A", "value": "192.0.2.1"}}})
			return
		}
		if body.Tool == "domain_records_delete" {
			called = body.Args["record_id"] == "owned-id"
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		t.Error("unexpected DNS mutation")
		w.WriteHeader(400)
	}))
	defer dns.Close()
	if _, e := s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=?,status='running' WHERE id=?`, dns.URL, id); e != nil {
		t.Fatal(e)
	}
	state := runtimeIngressState{Spec: &runtimeIngressSpec{Hostname: "voice.example.com", Domain: "example.com", DNSInstallID: id, DNSProjectID: "project-a"}, RecordID: "owned-id", IPv4: "192.0.2.1", OwnDNS: true}
	if _, e := s.store.db.Exec(`INSERT INTO app_runtime_ingress(install_id,hostname,record_json) VALUES(?,?,?)`, id, state.Spec.Hostname, runtimeJSON(state)); e != nil {
		t.Fatal(e)
	}
	if _, e := m.dnsTool(context.Background(), state.Spec, "domain_records_list", map[string]any{"domain": "example.com"}); e != nil {
		t.Fatal(e)
	}
	if e := m.cleanupIngress(id); e != nil {
		t.Fatal(e)
	}
	if !called {
		t.Fatal("owned record was not deleted exactly")
	}
	for _, host := range []string{"https://example.com", "*.example.com", "evil\nexample.com", "example..com"} {
		if validateRuntimeIngressSpec(&runtimeIngressSpec{Hostname: host}) == nil {
			t.Fatal("unsafe hostname", host)
		}
	}
	if validateRuntimeIngressSpec(&runtimeIngressSpec{Hostname: "voice.example.com"}) != nil {
		t.Fatal("valid hostname rejected")
	}
	if !strings.Contains(runtimeJSON(state), "owned-id") {
		t.Fatal("ownership journal lost record ID")
	}
}
