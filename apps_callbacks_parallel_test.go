package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func batchTestServer(t *testing.T, handler http.HandlerFunc) (*Server, int64, int64) {
	t.Helper()
	s := newTestServer(t)
	s.installedApps = NewInstalledAppsRegistry()
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sdk.InternalAppBatchPath {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(downstream.Close)
	m := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "parallel-target"}
	target := seedInstallWithBindings(t, s, m.Name, m, nil)
	s.installedApps.Add(&InstalledApp{InstallID: target, AppName: m.Name, ProjectID: "proj-1", Manifest: m, SidecarURL: downstream.URL, Token: "target-token"})
	caller := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "parallel-caller", Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermAppsCall}, Apps: []sdk.RequiredAppRef{{Name: m.Name}}}}
	id := seedInstallWithBindings(t, s, caller.Name, caller, map[string]any{m.Name: target})
	return s, id, target
}

func runTestBatch(s *Server, caller int64, ctx context.Context, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/apps/callback/apps/parallel-target/batch", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Apteva-App-Install-ID", itoa(caller))
	w := httptest.NewRecorder()
	s.handleAppCallback(w, r)
	return w
}

func TestAppBatchBoundedParallelOrderingAndErrors(t *testing.T) {
	var active, peak atomic.Int32
	started := make(chan string, 4)
	release := make(chan struct{})
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		var req struct{ Params struct{ Name string } }
		_ = json.NewDecoder(r.Body).Decode(&req)
		started <- req.Params.Name
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		switch req.Params.Name {
		case "slow":
			time.Sleep(30 * time.Millisecond)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		case "error":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"failed"}}`)
		case "invalid":
			_, _ = io.WriteString(w, `not json`)
		case "overload":
			w.WriteHeader(429)
		}
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- runTestBatch(s, caller, context.Background(), `{"execution":"parallel_independent","concurrency":2,"calls":[{"id":"a","tool":"slow"},{"id":"b","tool":"error"},{"id":"c","tool":"invalid"},{"id":"d","tool":"overload"}]}`)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("parallel calls not started")
		}
	}
	select {
	case <-started:
		close(release)
		t.Fatal("exceeded concurrency limit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	w := <-done
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var out appCallBatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 || len(out.Results) != 4 {
		t.Fatalf("peak=%d response=%+v", peak.Load(), out)
	}
	if s.automaticRuntime().get().Snapshot()["active"].(int) != 0 {
		t.Fatal("batch leaked admission permits")
	}
	for i, id := range []string{"a", "b", "c", "d"} {
		if out.Results[i].ID != id {
			t.Fatal("results reordered")
		}
	}
	if out.Results[0].Error != nil || out.Results[1].Error.Code != -32000 || out.Results[2].Error == nil || out.Results[3].Status != 429 {
		t.Fatalf("results=%+v", out.Results)
	}
}

func TestAppBatchSequentialDefaultAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started, stopped := make(chan struct{}), make(chan struct{})
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("scheduled remaining child after cancellation")
			return
		}
		_, _ = io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- runTestBatch(s, caller, ctx, `{"calls":[{"tool":"first"},{"tool":"second"},{"tool":"third"}]}`)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("not started")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream did not cancel")
	}
	select {
	case w := <-done:
		if s.automaticRuntime().get().Snapshot()["active"].(int) != 0 {
			t.Fatal("cancellation leaked admission permits")
		}
		var out appCallBatchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 || len(out.Results) != 3 {
			t.Fatalf("calls=%d results=%+v", calls.Load(), out)
		}
		for _, r := range out.Results {
			if r.Error == nil || r.Status != 499 {
				t.Fatalf("result=%+v", r)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("batch did not cancel")
	}
}

func TestAppBatchDeadlineForwardedAndRuntimeRefreshed(t *testing.T) {
	var calls atomic.Int32
	deadline := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	s, caller, target := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(sdk.HeaderAppCallDeadline) != strconv.FormatInt(deadline.UnixMilli(), 10) {
			t.Error("deadline not forwarded")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if w := runTestBatch(s, caller, ctx, `{"calls":[{"tool":"first"}]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sdk.InternalAppBatchPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer rotated-token" {
			t.Error("stale runtime token")
		}
		_, _ = io.WriteString(w, `{"new":true}`)
	}))
	defer replacement.Close()
	old := s.installedApps.Get(target)
	next := *old
	next.SidecarURL = replacement.URL
	next.Token = "rotated-token"
	s.installedApps.Add(&next)
	w := runTestBatch(s, caller, ctx, `{"calls":[{"tool":"second"}]}`)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), `"new":true`) {
		t.Fatalf("stale runtime: %s", w.Body.String())
	}
}

func TestAppBatchNegotiatesStructuredResultsAndLegacyFallback(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sdk.HeaderAppResultFormat) != sdk.AppResultJSON {
			t.Error("missing negotiation")
		}
		var req struct{ Params struct{ Name string } }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Params.Name == "new" {
			w.Header().Set(sdk.HeaderAppResultFormat, sdk.AppResultJSON)
			_, _ = io.WriteString(w, `{"error":{"message":"data"},"ok":true}`)
		} else {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
		}
	})
	w := runTestBatch(s, caller, context.Background(), `{"result_mode":"json","calls":[{"tool":"new"},{"tool":"old"}]}`)
	var out appCallBatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Results[0].Format != "json" || out.Results[0].Error != nil || out.Results[1].Format != "" || out.Results[1].Error != nil {
		t.Fatalf("out=%+v", out)
	}
}

func TestAppBatchResourceLimits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		size, calls int
		outerError  bool
	}{
		{"child", maxAppCallResponseBytes + 1, 1, false},
		{"aggregate", 12 << 20, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `"`+strings.Repeat("x", tc.size)+`"`)
			})
			calls := make([]sdk.InternalAppCall, tc.calls)
			for i := range calls {
				calls[i].Tool = "large"
			}
			body, _ := json.Marshal(appCallBatchRequest{Calls: calls, AppBatchOptions: sdk.AppBatchOptions{Execution: sdk.ParallelIndependent, Concurrency: 2}})
			w := runTestBatch(s, caller, context.Background(), string(body))
			if tc.outerError {
				if w.Code != 502 {
					t.Fatalf("status=%d", w.Code)
				}
				return
			}
			var out appCallBatchResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Results) != 1 || out.Results[0].Error == nil || out.Results[0].Result != nil {
				t.Fatal("oversize child not rejected")
			}
		})
	}
}

func TestAppBatchRejectsInvalidOptionsWithoutDispatch(t *testing.T) {
	s, caller, _ := batchTestServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("dispatched invalid batch") })
	for _, options := range []string{`"execution":"unknown"`, `"execution":"parallel_independent","concurrency":9`, `"execution":"parallel_independent","concurrency":-1`, `"concurrency":2`, `"result_mode":"unknown"`} {
		w := runTestBatch(s, caller, context.Background(), `{`+options+`,"calls":[{"tool":"read"}]}`)
		if w.Code != 400 {
			t.Fatalf("options=%s status=%d", options, w.Code)
		}
	}
}
