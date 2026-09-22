package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

func TestUnwrapAppCallResult(t *testing.T) {
	got := string(unwrapAppCallResult([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)))
	if got != `{"ok":true}` {
		t.Fatalf("unwrapped=%q", got)
	}
	errorBody := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"missing"}}`)
	if got := string(unwrapAppCallResult(errorBody)); got != string(errorBody) {
		t.Fatalf("error envelope changed: %q", got)
	}
}

func TestCallback_AppCallBatchAuthorizesOnceAndPreservesResults(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	var calls, probes atomic.Int32
	targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sdk.InternalAppBatchPath {
			probes.Add(1)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var rpc struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Errorf("decode target request: %v", err)
		}
		if got := r.Header.Get(sdk.HeaderBoundCallerInstallID); got == "" {
			t.Error("missing bound caller header")
		}
		if r.Header.Get("X-Apteva-Operator-ID") != "" || r.Header.Get("X-Apteva-Subject-Type") != "" || r.Header.Get(sdk.HeaderTrustedPrincipal) != "" {
			t.Errorf("plain app call gained user authority: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"`+rpc.Params.Name+`"}]}}`)
	}))
	defer targetHTTP.Close()

	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "batch-target"}
	targetID := seedInstallWithBindings(t, s, "batch-target", targetManifest, nil)
	s.installedApps.Add(&InstalledApp{
		InstallID: targetID, AppName: "batch-target", ProjectID: "proj-1",
		Manifest: targetManifest, SidecarURL: targetHTTP.URL, Token: "target-token",
	})
	callerManifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "batch-caller",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "batch-target"}},
		},
	}
	callerID := seedInstallWithBindings(t, s, "batch-caller", callerManifest, map[string]any{"batch-target": targetID})
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/batch-target/batch", strings.NewReader(`{"calls":[{"id":"one","tool":"first","input":{}},{"id":"two","tool":"second","input":{}}]}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []sdk.AppCallResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].ID != "one" || response.Results[1].ID != "two" {
		t.Fatalf("results=%#v", response.Results)
	}
	if response.Results[0].Error != nil || response.Results[1].Error != nil {
		t.Fatalf("unexpected child error: %#v", response.Results)
	}
	if calls.Load() != 2 {
		t.Fatalf("target calls=%d, want 2", calls.Load())
	}
	// The unsupported capability is cached for this runtime. A second batch
	// goes straight to the legacy path instead of paying another probe.
	req = httptest.NewRequest(http.MethodPost, "/apps/callback/apps/batch-target/batch", strings.NewReader(`{"calls":[{"id":"one","tool":"first","input":{}},{"id":"two","tool":"second","input":{}}]}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if probes.Load() != 1 {
		t.Fatalf("unsupported target probes=%d, want 1", probes.Load())
	}
	if got := rec.Header().Get("Server-Timing"); !strings.Contains(got, "apteva_app_auth") || !strings.Contains(got, "apteva_app_batch_dispatch") {
		t.Fatalf("missing timing header=%q", got)
	}
}

func TestCallback_AppCallBatchUsesSingleTargetRequestAndRawResults(t *testing.T) {
	t.Setenv("APTEVA_APP_TOKEN", "target-token")
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	var requests atomic.Int32
	targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == sdk.InternalAppBatchPath {
			w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		requests.Add(1)
		if r.URL.Path != sdk.InternalAppBatchPath {
			t.Errorf("unexpected legacy path %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Authorization") != "Bearer target-token" || r.Header.Get(sdk.HeaderBoundCallerInstallID) == "" {
			t.Error("missing authenticated internal identity")
		}
		if r.Header.Get("X-Apteva-Operator-ID") != "19" || r.Header.Get("X-Apteva-Project-ID") != "proj-1" {
			t.Errorf("operator headers=%v", r.Header)
		}
		principal, err := sdk.PrincipalFromRequest(r)
		if err != nil || principal != nil || r.Header.Get("X-Apteva-Subject-Type") != "" || r.Header.Get("X-Apteva-Subject-ID") != "" {
			t.Errorf("operator became application user: principal=%+v err=%v headers=%v", principal, err, r.Header)
		}
		var request sdk.InternalAppBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Calls) == 0 || (len(request.Calls) == 3 && !strings.Contains(string(request.Calls[0].Input), `9007199254740993`)) {
			t.Fatalf("raw calls=%+v", request.Calls)
		}
		w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
		w.Header().Set("Server-Timing", "apteva_target_handler;dur=1.2, untrusted;dur=999")
		results := make([]sdk.AppCallResult, len(request.Calls))
		for i, call := range request.Calls {
			results[i] = sdk.AppCallResult{ID: call.ID, Status: 200, Format: "json", Result: call.Input}
			if call.Tool == "b" {
				results[i] = sdk.AppCallResult{ID: call.ID, Status: 200, Error: &sdk.AppCallError{Code: -32000, Message: "failed"}}
			}
		}
		_ = json.NewEncoder(w).Encode(sdk.InternalAppBatchResponse{Results: results})
	}))
	defer targetHTTP.Close()
	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "fast-target"}
	targetID := seedInstallWithBindings(t, s, "fast-target", targetManifest, nil)
	s.installedApps.Add(&InstalledApp{InstallID: targetID, AppName: "fast-target", ProjectID: "proj-1", Manifest: targetManifest, SidecarURL: targetHTTP.URL, Token: "target-token"})
	callerManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "fast-caller", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: "fast-target"}}}}
	callerID := seedInstallWithBindings(t, s, "fast-caller", callerManifest, map[string]any{"fast-target": targetID})
	ctx := context.WithValue(context.Background(), appCallbackPrincipalKey{}, appCallbackPrincipal{installID: callerID, userID: 19, userSession: true})
	body := `{"result_mode":"json","execution":"parallel_independent","concurrency":2,"calls":[{"id":"one","tool":"a","input":{"n":9007199254740993}},{"id":"two","tool":"b","input":{}},{"id":"three","tool":"c","input":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/fast-target/batch", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatalf("status=%d requests=%d body=%s", rec.Code, requests.Load(), rec.Body.String())
	}
	var response appCallBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 3 || response.Results[0].Format != "json" || response.Results[1].Error == nil || response.Results[2].ID != "three" {
		t.Fatalf("results=%+v", response.Results)
	}
	if timing := rec.Header().Get("Server-Timing"); !strings.Contains(timing, "apteva_target_handler;dur=1.2") || strings.Contains(timing, "untrusted") {
		t.Fatalf("timing=%q", timing)
	}
	// The optimized wire is always structured JSON internally, but callers
	// using the historical default still receive the exact MCP envelope shape.
	req = httptest.NewRequest(http.MethodPost, "/apps/callback/apps/fast-target/batch", strings.NewReader(`{"calls":[{"id":"legacy","tool":"a","input":{"ok":true}}]}`)).WithContext(ctx)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec = httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("legacy status=%d requests=%d body=%s", rec.Code, requests.Load(), rec.Body.String())
	}
	response = appCallBatchResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response.Results) != 1 || response.Results[0].Format != "" {
		t.Fatalf("legacy response=%+v err=%v", response, err)
	}
	var decoded map[string]any
	if err := response.Results[0].Decode(&decoded); err != nil || decoded["ok"] != true {
		t.Fatalf("legacy decode=%v err=%v", decoded, err)
	}
}

func TestCallback_AppCallBatchDoesNotRetryAmbiguousFastPath(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("legacy call must not run after an ambiguous fast-path response")
	})
	// Replace the helper's old-target server with a route that may have
	// executed work but omitted the protocol marker. Retrying could duplicate
	// an external side effect, so this must fail closed.
	target := s.installedApps.GetByNameAndProject("parallel-target", "proj-1")
	var calls atomic.Int32
	ambiguous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set(sdk.HeaderInternalAppBatchVersion, sdk.InternalAppBatchVersion)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer ambiguous.Close()
	next := *target
	next.SidecarURL = ambiguous.URL
	s.installedApps.Add(&next)
	w := runTestBatch(s, caller, context.Background(), `{"calls":[{"tool":"write"}]}`)
	if w.Code != http.StatusBadGateway || calls.Load() != 1 || !strings.Contains(w.Body.String(), "unknown internal batch protocol") {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
}

