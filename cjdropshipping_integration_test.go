package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestCJDropshippingProductSearchUsesV2QueryParameters(t *testing.T) {
	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	app := catalog.Get("cjdropshipping")
	if app == nil {
		t.Fatal("CJ Dropshipping catalog missing")
	}
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "products_search" {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("CJ products_search tool missing")
	}
	if !matchesIntegrationRateLimitRetry(tool, &ExecuteResult{
		Status: http.StatusOK,
		Data:   map[string]any{"code": json.Number("1600200")},
	}) {
		t.Fatalf("CJ catalog rate-limit policy did not match provider code 1600200: %#v", tool.RateLimit)
	}

	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{"pageSize": 5, "content": []any{}},
		})
	}))
	defer server.Close()
	app.BaseURL = server.URL

	result, err := executeIntegrationTool(app, tool, map[string]string{
		"access_token":     "cj-query-test-token",
		"token_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, map[string]any{
		"productNameEn":  "wireless charger",
		"pageSize":       5,
		"startSellPrice": 5,
		"endSellPrice":   40,
	}, "")
	if err != nil {
		t.Fatalf("execute search: %v", err)
	}
	if !result.Success {
		t.Fatalf("search result=%+v", result)
	}

	want := map[string]string{
		"keyWord":        "wireless charger",
		"size":           "5",
		"startSellPrice": "5",
		"endSellPrice":   "40",
	}
	for name, expected := range want {
		if got := query.Get(name); got != expected {
			t.Fatalf("query %s=%q, want %q (all=%v)", name, got, expected, query)
		}
	}
	for _, obsolete := range []string{"productNameEn", "pageSize", "minPrice", "maxPrice"} {
		if got := query.Get(obsolete); got != "" {
			t.Fatalf("obsolete query %s leaked as %q (all=%v)", obsolete, got, query)
		}
	}
}

func TestIntegrationToolRateLimitRetriesHTTPAndSemanticLimits(t *testing.T) {
	tests := []struct {
		name        string
		firstStatus int
		firstCode   int
	}{
		{name: "http 429", firstStatus: http.StatusTooManyRequests, firstCode: 429},
		{name: "CJ code 1600200", firstStatus: http.StatusOK, firstCode: 1600200},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			requestTimes := make(chan time.Time, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				requestTimes <- time.Now()
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					w.WriteHeader(test.firstStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"code": test.firstCode, "result": false, "message": "rate limited",
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": true})
			}))
			defer server.Close()

			app := &AppTemplate{Slug: "rate-test-" + test.name, BaseURL: server.URL}
			tool := &AppToolDef{
				Name:   "search",
				Method: http.MethodGet,
				Path:   "/products",
				RateLimit: &ToolRateLimitDef{
					MinIntervalMS:   5,
					MaxRetries:      1,
					RetryStatuses:   []int{http.StatusTooManyRequests},
					RetryErrorCodes: []any{1600200},
				},
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			}
			result, err := executeIntegrationTool(app, tool, map[string]string{"api_key": "test"}, map[string]any{}, "")
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !result.Success || calls.Load() != 2 {
				t.Fatalf("result=%+v calls=%d, want success after one retry", result, calls.Load())
			}
			first, second := <-requestTimes, <-requestTimes
			if elapsed := second.Sub(first); elapsed < 4*time.Millisecond {
				t.Fatalf("retry spacing=%s, want at least 4ms", elapsed)
			}
		})
	}
}

func TestCJDropshippingShippingCalculatePostsCanonicalBody(t *testing.T) {
	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	app := catalog.Get("cjdropshipping")
	if app == nil {
		t.Fatal("CJ Dropshipping catalog missing")
	}
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "shipping_calculate" {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("CJ shipping_calculate tool missing")
	}
	if tool.Method != http.MethodPost {
		t.Fatalf("shipping_calculate method=%q, want POST", tool.Method)
	}

	var gotMethod string
	var gotQuery url.Values
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.Query()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    200,
			"result":  true,
			"success": true,
			"data": []any{map[string]any{
				"logisticName":  "CJPacket",
				"logisticPrice": 8.5,
				"logisticAging": "5-9",
			}},
		})
	}))
	defer server.Close()
	app.BaseURL = server.URL

	result, err := executeIntegrationTool(app, tool, map[string]string{
		"access_token":     "cj-shipping-post-test-token",
		"token_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, map[string]any{
		"startCountryCode": "CN",
		"endCountryCode":   "US",
		"zip":              "10001",
		"products": []any{map[string]any{
			"vid":      "cj-variant-1",
			"quantity": 3,
		}},
	}, "")
	if err != nil {
		t.Fatalf("execute shipping calculation: %v", err)
	}
	if !result.Success {
		t.Fatalf("shipping result=%+v", result)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("request method=%q, want POST", gotMethod)
	}
	if len(gotQuery) != 0 {
		t.Fatalf("shipping input leaked into query: %v", gotQuery)
	}
	if gotBody["startCountryCode"] != "CN" || gotBody["endCountryCode"] != "US" || gotBody["zip"] != "10001" {
		t.Fatalf("shipping body=%#v", gotBody)
	}
	products, ok := gotBody["products"].([]any)
	if !ok || len(products) != 1 {
		t.Fatalf("shipping products=%#v", gotBody["products"])
	}
	product, ok := products[0].(map[string]any)
	if !ok || product["vid"] != "cj-variant-1" || integrationRateLimitCode(product["quantity"]) != "3" {
		t.Fatalf("shipping product=%#v", products[0])
	}
}

func TestCJDropshippingShippingCalculateRejectsHTTP200ProviderFailure(t *testing.T) {
	catalog := NewAppCatalog()
	if err := catalog.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	app := catalog.Get("cjdropshipping")
	if app == nil {
		t.Fatal("CJ Dropshipping catalog missing")
	}
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "shipping_calculate" {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("CJ shipping_calculate tool missing")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    16900202,
			"result":  false,
			"success": false,
			"message": "Request method 'GET' not supported.",
			"data":    nil,
		})
	}))
	defer server.Close()
	app.BaseURL = server.URL

	result, err := executeIntegrationTool(app, tool, map[string]string{
		"access_token":     "cj-shipping-error-test-token",
		"token_expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, map[string]any{
		"startCountryCode": "CN",
		"endCountryCode":   "US",
		"products": []any{map[string]any{
			"vid":      "cj-variant-1",
			"quantity": 1,
		}},
	}, "")
	if err != nil {
		t.Fatalf("execute shipping calculation: %v", err)
	}
	if result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v, want semantic failure with HTTP 200", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data=%#v", result.Data)
	}
	if data["error"] != "upstream_api_error" || integrationRateLimitCode(data["code"]) != "16900202" {
		t.Fatalf("error data=%#v", data)
	}
	if data["message"] != "Request method 'GET' not supported." || data["failed_flag"] != "success" {
		t.Fatalf("error data=%#v", data)
	}
}
