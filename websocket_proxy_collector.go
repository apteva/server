package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const proxyTelemetryQueueSize = 256
const proxyHistoryLimit = 256

type proxyTelemetryRecord struct {
	id     string
	trace  *websocketProxyTrace
	kind   string
	at     time.Time
	fields map[string]any
}

// Encoding, logging and persistence happen only in the worker. A stalled
// writer never blocks a socket copy. The queue and history registry are bounded.
type proxyTelemetryCollector struct {
	server              *Server
	queue               chan proxyTelemetryRecord
	dropped             atomic.Uint64
	historyDropped      atomic.Uint64
	persistenceFailures atomic.Uint64
	mu                  sync.Mutex
	histories           map[*websocketProxyTrace]bool
	shutdown            chan struct{}
	shutdownOnce        sync.Once
	stop                chan struct{}
	stopOnce            sync.Once
	sampled             chan struct{}
	finished            chan struct{}
	accepting           atomic.Bool
	persist             func(context.Context, []TelemetryEvent) error
}

func (s *Server) proxyTelemetry() *proxyTelemetryCollector {
	s.proxyTelemetryOnce.Do(func() {
		c := &proxyTelemetryCollector{server: s, queue: make(chan proxyTelemetryRecord, proxyTelemetryQueueSize), histories: make(map[*websocketProxyTrace]bool), shutdown: make(chan struct{}), stop: make(chan struct{}), sampled: make(chan struct{}), finished: make(chan struct{})}
		if s.store != nil {
			c.persist = s.store.insertProxyTelemetry
		}
		c.accepting.Store(true)
		s.proxyTelemetryCollector = c
		go c.run()
		go c.sampler()
	})
	return s.proxyTelemetryCollector
}
func (c *proxyTelemetryCollector) enqueue(record proxyTelemetryRecord) {
	if !c.accepting.Load() {
		c.dropped.Add(1)
		return
	}
	select {
	case c.queue <- record:
	default:
		c.dropped.Add(1)
	}
}
func (c *proxyTelemetryCollector) track(t *websocketProxyTrace) {
	c.mu.Lock()
	if len(c.histories) < proxyHistoryLimit {
		c.histories[t] = true
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.historyDropped.Add(1)
	t.emit("history.unavailable", time.Now(), map[string]any{"reason": "history_capacity", "limit": proxyHistoryLimit})
}
func (c *proxyTelemetryCollector) sample(at time.Time, final bool) {
	c.mu.Lock()
	traces := make([]*websocketProxyTrace, 0, len(c.histories))
	for t := range c.histories {
		traces = append(traces, t)
	}
	c.mu.Unlock()
	for _, t := range traces {
		if t.sample(at, final) {
			c.mu.Lock()
			delete(c.histories, t)
			c.mu.Unlock()
		}
	}
}
func (c *proxyTelemetryCollector) sampler() {
	defer close(c.sampled)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			c.sample(time.Now(), true)
			return
		case <-ticker.C:
			c.sample(time.Now(), false)
		}
	}
}
func (c *proxyTelemetryCollector) run() {
	defer close(c.finished)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var batch []TelemetryEvent
	stopping := false
	lastFailureLog := time.Time{}
	for {
		select {
		case <-c.stop:
			// Final history chunks must be queued before the worker drains.
			<-c.sampled
			c.accepting.Store(false)
			stopping = true
		case record := <-c.queue:
			batch = append(batch, c.encode(record)...)
		case <-ticker.C:
		}
		for len(batch) < 32 {
			select {
			case record := <-c.queue:
				batch = append(batch, c.encode(record)...)
			default:
				goto drained
			}
		}
	drained:
		drops := c.dropped.Load()
		historyDrops := c.historyDropped.Load()
		failures := c.persistenceFailures.Load()
		attempt := append([]TelemetryEvent(nil), batch...)
		if drops+historyDrops+failures > 0 {
			id, at := generateID(), time.Now()
			fields := map[string]any{"event_id": id, "dropped_events": drops, "history_capacity_drops": historyDrops, "persistence_failures": failures, "process_id": proxyProcessID, "observed_at": at.UTC(), "monotonic_ns": at.Sub(proxyProcessStart).Nanoseconds(), "event_sequence": proxyEventSequence.Add(1)}
			data, _ := json.Marshal(fields)
			attempt = append(attempt, TelemetryEvent{ID: id, Type: "proxy.telemetry.loss", Time: at.UTC(), Data: data})
		}
		if len(attempt) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			var err error
			if c.persist != nil {
				err = c.persist(ctx, attempt)
			}
			cancel()
			if err != nil {
				c.persistenceFailures.Add(1)
				if time.Since(lastFailureLog) >= time.Second {
					log.Printf("[WS-PROXY] telemetry_persist_error=%v pending=%d dropped=%d", err, len(batch), c.dropped.Load())
					lastFailureLog = time.Now()
				}
				// Retry the pending batch; keep it bounded during a prolonged outage.
				if len(batch) > 64 {
					c.dropped.Add(uint64(len(batch) - 64))
					batch = batch[:64]
				}
				if stopping {
					log.Printf("[WS-PROXY] shutdown_telemetry_unpersisted=%d", len(batch))
					return
				}
			} else {
				// Remove only counts actually persisted; concurrent drops remain.
				if drops > 0 {
					c.dropped.Add(^(drops - 1))
				}
				if historyDrops > 0 {
					c.historyDropped.Add(^(historyDrops - 1))
				}
				if failures > 0 {
					c.persistenceFailures.Add(^(failures - 1))
				}
				if drops+historyDrops+failures > 0 {
					log.Printf("[WS-PROXY] telemetry_loss dropped_events=%d history_capacity_drops=%d persistence_failures=%d", drops, historyDrops, failures)
				}
				if c.server.broadcaster != nil {
					c.server.broadcaster.Broadcast(attempt)
				}
				batch = nil
			}
		}
		if stopping && len(c.queue) == 0 {
			return
		}
		if stopping {
			continue
		}
	}
}
func (c *proxyTelemetryCollector) encode(record proxyTelemetryRecord) []TelemetryEvent {
	data, err := json.Marshal(record.fields)
	if err != nil {
		c.dropped.Add(1)
		log.Printf("[WS-PROXY] telemetry_encode_error=%v", err)
		return nil
	}
	log.Printf("[WS-PROXY] event=%s data=%s", record.kind, data)
	return []TelemetryEvent{{ID: record.id, AgentID: record.trace.agentID, ThreadID: record.trace.threadID, Type: record.kind, Time: record.at.UTC(), Data: data}}
}
func (s *Server) shutdownWebsocketProxies() {
	s.proxyTelemetry().shutdownOnce.Do(func() { close(s.proxyTelemetry().shutdown) })
}
func (s *Server) stopProxyTelemetry(ctx context.Context) {
	c := s.proxyTelemetry()
	c.stopOnce.Do(func() { close(c.stop) })
	select {
	case <-c.finished:
	case <-ctx.Done():
		log.Printf("[WS-PROXY] telemetry_flush_timeout dropped=%d", c.dropped.Load())
	}
}
func (s *Store) insertProxyTelemetry(ctx context.Context, events []TelemetryEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO telemetry (id, agent_id, thread_id, type, time, data) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		thread := e.ThreadID
		if thread == "" {
			thread = "main"
		}
		if _, err = stmt.ExecContext(ctx, e.ID, e.AgentID, thread, e.Type, e.Time.UTC().Format(time.RFC3339Nano), string(e.Data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
