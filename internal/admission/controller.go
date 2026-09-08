// Package admission regulates work without operator-supplied concurrency groups.
// This package is mirrored in apps/mcp/functions/internal/admission so source
// app installations do not acquire a dependency on server internals.
package admission

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

type Pressure struct {
	CPUs      float64 `json:"effective_cpus"`
	Busy      float64 `json:"cpu_busy_fraction"` // -1 when unavailable
	Throttled bool    `json:"cpu_throttled"`
	MemoryLow bool    `json:"memory_pressure"`
}
type Request struct {
	Key, Operation, Caller   string
	Background, Nested       bool
	MaxConcurrency, MaxQueue int
	Wait                     time.Duration
}
type Result struct {
	Duration                     time.Duration
	CPUSeconds                   float64 // negative means unavailable; never infer CPU from latency
	Failed, Canceled, Overloaded bool
}
type Error struct{ Code, Reason string }

func (e *Error) Error() string { return "[" + e.Code + "] " + e.Reason }

type operation struct {
	observations                          int
	limit, active, samples, healthy, slow int
	cpu, baseline, recent                 float64
	lastChange, seen                      time.Time
}
type group struct {
	active, limit int
	operations    map[string]*operation
	seen          time.Time
}
type waiter struct {
	retryAt time.Time
	req     Request
	at      time.Time
	seq     uint64
}
type Controller struct {
	leaseDir                     string
	mu                           sync.Mutex
	groups                       map[string]*group
	queue                        []*waiter
	changed                      chan struct{}
	read                         func() Pressure
	pressure                     Pressure
	sampled                      time.Time
	active, background, nested   int
	charge                       float64
	seq                          uint64
	admitted, rejected, canceled uint64
	reason                       string
	closed                       bool
}

func New(read func() Pressure) *Controller {
	if read == nil {
		read = NewHostSampler()
	}
	return &Controller{groups: map[string]*group{}, changed: make(chan struct{}), read: read}
}
func (c *Controller) SetLeaseDirectory(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leaseDir = dir
}

