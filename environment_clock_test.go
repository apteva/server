package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestEnvironmentClockAdvanceAndSnapshot(t *testing.T) {
	initial := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, err := newEnvironmentClock(&sdk.RuntimeClockSpec{Mode: "manual", InitialTime: &initial})
	if err != nil {
		t.Fatal(err)
	}
	if !clock.Now().Equal(initial) {
		t.Fatal("wrong initial time")
	}
	next := initial.Add(24 * time.Hour)
	state, err := clock.Advance(next)
	if err != nil || len(state.Advancements) != 1 || !state.CurrentTime.Equal(next) {
		t.Fatalf("advance: %+v %v", state, err)
	}
	if _, err := clock.Advance(initial); err == nil {
		t.Fatal("backward advance accepted")
	}
	if _, err := clock.Advance(next); err == nil {
		t.Fatal("duplicate advance accepted")
	}
	store := NewSnapshotStore(filepath.Join(t.TempDir(), "environments"))
	man, err := store.Capture(CaptureSpec{ID: "snap-clock", ProjectID: "p", Clock: &state})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(man.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := clockFromState(*loaded.Clock)
	if err != nil || !restored.Now().Equal(next) || len(restored.State().Advancements) != 1 {
		t.Fatalf("restored clock: %+v %v", loaded.Clock, err)
	}
	real, err := newEnvironmentClock(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := real.Advance(next); err == nil {
		t.Fatal("real clock advanced")
	}
}

func TestTimedHTTPAndIntegrationFixtures(t *testing.T) {
	initial := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	change := initial.Add(time.Hour)
	clock, _ := newEnvironmentClock(&sdk.RuntimeClockSpec{Mode: "manual", InitialTime: &initial})
	mocks := []HTTPMock{
		{Host: "api.example.com", Path: "/status", Status: 200, Body: json.RawMessage(`{"phase":"later"}`), AvailableAt: &change},
		{Host: "api.example.com", Path: "/status", Status: 200, Body: json.RawMessage(`{"phase":"early"}`), ExpiresAt: &change},
	}
	fixtures := []IntegrationFixture{
		{App: "calendar", Tool: "list", Status: 200, Data: "later", AvailableAt: &change},
		{App: "calendar", Tool: "list", Status: 200, Data: "early", ExpiresAt: &change},
	}
	if err := validateTimedFixtures(mocks, fixtures); err != nil {
		t.Fatal(err)
	}
	edge, err := startEnvironmentEdge(SandboxPolicy{Mocks: mocks}, EdgeBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer edge.Stop()
	edge.clock = clock
	client := proxiedClient(t, edge)
	remove := RegisterEnvironmentInterceptor("clock-test", fixtures, clock)
	defer remove()
	check := func(want string) {
		resp, err := client.Get("http://api.example.com/status")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !containsPhase(body, want) {
			t.Fatalf("HTTP at %s: %d %s", clock.Now(), resp.StatusCode, body)
		}
		v, _ := environmentInterceptors.Load("clock-test")
		result, handled := v.(integrationInterceptorFn)(&AppTemplate{Slug: "calendar"}, &AppToolDef{Name: "list"}, nil)
		if !handled || result.Data != want {
			t.Fatalf("integration at %s: %+v", clock.Now(), result)
		}
	}
	check("early")
	if _, err := clock.Advance(change); err != nil {
		t.Fatal(err)
	}
	check("later")
	calls := edge.Calls()
	if len(calls) != 2 || !calls[0].LogicalTime.Equal(initial) || !calls[1].LogicalTime.Equal(change) {
		t.Fatalf("logical edge times: %+v", calls)
	}
	if err := validateTimedFixtures(append(mocks, mocks[0]), fixtures); err == nil {
		t.Fatal("overlap accepted")
	}
	legacy := []IntegrationFixture{{App: "calendar", Tool: "list", Data: "first"}, {App: "calendar", Tool: "list", Data: "last"}}
	removeLegacy := RegisterEnvironmentInterceptor("clock-legacy", legacy, clock)
	defer removeLegacy()
	v, _ := environmentInterceptors.Load("clock-legacy")
	result, handled := v.(integrationInterceptorFn)(&AppTemplate{Slug: "calendar"}, &AppToolDef{Name: "list"}, nil)
	if !handled || result.Data != "last" {
		t.Fatalf("legacy duplicate behavior changed: %+v", result)
	}
}

func containsPhase(body []byte, want string) bool {
	var got struct {
		Phase string `json:"phase"`
	}
	return json.Unmarshal(body, &got) == nil && got.Phase == want
}
