package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestHumanHiringCatalogRequests(t *testing.T) {
	for _, tc := range []struct {
		slug, tool, method, path, query string
		input, body                     map[string]any
	}{
		{"hire-a-human", "create_task", "POST", "/api-tasks", "", map[string]any{"task": map[string]any{"title": "Phone outreach", "providerSpecific": map[string]any{"enabled": false, "amount": 0}}}, map[string]any{"title": "Phone outreach", "providerSpecific": map[string]any{"enabled": false, "amount": 0}}},
		{"rentahuman", "unprefer_human", "DELETE", "/humans/preferred", "humanId=human-1", map[string]any{"humanId": "human-1"}, nil},
		{"rentahuman", "release_payment", "POST", "/escrow/esc-1/release", "", map[string]any{"escrowId": "esc-1", "applicationId": "app-1", "acknowledgeRelease": true}, map[string]any{"applicationId": "app-1", "acknowledgeRelease": true}},
	} {
		t.Run(tc.slug+"/"+tc.tool, func(t *testing.T) {
			raw, err := integrationsCatalogFS.ReadFile("integrations-catalog/" + tc.slug + ".json")
			if err != nil {
				t.Fatal(err)
			}
			source, err := os.ReadFile("../integrations/src/apps/" + tc.slug + ".json")
			if err != nil || !bytes.Equal(raw, source) {
				t.Fatalf("embedded catalog differs from source: %v", err)
			}
			var app AppTemplate
			if err := json.Unmarshal(raw, &app); err != nil {
				t.Fatal(err)
			}
			var tool *AppToolDef
			for i := range app.Tools {
				if app.Tools[i].Name == "start_conversation" {
					t.Fatal("retired direct messaging must not be advertised")
				}
				if app.Tools[i].Name == tc.tool {
					tool = &app.Tools[i]
				}
			}
			if tool == nil {
				t.Fatalf("missing tool %s", tc.tool)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("X-API-Key") != "test-human-key" {
					t.Error("missing provider API key")
				}
				data, _ := io.ReadAll(r.Body)
				if tc.body == nil {
					if len(data) != 0 {
						t.Errorf("unexpected request body: %s", data)
					}
				} else {
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("missing JSON content type")
					}
					var got, want any
					if err := json.Unmarshal(data, &got); err != nil {
						t.Error(err)
					}
					expected, _ := json.Marshal(tc.body)
					_ = json.Unmarshal(expected, &want)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("wrong body: %s", data)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"success":true,"paymentReleased":true,"task":{"id":"task-1"}}`)
			}))
			defer server.Close()
			app.BaseURL = server.URL
			result, err := executeIntegrationTool(&app, tool, map[string]string{"api_key": "test-human-key"}, tc.input, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %v %#v", err, result)
			}
			data, _ := json.Marshal(result.Data)
			if !bytes.Contains(data, []byte(`"paymentReleased":true`)) || !bytes.Contains(data, []byte(`"id":"task-1"`)) {
				t.Fatalf("lost provider response: %s", data)
			}
		})
	}
}
