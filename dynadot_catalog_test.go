package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDynadotCatalogMarketplaceRequests(t *testing.T) {
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/dynadot.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, saleType, response string
		input                    map[string]any
		success                  bool
	}{
		{"set_for_sale", "marketplace", `{"SetForSaleResponse":{"ResponseCode":0,"Status":"Success"}}`, map[string]any{"domain": "example.com", "listing_type": "buy_now", "price": "1000.00"}, true},
		{"remove_for_sale", "not_for_sale", `{"SetForSaleResponse":{"ResponseCode":"0","Status":"success"}}`, map[string]any{"domain": "example.com"}, true},
		{"set_for_sale", "marketplace", `{"SetForSaleResponse":{"ResponseCode":-1,"Status":"error","Error":"Domain is not in your account"}}`, map[string]any{"domain": "example.com", "listing_type": "make_offer"}, false},
	} {
		t.Run(tc.name+"/"+tc.response, func(t *testing.T) {
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
					break
				}
			}
			if tool == nil {
				t.Fatalf("missing tool %s", tc.name)
			}
			seen := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				if r.Method != http.MethodGet || r.URL.Path != "/api3.json" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				q := r.URL.Query()
				for key, want := range map[string]string{"key": "test-key", "command": "set_for_sale", "domains": "example.com", "for_sale_type": tc.saleType} {
					if got := q.Get(key); got != want {
						t.Errorf("query %s = %q, want %q", key, got, want)
					}
				}
				for _, key := range []string{"listing_type", "price"} {
					want, _ := tc.input[key].(string)
					if q.Get(key) != want {
						t.Errorf("query %s = %q, want %q", key, q.Get(key), want)
					}
				}
				if q.Has("domain") || r.Header.Get("Authorization") != "" {
					t.Error("unexpected unaliased domain or bearer authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			result, err := executeIntegrationTool(&app, tool, map[string]string{"api_key": "test-key"}, tc.input, "")
			if err != nil {
				t.Fatal(err)
			}
			if !seen || result.Success != tc.success {
				t.Fatalf("request seen=%v, success=%v, want %v", seen, result.Success, tc.success)
			}
		})
	}
}
