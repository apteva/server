package main

// Core AppEventBus behaviour:
//   - Publish stamps monotonic seq + writes to ring + fans out
//   - Subscribe(since=N) replays only seq > N from ring buffer
//   - Lane isolation: events for (storage, projA) don't reach
//     (storage, projB) or (crm, projA)
//   - Drop-on-overflow: a slow subscriber doesn't block fanout to
//     other subscribers
//   - Concurrent Publish from many goroutines yields strictly
//     increasing seq with no duplicates within a lane
//
// These tests exercise the bus directly — no HTTP. The HTTP edge
// is covered separately in appbus_handlers_test.go.

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppBus_PublishStampsMonotonicSeq(t *testing.T) {
	b := NewAppEventBus()
	ev1 := b.Publish("storage", "p1", 100, "file.added", json.RawMessage(`{"id":1}`))
	ev2 := b.Publish("storage", "p1", 100, "file.added", json.RawMessage(`{"id":2}`))
	ev3 := b.Publish("storage", "p1", 100, "file.deleted", json.RawMessage(`{"id":1}`))
	if ev1.Seq != 1 || ev2.Seq != 2 || ev3.Seq != 3 {
		t.Fatalf("expected seq 1/2/3, got %d/%d/%d", ev1.Seq, ev2.Seq, ev3.Seq)
	}
	if ev2.App != "storage" || ev2.ProjectID != "p1" || ev2.InstallID != 100 {
		t.Fatalf("envelope fields not stamped: %+v", ev2)
	}
	if ev2.Time.IsZero() {
		t.Fatal("time not stamped")
	}
}

