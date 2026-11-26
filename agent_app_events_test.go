package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// billingEvents gives the grant fixture's billing app declared events.
func billingEvents(t *testing.T, f *grantFixture) {
	t.Helper()
	entry := f.s.installedApps.Get(f.billing)
	entry.Manifest = sdk.Manifest{Name: "billing", DisplayName: "Billing", Provides: sdk.Provides{Publishes: []sdk.EventDecl{
		{Name: "invoice.paid", Description: "An invoice was paid", Payload: map[string]string{"id": "integer", "customer": "string"}},
		{Name: "invoice.created", Description: "An invoice was created"},
	}}}
}

// coreEvents runs a fake Core for the fixture's agent and records the
// messages it receives on /event.
type coreEvents struct {
	mu       sync.Mutex
	messages []map[string]string
	threads  []string
}

func (c *coreEvents) received() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]string(nil), c.messages...)
}

func runFakeCore(t *testing.T, s *Server, agentID int64, threads ...string) *coreEvents {
	t.Helper()
	c := &coreEvents{threads: threads}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/threads":
			rows := []map[string]any{}
			for _, id := range c.threads {
				rows = append(rows, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/event":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.mu.Lock()
			c.messages = append(c.messages, body)
			c.mu.Unlock()
		}
	}))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	_, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	s.agents.processes[agentID] = &runningAgent{port: port, reattached: true}
	return c
}

func toolText(t *testing.T, rec *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	var reply struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Result.Content) == 0 {
		t.Fatalf("decode tool reply: %v %s", err, rec.Body.String())
	}
	return reply.Result.Content[0].Text, reply.Result.IsError
}

func subscribeResult(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	text, isError := toolText(t, rec)
	if isError {
		t.Fatalf("subscribe failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode subscribe result: %v %s", err, text)
	}
	return out
}

func listedTool(t *testing.T, rec *httptest.ResponseRecorder, name string) map[string]any {
	t.Helper()
	var reply struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode tools/list: %v %s", err, rec.Body.String())
	}
	for _, tool := range reply.Result.Tools {
		if tool["name"] == name {
			return tool
		}
	}
	return nil
}

func TestAttachedAgentSeesSubscribeToolWithTheAppsEvents(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)

	tool := listedTool(t, f.call(t, true, "tools/list", "", nil), appEventsSubscribeTool)
	if tool == nil {
		t.Fatal("signed agent listing lacks the subscribe tool")
	}
	description, _ := tool["description"].(string)
	for _, want := range []string{"invoice.paid: An invoice was paid (fields: customer, id)", "invoice.created", "next matching event", "[app:billing:<event>]"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description lacks %q:\n%s", want, description)
		}
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	events, _ := props["events"].(map[string]any)
	items, _ := events["items"].(map[string]any)
	if enum, _ := items["enum"].([]any); len(enum) != 2 {
		t.Fatalf("events enum = %v", items["enum"])
	}
	// The app's own tools are still listed unchanged.
	if listedTool(t, f.call(t, true, "tools/list", "", nil), "invoices_list") == nil {
		t.Fatal("app tools missing")
	}

	if listedTool(t, f.call(t, false, "tools/list", "", nil), appEventsSubscribeTool) != nil {
		t.Fatal("unsigned caller got the subscribe tool")
	}
}

func TestAppWithoutEventsGetsNoSubscribeTool(t *testing.T) {
	f := newGrantFixture(t)
	if listedTool(t, f.call(t, true, "tools/list", "", nil), appEventsSubscribeTool) != nil {
		t.Fatal("app without publishes got the subscribe tool")
	}
	// The call reaches the app like any unknown tool.
	f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"events": []any{"x"}})
	if got := f.lastCall(t); got.tool != appEventsSubscribeTool {
		t.Fatalf("call did not reach the app: %+v", got)
	}
}

