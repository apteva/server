package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func selfServiceSportsTool(t *testing.T, app *AppTemplate, name string) *AppToolDef {
	t.Helper()
	for i := range app.Tools {
		if app.Tools[i].Name == name {
			return &app.Tools[i]
		}
	}
	t.Fatalf("missing %s.%s", app.Slug, name)
	return nil
}

func TestEmbeddedSelfServiceSportsCatalogs(t *testing.T) {
	slugs := []string{"mlb-stats", "nhl", "chess-com", "matchbook", "sharpsports", "api-sports-basketball", "api-sports-baseball", "api-sports-hockey", "api-sports-rugby", "api-sports-mma", "api-sports-formula-1", "api-sports-american-football"}
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) {
			app := sportsCatalog(t, slug)
			if app.Slug != slug || len(app.Tools) < 7 {
				t.Fatalf("incomplete embedded catalog: %s", slug)
			}
			for _, tool := range app.Tools {
				if slug != "matchbook" && slug != "sharpsports" && tool.Method != "GET" {
					t.Fatalf("unexpected sports data mutation: %s", tool.Name)
				}
			}
		})
	}
}

func TestSelfServiceSportsMatchbookSubmission(t *testing.T) {
	app := sportsCatalog(t, "matchbook")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/edge/rest/v2/offers" || r.Header.Get("session-token") != "test-session" {
			t.Errorf("unexpected offer request: %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		offers, ok := body["offers"].([]any)
		if !ok || len(offers) != 1 || body["odds-type"] != "DECIMAL" {
			t.Errorf("unexpected offers payload: %#v", body)
		} else if offer := offers[0].(map[string]any); offer["stake"] != 5.5 || offer["keep-in-play"] != true {
			t.Errorf("lost typed offer fields: %#v", offer)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"offers":[{"status":"failed","errors":["Insufficient balance"]}]}`)
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	tool := selfServiceSportsTool(t, app, "submit_offers")
	tool.RateLimit = nil
	result, err := executeIntegrationTool(app, tool, map[string]string{"session_token": "test-session"}, map[string]any{
		"odds-type": "DECIMAL",
		"offers":    []any{map[string]any{"runner-id": 123, "side": "back", "odds": 2.4, "stake": 5.5, "keep-in-play": true}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Data)
	if requests != 1 || string(raw) != `{"offers":[{"errors":["Insufficient balance"],"status":"failed"}]}` {
		t.Fatalf("lost per-offer status or retried submission: %d %s", requests, raw)
	}
}

func TestSelfServiceSportsSharpSportsCredentialRouting(t *testing.T) {
	app := sportsCatalog(t, "sharpsports")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Token test-private"
		if r.URL.Path == "/books" {
			want = "Token test-public"
		} else if r.URL.Path != "/bettors/BETT_123/betSlips" || r.URL.Query().Get("pageNum") != "2" {
			t.Errorf("unexpected bettor request: %s", r.URL)
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("wrong credential for %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[],"next":"next-page"}`)
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	creds := map[string]string{"public_key": "test-public", "private_key": "test-private"}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"list_books", map[string]any{}},
		{"list_bet_slips_by_bettor", map[string]any{"id": "BETT_123", "pageNum": 2}},
	} {
		tool := selfServiceSportsTool(t, app, tc.name)
		tool.RateLimit = nil
		result, err := executeIntegrationTool(app, tool, creds, tc.input, "")
		if err != nil || !result.Success {
			t.Fatalf("%s failed: %v %#v", tc.name, err, result)
		}
	}
}
