package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCatalogMCPInboxAPI(t *testing.T) {
	raw, err := fs.ReadFile(integrationsCatalogEmbeddedFS, "inboxapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var app AppTemplate
	if err := json.Unmarshal(raw, &app); err != nil {
		t.Fatal(err)
	}
	var tool *AppToolDef
	for i := range app.Tools {
		if app.Tools[i].Name == "list_messages" {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("missing InboxAPI list_messages")
	}

	for _, tc := range []struct {
		name, reply string
		status      int
		data        map[string]any
	}{
		{"success", `"result":{"content":[{"type":"text","text":"{\"count\":1}"}]}`, 200, map[string]any{"count": float64(1)}},
		{"auth failure", `"error":{"code":-32001,"message":"Invalid saved-token"}`, 401, map[string]any{"error": "Invalid [redacted]"}},
		{"tool failure", `"result":{"content":[{"type":"text","text":"Quota exceeded"}],"isError":true}`, 422, map[string]any{"error": "Quota exceeded"}},
		{"redirect", "", http.StatusTemporaryRedirect, map[string]any{"error": "MCP endpoint returned HTTP 307"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RequestURI() != "/mcp" || r.Header.Get("Accept") != "application/json, text/event-stream" {
					t.Errorf("unexpected MCP request: %s %s, Accept: %q", r.Method, r.URL, r.Header.Get("Accept"))
				}
				var request jsonRPCRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				want := jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
					"name": "get_emails", "arguments": map[string]any{"token": "saved-token", "limit": float64(2)},
				}}
				if !reflect.DeepEqual(request, want) {
					t.Errorf("unexpected MCP call: %#v", request)
				}
				if tc.status == http.StatusTemporaryRedirect {
					http.Redirect(w, r, "/redirect-target", tc.status)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,%s}\n\n", tc.reply)
			}))
			defer server.Close()
			app.BaseURL = server.URL
			result, err := executeIntegrationTool(&app, tool, map[string]string{"token": "saved-token"}, map[string]any{
				"limit": 2, "token": "supplied-token", "domain": "other.example",
			}, "")
			if err != nil || result.Status != tc.status || result.Success != (tc.status == 200) || !reflect.DeepEqual(result.Data, tc.data) {
				t.Fatalf("unexpected MCP result: %#v, %v", result, err)
			}
		})
	}
}