func TestCallback_AppCallBatchRejectsMixedProjects(t *testing.T) {
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`)
	}))
	defer targetHTTP.Close()
	targetManifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "batch-mixed-target"}
	targetID := seedInstallWithBindings(t, s, "batch-mixed-target", targetManifest, nil)
	s.installedApps.Add(&InstalledApp{InstallID: targetID, AppName: "batch-mixed-target", ProjectID: "proj-1", Manifest: targetManifest, SidecarURL: targetHTTP.URL, Token: "target-token"})
	manifest := sdk.Manifest{
		Schema: sdk.SchemaCurrent,
		Name:   "batch-caller-mixed",
		Requires: sdk.Requires{
			Permissions: []sdk.Permission{sdk.PermAppsCall},
			Apps:        []sdk.RequiredAppRef{{Name: "batch-mixed-target"}},
		},
	}
	callerID := seedInstallWithBindings(t, s, "batch-caller-mixed", manifest, map[string]any{"batch-mixed-target": targetID})
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/apps/batch-mixed-target/batch", strings.NewReader(`{"calls":[{"tool":"one","input":{"_project_id":"proj-1"}},{"tool":"two","input":{"_project_id":"proj-2"}}]}`))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(callerID))
	rec := httptest.NewRecorder()
	s.handleAppCallback(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "same project_id") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