func (c *Controller) signal() { close(c.changed); c.changed = make(chan struct{}) }
func (c *Controller) sample(now time.Time) Pressure {
	if c.sampled.IsZero() || now.Sub(c.sampled) >= 100*time.Millisecond {
		c.pressure = c.read()
		c.sampled = now
		if c.pressure.CPUs < 0.1 {
			c.pressure.CPUs = 1
		}
	}
	return c.pressure
}
func (c *Controller) lookup(r Request, now time.Time) (*group, *operation, error) {
	g := c.groups[r.Key]
	if g == nil {
		if len(c.groups) >= 1024 {
			for k, v := range c.groups {
				if v.active == 0 && now.Sub(v.seen) > 10*time.Minute && !c.queuedKey(k) {
					delete(c.groups, k)
				}
			}
			if len(c.groups) >= 1024 {
				return nil, nil, &Error{"adaptive_key_limit", "automatic admission tracking is full"}
			}
		}
		g = &group{limit: 1, operations: map[string]*operation{}}
		c.groups[r.Key] = g
	}
	o := g.operations[r.Operation]
	if o == nil {
		if len(g.operations) >= 128 {
			for k, v := range g.operations {
				if v.active == 0 && now.Sub(v.seen) > 10*time.Minute && !c.queuedOperation(r.Key, k) {
					delete(g.operations, k)
				}
			}
			if len(g.operations) >= 128 {
				return nil, nil, &Error{"adaptive_key_limit", "automatic operation tracking is full"}
			}
		}
		o = &operation{limit: 1, cpu: 1}
		g.operations[r.Operation] = o
	}
	g.seen = now
	o.seen = now
	return g, o, nil
}
func (c *Controller) queuedKey(key string) bool {
	for _, w := range c.queue {
		if w.req.Key == key {
			return true
		}
	}
	return false
}
func (c *Controller) queuedOperation(key, op string) bool {
	for _, w := range c.queue {
		if w.req.Key == key && w.req.Operation == op {
			return true
		}
	}
	return false
}
func normalize(r Request) Request {
	// The request context is the primary deadline. Do not manufacture a
	// short overload timeout for work that still has time to complete.
	// Callers without deadlines retain a finite ten-minute safety ceiling.
	if r.Wait <= 0 || r.Wait > 10*time.Minute {
		r.Wait = 10 * time.Minute
	}
	if r.MaxConcurrency <= 0 || r.MaxConcurrency > 64 {
		r.MaxConcurrency = 64
	}
	if r.MaxQueue <= 0 || r.MaxQueue > 64 {
		r.MaxQueue = 64
	}
	return r
}
func (c *Controller) eligible(w *waiter, p Pressure) bool {
	if time.Now().Before(w.retryAt) {
		return false
	}
	r := w.req
	g := c.groups[r.Key]
	o := g.operations[r.Operation]
	if c.closed || p.MemoryLow {
		return false
	}
	hard := min(64, max(4, int(math.Ceil(p.CPUs*8))))
	short := o.samples >= 4 && o.recent > 0 && o.recent <= 0.05 && !r.Background
	groupLimit := g.limit
	if short {
		groupLimit++
	}
	if c.active >= hard || o.active >= min(r.MaxConcurrency, o.limit) || g.active >= groupLimit {
		return false
	}
	// Child work has a finite escape reserve and never waits behind a parent.
	// It still obeys operation limits and the hard global active ceiling.
	if r.Nested {
		return c.nested < max(1, hard/8)
	}
	if c.active-c.nested >= hard-max(1, hard/8) {
		return false
	}
	if r.Background && c.background >= max(1, (hard-max(1, hard/8))/2) {
		return false
	}
	cost := o.cpu
	budget := math.Max(1, p.CPUs*0.75)
	light := o.samples >= 4 && cost <= 0.15 || short
	// A small measured-light allowance preserves responsiveness beside one
	// expensive invocation. It is not permission to start another unknown job.
	if light && !r.Background {
		budget += 1
	}
	if c.charge+cost > budget+0.00001 && c.active > 0 {
		return false
	}
	if (p.Busy >= 0.9 || p.Throttled) && !light && c.active > 0 {
		return false
	}
	return true
}
func (c *Controller) firstEligible(p Pressure, now time.Time) *waiter {
	var best *waiter
	var score time.Duration
	for _, w := range c.queue {
		if !c.eligible(w, p) {
			continue
		}
		s := now.Sub(w.at)
		if !w.req.Background {
			s += 100 * time.Millisecond
		}
		o := c.groups[w.req.Key].operations[w.req.Operation]
		if o.samples >= 4 && o.cpu <= 0.15 {
			s += 100 * time.Millisecond
		}
		// Bounded priority boosts: older expensive/background work cannot starve.
		if best == nil || s > score {
			best = w
			score = s
		}
	}
	return best
}
func (c *Controller) remove(w *waiter) {
	for i, q := range c.queue {
		if q == w {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return
		}
	}
}
func (c *Controller) Acquire(ctx context.Context, r Request) (*Permit, error) {
	r = normalize(r)
	if len(r.Key) == 0 || len(r.Key) > 256 || len(r.Operation) > 256 || len(r.Caller) > 256 {
		return nil, &Error{"adaptive_key_limit", "invalid admission identity"}
	}
	wctx, cancel := context.WithTimeout(ctx, r.Wait)
	defer cancel()
	now := time.Now()
	c.mu.Lock()
	if _, _, err := c.lookup(r, now); err != nil {
		c.rejected++
		c.mu.Unlock()
		return nil, err
	}
	n, callerWaiting, bgWaiting := 0, 0, 0
	for _, q := range c.queue {
		if q.req.Key == r.Key {
			n++
		}
		if q.req.Caller == r.Caller {
			callerWaiting++
		}
		if q.req.Background {
			bgWaiting++
		}
	}
	if len(c.queue) >= 256 || n >= r.MaxQueue || callerWaiting >= 64 || (r.Background && bgWaiting >= 128) {
		c.rejected++
		c.mu.Unlock()
		return nil, &Error{"adaptive_queue_full", "automatic waiting queue is full"}
	}
	c.seq++
	w := &waiter{req: r, at: now, seq: c.seq}
	c.queue = append(c.queue, w)
	for {
		// Recheck *under the admission lock* before every grant, including wakeup.
		if ctx.Err() != nil || wctx.Err() != nil || c.closed {
			c.remove(w)
			c.signal()
			var err error
			if ctx.Err() != nil {
				c.canceled++
				err = ctx.Err()
			} else if c.closed {
				err = &Error{"adaptive_stopped", "admission stopped"}
			} else {
				c.rejected++
				err = &Error{"adaptive_queue_timeout", "automatic admission wait expired"}
			}
			c.mu.Unlock()
			return nil, err
		}
		now = time.Now()
		p := c.sample(now)
		if c.firstEligible(p, now) == w {
			g := c.groups[r.Key]
			o := g.operations[r.Operation]
			leaseLimit := g.limit
			if o.samples >= 4 && o.recent > 0 && o.recent <= 0.05 && !r.Background {
				leaseLimit++
			}
			releaseLease, leaseErr := takeLease(c.leaseDir, r.Key, leaseLimit)
			if leaseErr != nil {
				c.remove(w)
				c.rejected++
				c.signal()
				c.mu.Unlock()
				return nil, &Error{"adaptive_coordination_unavailable", "cannot acquire shared admission lease"}
			}
			if releaseLease == nil {
				w.retryAt = now.Add(100 * time.Millisecond)
				c.signal()
			} else {
				c.remove(w)
				g.active++
				o.active++
				c.active++
				c.charge += o.cpu
				if r.Background {
					c.background++
				}
				if r.Nested {
					c.nested++
				}
				c.admitted++
				permit := &Permit{releaseLease: releaseLease, c: c, req: r, cost: o.cpu, started: now, Wait: now.Sub(w.at)}
				c.signal()
				c.mu.Unlock()
				return permit, nil
			}
		}
		if r.Nested {
			c.remove(w)
			c.rejected++
			c.signal()
			c.mu.Unlock()
			return nil, &Error{"adaptive_nested_overload", "child capacity unavailable; refusing to wait behind parents"}
		}
		if p.MemoryLow {
			c.reason = "waiting: host memory pressure"
		} else if p.Busy >= 0.9 || p.Throttled {
			c.reason = "waiting: CPU pressure"
		} else {
			c.reason = "waiting: automatic concurrency or CPU budget"
		}
		changed := c.changed
		c.mu.Unlock()
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-changed:
		case <-wctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		c.mu.Lock()
	}
}

