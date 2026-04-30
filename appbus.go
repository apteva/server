package main

// AppEventBus — generic in-memory pub/sub for app→dashboard live UI.
//
// Separate from TelemetryBroadcaster on purpose:
//
//   - Telemetry is agent-shaped (instance_id, thread_id, type) and
//     durable (every event lands in the telemetry table). App events
//     are app/project-scoped, fanout-only, and the source-of-truth
//     lives in the app's own DB. Forcing app fanout through telemetry
//     would pollute the table and force a wrong scope.
//
//   - Channel-chat's per-app hub got the pattern right (in-memory,
//     drop-on-overflow, reconnect-with-since for correctness) but
//     was bespoke. This bus generalises it so every app gets the
//     same shape via ctx.Emit() with no per-app server code.
//
// Subscription key: (app_name, project_id). Topics are free-form
// strings; the dashboard filters client-side.
//
// Per (app, project) we keep a small ring buffer (256 events) so a
// flapping reconnect can replay the gap via since=<seq>. Longer
// gaps the app owns — its own DB has the durable list.

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// AppEvent — one entry on the bus.
type AppEvent struct {
	Topic     string          `json:"topic"`
	App       string          `json:"app"`
	ProjectID string          `json:"project_id"`
	InstallID int64           `json:"install_id"`
	Seq       uint64          `json:"seq"`
	Time      time.Time       `json:"time"`
	Data      json.RawMessage `json:"data"`
}

// busKey identifies a single (app, project) channel.
type busKey struct {
	app       string
	projectID string
}

// busSubscriber is one connected SSE client.
type busSubscriber struct {
	id uint64
	ch chan AppEvent
}

// busLane holds the live subscribers + a small ring of recent events
// for since= replay on reconnect.
type busLane struct {
	mu       sync.Mutex
	subs     map[uint64]*busSubscriber
	ring     []AppEvent
	ringHead int    // next write position
	ringSize int    // number of valid entries (≤ cap(ring))
	nextSeq  uint64 // monotonic across the lane
}

const ringCap = 256

func newBusLane() *busLane {
	return &busLane{
		subs: make(map[uint64]*busSubscriber),
		ring: make([]AppEvent, ringCap),
	}
}

// AppEventBus — top-level bus, one per server.
type AppEventBus struct {
	mu     sync.Mutex
	lanes  map[busKey]*busLane
	nextID atomic.Uint64
}

func NewAppEventBus() *AppEventBus {
	return &AppEventBus{lanes: make(map[busKey]*busLane)}
}

// laneFor returns (and lazily creates) the lane for the given key.
// Caller does not hold lane.mu.
func (b *AppEventBus) laneFor(app, projectID string) *busLane {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := busKey{app: app, projectID: projectID}
	l, ok := b.lanes[k]
	if !ok {
		l = newBusLane()
		b.lanes[k] = l
	}
	return l
}

// Publish stamps the event with seq + time, writes it to the ring,
// and fans out to every subscriber. Drop-on-overflow per subscriber:
// the slow consumer recovers via since=<lastSeq> on reconnect.
func (b *AppEventBus) Publish(app, projectID string, installID int64, topic string, data json.RawMessage) AppEvent {
	lane := b.laneFor(app, projectID)
	lane.mu.Lock()
	lane.nextSeq++
	ev := AppEvent{
		Topic:     topic,
		App:       app,
		ProjectID: projectID,
		InstallID: installID,
		Seq:       lane.nextSeq,
		Time:      time.Now().UTC(),
		Data:      data,
	}
	// Append to the ring. ringSize caps at ringCap; ringHead wraps.
	lane.ring[lane.ringHead] = ev
	lane.ringHead = (lane.ringHead + 1) % ringCap
	if lane.ringSize < ringCap {
		lane.ringSize++
	}
	subs := make([]*busSubscriber, 0, len(lane.subs))
	for _, s := range lane.subs {
		subs = append(subs, s)
	}
	lane.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// drop — subscriber will replay via since= on reconnect.
		}
	}
	return ev
}

// Subscribe attaches a listener, optionally replaying events newer
// than since= from the ring buffer. Returns the channel + cancel fn.
// since=0 means "live from now"; the dashboard sends the largest seq
// it has seen so reconnect after a brief drop is gap-free.
func (b *AppEventBus) Subscribe(app, projectID string, since uint64) (chan AppEvent, []AppEvent, func()) {
	lane := b.laneFor(app, projectID)
	lane.mu.Lock()
	id := b.nextID.Add(1)
	sub := &busSubscriber{id: id, ch: make(chan AppEvent, 64)}
	lane.subs[id] = sub
	// Replay from ring (ordered by seq ascending).
	var replay []AppEvent
	if since > 0 && lane.ringSize > 0 {
		// The ring holds up to ringSize entries written most-recently
		// at ring[ringHead-1]; oldest is the next slot we'd overwrite.
		start := 0
		if lane.ringSize == ringCap {
			start = lane.ringHead
		}
		for i := 0; i < lane.ringSize; i++ {
			ev := lane.ring[(start+i)%ringCap]
			if ev.Seq > since {
				replay = append(replay, ev)
			}
		}
	}
	lane.mu.Unlock()
	cancel := func() {
		lane.mu.Lock()
		if existing, ok := lane.subs[id]; ok && existing == sub {
			delete(lane.subs, id)
			close(sub.ch)
		}
		lane.mu.Unlock()
	}
	return sub.ch, replay, cancel
}