func TestAppBus_FanoutToAllSubscribers(t *testing.T) {
	b := NewAppEventBus()
	ch1, _, cancel1 := b.Subscribe("storage", "p1", 0)
	defer cancel1()
	ch2, _, cancel2 := b.Subscribe("storage", "p1", 0)
	defer cancel2()

	b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{"id":1}`))

	for _, ch := range []chan AppEvent{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Topic != "file.added" || ev.Seq != 1 {
				t.Fatalf("unexpected event: %+v", ev)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("subscriber didn't receive event")
		}
	}
}

func TestAppBus_LaneIsolation(t *testing.T) {
	b := NewAppEventBus()
	chSameApp, _, cancel1 := b.Subscribe("storage", "p2", 0)
	defer cancel1()
	chOtherApp, _, cancel2 := b.Subscribe("crm", "p1", 0)
	defer cancel2()

	// Publish on (storage, p1) — neither subscriber should hear it.
	b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))

	select {
	case ev := <-chSameApp:
		t.Fatalf("(storage,p2) subscriber received cross-project event: %+v", ev)
	case ev := <-chOtherApp:
		t.Fatalf("(crm,p1) subscriber received cross-app event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected — silence
	}

	// Now confirm matching lane DOES receive.
	chMatch, _, cancel3 := b.Subscribe("storage", "p1", 0)
	defer cancel3()
	b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	select {
	case <-chMatch:
		// good
	case <-time.After(200 * time.Millisecond):
		t.Fatal("matching subscriber didn't receive event")
	}
}

func TestAppBus_SubscribeReplaysFromRing(t *testing.T) {
	b := NewAppEventBus()
	// Publish 5 events before any subscriber attaches.
	for i := 1; i <= 5; i++ {
		b.Publish("storage", "p1", 1, "file.added",
			json.RawMessage([]byte(`{"i":`+itoaPositive(i)+`}`)))
	}
	// Subscribe with since=2 — should replay seq 3, 4, 5.
	_, replay, cancel := b.Subscribe("storage", "p1", 2)
	defer cancel()
	if len(replay) != 3 {
		t.Fatalf("expected 3 replayed events, got %d: %+v", len(replay), replay)
	}
	for i, ev := range replay {
		expectSeq := uint64(3 + i)
		if ev.Seq != expectSeq {
			t.Fatalf("replay[%d] seq = %d, want %d", i, ev.Seq, expectSeq)
		}
	}
}

func TestAppBus_SubscribeSinceZeroSkipsReplay(t *testing.T) {
	b := NewAppEventBus()
	for i := 1; i <= 3; i++ {
		b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	}
	_, replay, cancel := b.Subscribe("storage", "p1", 0)
	defer cancel()
	if len(replay) != 0 {
		t.Fatalf("since=0 should mean live-from-now, got %d replayed", len(replay))
	}
}

func TestAppBus_RingBufferWrapsAtCap(t *testing.T) {
	b := NewAppEventBus()
	// Publish more than ringCap (256) events; older entries should
	// drop off the ring and be unreachable via since=replay.
	total := ringCap + 10
	for i := 1; i <= total; i++ {
		b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	}
	// since=0 with replay reads back what's still in the ring. The
	// oldest available seq should be total - ringCap + 1.
	_, replay, cancel := b.Subscribe("storage", "p1", 1)
	defer cancel()
	if len(replay) != ringCap {
		t.Fatalf("expected %d replayed events (ring full), got %d", ringCap, len(replay))
	}
	if replay[0].Seq != uint64(total-ringCap+1) {
		t.Fatalf("oldest replayed seq = %d, want %d", replay[0].Seq, total-ringCap+1)
	}
	if replay[len(replay)-1].Seq != uint64(total) {
		t.Fatalf("newest replayed seq = %d, want %d", replay[len(replay)-1].Seq, total)
	}
}

func TestAppBus_DropOnOverflowDoesntBlockPublisher(t *testing.T) {
	b := NewAppEventBus()
	// Slow subscriber — never drains; its buffer should fill (~64)
	// and subsequent Publish should silently drop, NOT block.
	_, _, cancelSlow := b.Subscribe("storage", "p1", 0)
	defer cancelSlow()

	// Publish well past the per-subscriber buffer. The publisher
	// must complete in bounded time even though the slow sub is
	// stuck — that's the contract of drop-on-overflow.
	const N = 1000
	done := make(chan struct{})
	go func() {
		for i := 0; i < N; i++ {
			b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
		}
		close(done)
	}()
	select {
	case <-done:
		// good — publisher wasn't blocked by the stuck subscriber
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a non-draining subscriber")
	}
}

func TestAppBus_PacedPublishDeliversEveryEvent(t *testing.T) {
	// Drop-on-overflow is a deliberate property of the bus — bursts
	// past a subscriber's per-channel buffer (64) get dropped. But
	// when the publisher paces below that rate, every event reaches
	// every subscriber. This test asserts the no-drop case so a
	// future regression that broke fanout entirely (e.g. send to a
	// closed channel) would surface here.
	b := NewAppEventBus()
	ch, _, cancel := b.Subscribe("storage", "p1", 0)
	defer cancel()

	const N = 50 // well under the 64-buffer
	received := make([]uint64, 0, N)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					close(done)
					return
				}
				received = append(received, ev.Seq)
				if len(received) >= N {
					close(done)
					return
				}
			case <-time.After(2 * time.Second):
				close(done)
				return
			}
		}
	}()

	for i := 0; i < N; i++ {
		b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	}
	<-done
	if len(received) != N {
		t.Fatalf("got %d events, want %d", len(received), N)
	}
	// Order preserved.
	for i, seq := range received {
		if seq != uint64(i+1) {
			t.Fatalf("received[%d] = seq %d, want %d", i, seq, i+1)
		}
	}
}

func TestAppBus_ConcurrentPublishHasUniqueSeq(t *testing.T) {
	b := NewAppEventBus()
	const goroutines = 20
	const each = 50
	var wg sync.WaitGroup
	seqs := make([]uint64, goroutines*each)
	var idx atomic.Int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ev := b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
				seqs[idx.Add(1)-1] = ev.Seq
			}
		}()
	}
	wg.Wait()

	// Every seq should be unique and within [1, N].
	seen := make(map[uint64]bool, len(seqs))
	for _, s := range seqs {
		if s < 1 || s > uint64(len(seqs)) {
			t.Fatalf("seq %d out of range [1,%d]", s, len(seqs))
		}
		if seen[s] {
			t.Fatalf("duplicate seq %d", s)
		}
		seen[s] = true
	}
}

func TestAppBus_CancelClosesChannel(t *testing.T) {
	b := NewAppEventBus()
	ch, _, cancel := b.Subscribe("storage", "p1", 0)
	cancel()
	// Channel should be closed after cancel.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed within deadline")
	}
}

func TestAppBus_LaneCreatedLazily(t *testing.T) {
	b := NewAppEventBus()
	if len(b.lanes) != 0 {
		t.Fatalf("expected zero lanes at construction, got %d", len(b.lanes))
	}
	b.Publish("storage", "p1", 1, "file.added", json.RawMessage(`{}`))
	if len(b.lanes) != 1 {
		t.Fatalf("expected 1 lane after publish, got %d", len(b.lanes))
	}
	b.Publish("storage", "p2", 1, "file.added", json.RawMessage(`{}`))
	b.Publish("crm", "p1", 1, "contact.added", json.RawMessage(`{}`))
	if len(b.lanes) != 3 {
		t.Fatalf("expected 3 lanes, got %d", len(b.lanes))
	}
}

// itoaPositive is a tiny helper so the test file doesn't drag in
// strconv just to format a single int into a JSON literal.
func itoaPositive(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
