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
		if r.URL.Path != "/mcp" || r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("incorrect SDK MCP endpoint or authentication")
			w.WriteHeader(404)
			return
		}
		var body struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Method != "tools/call" || body.Params.Arguments["_project_id"] != "project-a" {
			t.Error("missing SDK method or authenticated project scope")
			w.WriteHeader(400)
			return
		}
		reply := func(value any) {
			data, _ := json.Marshal(value)
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": "runtime-dns", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(data)}}}})
		}
		if body.Params.Name == "domain_records_list" {
			reply(map[string]any{"records": []any{map[string]any{"id": "owned-id", "name": "voice.example.com", "type": "A", "value": "192.0.2.1"}}})
			return
		}
		if body.Params.Name == "domain_records_delete" {
			called = body.Params.Arguments["record_id"] == "owned-id"
			reply(map[string]bool{"ok": true})
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

func TestManagedRuntimeDNSRejectsMCPFailuresAndMissingInventory(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"2.0","error":{"code":-32000,"message":"unavailable"}}`,
		`{"jsonrpc":"2.0","result":{"isError":true,"content":[{"type":"text","text":"unavailable"}]}}`,
		`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{}"}]}}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"records":[]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s, m, _, id, _ := runtimeTestSetup(t)
			dns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(raw))
			}))
			defer dns.Close()
			if _, err := s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=?,status='running' WHERE id=?`, dns.URL, id); err != nil {
				t.Fatal(err)
			}
			_, err := m.dnsTool(context.Background(), &runtimeIngressSpec{DNSInstallID: id, DNSProjectID: "project-a"}, "domain_records_list", map[string]any{"domain": "example.com"})
			if err == nil {
				t.Fatal("invalid response accepted as successful DNS inventory")
			}
		})
	}
}
