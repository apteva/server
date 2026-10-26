package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestQueryParamSerializationRequests(t *testing.T) {
	for _, method := range []string{"GET", "POST", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			var received url.Values
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = r.URL.Query()
				if method == "POST" {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			app := &AppTemplate{Slug: "test", BaseURL: srv.URL, Auth: AppAuthConfig{Types: []string{"oauth2"}}}
			tool := &AppToolDef{Name: "analytics", Method: method, Path: "/analytics", QueryParams: []string{"ids", "ranges"}, QueryParamAliases: map[string]string{"metrics": "metric_types"}, QueryParamSerialization: map[string]string{"metrics": "csv", "ids": "csv"}}
			input := map[string]any{"metrics": []any{"IMPRESSION", "PIN_CLICK", "OUTBOUND_CLICK", "SAVE"}, "ids": []string{"a&b", "c d"}, "ranges": []string{"a", "b"}, "extra": "body"}
			if _, err := executeIntegrationTool(app, tool, map[string]string{"access_token": "test"}, input, ""); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(received["metric_types"], []string{"IMPRESSION,PIN_CLICK,OUTBOUND_CLICK,SAVE"}) || !reflect.DeepEqual(received["ids"], []string{"a&b,c d"}) || !reflect.DeepEqual(received["ranges"], []string{"a", "b"}) {
				t.Fatal(received)
			}
			if method == "POST" && !reflect.DeepEqual(body, map[string]any{"extra": "body"}) {
				t.Fatal(body)
			}
		})
	}
}
func TestQueryParamSerializationImplicitGETAndValidation(t *testing.T) {
	q := url.Values{}
	for _, tc := range []struct {
		key   string
		value any
		mode  string
		bad   bool
	}{
		{"ids", []any{"1", nil, "2"}, "csv", false}, {"scalar", "one", "csv", false}, {"empty", []string{}, "csv", false}, {"nested", []any{map[string]any{"a": 1}}, "csv", true}, {"unknown", []string{"a"}, "unsupported", true},
	} {
		if err := addSerializedQueryValue(q, tc.key, tc.value, tc.mode); (err != nil) != tc.bad {
			t.Fatalf("%s: %v", tc.key, err)
		}
	}
	if q.Get("ids") != "1,2" || q.Get("scalar") != "one" || q.Has("empty") {
		t.Fatal(q)
	}
}
func TestPinterestCSVMetadata(t *testing.T) {
	catalog := NewAppCatalog()
	err := catalog.LoadFromDir("integrations-catalog")
	if err != nil {
		t.Fatal(err)
	}
	app := catalog.Get("pinterest")
	if app == nil {
		t.Fatal("pinterest missing")
	}
	for _, tool := range app.Tools {
		for _, name := range tool.QueryParams {
			if name == "metric_types" || name == "pin_ids" {
				if tool.QueryParamSerialization[name] != "csv" {
					t.Fatalf("%s.%s", tool.Name, name)
				}
			}
		}
	}
}
