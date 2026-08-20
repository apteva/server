package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppEventLaneCursorsUsesMinForReplayAndMaxForSequence(t *testing.T) {
	replaySince, sequenceFloor := appEventLaneCursors([]*Subscription{
		{LastSeqDelivered: 96},
		{LastSeqDelivered: 51},
	})
	if replaySince != 51 || sequenceFloor != 96 {
		t.Fatalf("cursors = replay %d floor %d, want replay 51 floor 96", replaySince, sequenceFloor)
	}
}

func TestAppEventDispatcherRestartSeedsPersistedSequence(t *testing.T) {
	s := newTestServer(t)
	userID := ensureTestAdmin(t, s)
	s.appBus = NewAppEventBus() // models the fresh, empty bus after restart

	agent, err := s.store.CreateAgent(userID, "event target", "handle events", "autonomous", `{}`, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.store.CreateAppEventSubscription(
		userID, agent.ID, "first", "storage:file.added", "", "main", "project-a", []string{"file.added"}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.store.CreateAppEventSubscription(
		userID, agent.ID, "second", "storage:file.added", "", "main", "project-a", []string{"file.added"}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE subscriptions SET last_seq_delivered = 51 WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE subscriptions SET last_seq_delivered = 96 WHERE id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}

	var deliveries atomic.Int64
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			t.Errorf("core path = %q, want /event", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer core-test-key" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode event: %v", err)
		}
		if body["thread_id"] != "main" {
			t.Errorf("thread_id = %q, want main", body["thread_id"])
		}
		deliveries.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer core.Close()

	parsed, err := url.Parse(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	s.agents.mu.Lock()
	s.agents.processes[agent.ID] = &runningAgent{
		port:       port,
		coreAPIKey: "core-test-key",
		reattached: true,
	}
	s.agents.mu.Unlock()

	dispatcher := NewAppEventDispatcher(s)
	s.appEventDispatcher = dispatcher
	if err := dispatcher.Reconcile(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dispatcher.mu.Lock()
		for _, lane := range dispatcher.lanes {
			lane.cancel()
		}
		dispatcher.mu.Unlock()
	})

	ev := s.appBus.Publish("storage", "project-a", 42, "file.added", json.RawMessage(`{"id":"new"}`))
	if ev.Seq != 97 {
		t.Fatalf("first event after restart has seq %d, want 97", ev.Seq)
	}

	deadline := time.Now().Add(2 * time.Second)
	for deliveries.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := deliveries.Load(); got != 2 {
		t.Fatalf("core deliveries = %d, want exactly 2", got)
	}

	for _, id := range []string{first.ID, second.ID} {
		var cursor uint64
		if err := s.store.db.QueryRow(`SELECT last_seq_delivered FROM subscriptions WHERE id = ?`, id).Scan(&cursor); err != nil {
			t.Fatal(err)
		}
		if cursor != 97 {
			t.Fatalf("subscription %s cursor = %d, want 97", id, cursor)
		}
	}

	// A second event proves normal sequencing continues and is not delivered
	// twice because of the lower replay cursor.
	ev = s.appBus.Publish("storage", "project-a", 42, "file.added", json.RawMessage(`{"id":"next"}`))
	if ev.Seq != 98 {
		t.Fatalf("second event seq = %d, want 98", ev.Seq)
	}
	deadline = time.Now().Add(2 * time.Second)
	for deliveries.Load() != 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := deliveries.Load(); got != 4 {
		t.Fatalf("core deliveries after second event = %d, want exactly 4", got)
	}
}