func TestAgentSubscribeDefaultsToNextEventInCallingThread(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)
	before := len(f.calls)

	rec := f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.paid"}, "filters": map[string]any{"customer": "acme"}, "_apteva_caller_thread": "chat-7",
	})
	out := subscribeResult(t, rec)
	if len(f.calls) != before {
		t.Fatal("the app saw a server-answered call")
	}
	if out["thread_id"] != "chat-7" || out["once"] != true || out["app"] != "billing" {
		t.Fatalf("result = %v", out)
	}
	if delivery, _ := out["delivery"].(string); !strings.Contains(delivery, "then this subscription ends") {
		t.Fatalf("delivery = %q", delivery)
	}
	id, _ := out["subscription_id"].(string)
	sub, err := f.s.store.GetSubscription(f.owner, id)
	if err != nil || sub == nil {
		t.Fatalf("stored subscription: %v", err)
	}
	if sub.Kind != agentSubscriptionKind || !sub.DeleteOnMatch || sub.ThreadID != "chat-7" || sub.ProjectID != f.home ||
		sub.SourceInstallID != f.billing || sub.Slug != "billing:*" || sub.AgentID != f.agent.ID || sub.NotifyAgent {
		t.Fatalf("subscription = %+v", sub)
	}
	at, err := parseTime(sub.ExpiresAt)
	if err != nil || at.Sub(time.Now()) < 59*time.Minute || at.Sub(time.Now()) > 61*time.Minute {
		t.Fatalf("expires_at = %q, want about an hour", sub.ExpiresAt)
	}
	if sub.Filters["customer"] != "acme" {
		t.Fatalf("filters = %v", sub.Filters)
	}

	// The same call again returns the same subscription.
	again := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.paid"}, "filters": map[string]any{"customer": "acme"}, "_apteva_caller_thread": "chat-7",
	}))
	if again["subscription_id"] != id || again["existing"] != true {
		t.Fatalf("repeat = %v, want existing %s", again, id)
	}
	// It shows on the Events page list.
	subs, err := f.s.store.ListSubscriptions(f.owner)
	if err != nil || len(subs) != 1 {
		t.Fatalf("listed subscriptions = %d err=%v", len(subs), err)
	}
}

func TestAgentSubscribeStandingAndCancel(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)

	out := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.created", "invoice.paid"}, "once": false, "expires_in_seconds": 86400,
	}))
	if out["thread_id"] != "main" || out["once"] != false {
		t.Fatalf("result = %v", out)
	}
	id, _ := out["subscription_id"].(string)
	sub, _ := f.s.store.GetSubscription(f.owner, id)
	if sub == nil || sub.DeleteOnMatch || !sub.NotifyAgent {
		t.Fatalf("standing subscription = %+v", sub)
	}

	// Another agent's id, or a made-up one, is refused.
	text, isError := toolText(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"cancel": "nope"}))
	if !isError || !strings.Contains(text, "no subscription nope") {
		t.Fatalf("bad cancel = %q", text)
	}
	canceled := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"cancel": id}))
	if canceled["canceled"] != id {
		t.Fatalf("cancel = %v", canceled)
	}
	if sub, _ := f.s.store.GetSubscription(f.owner, id); sub != nil {
		t.Fatal("canceled subscription still stored")
	}
}

