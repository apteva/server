package admission

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func controlled(cpus float64) *Controller {
	return New(func() Pressure { return Pressure{CPUs: cpus, Busy: 0.2} })
}
func request(key string) Request {
	return Request{Key: key, Operation: "execute", Caller: key, Wait: time.Second}
}
func waitQueued(t *testing.T, c *Controller, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.Snapshot()["queued"].(int) == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue wanted %d: %+v", n, c.Snapshot())
}
func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
func cpuResult() Result { return Result{Duration: 200 * time.Millisecond, CPUSeconds: 0.19} }

func TestUnknownSameFunctionSerializes(t *testing.T) {
	c := controlled(8)
	first, err := c.Acquire(context.Background(), request("same"))
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan *Permit, 1)
	go func() {
		p, e := c.Acquire(context.Background(), request("same"))
		if e != nil {
			t.Error(e)
		}
		second <- p
	}()
	waitQueued(t, c, 1)
	select {
	case <-second:
		t.Fatal("unknown simultaneous invocation started")
	default:
	}
	first.Finish(cpuResult())
	p := <-second
	if p == nil {
		t.Fatal("queued call not admitted")
	}
	p.Finish(cpuResult())
	if c.Snapshot()["active"].(int) != 0 {
		t.Fatal("permit leak")
	}
}
func TestMeasuredCPUConcurrencyAdaptsToHardware(t *testing.T) {
	for _, cpus := range []float64{1, 4} {
		t.Run(string(rune('0'+int(cpus))), func(t *testing.T) {
			c := controlled(cpus)
			for i := 0; i < 8; i++ {
				p, e := c.Acquire(context.Background(), request("heavy"))
				if e != nil {
					t.Fatal(e)
				}
				p.Finish(cpuResult())
			}
			first, e := c.Acquire(context.Background(), request("heavy"))
			if e != nil {
				t.Fatal(e)
			}
			defer first.Finish(cpuResult())
			r := request("heavy")
			r.Wait = 25 * time.Millisecond
			second, e := c.Acquire(context.Background(), r)
			if cpus == 1 {
				if code(e) != "adaptive_queue_timeout" {
					t.Fatalf("single CPU admitted second heavy call: %v", e)
				}
			} else {
				if e != nil {
					t.Fatalf("spare CPUs not used: %v", e)
				}
				second.Finish(cpuResult())
			}
		})
	}
}
func TestCPUBoundAndRemoteWaitingAreDifferent(t *testing.T) {
	c := controlled(1)
	for i := 0; i < 8; i++ {
		p, e := c.Acquire(context.Background(), request("ai"))
		if e != nil {
			t.Fatal(e)
		}
		p.Finish(Result{Duration: 60 * time.Second, CPUSeconds: 0})
	}
	a, e := c.Acquire(context.Background(), request("ai"))
	if e != nil {
		t.Fatal(e)
	}
	defer a.Finish(Result{Duration: 60 * time.Second, CPUSeconds: 0})
	b, e := c.Acquire(context.Background(), request("ai"))
	if e != nil {
		t.Fatal("I/O wait treated as full CPU", e)
	}
	b.Finish(Result{Duration: 60 * time.Second, CPUSeconds: 0})
}

func TestInFlightIOLearnsBeforeFirstResponse(t *testing.T) {
	c := controlled(1)
	first, e := c.Acquire(context.Background(), request("long-io"))
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan *Permit, 1)
	go func() {
		p, e := c.Acquire(context.Background(), request("long-io"))
		if e != nil {
			t.Error(e)
		}
		done <- p
	}()
	waitQueued(t, c, 1)
	for i := 0; i < 4; i++ {
		first.ObserveCPU(0, 100*time.Millisecond)
	}
	select {
	case second := <-done:
		if second == nil {
			t.Fatal("no second permit")
		}
		second.Finish(Result{Duration: 60 * time.Second, CPUSeconds: 0})
	case <-time.After(time.Second):
		t.Fatal("network wait prevented useful concurrency")
	}
	first.Finish(Result{Duration: 60 * time.Second, CPUSeconds: 0})
	first.ObserveCPU(1, time.Second)
	if c.Snapshot()["active"].(int) != 0 {
		t.Fatal("observation resurrected finished work")
	}
}

func TestInFlightHeavyDoesNotExpandOnSingleCPU(t *testing.T) {
	c := controlled(1)
	p, e := c.Acquire(context.Background(), request("heavy"))
	if e != nil {
		t.Fatal(e)
	}
	defer p.Finish(cpuResult())
	for i := 0; i < 8; i++ {
		p.ObserveCPU(0.1, 100*time.Millisecond)
	}
	r := request("heavy")
	r.Wait = 10 * time.Millisecond
	if _, e := c.Acquire(context.Background(), r); code(e) != "adaptive_queue_timeout" {
		t.Fatal("CPU-heavy work expanded", e)
	}
}

