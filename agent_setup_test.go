package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestAgentSetupInstalledManifestProjection(t *testing.T) {
	s, id := widgetSettingsFixture(t)
	m := sdk.Manifest{Name: "metrics", DisplayName: "Metrics", AgentSetup: &sdk.AgentSetup{Recommended: true, Reason: "Explore trends."}}
	raw, _ := json.Marshal(m)
	if _, err := s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/apps?project_id=p", nil)
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleListApps(rec, req)
	var rows []AppRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AgentSetup == nil || rows[0].AgentSetup.Reason != "Explore trends." {
		t.Fatalf("installed metadata lost: %s", rec.Body.String())
	}
}

func TestAgentSetupMarketplaceProjectionAndManifestPrecedence(t *testing.T) {
	s := newTestServer(t)
	var manifestCalls atomic.Int32
	manifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestCalls.Add(1)
		w.Write([]byte("schema: apteva-app/v1\nname: arbitrary-tools\nversion: 1.0.0\nruntime:\n  kind: static\n  static_dir: /tmp/arbitrary-ui\nagent_setup:\n  recommended: false\n  reason: Installed independently.\n"))
	}))
	defer manifest.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"apps": []RegistryEntry{
			{Name: "registry-only", AgentSetup: &sdk.AgentSetup{Recommended: true, Reason: "Useful helpers."}},
			{Name: "arbitrary-tools", ManifestURL: manifest.URL, AgentSetup: &sdk.AgentSetup{Recommended: true, Reason: "Stale recommendation."}},
			{Name: "invalid", AgentSetup: &sdk.AgentSetup{Recommended: true, Reason: strings.Repeat("a", 301)}},
		}})
	}))
	defer registry.Close()
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleMarketplace(rec, authedRequest(t, "GET", "/apps/marketplace?registry_url="+url.QueryEscape(registry.URL), "", nil))
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		var out struct {
			Apps []RegistryEntry `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Apps) != 3 || out.Apps[0].AgentSetup == nil || !out.Apps[0].AgentSetup.Recommended {
			t.Fatal("registry projection lost")
		}
		if out.Apps[1].AgentSetup == nil || out.Apps[1].AgentSetup.Recommended || out.Apps[1].AgentSetup.Reason != "Installed independently." {
			t.Fatal("manifest did not override registry")
		}
		if out.Apps[2].AgentSetup != nil {
			t.Fatal("invalid recommendation exposed")
		}
	}
	if manifestCalls.Load() != 1 {
		t.Fatalf("recommendations bypassed existing manifest cache: calls=%d", manifestCalls.Load())
	}
}
