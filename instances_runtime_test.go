package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoreRuntimeInfoUsesShortCache(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		requests.Add(1)
		_, _ = w.Write([]byte(`{"core_version":"test","core_build_time":"now","uptime_seconds":7}`))
	}))
	defer ts.Close()
	parsed, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	im := &AgentManager{processes: map[int64]*runningAgent{
		42: {port: port, pid: 42, reattached: true},
	}}
	first, ok := im.CoreRuntimeInfo(42)
	if !ok || first.Version != "test" {
		t.Fatalf("first=%#v ok=%t", first, ok)
	}
	second, ok := im.CoreRuntimeInfo(42)
	if !ok || second != first {
		t.Fatalf("second=%#v ok=%t", second, ok)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("status requests=%d, want 1", got)
	}

	// Expired entries must refresh rather than serving stale runtime data.
	ri := im.processes[42]
	ri.runtimeMu.Lock()
	ri.runtimeAt = time.Now().Add(-coreRuntimeInfoCacheTTL - time.Second)
	ri.runtimeMu.Unlock()
	if _, ok := im.CoreRuntimeInfo(42); !ok {
		t.Fatal("expired refresh failed")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("refreshed status requests=%d, want 2 (%s)", got, fmt.Sprint(first))
	}
}
