package framework

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AppBus is an in-process pub/sub for cross-app event exchange.
//
// Publish is fire-and-forget; Subscribe runs the handler in a new
// goroutine per delivery. Handlers may panic — the bus recovers and
// logs. If a subscriber handler is slow, it does NOT block the
// publisher. If one subscriber falls >maxBacklog events behind, the
// bus starts counting drops on that subscription (exposed via
// DropCount) so ops can spot a runaway handler.
//
// Ordering within a single topic is preserved per subscriber (events
// are appended to the subscriber's queue in Publish order, handler
// drains serially). Cross-topic ordering is NOT guaranteed.
type AppBus struct {
	mu          sync.RWMutex
	subs        map[string][]*subscription // topic → subs
	wildcards   []*subscription            // subs on "*"
	logger      *slog.Logger
	globalCtx   context.Context
	errNoHandler bool // for tests
}

type subscription struct {
	topic    string
	slug     string // owning app
	q        chan Event
	drops    atomic.Uint64
	handler  func(event Event, ctx *AppCtx) error
	appCtx   *AppCtx
}

// ErrNoApp is returned by CallApp when the target slug isn't loaded.
var ErrNoApp = errors.New("framework: no such app loaded")

// maxBacklog caps the per-subscription queue. Chosen to absorb short
// bursts (a sub-thread dispatching 10 tool calls in a turn) while
// still crashing early on a truly stuck handler.
const maxBacklog = 256

func NewAppBus(ctx context.Context, logger *slog.Logger) *AppBus {
	return &AppBus{
		subs:      make(map[string][]*subscription),
		logger:    logger,
		globalCtx: ctx,
	}
}

// Publish broadcasts an event to every matching subscriber. Returns
// synchronously — deliveries happen in subscriber goroutines.
func (b *AppBus) Publish(topic, source string, payload any) {
	ev := Event{
		Topic:     topic,
		Source:    source,
		Timestamp: time.Now(),
		Payload:   payload,
	}
	b.mu.RLock()
	snapshot := append([]*subscription(nil), b.subs[topic]...)
	snapshot = append(snapshot, b.wildcards...)
	b.mu.RUnlock()

	for _, sub := range snapshot {
		select {
		case sub.q <- ev:
		default:
			// Queue full — drop. Log once per sub per second.
			sub.drops.Add(1)
			if b.logger != nil {
				b.logger.Warn("appbus drop",
					"topic", topic,
					"sub", sub.slug,
					"drops", sub.drops.Load(),
				)
			}
		}
	}
}

// Subscribe registers a handler for a topic. Use "*" to receive every
// event. The returned cancel stops delivery to this handler.
func (b *AppBus) Subscribe(topic string, owner *AppCtx, h func(event Event, ctx *AppCtx) error) (cancel func()) {
	sub := &subscription{
		topic:   topic,
		slug:    owner.Slug,
		q:       make(chan Event, maxBacklog),
		handler: h,
		appCtx:  owner,
	}
	b.mu.Lock()
	if topic == "*" {
		b.wildcards = append(b.wildcards, sub)
	} else {
		b.subs[topic] = append(b.subs[topic], sub)
	}
	b.mu.Unlock()

	// One goroutine per subscription drains its queue. Panics in
	// the handler are recovered so a bad app can't crash the bus.
	go b.drain(sub)

	return func() { b.unsubscribe(sub) }
}

func (b *AppBus) drain(sub *subscription) {
	for {
		select {
		case <-b.globalCtx.Done():
			return
		case ev, ok := <-sub.q:
			if !ok {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil && b.logger != nil {
						b.logger.Error("appbus handler panic",
							"sub", sub.slug,
							"topic", ev.Topic,
							"panic", r,
						)
					}
				}()
				if err := sub.handler(ev, sub.appCtx); err != nil && b.logger != nil {
					b.logger.Warn("appbus handler error",
						"sub", sub.slug,
						"topic", ev.Topic,
						"err", err,
					)
				}
			}()
		}
	}
}

func (b *AppBus) unsubscribe(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub.topic == "*" {
		b.wildcards = removeSub(b.wildcards, sub)
	} else {
		b.subs[sub.topic] = removeSub(b.subs[sub.topic], sub)
	}
	close(sub.q)
}

func removeSub(s []*subscription, target *subscription) []*subscription {
	out := s[:0]
	for _, v := range s {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

// DropCount returns the total events dropped across all subscriptions
// for the given app slug. Useful in /api/apps/status and in tests.
func (b *AppBus) DropCount(slug string) uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var total uint64
	scan := func(list []*subscription) {
		for _, s := range list {
			if s.slug == slug {
				total += s.drops.Load()
			}
		}
	}
	for _, v := range b.subs {
		scan(v)
	}
	scan(b.wildcards)
	return total
}
