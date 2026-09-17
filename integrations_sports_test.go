package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sportsCatalog(t *testing.T, slug string) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + slug + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func TestEmbeddedSportsAPICatalogs(t *testing.T) {
	slugs := []string{"openligadb", "opendota", "squiggle", "jolpica-f1", "openf1", "college-football-data", "the-racing-api", "sportsgameodds", "football-data-org", "cricketdata", "balldontlie", "sportmonks-football", "pandascore", "sharpapi", "odds-api-io", "betfair-exchange", "datagolf", "api-tennis", "sportradar"}
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) {
			app := sportsCatalog(t, slug)
			if app.Slug != slug || len(app.Tools) == 0 {
				t.Fatalf("missing catalog content: %s", slug)
			}
			for _, tool := range app.Tools {
				if tool.Method != "GET" && !(slug == "betfair-exchange" && tool.Method == "POST") {
					t.Fatalf("unexpected write method: %s", tool.Name)
				}
			}
		})
	}
}

func TestSportsCricketDataOmitsEchoedKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		success bool
	}{
		{"success", 200, `{"status":"success","apikey":"test-key","data":{"id":"match-uuid"},"info":{"hitsToday":1}}`, true},
		{"semantic failure", 200, `{"status":"failure","apikey":"test-key","reason":"quota exceeded"}`, false},
		{"http failure", 401, `{"status":"failure","apikey":"test-key","reason":"expired key"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := sportsCatalog(t, "cricketdata")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/match_info" || r.URL.Query().Get("apikey") != "test-key" || r.URL.Query().Get("id") != "match-uuid" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == "get_match" {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatal("missing get_match")
			}
			tool.RateLimit = nil
			result, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "test-key"}, map[string]any{"id": "match-uuid"}, "")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result.Data)
			if result.Success != tc.success || strings.Contains(string(raw), "test-key") {
				t.Fatalf("unexpected result: %s success=%v", raw, result.Success)
			}
			if tc.success && !strings.Contains(string(raw), "hitsToday") {
				t.Fatalf("lost quota metadata: %s", raw)
			}
		})
	}
}
