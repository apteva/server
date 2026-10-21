package main

import (
	"context"
	"errors"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

func TestTelemetrySharedHistoryFiltersAndCancellation(t *testing.T) {
	s, _ := telemetryReadFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	events := []TelemetryEvent{
		{ID: "thought", AgentID: 41, ThreadID: "chat-1", Type: "thought", Time: at},
		{ID: "tool", AgentID: 41, ThreadID: "chat-1", Type: "tool.call", Time: at},
		{ID: "underscore", AgentID: 41, ThreadID: "chat-1", Type: "tool_call.start", Time: at},
		{ID: "different", AgentID: 41, ThreadID: "chat-1", Type: "toolXcall.start", Time: at},
		{ID: "percent", AgentID: 41, ThreadID: "chat-1", Type: "percent%tool.done", Time: at},
		{ID: "unrelated", AgentID: 41, ThreadID: "chat-1", Type: "percentANYtool.done", Time: at},
		{ID: "other-thread", AgentID: 41, ThreadID: "main", Type: "tool.call", Time: at},
		{ID: "other-agent", AgentID: 42, ThreadID: "chat-1", Type: "tool.call", Time: at},
	}
	if err := s.store.InsertTelemetry(events); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		filter string
		want   string
	}{{"thought", "[thought]"}, {"tool", "[tool]"}, {"tool_call", "[underscore]"}, {"percent%tool", "[percent]"}} {
		t.Run(tc.filter, func(t *testing.T) {
			stored, err := s.store.QueryTelemetry(41, tc.filter, at.Add(-time.Second), 100, "chat-1")
			if err != nil {
				t.Fatal(err)
			}
			q := sdk.TelemetryQuery{AgentID: 41, ThreadID: "chat-1", Events: []string{tc.filter}, Since: at.Add(-time.Second), Limit: 100, Projection: "metadata"}
			page, err := s.queryTelemetryPage(httptest.NewRequest("GET", "/telemetry", nil), q, map[int64]telemetryAgent{41: {project: "proj-1"}})
			if err != nil {
				t.Fatal(err)
			}
			ids := func(list []TelemetryEvent) string {
				out := []string{}
				for _, e := range list {
					out = append(out, e.ID)
				}
				sort.Strings(out)
				return fmt.Sprint(out)
			}
			live := []TelemetryEvent{}
			for _, e := range events {
				if e.AgentID == 41 && matchesTelemetry(q, e) {
					live = append(live, e)
				}
			}
			paged := []TelemetryEvent{}
			for _, e := range page.Events {
				paged = append(paged, TelemetryEvent{ID: e.ID})
			}
			if ids(stored) != tc.want || ids(paged) != tc.want || ids(live) != tc.want {
				t.Fatal("inconsistent history/live filters", ids(stored), ids(paged), ids(live))
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.store.QueryTelemetryContext(ctx, 41, "", time.Time{}, 100); !errors.Is(err, context.Canceled) {
		t.Fatal("history did not honor cancellation", err)
	}
	// Corrupt rows fail explicitly; the old reader silently ignored decoding errors.
	if _, err := s.store.db.Exec(`UPDATE telemetry SET time='invalid' WHERE id='thought'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.QueryTelemetry(41, "thought", time.Time{}, 100); err == nil {
		t.Fatal("invalid timestamp silently decoded")
	}
}

func TestTelemetryReplayCompatibilityUsesSharedIdentity(t *testing.T) {
	b := NewTelemetryBroadcaster()
	live := b.SubscribeAll()
	defer b.UnsubscribeAll(live)
	b.Broadcast([]TelemetryEvent{{ID: "first", AgentID: 41}, {AgentID: 41}})
	<-live
	generated := <-live
	if generated.ID == "" {
		t.Fatal("live event did not get a stable fallback identity")
	}
	old, reset := b.ReplaySince(1)
	modern, _, _, reason := b.replayTelemetry(b.replayEpoch, 1)
	if reset || reason != "" || len(old) != 1 || len(modern) != 1 || old[0].ID != generated.ID || modern[0].ID != generated.ID {
		t.Fatal("legacy and modern replay drifted")
	}
	if old, reset = b.ReplaySince(0); len(old) != 0 || reset {
		t.Fatal("legacy fresh subscription replayed old events")
	}
}