type Permit struct {
	releaseLease func()
	done         bool // guarded by c.mu
	c            *Controller
	req          Request
	cost         float64
	started      time.Time
	once         sync.Once
	Wait         time.Duration
}

func (p *Permit) Finish(result Result) {
	if p == nil {
		return
	}
	p.once.Do(func() {
		c := p.c
		c.mu.Lock()
		defer c.mu.Unlock()
		p.done = true
		if p.releaseLease != nil {
			p.releaseLease()
		}
		if result.Canceled {
			c.canceled++
		}
		now := time.Now()
		g := c.groups[p.req.Key]
		o := g.operations[p.req.Operation]
		g.active--
		o.active--
		c.active--
		c.charge = math.Max(0, c.charge-p.cost)
		if p.req.Background {
			c.background--
		}
		if p.req.Nested {
			c.nested--
		}
		g.seen = now
		o.seen = now
		pressure := c.sample(now)
		if result.Overloaded || pressure.MemoryLow || pressure.Busy >= 0.9 || pressure.Throttled {
			if now.Sub(o.lastChange) >= 500*time.Millisecond {
				o.limit = max(1, o.limit/2)
				g.limit = max(1, g.limit/2)
				o.lastChange = now
				o.healthy = 0
				c.reason = "pressure: reducing admission"
			}
		} else if !result.Canceled && !result.Failed && result.Duration > 0 {
			o.samples++
			if result.CPUSeconds >= 0 {
				duty := math.Min(math.Max(1, pressure.CPUs), math.Max(0.05, result.CPUSeconds/result.Duration.Seconds()))
				if o.samples == 1 {
					o.cpu = duty
				} else {
					o.cpu = 0.7*o.cpu + 0.3*duty
				}
			}
			seconds := result.Duration.Seconds()
			if o.recent == 0 {
				o.recent = seconds
			} else {
				o.recent = 0.5*o.recent + 0.5*seconds
			}
			if o.baseline == 0 {
				o.baseline = seconds
			} else {
				o.baseline = 0.95*o.baseline + 0.05*math.Min(seconds, o.baseline*1.1)
			}
			if seconds <= o.baseline*1.5 {
				o.healthy++
				o.slow = 0
			} else {
				o.slow++
				// Remote contention can increase latency while local CPU is idle.
				// Require several slow completions, not a single larger request.
				if o.slow >= 3 && now.Sub(o.lastChange) >= time.Second {
					o.limit = max(1, o.limit/2)
					g.limit = max(1, g.limit/2)
					o.lastChange = now
					o.slow = 0
					c.reason = "sustained service slowdown: reducing admission"
				}
				o.healthy = 0
			}
			// Missing CPU telemetry never learns higher execution concurrency.
			if pressure.Busy >= 0 && o.healthy >= 8 && now.Sub(o.lastChange) >= time.Second {
				ceiling := min(p.req.MaxConcurrency, max(1, int(math.Floor(math.Max(1, pressure.CPUs*0.75)/o.cpu))))
				if o.limit < ceiling {
					o.limit++
					g.limit = max(g.limit, o.limit)
					c.reason = "healthy completions: cautiously increasing admission"
				}
				o.lastChange = now
				o.healthy = 0
			}
		}
		c.signal()
	})
}

