package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func googleAdsAttributionCatalog(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/google-ads.json")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../integrations/src/apps/google-ads.json")
	if err != nil || string(source) != string(raw) {
		t.Fatalf("Google Ads embedded catalog differs from source: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func TestGoogleAdsAttributionGenericRequests(t *testing.T) {
	query := "SELECT campaign.id, segments.conversion_action, metrics.conversions_value FROM campaign WHERE segments.date DURING LAST_30_DAYS"
	for _, tc := range []struct {
		name, method, path, response string
		input, body                  map[string]any
	}{
		{"search", "POST", "/customers/123/googleAds:search", `{"results":[{"metrics":{"conversions":0.5,"conversionsValue":75}}],"nextPageToken":"next","fieldMask":"metrics.conversions","requestId":"request"}`, map[string]any{"customer_id": "123", "query": query, "page_token": "page", "validateOnly": false, "searchSettings": map[string]any{"returnSummaryRow": true}}, map[string]any{"query": query, "pageToken": "page", "validateOnly": false, "searchSettings": map[string]any{"returnSummaryRow": true}}},
		{"report_search", "POST", "/customers/123/googleAds:search", `{"results":[],"summaryRow":{"metrics":{"conversions":0}},"totalResultsCount":"0"}`, map[string]any{"customer_id": "123", "query": query, "pageToken": "page", "validateOnly": true}, map[string]any{"query": query, "pageToken": "page", "validateOnly": true}},
		{"search_stream", "POST", "/customers/123/googleAds:searchStream", `[{"results":[{"callView":{"resourceName":"customers/123/callViews/abc","callStatus":"MISSED","callDurationSeconds":"0"}}]},{"summaryRow":{"metrics":{"conversions":0.5}},"requestId":"summary"}]`, map[string]any{"customer_id": "123", "query": "SELECT call_view.call_status FROM call_view", "summaryRowSetting": "SUMMARY_ROW_WITH_RESULTS"}, map[string]any{"query": "SELECT call_view.call_status FROM call_view", "summaryRowSetting": "SUMMARY_ROW_WITH_RESULTS"}},
		{"get_field", "GET", "/googleAdsFields/asset.call_asset.phone_number", `{"name":"asset.call_asset.phone_number","selectable":true,"selectableWith":["campaign_asset","customer_asset"]}`, map[string]any{"field_name": "asset.call_asset.phone_number"}, nil},
		{"search_fields", "POST", "/googleAdsFields:search", `{"results":[{"name":"click_view.gclid","dataType":"STRING","selectable":true}],"nextPageToken":"next","totalResultsCount":"1"}`, map[string]any{"query": "SELECT name, selectable_with WHERE name LIKE 'click_view.%'", "pageToken": "page", "pageSize": 100.0}, map[string]any{"query": "SELECT name, selectable_with WHERE name LIKE 'click_view.%'", "pageToken": "page", "pageSize": 100.0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := googleAdsAttributionCatalog(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != "" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("developer-token") != "test-developer" || r.Header.Get("login-customer-id") != "1234567890" {
					t.Error("lost Google Ads credentials")
				}
				if tc.body != nil {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(body, tc.body) {
						t.Errorf("wrong JSON request: %#v != %#v", body, tc.body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatalf("missing generic tool %s", tc.name)
			}
			result, err := executeIntegrationTool(app, tool, map[string]string{"token": "test-token", "developer_token": "test-developer", "manager_customer_id": "1234567890"}, tc.input, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %v %#v", err, result)
			}
			var expected any
			if err := json.Unmarshal([]byte(tc.response), &expected); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(expected)
			got, _ := json.Marshal(result.Data)
			if string(want) != string(got) {
				t.Fatalf("lost native attribution, pagination or summary fields: %s != %s", got, want)
			}
		})
	}
}
