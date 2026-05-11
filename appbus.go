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
// Two subscription kinds:
//
//   - Per-project (lanes map, keyed by busKey{app, projectID} with
//     projectID always non-empty). Project-scoped dashboard tabs +
//     project-scoped sidecars attach here.
//
//   - Wildcard (wildcards map, keyed by app). Global sidecars that
//     want every project's events for one app attach here. Each
//     wildcard has its own seq counter + ring buffer so reconnect-
//     with-since works independently of any per-project lane.
//
// Publish writes to the (app, project) lane (when projectID is set)
// AND copies the event onto the wildcard lane with the wildcard's
// own seq. The two paths never share a busKey so there is no lane
// collision when a global install emits an event for a specific
// project (the bug that motivated this refactor).
//
// Per (app, project) we keep a small ring buffer (256 events) so a
// flapping reconnect can replay the gap via since=<seq>. Longer
// gaps the app owns — its own DB has the durable list.

import (
	"encoding/json"
	"log"
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

// busKey identifies a single (app, project) channel. Used for the
// per-project lanes map only; wildcards use the app name directly
// as the map key.
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
	mu        sync.Mutex
	lanes     map[busKey]*busLane // (app, project) — project ALWAYS non-empty
	wildcards map[string]*busLane // keyed by app — cross-project subscribers
	nextID    atomic.Uint64
}

func NewAppEventBus() *AppEventBus {
	return &AppEventBus{
		lanes:     make(map[busKey]*busLane),
		wildcards: make(map[string]*busLane),
	}
}

// laneFor returns (and lazily creates) the per-project lane for the
// given (app, projectID). Caller must pass a non-empty projectID;
// the caller decides whether a project lane is needed.
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

// wildcardFor returns (and lazily creates) the per-app wildcard lane.
func (b *AppEventBus) wildcardFor(app string) *busLane {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.wildcards[app]
	if !ok {
		l = newBusLane()
		b.wildcards[app] = l
	}
	return l
}

// Publish stamps the event with seq + time, writes it to the
// per-project lane (if projectID is non-empty), fans out to per-
// project subscribers, and ALSO copies the event onto the wildcard
// lane for the app with that lane's own seq.
//
// Drop-on-overflow per subscriber: the slow consumer recovers via
// since=<lastSeq> on reconnect.
//
// projectID semantics:
//
//   - Non-empty: per-project lane gets the canonical event with the
//     lane's seq; wildcard lane gets a copy with the wildcard's own
//     seq. The event surfaced to a per-project subscriber carries
//     the lane seq; the event surfaced to a wildcard subscriber
//     carries the wildcard seq.
//   - Empty: no per-project lane is written. The event still lands
//     on the wildcard lane (for global subscribers that want every
//     cross-project event). Project-scoped dashboard tabs never see
//     unanchored events.
func (b *AppEventBus) Publish(app, projectID string, installID int64, topic string, data json.RawMessage) AppEvent {
	now := time.Now().UTC()
	base := AppEvent{
		Topic:     topic,
		App:       app,
		ProjectID: projectID,
		InstallID: installID,
		Time:      now,
		Data:      data,
	}

	var (
		projectEv             AppEvent
		projectSubs           int
		projectDelivered      int
		projectDropped        int
	)
	if projectID != "" {
		lane := b.laneFor(app, projectID)
		lane.mu.Lock()
		lane.nextSeq++
		projectEv = base
		projectEv.Seq = lane.nextSeq
		lane.ring[lane.ringHead] = projectEv
		lane.ringHead = (lane.ringHead + 1) % ringCap
		if lane.ringSize < ringCap {
			lane.ringSize++
		}
		subs := make([]*busSubscriber, 0, len(lane.subs))
		for _, s := range lane.subs {
			subs = append(subs, s)
		}
		lane.mu.Unlock()
		projectSubs = len(subs)
		for _, s := range subs {
			select {
			case s.ch <- projectEv:
				projectDelivered++
			default:
				projectDropped++
			}
		}
	}

	wildcardDelivered, wildcardDropped, wildcardSubs := b.publishWildcard(app, base)

	log.Printf("[APPBUS] publish app=%s project=%s topic=%s install=%d project_subs=%d project_delivered=%d project_dropped=%d wildcard_subs=%d wildcard_delivered=%d wildcard_dropped=%d",
		app, projectID, topic, installID,
		projectSubs, projectDelivered, projectDropped,
		wildcardSubs, wildcardDelivered, wildcardDropped)

	if projectID != "" {
		return projectEv
	}
	// Wildcard-only event — return the version that landed on the
	// wildcard lane so callers can read its seq.
	wcLane := b.wildcardFor(app)
	wcLane.mu.Lock()
	defer wcLane.mu.Unlock()
	if wcLane.ringSize > 0 {
		idx := (wcLane.ringHead - 1 + ringCap) % ringCap
		return wcLane.ring[idx]
	}
	return base
}

// publishWildcard appends the event onto the (app, *) wildcard lane
// with the wildcard's own seq + ring. Mirrors the per-project lane's
// fan-out logic. Keeping it in its own method makes the locking
// discipline easy to read — never holds two lane locks at once.
func (b *AppEventBus) publishWildcard(app string, base AppEvent) (delivered, dropped, subCount int) {
	lane := b.wildcardFor(app)
	lane.mu.Lock()
	lane.nextSeq++
	ev := base
	ev.Seq = lane.nextSeq
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
			delivered++
		default:
			dropped++
		}
	}
	subCount = len(subs)
	return
}

// Subscribe attaches a listener, optionally replaying events newer
// than since= from the ring buffer. Returns the channel + cancel fn.
// since=0 means "live from now"; the dashboard sends the largest seq
// it has seen so reconnect after a brief drop is gap-free.
//
// projectID="" subscribes to the wildcard lane for `app`: every
// project's events for that app, each tagged with its origin
// project_id. The seq + ring are wildcard-scoped so reconnect-with-
// since works independently of any per-project lane.
func (b *AppEventBus) Subscribe(app, projectID string, since uint64) (chan AppEvent, []AppEvent, func()) {
	var lane *busLane
	var laneTag string
	if projectID == "" {
		lane = b.wildcardFor(app)
		laneTag = "wildcard"
	} else {
		lane = b.laneFor(app, projectID)
		laneTag = "project"
	}
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
	log.Printf("[APPBUS] subscribe lane=%s app=%s project=%s since=%d sub_id=%d replay=%d total_subs=%d",
		laneTag, app, projectID, since, id, len(replay), len(lane.subs))
	cancel := func() {
		lane.mu.Lock()
		if existing, ok := lane.subs[id]; ok && existing == sub {
			delete(lane.subs, id)
			close(sub.ch)
		}
		remaining := len(lane.subs)
		lane.mu.Unlock()
		log.Printf("[APPBUS] unsubscribe lane=%s app=%s project=%s sub_id=%d remaining=%d",
			laneTag, app, projectID, id, remaining)
	}
	return sub.ch, replay, cancel
}
