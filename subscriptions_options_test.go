package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustFilters(t *testing.T, filters map[string]any) string {
	t.Helper()
	_, matchJSON, err := normalizeSubscriptionFilters(filters)
	if err != nil {
		t.Fatalf("normalizeSubscriptionFilters: %v", err)
	}
	return matchJSON
}

func TestSubscriptionFiltersMatchDottedPathsAndOperators(t *testing.T) {
	for _, tc := range []struct {
		name    string
		filters map[string]any
		payload string
		want    bool
	}{
		{"suffix on a nested field ignores case", map[string]any{"from.email": map[string]any{"suffix": "@acmecorp.com"}}, `{"from":{"email":"Pat@AcmeCorp.com"}}`, true},
		{"suffix rejects another domain", map[string]any{"from.email": map[string]any{"suffix": "@acmecorp.com"}}, `{"from":{"email":"pat@acmecorp.com.evil.test"}}`, false},
		{"path through an array of objects", map[string]any{"to.email": "ops@example.com"}, `{"to":[{"email":"a@example.com"},{"email":"ops@example.com"}]}`, true},
		{"path through an array without a match", map[string]any{"to.email": "ops@example.com"}, `{"to":[{"email":"a@example.com"}]}`, false},
		{"prefix", map[string]any{"subject": map[string]any{"prefix": "invoice"}}, `{"subject":"Invoice #12"}`, true},
		{"contains on an array field", map[string]any{"labels": map[string]any{"contains": "urgent"}}, `{"labels":["low","Very-Urgent"]}`, true},
		{"in matches one listed value", map[string]any{"status": map[string]any{"in": []any{"open", "pending"}}}, `{"status":"pending"}`, true},
		{"in rejects an unlisted value", map[string]any{"status": map[string]any{"in": []any{"open", "pending"}}}, `{"status":"closed"}`, false},
		{"in compares numbers exactly", map[string]any{"list_id": map[string]any{"in": []any{2.0, 5.0}}}, `{"list_id":5}`, true},
		{"text operators need text", map[string]any{"amount": map[string]any{"prefix": "1"}}, `{"amount":12}`, false},
		{"a top-level key with a dot wins over the path", map[string]any{"a.b": 1.0}, `{"a.b":1,"a":{"b":2}}`, true},
		{"missing path", map[string]any{"from.email": "x@y.z"}, `{"from":"x@y.z"}`, false},
		{"all filters must match", map[string]any{"event": "email.received", "from.email": map[string]any{"suffix": "@acmecorp.com"}}, `{"event":"email.sent","from":{"email":"a@acmecorp.com"}}`, false},
		{"non-object payload", map[string]any{"event": "x"}, `["x"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &Subscription{MatchJSON: mustFilters(t, tc.filters)}
			if got := subscriptionPayloadMatches(sub, json.RawMessage(tc.payload)); got != tc.want {
				t.Fatalf("subscriptionPayloadMatches(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

func TestSubscriptionFilterValidation(t *testing.T) {
	for name, filters := range map[string]map[string]any{
		"two operators":       {"a": map[string]any{"prefix": "x", "suffix": "y"}},
		"unknown operator":    {"a": map[string]any{"regex": ".*"}},
		"empty text":          {"a": map[string]any{"suffix": ""}},
		"non-text suffix":     {"a": map[string]any{"suffix": 3}},
		"empty in":            {"a": map[string]any{"in": []any{}}},
		"structured in value": {"a": map[string]any{"in": []any{map[string]any{"x": 1}}}},
		"array value":         {"a": []any{1, 2}},
		"long text":           {"a": map[string]any{"contains": strings.Repeat("x", maxSubscriptionFilterText+1)}},
	} {
		if _, _, err := normalizeSubscriptionFilters(filters); err == nil {
			t.Errorf("%s: accepted %v", name, filters)
		}
	}
	if _, _, err := normalizeSubscriptionFilters(map[string]any{"a": map[string]any{"in": []any{"x", 2.0, true, nil}}, "b.c": "d"}); err != nil {
		t.Fatalf("valid filters rejected: %v", err)
	}
}

func TestParseSubscriptionDeliveryOptions(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	opts, err := parseSubscriptionDeliveryOptions(map[string]any{
		"filters":            `{"from.email":{"suffix":"@acmecorp.com"}}`,
		"expires_in_seconds": float64(7 * 24 * 3600),
		"once":               true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if opts.MatchJSON != `{"from.email":{"suffix":"@acmecorp.com"}}` || !opts.DeleteOnMatch || !opts.ExpiresAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("options = %+v", opts)
	}
	if got := opts.expiresAtValue(); got != "2026-11-30 12:00:00" {
		t.Fatalf("stored expiry = %q", got)
	}

	opts, err = parseSubscriptionDeliveryOptions(map[string]any{"expires_at": "2026-11-24T09:30:00+01:00"}, now)
	if err != nil || !opts.ExpiresAt.Equal(time.Date(2026, 11, 24, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("expires_at: %+v, %v", opts, err)
	}

	if opts, err := parseSubscriptionDeliveryOptions(map[string]any{}, now); err != nil || opts != (subscriptionDeliveryOptions{}) {
		t.Fatalf("no options: %+v, %v", opts, err)
	}

	for name, args := range map[string]map[string]any{
		"both expiries":     {"expires_at": "2026-11-24T00:00:00Z", "expires_in_seconds": 60},
		"past expiry":       {"expires_at": "2026-11-22T00:00:00Z"},
		"bad expiry":        {"expires_at": "tomorrow"},
		"zero seconds":      {"expires_in_seconds": 0},
		"beyond a year":     {"expires_in_seconds": float64(400 * 24 * 3600)},
		"bad filters":       {"filters": map[string]any{"a": map[string]any{"regex": "x"}}},
		"filters not JSON":  {"filters": "from=acme"},
		"once not a bool":   {"once": 1},
		"expires_at beyond": {"expires_at": "2028-01-01T00:00:00Z"},
	} {
		if _, err := parseSubscriptionDeliveryOptions(args, now); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}

// coreRecorder is a fake Core /event endpoint that records delivered
// messages and can be told to fail.
type coreRecorder struct {
	mu       sync.Mutex
	messages []string
	fail     bool
}

func (c *coreRecorder) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		http.Error(w, "core down", http.StatusServiceUnavailable)
		return
	}
	c.messages = append(c.messages, body.Message)
	w.WriteHeader(http.StatusOK)
}

func (c *coreRecorder) delivered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.messages...)
}

func (c *coreRecorder) setFail(fail bool) {
	c.mu.Lock()
	c.fail = fail
	c.mu.Unlock()
}

func postSubscriptionWebhook(s *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleWebhook(rec, req)
	return rec
}

func subscriptionExists(t *testing.T, s *Server, id string) bool {
	t.Helper()
	sub, err := s.store.GetSubscription(1, id)
	return err == nil && sub != nil
}

func TestWebhookSubscriptionDeliversOnlyMatchingEvents(t *testing.T) {
	s := newTestServer(t)
	core := &coreRecorder{}
	a, _ := auditCore(t, s, core.handler)
	sub, err := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "acme mail", "mail", "", "hook-filtered", "", "", "", nil, false,
		subscriptionDeliveryOptions{MatchJSON: mustFilters(t, map[string]any{"from.email": map[string]any{"suffix": "@acmecorp.com"}})})
	if err != nil {
		t.Fatal(err)
	}

	rec := postSubscriptionWebhook(s, sub.WebhookPath, `{"from":{"email":"someone@other.test"},"subject":"skip"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"filtered"`) {
		t.Fatalf("non-matching event: %d %s", rec.Code, rec.Body.String())
	}
	rec = postSubscriptionWebhook(s, sub.WebhookPath, `{"from":{"email":"pat@acmecorp.com"},"subject":"deliver"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"delivered"`) {
		t.Fatalf("matching event: %d %s", rec.Code, rec.Body.String())
	}
	if got := core.delivered(); len(got) != 1 || !strings.Contains(got[0], "deliver") {
		t.Fatalf("delivered = %q, want only the matching event", got)
	}
	if !subscriptionExists(t, s, sub.ID) {
		t.Fatal("a filtered subscription without once must stay")
	}
}

func TestWebhookSubscriptionOnceDeliversOneEventThenIsRemoved(t *testing.T) {
	s := newTestServer(t)
	core := &coreRecorder{}
	a, _ := auditCore(t, s, core.handler)
	sub, err := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "first reply", "mail", "", "hook-once", "", "", "", nil, false,
		subscriptionDeliveryOptions{MatchJSON: mustFilters(t, map[string]any{"thread": "T1"}), DeleteOnMatch: true})
	if err != nil {
		t.Fatal(err)
	}

	// A non-matching event does not use up the subscription.
	postSubscriptionWebhook(s, sub.WebhookPath, `{"thread":"T2"}`)
	if !subscriptionExists(t, s, sub.ID) {
		t.Fatal("removed by an event that did not match")
	}

	// A failed delivery leaves it in place and enabled.
	core.setFail(true)
	if rec := postSubscriptionWebhook(s, sub.WebhookPath, `{"thread":"T1","n":1}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed delivery status = %d", rec.Code)
	}
	if got, err := s.store.GetSubscription(1, sub.ID); err != nil || !got.Enabled {
		t.Fatalf("after a failed delivery: %+v, %v", got, err)
	}

	core.setFail(false)
	if rec := postSubscriptionWebhook(s, sub.WebhookPath, `{"thread":"T1","n":2}`); rec.Code != http.StatusOK {
		t.Fatalf("delivery status = %d %s", rec.Code, rec.Body.String())
	}
	if subscriptionExists(t, s, sub.ID) {
		t.Fatal("once subscription remained after delivering its event")
	}
	if rec := postSubscriptionWebhook(s, sub.WebhookPath, `{"thread":"T1","n":3}`); rec.Code != http.StatusNotFound {
		t.Fatalf("event after removal: %d", rec.Code)
	}
	if got := core.delivered(); len(got) != 1 || !strings.Contains(got[0], `"n":2`) {
		t.Fatalf("delivered = %q, want exactly the one event", got)
	}
}

func TestWebhookSubscriptionOnceDeliversOnceUnderConcurrentEvents(t *testing.T) {
	s := newTestServer(t)
	core := &coreRecorder{}
	a, _ := auditCore(t, s, core.handler)
	sub, err := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "race", "mail", "", "hook-race", "", "", "", nil, false,
		subscriptionDeliveryOptions{DeleteOnMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postSubscriptionWebhook(s, sub.WebhookPath, `{"x":1}`)
		}()
	}
	wg.Wait()
	if got := core.delivered(); len(got) != 1 {
		t.Fatalf("delivered %d events, want 1", len(got))
	}
}

func setSubscriptionExpiry(t *testing.T, s *Server, id string, at time.Time) {
	t.Helper()
	if _, err := s.store.db.Exec(`UPDATE subscriptions SET expires_at = ? WHERE id = ?`, at.UTC().Format(subscriptionExpiryLayout), id); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredSubscriptionsAreRemoved(t *testing.T) {
	s := newTestServer(t)
	core := &coreRecorder{}
	a, _ := auditCore(t, s, core.handler)
	opts := subscriptionDeliveryOptions{ExpiresAt: time.Now().Add(time.Hour)}
	live, _ := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "live", "mail", "", "hook-live", "", "", "", nil, false, opts)
	hooked, _ := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "hooked", "mail", "", "hook-expired", "", "", "", nil, false, opts)
	swept, _ := s.store.CreateSubscriptionWithOptions(1, a.ID, 0, "swept", "mail", "", "hook-swept", "", "", "", nil, false, opts)
	plain, _ := s.store.CreateSubscription(1, a.ID, 0, "plain", "mail", "", "hook-plain", "", "", "", nil)

	if rec := postSubscriptionWebhook(s, live.WebhookPath, `{"x":1}`); rec.Code != http.StatusOK {
		t.Fatalf("before expiry: %d %s", rec.Code, rec.Body.String())
	}

	past := time.Now().Add(-time.Minute)
	setSubscriptionExpiry(t, s, hooked.ID, past)
	setSubscriptionExpiry(t, s, swept.ID, past)

	if rec := postSubscriptionWebhook(s, hooked.WebhookPath, `{"x":2}`); rec.Code != http.StatusGone {
		t.Fatalf("expired webhook status = %d", rec.Code)
	}
	if subscriptionExists(t, s, hooked.ID) {
		t.Fatal("an expired subscription that received an event remained")
	}

	s.retireExpiredSubscriptions()
	if subscriptionExists(t, s, swept.ID) {
		t.Fatal("the expiry sweep left an expired subscription")
	}
	if !subscriptionExists(t, s, live.ID) || !subscriptionExists(t, s, plain.ID) {
		t.Fatal("the sweep removed a subscription that had not expired")
	}
	if got := core.delivered(); len(got) != 1 {
		t.Fatalf("delivered = %q, want only the event before expiry", got)
	}
}

func TestSubscriptionListsShowDeliveryOptions(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	sub, err := s.store.CreateSubscriptionWithOptions(1, 7, 0, "acme", "mail", "", "hook-list", "", "", "", []string{"email.received"}, true,
		subscriptionDeliveryOptions{MatchJSON: mustFilters(t, map[string]any{"from.email": map[string]any{"suffix": "@acmecorp.com"}}), ExpiresAt: at, DeleteOnMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"filters":    map[string]any{"from.email": map[string]any{"suffix": "@acmecorp.com"}},
		"expires_at": "2030-01-02T03:04:05Z",
		"once":       true,
	}
	check := func(label string, got *Subscription) {
		t.Helper()
		summary, _ := json.Marshal(subscriptionDeliveryOptionsSummary(got))
		expected, _ := json.Marshal(want)
		if string(summary) != string(expected) {
			t.Fatalf("%s: options = %s, want %s", label, summary, expected)
		}
	}
	check("created", sub)
	subs, err := s.store.ListSubscriptions(1)
	if err != nil || len(subs) != 1 {
		t.Fatalf("list: %v %d", err, len(subs))
	}
	check("listed", &subs[0])
	forAgent, err := s.store.ListSubscriptionsForAgent(1, 7)
	if err != nil || len(forAgent) != 1 {
		t.Fatalf("list for agent: %v %d", err, len(forAgent))
	}
	check("listed for the agent", &forAgent[0])
	line := formatSubscriptionSummaryLine(&forAgent[0])
	for _, part := range []string{`filters: {"from.email":{"suffix":"@acmecorp.com"}}`, "expires: 2030-01-02T03:04:05Z", "once"} {
		if !strings.Contains(line, part) {
			t.Fatalf("agent notice line %q lacks %q", line, part)
		}
	}
}

// pollTestSetup registers a fake integration whose list tool returns
// items, a connection to it, and an agent whose Core records events.
func pollTestSetup(t *testing.T, items string) (*Server, *Agent, *coreRecorder, int64) {
	t.Helper()
	t.Setenv(allowPrivateIntegrationHostsEnv, "1")
	s := newTestServer(t)
	core := &coreRecorder{}
	a, _ := auditCore(t, s, core.handler)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, items)
	}))
	t.Cleanup(upstream.Close)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug: "pollmail", Name: "Poll Mail", BaseURL: upstream.URL,
		Auth:  AppAuthConfig{Headers: map[string]string{"Authorization": "Bearer {{token}}"}},
		Tools: []AppToolDef{{Name: "list_messages", Method: "GET", Path: "/messages"}},
	})
	encrypted, _ := Encrypt(s.secret, `{"token":"t"}`)
	conn, err := s.store.CreateConnection(1, "pollmail", "Poll Mail", "inbox", "bearer", encrypted, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, a, core, conn.ID
}

