package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const exactMCPRequest = `{
 "jsonrpc":"2.0","id":9007199254740993,"method":"tools/call",
 "params":{"name":"rows_insert","arguments":{
  "_project_id":"spoofed","_apteva_caller_thread":"chat-room-a","_apteva_tool_call_id":"call-precision",
  "id":9223372036854775807,
  "rows":[{"payload":{"id":9007199254740993,"unsigned":18446744073709551615,
    "decimal":0.12345678901234567890123456789,"exponent":1.234567890123456789e+30}}],
  "columns":[{"name":"payload","type":"json","default":{"id":9007199254740993}}]
 }}}
`

func assertExactMCPNumbers(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var rpc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	args := rpc["params"].(map[string]any)["arguments"].(map[string]any)
	payload := args["rows"].([]any)[0].(map[string]any)["payload"].(map[string]any)
	defaults := args["columns"].([]any)[0].(map[string]any)["default"].(map[string]any)
	for _, test := range []struct {
		name string
		got  any
		want string
	}{
		{"rpc id", rpc["id"], "9007199254740993"},
		{"row id", args["id"], "9223372036854775807"},
		{"nested id", payload["id"], "9007199254740993"},
		{"unsigned", payload["unsigned"], "18446744073709551615"},
		{"decimal", payload["decimal"], "0.12345678901234567890123456789"},
		{"exponent", payload["exponent"], "1.234567890123456789e+30"},
		{"default", defaults["id"], "9007199254740993"},
	} {
		if test.got != json.Number(test.want) {
			t.Errorf("%s=%v (%T), want exact %s", test.name, test.got, test.got, test.want)
		}
	}
	return args
}

func TestMCPProxyRewritersPreserveExactNumbers(t *testing.T) {
	for _, mode := range []string{"project", "caller", "both"} {
		t.Run(mode, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/apps/tables/mcp", strings.NewReader(exactMCPRequest))
			r.Header.Set("X-Apteva-Caller-Agent", "42")
			if mode != "project" {
				if err := extractCallerThreadFromMCPRequest(r); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "caller" {
				if err := injectProjectIntoMCPRequest(r, "trusted-project"); err != nil {
					t.Fatal(err)
				}
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			args := assertExactMCPNumbers(t, body)
			if mode != "caller" && args["_project_id"] != "trusted-project" {
				t.Fatalf("project not replaced: %v", args["_project_id"])
			}
			if mode != "project" {
				if _, exists := args["_apteva_caller_thread"]; exists {
					t.Fatal("hidden caller argument leaked")
				}
				if _, exists := args["_apteva_tool_call_id"]; exists {
					t.Fatal("hidden tool-call argument leaked")
				}
				if r.Header.Get("X-Apteva-Caller-Thread") != "chat-room-a" || r.Header.Get("X-Apteva-Tool-Call-ID") != "call-precision" {
					t.Fatal("trusted caller headers lost")
				}
			}
			if r.ContentLength != int64(len(body)) || r.GetBody == nil {
				t.Fatal("rewritten request body metadata invalid")
			}
			replay, err := r.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			replayed, err := io.ReadAll(replay)
			if err != nil || !bytes.Equal(body, replayed) {
				t.Fatalf("replayed body differs: %v", err)
			}
		})
	}
}

func TestMCPProxyRewritersPreserveNonCallAndInvalidBodies(t *testing.T) {
	for _, body := range []string{
		"", " \n", "null", "[]", `"value"`, `{"method":`,
		exactMCPRequest + `{}`, exactMCPRequest + `invalid`,
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"tools/list"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/apps/tables/mcp", strings.NewReader(body))
		for _, rewrite := range []func(*http.Request) error{
			extractCallerThreadFromMCPRequest,
			func(r *http.Request) error { return injectProjectIntoMCPRequest(r, "trusted-project") },
		} {
			if err := rewrite(r); err != nil {
				t.Fatalf("rewrite %q: %v", body, err)
			}
		}
		actual, _ := io.ReadAll(r.Body)
		if string(actual) != body {
			t.Errorf("unexpected rewrite of non-call/invalid body: %q", body)
		}
	}
}

func TestAppProxyForwardsExactNumbers(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.installedApps = NewInstalledAppsRegistry()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer sidecar-test-token" {
			t.Errorf("incorrect upstream route or authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer sidecar.Close()
	s.installedApps.Add(&InstalledApp{InstallID: 900, AppName: "tables", ProjectID: "trusted-project", SidecarURL: sidecar.URL, Token: "sidecar-test-token"})
	r := httptest.NewRequest(http.MethodPost, "/apps/tables/mcp?project_id=trusted-project", strings.NewReader(exactMCPRequest))
	r.Header.Set("X-User-ID", "1")
	r.Header.Set("X-Apteva-Caller-Agent", "42")
	recorder := httptest.NewRecorder()
	s.handleAppProxy(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status=%d: %s", recorder.Code, recorder.Body.String())
	}
	args := assertExactMCPNumbers(t, recorder.Body.Bytes())
	if args["_project_id"] != "trusted-project" {
		t.Fatal("trusted project missing upstream")
	}
	if _, exists := args["_apteva_caller_thread"]; exists {
		t.Fatal("hidden caller argument leaked upstream")
	}
}
