package admission

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Observer accounts for transport work without scheduling it. Resource owners
// enforce capacity; an app/connection identity never serializes HTTP requests.
type Observer struct {
	mu                                            sync.Mutex
	closed                                        bool
	active, admitted, completed, failed, canceled int
	rows                                          map[string]*transportRow
}
type transportRow struct {
	key, operation    string
	active, completed int
	duration          time.Duration
}

func NewObserver() *Observer { return &Observer{rows: make(map[string]*transportRow)} }

type Observation struct {
	observer *Observer
	row      *transportRow
	once     sync.Once
	Wait     time.Duration // Always zero: no transport admission queue.
}

func (o *Observer) Acquire(ctx context.Context, r Request) (*Observation, error) {
	// Bounded labels as well as bounded row count; telemetry must not retain
	// an arbitrarily large caller-supplied tool name.
	if len(r.Key) > 256 {
		sum := sha256.Sum256([]byte(r.Key))
		r.Key = fmt.Sprintf("#%x", sum[:16])
	}
	if len(r.Operation) > 256 {
		sum := sha256.Sum256([]byte(r.Operation))
		r.Operation = fmt.Sprintf("#%x", sum[:16])
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.closed {
		return nil, &Error{Code: "adaptive_stopped", Reason: "transport shutting down"}
	}
	key := r.Key + "\x00" + r.Operation
	row := o.rows[key]
	// Observability cardinality never rejects business work. Completed entries
	// may be evicted; requests beyond the bound still contribute global totals.
	if row == nil && len(o.rows) >= 1024 {
		for k, v := range o.rows {
			if v.active == 0 {
				delete(o.rows, k)
				break
			}
		}
	}
	if row == nil && len(o.rows) < 1024 {
		row = &transportRow{key: r.Key, operation: r.Operation}
		o.rows[key] = row
	}
	if row != nil {
		row.active++
	}
	o.active++
	o.admitted++
	return &Observation{observer: o, row: row}, nil
}
func (p *Observation) Finish(r Result) {
	p.once.Do(func() {
		o := p.observer
		o.mu.Lock()
		defer o.mu.Unlock()
		o.active--
		o.completed++
		if r.Failed {
			o.failed++
		}
		if r.Canceled {
			o.canceled++
		}
		if p.row != nil {
			p.row.active--
			p.row.completed++
			p.row.duration += r.Duration
		}
	})
}
func (o *Observer) Close() { o.mu.Lock(); o.closed = true; o.mu.Unlock() }
func (o *Observer) Snapshot() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	rows := []map[string]any{}
	for _, r := range o.rows {
		rows = append(rows, map[string]any{"key": r.key, "operation": r.operation, "active": r.active, "queued": 0, "completed": r.completed, "service_ms": r.duration.Milliseconds()})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["key"].(string)+rows[i]["operation"].(string) < rows[j]["key"].(string)+rows[j]["operation"].(string)
	})
	return map[string]any{"mode": "parallel", "active": o.active, "queued": 0, "admitted": o.admitted, "completed": o.completed, "failed": o.failed, "canceled": o.canceled, "operations": rows}
}