// ObserveCPU updates the cost of an in-flight call from measured CPU deltas.
// Long network waits can release their CPU scheduling charge before completing;
// wall-clock duration alone never grants that credit. Existing calls aren't killed.
func (p *Permit) ObserveCPU(cpuSeconds float64, elapsed time.Duration) {
	if p == nil || cpuSeconds < 0 || elapsed <= 0 {
		return
	}
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.done {
		return
	}
	now := time.Now()
	pressure := c.sample(now)
	g := c.groups[p.req.Key]
	o := g.operations[p.req.Operation]
	duty := math.Min(math.Max(1, pressure.CPUs), math.Max(0.05, cpuSeconds/elapsed.Seconds()))
	o.cpu = 0.5*o.cpu + 0.5*duty
	o.observations++
	c.charge = math.Max(0, c.charge-p.cost+o.cpu)
	p.cost = o.cpu
	if o.observations%4 == 0 && pressure.Busy >= 0 && pressure.Busy < 0.8 && !pressure.Throttled && !pressure.MemoryLow && now.Sub(o.lastChange) >= time.Second {
		ceiling := min(p.req.MaxConcurrency, max(1, int(math.Floor(math.Max(1, pressure.CPUs*0.75)/o.cpu))))
		if o.limit < ceiling {
			o.limit++
			g.limit = max(g.limit, o.limit)
			o.lastChange = now
			c.reason = "measured in-flight CPU headroom: cautiously increasing admission"
		}
	}
	c.signal()
}

// TrackCPU is bounded by an active permit and request context. stop joins the
// sampler before the permit is released. A constant-zero reader is appropriate
// only for known remote integration I/O, never for arbitrary local app work.
func (p *Permit) TrackCPU(ctx context.Context, read func() float64) func() {
	if p == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	initial := read()
	at := time.Now()
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case now := <-ticker.C:
				current := read()
				if initial >= 0 && current >= initial {
					p.ObserveCPU(current-initial, now.Sub(at))
				}
				initial = current
				at = now
			}
		}
	}()
	return func() { once.Do(func() { close(stop); <-done }) }
}
func (c *Controller) Close() { c.mu.Lock(); c.closed = true; c.signal(); c.mu.Unlock() }
func (c *Controller) Snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	p := c.sample(now)
	rows := []map[string]any{}
	for key, g := range c.groups {
		for name, o := range g.operations {
			queued := 0
			for _, w := range c.queue {
				if w.req.Key == key && w.req.Operation == name {
					queued++
				}
			}
			rows = append(rows, map[string]any{"key": key, "operation": name, "limit": o.limit, "destination_limit": g.limit, "active": o.active, "queued": queued, "samples": o.samples, "estimated_cpu_cores": o.cpu})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["key"].(string)+rows[i]["operation"].(string) < rows[j]["key"].(string)+rows[j]["operation"].(string)
	})
	return map[string]any{"mode": "automatic", "active": c.active, "queued": len(c.queue), "estimated_active_cpu_cores": c.charge, "pressure": p, "admitted": c.admitted, "rejected": c.rejected, "canceled": c.canceled, "last_decision": c.reason, "operations": rows}
}