func createPollTestSubscription(t *testing.T, s *Server, agentID, connID int64, opts subscriptionDeliveryOptions) *Subscription {
	t.Helper()
	cfg, _ := json.Marshal(storedWebhookPollConfig{
		EventName: "email.received", AppSlug: "pollmail", Tool: "list_messages",
		IntervalSeconds: 60, ItemsPath: "messages", IDFields: []string{"id"}, EmitInitial: true,
	})
	sub, err := s.store.CreatePollSubscriptionWithOptions(1, agentID, connID, "acme mail", "pollmail", "", "", "", []string{"email.received"}, string(cfg), time.Now().Add(-time.Second), false, opts)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func runDuePoll(t *testing.T, s *Server, id string) {
	t.Helper()
	due, err := s.store.ListDuePollSubscriptions(time.Now().Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range due {
		if sub.ID == id {
			if err := NewPollingSubscriptionDispatcher(s).run(sub); err != nil {
				t.Fatalf("poll run: %v", err)
			}
			return
		}
	}
	t.Fatalf("subscription %s was not due", id)
}

const pollTestItems = `{"messages":[
	{"id":"1","from":"a@other.test"},
	{"id":"2","from":"b@AcmeCorp.com"},
	{"id":"3","from":"c@acmecorp.com"}]}`

func TestPollSubscriptionDeliversOnlyMatchingItems(t *testing.T) {
	s, a, core, connID := pollTestSetup(t, pollTestItems)
	sub := createPollTestSubscription(t, s, a.ID, connID,
		subscriptionDeliveryOptions{MatchJSON: mustFilters(t, map[string]any{"item.from": map[string]any{"suffix": "@acmecorp.com"}})})
	runDuePoll(t, s, sub.ID)

	got := core.delivered()
	if len(got) != 2 || !strings.Contains(got[0], `"id":"2"`) || !strings.Contains(got[1], `"id":"3"`) {
		t.Fatalf("delivered = %q, want items 2 and 3", got)
	}
	if !subscriptionExists(t, s, sub.ID) {
		t.Fatal("a filtered poll subscription without once must stay")
	}
}

func TestPollSubscriptionOnceDeliversFirstMatchThenIsRemoved(t *testing.T) {
	s, a, core, connID := pollTestSetup(t, pollTestItems)
	sub := createPollTestSubscription(t, s, a.ID, connID, subscriptionDeliveryOptions{
		MatchJSON:     mustFilters(t, map[string]any{"item.from": map[string]any{"suffix": "@acmecorp.com"}}),
		DeleteOnMatch: true,
	})

	core.setFail(true)
	due, _ := s.store.ListDuePollSubscriptions(time.Now().Add(time.Second), 10)
	if len(due) != 1 {
		t.Fatalf("due = %d", len(due))
	}
	_ = NewPollingSubscriptionDispatcher(s).run(due[0]) // fails to deliver
	if got, err := s.store.GetSubscription(1, sub.ID); err != nil || !got.Enabled {
		t.Fatalf("after a failed delivery: %+v, %v", got, err)
	}

	core.setFail(false)
	// The failure pushed next_run_at out; make it due again.
	if _, err := s.store.db.Exec(`UPDATE subscriptions SET next_run_at = '' WHERE id = ?`, sub.ID); err != nil {
		t.Fatal(err)
	}
	runDuePoll(t, s, sub.ID)
	got := core.delivered()
	if len(got) != 1 || !strings.Contains(got[0], `"id":"2"`) {
		t.Fatalf("delivered = %q, want only the first match", got)
	}
	if subscriptionExists(t, s, sub.ID) {
		t.Fatal("once poll subscription remained after delivering its event")
	}
}

func TestExpiredPollSubscriptionIsRemovedWithoutPolling(t *testing.T) {
	s, a, core, connID := pollTestSetup(t, pollTestItems)
	sub := createPollTestSubscription(t, s, a.ID, connID, subscriptionDeliveryOptions{ExpiresAt: time.Now().Add(time.Hour)})
	setSubscriptionExpiry(t, s, sub.ID, time.Now().Add(-time.Minute))
	runDuePoll(t, s, sub.ID)
	if len(core.delivered()) != 0 || subscriptionExists(t, s, sub.ID) {
		t.Fatalf("expired poll subscription: delivered=%q exists=%v", core.delivered(), subscriptionExists(t, s, sub.ID))
	}
}