func TestAgentSubscribeRefusals(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)

	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"undeclared event": {map[string]any{"events": []any{"invoice.voided"}}, `does not publish "invoice.voided"; its events are: invoice.paid, invoice.created`},
		"wildcard":         {map[string]any{"events": []any{"*"}}, `does not publish "*"`},
		"no events":        {map[string]any{}, "name at least one event"},
		"bad filter":       {map[string]any{"events": []any{"invoice.paid"}, "filters": map[string]any{"id": map[string]any{"near": 3}}}, "unknown operator"},
		"too long":         {map[string]any{"events": []any{"invoice.paid"}, "expires_in_seconds": 400 * 24 * 3600}, "at most"},
		"bad thread":       {map[string]any{"events": []any{"invoice.paid"}, "thread_id": ".hidden"}, "invalid thread_id"},
	} {
		text, isError := toolText(t, f.call(t, true, "tools/call", appEventsSubscribeTool, tc.args))
		if !isError || !strings.Contains(text, tc.want) {
			t.Fatalf("%s: got %q (error=%v), want %q", name, text, isError, tc.want)
		}
	}

	// A detached agent keeps the listing (Core lists before saving the
	// attachment) but cannot subscribe.
	if _, err := f.s.store.db.Exec(`DELETE FROM app_agent_bindings WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	text, isError := toolText(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"events": []any{"invoice.paid"}}))
	if !isError || !strings.Contains(text, "not attached to billing") {
		t.Fatalf("detached = %q", text)
	}
}

func TestAgentSubscribeNamedThreadMustExist(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)
	runFakeCore(t, f.s, f.agent.ID, "main", "orders")

	text, isError := toolText(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"events": []any{"invoice.paid"}, "thread_id": "ghost"}))
	if !isError || !strings.Contains(text, "thread ghost does not exist") {
		t.Fatalf("missing thread = %q", text)
	}
	out := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{"events": []any{"invoice.paid"}, "thread_id": "orders"}))
	if out["thread_id"] != "orders" {
		t.Fatalf("result = %v", out)
	}
}

func billingEvent(f *grantFixture, topic, data string) AppEvent {
	return AppEvent{App: "billing", ProjectID: f.home, InstallID: f.billing, Topic: topic, Data: json.RawMessage(data)}
}

func TestOnceAgentSubscriptionDeliversOneEventThenEnds(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)
	core := runFakeCore(t, f.s, f.agent.ID, "main")
	out := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.paid"}, "filters": map[string]any{"customer": "acme"},
	}))
	id, _ := out["subscription_id"].(string)
	sub, _ := f.s.store.GetSubscription(f.owner, id)

	d := NewAppEventDispatcher(f.s)
	lane := &appEventLane{rows: []*Subscription{sub}}
	d.dispatch(lane, billingEvent(f, "invoice.paid", `{"id":1,"customer":"other"}`))
	d.dispatch(lane, billingEvent(f, "invoice.created", `{"id":2,"customer":"acme"}`))
	d.dispatch(lane, billingEvent(f, "invoice.paid", `{"id":3,"customer":"acme"}`))
	d.dispatch(lane, billingEvent(f, "invoice.paid", `{"id":4,"customer":"acme"}`))
	d.drainOutbox(context.Background())
	d.drainOutbox(context.Background())

	got := core.received()
	if len(got) != 1 || got[0]["thread_id"] != "main" || !strings.HasPrefix(got[0]["message"], `[app:billing:invoice.paid] {"id":3`) {
		t.Fatalf("delivered = %v, want only event 3 in main", got)
	}
	if left, _ := f.s.store.GetSubscription(f.owner, id); left != nil {
		t.Fatal("once subscription survived its event")
	}
	var queued int
	if err := f.s.store.db.QueryRow(`SELECT count(*) FROM app_subscription_outbox WHERE subscription_id=?`, id).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("outbox rows left = %d err=%v", queued, err)
	}
}

func TestAgentSubscriptionEndsWhenAppIsDetached(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)
	core := runFakeCore(t, f.s, f.agent.ID, "main")
	out := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.paid"}, "once": false,
	}))
	id, _ := out["subscription_id"].(string)
	sub, _ := f.s.store.GetSubscription(f.owner, id)
	if _, err := f.s.store.db.Exec(`UPDATE app_agent_bindings SET enabled=0 WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	d := NewAppEventDispatcher(f.s)
	d.dispatch(&appEventLane{rows: []*Subscription{sub}}, billingEvent(f, "invoice.paid", `{"id":1}`))
	d.drainOutbox(context.Background())
	if got := core.received(); len(got) != 0 {
		t.Fatalf("delivered after detach: %v", got)
	}
	if left, _ := f.s.store.GetSubscription(f.owner, id); left != nil {
		t.Fatal("subscription survived the detach")
	}
}

func TestAgentSubscriptionEndsWithItsThread(t *testing.T) {
	f := newGrantFixture(t)
	billingEvents(t, f)
	runFakeCore(t, f.s, f.agent.ID, "main", "chat-7")
	out := subscribeResult(t, f.call(t, true, "tools/call", appEventsSubscribeTool, map[string]any{
		"events": []any{"invoice.paid"}, "_apteva_caller_thread": "chat-7",
	}))
	id, _ := out["subscription_id"].(string)
	if err := f.s.store.DeleteAgentThreadScope(f.agent.ID, "chat-7"); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.s.store.GetSubscription(f.owner, id); left != nil {
		t.Fatal("subscription survived its thread")
	}
}

// Operator-made app-event subscriptions take the same once and expiry
// options, and once now ends them after one delivered event.
func TestOperatorAppEventSubscriptionOnceAndExpiry(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(userID, "watcher", "", "autonomous", "{}", "")
	if err != nil {
		t.Fatal(err)
	}
	core := runFakeCore(t, s, agent.ID, "main")
	raw, _ := json.Marshal(map[string]any{"name": "Paid", "slug": "billing:*", "events": []string{"invoice.paid"}, "agent_id": agent.ID,
		"source": "app_event", "once": true, "expires_in_seconds": 600})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(raw))
	req.Header.Set("X-User-ID", itoa64(userID))
	rec := httptest.NewRecorder()
	s.handleCreateSubscription(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	sub, err := s.store.GetSubscription(userID, created.ID)
	if err != nil || sub == nil || !sub.DeleteOnMatch || sub.ExpiresAt == "" {
		t.Fatalf("stored = %+v err=%v", sub, err)
	}

	raw, _ = json.Marshal(map[string]any{"name": "Bad", "slug": "billing:*", "agent_id": agent.ID, "source": "app_event", "expires_in_seconds": -5})
	req = httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(raw))
	req.Header.Set("X-User-ID", itoa64(userID))
	rec = httptest.NewRecorder()
	s.handleCreateSubscription(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad expiry: %d %s", rec.Code, rec.Body.String())
	}

	d := NewAppEventDispatcher(s)
	lane := &appEventLane{rows: []*Subscription{sub}}
	d.dispatch(lane, AppEvent{App: "billing", Topic: "invoice.paid", Data: json.RawMessage(`{"id":1}`)})
	d.dispatch(lane, AppEvent{App: "billing", Topic: "invoice.paid", Data: json.RawMessage(`{"id":2}`)})
	d.drainOutbox(context.Background())
	d.drainOutbox(context.Background())
	if got := core.received(); len(got) != 1 {
		t.Fatalf("delivered %d events, want 1: %v", len(got), got)
	}
	if left, _ := s.store.GetSubscription(userID, sub.ID); left != nil {
		t.Fatal("once subscription survived its event")
	}
}
