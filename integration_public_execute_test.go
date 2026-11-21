package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func publicExecuteRequest(t *testing.T, s *Server, installID int64, slug, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/apps/callback/integrations/public/"+slug+"/execute", strings.NewReader(body))
	if installID != 0 {
		req.Header.Set("X-Apteva-App-Install-ID", itoa(installID))
	}
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	return rec
}

func TestCallback_PublicIntegrationExecute(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"markets":[{"ticker":"K1"}]}`))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	s.catalog = NewAppCatalog()
	// Keyless integration: every tool is public.
	s.catalog.Register(&AppTemplate{
		Slug: "kalshi", BaseURL: upstream.URL, Auth: AppAuthConfig{Types: []string{"none"}},
		Tools: []AppToolDef{{Name: "list_markets", Method: "GET", Path: "/markets", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}}}}},
	})
	// Credentialed integration with one public read.
	s.catalog.Register(&AppTemplate{
		Slug: "polymarket", BaseURL: upstream.URL,
		Auth: AppAuthConfig{Types: []string{"bearer"}, Headers: map[string]string{"Authorization": "Bearer {{token}}"},
			QueryParams: map[string]string{"key": "{{api_key}}"}},
		Tools: []AppToolDef{
			{Name: "list_markets", Method: "GET", Path: "/markets", Public: true},
			{Name: "create_order", Method: "POST", Path: "/order"},
		},
	})
	s.catalog.Register(&AppTemplate{
		Slug: "templated", BaseURL: "https://{{region}}.example.com", Auth: AppAuthConfig{Types: []string{"none"}},
		Tools: []AppToolDef{{Name: "read", Method: "GET", Path: "/x"}},
	})
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "intelligence",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermConnectionsExecute}}}
	installID := seedInstallWithBindings(t, s, "intelligence", manifest, map[string]any{})
	noPerm := seedInstallWithBindings(t, s, "other", sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "other"}, map[string]any{})

	t.Run("keyless integration runs without a connection", func(t *testing.T) {
		rec := publicExecuteRequest(t, s, installID, "kalshi", `{"tool":"list_markets","input":{"status":"open","_project_id":"p"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out ExecuteResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.Success || out.Status != 200 {
			t.Fatalf("result %+v err %v: %s", out, err, rec.Body.String())
		}
		last := seen[len(seen)-1]
		if last.URL.Path != "/markets" || last.URL.Query().Get("status") != "open" || last.URL.Query().Has("_project_id") {
			t.Fatalf("upstream got %s", last.URL.String())
		}
	})

	t.Run("public tool sends no credential placeholders", func(t *testing.T) {
		rec := publicExecuteRequest(t, s, installID, "polymarket", `{"tool":"list_markets","input":{}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		last := seen[len(seen)-1]
		if last.Header.Get("Authorization") != "" || strings.Contains(last.URL.RawQuery, "{{") || last.URL.Query().Has("key") {
			t.Fatalf("credential leaked into keyless call: auth=%q query=%q", last.Header.Get("Authorization"), last.URL.RawQuery)
		}
	})

	for _, tc := range []struct {
		name, slug, body string
		install          int64
		code             int
	}{
		{"credentialed tool refused", "polymarket", `{"tool":"create_order","input":{}}`, installID, http.StatusForbidden},
		{"unknown integration", "nope", `{"tool":"x","input":{}}`, installID, http.StatusNotFound},
		{"unknown tool", "kalshi", `{"tool":"nope","input":{}}`, installID, http.StatusNotFound},
		{"credential in URL refused", "templated", `{"tool":"read","input":{}}`, installID, http.StatusForbidden},
		{"permission required", "kalshi", `{"tool":"list_markets","input":{}}`, noPerm, http.StatusForbidden},
		{"install token required", "kalshi", `{"tool":"list_markets","input":{}}`, 0, http.StatusUnauthorized},
		{"tool required", "kalshi", `{"input":{}}`, installID, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen)
			rec := publicExecuteRequest(t, s, tc.install, tc.slug, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if len(seen) != before {
				t.Fatal("refused call reached upstream")
			}
		})
	}
}

func TestBuildAuthParamsDropUnresolvedPlaceholders(t *testing.T) {
	if q := buildAuthQuery(map[string]string{"key": "{{api_key}}"}, map[string]string{}); q != "" {
		t.Fatalf("query = %q", q)
	}
	if q := buildAuthQuery(map[string]string{"key": "{{api_key}}"}, map[string]string{"api_key": "k"}); q != "key=k" {
		t.Fatalf("query = %q", q)
	}
	if b := buildAuthBodyParams(map[string]string{"registrationkey": "{{api_key}}"}, nil); len(b) != 0 {
		t.Fatalf("body = %v", b)
	}
}