func TestCPUSamplerStopsAndPermitRemainsOwned(t *testing.T) {
	c := controlled(4)
	p, _ := c.Acquire(context.Background(), request("sample"))
	var reads atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	stop := p.TrackCPU(ctx, func() float64 { reads.Add(1); return 0 })
	cancel()
	stop()
	before := reads.Load()
	time.Sleep(110 * time.Millisecond)
	if reads.Load() != before {
		t.Fatal("sampler leaked")
	}
	if c.Snapshot()["active"].(int) != 1 {
		t.Fatal("cancel released work before it stopped")
	}
	p.Finish(Result{Canceled: true, CPUSeconds: 0})
}
func TestSharedDestinationAcrossDistinctOperationsAndCallers(t *testing.T) {
	c := controlled(8)
	a := request("app:42")
	a.Operation = "monthly"
	first, e := c.Acquire(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for _, op := range []string{"summary", "trend", "campaigns", "commercials", "operational", "remuneration"} {
		wg.Add(1)
		go func(op string) {
			defer wg.Done()
			r := request("app:42")
			r.Operation = op
			r.Caller = op
			r.Wait = 30 * time.Millisecond
			p, e := c.Acquire(context.Background(), r)
			if p != nil {
				p.Finish(cpuResult())
				t.Error("operation bypassed shared destination ceiling")
			}
			if code(e) != "adaptive_queue_timeout" {
				t.Errorf("%s: %v", op, e)
			}
		}(op)
	}
	wg.Wait()
	first.Finish(cpuResult())
	if c.Snapshot()["queued"].(int) != 0 {
		t.Fatal("waiters leaked")
	}
}
func TestCancellationQueueLimitAndDeadlineBoundary(t *testing.T) {
	c := controlled(1)
	p, _ := c.Acquire(context.Background(), request("busy"))
	defer p.Finish(cpuResult())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := c.Acquire(ctx, request("busy")); done <- e }()
	waitQueued(t, c, 1)
	r := request("busy")
	r.MaxQueue = 1
	if _, e := c.Acquire(context.Background(), r); code(e) != "adaptive_queue_full" {
		t.Fatal(e)
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	waitQueued(t, c, 0)
	c.mu.Lock()
	expired, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer stop()
	go func() { _, e := c.Acquire(expired, request("different")); done <- e }()
	<-expired.Done()
	c.mu.Unlock()
	if e := <-done; !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("expired waiter admitted: %v", e)
	}
}
func TestQueueExpiresAndRecoversWithoutRetryingWork(t *testing.T) {
	c := controlled(1)
	p, _ := c.Acquire(context.Background(), request("one"))
	r := request("two")
	r.Wait = 15 * time.Millisecond
	start := time.Now()
	if _, e := c.Acquire(context.Background(), r); code(e) != "adaptive_queue_timeout" {
		t.Fatal(e)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("wait was not bounded")
	}
	p.Finish(cpuResult())
	q, e := c.Acquire(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	q.Finish(cpuResult())
}
func TestNestedReserveDoesNotWaitBehindParent(t *testing.T) {
	c := controlled(1)
	p, _ := c.Acquire(context.Background(), request("parent"))
	defer p.Finish(cpuResult())
	r := request("child")
	r.Nested = true
	q, e := c.Acquire(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	r.Key = "another-child"
	if _, e = c.Acquire(context.Background(), r); code(e) != "adaptive_nested_overload" {
		t.Fatal(e)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("child waited behind parent")
	}
	q.Finish(cpuResult())
}
func TestLightOperationResponsiveDuringHeavyCall(t *testing.T) {
	c := controlled(1)
	light := request("shared-app")
	light.Operation = "light"
	for i := 0; i < 4; i++ {
		p, e := c.Acquire(context.Background(), light)
		if e != nil {
			t.Fatal(e)
		}
		p.Finish(Result{Duration: 10 * time.Millisecond, CPUSeconds: 0.005})
	}
	heavy := request("shared-app")
	heavy.Operation = "heavy"
	p, e := c.Acquire(context.Background(), heavy)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Finish(cpuResult())
	start := time.Now()
	q, e := c.Acquire(context.Background(), light)
	if e != nil {
		t.Fatal(e)
	}
	q.Finish(Result{Duration: 10 * time.Millisecond, CPUSeconds: 0.005})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("light operation queued behind heavy operation")
	}
}
func TestHostPressureShrinksAndMemoryPressureStopsAdmission(t *testing.T) {
	var busy, low atomic.Bool
	c := New(func() Pressure {
		return Pressure{CPUs: 4, Busy: map[bool]float64{true: 0.99, false: 0.2}[busy.Load()], MemoryLow: low.Load()}
	})
	for i := 0; i < 8; i++ {
		p, _ := c.Acquire(context.Background(), request("work"))
		p.Finish(cpuResult())
	}
	p, _ := c.Acquire(context.Background(), request("work"))
	busy.Store(true)
	c.mu.Lock()
	c.sampled = time.Time{}
	c.groups["work"].operations["execute"].lastChange = time.Time{}
	c.mu.Unlock()
	p.Finish(cpuResult())
	rows := c.Snapshot()["operations"].([]map[string]any)
	if rows[0]["limit"].(int) != 1 {
		t.Fatal("pressure did not reduce limit", rows)
	}
	low.Store(true)
	c.mu.Lock()
	c.sampled = time.Time{}
	c.mu.Unlock()
	r := request("new")
	r.Wait = 10 * time.Millisecond
	if _, e := c.Acquire(context.Background(), r); code(e) != "adaptive_queue_timeout" {
		t.Fatal("memory pressure ignored", e)
	}
	low.Store(false)
	busy.Store(false)
	c.mu.Lock()
	c.sampled = time.Time{}
	c.mu.Unlock()
	q, e := c.Acquire(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	q.Finish(cpuResult())
}
func TestMissingTelemetryStaysConservativeAndFailuresDoNotLeak(t *testing.T) {
	c := New(func() Pressure { return Pressure{CPUs: 16, Busy: -1} })
	for i := 0; i < 50; i++ {
		p, e := c.Acquire(context.Background(), request("work"))
		if e != nil {
			t.Fatal(e)
		}
		p.Finish(Result{Duration: time.Second, CPUSeconds: -1, Failed: i%2 == 0})
		p.Finish(cpuResult())
	}
	s := c.Snapshot()
	rows := s["operations"].([]map[string]any)
	if s["active"].(int) != 0 || rows[0]["limit"].(int) != 1 {
		t.Fatal(s)
	}
}
func TestConcurrentColdBurstAndShutdown(t *testing.T) {
	c := controlled(4)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := c.Acquire(context.Background(), request("same"))
			if e != nil {
				t.Error(e)
				return
			}
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			p.Finish(Result{Failed: true, CPUSeconds: -1})
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatal("cold concurrency", peak.Load())
	}
	p, _ := c.Acquire(context.Background(), request("same"))
	done := make(chan error, 1)
	go func() { _, e := c.Acquire(context.Background(), request("same")); done <- e }()
	waitQueued(t, c, 1)
	c.Close()
	if e := <-done; code(e) != "adaptive_stopped" {
		t.Fatal(e)
	}
	p.Finish(cpuResult())
}

func TestSustainedRemoteSlowdownReducesConcurrency(t *testing.T) {
	c := controlled(4)
	for i := 0; i < 8; i++ {
		p, e := c.Acquire(context.Background(), request("remote"))
		if e != nil {
			t.Fatal(e)
		}
		p.Finish(Result{Duration: time.Second, CPUSeconds: 0})
	}
	c.mu.Lock()
	op := c.groups["remote"].operations["execute"]
	before := op.limit
	op.lastChange = time.Now().Add(-2 * time.Second)
	c.mu.Unlock()
	if before < 2 {
		t.Fatal("did not learn healthy concurrency")
	}
	for i := 0; i < 3; i++ {
		p, e := c.Acquire(context.Background(), request("remote"))
		if e != nil {
			t.Fatal(e)
		}
		p.Finish(Result{Duration: 4 * time.Second, CPUSeconds: 0})
	}
	if op.limit >= before {
		t.Fatal("remote slowdown did not reduce admission", before, op.limit)
	}
}

func TestDefaultWaitingUsesCallerDeadlineInsteadOfOneSecond(t *testing.T) {
	c := controlled(1)
	req := request("burst")
	req.Wait = 0
	first, e := c.Acquire(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan *Permit, 1)
	go func() {
		p, e := c.Acquire(ctx, req)
		if e != nil {
			t.Error(e)
		}
		done <- p
	}()
	waitQueued(t, c, 1)
	time.Sleep(1100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("premature overload before caller deadline")
	default:
	}
	first.Finish(cpuResult())
	second := <-done
	if second == nil {
		t.Fatal("queued request failed")
	}
	second.Finish(cpuResult())
	if c.Snapshot()["rejected"].(uint64) != 0 {
		t.Fatal("burst caused rejection")
	}
	// With no explicit queue override, expiration belongs to the caller.
	first, e = c.Acquire(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Finish(cpuResult())
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, e = c.Acquire(short, req); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("lost caller deadline", e)
	}
}
