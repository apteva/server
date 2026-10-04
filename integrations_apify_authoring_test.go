package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"
)

func apifyAuthoringCatalog(t *testing.T) *AppTemplate {
	t.Helper()
	raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/apify.json")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../integrations/src/apps/apify.json")
	if err != nil || !bytes.Equal(raw, source) {
		t.Fatalf("Apify embedded catalog differs from source: %v", err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	return &app
}

func TestApifyAuthoringCatalogRequests(t *testing.T) {
	source := map[string]any{
		"sourceType":  "SOURCE_FILES",
		"sourceFiles": []any{map[string]any{"name": "src/main.js", "format": "TEXT", "content": "await Actor.init();\nawait Actor.exit();"}},
		"envVars":     []any{map[string]any{"name": "SERVICE_SECRET", "value": "test-secret", "isSecret": true}},
		"buildTag":    "latest",
	}
	version := map[string]any{"versionNumber": "0.1"}
	for k, v := range source {
		version[k] = v
	}
	actor := map[string]any{"name": "example-actor", "isPublic": false, "versions": []any{version}}
	pricing := map[string]any{
		"isPublic":     true,
		"title":        "Example Actor",
		"taggedBuilds": map[string]any{"latest": map[string]any{"buildId": "build-123"}, "beta": nil},
		"pricingInfos": []any{map[string]any{
			"pricingModel": "PAY_PER_EVENT", "apifyMarginPercentage": 0.2,
			"createdAt": "2026-10-01T00:00:00Z", "startedAt": "2026-10-17T00:00:00Z",
			"pricingPerEvent": map[string]any{"actorChargeEvents": map[string]any{
				"apify-default-dataset-item": map[string]any{"eventTitle": "Result", "eventDescription": "One result", "eventPriceUsd": 0.001},
			}},
		}},
	}
	for _, tc := range []struct {
		name, method, path string
		input, body        map[string]any
		query              url.Values
	}{
		{"create_actor", "POST", "/v2/actors", actor, actor, nil},
		{"create_actor_version", "POST", "/v2/actors/my-user~example-actor/versions", version, version, nil},
		{"update_actor_version", "PUT", "/v2/actors/my-user~example-actor/versions/0.1", version, source, nil},
		{"update_actor", "PUT", "/v2/actors/my-user~example-actor", pricing, pricing, nil},
		{"build_actor", "POST", "/v2/actors/my-user~example-actor/builds", map[string]any{"version": "0.1", "useCache": false, "betaPackages": true, "tag": "beta & latest", "waitForFinish": 60}, nil, url.Values{"version": {"0.1"}, "useCache": {"false"}, "betaPackages": {"true"}, "tag": {"beta & latest"}, "waitForFinish": {"60"}}},
		{"abort_build", "POST", "/v2/actor-builds/build-123/abort", map[string]any{"buildId": "build-123"}, nil, nil},
		{"get_build", "GET", "/v2/actor-builds/build-123", map[string]any{"buildId": "build-123", "waitForFinish": 60}, nil, url.Values{"waitForFinish": {"60"}}},
		{"list_actor_builds", "GET", "/v2/actors/my-user~example-actor/builds", map[string]any{"offset": 1000, "limit": 1000, "desc": true}, nil, url.Values{"offset": {"1000"}, "limit": {"1000"}, "desc": {"true"}}},
		{"validate_actor_input", "POST", "/v2/actors/my-user~example-actor/validate-input", map[string]any{"build": "beta", "input": map[string]any{"url": "https://example.com?a=1&b=2"}}, map[string]any{"url": "https://example.com?a=1&b=2"}, url.Values{"build": {"beta"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := apifyAuthoringCatalog(t)
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == tc.name {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatalf("missing tool: %s", tc.name)
			}
			response := `{"data":{"id":"build-123","status":"RUNNING","usageTotalUsd":0.01,"pricingInfos":[{"pricingModel":"PAY_PER_EVENT","startedAt":"2026-10-17T00:00:00Z"}]}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.Query().Encode() != tc.query.Encode() {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer test-apify-token" {
					t.Error("lost API token authentication")
				}
				raw, _ := io.ReadAll(r.Body)
				if tc.body == nil {
					if len(raw) != 0 {
						t.Errorf("body must be empty: %s", raw)
					}
				} else {
					var body any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
					}
					want, _ := json.Marshal(tc.body)
					var expected any
					_ = json.Unmarshal(want, &expected)
					if !reflect.DeepEqual(body, expected) {
						t.Errorf("wrong JSON body: %s != %s", raw, want)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, response)
			}))
			defer srv.Close()
			app.BaseURL = srv.URL + "/v2"
			input := map[string]any{"actorId": "my-user~example-actor"}
			for k, v := range tc.input {
				input[k] = v
			}
			// Only supply actorId when the selected route actually declares it.
			if tc.name == "create_actor" || tc.name == "abort_build" || tc.name == "get_build" {
				delete(input, "actorId")
			}
			result, err := executeIntegrationTool(app, tool, map[string]string{"apiToken": "test-apify-token"}, input, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %v %#v", err, result)
			}
			var expected any
			_ = json.Unmarshal([]byte(response), &expected)
			want, _ := json.Marshal(expected)
			got, _ := json.Marshal(result.Data)
			if !bytes.Equal(got, want) {
				t.Fatalf("lost native build/pricing response: %#v", result.Data)
			}
		})
	}
}
