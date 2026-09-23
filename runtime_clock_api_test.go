package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestRuntimeAPIManualClockSnapshotRestore(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "clock-owner", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	other := seedRuntimeAPIInstall(t, s, "clock-other", sdk.PermRuntimesManage, sdk.PermRuntimesRead)
	initial := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	until := initial.Add(time.Hour)
	created := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{
		ID: "rt-clock", Clock: &sdk.RuntimeClockSpec{Mode: "manual", InitialTime: &initial},
		HTTPMocks: []sdk.RuntimeHTTPMock{{Host: "api.example.com", Path: "/status", Status: 200, Body: json.RawMessage(`{"ok":true}`), ExpiresAt: &until}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	foreign := runtimeAPIRequest(t, s, other, http.MethodGet, "/apps/callback/runtimes/rt-clock/clock", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign read: %d", foreign.Code)
	}
	advanced := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-clock/clock", map[string]time.Time{"to": until})
	if advanced.Code != http.StatusOK {
		t.Fatalf("advance: %d %s", advanced.Code, advanced.Body.String())
	}
	var state sdk.RuntimeClockState
	if err := json.Unmarshal(advanced.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.CurrentTime.Equal(until) || len(state.Advancements) != 1 {
		t.Fatalf("state: %+v", state)
	}
	backward := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-clock/clock", map[string]time.Time{"to": initial})
	if backward.Code != http.StatusBadRequest {
		t.Fatalf("backward: %d", backward.Code)
	}
	snapshot := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes/rt-clock/snapshots", sdk.RuntimeSnapshotRequest{ID: "snap-clock-api"})
	if snapshot.Code != http.StatusCreated {
		t.Fatalf("snapshot: %d %s", snapshot.Code, snapshot.Body.String())
	}
	clone := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-restored-clock", SnapshotID: "snap-clock-api"})
	if clone.Code != http.StatusCreated {
		t.Fatalf("restore: %d %s", clone.Code, clone.Body.String())
	}
	var summary sdk.RuntimeSummary
	if err := json.Unmarshal(clone.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Clock.Mode != "manual" || !summary.Clock.CurrentTime.Equal(until) || len(summary.Clock.Advancements) != 1 {
		t.Fatalf("restored clock: %+v", summary.Clock)
	}
	live, _ := s.environments.Get("rt-restored-clock")
	if len(live.httpMocks) != 1 || live.httpMocks[0].ExpiresAt == nil || !live.httpMocks[0].ExpiresAt.Equal(until) {
		t.Fatalf("restored mocks: %+v", live.httpMocks)
	}
}

func TestRuntimeClockMCPIsReadOnlyAndScoped(t *testing.T) {
	s := newRuntimeAPITestServer(t)
	owner := seedRuntimeAPIInstall(t, s, "mcp-clock-owner", sdk.PermRuntimesManage)
	initial := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	created := runtimeAPIRequest(t, s, owner, http.MethodPost, "/apps/callback/runtimes", sdk.RuntimeCreateRequest{ID: "rt-mcp-clock", Clock: &sdk.RuntimeClockSpec{Mode: "manual", InitialTime: &initial}})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	runtime, _ := s.environments.Get("rt-mcp-clock")
	call := func(token, method, name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"name": name}})
		req := httptest.NewRequest(http.MethodPost, "/runtime-clock-mcp/rt-mcp-clock/"+token, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		s.handleRuntimeClockMCP(rec, req)
		return rec
	}
	if got := call("wrong", "tools/list", ""); got.Code != http.StatusNotFound {
		t.Fatalf("bad token: %d", got.Code)
	}
	listed := call(runtime.clockToken, "tools/list", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "environment_clock_get") {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
	}
	got := call(runtime.clockToken, "tools/call", "environment_clock_get")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), initial.Format(time.RFC3339)) {
		t.Fatalf("get: %d %s", got.Code, got.Body.String())
	}
	unknown := call(runtime.clockToken, "tools/call", "environment_clock_advance")
	if !strings.Contains(unknown.Body.String(), "unknown tool") {
		t.Fatalf("mutation exposed: %s", unknown.Body.String())
	}
}
