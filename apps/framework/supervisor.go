package framework

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Supervisor runs an app's Workers. Each worker gets its own
// goroutine with panic recovery + backoff between restarts. On server
// shutdown the supervisor cancels the root context; workers are
// expected to honor ctx.Done().
type Supervisor struct {
	mu       sync.Mutex
	cancels  []context.CancelFunc
	rootCtx  context.Context
	wg       sync.WaitGroup
	appCtx   *AppCtx
}

func NewSupervisor(rootCtx context.Context, appCtx *AppCtx) *Supervisor {
	return &Supervisor{rootCtx: rootCtx, appCtx: appCtx}
}

// Start launches a worker. Safe to call multiple times.
func (s *Supervisor) Start(w Worker) {
	backoff := w.RestartBackoff
	if backoff == 0 {
		backoff = 5 * time.Second
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	s.mu.Lock()
	s.cancels = append(s.cancels, cancel)
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		if w.Cron != "" {
			s.runCron(ctx, w)
			return
		}
		// Loop-forever worker. Restart with backoff on return.
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.runOnce(ctx, w)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
}

// runOnce executes the worker with panic recovery. Logs panics and
// errors; returns so the outer loop can restart.
func (s *Supervisor) runOnce(ctx context.Context, w Worker) {
	defer func() {
		if r := recover(); r != nil && s.appCtx.Logger != nil {
			s.appCtx.Logger.Error("worker panic",
				"app", s.appCtx.Slug,
				"worker", w.Name,
				"panic", r,
			)
		}
	}()
	if err := w.Run(ctx, s.appCtx); err != nil && s.appCtx.Logger != nil {
		s.appCtx.Logger.Warn("worker returned error",
			"app", s.appCtx.Slug,
			"worker", w.Name,
			"err", err,
		)
	}
}

// runCron parses a minimal crontab expression and fires the worker on
// the schedule. Supported fields: standard 5-field "m h dom mon dow".
// Only "*", "*/N", and literal integers are supported — good enough
// for v1 "every N minutes" / "at hour X" use cases. Apps needing more
// should run their own scheduler in a loop-worker.
func (s *Supervisor) runCron(ctx context.Context, w Worker) {
	schedule, err := parseCron(w.Cron)
	if err != nil {
		if s.appCtx.Logger != nil {
			s.appCtx.Logger.Error("cron parse error",
				"app", s.appCtx.Slug,
				"worker", w.Name,
				"cron", w.Cron,
				"err", err,
			)
		}
		return
	}
	for {
		next := schedule.next(time.Now())
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			s.runOnce(ctx, w)
		}
	}
}

// Stop cancels all workers and waits for them to exit.
func (s *Supervisor) Stop(timeout time.Duration) {
	s.mu.Lock()
	for _, c := range s.cancels {
		c()
	}
	s.cancels = nil
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// --- Minimal cron parser ----------------------------------------------

type cronSchedule struct {
	min, hour, dom, mon, dow cronField
}

// cronField holds a match set. 'step' is used for "*/N" patterns.
type cronField struct {
	any     bool
	step    int   // for */N
	values  []int // literal values (sorted)
	min, max int
}

func (f cronField) matches(n int) bool {
	if f.any && f.step <= 1 {
		return true
	}
	if f.step > 1 {
		// */N fires when (n - min) is divisible by step.
		return (n-f.min)%f.step == 0
	}
	for _, v := range f.values {
		if v == n {
			return true
		}
	}
	return false
}

func parseCron(expr string) (*cronSchedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron must have 5 fields, got %d", len(parts))
	}
	ranges := [5]struct{ min, max int }{
		{0, 59}, // minute
		{0, 23}, // hour
		{1, 31}, // day of month
		{1, 12}, // month
		{0, 6},  // day of week
	}
	fields := make([]cronField, 5)
	for i, p := range parts {
		f, err := parseCronField(p, ranges[i].min, ranges[i].max)
		if err != nil {
			return nil, fmt.Errorf("field %d (%s): %w", i, p, err)
		}
		fields[i] = f
	}
	return &cronSchedule{
		min: fields[0], hour: fields[1], dom: fields[2], mon: fields[3], dow: fields[4],
	}, nil
}

func parseCronField(s string, lo, hi int) (cronField, error) {
	f := cronField{min: lo, max: hi}
	if s == "*" {
		f.any = true
		return f, nil
	}
	if strings.HasPrefix(s, "*/") {
		var n int
		if _, err := fmt.Sscanf(s[2:], "%d", &n); err != nil || n <= 0 {
			return f, fmt.Errorf("invalid step %q", s)
		}
		f.any = true
		f.step = n
		return f, nil
	}
	// Comma-separated literals.
	for _, part := range strings.Split(s, ",") {
		var v int
		if _, err := fmt.Sscanf(part, "%d", &v); err != nil {
			return f, fmt.Errorf("bad value %q", part)
		}
		if v < lo || v > hi {
			return f, fmt.Errorf("%d out of range [%d,%d]", v, lo, hi)
		}
		f.values = append(f.values, v)
	}
	return f, nil
}

// next returns the next scheduled time strictly after `from`.
func (c *cronSchedule) next(from time.Time) time.Time {
	t := from.Add(time.Minute).Truncate(time.Minute)
	// Walk forward a minute at a time; bounded search so we don't
	// loop forever on an unsatisfiable schedule. 4 years covers
	// everything realistic.
	deadline := t.Add(4 * 365 * 24 * time.Hour)
	for t.Before(deadline) {
		if c.min.matches(t.Minute()) &&
			c.hour.matches(t.Hour()) &&
			c.dom.matches(t.Day()) &&
			c.mon.matches(int(t.Month())) &&
			c.dow.matches(int(t.Weekday())) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return deadline
}
