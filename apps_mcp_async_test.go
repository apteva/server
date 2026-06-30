package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

func TestAppMCPAsyncCreatesEphemeralSubscriptionsAndAugmentsResult(t *testing.T) {
	s := newTestServer(t)
	user, err := s.store.CreateUser("async@test.local", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.store.CreateAgent(user.ID, "renderer", "render", "autonomous", "{}", "proj-media")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	entry := &InstalledApp{
		InstallID:  44,
		AppName:    "media",
		ProjectID:  "proj-media",
		SidecarURL: "http://127.0.0.1:1",
		Manifest: sdk.Manifest{Provides: sdk.Provides{MCPTools: []sdk.MCPToolSpec{{
			Name:        "media_extract_reel",
			Description: "extract",
			AsyncResult: &sdk.AsyncResultSpec{
				IDField: "render_id",
				Notify: &sdk.AsyncNotifySpec{
					Target:       "caller",
					Mode:         "once",
					Events:       []string{"render.completed", "render.failed", "render.cancelled"},
					Match:        map[string]string{"render_id": "$result.render_id"},
					ExpiresAfter: "24h",
				},
			},
		}}}},
	}
	req := &appMCPAsyncRequest{
		ToolName: "media_extract_reel",
		Spec:     entry.Manifest.Provides.MCPTools[0].AsyncResult,
		AgentID:  agent.ID,
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"render_id\":123,\"status\":\"pending\"}"}],"isError":false}}`)),
	}

	if err := s.maybeAugmentAppMCPAsyncResponse(entry, req, resp); err != nil {
		t.Fatalf("augment: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	raw := string(b)
	if !strings.Contains(raw, `_apteva_async`) {
		t.Fatalf("expected augmented async metadata, got %s", raw)
	}
	rows, err := s.store.ListAllAppEventSubscriptions()
	if err != nil {
		t.Fatalf("ListAllAppEventSubscriptions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 ephemeral subscription, got %d", len(rows))
	}
	for _, sub := range rows {
		if sub.Kind != "ephemeral" || !sub.DeleteOnMatch || sub.AgentID != agent.ID || sub.ProjectID != "proj-media" {
			t.Fatalf("unexpected sub: %+v", sub)
		}
		if sub.Slug != "media:*" {
			t.Fatalf("expected app-level wildcard slug, got %q", sub.Slug)
		}
		if got, want := strings.Join(sub.Events, ","), "render.completed,render.failed,render.cancelled"; got != want {
			t.Fatalf("events mismatch: got %q want %q", got, want)
		}
		if !subscriptionPayloadMatches(sub, json.RawMessage(`{"render_id":123}`)) {
			t.Fatalf("subscription did not match render_id 123: %+v", sub)
		}
		if subscriptionPayloadMatches(sub, json.RawMessage(`{"render_id":124}`)) {
			t.Fatalf("subscription matched wrong render_id: %+v", sub)
		}
		if sub.WaitGroupID == "" {
			t.Fatalf("expected wait group id")
		}
	}
}

func TestEphemeralWaitGroupCleanup(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.CreateEphemeralAppEventSubscription(1, 7, "render", "media:*", "", "", "proj", []string{"render.completed", "render.failed"}, `{"render_id":123}`, "wg-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create async row: %v", err)
	}
	if err := store.DeleteEphemeralSubscriptionWaitGroup("wg-1"); err != nil {
		t.Fatalf("delete wait group: %v", err)
	}
	rows, err := store.ListAllAppEventSubscriptions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected wait group deleted, got %d rows", len(rows))
	}
}

func TestAppEventSubscriptionTopicMatchingUsesEventsList(t *testing.T) {
	sub := &Subscription{Slug: "media:*", Events: []string{"render.completed", "render.failed"}}
	if !appEventSubscriptionTopicMatches(sub, "*", "render.completed") {
		t.Fatal("expected completed event to match events list")
	}
	if appEventSubscriptionTopicMatches(sub, "*", "render.progress") {
		t.Fatal("did not expect progress event to match events list")
	}
	legacy := &Subscription{Slug: "media:render.*"}
	if !appEventSubscriptionTopicMatches(legacy, "render.*", "render.completed") {
		t.Fatal("expected legacy slug pattern to still match")
	}
}
